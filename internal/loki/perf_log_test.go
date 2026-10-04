package loki

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// freshPerf isole l'anneau de télémétrie d'un test.
func freshPerf(t *testing.T) {
	t.Helper()
	old := perfLog
	perfLog = &perfStore{}
	t.Cleanup(func() { perfLog = old })
}

func iptr(v int) *int { return &v }

// Chunk final d'un llama-server récent, en flux, avec MTP actif : choices
// vide, timings et usage à la racine.
const llamaFinalChunk = `{"choices":[],"created":1,"id":"x","model":"m","object":"chat.completion.chunk",` +
	`"usage":{"completion_tokens":120,"prompt_tokens":41484,"total_tokens":41604,"prompt_tokens_details":{"cached_tokens":41230}},` +
	`"timings":{"cache_n":41230,"prompt_n":254,"prompt_ms":310.2,"prompt_per_token":1.2,"prompt_per_second":818.8,` +
	`"predicted_n":120,"predicted_ms":2400,"predicted_per_token":20,"predicted_per_second":50,"draft_n":90,"draft_n_accepted":71}}`

func TestPerfWireDecode(t *testing.T) {
	cases := []struct {
		name                 string
		data                 string
		cache, draft, accept *int
	}{
		{"chunk final llama.cpp", llamaFinalChunk, iptr(41230), iptr(90), iptr(71)},
		// Sans brouillon, llama.cpp n'émet pas draft_n : inconnu, pas 0.
		{"sans brouillon", `{"timings":{"cache_n":12,"prompt_n":3}}`, iptr(12), nil, nil},
		// Tout recalculé : un 0 CONNU, qu'il faut pouvoir montrer.
		{"cache vide", `{"timings":{"cache_n":0,"prompt_n":3000}}`, iptr(0), nil, nil},
		// API tierce : seulement le format OpenAI.
		{"usage seul", `{"usage":{"prompt_tokens":900,"prompt_tokens_details":{"cached_tokens":512}}}`, iptr(512), nil, nil},
		{"moteur muet", `{"usage":{"prompt_tokens":900}}`, nil, nil, nil},
		// Types de travers (fork, passerelle) : lu si c'est un nombre, sinon inconnu.
		{"types de travers", `{"timings":{"cache_n":"abc","draft_n":"12","draft_n_accepted":7.0}}`, nil, iptr(12), iptr(7)},
		// Octets parasites après la réponse (proxy) : la première valeur compte.
		{"octets parasites", `{"timings":{"cache_n":12}}` + "\nxyz", iptr(12), nil, nil},
		{"structure de travers", `{"timings":"nope","usage":{"prompt_tokens_details":{"cached_tokens":5.9}}}`, iptr(5), nil, nil},
	}
	eq := func(a, b *int) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var s StatsEvent
			s.applyPerf(decodePerfWire([]byte(c.data)))
			if !eq(s.CacheTokens, c.cache) || !eq(s.DraftN, c.draft) || !eq(s.DraftAccepted, c.accept) {
				t.Fatalf("cache=%v draft=%v accepted=%v", s.CacheTokens, s.DraftN, s.DraftAccepted)
			}
		})
	}
}

// Le chunk final peut aussi porter le dernier texte et le finish_reason (forks,
// API compatibles). Un compteur de télémétrie mal typé ne doit pas le faire
// sauter : avant, cached_tokens strictement entier jetait tout le chunk.
func TestStreamChunkIgnoresOddTelemetry(t *testing.T) {
	data := `{"choices":[{"delta":{"content":"fin"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":500,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":480.5}},` +
		`"timings":{"prompt_n":20,"predicted_n":3,"draft_n":"x","cache_n":480.0}}`
	var c streamChunk
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		t.Fatalf("chunk jeté : %v", err)
	}
	if len(c.Choices) != 1 || c.Choices[0].Delta.Content != "fin" || c.Choices[0].FinishReason != "stop" || c.Usage.PromptTokens != 500 {
		t.Fatalf("chunk mal lu : %+v", c)
	}
	var s StatsEvent
	s.applyPerf(decodePerfWire([]byte(data)))
	if s.CacheTokens == nil || *s.CacheTokens != 480 || s.DraftN != nil {
		t.Fatalf("télémétrie : cache=%v draft=%v", s.CacheTokens, s.DraftN)
	}
}

