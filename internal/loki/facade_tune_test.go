package loki

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// L'optimiseur part à la façade avec LOKI_TUNE=rust (lot 28) ; le reste,
// non. La façade tient le verrou de génération par le bail du moteur, motif
// « tune » ; le Go lui prête ce qu'il est seul à voir et le démarrage du
// moteur sans le refus de l'optimiseur.
func TestFacadeTune(t *testing.T) {
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

	t.Setenv(facadeTuneEnv, "")
	if facadeTune() {
		t.Fatal("tenu sans LOKI_TUNE")
	}
	for p := range facadeTuneRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if w.Body.String() != "go" {
			t.Fatalf("sans LOKI_TUNE, %s : %q", p, w.Body.String())
		}
	}
	t.Setenv(facadeTuneEnv, " RUST")
	if len(facadeTuneRoutes) != 5 {
		t.Fatalf("routes : %v", facadeTuneRoutes)
	}
	for p := range facadeTuneRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if !strings.Contains(w.Body.String(), "facade") {
			t.Fatalf("%s non relayé : %q", p, w.Body.String())
		}
	}
	mu.Lock()
	n := len(calls)
	mu.Unlock()
	if n != len(facadeTuneRoutes) {
		t.Fatalf("relais : %v", calls)
	}
	w := httptest.NewRecorder()
	withFacadeChat("/api/bench", local)(w, httptest.NewRequest("POST", "/api/bench", nil))
	if w.Body.String() != "go" {
		t.Fatalf("le banc d'essai reste au Go : %q", w.Body.String())
	}

	post := func(h http.HandlerFunc, path, body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	tuneCtx := func() map[string]any {
		rec := httptest.NewRecorder()
		handleInternalTuneContext(rec, httptest.NewRequest("GET", "/internal/v1/tune/context", nil))
		if rec.Code != 200 {
			t.Fatalf("contexte : %d", rec.Code)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Au repos : rien n'interdit, la télémétrie suit.
	perfRecord(perfRec{Kind: perfMain, Complete: true, Total: 100, New: 100, Gen: 10}, nil)
	c := tuneCtx()
	if c["busy"] != "" || c["main_active"] != false {
		t.Fatalf("contexte au repos : %v", c)
	}
	if perf, _ := c["perf"].([]any); len(perf) == 0 {
		t.Fatalf("télémétrie absente : %v", c["perf"])
	}
	rec := httptest.NewRecorder()
	handleInternalTuneContext(rec, httptest.NewRequest("POST", "/internal/v1/tune/context", nil))
	if rec.Code != 405 || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("POST : %d", rec.Code)
	}

	// Le bail « tune » : les autres reçoivent le refus de l'optimiseur ; ce
	// n'est pas un benchmark.
	code, out := post(handleInternalLease, "/internal/v1/engine/lease", `{"ttl_ms":60000,"why":"tune"}`)
	if code != 200 {
		t.Fatalf("bail : %d %v", code, out)
	}
	id, _ := out["lease"].(string)
	if conv.busyReason() != errTuneBusy || benchRunning() || !conv.isGenerating() {
		t.Fatalf("bail de l'optimiseur : refus=%v bench=%v", conv.busyReason(), benchRunning())
	}
	if code, out := post(handleInternalLease, "/internal/v1/engine/lease", `{}`); code != 409 || out["error"] != errTuneBusy.Error() {
		t.Fatalf("second bail : %d %v", code, out)
	}
	if c := tuneCtx(); c["busy"] != errTuneBusy.Error() {
		t.Fatalf("contexte pendant l'optimisation : %v", c)
	}
	// Changer de discussion ne rompt pas le bail de l'optimiseur.
	conv.loadFrom("autre", nil)
	if !conv.isGenerating() || conv.busyReason() != errTuneBusy {
		t.Fatal("le bail de l'optimiseur a sauté à la bascule de discussion")
	}
	if code, _ := post(handleInternalLeaseRelease, "/internal/v1/engine/lease/release", `{"lease":"`+id+`"}`); code != 204 {
		t.Fatalf("libération : %d", code)
	}
	if conv.isGenerating() || conv.busyReason() == errTuneBusy {
		t.Fatal("bail rendu : le verrou devait être libre")
	}
	// Un benchmark de la façade : le contexte le dit en premier.
	code, out = post(handleInternalLease, "/internal/v1/engine/lease", `{"why":"bench"}`)
	if code != 200 {
		t.Fatalf("bail du bench : %d %v", code, out)
	}
	if c := tuneCtx(); c["busy"] != errBenchBusy.Error() {
		t.Fatalf("contexte pendant un bench : %v", c)
	}
	post(handleInternalLeaseRelease, "/internal/v1/engine/lease/release", `{"lease":"`+out["lease"].(string)+`"}`)

	// Démarrage et arrêt sans le refus de l'optimiseur : la façade tient le
	// verrou.
	var guarded, osActs []string
	oldA, oldOS := internalServiceAction, internalServiceActionOS
	t.Cleanup(func() { internalServiceAction, internalServiceActionOS = oldA, oldOS })
	internalServiceAction = func(a string) error { guarded = append(guarded, a); return errTuneBusy }
	internalServiceActionOS = func(a string) error { osActs = append(osActs, a); return nil }
	for _, c := range []struct{ body, want string }{
		{`{"action":"stop","os":true}`, `{"error":"","ran":true}`},
		{`{"action":"start","os":true}`, `{"error":"","ran":true}`},
		{`{"action":"start"}`, `{"error":"` + errTuneBusy.Error() + `","ran":true}`},
	} {
		w := httptest.NewRecorder()
		handleInternalServiceAction(w, httptest.NewRequest("POST", "/internal/v1/service/action", strings.NewReader(c.body)))
		if got := strings.TrimSpace(w.Body.String()); w.Code != 200 || got != c.want {
			t.Fatalf("%s : %d %s", c.body, w.Code, got)
		}
	}
	if strings.Join(osActs, ",") != "stop,start" || strings.Join(guarded, ",") != "start" {
		t.Fatalf("actions : sans refus %v, avec %v", osActs, guarded)
	}

	t.Setenv(internalTokenEnv, "")
	if facadeTune() {
		t.Fatal("tenu sans jeton interne")
	}
}
