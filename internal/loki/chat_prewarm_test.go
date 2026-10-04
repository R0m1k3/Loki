package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// moteurPrewarm : faux llama-server qui note TOUTES les requêtes (chemin et
// corps), sert /health et /props, et répond aux complétions streamées comme aux
// autres.
type moteurPrewarm struct {
	mu    sync.Mutex
	reqs  []prewarmReq
	slots int
}

type prewarmReq struct{ path, body string }

func (m *moteurPrewarm) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		m.mu.Lock()
		m.reqs = append(m.reqs, prewarmReq{r.URL.Path, buf.String()})
		slots := m.slots
		m.mu.Unlock()
		switch r.URL.Path {
		case "/health":
			sendJSON(w, 200, map[string]any{"status": "ok"})
		case "/props":
			sendJSON(w, 200, map[string]any{"total_slots": slots, "build_info": "b9000-x"})
		default:
			if strings.Contains(buf.String(), `"stream":true`) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(sseChunk("ok.") + sseFinal("stop", 100, 2) + "data: [DONE]\n\n"))
				return
			}
			sendJSON(w, 200, map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{"content": "x"}}},
				"usage":   map[string]any{"prompt_tokens": 120, "completion_tokens": 1},
				"timings": map[string]any{"prompt_n": 4, "cache_n": 116, "predicted_n": 1},
			})
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}
}

func (m *moteurPrewarm) all() []prewarmReq {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]prewarmReq(nil), m.reqs...)
}

// completions : corps des complétions, streamées ou non.
func (m *moteurPrewarm) completions() []string {
	var out []string
	for _, r := range m.all() {
		if r.path == "/v1/chat/completions" {
			out = append(out, r.body)
		}
	}
	return out
}

// prewarmSetup : base neuve, discussion active, faux moteur à un slot,
// préchauffage synchrone. keys : réglages posés.
func prewarmSetup(t *testing.T, keys map[string]string) *moteurPrewarm {
	t.Helper()
	withWorkspace(t)
	activeProjMu.Lock()
	activeProjCache = ""
	activeProjMu.Unlock()
	setProjectOverride("")
	t.Cleanup(func() {
		activeProjMu.Lock()
		activeProjCache = ""
		activeProjMu.Unlock()
		setProjectOverride("")
	})
	ensureDefaultProject()
	freshTplProbe(t)
	convEnsureActive()
	prewarmAsync = false
	prewarmCaps.mu.Lock()
	prewarmCaps.m = nil
	prewarmCaps.mu.Unlock()
	t.Cleanup(func() {
		prewarmAsync = true
		prewarmCaps.mu.Lock()
		prewarmCaps.m = nil
		prewarmCaps.mu.Unlock()
	})
	if err := SetConfigKey("MODEL", "/models/"+echoTestModel); err != nil {
		t.Fatal(err)
	}
	for k, v := range keys {
		if err := SetConfigKey(k, v); err != nil {
			t.Fatal(err)
		}
	}
	m := &moteurPrewarm{slots: 1}
	m.start(t)
	return m
}

// prewarmTurn : un tour de chat comme StartTurn le lance (capacités notées pour
// le préchauffage), joué jusqu'au bout, préchauffage éventuel compris.
func prewarmTurn(t *testing.T, c *Conversation, caps Caps, text string) {
	t.Helper()
	prewarmNoteCaps(convActiveID(), caps)
	c.mu.Lock()
	c.Messages = append(c.Messages, um(text))
	c.mu.Unlock()
	c.generate(context.Background(), withTurnRole(caps, text), 0.7, c.epoch)
}

// Clé absente : pas une requête de plus au moteur — ni complétion, ni /health,
// ni /props — et la conversation est celle d'avant.
func TestPrewarmDefautAucuneRequete(t *testing.T) {
	m := prewarmSetup(t, nil)
	c := newTestConv()
	caps := Caps{Agent: true, Mem: MemAlways}
	prewarmTurn(t, c, caps, "bonjour")
	prewarmTurn(t, c, caps, "et ensuite ?")
	reqs := m.all()
	if len(reqs) != 2 {
		t.Fatalf("2 requêtes attendues (une par tour), %d reçues : %+v", len(reqs), reqs)
	}
	for _, r := range reqs {
		if r.path != "/v1/chat/completions" || strings.Contains(r.body, `"max_tokens"`) || !strings.Contains(r.body, `"stream":true`) {
			t.Fatalf("requête inattendue sans la clé : %s %s", r.path, r.body)
		}
	}
	// La fin de tour ne lance rien non plus en direct.
	c.prewarm(prewarmTurnEnd)
	if n := len(m.all()); n != 2 {
		t.Fatalf("prewarm sans la clé a parlé au moteur (%d requêtes)", n)
	}
}