// De bout en bout : le dernier événement Stats porte la télémétrie, le texte et
// le comptage du contexte sont intacts, et rien n'est ajouté à la requête.
func TestRunChatPerfFinalStats(t *testing.T) {
	testHome(t)
	freshPerf(t)
	var payload map[string]any
	body := sseChunk("bonjour") +
		`data: {"choices":[{"delta":{"content":" toi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":500,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":480.5}},"timings":{"prompt_n":20,"prompt_ms":10,"prompt_per_second":2000,"predicted_n":2,"predicted_ms":40,"predicted_per_second":50,"draft_n":"x"}}` + "\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}
	var content strings.Builder
	var last *StatsEvent
	ctx := withPerf(context.Background(), perfMain, "c1")
	if _, err := runChat(ctx, []Message{{Role: "user", Content: "salut"}}, 0.7, Caps{}, func(ev StreamEvent) bool {
		if ev.Content != "" {
			content.WriteString(ev.Content)
		}
		if ev.Stats != nil {
			last = ev.Stats
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if content.String() != "bonjour toi" {
		t.Fatalf("texte : %q", content.String())
	}
	if last == nil || last.PromptTokensTotal != 500 || last.Kind != perfMain || last.CacheTokens == nil || *last.CacheTokens != 480 || last.TTFTms == nil {
		t.Fatalf("dernier Stats : %+v", last)
	}
	for _, k := range []string{"timings_per_token", "return_progress", "n_probs", "id_slot", "cache_prompt"} {
		if _, ok := payload[k]; ok {
			t.Fatalf("la télémétrie a ajouté %q à la requête", k)
		}
	}
	recs := perfLog.snapshot()
	if len(recs) != 1 || !recs[0].Complete || recs[0].Conv != "c1" || recs[0].Total != 500 || recs[0].New != 20 {
		t.Fatalf("enregistrement : %+v", recs)
	}
}

// Un flux coupé n'a ni cache ni total fiables : rangé incomplet, sans perte.
func TestRunChatPerfCutStreamIncomplete(t *testing.T) {
	testHome(t)
	freshPerf(t)
	port := sseCuttingServer(t, sseChunk("début"), true)
	if err := SetConfigKey("PORT", port); err != nil {
		t.Fatal(err)
	}
	_, _ = runChat(context.Background(), []Message{{Role: "user", Content: "x"}}, 0.7, Caps{}, func(StreamEvent) bool { return true })
	recs := perfLog.snapshot()
	if len(recs) != 1 || recs[0].Complete || recs[0].Lost != nil {
		t.Fatalf("flux coupé : %+v", recs)
	}
}

func TestPerfRecordLost(t *testing.T) {
	a := Message{Role: "user", Content: "a"}
	b := Message{Role: "assistant", Content: "b"}
	c := Message{Role: "tool", Content: "c"}
	z := Message{Role: "user", Content: "z"}
	type step struct {
		rec  perfRec
		msgs []Message
	}
	main := func(total int, cached *int) perfRec {
		return perfRec{Kind: perfMain, Conv: "c1", Complete: true, Total: total, Cached: cached}
	}
	cases := []struct {
		name      string
		steps     []step
		lost      *int
		lostAfter string
	}{
		{"prolonge, cache intact", []step{{main(1000, iptr(0)), []Message{a}}, {main(1200, iptr(1000)), []Message{a, b, c}}}, iptr(0), ""},
		{"prolonge, cache perdu", []step{{main(1000, iptr(0)), []Message{a}}, {main(1200, iptr(300)), []Message{a, b, c}}}, iptr(700), ""},
		{"diverge", []step{{main(1000, iptr(0)), []Message{a, b}}, {main(1200, iptr(10)), []Message{z, b, c}}}, nil, ""},
		{"même liste, pas une suite", []step{{main(1000, iptr(0)), []Message{a, b}}, {main(1000, iptr(10)), []Message{a, b}}}, nil, ""},
		{"cache inconnu", []step{{main(1000, iptr(0)), []Message{a}}, {main(1200, nil), []Message{a, b}}}, nil, ""},
		{"complétion coupée", []step{{main(1000, iptr(0)), []Message{a}}, {perfRec{Kind: perfMain, Conv: "c1", Cached: iptr(0)}, []Message{a, b}}}, nil, ""},
		{"autre discussion", []step{{main(1000, iptr(0)), []Message{a}}, {perfRec{Kind: perfMain, Conv: "c2", Complete: true, Total: 1200, Cached: iptr(0)}, []Message{a, b}}}, nil, ""},
		{"un sous-agent ne se compare pas au builder", []step{
			{main(1000, iptr(0)), []Message{a}},
			{perfRec{Kind: perfSubagent, Conv: "c1", Complete: true, Total: 1200, Cached: iptr(0)}, []Message{a, b}},
		}, nil, ""},
		{"perte après sous-agent et client externe", []step{
			{main(1000, iptr(0)), []Message{a}},
			{perfRec{Kind: perfSubagent, Conv: "c1", Complete: true, Total: 300, Cached: iptr(0)}, []Message{z}},
			{perfRec{Kind: perfForeign}, nil},
			{main(1300, iptr(200)), []Message{a, b, c}},
		}, iptr(800), "foreign,subagent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			freshPerf(t)
			var last perfRec
			for _, s := range tc.steps {
				var prefix []uint64
				if s.msgs != nil {
					prefix = perfPrefix(s.msgs)
				}
				last = perfLog.record(s.rec, prefix)
			}
			if (last.Lost == nil) != (tc.lost == nil) || (last.Lost != nil && *last.Lost != *tc.lost) {
				t.Fatalf("lost = %v, attendu %v", last.Lost, tc.lost)
			}
			if last.LostAfter != tc.lostAfter {
				t.Fatalf("lost_after = %q, attendu %q", last.LostAfter, tc.lostAfter)
			}
		})
	}
}

func TestPerfRingBounded(t *testing.T) {
	freshPerf(t)
	for i := 0; i < perfRingMax+10; i++ {
		perfLog.record(perfRec{Kind: perfMain, Conv: string(rune('a' + i%200)), Complete: true, Total: 10}, []uint64{uint64(i)})
	}
	recs := perfLog.snapshot()
	if len(recs) != perfRingMax {
		t.Fatalf("anneau : %d entrées", len(recs))
	}
	for i := 1; i < len(recs); i++ {
		if recs[i].Seq != recs[i-1].Seq+1 {
			t.Fatalf("ordre rompu en %d : %d puis %d", i, recs[i-1].Seq, recs[i].Seq)
		}
	}
	if recs[len(recs)-1].Seq != perfRingMax+10 {
		t.Fatalf("dernière entrée : %d", recs[len(recs)-1].Seq)
	}
	if len(perfLog.convs) > perfConvMax {
		t.Fatalf("état par conversation non borné : %d", len(perfLog.convs))
	}
}

func TestPerfSummarize(t *testing.T) {
	recs := []perfRec{
		{Kind: perfMain, Iter: 0, Complete: true, Total: 4000, Cached: iptr(3000), New: 1000, PPms: 2000, PPtps: 500, Gen: 50, TGtps: 40, DraftN: iptr(40), DraftAcc: iptr(30), TTFTms: ptr64(2100)},
		{Kind: perfMain, Iter: 1, Complete: true, Total: 20000, Cached: iptr(19000), New: 1000, PPms: 1000, PPtps: 1000, Gen: 10, TGtps: 30, Lost: iptr(500), TTFTms: ptr64(1100)},
		{Kind: perfSubagent, Iter: 0, Complete: true, Total: 2000, New: 2000, PPms: 4000, PPtps: 500},
		{Kind: perfMain, Iter: 0},
		{Kind: perfForeign},
	}
	s := perfSummarize(recs)
	if s.Entries != 5 || s.CacheHitRatio == nil || *s.CacheHitRatio != 22000.0/24000.0 {
		t.Fatalf("ratio : %+v", s.CacheHitRatio)
	}
	m := s.Kinds[perfMain]
	if m.Completions != 2 || m.Incomplete != 1 || m.Turns != 1 || m.NewTokensPerTurn != 2000 || m.PrefillSecPerTurn != 3 || m.LostEvents != 1 || m.LostTokens != 500 {
		t.Fatalf("main : %+v", m)
	}
	if m.TTFTMedianMs == nil || *m.TTFTMedianMs != 1600 {
		t.Fatalf("ttft médian : %v", m.TTFTMedianMs)
	}
	if s.Kinds[perfForeign].Incomplete != 1 {
		t.Fatalf("foreign : %+v", s.Kinds[perfForeign])
	}
	if s.DraftRate == nil || *s.DraftRate != 0.75 {
		t.Fatalf("acceptation : %v", s.DraftRate)
	}
	// Par nature aussi : le sous-agent sans brouillon n'a pas de taux inventé.
	if m.DraftRate == nil || *m.DraftRate != 0.75 || m.DraftN != 40 || m.DraftAccepted != 30 {
		t.Fatalf("acceptation main : %+v", m)
	}
	if sa := s.Kinds[perfSubagent]; sa.DraftRate != nil || sa.DraftN != 0 {
		t.Fatalf("acceptation sous-agent : %+v", sa)
	}
	if len(s.Depth) != 4 || s.Depth[0].PPMedianTPS == nil || *s.Depth[0].PPMedianTPS != 500 || s.Depth[2].TGMedianTPS == nil || *s.Depth[2].TGMedianTPS != 30 || s.Depth[3].N != 0 {
		t.Fatalf("profondeurs : %+v", s.Depth)
	}
	// Aucune donnée : pas de ratio inventé.
	if e := perfSummarize(nil); e.CacheHitRatio != nil || e.DraftRate != nil {
		t.Fatalf("vide : %+v", e)
	}
}

func ptr64(v int64) *int64 { return &v }

func TestPerfLostAlert(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]string
		want int
	}{
		{"modèle classique", map[string]string{}, 1000},
		{"espacement posé", map[string]string{"CKPT_MIN_STEP": "2048"}, 2560},
		{"espacement et micro-batch", map[string]string{"CKPT_MIN_STEP": "4096", "UBATCH": "1024"}, 5120},
		{"espacement illisible", map[string]string{"CKPT_MIN_STEP": "beaucoup"}, 1000},
		// Comme au lancement : EXTRA_ARGS l'emporte sur les clés du preset.
		{"-cms d'EXTRA_ARGS", map[string]string{"CKPT_MIN_STEP": "2048", "EXTRA_ARGS": "--checkpoint-min-step 8192 -ub 256"}, 8448},
		{"-ub d'EXTRA_ARGS seul", map[string]string{"CKPT_MIN_STEP": "2048", "UBATCH": "1024", "EXTRA_ARGS": "-ub=2048"}, 4096},
	}
	t.Setenv("LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT", "")
	for _, c := range cases {
		if got := perfLostAlertAt(c.cfg); got != c.want {
			t.Errorf("%s : seuil %d, attendu %d", c.name, got, c.want)
		}
	}
	t.Setenv("LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT", "3000")
	if got := perfLostAlertAt(map[string]string{"CKPT_MIN_STEP": "2048"}); got != 3512 {
		t.Errorf("LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT ignorée : seuil %d", got)
	}
	if perfAlert(perfRec{Lost: iptr(900)}, 1000) || !perfAlert(perfRec{Lost: iptr(1500)}, 1000) {
		t.Fatal("seuil de perte")
	}
	if perfAlert(perfRec{Lost: iptr(2000)}, 2560) {
		t.Fatal("hybride : une perte sous l'espacement des points de reprise est normale")
	}
	if !perfAlert(perfRec{Cached: iptr(0), Total: 9000}, 1000) || perfAlert(perfRec{Cached: iptr(0), Total: 500}, 1000) || perfAlert(perfRec{Total: 9000}, 1000) {
		t.Fatal("gros prompt sans cache")
	}
}

