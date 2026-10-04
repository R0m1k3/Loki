package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const echoTestModel = "Qwen3.6-27B-Q8_0.gguf"

// echoTestSetup : base de test, modèle connu, sonde vierge, mémoire des refus
// nettoyée. on = REASONING_ECHO posé.
func echoTestSetup(t *testing.T, on bool) {
	t.Helper()
	withWorkspace(t)
	freshTplProbe(t)
	if err := SetConfigKey("MODEL", "/models/"+echoTestModel); err != nil {
		t.Fatal(err)
	}
	if on {
		if err := SetConfigKey("REASONING_ECHO", "on"); err != nil {
			t.Fatal(err)
		}
	}
	echoRefused.Delete(echoTestModel)
	t.Cleanup(func() { echoRefused.Delete(echoTestModel) })
}

// moteurEcho : llama-server de comédie. Chaque requête de complétion reçoit
// la réponse de reply (statut, corps SSE ou erreur) ; les corps bruts sont
// gardés pour être comparés. Les requêtes non streamées (résumé de compaction)
// reçoivent un résumé.
type moteurEcho struct {
	mu      sync.Mutex
	bodies  []string
	summary int
	reply   func(n int, body string) (int, string)
}

func (m *moteurEcho) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		body := buf.String()
		if !strings.Contains(body, `"stream":true`) {
			m.mu.Lock()
			m.summary++
			m.mu.Unlock()
			sendJSON(w, 200, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "- résumé des étapes précédentes, avec les faits trouvés"}}}})
			return
		}
		m.mu.Lock()
		n := len(m.bodies)
		m.bodies = append(m.bodies, body)
		m.mu.Unlock()
		status, out := m.reply(n, body)
		if status != 200 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(out))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(out + "data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}
}

func (m *moteurEcho) all() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.bodies...)
}