// Preset externe : la clé est sans effet.
func TestPrewarmJamaisExterne(t *testing.T) {
	cfg := map[string]string{"PREWARM": "on", extKeyFlag: "1", extKeyURL: "http://127.0.0.1:1"}
	if prewarmMode(cfg) != "" {
		t.Fatal("PREWARM actif sur un preset externe")
	}
	for v, want := range map[string]string{"": "", "off": "", "on": "on", "1": "on", "full": "full", "FULL": "full", "x": ""} {
		if got := prewarmMode(map[string]string{"PREWARM": v}); got != want {
			t.Fatalf("PREWARM=%q : %q, attendu %q", v, got, want)
		}
	}
}

// Avec la clé : la requête de préchauffage est celle du prochain tour, sentinelle
// en plus — mêmes messages jusqu'au dernier, mêmes outils et arguments du
// gabarit — avec max_tokens 1, sans flux. Rien n'en reste dans la discussion,
// et la télémétrie la range sous kind=prewarm.
func TestPrewarmEgalTourSuivantPlusSentinelle(t *testing.T) {
	m := prewarmSetup(t, map[string]string{"PREWARM": "on", "REASONING_EFFORT": "high", "TOP_P": "0.9"})
	if err := MemAdd("nas.md", "# NAS\n"); err != nil {
		t.Fatal(err)
	}
	c := newTestConv()
	caps := Caps{Agent: true, Mem: MemAlways}
	prewarmTurn(t, c, caps, "bonjour")
	comps := m.completions()
	if len(comps) != 2 {
		t.Fatalf("tour + préchauffage attendus, %d complétions", len(comps))
	}
	var pw map[string]any
	if err := json.Unmarshal([]byte(comps[1]), &pw); err != nil {
		t.Fatal(err)
	}
	if pw["stream"] != false || pw["max_tokens"] != float64(1) || pw["stream_options"] != nil {
		t.Fatalf("champs de réponse du préchauffage : stream=%v max_tokens=%v stream_options=%v", pw["stream"], pw["max_tokens"], pw["stream_options"])
	}
	c.mu.Lock()
	for _, msg := range c.Messages {
		if s, _ := msg.Content.(string); s == prewarmSentinel {
			c.mu.Unlock()
			t.Fatal("la sentinelle a été rangée dans la discussion")
		}
	}
	c.mu.Unlock()

	prewarmTurn(t, c, caps, "et ensuite ?")
	comps = m.completions()
	var real map[string]any
	if err := json.Unmarshal([]byte(comps[2]), &real); err != nil {
		t.Fatal(err)
	}
	pm, rm := pw["messages"].([]any), real["messages"].([]any)
	if len(pm) != len(rm) {
		t.Fatalf("préchauffage %d messages, tour suivant %d", len(pm), len(rm))
	}
	if !reflect.DeepEqual(pm[:len(pm)-1], rm[:len(rm)-1]) {
		t.Fatalf("préfixe différent du tour suivant :\npréchauffage %v\ntour         %v", pm[:len(pm)-1], rm[:len(rm)-1])
	}
	if last := pm[len(pm)-1].(map[string]any); last["role"] != "user" || last["content"] != prewarmSentinel {
		t.Fatalf("dernier message du préchauffage : %v", last)
	}
	for _, k := range append([]string{"temperature", "top_p", "model"}, prewarmOptsKeys...) {
		if !reflect.DeepEqual(pw[k], real[k]) {
			t.Fatalf("champ %s : préchauffage %v, tour %v", k, pw[k], real[k])
		}
	}
	found := false
	for _, r := range perfLog.snapshot() {
		if r.Kind == perfPrewarm {
			found = true
		}
	}
	if !found {
		t.Fatal("aucune complétion kind=prewarm dans la télémétrie")
	}
}