// La compaction n'est pas streamée : sa télémétrie vient de la racine de la
// réponse, et un compteur mal typé ne la fait pas échouer.
func TestSummarizePerfLenient(t *testing.T) {
	testHome(t)
	freshPerf(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"- résumé"}}],` +
			`"usage":{"prompt_tokens":3000,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":"beaucoup"}},` +
			`"timings":{"cache_n":2900.0,"prompt_n":100,"prompt_ms":50,"prompt_per_second":2000,"predicted_n":40,"predicted_per_second":30}}`))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}
	got, err := summarizeTranscriptFor(withPerf(context.Background(), perfMain, "c9"), "transcript", false)
	if err != nil || got != "- résumé" {
		t.Fatalf("résumé : %q, %v", got, err)
	}
	recs := perfLog.snapshot()
	if len(recs) != 1 || recs[0].Kind != perfCompact || recs[0].Conv != "c9" || !recs[0].Complete ||
		recs[0].Cached == nil || *recs[0].Cached != 2900 || recs[0].Total != 3000 || recs[0].New != 100 {
		t.Fatalf("enregistrement : %+v", recs)
	}
}

func TestPerfLine(t *testing.T) {
	l := perfLine(perfRec{Kind: perfMain, Conv: "c1", Total: 41484, Cached: iptr(41230), New: 254, DraftN: iptr(90), DraftAcc: iptr(71), Lost: iptr(0), Complete: true})
	for _, want := range []string{"kind=main", "conv=c1", "cached=41230", "new=254", "draft=71/90", "ttft_ms=?", "lost=0"} {
		if !strings.Contains(l, want) {
			t.Fatalf("%q absent de %q", want, l)
		}
	}
	t.Setenv("LOKI_PERF_LOG", "")
	if perfLogOn() {
		t.Fatal("ligne [perf] active par défaut")
	}
	t.Setenv("LOKI_PERF_LOG", "1")
	if !perfLogOn() {
		t.Fatal("LOKI_PERF_LOG=1 ignorée")
	}
}
