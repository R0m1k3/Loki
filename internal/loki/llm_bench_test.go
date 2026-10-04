package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeBenchEngine simule llama-server pour le protocole du bench : un compte
// de jetons à 4 octets par jeton, un cache qui reprend le prompt précédent
// quand cache_prompt est vrai, et le journal des requêtes de chat reçues.
type fakeBenchEngine struct {
	mu           sync.Mutex
	nCtx         int
	busy         bool
	failAt       int           // n° (1-based) de la requête de chat qui répond 500 ; 0 = aucune
	noTiming     bool          // réponses sans timings
	rejectEffort string        // niveau de reasoning_effort que le « gabarit » refuse (500)
	slowHot      time.Duration // délai des tours qui reprennent le cache
	prev         int
	calls        []map[string]any
}

func (f *fakeBenchEngine) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Write([]byte(`{"status":"ok"}`))
		case "/slots":
			fmt.Fprintf(w, `[{"id":0,"is_processing":%v}]`, f.busy)
		case "/props":
			fmt.Fprintf(w, `{"build_info":"b9999-test","total_slots":1,"default_generation_settings":{"n_ctx":%d}}`, f.nCtx)
		case "/tokenize":
			var b struct {
				Content string `json:"content"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			toks := make([]int, len(b.Content)/4)
			json.NewEncoder(w).Encode(map[string]any{"tokens": toks})
		case "/v1/chat/completions":
			var p map[string]any
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				t.Errorf("corps illisible : %v", err)
			}
			f.mu.Lock()
			f.calls = append(f.calls, p)
			n := len(f.calls)
			f.mu.Unlock()
			if f.failAt == n {
				http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
				return
			}
			if e, _ := p["reasoning_effort"].(string); f.rejectEffort != "" && e == f.rejectEffort {
				// Le vrai message cite la trace jinja d'abord : les niveaux acceptés
				// arrivent bien après les 300 premiers caractères.
				http.Error(w, `{"error":{"code":500,"message":"`+strings.Repeat("jinja trace ", 40)+
					`Unexpected reasoning effort `+e+`. Supported types are xhigh (default), medium, and low."}}`, http.StatusInternalServerError)
				return
			}
			cache, _ := p["cache_prompt"].(bool)
			if d := f.slowHot; cache && d > 0 {
				select {
				case <-time.After(d):
				case <-r.Context().Done():
					return
				}
			}
			total := 0
			for _, m := range p["messages"].([]any) {
				c, _ := m.(map[string]any)["content"].(string)
				total += len(c)/4 + 4
			}
			cached := 0
			f.mu.Lock()
			if cache {
				cached = min(f.prev, total)
			}
			f.prev = total
			f.mu.Unlock()
			gen := int(p["max_tokens"].(float64))
			resp := map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "réponse"}}},
				"usage":   map[string]any{"prompt_tokens": total, "completion_tokens": gen},
			}
			if !f.noTiming {
				resp["timings"] = map[string]any{
					"prompt_n": total - cached, "prompt_ms": float64(total-cached) / 2, "prompt_per_second": 2000.0,
					"predicted_n": gen, "predicted_ms": float64(gen) * 20, "predicted_per_second": 50.0,
					"cache_n": cached, "draft_n": 10, "draft_n_accepted": 7,
				}
			}
			json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	})
}

func (f *fakeBenchEngine) start(t *testing.T) (benchEngine, *httptest.Server) {
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return benchEngine{base: srv.URL, client: srv.Client()}, srv
}

// testBenchCorpora : un code de 200 Ko sans aucune ligne répétée.
func testBenchCorpora() benchCorpora {
	var b strings.Builder
	for i := 0; b.Len() < 200<<10; i++ {
		fmt.Fprintf(&b, "const ligne%06d = « contenu distinct %d » ;\n", i, i*7)
	}
	return benchCorpora{prose: benchCorpus, code: b.String()}
}

func testBenchSetup() benchSetup {
	return benchSetupFrom(map[string]string{"TOP_K": "40", "MIN_P": "0.05", "REASONING": "off"}, nil)
}