// Les garde-fous : plus d'un slot, tour en cours, capacités inconnues, travail
// annexe — aucune complétion de préchauffage.
func TestPrewarmGardeFous(t *testing.T) {
	m := prewarmSetup(t, map[string]string{"PREWARM": "on"})
	c := newTestConv()
	caps := Caps{Agent: true}
	c.mu.Lock()
	c.Messages = []Message{um("bonjour"), am("salut")}
	c.mu.Unlock()
	count := func() int { return len(m.completions()) }

	// Capacités inconnues (aucun tour joué dans cette discussion).
	c.prewarm(prewarmTurnEnd)
	if count() != 0 {
		t.Fatal("préchauffage sans capacités connues")
	}
	prewarmNoteCaps(convActiveID(), caps)

	m.mu.Lock()
	m.slots = 2
	m.mu.Unlock()
	c.prewarm(prewarmTurnEnd)
	if count() != 0 {
		t.Fatal("préchauffage avec deux slots")
	}
	m.mu.Lock()
	m.slots = 1
	m.mu.Unlock()

	c.mu.Lock()
	c.Generating = true
	c.mu.Unlock()
	c.prewarm(prewarmTurnEnd)
	c.mu.Lock()
	c.Generating = false
	c.mu.Unlock()
	if count() != 0 {
		t.Fatal("préchauffage pendant un tour")
	}

	sideJobs.Add(1)
	c.prewarm(prewarmTurnEnd)
	sideJobs.Add(-1)
	if count() != 0 {
		t.Fatal("préchauffage pendant un travail annexe")
	}

	end := engineRequestStart()
	c.prewarm(prewarmTurnEnd)
	end()
	if count() != 0 {
		t.Fatal("préchauffage avec une requête de Loki en vol")
	}

	c.prewarm(prewarmTurnEnd)
	if count() != 1 {
		t.Fatalf("toutes conditions réunies : 1 préchauffage attendu, %d", count())
	}
}

// Toute requête de Loki annule le préchauffage en vol, sauf un tour de chat qui
// prolonge exactement son préfixe.
func TestPrewarmAnnuleSaufPrefixeExact(t *testing.T) {
	hist := []Message{{Role: "system", Content: "sys"}, um("a"), am("b")}
	pwMsgs := append(append([]Message(nil), hist...), um(prewarmSentinel))
	real := append(append([]Message(nil), hist...), um("suite"))
	payload := map[string]any{"tools": []string{"t"}, "parallel_tool_calls": false}
	other := []Message{{Role: "system", Content: "sys"}, um("a"), am("c"), um("suite")}

	// Sans préchauffage en vol, rien n'est calculé.
	if prewarmKeeper(perfMain, real, payload) != nil {
		t.Fatal("empreintes calculées sans préchauffage en vol")
	}
	cases := []struct {
		name string
		keep func() func(*prewarmRun) bool // construit préchauffage en vol
		kept bool
	}{
		{"tour qui prolonge", func() func(*prewarmRun) bool { return prewarmKeeper(perfMain, real, payload) }, true},
		{"autres outils", func() func(*prewarmRun) bool {
			return prewarmKeeper(perfMain, real, map[string]any{"tools": []string{"u"}, "parallel_tool_calls": false})
		}, false},
		{"autre historique", func() func(*prewarmRun) bool { return prewarmKeeper(perfMain, other, payload) }, false},
		{"préfixe seul, rien après", func() func(*prewarmRun) bool { return prewarmKeeper(perfMain, hist, payload) }, false},
		{"compaction", func() func(*prewarmRun) bool { return prewarmKeeper(perfCompact, real, payload) }, false},
		{"requête quelconque", func() func(*prewarmRun) bool { return nil }, false},
	}
	for _, tc := range cases {
		ctx, cancel := context.WithCancel(context.Background())
		run := &prewarmRun{n: len(pwMsgs) - 1, prefix: perfPrefix(pwMsgs)[len(pwMsgs)-2], opts: prewarmOptsHash(payload), cancel: cancel, done: make(chan struct{})}
		end, ok := prewarmBegin(run)
		if !ok {
			t.Fatal("prewarmBegin refusé, moteur libre")
		}
		done := engineRequestStartKeep(tc.keep())
		if kept := ctx.Err() == nil; kept != tc.kept {
			t.Fatalf("%s : gardé=%v, attendu %v", tc.name, kept, tc.kept)
		}
		done()
		end()
		cancel()
	}
	// Une requête en vol interdit d'en démarrer un.
	busy := engineRequestStart()
	if _, ok := prewarmBegin(&prewarmRun{cancel: func() {}}); ok {
		t.Fatal("préchauffage inscrit avec une requête en vol")
	}
	busy()
	if prewarmLive.Load() {
		t.Fatal("prewarmLive resté levé")
	}
}

