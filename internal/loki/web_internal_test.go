package loki

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const jetonTest = "0123456789abcdef0123456789abcdef"

func appelInterne(t *testing.T, path, jeton string) *httptest.ResponseRecorder {
	t.Helper()
	suite := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("mux"))
	})
	r := httptest.NewRequest("GET", path, nil)
	if jeton != "" {
		r.Header.Set(internalTokenHeader, jeton)
	}
	w := httptest.NewRecorder()
	withInternalAPI(suite).ServeHTTP(w, r)
	return w
}

func TestAPIInterneFermeeSansJeton(t *testing.T) {
	t.Setenv(internalTokenEnv, "")
	if w := appelInterne(t, "/internal/v1/auth", jetonTest); w.Code != 404 {
		t.Fatalf("sans jeton configuré : %d, attendu 404", w.Code)
	}
	// Jeton configuré mais trop court : API désactivée, même avec le bon.
	t.Setenv(internalTokenEnv, "court")
	if w := appelInterne(t, "/internal/v1/auth", "court"); w.Code != 404 {
		t.Fatalf("jeton trop court : %d, attendu 404", w.Code)
	}
}

func TestAPIInterneRefuseMauvaisJeton(t *testing.T) {
	t.Setenv(internalTokenEnv, jetonTest)
	for _, j := range []string{"", "x", strings.ToUpper(jetonTest)} {
		if w := appelInterne(t, "/internal/v1/auth", j); w.Code != 404 {
			t.Fatalf("jeton %q : %d, attendu 404", j, w.Code)
		}
	}
}

func TestAPIInterneAuth(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	if err := putStr(bkState, "web_key", "loki-web-secret"); err != nil {
		t.Fatal(err)
	}
	w := appelInterne(t, "/internal/v1/auth", jetonTest)
	if w.Code != 200 {
		t.Fatalf("code %d : %s", w.Code, w.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["web_key"] != "loki-web-secret" || got["version"] != Version {
		t.Fatalf("réponse : %+v", got)
	}
	if w := appelInterne(t, "/internal/v1/autre", jetonTest); w.Code != 404 {
		t.Fatalf("route inconnue : %d", w.Code)
	}
}

func TestAPIInterneLaisseLeResteAuMux(t *testing.T) {
	t.Setenv(internalTokenEnv, jetonTest)
	if w := appelInterne(t, "/api/ping", ""); w.Body.String() != "mux" {
		t.Fatalf("relais au mux : %q", w.Body.String())
	}
}

func appelStore(t *testing.T, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	r.Header.Set(internalTokenHeader, jetonTest)
	w := httptest.NewRecorder()
	withInternalAPI(http.NotFoundHandler()).ServeHTTP(w, r)
	return w
}

func TestAPIInterneStore(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	if w := appelStore(t, "GET", "/internal/v1/store?bucket=state&key=absente", ""); w.Code != 404 {
		t.Fatalf("clé absente : %d", w.Code)
	}
	if w := appelStore(t, "PUT", "/internal/v1/store?bucket=state&key=k", "valeur\x00binaire"); w.Code != 204 {
		t.Fatalf("put : %d %s", w.Code, w.Body.String())
	}
	if got := getStr(bkState, "k"); got != "valeur\x00binaire" {
		t.Fatalf("écrit : %q", got)
	}
	w := appelStore(t, "GET", "/internal/v1/store?bucket=state&key=k", "")
	if w.Code != 200 || w.Body.String() != "valeur\x00binaire" {
		t.Fatalf("get : %d %q", w.Code, w.Body.String())
	}
	if err := putStr(bkConfig, "PORT", "8081"); err != nil {
		t.Fatal(err)
	}
	w = appelStore(t, "GET", "/internal/v1/store/all?bucket=config", "")
	var m map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil || m["PORT"] != "8081" {
		t.Fatalf("all : %d %s", w.Code, w.Body.String())
	}
	// Écriture via l'API = écriture locale : le cache de config est à jour.
	_ = ReadConfig()
	if w := appelStore(t, "PUT", "/internal/v1/store?bucket=config&key=PORT", "9000"); w.Code != 204 {
		t.Fatalf("put config : %d", w.Code)
	}
	if LLMPort() != 9000 {
		t.Fatalf("cache non invalidé : port %d", LLMPort())
	}
	if w := appelStore(t, "DELETE", "/internal/v1/store?bucket=state&key=k", ""); w.Code != 204 {
		t.Fatalf("delete : %d", w.Code)
	}
	if getStr(bkState, "k") != "" {
		t.Fatal("clé non supprimée")
	}
	if w := appelStore(t, "GET", "/internal/v1/store?bucket=state", ""); w.Code != 400 {
		t.Fatalf("sans clé : %d", w.Code)
	}
}
