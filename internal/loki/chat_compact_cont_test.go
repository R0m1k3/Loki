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
)

// oldCompactorSys : le prompt du résumeur tel qu'il était avant le partage de
// ses règles avec la continuation — copie figée, à l'octet près.
const oldCompactorSys = `You are a context compactor. You are given the transcript of the older turns of a conversation between a user and an AI assistant (with its tools). The PURPOSE of your summary is to let the conversation continue in a fresh, smaller context WITHOUT losing any information that is useful or important to understand what came before and keep working — preserve everything that matters, drop only what is redundant.

The assistant is MID-TASK: it will read your summary and must resume exactly where it left off, WITHOUT redoing work it has already done. Its own internal reasoning is NOT part of the transcript and is lost — your summary is the only memory it keeps.

Summarize densely and faithfully, keeping ONLY the essentials:
- The user's CURRENT request, goal(s) and constraints
- FINDINGS: the concrete information already gathered — facts, figures, dates, names, URLs, file paths, values, config. This is the most important part: whatever is not here is lost and will have to be looked up again.
- Sources already consulted (URLs opened, files read, commands run) — so they are not consulted a second time
- Decisions made and established facts
- STATE OF PROGRESS: what is already answered, what is still missing, and the next concrete step
Strict rules: no preamble or conclusion, no verbatim or long quotes, no throwaway detail. Use short bullet points. Be as concise as you can WHILE keeping every fact, decision and still-open task: a detail you drop here is lost for good, so when in doubt keep it. This is a dense compression summary, not a report. Always write ACTUAL prose sentences/bullets — never answer with just an id or a reference.
Write the summary in the SAME language as the conversation.`

const contSummary = "- L'utilisateur prépare un voyage à Lyon ; horaires trouvés : départ 8 h 12, arrivée 10 h 05."

// moteurCont : faux llama-server. Tours streamés : « ok. ». Résumés :
// contSummary ; pour la continuation, contStatus (refus) ou contReply s'ils
// sont posés.
type moteurCont struct {
	mu         sync.Mutex
	bodies     []string
	contStatus int
	contReply  map[string]any
}