// turnViewDry ne touche à rien (instantané, mises à jour, nettoyage) et rend,
// jusqu'au dernier message, ce que turnView enverra.
func TestTurnViewDryNeTouchePas(t *testing.T) {
	for _, on := range []bool{false, true} {
		m := projSnapSetup(t, on)
		c := newTestConv()
		caps := Caps{Agent: true, Mem: MemAlways}
		playTurn(t, m, c, caps, "bonjour")
		if err := MemAdd("docker.md", "# Docker\n"); err != nil {
			t.Fatal(err)
		}
		if !on {
			// Reste d'une époque avec la clé : turnView le nettoierait.
			c.mu.Lock()
			c.ProjSnap = &projSnapshot{Key: "x"}
			c.mu.Unlock()
		}
		c.mu.Lock()
		before, _ := json.Marshal(c)
		hist := append([]Message(nil), c.Messages...)
		c.mu.Unlock()

		dry, _ := c.turnViewDry(caps, append(append([]Message(nil), hist...), um(prewarmSentinel)), true)
		c.mu.Lock()
		after, _ := json.Marshal(c)
		c.mu.Unlock()
		if !bytes.Equal(before, after) {
			t.Fatalf("on=%v : turnViewDry a modifié la discussion", on)
		}
		sent, _, _ := c.turnView(caps, c.epoch, append(append([]Message(nil), hist...), um("suite")), true)
		dw, sw := normalizeSystemMessages(dry), normalizeSystemMessages(sent)
		if len(dw) != len(sw) || !reflect.DeepEqual(jsonValue(t, dw[:len(dw)-1]), jsonValue(t, sw[:len(sw)-1])) {
			t.Fatalf("on=%v : préfixe de turnViewDry ≠ turnView\n%v\n%v", on, jsonValue(t, dw), jsonValue(t, sw))
		}
	}
}

// oldChatPayload : la construction du corps dans runChat avant buildChatPayload,
// recopiée telle quelle.
func oldChatPayload(ep chatEndpoint, messages []Message, temperature float64, tools []Tool, disableTools, toolChoiceNone, echoOff bool, effort string, kwargs map[string]any) map[string]any {
	sent := normalizeSystemMessages(messages)
	if toolChoiceNone {
		sent = withTrailingHint(sent, toolsOffHint)
	}
	pol := currentEchoPolicy()
	if ep.External || echoOff {
		pol = echoPolicy{}
	}
	sent, _ = echoMessages(sent, pol)
	payload := map[string]any{
		"model":          ep.Model,
		"messages":       expandImageRefs(sent),
		"stream":         true,
		"temperature":    temperature,
		"stream_options": map[string]any{"include_usage": true},
	}
	applySampling(payload)
	if effort != "" {
		payload["reasoning_effort"] = effort
	}
	if kwargs != nil && !ep.External {
		payload["chat_template_kwargs"] = kwargs
	}
	if len(tools) > 0 && !disableTools {
		payload["tools"] = tools
		payload["parallel_tool_calls"] = false
		if toolChoiceNone {
			payload["tool_choice"] = "none"
		}
	}
	return payload
}

// oldTurnReasoning : le calcul du raisonnement dans runChat d'avant, recopié.
func oldTurnReasoning(cfg map[string]string) (string, string, bool, map[string]any) {
	effortWanted := reasoningEffortValue(cfg["REASONING_EFFORT"])
	reasoningEffort := effortResolve(effortWanted)
	thinkOff := reasoningExplicitlyOff(cfg["REASONING"]) || effortWanted == "none"
	return effortWanted, reasoningEffort, thinkOff, reasoningTemplateKwargs(thinkOff, reasoningEffort)
}

