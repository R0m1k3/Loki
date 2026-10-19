package loki

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func getKatex(t *testing.T, méthode, chemin string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handleKatex(rec, httptest.NewRequest(méthode, chemin, nil))
	return rec
}

// Le script, embarqué, est servi sous /katex/ avec les en-têtes que rend aussi
// la façade Rust (ui.rs).
func TestKatexScriptServi(t *testing.T) {
	rec := getKatex(t, http.MethodGet, "/katex/katex.min.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d", rec.Code)
	}
	h := rec.Header()
	if got := h.Get("Content-Type"); got != "application/javascript" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := h.Get("Cache-Control"); got != "public, max-age=86400" {
		t.Errorf("Cache-Control = %q", got)
	}
	if !strings.Contains(rec.Body.String(), `version:"0.16.11"`) {
		t.Error("katex.min.js n'est pas KaTeX 0.16.11")
	}
	if h.Get("Content-Length") != strconv.Itoa(rec.Body.Len()) {
		t.Errorf("Content-Length = %q pour %d octets", h.Get("Content-Length"), rec.Body.Len())
	}
}

// Chaque police woff2 que la feuille de style demande est embarquée et servie.
func TestKatexFeuilleEtPolices(t *testing.T) {
	rec := getKatex(t, http.MethodGet, "/katex/katex.min.css")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/css; charset=utf-8" {
		t.Fatalf("css : code %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	polices := regexp.MustCompile(`url\((fonts/[^)]+\.woff2)\)`).FindAllStringSubmatch(rec.Body.String(), -1)
	if len(polices) < 10 {
		t.Fatalf("%d polices woff2 dans la feuille", len(polices))
	}
	for _, m := range polices {
		r := getKatex(t, http.MethodGet, "/katex/"+m[1])
		if r.Code != http.StatusOK || r.Header().Get("Content-Type") != "font/woff2" || r.Body.Len() == 0 {
			t.Errorf("%s : code %d, type %q", m[1], r.Code, r.Header().Get("Content-Type"))
		}
	}
}

// Rien d'autre : ni index de dossier, ni licence, ni remontée, ni POST.
func TestKatexRefus(t *testing.T) {
	for _, c := range []struct{ méthode, chemin string }{
		{http.MethodGet, "/katex/"},
		{http.MethodGet, "/katex/fonts/"},
		{http.MethodGet, "/katex/LICENSE"},
		{http.MethodGet, "/katex/fonts/absente.woff2"},
		{http.MethodGet, "/katex/../index.html"},
		{http.MethodGet, "/katex/fonts/../katex.min.js"},
		{http.MethodGet, "/katexkatex.min.js"},
		{http.MethodPost, "/katex/katex.min.js"},
	} {
		if rec := getKatex(t, c.méthode, c.chemin); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s : code %d, attendu 404", c.méthode, c.chemin, rec.Code)
		}
	}
}

// Derrière gzipHandler (comme en production) : script et feuille compressés,
// polices non — comme la façade.
func TestKatexCompression(t *testing.T) {
	h := gzipHandler(http.HandlerFunc(handleKatex))
	for chemin, gz := range map[string]bool{
		"/katex/katex.min.js":                   true,
		"/katex/katex.min.css":                  true,
		"/katex/fonts/KaTeX_Main-Regular.woff2": false,
	} {
		req := httptest.NewRequest(http.MethodGet, chemin, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Content-Encoding") == "gzip"; got != gz {
			t.Errorf("%s : gzip %v, attendu %v", chemin, got, gz)
		}
	}
}