func (m *moteurCont) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		body := buf.String()
		switch r.URL.Path {
		case "/health":
			sendJSON(w, 200, map[string]any{"status": "ok"})
			return
		case "/props":
			sendJSON(w, 200, map[string]any{"total_slots": 1, "build_info": "b9000-x"})
			return
		}
		m.mu.Lock()
		m.bodies = append(m.bodies, body)
		status, reply := m.contStatus, m.contReply
		m.mu.Unlock()
		if strings.Contains(body, `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(sseChunk("ok.") + sseFinal("stop", 100, 2) + "data: [DONE]\n\n"))
			return
		}
		if isContBody(body) {
			if status != 0 {
				sendJSON(w, status, map[string]any{"error": map[string]any{"message": "template error"}})
				return
			}
			if reply != nil {
				sendJSON(w, 200, reply)
				return
			}
		}
		sendJSON(w, 200, map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": contSummary}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 900, "completion_tokens": 40},
			"timings": map[string]any{"prompt_n": 30, "cache_n": 870, "predicted_n": 40},
		})
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}
}

func isContBody(body string) bool {
	var p struct {
		Messages []Message `json:"messages"`
	}
	_ = json.Unmarshal([]byte(body), &p)
	n := len(p.Messages)
	return n > 0 && strings.HasPrefix(msgText(p.Messages[n-1]), compactContPrefix)
}

func isTranscriptBody(body string) bool {
	var p struct {
		Messages []Message `json:"messages"`
	}
	_ = json.Unmarshal([]byte(body), &p)
	return len(p.Messages) == 2 && p.Messages[0].Role == "system" &&
		strings.HasPrefix(msgText(p.Messages[0]), "You are a context compactor.")
}

func (m *moteurCont) all() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.bodies...)
}

// contReset : état global de la continuation remis à neuf.
func contReset(t *testing.T) {
	reset := func() {
		engineGate.mu.Lock()
		engineGate.main = engineMainStamp{}
		engineGate.mu.Unlock()
		compactContFails.mu.Lock()
		compactContFails.m = nil
		compactContFails.mu.Unlock()
		compactRefused.mu.Lock()
		compactRefused.m = nil
		compactRefused.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func contSetup(t *testing.T, keys map[string]string) *moteurCont {
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
	contReset(t)
	if err := SetConfigKey("MODEL", "/models/"+echoTestModel); err != nil {
		t.Fatal(err)
	}
	if err := SetConfigKey("CTX", "8192"); err != nil {
		t.Fatal(err)
	}
	for k, v := range keys {
		if err := SetConfigKey(k, v); err != nil {
			t.Fatal(err)
		}
	}
	m := &moteurCont{}
	m.start(t)
	return m
}

// contHistory : une longue discussion, de quoi compacter.
func contHistory() []Message {
	var h []Message
	for i := 0; i < 10; i++ {
		h = append(h, um("question "+string(rune('a'+i))+" "+strings.Repeat("u", 1500)),
			am("réponse "+string(rune('a'+i))+" "+strings.Repeat("r", 1500)))
	}
	return h
}

// contTwoTurns : un premier tour sans compaction (il pose le tampon du slot),
// puis un second dont le début compacte. Rend les corps envoyés pendant le
// second tour, et celui du premier tour.
func contTwoTurns(t *testing.T, m *moteurCont, c *Conversation, caps Caps) (first string, second []string) {
	t.Helper()
	c.mu.Lock()
	c.Messages = contHistory()
	c.CtxUsed, c.ctxUsedLen = 1000, len(c.Messages)
	c.mu.Unlock()
	prewarmTurn(t, c, caps, "première demande")
	bodies := m.all()
	if len(bodies) != 1 {
		t.Fatalf("premier tour : 1 requête attendue, %d", len(bodies))
	}
	c.mu.Lock()
	c.CtxUsed, c.ctxUsedLen = 6200, len(c.Messages)
	c.mu.Unlock()
	prewarmTurn(t, c, caps, "deuxième demande")
	return bodies[0], m.all()[1:]
}

// Sans la clé : la requête de résumé est celle d'avant — prompt du résumeur
// identique à l'octet près, transcription, aucun champ de plus — et aucun
// tampon de slot n'est posé.
func TestCompactContDefautIdentique(t *testing.T) {
	m := contSetup(t, map[string]string{"TOP_P": "0.9", "REASONING_EFFORT": "high"})
	c := newTestConv()
	_, second := contTwoTurns(t, m, c, Caps{Agent: true})
	if len(second) != 2 || !isTranscriptBody(second[0]) {
		t.Fatalf("sans la clé : résumé sur transcription puis tour attendus, %d requêtes", len(second))
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(second[0]), &p); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"model": p["model"], "stream": false, "temperature": 0.2, "max_tokens": float64(700),
		"chat_template_kwargs": map[string]any{"enable_thinking": false}, "messages": p["messages"]}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("champs du résumé changés : %v", p)
	}
	if sys := p["messages"].([]any)[0].(map[string]any)["content"]; sys != oldCompactorSys {
		t.Fatalf("prompt du résumeur changé :\n%v", sys)
	}
	engineGate.mu.Lock()
	stamp := engineGate.main
	engineGate.mu.Unlock()
	if stamp.seq != 0 {
		t.Fatal("tampon de slot posé sans la clé")
	}
	if compactContinuationOn(ReadConfig()) || compactRefusedSkip(convActiveID(), 7000, nil) {
		t.Fatal("continuation ou mémoire des refus active sans la clé")
	}
}

// Avec la clé : la requête de résumé prolonge EXACTEMENT celle du premier tour
// (même préfixe, mêmes outils et réglages du gabarit), plus la réponse rangée
// et une seule demande de résumé ; sans l'échantillonnage du preset ; et rien
// d'injecté n'entre dans l'historique.
func TestCompactContPrefixeEtHistorique(t *testing.T) {
	m := contSetup(t, map[string]string{"COMPACT_CONTINUATION": "on", "TOP_P": "0.9", "REASONING_EFFORT": "high"})
	c := newTestConv()
	first, second := contTwoTurns(t, m, c, Caps{Agent: true})
	if len(second) != 2 || !isContBody(second[0]) {
		t.Fatalf("continuation puis tour attendus, %d requêtes (transcription : %v)", len(second), len(second) > 0 && isTranscriptBody(second[0]))
	}
	var f, s map[string]any
	if err := json.Unmarshal([]byte(first), &f); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(second[0]), &s); err != nil {
		t.Fatal(err)
	}
	fm, sm := f["messages"].([]any), s["messages"].([]any)
	if len(sm) != len(fm)+2 {
		t.Fatalf("continuation : %d messages, premier tour %d (+ réponse + demande attendus)", len(sm), len(fm))
	}
	if !reflect.DeepEqual(sm[:len(fm)], fm) {
		t.Fatal("la continuation ne prolonge pas la requête du premier tour")
	}
	if a := sm[len(fm)].(map[string]any); a["role"] != "assistant" || a["content"] != "ok." {
		t.Fatalf("réponse rangée attendue après le préfixe : %v", a)
	}
	last := sm[len(sm)-1].(map[string]any)
	if last["role"] != "user" || !strings.Contains(last["content"].(string), "STATE OF PROGRESS") {
		t.Fatalf("demande de résumé : %v", last)
	}
	for _, k := range []string{"tools", "parallel_tool_calls", "reasoning_effort", "model"} {
		if !reflect.DeepEqual(f[k], s[k]) {
			t.Fatalf("champ %s : tour %v, continuation %v", k, f[k], s[k])
		}
	}
	if s["tool_choice"] != "none" || s["stream"] != false || s["max_tokens"] != float64(700) ||
		s["temperature"] != 0.2 || s["top_p"] != nil || s["stream_options"] != nil {
		t.Fatalf("champs de réponse : %v", s)
	}
	if f["top_p"] != 0.9 {
		t.Fatal("le tour lui-même doit garder l'échantillonnage du preset")
	}
	kw, _ := s["chat_template_kwargs"].(map[string]any)
	if kw["enable_thinking"] != false || kw["reasoning_effort"] != "high" {
		t.Fatalf("arguments du gabarit : %v", kw)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	found := false
	for _, msg := range c.Messages {
		txt := msgText(msg)
		if msg.Role == "system" || strings.HasPrefix(txt, compactContPrefix) || strings.Contains(txt, "<project_context>") {
			t.Fatalf("message injecté rangé dans l'historique : %s %.80q", msg.Role, txt)
		}
		if strings.Contains(txt, contSummary) {
			found = true
		}
	}
	if !found {
		t.Fatal("résumé absent de l'historique compacté")
	}
	kinds := false
	for _, r := range perfLog.snapshot() {
		if r.Kind == perfCompact && r.Conv == convActiveID() {
			kinds = true
		}
	}
	if !kinds {
		t.Fatal("aucune complétion kind=compact rattachée à la discussion")
	}
}

// contViewOpts : options en cours de tour pour msgs, slot tamponné pour conv.
func contViewOpts(t *testing.T, msgs []Message, used int) compactOpts {
	t.Helper()
	end, seq := engineRequestBegin(nil)
	end()
	engineMarkMain(seq, "conv-test")
	return compactOptsMid(ReadConfig(), resolveChatEndpoint(), "conv-test", msgs, used, nil, "", nil, echoPolicy{}, false)
}

func contMidMsgs() []Message {
	return append([]Message{{Role: "system", Content: "SYS"}}, contHistory()...)
}

// Les replis : refus du moteur (gabarit), appel d'outil émis, marge
// insuffisante, slot pris entre-temps — la compaction passe par la
// transcription et réussit quand même. Un refus du gabarit ou un appel
// d'outil suspend la continuation pour ce modèle.
func TestCompactContReplis(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(m *moteurCont)
		used     int
		stale    bool
		wantCont bool
		disabled bool
	}{
		{"gabarit 500", func(m *moteurCont) { m.contStatus = 500 }, 5000, false, true, true},
		{"appel d'outil", func(m *moteurCont) {
			m.contReply = map[string]any{"choices": []any{map[string]any{"message": map[string]any{
				"content": "", "tool_calls": []any{map[string]any{"id": "x", "type": "function"}}}, "finish_reason": "tool_calls"}}}
		}, 5000, false, true, true},
		{"raisonnement seul", func(m *moteurCont) {
			m.contReply = map[string]any{"choices": []any{map[string]any{"message": map[string]any{
				"content": "", "reasoning_content": "je réfléchis"}, "finish_reason": "length"}}}
		}, 5000, false, true, false},
		{"marge", func(*moteurCont) {}, 7500, false, false, false},
		{"slot pris", func(*moteurCont) {}, 5000, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := contSetup(t, map[string]string{"COMPACT_CONTINUATION": "on"})
			tc.setup(m)
			msgs := contMidMsgs()
			opt := contViewOpts(t, msgs, tc.used)
			if opt.view == nil {
				t.Fatal("vue attendue")
			}
			if tc.stale {
				engineRequestStart()()
			}
			out, changed := compactMessagesNoted(context.Background(), msgs, Caps{}, opt)
			if !changed {
				t.Fatal("compaction refusée")
			}
			if out[0].Role != "system" || msgText(out[0]) != "SYS" || !strings.Contains(msgText(out[1]), contSummary) {
				t.Fatalf("sortie inattendue : %v / %.80q", out[0], msgText(out[1]))
			}
			bodies := m.all()
			cont := len(bodies) == 2 && isContBody(bodies[0]) && isTranscriptBody(bodies[1])
			direct := len(bodies) == 1 && isTranscriptBody(bodies[0])
			if tc.wantCont && !cont || !tc.wantCont && !direct {
				t.Fatalf("requêtes inattendues (%d)", len(bodies))
			}
			if compactContDisabled() != tc.disabled {
				t.Fatalf("suspension pour le modèle : %v, attendu %v", compactContDisabled(), tc.disabled)
			}
		})
	}
}

// Deux résumés ratés de suite suspendent la continuation ; un réussi remet le
// compte à zéro.
func TestCompactContSuspension(t *testing.T) {
	contSetup(t, map[string]string{"COMPACT_CONTINUATION": "on"})
	compactContFail(false)
	if compactContDisabled() {
		t.Fatal("suspendue après un seul échec")
	}
	compactContOK()
	compactContFail(false)
	if compactContDisabled() {
		t.Fatal("le succès n'a pas remis le compte à zéro")
	}
	compactContFail(false)
	if !compactContDisabled() {
		t.Fatal("pas suspendue après deux échecs")
	}
	if o := contViewOpts(t, contMidMsgs(), 5000); o.view != nil || !o.precheck {
		t.Fatal("vue construite pour un modèle suspendu")
	}
}

// Le tampon du slot : posé par une étape d'un tour de discussion, perdu dès
// qu'une autre requête part (résumé, vérification, préchauffage…), ou que le
// modèle ou la fenêtre changent.
func TestEngineSlotHolds(t *testing.T) {
	testHome(t)
	contReset(t)
	if err := SetConfigKey("MODEL", "/models/a.gguf"); err != nil {
		t.Fatal(err)
	}
	mark := func() {
		end, seq := engineRequestBegin(nil)
		end()
		engineMarkMain(seq, "c1")
	}
	mark()
	if !engineSlotHolds("c1") || engineSlotHolds("c2") || engineSlotHolds("") {
		t.Fatal("tampon mal lu")
	}
	engineRequestStart()()
	if engineSlotHolds("c1") {
		t.Fatal("tampon gardé après une autre requête")
	}
	// Une requête partie entre le départ et l'acceptation : pas de tampon.
	end, seq := engineRequestBegin(nil)
	end()
	engineRequestStart()()
	engineMarkMain(seq, "c1")
	if engineSlotHolds("c1") {
		t.Fatal("tampon posé pour une requête dépassée")
	}
	mark()
	p := &prewarmRun{cancel: func() {}}
	if endP, ok := prewarmBegin(p); ok {
		endP()
	}
	if engineSlotHolds("c1") {
		t.Fatal("tampon gardé après un préchauffage")
	}
	mark()
	if err := SetConfigKey("MODEL", "/models/b.gguf"); err != nil {
		t.Fatal(err)
	}
	if engineSlotHolds("c1") {
		t.Fatal("tampon gardé après un changement de modèle")
	}
}

// Une étape d'un sous-agent ou d'une vérification ne pose pas de tampon :
// après elle, la compaction de fin de tour passe par la transcription.
func TestCompactContPasApresVerification(t *testing.T) {
	contSetup(t, map[string]string{"COMPACT_CONTINUATION": "on"})
	end, seq := engineRequestBegin(nil)
	end()
	engineMarkMain(seq, convActiveID())
	ctx := withPerf(context.Background(), perfMain, convActiveID())
	if _, err := runChat(withPerfKind(ctx, perfVerify), []Message{um("vérifie")}, 0.2, Caps{}, func(StreamEvent) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if engineSlotHolds(convActiveID()) {
		t.Fatal("tampon gardé après une vérification")
	}
	if _, err := runChat(ctx, []Message{um("suite")}, 0.2, Caps{}, func(StreamEvent) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if !engineSlotHolds(convActiveID()) {
		t.Fatal("une étape du tour devrait poser le tampon")
	}
}

// La correspondance des deux vues : messages système ajoutés ou déplacés,
// les autres un pour un ; tout écart rend -1.
func TestCompactWireIndex(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: memReminderPrefix + " pages"},
		um("a"), atc("bash"), tm("r"), am("b"), um("c"),
	}
	wire := normalizeSystemMessages(append([]Message{{Role: "system", Content: "SYS"}, {Role: "system", Content: projectContextPrefix + " p"}}, msgs[:5]...))
	v := &compactView{wire: wire, n: 5}
	if wire[0].Role != "system" || len(wire) != 5 {
		t.Fatalf("vue d'envoi : %v", wire)
	}
	for i, want := range map[int]int{1: 1, 2: 2, 3: 3, 4: 4, 5: 5, 0: -1, 6: -1} {
		if got := compactWireIndex(msgs, v, i); got != want {
			t.Errorf("index %d : %d, attendu %d", i, got, want)
		}
	}
	bad := append([]Message(nil), wire...)
	bad[3] = am("x")
	if compactWireIndex(msgs, &compactView{wire: bad, n: 5}, 2) != -1 {
		t.Fatal("vue désalignée acceptée")
	}
	if compactWireIndex(msgs, &compactView{wire: wire[:4], n: 5}, 2) != -1 {
		t.Fatal("vue tronquée acceptée")
	}
}

// La frontière désignée : appel d'outil par son nom et ses arguments, texte
// par son début, allongé s'il est déjà apparu plus haut.
func TestCompactBoundaryDesc(t *testing.T) {
	call := Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Function: ToolCallFunc{Name: "web_read", Arguments: `{"url":"https://exemple.fr"}`}}}}
	if d := compactBoundaryDesc(nil, 0, call); !strings.Contains(d, `web_read({"url":"https://exemple.fr"})`) {
		t.Fatalf("appel d'outil : %s", d)
	}
	long := strings.Repeat("même début ", 10) + "fin unique"
	wire := []Message{um(strings.Repeat("même début ", 10) + "autre fin"), um(long)}
	d := compactBoundaryDesc(wire, 1, wire[1])
	if !strings.Contains(d, "fin unique") {
		t.Fatalf("début ambigu non allongé : %s", d)
	}
	if d := compactBoundaryDesc(nil, 0, um("court")); d != "the user message that starts with «court»" {
		t.Fatalf("texte : %s", d)
	}
}

// La demande de résumé reprend les règles du résumeur, et celles du mode Code.
func TestCompactContInstruction(t *testing.T) {
	s := compactContInstruction(3, "the user message that starts with «x»", true)
	for _, want := range []string{compactContPrefix, compactSummaryRules, codeSummaryRules, "the last 3", "BEFORE the user message"} {
		if !strings.Contains(s, want) {
			t.Fatalf("demande sans %q", want)
		}
	}
	if s := compactContInstruction(0, "", false); !strings.Contains(s, "whole conversation") || strings.Contains(s, "CODING") {
		t.Fatal("demande sans frontière")
	}
}

// Pré-contrôle : quand même un résumé vide ne réduirait pas de 20 %, aucun
// résumé n'est demandé ni aucun bloc archivé ; sans lui (sans la clé), la
// requête part comme avant, pour être refusée ensuite.
func TestCompactHopeless(t *testing.T) {
	m := contSetup(t, nil)
	// Torse minuscule, queue énorme : rien à gagner.
	msgs := []Message{um("a"), am("b"), um("c"), am(strings.Repeat("r", 40000))}
	head, tailStart := compactBounds(msgs, int(float64(estimateTokens(msgs))*compactTailFrac))
	if !compactHopeless(msgs, head, tailStart) {
		t.Fatal("cas sans issue non reconnu")
	}
	if _, changed, _ := compactMessagesOpt(context.Background(), msgs, Caps{Agent: true}, compactOpts{precheck: true}); changed {
		t.Fatal("compaction acceptée")
	}
	if n := len(m.all()); n != 0 {
		t.Fatalf("résumé demandé malgré le pré-contrôle (%d requêtes)", n)
	}
	_, changed, refused := compactMessagesOpt(context.Background(), msgs, Caps{Agent: true}, compactOpts{})
	if changed || !refused || len(m.all()) != 1 {
		t.Fatalf("sans pré-contrôle : changed=%v refused=%v requêtes=%d", changed, refused, len(m.all()))
	}
	// Un refus après résumé est noté ; il n'arrête ni le filet réactif ni le
	// bouton manuel (compactMessages, sans options), qui redemandent un résumé.
	if _, changed := compactMessagesNoted(context.Background(), msgs, Caps{}, compactOpts{conv: "c9", used: 7000}); changed {
		t.Fatal("compaction acceptée")
	}
	compactRefused.mu.Lock()
	_, noted := compactRefused.m["c9"]
	compactRefused.mu.Unlock()
	if !noted {
		t.Fatal("refus non noté")
	}
	if err := SetConfigKey("COMPACT_CONTINUATION", "on"); err != nil {
		t.Fatal(err)
	}
	before := len(m.all())
	compactMessages(context.Background(), msgs, Caps{})
	if len(m.all()) != before+1 {
		t.Fatal("le chemin réactif ne doit pas tenir compte des refus mémorisés")
	}
	// Un cas qui réussit n'est jamais déclaré sans issue.
	h := contHistory()
	head, tailStart = compactBounds(h, int(float64(estimateTokens(h))*compactTailFrac))
	if compactHopeless(h, head, tailStart) {
		t.Fatal("compaction faisable déclarée sans issue")
	}
}

// Mémoire des refus : pas de nouvel essai avant +10 % de contexte, jamais à
// 90 % de la fenêtre, oubliée quand le contexte baisse ou que l'historique est
// réécrit ; rien sans la clé.
func TestCompactRefusedSkip(t *testing.T) {
	testHome(t)
	contReset(t)
	if err := SetConfigKey("CTX", "65536"); err != nil {
		t.Fatal(err)
	}
	h := contHistory()
	grown := append(append([]Message(nil), h...), um("suite"))
	compactRefusedNote("c1", 50000, h)
	if compactRefusedSkip("c1", 52000, grown) {
		t.Fatal("sans la clé, aucun essai ne doit être sauté")
	}
	if err := SetConfigKey("COMPACT_CONTINUATION", "on"); err != nil {
		t.Fatal(err)
	}
	if !compactRefusedSkip("c1", 52000, grown) || compactRefusedSkip("c2", 52000, grown) {
		t.Fatal("refus mal appliqué")
	}
	if compactRefusedSkip("c1", 59000, grown) {
		t.Fatal("essai sauté à 90 % de la fenêtre")
	}
	if compactRefusedSkip("c1", 55000, grown) {
		t.Fatal("essai sauté après +10 %")
	}
	if compactRefusedSkip("c1", 52000, grown) {
		t.Fatal("refus non oublié après +10 %")
	}
	compactRefusedNote("c1", 50000, h)
	if compactRefusedSkip("c1", 30000, grown) || compactRefusedSkip("c1", 50000, grown) {
		t.Fatal("refus non oublié quand le contexte baisse")
	}
	// Historique réécrit (édition, régénération, discussion vidée puis
	// regarnie) : même taille de contexte, autre fil — on retente.
	compactRefusedNote("c1", 50000, h)
	edited := append([]Message(nil), grown...)
	edited[3] = um("message modifié")
	if compactRefusedSkip("c1", 51000, edited) || compactRefusedSkip("c1", 51000, grown) {
		t.Fatal("refus gardé pour un historique réécrit")
	}
	compactRefusedNote("c1", 50000, h)
	if compactRefusedSkip("c1", 51000, h[:5]) {
		t.Fatal("refus gardé pour un historique raccourci")
	}
	// Une compaction réussie l'efface.
	compactRefusedNote("c1", 50000, h)
	compactRefusedClear("c1")
	if compactRefusedSkip("c1", 51000, grown) {
		t.Fatal("refus non effacé")
	}
}

// buildChatPayload : sans noSampling, l'échantillonnage du preset est posé
// comme avant ; avec, il ne l'est pas.
func TestBuildChatPayloadNoSampling(t *testing.T) {
	testHome(t)
	if err := SetConfigKey("TOP_P", "0.9"); err != nil {
		t.Fatal(err)
	}
	ep := chatEndpoint{Model: "m"}
	if p := buildChatPayload(ep, []Message{um("x")}, 0.7, chatPayloadOpts{}); p["top_p"] != 0.9 {
		t.Fatalf("échantillonnage perdu : %v", p)
	}
	if p := buildChatPayload(ep, []Message{um("x")}, 0.2, chatPayloadOpts{noSampling: true}); p["top_p"] != nil || p["temperature"] != 0.2 {
		t.Fatalf("échantillonnage posé malgré noSampling : %v", p)
	}
}
