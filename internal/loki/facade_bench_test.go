package loki

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Le banc d'essai part à la façade avec LOKI_BENCH=rust (lot 27) ; le reste,
// non. La façade tient le verrou de génération par le bail du moteur, motif
// « bench », et le Go lui prête le travail annexe et la télémétrie.
func TestFacadeBench(t *testing.T) {
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

	t.Setenv(facadeBenchEnv, "")
	if facadeBench() {
		t.Fatal("tenu sans LOKI_BENCH")
	}
	for p := range facadeBenchRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if w.Body.String() != "go" {
			t.Fatalf("sans LOKI_BENCH, %s : %q", p, w.Body.String())
		}
	}
	t.Setenv(facadeBenchEnv, " RUST")
	if len(facadeBenchRoutes) != 4 {
		t.Fatalf("routes : %v", facadeBenchRoutes)
	}
	for p := range facadeBenchRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if !strings.Contains(w.Body.String(), "facade") {
			t.Fatalf("%s non relayé : %q", p, w.Body.String())
		}
	}
	mu.Lock()
	n := len(calls)
	mu.Unlock()
	if n != len(facadeBenchRoutes) {
		t.Fatalf("relais : %v", calls)
	}
	w := httptest.NewRecorder()
	withFacadeChat("/api/tune", local)(w, httptest.NewRequest("POST", "/api/tune", nil))
	if w.Body.String() != "go" {
		t.Fatalf("l'optimiseur reste au Go : %q", w.Body.String())
	}

	// Le bail « bench » : le verrou, le refus du bench pour les autres.
	post := func(h http.HandlerFunc, path, body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	code, out := post(handleInternalLease, "/internal/v1/engine/lease", `{"ttl_ms":60000,"why":"bench"}`)
	if code != 200 {
		t.Fatalf("bail : %d %v", code, out)
	}
	id, _ := out["lease"].(string)
	if !benchRunning() || conv.busyReason() != errBenchBusy {
		t.Fatalf("bail du bench : running=%v refus=%v", benchRunning(), conv.busyReason())
	}
	if code, out := post(handleInternalLease, "/internal/v1/engine/lease", `{}`); code != 409 || out["error"] != errBenchBusy.Error() {
		t.Fatalf("second bail : %d %v", code, out)
	}
	// Changer de discussion ne rompt pas le bail d'un bench.
	conv.loadFrom("autre", nil)
	if !benchRunning() || !conv.isGenerating() {
		t.Fatal("le bail du bench a sauté à la bascule de discussion")
	}
	if code, _ := post(handleInternalLeaseRelease, "/internal/v1/engine/lease/release", `{"lease":"`+id+`"}`); code != 204 {
		t.Fatalf("libération : %d", code)
	}
	if benchRunning() || conv.isGenerating() || conv.busyReason() == errBenchBusy {
		t.Fatal("bail rendu : le verrou devait être libre")
	}
	// Un bail ordinaire n'est pas un bench ; un bail de bench expiré rend
	// le verrou et le refus.
	code, out = post(handleInternalLease, "/internal/v1/engine/lease", `{}`)
	if code != 200 || benchRunning() {
		t.Fatalf("bail ordinaire : %d %v bench=%v", code, out, benchRunning())
	}
	post(handleInternalLeaseRelease, "/internal/v1/engine/lease/release", `{"lease":"`+out["lease"].(string)+`"}`)
	conv.mu.Lock()
	conv.extLeaseClearLocked()
	conv.Generating = false
	conv.mu.Unlock()
	if _, err := conv.extLeaseAcquire(5*time.Second, true); err != nil {
		t.Fatal(err)
	}
	conv.mu.Lock()
	st := conv.extLease
	conv.mu.Unlock()
	conv.extLeaseExpire(st)
	if benchRunning() || conv.busyReason() == errBenchBusy {
		t.Fatal("bail de bench expiré : refus du bench encore là")
	}

	// Le travail annexe ouvert puis fermé par la façade.
	code, out = post(handleInternalSideJob, "/internal/v1/engine/sidejob", `{}`)
	job, _ := out["id"].(float64)
	if code != 200 || job <= 0 || sideJobs.Load() != 1 {
		t.Fatalf("ouverture : %d %v (en cours : %d)", code, out, sideJobs.Load())
	}
	served := engineServedCount()
	if code, _ := post(handleInternalSideJob, "/internal/v1/engine/sidejob", `{"end":`+strconv.Itoa(int(job))+`,"served":true}`); code != 204 {
		t.Fatalf("fermeture : %d", code)
	}
	if sideJobs.Load() != 0 || engineServedCount() != served+1 {
		t.Fatalf("après la fermeture : en cours %d, servies %d", sideJobs.Load(), engineServedCount()-served)
	}
	if code, _ := post(handleInternalSideJob, "/internal/v1/engine/sidejob", `{"end":999}`); code != 404 {
		t.Fatalf("travail inconnu : %d", code)
	}
	rec := httptest.NewRecorder()
	handleInternalSideJob(rec, httptest.NewRequest("GET", "/internal/v1/engine/sidejob", nil))
	if rec.Code != 405 {
		t.Fatalf("GET : %d", rec.Code)
	}

	// La trace du bench dans la télémétrie.
	var before uint64
	if recs := perfLog.snapshot(); len(recs) > 0 {
		before = recs[len(recs)-1].Seq
	}
	if code, _ := post(handleInternalBenchPerf, "/internal/v1/bench/perf",
		`{"prompt_n":2036,"prompt_ms":1018,"prompt_per_second":2000,"predicted_n":300,"predicted_per_second":50}`); code != 204 {
		t.Fatalf("trace : %d", code)
	}
	recs := perfLog.snapshot()
	if len(recs) == 0 || recs[len(recs)-1].Seq != before+1 {
		t.Fatalf("trace absente (dernière avant : %d)", before)
	}
	r := recs[len(recs)-1]
	if r.Kind != perfBench || !r.Complete || r.Total != 2036 || r.New != 2036 || r.Gen != 300 || r.TGtps != 50 || r.PPtps != 2000 || r.PPms != 1018 {
		t.Fatalf("trace : %+v", r)
	}

	t.Setenv(internalTokenEnv, "")
	if facadeBench() {
		t.Fatal("tenu sans jeton interne")
	}
}