// Refactorisation : le corps construit par wireMessages + buildChatPayload est,
// octet pour octet, celui d'avant — sur toute la matrice des options d'une
// requête (outils coupés ou neutralisés, intensité, gabarit, API externe,
// raisonnement renvoyé, images).
func TestBuildChatPayloadIdentiqueAvant(t *testing.T) {
	m := prewarmSetup(t, map[string]string{"TOP_P": "0.9", "TOP_K": "20", "REASONING_ECHO": "on"})
	_ = m
	tools := EnabledTools(Caps{Agent: true, Mem: MemAlways})
	if len(tools) == 0 {
		t.Fatal("aucun outil pour la matrice")
	}
	msgs := []Message{
		{Role: "system", Content: "sys"},
		projectContextMessageForTest(),
		um("bonjour"),
		{Role: "assistant", Content: "salut", ReasoningContent: "je pense", ReasoningModel: echoTestModel},
		{Role: "user", Content: []any{map[string]any{"type": "text", "text": "et ça ?"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}}}},
	}
	eps := []chatEndpoint{{URL: "http://localhost:1/v1/chat/completions", Model: "loki"}, {URL: "https://api.example/v1/chat/completions", Model: "gpt", External: true}}
	for _, effortCfg := range []string{"", "high", "none"} {
		cfg := map[string]string{"REASONING_EFFORT": effortCfg, "REASONING": "on"}
		w1, e1, o1, k1 := oldTurnReasoning(cfg)
		w2, e2, o2, k2 := turnReasoning(cfg)
		if w1 != w2 || e1 != e2 || o1 != o2 || !reflect.DeepEqual(k1, k2) {
			t.Fatalf("turnReasoning(%q) différent d'avant", effortCfg)
		}
		for _, ep := range eps {
			for _, tl := range [][]Tool{nil, tools} {
				for _, disable := range []bool{false, true} {
					for _, none := range []bool{false, true} {
						for _, echoOff := range []bool{false, true} {
							want := oldChatPayload(ep, msgs, 0.7, tl, disable, none, echoOff, e2, k2)
							sent, _ := wireMessages(msgs, none, turnEchoPolicy(ep.External || echoOff))
							got := buildChatPayload(ep, sent, 0.7, chatPayloadOpts{tools: tl, disableTools: disable, toolChoiceNone: none, effort: e2, kwargs: k2})
							wb, _ := json.Marshal(want)
							gb, _ := json.Marshal(got)
							if !bytes.Equal(wb, gb) {
								t.Fatalf("corps différent (effort=%q ext=%v outils=%d off=%v none=%v echoOff=%v)\navant %s\naprès %s",
									effortCfg, ep.External, len(tl), disable, none, echoOff, wb, gb)
							}
						}
					}
				}
			}
		}
	}
}

// projectContextMessageForTest : un message projet, que normalizeSystemMessages
// déplace en tête du premier message utilisateur.
func projectContextMessageForTest() Message {
	return renderProjectContext("Projet de test")
}

// Refactorisation, de bout en bout : la requête d'un tour joué par generate est
// celle de l'assemblage et du corps d'avant, pour chaque combinaison de modes.
func TestGenerateCorpsIdentiqueAvant(t *testing.T) {
	m := prewarmSetup(t, map[string]string{"REASONING_EFFORT": "high", "TOP_P": "0.9"})
	matrix := []Caps{
		{},
		{Agent: true},
		{Agent: true, Mem: MemAlways},
		{Agent: true, Mem: MemOnDemand, Internet: true},
		{Agent: true, Code: true, Role: "builder"},
		{Agent: true, Code: true, Role: "planner"},
	}
	for i, caps := range matrix {
		c := newTestConv()
		c.mu.Lock()
		c.Messages = []Message{um("bonjour"), am("salut")}
		history := append(append([]Message(nil), c.Messages...), um("suite"))
		c.mu.Unlock()
		_, effort, _, kwargs := oldTurnReasoning(ReadConfig())
		final := oldAssembly(caps, history) // normalisé
		want := oldChatPayload(resolveChatEndpoint(), final, 0.7, EnabledTools(caps), false, false, false, effort, kwargs)
		before := len(m.completions())
		prewarmTurn(t, c, caps, "suite")
		comps := m.completions()
		if len(comps) <= before {
			t.Fatalf("cas %d : aucune requête", i)
		}
		var got any
		if err := json.Unmarshal([]byte(comps[before]), &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, jsonValue(t, want)) {
			t.Fatalf("cas %d (%+v) : corps différent d'avant\nreçu    %v\nattendu %v", i, caps, got, jsonValue(t, want))
		}
	}
}

func TestPrewarmNearMidnight(t *testing.T) {
	loc := time.Local
	if !prewarmNearMidnight(time.Date(2026, 10, 4, 23, 55, 0, 0, loc)) {
		t.Fatal("23:55 doit s'abstenir")
	}
	if prewarmNearMidnight(time.Date(2026, 10, 4, 23, 45, 0, 0, loc)) || prewarmNearMidnight(time.Date(2026, 10, 4, 0, 5, 0, 0, loc)) {
		t.Fatal("23:45 et 00:05 doivent préchauffer")
	}
}