const sseGlobStep = `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"g1","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\"*.go\"}"}}]}}]}` + "\n\n" +
	`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"

// Une boucle d'outils : raisonnement séparé + appel, puis raisonnement + réponse.
func scriptBoucle(n int, _ string) (int, string) {
	if n == 0 {
		return 200, sseReasoning("je cherche ") + sseReasoning("les fichiers") + sseGlobStep
	}
	return 200, sseReasoning("j'ai trouvé") + sseChunk("Voilà.") + sseFinal("stop", 120, 5)
}

// requestMessages décode les messages d'un corps de requête.
func requestMessages(t *testing.T, body string) []map[string]any {
	t.Helper()
	var p struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	return p.Messages
}

// Clé absente : rien n'est capturé ni renvoyé, aucun événement nouveau. La
// requête est celle d'avant la clé.
func TestEchoDefautRienNeChange(t *testing.T) {
	echoTestSetup(t, false)
	m := &moteurEcho{reply: scriptBoucle}
	m.start(t)
	var echoes int
	extra, err := runChat(context.Background(), []Message{um("liste les fichiers")}, 0.7, Caps{Agent: true}, func(ev StreamEvent) bool {
		if ev.Echo != nil {
			echoes++
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	bodies := m.all()
	if len(bodies) != 2 {
		t.Fatalf("%d requêtes, attendu 2", len(bodies))
	}
	for i, b := range bodies {
		if strings.Contains(b, "reasoning_content") || strings.Contains(b, "reasoning_model") {
			t.Fatalf("requête %d : raisonnement envoyé sans la clé : %s", i, b)
		}
	}
	for _, e := range extra {
		if e.ReasoningContent != "" || e.ReasoningModel != "" {
			t.Fatalf("raisonnement capturé sans la clé : %+v", e)
		}
	}
	if echoes != 0 {
		t.Fatalf("%d événements Echo sans la clé", echoes)
	}
}

// Point de sortie : sans raisonnement nulle part, la tranche ressort telle
// quelle ; avec du raisonnement stocké mais une politique éteinte, le JSON est
// octet pour octet celui d'avant le champ.
func TestEchoMessagesOctetPourOctet(t *testing.T) {
	plain := []Message{
		{Role: "system", Content: "sys"},
		um("question"),
		{Role: "assistant", Content: "je regarde", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: ToolCallFunc{Name: "bash", Arguments: "{}"}}}},
		tm("sortie"),
		am("réponse"),
	}
	out, n := echoMessages(plain, echoPolicy{on: true, model: echoTestModel, keepsPast: true})
	if n != 0 || &out[0] != &plain[0] {
		t.Fatal("aucun raisonnement : la tranche doit ressortir sans copie")
	}
	want, _ := json.Marshal(plain)
	stored := append([]Message(nil), plain...)
	stored[2].ReasoningContent, stored[2].ReasoningModel = "raisonnement", echoTestModel
	stored[4].ReasoningContent, stored[4].ReasoningModel = "autre", echoTestModel
	for _, p := range []echoPolicy{{}, {on: true, model: "autre-modele.gguf", keepsPast: true}} {
		got, n := echoMessages(stored, p)
		b, _ := json.Marshal(got)
		if n != 0 || !bytes.Equal(b, want) {
			t.Fatalf("politique %+v : %s\nattendu %s", p, b, want)
		}
	}
	if stored[2].ReasoningContent == "" {
		t.Fatal("echoMessages a modifié l'historique d'origine")
	}
}

// Ce qui part : seulement le raisonnement du même modèle, jamais l'étiquette,
// jamais sur un message assistant sans texte ni appel.
func TestEchoMessagesPolitique(t *testing.T) {
	p := echoPolicy{on: true, model: echoTestModel, keepsPast: true}
	msgs := []Message{
		um("q"),
		{Role: "assistant", Content: "a", ReasoningContent: "r1", ReasoningModel: echoTestModel},
		{Role: "assistant", Content: "b", ReasoningContent: "r2", ReasoningModel: "Llama-3.gguf"},
		{Role: "assistant", ReasoningContent: "r3", ReasoningModel: echoTestModel}, // ni texte ni appel
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: ToolCallFunc{Name: "bash", Arguments: "{}"}}}, ReasoningContent: "r4", ReasoningModel: echoTestModel},
		{Role: "user", Content: "x", ReasoningContent: "r5", ReasoningModel: echoTestModel},
	}
	out, n := echoMessages(msgs, p)
	if n != 2 || out[1].ReasoningContent != "r1" || out[4].ReasoningContent != "r4" {
		t.Fatalf("renvoyés : %d, %+v", n, out)
	}
	for i, m := range out {
		if m.ReasoningModel != "" {
			t.Fatalf("étiquette du modèle envoyée (message %d)", i)
		}
		if m.Role == "assistant" && m.ReasoningContent != "" && !hasBody(m) {
			t.Fatalf("message assistant de pur raisonnement envoyé (message %d)", i)
		}
	}
	if out[2].ReasoningContent != "" || out[3].ReasoningContent != "" || out[5].ReasoningContent != "" {
		t.Fatalf("raisonnement envoyé à tort : %+v", out)
	}
}

func TestEchoPolicyFor(t *testing.T) {
	echoTestSetup(t, false)
	base := map[string]string{"REASONING_ECHO": "on", "MODEL": "/models/" + echoTestModel}
	with := func(k, v string) map[string]string {
		c := map[string]string{}
		for a, b := range base {
			c[a] = b
		}
		c[k] = v
		return c
	}
	if p := echoPolicyFor(base); !p.on || p.model != echoTestModel || !p.keepsPast {
		t.Fatalf("clé posée : %+v", p)
	}
	if p := echoPolicyFor(with("REASONING_ECHO", "")); p.on {
		t.Fatal("clé absente : écho actif")
	}
	if p := echoPolicyFor(with(extKeyFlag, "1")); p.on {
		t.Fatal("preset externe : écho actif")
	}
	if p := echoPolicyFor(with("MODEL", "")); p.on {
		t.Fatal("modèle inconnu : écho actif")
	}
	echoRefuse(echoTestModel)
	if p := echoPolicyFor(base); p.on {
		t.Fatal("gabarit qui a refusé : écho actif")
	}
	echoRefused.Delete(echoTestModel)
	// Verdicts de la sonde, seulement pour CE modèle.
	set := func(r tplProbeResult) {
		tplProbeMu.Lock()
		tplProbeLast = &r
		tplProbeMu.Unlock()
	}
	set(tplProbeResult{Model: echoTestModel, RendersReasoning: tplNo, PreservesHistory: tplNo})
	if p := echoPolicyFor(base); p.on {
		t.Fatal("gabarit qui ne rend jamais le raisonnement : écho actif")
	}
	set(tplProbeResult{Model: echoTestModel, RendersReasoning: tplYes, PreservesHistory: tplNo})
	if p := echoPolicyFor(base); !p.on || p.keepsPast {
		t.Fatalf("gabarit façon Qwen3.5 : %+v", p)
	}
	set(tplProbeResult{Model: "autre.gguf", RendersReasoning: tplNo, PreservesHistory: tplNo})
	if p := echoPolicyFor(base); !p.on || !p.keepsPast {
		t.Fatalf("verdict d'un autre modèle appliqué : %+v", p)
	}
}

// Comptage : le raisonnement d'après la dernière question compte toujours,
// celui d'avant seulement si le gabarit garde le passé (ou qu'on l'ignore).
func TestEchoTokensSuitLeRendu(t *testing.T) {
	r := strings.Repeat("r", 400) // 100 jetons
	msgs := []Message{
		um("q1"),
		{Role: "assistant", Content: "a", ReasoningContent: r, ReasoningModel: echoTestModel},
		um("q2"),
		{Role: "assistant", Content: "b", ReasoningContent: r, ReasoningModel: echoTestModel},
	}
	sum := func(v []int) int {
		s := 0
		for _, n := range v {
			s += n
		}
		return s
	}
	if got := sum(echoTokensFor(msgs, echoPolicy{on: true, model: echoTestModel, keepsPast: true})); got != 200 {
		t.Fatalf("gabarit qui garde le passé : %d, attendu 200", got)
	}
	if got := sum(echoTokensFor(msgs, echoPolicy{on: true, model: echoTestModel})); got != 100 {
		t.Fatalf("gabarit qui retire le passé : %d, attendu 100", got)
	}
	if got := echoTokensFor(msgs, echoPolicy{}); got != nil {
		t.Fatalf("écho coupé : %v", got)
	}
	// estimateTokens suit la configuration réelle.
	echoTestSetup(t, false)
	without := estimateTokens(msgs)
	if err := SetConfigKey("REASONING_ECHO", "on"); err != nil {
		t.Fatal(err)
	}
	if got := estimateTokens(msgs); got != without+200 {
		t.Fatalf("estimateTokens avec l'écho = %d, attendu %d", got, without+200)
	}
}

// shrinkToFit retire d'abord le raisonnement, du plus ancien au plus récent,
// même dans un seul long tour sans frontière utilisateur.
func TestShrinkRetireDAbordLeRaisonnement(t *testing.T) {
	echoTestSetup(t, true)
	if err := SetConfigKey("CTX", "8192"); err != nil {
		t.Fatal(err)
	}
	r := strings.Repeat("r", 8000) // 2000 jetons chacun
	msgs := []Message{um("corrige le build")}
	for i := 0; i < 4; i++ {
		msgs = append(msgs,
			Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: ToolCallFunc{Name: "bash", Arguments: "{}"}}}, ReasoningContent: r, ReasoningModel: echoTestModel},
			tm("sortie courte"))
	}
	out, changed := shrinkToFit(msgs, 0)
	if !changed {
		t.Fatal("rien retiré")
	}
	if len(out) != len(msgs) {
		t.Fatalf("des messages ont été retirés (%d → %d) alors que le raisonnement suffisait", len(msgs), len(out))
	}
	if out[1].ReasoningContent != "" || out[len(out)-2].ReasoningContent == "" {
		t.Fatal("ordre : le plus ancien raisonnement doit partir en premier, le plus récent rester")
	}
	for i, m := range out {
		if m.Role == "tool" && msgText(m) != "sortie courte" {
			t.Fatalf("résultat d'outil %d tronqué avant le raisonnement", i)
		}
	}
	if msgs[1].ReasoningContent == "" {
		t.Fatal("shrinkToFit a modifié l'historique d'origine")
	}
}

// Clé posée : le raisonnement séparé de l'étape d'outil repart avec elle, sans
// étiquette ; celui de la réponse finale est rendu à l'appelant.
func TestEchoRenvoieLeRaisonnementSepare(t *testing.T) {
	echoTestSetup(t, true)
	m := &moteurEcho{reply: scriptBoucle}
	m.start(t)
	var echo *ReasoningEcho
	extra, err := runChat(context.Background(), []Message{um("liste les fichiers")}, 0.7, Caps{Agent: true}, func(ev StreamEvent) bool {
		if ev.Echo != nil {
			echo = ev.Echo
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(extra) < 1 || extra[0].ReasoningContent != "je cherche les fichiers" || extra[0].ReasoningModel != echoTestModel {
		t.Fatalf("message tool_calls : %+v", extra)
	}
	bodies := m.all()
	if len(bodies) != 2 {
		t.Fatalf("%d requêtes", len(bodies))
	}
	if strings.Contains(bodies[1], "reasoning_model") {
		t.Fatal("étiquette du modèle envoyée au moteur")
	}
	msgs := requestMessages(t, bodies[1])
	var found bool
	for _, mm := range msgs {
		if mm["role"] == "assistant" && mm["reasoning_content"] == "je cherche les fichiers" {
			found = true
		}
	}
	if !found {
		t.Fatalf("raisonnement de l'étape non renvoyé : %s", bodies[1])
	}
	if echo == nil || echo.Text != "j'ai trouvé" || echo.Model != echoTestModel {
		t.Fatalf("raisonnement final : %+v", echo)
	}
}

// Découpage « </think> » fait chez nous : jamais copié dans ReasoningContent
// (le gabarit le redécouperait, rendu en double).
func TestEchoJamaisDepuisLeDecoupageClient(t *testing.T) {
	echoTestSetup(t, true)
	if err := SetConfigKey("REASONING", "on"); err != nil {
		t.Fatal(err)
	}
	m := &moteurEcho{reply: func(n int, _ string) (int, string) {
		if n == 0 {
			return 200, sseChunk("je pense") + sseChunk("</think>") + sseChunk("je regarde") + sseGlobStep
		}
		return 200, sseChunk("pensée</think>") + sseChunk("Voilà.") + sseFinal("stop", 100, 4)
	}}
	m.start(t)
	var echo *ReasoningEcho
	extra, err := runChat(context.Background(), []Message{um("liste")}, 0.7, Caps{Agent: true}, func(ev StreamEvent) bool {
		if ev.Echo != nil {
			echo = ev.Echo
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range m.all() {
		if strings.Contains(b, "reasoning_content") {
			t.Fatalf("raisonnement découpé chez nous renvoyé à part : %s", b)
		}
	}
	if len(extra) < 1 || extra[0].ReasoningContent != "" || msgText(extra[0]) != "je pense</think>je regarde" {
		t.Fatalf("message tool_calls : %+v", extra)
	}
	if echo != nil {
		t.Fatalf("raisonnement final tiré du découpage client : %+v", echo)
	}
}

// Une relance « pensé sans agir » (DropReasoning) : seul le raisonnement de la
// complétion retenue accompagne la réponse.
func TestEchoApresDropReasoning(t *testing.T) {
	echoTestSetup(t, true)
	m := &moteurEcho{reply: func(n int, _ string) (int, string) {
		if n == 0 {
			return 200, sseReasoning("mort-né") + sseFinal("stop", 100, 3)
		}
		return 200, sseReasoning("retenu") + sseChunk("Fait.") + sseFinal("stop", 110, 3)
	}}
	m.start(t)
	var echo *ReasoningEcho
	var dropped bool
	if _, err := runChat(context.Background(), []Message{um("fais-le")}, 0.7, Caps{Agent: true}, func(ev StreamEvent) bool {
		if ev.Echo != nil {
			echo = ev.Echo
		}
		if ev.DropReasoning {
			dropped = true
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if !dropped || echo == nil || echo.Text != "retenu" {
		t.Fatalf("après relance : dropped=%v echo=%+v", dropped, echo)
	}
}

// generate range le raisonnement final avec la réponse, et rien sans la clé.
func TestGenerateRangeLeRaisonnement(t *testing.T) {
	for _, on := range []bool{false, true} {
		echoTestSetup(t, on)
		m := &moteurEcho{reply: func(int, string) (int, string) {
			return 200, sseReasoning("réflexion") + sseChunk("Bonjour.") + sseFinal("stop", 50, 3)
		}}
		m.start(t)
		c := newTestConv()
		c.Messages = []Message{um("salut")}
		c.generate(context.Background(), Caps{}, 0.7, c.epoch)
		last := c.Messages[len(c.Messages)-1]
		if msgText(last) != "Bonjour." {
			t.Fatalf("on=%v : dernier message %+v", on, last)
		}
		if on && (last.ReasoningContent != "réflexion" || last.ReasoningModel != echoTestModel) {
			t.Fatalf("clé posée : raisonnement non rangé : %+v", last)
		}
		if !on && (last.ReasoningContent != "" || last.ReasoningModel != "") {
			t.Fatalf("clé absente : raisonnement rangé : %+v", last)
		}
	}
}

// Historique qui porte un raisonnement de ce modèle.
func echoHistory() []Message {
	return []Message{
		um("question"),
		{Role: "assistant", Content: "préambule", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: ToolCallFunc{Name: "glob", Arguments: `{"pattern":"*"}`}}},
			ReasoningContent: "réflexion passée", ReasoningModel: echoTestModel},
		tm("a.go"),
	}
}

// Gabarit qui refuse (gpt-oss sur un moteur ancien) : rejoué une fois sans
// raisonnement, outils et prompt intacts, et le refus est retenu.
func TestEchoErreurDeGabaritRelanceSansRaisonnement(t *testing.T) {
	echoTestSetup(t, true)
	m := &moteurEcho{reply: func(_ int, body string) (int, string) {
		if strings.Contains(body, "reasoning_content") {
			return 500, `{"error":{"code":500,"message":"Jinja Exception: raise_exception('Cannot pass both content and thinking in an assistant message with tool calls')"}}`
		}
		return 200, sseChunk("Réponse.") + sseFinal("stop", 80, 2)
	}}
	m.start(t)
	var content strings.Builder
	if _, err := runChat(context.Background(), echoHistory(), 0.7, Caps{Agent: true}, func(ev StreamEvent) bool {
		content.WriteString(ev.Content)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	bodies := m.all()
	if len(bodies) != 2 || content.String() != "Réponse." {
		t.Fatalf("%d requêtes, réponse %q", len(bodies), content.String())
	}
	if strings.Contains(bodies[1], "reasoning_content") || strings.Contains(bodies[1], `"tool_choice"`) ||
		!strings.Contains(bodies[1], `"tools"`) {
		t.Fatalf("relance : raisonnement retiré, outils intacts attendus : %s", bodies[1])
	}
	if !echoIsRefused(echoTestModel) {
		t.Fatal("refus du gabarit non retenu")
	}
	if p := currentEchoPolicy(); p.on {
		t.Fatal("écho encore actif après le refus")
	}
}

// Erreur de gabarit qui n'a rien à voir avec le raisonnement : la relance
// échoue aussi, et rien n'est retenu contre l'écho.
func TestEchoErreurDeGabaritSansRapport(t *testing.T) {
	echoTestSetup(t, true)
	m := &moteurEcho{reply: func(int, string) (int, string) {
		return 500, `{"error":{"code":500,"message":"Jinja Exception: raise_exception('Conversation roles must alternate')"}}`
	}}
	m.start(t)
	if _, err := runChat(context.Background(), echoHistory(), 0.7, Caps{}, func(StreamEvent) bool { return true }); err == nil {
		t.Fatal("erreur attendue")
	}
	bodies := m.all()
	if len(bodies) < 2 || !strings.Contains(bodies[0], "reasoning_content") {
		t.Fatalf("%d requêtes ; la première doit porter le raisonnement", len(bodies))
	}
	// Ensuite, les filets habituels (tool_choice none, outils coupés) partent
	// sans raisonnement : il n'est plus envoyé de ce tour.
	for i, b := range bodies[1:] {
		if strings.Contains(b, "reasoning_content") {
			t.Fatalf("requête %d : raisonnement renvoyé après le repli", i+1)
		}
	}
	if echoIsRefused(echoTestModel) {
		t.Fatal("refus retenu alors que le raisonnement n'y était pour rien")
	}
}

// Prompt trop long avec du raisonnement : relance sans lui AVANT toute
// compaction (aucune perte d'information).
func TestEchoDebordementRelanceSansRaisonnementAvantCompaction(t *testing.T) {
	echoTestSetup(t, true)
	m := &moteurEcho{reply: func(_ int, body string) (int, string) {
		if strings.Contains(body, "reasoning_content") {
			return 400, `{"error":{"code":400,"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error"}}`
		}
		return 200, sseChunk("Réponse.") + sseFinal("stop", 80, 2)
	}}
	m.start(t)
	var compacted bool
	if _, err := runChat(context.Background(), echoHistory(), 0.7, Caps{Agent: true}, func(ev StreamEvent) bool {
		if ev.NewHistory != nil {
			compacted = true
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	bodies := m.all()
	if len(bodies) != 2 || strings.Contains(bodies[1], "reasoning_content") {
		t.Fatalf("%d requêtes ; relance sans raisonnement attendue", len(bodies))
	}
	m.mu.Lock()
	summaries := m.summary
	m.mu.Unlock()
	if compacted || summaries != 0 {
		t.Fatalf("compaction avant la relance sans raisonnement (résumés=%d)", summaries)
	}
	if echoIsRefused(echoTestModel) {
		t.Fatal("un débordement n'est pas un refus du gabarit")
	}
}

// Passe de correction du mode Code : une compaction survenue pendant la passe
// REMPLACE l'historique, comme dans generate, au lieu d'être perdue.
func TestBuilderAppliqueNewHistory(t *testing.T) {
	echoTestSetup(t, false)
	convEnsureActive()
	m := &moteurEcho{reply: func(n int, _ string) (int, string) {
		if n == 0 {
			return 400, `{"error":{"code":400,"message":"request (99999 tokens) exceeds the available context size"}}`
		}
		return 200, sseChunk("Corrigé.") + sseFinal("stop", 80, 2)
	}}
	m.start(t)
	c := newTestConv()
	c.Messages = []Message{um("fais le build")}
	for i := 0; i < 12; i++ {
		c.Messages = append(c.Messages, am("étape "+strings.Repeat("détail ", 300)), um("continue "+strings.Repeat("x", 600)))
	}
	before := len(c.Messages)
	c.runBuilderTurn(context.Background(), Caps{Agent: true, Code: true}, 0.2, c.epoch, "Fix the failing test.")
	if len(c.Messages) >= before {
		t.Fatalf("compaction perdue : %d messages (avant %d)", len(c.Messages), before)
	}
	if !strings.HasPrefix(msgText(c.Messages[0]), compactSummaryPrefix) {
		t.Fatalf("premier message : %q", msgText(c.Messages[0]))
	}
	if c.Messages[0].Role == "system" {
		t.Fatal("préfixe système injecté persisté")
	}
	fix := 0
	for _, mm := range c.Messages {
		if msgText(mm) == "Fix the failing test." {
			fix++
		}
	}
	if fix != 1 || msgText(c.Messages[len(c.Messages)-1]) != "Corrigé." {
		t.Fatalf("consigne présente %d fois, dernier message %q", fix, msgText(c.Messages[len(c.Messages)-1]))
	}
}

// Corrections de relecture du lot 2 : un refus de gabarit est reconnu, une
// erreur d'analyse d'appel d'outil qui cite le mot « thinking » ne l'est pas.
func TestEchoTemplateErrorEtroit(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{500, `{"error":{"message":"Jinja Exception: raise_exception('Cannot pass both content and thinking')"}}`, true},
		{500, `{"error":{"message":"Error in template: thinking blocks not allowed here"}}`, true},
		{400, `{"error":{"message":"Expected 'content' or 'tool_calls'"}}`, true},
		{500, `{"error":{"message":"Failed to parse input at pos 12: <tool_call>{\"thinking\": 1}"}}`, false},
		{500, `{"error":{"message":"the model kept thinking forever"}}`, false},
		{502, `{"error":{"message":"raise_exception"}}`, false},
	}
	for _, c := range cases {
		if got := echoTemplateError(c.status, c.body); got != c.want {
			t.Errorf("%d %s : %v", c.status, c.body, got)
		}
	}
}
