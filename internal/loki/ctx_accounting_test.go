package loki

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Le contexte de la requête suivante = prompt + généré − raisonnement (jamais
// renvoyé), sans jamais retirer plus que ce qui a été généré.
func TestStatsCtxAfter(t *testing.T) {
	cases := []struct {
		name                 string
		prompt, gen, reasonN int
		want                 int
	}{
		{"sans raisonnement", 40000, 500, 0, 40500},
		{"raisonnement retiré", 40000, 6000, 5000, 41000},
		{"tout est raisonnement", 40000, 3000, 3000, 40000},
		{"compte de morceaux au-delà du généré : borné", 40000, 2000, 2500, 40000},
		{"pas d'usage", 0, 300, 100, 200},
	}
	for _, c := range cases {
		s := StatsEvent{PromptTokensTotal: c.prompt, GenTokens: c.gen, ReasoningTokens: c.reasonN}
		if got := s.ctxAfter(); got != c.want {
			t.Errorf("%s : ctxAfter = %d, attendu %d", c.name, got, c.want)
		}
	}
}

// Garde de marge : à 65 k comme à 131 k, plancher et marge seuls ne devancent
// jamais le seuil de 75 % ; seule une génération récente longue l'avance.
func TestGenHeadroomShort(t *testing.T) {
	cases := []struct {
		name                 string
		used, peak, window   int
		want                 bool
		ratioAlreadyTriggers bool
	}{
		{"65k, inconnu", 60000, 0, 65536, false, true},
		{"65k, courte : sous le seuil, pas d'avance", 49000, 2000, 65536, false, false},
		{"65k, courte : plancher 8k + marge, sous 84 %", 55000, 2000, 65536, false, true},
		{"65k, longue : 1,2×15k ne tient plus", 47000, 15000, 65536, true, false},
		{"65k, longue mais la place y est", 40000, 15000, 65536, false, false},
		{"65k, énorme (45k) mais historique tout juste compacté", 20000, 45000, 65536, false, false},
		{"65k, énorme (45k) au-delà de la moitié", 33000, 45000, 65536, true, false},
		{"131k, courte : rien avant le seuil", 98000, 4000, 131072, false, false},
		{"131k, très longue génération", 95000, 30000, 131072, true, false},
		{"8k, plancher ramené à 1/8", 6000, 0, 8192, false, false},
		{"contexte inconnu", 0, 30000, 65536, false, false},
	}
	for _, c := range cases {
		if got := genHeadroomShort(c.used, c.peak, c.window); got != c.want {
			t.Errorf("%s : genHeadroomShort = %v, attendu %v", c.name, got, c.want)
		}
		ratio := c.used >= int(float64(c.window)*compactTriggerFrac)
		if ratio != c.ratioAlreadyTriggers {
			t.Errorf("%s : cas mal posé, seuil de 75 %% = %v", c.name, ratio)
		}
	}
	// Sans génération longue, la garde ne part jamais avant le seuil de 75 %.
	for _, w := range []int{4096, 8192, 16384, 32768, 65536, 131072, 262144} {
		below := int(float64(w)*compactTriggerFrac) - 1
		if genHeadroomShort(below, 1, w) {
			t.Errorf("fenêtre %d : la garde devance le seuil sans génération longue", w)
		}
		// Génération aussi longue que la fenêtre : jamais avant la moitié, sinon
		// l'historique fraîchement compacté la redéclenche à chaque étape.
		if genHeadroomShort(w/2, w, w) {
			t.Errorf("fenêtre %d : la garde part à 50 %% (compaction en boucle)", w)
		}
		if !genHeadroomShort(w/2+1, w, w) {
			t.Errorf("fenêtre %d : la garde ne part pas au-delà de 50 %% avec une génération énorme", w)
		}
	}
}

// Le message arrivé depuis la dernière mesure s'ajoute ; une mesure inconnue
// ou qui ne correspond plus à l'historique n'ajoute rien.
func TestCtxPending(t *testing.T) {
	msgs := []Message{um("bonjour"), am("salut"), um(strings.Repeat("x", 4000))}
	added := estimateTokens(msgs[2:])
	cases := []struct {
		name           string
		used, measured int
		want           int
	}{
		{"nouveau message estimé", 30000, 2, 30000 + added},
		{"rien de nouveau", 30000, 3, 30000},
		{"mesure inconnue (rechargement)", 30000, 0, 30000},
		{"historique raccourci depuis", 30000, 5, 30000},
		{"aucun compte : repli sur l'estimation complète, ailleurs", 0, 2, 0},
	}
	for _, c := range cases {
		if got := ctxPending(c.used, c.measured, msgs); got != c.want {
			t.Errorf("%s : ctxPending = %d, attendu %d", c.name, got, c.want)
		}
	}
}

// fauxMoteurScript : llama-server de comédie qui sert ses réponses streamées
// dans l'ordre (la dernière se répète), et un résumé JSON aux appels non
// streamés (compaction). Garde le dernier message de chaque requête streamée.
func fauxMoteurScript(t *testing.T, streams ...string) (port string, lastMsgs func() []string) {
	t.Helper()
	var mu sync.Mutex
	var last []string
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream   bool      `json:"stream"`
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Stream {
			sendJSON(w, 200, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "- résumé des étapes précédentes"}}}})
			return
		}
		mu.Lock()
		i := min(n, len(streams)-1)
		n++
		if len(body.Messages) > 0 {
			last = append(last, msgText(body.Messages[len(body.Messages)-1]))
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(streams[i] + "data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Port(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), last...)
	}
}

