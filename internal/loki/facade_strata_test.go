package loki

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Strata part à la façade avec LOKI_STRATA=rust (lot 25) ; le reste du
// moteur, non.
func TestFacadeStrata(t *testing.T) {
	testHome(t)
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(internalTokenHeader) != jetonTest {
			w.WriteHeader(403)
			return
		}
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		io.WriteString(w, `{"ok":true,"de":"facade"}`)
	}))
	defer srv.Close()
	t.Setenv(internalTokenEnv, jetonTest)
	t.Setenv(facadeAddrEnv, strings.TrimPrefix(srv.URL, "http://"))
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }

	t.Setenv(facadeStrataEnv, "")
	for p := range facadeStrataRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if w.Body.String() != "go" {
			t.Fatalf("sans LOKI_STRATA, %s : %q", p, w.Body.String())
		}
	}
	t.Setenv(facadeStrataEnv, " RUST")
	if len(facadeStrataRoutes) != 3 {
		t.Fatalf("routes : %v", facadeStrataRoutes)
	}
	for p := range facadeStrataRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if !strings.Contains(w.Body.String(), "facade") {
			t.Fatalf("%s non relayé : %q", p, w.Body.String())
		}
	}
	mu.Lock()
	n := len(calls)
	mu.Unlock()
	if n != len(facadeStrataRoutes) {
		t.Fatalf("relais : %v", calls)
	}
	w := httptest.NewRecorder()
	withFacadeChat("/api/llamacpp/install", local)(w, httptest.NewRequest("POST", "/api/llamacpp/install", nil))
	if w.Body.String() != "go" {
		t.Fatalf("la compilation reste au Go : %q", w.Body.String())
	}
	t.Setenv(internalTokenEnv, "")
	if facadeStrata() {
		t.Fatal("tenu sans jeton interne")
	}
}
