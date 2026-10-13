package loki

import (
	"encoding/json"
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
