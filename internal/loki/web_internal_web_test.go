package loki

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// L'API interne du moteur web intégré (lot 42) : la conversion d'une page et
// la recherche DuckDuckGo, telles que la façade les demande.
func TestInternalMoteurWebIntegre(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page.txt":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "bonjour\n")
		case "/html/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<div class="result"><a class="result__a" href="https://r.example/">R</a>`+
				`<a class="result__snippet">résumé</a></div>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer site.Close()
	old := ddgHTMLBase
	ddgHTMLBase = site.URL
	defer func() { ddgHTMLBase = old }()

	call := func(h http.HandlerFunc, method, body string) (int, map[string]any) {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(method, "/x", strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if code, out := call(handleInternalWebFetch, "POST", `{"url":"`+site.URL+`/page.txt"}`); code != 200 || out["markdown"] != "bonjour\n" {
		t.Fatalf("fetch : %d %v", code, out)
	}
	if code, out := call(handleInternalWebFetch, "POST", `{"url":"`+site.URL+`/absente"}`); code != 200 ||
		out["error"] != "HTTP 404 sur "+site.URL+"/absente" {
		t.Fatalf("fetch absente : %d %v", code, out)
	}
	code, out := call(handleInternalWebSearch, "POST", `{"query":"q","limit":5}`)
	res, _ := json.Marshal(out["results"])
	if code != 200 || string(res) != `[{"snippet":"résumé","title":"R","url":"https://r.example/"}]` {
		t.Fatalf("search : %d %s", code, res)
	}
	if code, _ := call(handleInternalWebSearch, "GET", ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET : %d", code)
	}
	if code, _ := call(handleInternalWebFetch, "POST", "pas du json"); code != http.StatusBadRequest {
		t.Fatalf("corps illisible : %d", code)
	}
}