// Le protocole complet, requête par requête : préchauffage, ligne courte et
// prefill à froid sans cache, puis trois tours qui prolongent la MÊME
// discussion avec le cache — et l'échantillonnage du preset partout.
func TestBenchProtocoleComplet(t *testing.T) {
	f := &fakeBenchEngine{nCtx: 16384}
	eng, _ := f.start(t)
	var phases []string
	res, err := benchRun(context.Background(), eng, benchOpts{Mode: benchModeFull}, testBenchSetup(), testBenchCorpora(),
		func(phase string, step, steps int) {
			phases = append(phases, fmt.Sprintf("%d/%d %s", step, steps, phase))
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 6 {
		t.Fatalf("%d requêtes de chat, attendu 6 (préchauffage, ligne courte, froid, 3 tours)", len(f.calls))
	}
	wantCache := []bool{false, false, false, true, true, true}
	wantMax := []float64{benchWarmGen, 300, benchGenTokens, benchGenTokens, benchGenTokens, benchGenTokens}
	wantMsgs := []int{1, 1, 1, 3, 5, 7}
	for i, p := range f.calls {
		if p["cache_prompt"] != wantCache[i] {
			t.Errorf("requête %d : cache_prompt=%v, attendu %v", i+1, p["cache_prompt"], wantCache[i])
		}
		if p["max_tokens"] != wantMax[i] {
			t.Errorf("requête %d : max_tokens=%v, attendu %v", i+1, p["max_tokens"], wantMax[i])
		}
		msgs := p["messages"].([]any)
		if len(msgs) != wantMsgs[i] {
			t.Errorf("requête %d : %d messages, attendu %d", i+1, len(msgs), wantMsgs[i])
		}
		for j, m := range msgs {
			want := "user"
			if j%2 == 1 {
				want = "assistant"
			}
			if role := m.(map[string]any)["role"]; role != want {
				t.Errorf("requête %d, message %d : rôle %v, attendu %s", i+1, j, role, want)
			}
		}
		if p["top_k"] != float64(40) || p["min_p"] != 0.05 || p["seed"] != float64(benchSeed) {
			t.Errorf("requête %d : échantillonnage du preset ou seed absents : %v", i+1, p)
		}
		if _, ok := p["ignore_eos"]; ok {
			t.Errorf("requête %d : ignore_eos envoyé — génération hors distribution", i+1)
		}
		if kw, _ := p["chat_template_kwargs"].(map[string]any); kw["enable_thinking"] != false {
			t.Errorf("requête %d : REASONING=off non transmis au gabarit : %v", i+1, p["chat_template_kwargs"])
		}
	}
	// Chaque tour apporte du code JAMAIS vu : aucun morceau ne se répète.
	seen := map[string]bool{}
	for _, m := range f.calls[5]["messages"].([]any) {
		c := m.(map[string]any)["content"].(string)
		if c != "réponse" && seen[c] {
			t.Error("un morceau de corpus est renvoyé deux fois")
		}
		seen[c] = true
	}
	if len(phases) != 6 || !strings.HasPrefix(phases[5], "6/6 ") {
		t.Errorf("progression inattendue : %v", phases)
	}
	d := res.Depth
	if d == nil || d.Skipped != "" || d.Partial != "" || d.Cold == nil || len(d.Turns) != benchTurns {
		t.Fatalf("phase en profondeur incomplète : %+v", d)
	}
	if want, _ := benchDepthFor(16384, false, 2*512+benchWarmGen); d.Target != want || d.Cold.New < want*9/10 {
		t.Errorf("profondeur : visée %d (attendu %d), froid %d jetons", d.Target, want, d.Cold.New)
	}
	if d.Reuse < 0.99 || d.ReuseNote != "" {
		t.Errorf("reprise du cache : %v %q, attendu complète", d.Reuse, d.ReuseNote)
	}
	if d.DecodePerSec != 50 || d.CachedPerSec != 2000 {
		t.Errorf("agrégats : decode %v, prefill %v", d.DecodePerSec, d.CachedPerSec)
	}
	if d.Draft == nil || d.Draft.N != 30 || d.Draft.Accepted != 21 || res.Draft == nil || res.Draft.N != 10 {
		t.Errorf("acceptation du brouillon : profondeur %+v, ligne courte %+v", d.Draft, res.Draft)
	}
	if res.Engine != "b9999-test" || res.Protocol != benchProtocol || res.Mode != benchModeFull || !benchSavable(res) {
		t.Errorf("métadonnées : %+v", res)
	}
}

// Le mode rapide s'arrête à la ligne courte.
func TestBenchModeRapide(t *testing.T) {
	f := &fakeBenchEngine{nCtx: 16384}
	eng, _ := f.start(t)
	res, err := benchRun(context.Background(), eng, benchOpts{Mode: benchModeQuick}, testBenchSetup(), testBenchCorpora(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 || res.Depth != nil || res.PredictedPerSec != 50 {
		t.Fatalf("%d requêtes, profondeur %+v, decode %v", len(f.calls), res.Depth, res.PredictedPerSec)
	}
}

// Une réponse non-200 ou sans timings n'est jamais une mesure : erreur, donc
// rien d'enregistré (runBench n'enregistre qu'un résultat sans erreur).
func TestBenchEchecsNonEnregistres(t *testing.T) {
	for _, c := range []struct {
		name string
		f    *fakeBenchEngine
		want string
	}{
		{"500 sur la ligne courte", &fakeBenchEngine{nCtx: 16384, failAt: 2}, "ligne courte"},
		{"500 sur un tour", &fakeBenchEngine{nCtx: 16384, failAt: 5}, "tour 2"},
		{"sans timings", &fakeBenchEngine{nCtx: 16384, noTiming: true}, errBenchNoTimings.Error()},
		{"moteur occupé", &fakeBenchEngine{nCtx: 16384, busy: true}, "déjà une requête"},
	} {
		t.Run(c.name, func(t *testing.T) {
			eng, _ := c.f.start(t)
			res, err := benchRun(context.Background(), eng, benchOpts{Mode: benchModeFull}, testBenchSetup(), testBenchCorpora(), nil)
			if err == nil || res != nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("res=%v err=%v, attendu une erreur contenant %q", res, err, c.want)
			}
			if c.f.busy && len(c.f.calls) != 0 {
				t.Error("moteur occupé : aucune requête ne devait partir")
			}
		})
	}
}

// Contexte trop petit : la profondeur est sautée, la ligne courte reste.
func TestBenchProfondeurSautee(t *testing.T) {
	f := &fakeBenchEngine{nCtx: 8192}
	eng, _ := f.start(t)
	res, err := benchRun(context.Background(), eng, benchOpts{Mode: benchModeFull}, testBenchSetup(), testBenchCorpora(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Depth == nil || res.Depth.Skipped == "" || len(f.calls) != 2 || !benchSavable(res) {
		t.Fatalf("profondeur %+v, %d requêtes", res.Depth, len(f.calls))
	}
}

// Un tour qui dépasse son budget rend un résultat PARTIEL, montré mais jamais
// enregistré.
func TestBenchBudgetPartiel(t *testing.T) {
	old := benchReqTimeout
	benchReqTimeout = 200 * time.Millisecond
	t.Cleanup(func() { benchReqTimeout = old })
	f := &fakeBenchEngine{nCtx: 16384, slowHot: 2 * time.Second}
	eng, _ := f.start(t)
	res, err := benchRun(context.Background(), eng, benchOpts{Mode: benchModeFull}, testBenchSetup(), testBenchCorpora(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Depth == nil || !strings.Contains(res.Depth.Partial, "tour 1") || benchSavable(res) {
		t.Fatalf("attendu un résultat partiel non enregistrable : %+v", res.Depth)
	}
}

// Annulé : erreur, rien d'enregistré.
func TestBenchAnnule(t *testing.T) {
	f := &fakeBenchEngine{nCtx: 16384, slowHot: 5 * time.Second}
	eng, _ := f.start(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	res, err := benchRun(ctx, eng, benchOpts{Mode: benchModeFull}, testBenchSetup(), testBenchCorpora(), nil)
	if err == nil || res != nil {
		t.Fatalf("annulation : res=%v err=%v", res, err)
	}
}

func TestBenchDepthFor(t *testing.T) {
	warm := 2*512 + benchWarmGen
	for _, c := range []struct {
		ctx      int
		cpu      bool
		want     int
		skipped  bool
		comments string
	}{
		{131072, false, benchDepthMax, false, "grand contexte : plafond"},
		{131072, true, benchDepthCPU, false, "poids sur CPU : plafond réduit"},
		{65536, false, 32768, false, "moitié du contexte"},
		{16384, false, 16384 - benchTurns*(benchAppendTokens+benchGenTokens) - benchGenTokens - warm - benchMargin, false, "les tours doivent tenir"},
		{8192, false, 0, true, "trop petit"},
		{0, false, 0, true, "inconnu"},
	} {
		got, why := benchDepthFor(c.ctx, c.cpu, warm)
		if got != c.want || (why != "") != c.skipped {
			t.Errorf("%s : benchDepthFor(%d) = %d %q", c.comments, c.ctx, got, why)
		}
		if got > 0 && got+benchTurns*(benchAppendTokens+benchGenTokens)+benchGenTokens > c.ctx {
			t.Errorf("%s : %d + tours déborde %d", c.comments, got, c.ctx)
		}
	}
}

func TestBenchKVLabel(t *testing.T) {
	for _, c := range []struct {
		k, v, label string
		quant       bool
	}{
		{"", "", "f16", false},
		{"f16", "", "f16", false},
		{"bf16", "bf16", "bf16", false},
		{"q8_0", "q8_0", "q8_0", true},
		{"q8_0", "", "q8_0/f16", true},
		{"", "q4_0", "f16/q4_0", true},
	} {
		label, quant := benchKVLabel(c.k, c.v)
		if label != c.label || quant != c.quant {
			t.Errorf("benchKVLabel(%q,%q) = %q,%v ; attendu %q,%v", c.k, c.v, label, quant, c.label, c.quant)
		}
	}
	// Le -ctk d'EXTRA_ARGS l'emporte, comme au lancement.
	s := benchSetupFrom(map[string]string{"KV_TYPE": "f16", "EXTRA_ARGS": "-ctk q8_0 -ctv q8_0 --n-cpu-moe 20"}, nil)
	if s.kv != "q8_0" || !s.kvQuant || !s.cpuPlaced {
		t.Errorf("réglages tirés du preset : %+v", s)
	}
}

func TestBenchPayload(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  map[string]string
		want map[string]any
		none []string
	}{
		{"échantillonnage du preset", map[string]string{"TEMP": "0.6", "TOP_P": "0.95", "TOP_K": "20"},
			map[string]any{"temperature": 0.6, "top_p": 0.95, "top_k": 20, "seed": benchSeed}, []string{"chat_template_kwargs", "reasoning_effort"}},
		{"sans réglage", map[string]string{},
			map[string]any{"temperature": 0.7, "seed": benchSeed}, []string{"top_k", "chat_template_kwargs"}},
		{"effort explicite", map[string]string{"REASONING_EFFORT": "high"},
			map[string]any{"reasoning_effort": "high"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			testHome(t) // effortResolve lit le modèle de la configuration
			p := benchSetupFrom(c.cfg, nil).payload([]Message{{Role: "user", Content: "x"}}, 10, true)
			for k, v := range c.want {
				if fmt.Sprint(p[k]) != fmt.Sprint(v) {
					t.Errorf("%s = %v, attendu %v", k, p[k], v)
				}
			}
			for _, k := range append(c.none, "ignore_eos") {
				if _, ok := p[k]; ok {
					t.Errorf("%s ne devait pas être envoyé : %v", k, p[k])
				}
			}
			if p["cache_prompt"] != true || p["stream"] != false {
				t.Errorf("cache_prompt/stream : %v", p)
			}
		})
	}
}

func TestBenchSlice(t *testing.T) {
	text := strings.Repeat("ééé ligne\n", 100)
	off := 0
	for off < len(text) {
		chunk, next := benchSlice(text, off, 37)
		if chunk == "" || !utf8.ValidString(chunk) {
			t.Fatalf("morceau invalide à %d : %q", off, chunk)
		}
		if next < len(text) && !strings.HasSuffix(chunk, "\n") {
			t.Errorf("morceau coupé hors fin de ligne : %q", chunk)
		}
		off = next
	}
	if c, _ := benchSlice(text, len(text), 10); c != "" {
		t.Error("corpus épuisé : morceau vide attendu")
	}
	if got := benchLineStart("ab\ncd", 1); got != 3 {
		t.Errorf("benchLineStart = %d", got)
	}
}

func TestBenchReuse(t *testing.T) {
	full := []benchTurn{{Cached: 1000, Expected: 1004}, {Cached: 3000, Expected: 3000}}
	if r, note := benchReuse(full, false); r < 0.99 || note != "" {
		t.Errorf("reprise complète : %v %q", r, note)
	}
	part := []benchTurn{{Cached: 512, Expected: 3000}}
	if _, note := benchReuse(part, true); !strings.Contains(note, "hybride") {
		t.Errorf("hybride : %q", note)
	}
	if _, note := benchReuse(part, false); !strings.Contains(note, "gabarit") {
		t.Errorf("non hybride : %q", note)
	}
	if r, note := benchReuse([]benchTurn{{Cached: -1, Expected: 3000}}, false); r != -1 || note == "" {
		t.Errorf("inconnu : %v %q", r, note)
	}
}

func TestBenchThrashHint(t *testing.T) {
	if h := benchThrashHint(100<<20, 256, 0); h != "" {
		t.Errorf("100 Mo / 256 jetons : pas d'indication attendue, %q", h)
	}
	if h := benchThrashHint(3<<30, 768, 60<<30); !strings.Contains(h, "thrash") {
		t.Errorf("1 Go / 256 jetons : indication attendue, %q", h)
	}
}

// Une mesure enregistrée ne s'affiche que pour la configuration et les cartes
// sur lesquelles elle a été prise ; une mesure d'avant l'empreinte retombe sur
// le nom du modèle.
func TestBenchEmpreinte(t *testing.T) {
	cfg := map[string]string{"MODEL": "/models/Q.gguf", "CTX": "32768", "CUDA_VISIBLE_DEVICES": "0,1"}
	res := &benchResult{PromptPerSecond: 1, Depth: &benchDepth{Hint: "possible thrash"}}
	sb := newSavedBench(res, cfg)
	if sb.Fingerprint == "" || sb.Result.Depth.Hint != "" || res.Depth.Hint == "" {
		t.Fatalf("enregistrement : %+v (l'indication ne s'enregistre pas, le résultat montré la garde)", sb)
	}
	preset := map[string]string{"MODEL": "/models/Q.gguf", "CTX": "32768"}
	if !benchMatchesPreset(sb, preset, cfg) {
		t.Error("même configuration, mêmes cartes : la pastille doit s'afficher")
	}
	if benchMatchesPreset(sb, map[string]string{"MODEL": "/models/Q.gguf", "CTX": "65536"}, cfg) {
		t.Error("contexte différent : pastille masquée attendue")
	}
	if benchMatchesPreset(sb, preset, map[string]string{"CUDA_VISIBLE_DEVICES": "1,0"}) {
		t.Error("cartes dans un autre ordre : pastille masquée attendue")
	}
	own := map[string]string{"MODEL": "/models/Q.gguf", "CTX": "32768", "CUDA_VISIBLE_DEVICES": "0,1"}
	if !benchMatchesPreset(sb, own, map[string]string{"CUDA_VISIBLE_DEVICES": "0"}) {
		t.Error("preset qui impose ses cartes : ce sont elles qui comptent")
	}
	legacy := savedBench{Model: "Q.gguf"}
	if !benchMatchesPreset(legacy, preset, cfg) {
		t.Error("mesure d'avant l'empreinte : repli sur le nom du modèle")
	}
}

// Le lancement par l'interface : POST seulement, 409 pendant une génération,
// puis un seul bench à la fois, qui tient le verrou du chat et s'annule.
func TestBenchHandlers(t *testing.T) {
	testHome(t)
	f := &fakeBenchEngine{nCtx: 16384}
	_, srv := f.start(t)
	u, _ := url.Parse(srv.URL)
	if err := WriteConfig(map[string]string{"PORT": u.Port()}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	old := benchRunner
	benchRunner = func(ctx context.Context, o benchOpts, p benchProgress) (*benchResult, error) {
		p("mesure", 1, 2)
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	t.Cleanup(func() { benchRunner = old })

	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		switch path {
		case "/api/bench":
			handleBench(rec, req)
		case "/api/bench/cancel":
			handleBenchCancel(rec, req)
		default:
			handleBenchStatus(rec, req)
		}
		return rec
	}
	if rec := do(http.MethodGet, "/api/bench", ""); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET : %d, Allow=%q", rec.Code, rec.Header().Get("Allow"))
	}

	conv.mu.Lock()
	conv.Generating = true
	conv.mu.Unlock()
	rec := do(http.MethodPost, "/api/bench", `{"mode":"full"}`)
	conv.mu.Lock()
	conv.Generating = false
	conv.mu.Unlock()
	if rec.Code != http.StatusConflict {
		t.Fatalf("pendant une génération : %d, attendu 409", rec.Code)
	}

	if rec := do(http.MethodPost, "/api/bench", `{"mode":"full"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("lancement : %d %s", rec.Code, rec.Body)
	}
	<-started
	if rec := do(http.MethodPost, "/api/bench", `{}`); rec.Code != http.StatusConflict {
		t.Errorf("second bench : %d, attendu 409", rec.Code)
	}
	if err := conv.StartTurn("bonjour", nil, Caps{}, 0); err != ErrBusy {
		t.Errorf("tour de chat pendant le bench : %v, attendu ErrBusy", err)
	}
	if err := conv.busyReason(); err != errBenchBusy {
		t.Errorf("motif du refus : %v", err)
	}
	if queued, err := conv.EnqueueOrStart("cid-bench", "x", nil, Caps{}, 0); queued || err == nil {
		t.Errorf("message pendant le bench : mis en file=%v err=%v, attendu un refus", queued, err)
	}
	var st map[string]any
	json.Unmarshal(do(http.MethodGet, "/api/bench/status", "").Body.Bytes(), &st)
	if st["running"] != true || st["phase"] != "mesure" || st["mode"] != benchModeFull {
		t.Errorf("statut en cours : %v", st)
	}
	if rec := do(http.MethodPost, "/api/bench/cancel", ""); rec.Code != 200 {
		t.Fatalf("annulation : %d", rec.Code)
	}
	deadline := time.Now().Add(3 * time.Second)
	for benchRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	json.Unmarshal(do(http.MethodGet, "/api/bench/status", "").Body.Bytes(), &st)
	if st["running"] != false || st["canceled"] != true {
		t.Errorf("statut après annulation : %v", st)
	}
	if conv.isGenerating() {
		t.Error("verrou de génération non rendu après le bench")
	}
}

// Les clients /v1 reçoivent un 503 à retenter pendant un benchmark.
func TestBenchRefuseV1(t *testing.T) {
	benchJob.mu.Lock()
	benchJob.running = true
	benchJob.mu.Unlock()
	t.Cleanup(func() {
		benchJob.mu.Lock()
		benchJob.running = false
		benchJob.mu.Unlock()
	})
	rec := httptest.NewRecorder()
	oaiHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("pendant le bench : %d, Retry-After=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

// Un gabarit qui refuse le niveau de raisonnement du preset : le bench rejoue
// une fois avec le niveau accepté, comme le chat, et le retient — au lieu
// d'échouer sur le 500.
func TestBenchNiveauRaisonnementRefuse(t *testing.T) {
	f := &fakeBenchEngine{nCtx: 16384, rejectEffort: "high"}
	eng, _ := f.start(t)
	s := benchSetupFrom(map[string]string{"REASONING_EFFORT": "high"}, nil)
	var learned []string
	s.learn = func(want, got string) { learned = append(learned, want+">"+got) }
	res, err := benchRun(context.Background(), eng, benchOpts{Mode: benchModeQuick}, s, testBenchCorpora(), nil)
	if err != nil {
		t.Fatalf("refus du niveau non rattrapé : %v", err)
	}
	if len(f.calls) != 3 || res.PredictedPerSec != 50 {
		t.Fatalf("%d requêtes (attendu 3 : refus, préchauffage rejoué, ligne courte)", len(f.calls))
	}
	for i, p := range f.calls[1:] {
		kw, _ := p["chat_template_kwargs"].(map[string]any)
		if p["reasoning_effort"] != "xhigh" || kw["reasoning_effort"] != "xhigh" {
			t.Errorf("requête %d : niveau %v / %v, attendu xhigh", i+2, p["reasoning_effort"], kw)
		}
	}
	if len(learned) != 1 || learned[0] != "high>xhigh" {
		t.Errorf("traduction retenue : %v", learned)
	}
	// Une autre erreur que ce refus n'est pas rejouée.
	f2 := &fakeBenchEngine{nCtx: 16384, failAt: 1}
	eng2, _ := f2.start(t)
	if _, err := benchRun(context.Background(), eng2, benchOpts{}, benchSetupFrom(map[string]string{"REASONING_EFFORT": "high"}, nil), testBenchCorpora(), nil); err == nil || len(f2.calls) != 1 {
		t.Errorf("500 ordinaire : err=%v, %d requêtes", err, len(f2.calls))
	}
}

// Un gros ubatch sur un petit contexte : le préchauffage reste sous le quart
// de la fenêtre au lieu de la dépasser.
func TestBenchPrechauffageBorne(t *testing.T) {
	f := &fakeBenchEngine{nCtx: 8192}
	eng, _ := f.start(t)
	s := benchSetupFrom(map[string]string{"UBATCH": "4096"}, nil)
	if _, err := benchRun(context.Background(), eng, benchOpts{}, s, testBenchCorpora(), nil); err != nil {
		t.Fatal(err)
	}
	c := f.calls[0]["messages"].([]any)[0].(map[string]any)["content"].(string)
	if toks := len(c) / 4; toks > 8192/4+64 {
		t.Errorf("préchauffage de %d jetons sur 8192 de contexte", toks)
	}
}

// Changer de discussion n'arrête pas le bench et ne rend pas le verrou ; un
// Reset l'arrête et le rend, et la libération tardive du bench ne déclare pas
// libre le tour qui a démarré depuis.
func TestBenchVerrouEtDiscussions(t *testing.T) {
	testHome(t)
	c := newTestConv()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release, err := c.benchLease(cancel)
	if err != nil {
		t.Fatal(err)
	}
	c.loadFrom("autre", nil)
	if ctx.Err() != nil || !c.isGenerating() || c.busyReason() != errBenchBusy {
		t.Fatal("changer de discussion a arrêté le bench ou rendu son verrou")
	}
	release()
	if c.isGenerating() {
		t.Fatal("verrou non rendu à la fin du bench, après un changement de discussion")
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	release2, err := c.benchLease(cancel2)
	if err != nil {
		t.Fatal(err)
	}
	c.Reset()
	if ctx2.Err() == nil || c.isGenerating() {
		t.Fatal("Reset doit arrêter le bench et rendre le verrou")
	}
	c.mu.Lock()
	c.Generating = true // un tour démarré depuis
	c.mu.Unlock()
	release2()
	if !c.isGenerating() {
		t.Fatal("la libération tardive du bench a déclaré libre le tour suivant")
	}
}
