//go:build linux

package loki

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// parity_bench_test.go — journal de référence du banc d'essai (lot 27 de la
// migration Rust) : les quatre routes, chaque refus au lancement, le mode
// rapide et le mode complet (profondeur, tours, reprise du cache), les
// échecs d'une mesure, le niveau de raisonnement refusé puis rejoué, la
// profondeur partielle et l'annulation, contre le faux moteur des tests du
// bench (llm_bench_test.go). Chaque requête reçue par le moteur est
// enregistrée — les longs textes (corpus) réduits à leur taille et à leur
// empreinte. Rangé dans rust/loki-rs/tests/parity_bench/bench.json ;
// tests/bench.rs le rejoue par la façade.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteBench

const parityBenchFile = "../../rust/loki-rs/tests/parity_bench/bench.json"

// parityBenchEngine : le réglage du faux moteur pour une étape (remis à zéro
// à chaque étape qui en donne un).
type parityBenchEngine struct {
	NCtx         int    `json:"n_ctx"`
	Busy         bool   `json:"busy,omitempty"`
	FailAt       int    `json:"fail_at,omitempty"`
	NoTiming     bool   `json:"no_timing,omitempty"`
	RejectEffort string `json:"reject_effort,omitempty"`
	SlowHotMs    int    `json:"slow_hot_ms,omitempty"`
	TimeoutMs    int    `json:"timeout_ms,omitempty"` // benchReqTimeout
}

type parityBenchStep struct {
	Name       string             `json:"name"`
	Config     map[string]string  `json:"config,omitempty"`
	Files      map[string]string  `json:"files,omitempty"`
	Engine     *parityBenchEngine `json:"engine,omitempty"`
	Tune       bool               `json:"tune,omitempty"`
	Generating bool               `json:"generating,omitempty"`
	Method     string             `json:"method"`
	Path       string             `json:"path"`
	Body       string             `json:"body,omitempty"`
	Status     int                `json:"status"`
	Out        any                `json:"out"`
	// Again : un second lancement pendant le bench ; CancelAt : la phase
	// (préfixe) à laquelle l'annuler.
	Again    bool   `json:"again,omitempty"`
	AgainOut any    `json:"again_out,omitempty"`
	CancelAt string `json:"cancel_at,omitempty"`
	Wait     bool   `json:"wait,omitempty"`
	Job      any    `json:"job,omitempty"` // état final du job
	Calls    []any  `json:"calls"`         // requêtes reçues par le moteur
	Last     any    `json:"last"`          // state/last_bench
	Presets  any    `json:"presets"`       // state/bench_presets
}

var (
	parityBenchIDRe = regexp.MustCompile(`"b[0-9]+-[0-9]+"`)
	parityBenchTRe  = regexp.MustCompile(`"(elapsed_sec|at)":[0-9.e+-]+`)
	parityBenchFPRe = regexp.MustCompile(`"fingerprint":"[0-9a-f]+"`)
)

// parityBenchDigest : une valeur JSON dont les longs textes sont réduits à
// leur taille et à leur empreinte.
func parityBenchDigest(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = parityBenchDigest(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = parityBenchDigest(e)
		}
		return out
	case string:
		if len(x) > 64 {
			sum := sha256.Sum256([]byte(x))
			return fmt.Sprintf("<%d octets %x>", len(x), sum[:6])
		}
		return x
	}
	return v
}

