package loki

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Lot 35 : les routes de base arrivent à la façade par le tunnel du relais
// dès qu'elle tourne, sans variable ; sans façade (LOKI_FACADE=0 : ni
// adresse ni jeton), le Go les sert lui-même.
func TestFacadeRelaieLesRoutesDeBase(t *testing.T) {
	testHome(t)
	f := demarrerFacade(t)
	t.Setenv(facadeChatEnv, "")
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }
	for p := range facadeCoreRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("GET", p, nil))
		if w.Body.String() != f.reply {
			t.Fatalf("%s : réponse %q", p, w.Body.String())
		}
	}
	if got := f.recu(); len(got) != len(facadeCoreRoutes) || !strings.HasSuffix(strings.TrimSpace(got[0]), "jeton") {
		t.Fatalf("reçu par la façade : %v", got)
	}
	// Une route que le Go garde : ici.
	w := httptest.NewRecorder()
	withFacadeChat("/api/mem/health", local)(w, httptest.NewRequest("GET", "/api/mem/health", nil))
	if w.Body.String() != "go" {
		t.Fatalf("route du Go relayée : %q", w.Body.String())
	}
	// Façade muette : 502 qui le dit.
	f.srv.Close()
	w = httptest.NewRecorder()
	withFacadeChat("/api/status", local)(w, httptest.NewRequest("GET", "/api/status", nil))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "façade indisponible : ") {
		t.Fatalf("façade muette : %d %s", w.Code, w.Body.String())
	}
	// Sans façade : le Go sert.
	t.Setenv(facadeAddrEnv, "")
	w = httptest.NewRecorder()
	withFacadeChat("/api/status", local)(w, httptest.NewRequest("GET", "/api/status", nil))
	if w.Body.String() != "go" {
		t.Fatalf("sans façade : %q", w.Body.String())
	}
}