func sseReasoning(text string) string {
	return `data: {"choices":[{"delta":{"reasoning_content":"` + text + `"}}]}` + "\n\n"
}

func sseFinal(finish string, prompt, gen int) string {
	return `data: {"choices":[{"delta":{},"finish_reason":"` + finish + `"}]}` + "\n\n" +
		`data: {"choices":[],"timings":{"prompt_n":10,"predicted_n":` + itoa(gen) + `},"usage":{"prompt_tokens":` + itoa(prompt) + `,"completion_tokens":` + itoa(gen) + `}}` + "\n\n"
}

// runForStats lance runChat et rend le dernier StatsEvent reçu.
func runForStats(t *testing.T, msgs []Message, caps Caps) (StatsEvent, string) {
	t.Helper()
	var st StatsEvent
	var content strings.Builder
	if _, err := runChat(context.Background(), msgs, 0.7, caps, func(ev StreamEvent) bool {
		if ev.Stats != nil {
			st = *ev.Stats
		}
		content.WriteString(ev.Content)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return st, content.String()
}

// Raisonnement séparé par le moteur : compté, et retiré du contexte suivant.
func TestRaisonnementSepareCompte(t *testing.T) {
	testHome(t)
	port, _ := fauxMoteurScript(t, sseReasoning("je ")+sseReasoning("réfléchis ")+sseReasoning("ici")+
		sseChunk("Bon")+sseChunk("jour")+sseFinal("stop", 100, 7))
	if err := SetConfigKey("PORT", port); err != nil {
		t.Fatal(err)
	}
	st, _ := runForStats(t, []Message{um("salut")}, Caps{})
	if st.ReasoningTokens != 3 {
		t.Fatalf("ReasoningTokens = %d, attendu 3", st.ReasoningTokens)
	}
	if got := st.ctxAfter(); got != 104 {
		t.Fatalf("ctxAfter = %d, attendu 104 (100 + 7 − 3)", got)
	}
}

// <think> en ligne découpé chez nous : ce texte reste dans le message renvoyé
// au modèle pendant le tour, il ne doit donc PAS être retiré du compte.
func TestRaisonnementEnLigneNonCompte(t *testing.T) {
	testHome(t)
	port, _ := fauxMoteurScript(t, sseChunk("je pense")+sseChunk("</think>")+sseChunk("Réponse")+sseFinal("stop", 100, 6))
	if err := SetConfigKey("PORT", port); err != nil {
		t.Fatal(err)
	}
	if err := SetConfigKey("REASONING", "on"); err != nil {
		t.Fatal(err)
	}
	var sawReasoning bool
	var st StatsEvent
	if _, err := runChat(context.Background(), []Message{um("salut")}, 0.7, Caps{}, func(ev StreamEvent) bool {
		if ev.Reasoning != "" {
			sawReasoning = true
		}
		if ev.Stats != nil {
			st = *ev.Stats
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if !sawReasoning {
		t.Fatal("le <think> en ligne n'a pas été découpé : test mal posé")
	}
	if st.ReasoningTokens != 0 || st.ctxAfter() != 106 {
		t.Fatalf("raisonnement en ligne compté : ReasoningTokens=%d ctxAfter=%d", st.ReasoningTokens, st.ctxAfter())
	}
}

// Une relance (ici le nudge « pensé sans agir ») repart d'un compte vierge :
// le raisonnement d'une tentative jetée n'est pas retiré deux fois.
func TestRaisonnementRemisAZeroALaRelance(t *testing.T) {
	withWorkspace(t)
	port, _ := fauxMoteurScript(t,
		sseReasoning("a")+sseReasoning("b")+sseReasoning("c")+sseFinal("stop", 100, 3),
		sseReasoning("d")+sseChunk("Fait.")+sseFinal("stop", 120, 4))
	if err := SetConfigKey("PORT", port); err != nil {
		t.Fatal(err)
	}
	st, content := runForStats(t, []Message{um("fais-le")}, Caps{Agent: true})
	if content != "Fait." {
		t.Fatalf("réponse = %q, la relance n'a pas eu lieu", content)
	}
	if st.ReasoningTokens != 1 || st.ctxAfter() != 123 {
		t.Fatalf("après relance : ReasoningTokens=%d ctxAfter=%d, attendu 1 et 123", st.ReasoningTokens, st.ctxAfter())
	}
}

// Fenêtre pleine en plein raisonnement (« length ») : on compacte et on rejoue
// l'étape, sans le nudge qui demanderait au modèle d'arrêter de raisonner.
func TestFenetrePleineCompacteEtRejoue(t *testing.T) {
	withWorkspace(t)
	if err := SetConfigKey("CTX", "4096"); err != nil {
		t.Fatal(err)
	}
	port, lastMsgs := fauxMoteurScript(t,
		sseReasoning("long")+sseReasoning(" raisonnement")+sseFinal("length", 3900, 196),
		sseChunk("Réponse.")+sseFinal("stop", 1500, 3))
	if err := SetConfigKey("PORT", port); err != nil {
		t.Fatal(err)
	}
	msgs := []Message{um("corrige le build"), am("ok"), um("continue")}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, atc("bash"), tm("sortie "+strings.Repeat("x", 400)))
	}
	msgs = append(msgs, um("et maintenant ?"))
	var compacted, dropped bool
	var content strings.Builder
	if _, err := runChat(context.Background(), msgs, 0.7, Caps{Agent: true}, func(ev StreamEvent) bool {
		if ev.NewHistory != nil {
			compacted = true
		}
		if ev.DropReasoning {
			dropped = true
		}
		content.WriteString(ev.Content)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if !compacted || !dropped {
		t.Fatalf("fenêtre pleine : compacté=%v raisonnement retiré=%v", compacted, dropped)
	}
	if content.String() != "Réponse." {
		t.Fatalf("réponse = %q", content.String())
	}
	for _, m := range lastMsgs() {
		if strings.Contains(m, "You reasoned but did not") || strings.Contains(m, "Stop reasoning") {
			t.Fatalf("nudge envoyé malgré la fenêtre pleine : %q", m)
		}
	}
}

// La passe du vérificateur tourne sur sa propre trace : ses comptes ne
// touchent ni CtxUsed ni la jauge, contrairement à une passe de correction.
func TestPasseIsoleeNeTouchePasLeContexte(t *testing.T) {
	testHome(t)
	c := newTestConv()
	c.CtxUsed = 45000
	st := &StatsEvent{PromptTokensTotal: 3000, GenTokens: 800, ReasoningTokens: 500}
	c.forwardStream(StreamEvent{Stats: st}, c.epoch, true)
	if c.CtxUsed != 45000 {
		t.Fatalf("passe isolée : CtxUsed = %d, attendu 45000 inchangé", c.CtxUsed)
	}
	if last := c.Log[len(c.Log)-1].Delta; last["ctx_isolated"] != true {
		t.Fatalf("passe isolée : la jauge n'est pas prévenue (%v)", last)
	}
	c.forwardStream(StreamEvent{Stats: st}, c.epoch, false)
	if c.CtxUsed != 3300 {
		t.Fatalf("passe de correction : CtxUsed = %d, attendu 3300 (3000 + 800 − 500)", c.CtxUsed)
	}
	if c.genPeak != 800 {
		t.Fatalf("passe de correction : genPeak = %d, attendu 800", c.genPeak)
	}
}

// Preset externe : le compte mesuré seul, comme avant ; local : complété des
// messages arrivés depuis la mesure.
func TestCtxNowLockedExternal(t *testing.T) {
	msgs := []Message{um("bonjour"), am("salut"), um(strings.Repeat("x", 4000))}
	c := &Conversation{CtxUsed: 30000, ctxUsedLen: 2}
	if got := c.ctxNowLocked(msgs, false); got != 30000 {
		t.Errorf("externe : %d, attendu le compte mesuré seul (30000)", got)
	}
	if got, want := c.ctxNowLocked(msgs, true), 30000+estimateTokens(msgs[2:]); got != want {
		t.Errorf("local : %d, attendu %d", got, want)
	}
}