func TestPariteBench(t *testing.T) {
	home := testHome(t)
	oldTimeout := benchReqTimeout
	// Un état laissé par un autre test du paquet : ni niveau appris, ni bench
	// précédent.
	resetBench := func() {
		effortFallbacks.Range(func(k, _ any) bool {
			effortFallbacks.Delete(k)
			return true
		})
		benchJob.mu.Lock()
		benchJob.running, benchJob.id, benchJob.mode, benchJob.phase = false, "", "", ""
		benchJob.step, benchJob.steps, benchJob.cancel, benchJob.result = 0, 0, nil, nil
		benchJob.err, benchJob.canceled = "", false
		benchJob.mu.Unlock()
	}
	resetBench()
	t.Cleanup(func() {
		benchReqTimeout = oldTimeout
		// Les niveaux de raisonnement appris ici ne valent que pour ce test.
		effortFallbacks.Range(func(k, _ any) bool {
			effortFallbacks.Delete(k)
			return true
		})
	})

	// Le faux moteur : celui des tests du bench, derrière un journal de
	// toutes les requêtes reçues.
	var mu sync.Mutex
	var calls []any
	fake := &fakeBenchEngine{}
	inner := http.Handler(nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		entry := map[string]any{"method": r.Method, "path": r.URL.Path}
		if len(b) > 0 {
			var body any
			if json.Unmarshal(b, &body) == nil {
				entry["body"] = parityBenchDigest(body)
			}
		}
		mu.Lock()
		calls = append(calls, entry)
		h := inner
		mu.Unlock()
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close()

	mask := func(s string) string {
		s = strings.ReplaceAll(s, home, "{{HOME}}")
		s = parityBenchIDRe.ReplaceAllString(s, `"{{ID}}"`)
		s = parityBenchTRe.ReplaceAllString(s, `"$1":"{{T}}"`)
		return parityBenchFPRe.ReplaceAllString(s, `"fingerprint":"{{FP}}"`)
	}
	unmask := func(s string) string {
		s = strings.ReplaceAll(s, "{{PORT}}", port)
		s = strings.ReplaceAll(s, "{{CLOSED}}", closed)
		return strings.ReplaceAll(s, "{{HOME}}", home)
	}
	decode := func(raw string) any {
		var v any
		if raw == "" {
			return nil
		}
		if err := json.Unmarshal([]byte(mask(raw)), &v); err != nil {
			t.Fatalf("illisible : %q", raw)
		}
		return v
	}
	handlers := map[string]http.HandlerFunc{
		"/api/bench":        handleBench,
		"/api/bench/status": handleBenchStatus,
		"/api/bench/cancel": handleBenchCancel,
		"/api/bench/last":   handleBenchLast,
	}
	call := func(method, path, body string) (int, any) {
		rec := httptest.NewRecorder()
		handlers[path](rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec.Code, decode(rec.Body.String())
	}
	status := func() map[string]any {
		_, v := call("GET", "/api/bench/status", "")
		return v.(map[string]any)
	}
	wait := func(name string) {
		deadline := time.Now().Add(60 * time.Second)
		for status()["running"] == true {
			if time.Now().After(deadline) {
				t.Fatalf("%s : le bench ne finit pas", name)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	var steps []parityBenchStep
	run := func(st parityBenchStep) {
		for k, v := range st.Config {
			if err := SetConfigKey(k, unmask(v)); err != nil {
				t.Fatal(err)
			}
		}
		files := map[string]string{}
		for k, v := range st.Files {
			files[k] = unmask(v)
		}
		parityApplyFiles(t, home, files)
		if e := st.Engine; e != nil {
			f := &fakeBenchEngine{nCtx: e.NCtx, busy: e.Busy, failAt: e.FailAt, noTiming: e.NoTiming,
				rejectEffort: e.RejectEffort, slowHot: time.Duration(e.SlowHotMs) * time.Millisecond}
			mu.Lock()
			fake = f
			inner = f.handler(t)
			mu.Unlock()
			benchReqTimeout = oldTimeout
			if e.TimeoutMs > 0 {
				benchReqTimeout = time.Duration(e.TimeoutMs) * time.Millisecond
			}
		}
		mu.Lock()
		calls = nil
		mu.Unlock()
		var lock *tuneLock
		if st.Tune {
			l, err := tuneLockAcquire("web", false)
			if err != nil {
				t.Fatal(err)
			}
			lock = l
		}
		if st.Generating {
			conv.mu.Lock()
			conv.Generating = true
			conv.mu.Unlock()
		}
		st.Status, st.Out = call(st.Method, st.Path, unmask(st.Body))
		if st.Generating {
			conv.mu.Lock()
			conv.Generating = false
			conv.mu.Unlock()
		}
		if lock != nil {
			lock.release()
		}
		if st.Again {
			_, st.AgainOut = call("POST", "/api/bench", "{}")
		}
		if st.CancelAt != "" {
			deadline := time.Now().Add(30 * time.Second)
			for {
				p, _ := status()["phase"].(string)
				if strings.HasPrefix(p, st.CancelAt) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s : phase %q jamais atteinte", st.Name, st.CancelAt)
				}
				time.Sleep(2 * time.Millisecond)
			}
			// Le moteur a reçu la requête de cette phase avant l'annulation.
			for {
				mu.Lock()
				n := len(fake.calls)
				mu.Unlock()
				if n >= 4 {
					break
				}
				time.Sleep(2 * time.Millisecond)
			}
			call("POST", "/api/bench/cancel", "")
		}
		if st.Wait || st.CancelAt != "" {
			wait(st.Name)
			st.Job = status()
		}
		mu.Lock()
		st.Calls = append([]any{}, calls...)
		mu.Unlock()
		st.Last = decode(string(getBytes(bkState, "last_bench")))
		st.Presets = decode(string(getBytes(bkState, "bench_presets")))
		steps = append(steps, st)
	}

	preset := "MODEL=/models/qwen.gguf\nPORT={{PORT}}\nCTX=16384\nTOP_K=40\nMIN_P=0.05\nREASONING=off\nCACHE_ISOLATE=off\n"
	base := map[string]string{"MODEL": "/models/qwen.gguf", "PORT": "{{PORT}}", "CTX": "16384",
		"TOP_K": "40", "MIN_P": "0.05", "REASONING": "off", "CACHE_ISOLATE": "off"}
	for _, st := range []parityBenchStep{
		{Name: "statut sans bench", Config: base, Files: map[string]string{"presets/qwen.env": preset},
			Engine: &parityBenchEngine{NCtx: 16384}, Method: "GET", Path: "/api/bench/status"},
		{Name: "dernier bench : aucun", Method: "GET", Path: "/api/bench/last"},
		{Name: "lancement en GET", Method: "GET", Path: "/api/bench"},
		{Name: "annulation en GET", Method: "GET", Path: "/api/bench/cancel"},
		{Name: "annulation sans bench", Method: "POST", Path: "/api/bench/cancel"},
		{Name: "preset externe", Config: map[string]string{"EXTERNAL": "1"}, Method: "POST", Path: "/api/bench", Body: `{}`},
		{Name: "optimisation en cours", Config: map[string]string{"EXTERNAL": ""}, Tune: true, Method: "POST", Path: "/api/bench", Body: `{}`},
		{Name: "modèle pas prêt", Config: map[string]string{"PORT": "{{CLOSED}}"}, Method: "POST", Path: "/api/bench", Body: `{}`},
		{Name: "génération en cours", Config: map[string]string{"PORT": "{{PORT}}"}, Generating: true, Method: "POST", Path: "/api/bench", Body: `{}`},
		{Name: "moteur occupé par un autre", Engine: &parityBenchEngine{NCtx: 16384, Busy: true},
			Method: "POST", Path: "/api/bench", Body: `{}`, Wait: true},
		{Name: "mode rapide", Engine: &parityBenchEngine{NCtx: 16384}, Method: "POST", Path: "/api/bench", Body: `{}`, Wait: true},
		{Name: "dernier bench", Method: "GET", Path: "/api/bench/last"},
		{Name: "mode complet", Engine: &parityBenchEngine{NCtx: 16384}, Method: "POST", Path: "/api/bench", Body: `{"mode":" FULL "}`, Wait: true},
		{Name: "mode complet, contexte trop petit", Engine: &parityBenchEngine{NCtx: 4096},
			Method: "POST", Path: "/api/bench", Body: `{"mode":"full"}`, Wait: true},
		{Name: "contexte inconnu du moteur : celui du preset", Engine: &parityBenchEngine{NCtx: 0},
			Method: "POST", Path: "/api/bench", Body: `{"mode":"full"}`, Wait: true},
		{Name: "paramètres bornés", Engine: &parityBenchEngine{NCtx: 65536},
			Method: "POST", Path: "/api/bench", Body: `{"prompt":99999,"n":9999,"mode":"quick"}`, Wait: true},
		{Name: "petits paramètres", Engine: &parityBenchEngine{NCtx: 16384},
			Method: "POST", Path: "/api/bench", Body: `{"prompt":100,"n":8}`, Wait: true},
		{Name: "prompt juste au-delà de la prose", Engine: &parityBenchEngine{NCtx: 16384},
			Method: "POST", Path: "/api/bench", Body: `{"prompt":900,"n":16}`, Wait: true},
		{Name: "corps illisible : défauts", Engine: &parityBenchEngine{NCtx: 16384},
			Method: "POST", Path: "/api/bench", Body: `{"prompt":"beaucoup"}`, Wait: true},
		{Name: "le préchauffage échoue", Engine: &parityBenchEngine{NCtx: 16384, FailAt: 1},
			Method: "POST", Path: "/api/bench", Body: `{}`, Wait: true},
		{Name: "la ligne courte échoue", Engine: &parityBenchEngine{NCtx: 16384, FailAt: 2},
			Method: "POST", Path: "/api/bench", Body: `{}`, Wait: true},
		{Name: "le prefill à froid échoue", Engine: &parityBenchEngine{NCtx: 16384, FailAt: 3},
			Method: "POST", Path: "/api/bench", Body: `{"mode":"full"}`, Wait: true},
		{Name: "un tour en profondeur échoue", Engine: &parityBenchEngine{NCtx: 16384, FailAt: 5},
			Method: "POST", Path: "/api/bench", Body: `{"mode":"full"}`, Wait: true},
		{Name: "sans timings", Engine: &parityBenchEngine{NCtx: 16384, NoTiming: true},
			Method: "POST", Path: "/api/bench", Body: `{}`, Wait: true},
		{Name: "niveau de raisonnement refusé puis rejoué",
			Config: map[string]string{"REASONING": "", "REASONING_EFFORT": "high"},
			Engine: &parityBenchEngine{NCtx: 16384, RejectEffort: "high"},
			Method: "POST", Path: "/api/bench", Body: `{}`, Wait: true},
		{Name: "niveau appris : envoyé d'emblée", Engine: &parityBenchEngine{NCtx: 16384, RejectEffort: "high"},
			Method: "POST", Path: "/api/bench", Body: `{}`, Wait: true},
		{Name: "budget d'une requête dépassé : profondeur partielle",
			Config: map[string]string{"REASONING_EFFORT": "", "REASONING": "off"},
			Engine: &parityBenchEngine{NCtx: 16384, SlowHotMs: 400, TimeoutMs: 150},
			Method: "POST", Path: "/api/bench", Body: `{"mode":"full"}`, Wait: true},
		{Name: "annulation pendant la profondeur", Engine: &parityBenchEngine{NCtx: 16384, SlowHotMs: 20000},
			Method: "POST", Path: "/api/bench", Body: `{"mode":"full"}`, Again: true, CancelAt: "tour 1/"},
		{Name: "preset Strata", Config: map[string]string{"ENGINE": "strata"}, Engine: &parityBenchEngine{NCtx: 16384},
			Method: "POST", Path: "/api/bench", Body: `{}`, Wait: true},
		{Name: "dernier bench, inchangé", Config: map[string]string{"ENGINE": ""}, Method: "GET", Path: "/api/bench/last"},
	} {
		run(st)
	}

	got, err := parityJSON(map[string]any{"steps": steps})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll("../../rust/loki-rs/tests/parity_bench", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityBenchFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityBenchFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("le banc d'essai ne produit plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteBench,\n"+
			"puis cargo test --test bench dit si le port Rust suit.\n--- obtenu ---\n%s", parityBenchFile, got)
	}
}
