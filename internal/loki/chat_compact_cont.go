package loki

// chat_compact_cont.go — COMPACT_CONTINUATION : la compaction proactive comme
// suite du prompt que le moteur a déjà en cache.
//
// Pourquoi. Aujourd'hui, le résumé d'une compaction part dans une requête à
// part : son propre prompt système et une transcription du torse. Le moteur
// local la calcule à froid (5 à 46k jetons), puis recalcule à froid le prompt
// compacté au message suivant. Sur un 27B, des dizaines de secondes ; sur un
// MoE aux experts en RAM, des minutes — à chaque compaction.
//
// Avec la clé (off par défaut), quand le slot porte selon toute vraisemblance
// le prompt de CETTE discussion, la requête de résumé est la requête du tour
// telle qu'elle est partie — mêmes messages assemblés par les mêmes fonctions
// (turnViewDry, wireMessages, buildChatPayload), mêmes outils, mêmes arguments
// du gabarit, même intensité de raisonnement — suivie d'un seul message
// utilisateur qui demande le résumé. Le moteur ne calcule que ce message.
//
// Ce qui ne change pas :
//   - la compaction elle-même : bornes, archives recall, demande réinjectée,
//     garantie de réduction et historique rangé sont calculés sur la vue
//     MODÈLE, comme avant. La vue d'envoi ne sert qu'à construire la requête :
//     ni prompt système, ni skills, ni bloc projet n'entrent dans l'historique ;
//   - les exigences du résumé : mêmes règles (compactSummaryRules, plus
//     codeSummaryRules en mode Code), même budget, même température 0.2 sans
//     l'échantillonnage du preset, réflexion coupée (enable_thinking=false,
//     ajouté aux arguments du tour) ;
//   - le modèle principal, ses tours et leur échantillonnage.
//
// Ce qui change : le résumeur voit le torse entier dans son format natif (plus
// que la transcription, qui raccourcit les résultats d'outils), depuis la
// place de l'agent. La frontière lui est désignée par le nombre de messages
// gardés et le début du premier d'entre eux ; en cas de doute, il doit plutôt
// répéter que laisser tomber, et dater l'état d'avancement à la frontière.
//
// Repli sur la transcription d'avant, sans rien perdre, au moindre écart :
// preset externe, autre requête passée par le slot depuis le dernier tour
// (vérificateur, sous-agent, tâche, préchauffage, résumé, bench, client /v1,
// autre discussion), moteur relancé sur un autre modèle ou une autre fenêtre,
// étape de secours (outils coupés), marge insuffisante dans la fenêtre, vue
// désalignée, refus du moteur (gabarit qui exige l'alternance, fenêtre
// pleine…), erreur réseau, appel d'outil émis, raisonnement sans résumé,
// résumé vide. Un refus du gabarit ou un appel d'outil suspend la continuation
// pour ce modèle jusqu'au redémarrage ; deux résumés ratés de suite aussi.
//
// La clé apporte aussi deux économies sans effet sur le résultat :
//   - un résumé n'est pas demandé quand même un résumé vide ne passerait pas la
//     garantie de réduction (compactHopeless, borne exacte) ;
//   - une compaction refusée parce que le résumé obtenu était trop long n'est
//     retentée, en début ou fin de tour et entre deux étapes, qu'une fois le
//     contexte grossi de 10 % sur le même fil (un historique réécrit retente)
//     — jamais pour le filet réactif, le bouton « compacter », une fenêtre
//     pleine à 90 % ou plus.
//
// Réactif (prompt refusé par le moteur), fenêtre pleine en pleine génération,
// bouton manuel, tâches, sous-agents, terminal : chemin d'avant, toujours.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"unicode/utf8"
)

// compactContinuationOn : clé posée, moteur local.
func compactContinuationOn(cfg map[string]string) bool {
	if isExternalConfig(cfg) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(cfg["COMPACT_CONTINUATION"])) {
	case "on", "1", "true", "yes", "oui":
		return true
	}
	return false
}

// compactView : la requête du tour telle qu'elle est partie, pour un résumé
// en continuation. wire couvre les n premiers messages de la vue modèle.
type compactView struct {
	wire   []Message
	n      int
	tools  []Tool
	effort string
	kwargs map[string]any
	used   int    // contexte réel (dernier compte du moteur + ce qu'il n'a pas vu)
	conv   string // discussion, pour la télémétrie
}

// compactOpts : options d'une compaction. Nulles : le chemin d'avant, exact.
type compactOpts struct {
	view     *compactView // nil : résumé sur transcription
	precheck bool         // pas de résumé quand même un vide serait refusé
	conv     string       // discussion, pour la mémoire des refus
	used     int
}

// compactViewLen : la vue s'arrête avant les messages utilisateur de fin (le
// nouveau message, ceux de la file, une image montrée à l'étape) : le moteur
// ne les a pas en cache, et un second message utilisateur d'affilée ferait
// refuser la requête par les gabarits à alternance stricte.
func compactViewLen(msgs []Message) int {
	k := len(msgs)
	for k > 0 && msgs[k-1].Role == "user" {
		k--
	}
	return k
}

// compactOptsFor : options des compactions de début et de fin de tour.
func (c *Conversation) compactOptsFor(caps Caps, msgs []Message, used int, conv string) compactOpts {
	cfg := ReadConfig()
	if conv == "" || !compactContinuationOn(cfg) {
		return compactOpts{}
	}
	opt := compactOpts{precheck: true, conv: conv, used: used}
	if used <= 0 || !engineSlotHolds(conv) || compactContDisabled() {
		return opt
	}
	k := compactViewLen(msgs)
	if k == 0 {
		return opt
	}
	sent, tools := c.turnViewDry(caps, msgs[:k], caps.Agent)
	_, effort, _, kwargs := turnReasoning(cfg)
	wire, _ := wireMessages(sent, false, turnEchoPolicy(false))
	opt.view = &compactView{wire: wire, n: k, tools: tools, effort: effort, kwargs: kwargs, used: used, conv: conv}
	return opt
}

// compactOptsMid : options d'une compaction entre deux étapes d'un tour
// (runChatTools). messages porte déjà le prompt système et le bloc projet ;
// pol est la politique de raisonnement renvoyé de la dernière requête.
// rescue : étape de secours (outils coupés ou neutralisés), dont le prompt
// n'est pas celui du tour.
func compactOptsMid(cfg map[string]string, ep chatEndpoint, conv string, messages []Message, used int,
	tools []Tool, effort string, kwargs map[string]any, pol echoPolicy, rescue bool) compactOpts {
	if conv == "" || ep.External || !compactContinuationOn(cfg) {
		return compactOpts{}
	}
	opt := compactOpts{precheck: true, conv: conv, used: used}
	if rescue || used <= 0 || !engineSlotHolds(conv) || compactContDisabled() {
		return opt
	}
	k := compactViewLen(messages)
	if k == 0 {
		return opt
	}
	wire, _ := wireMessages(messages[:k], false, pol)
	opt.view = &compactView{wire: wire, n: k, tools: tools, effort: effort, kwargs: kwargs, used: used, conv: conv}
	return opt
}

// compactMessagesNoted : compactMessagesOpt, plus la mémoire des refus.
func compactMessagesNoted(ctx context.Context, msgs []Message, caps Caps, opt compactOpts) ([]Message, bool) {
	out, changed, refused := compactMessagesOpt(ctx, msgs, caps, opt)
	if opt.conv != "" {
		if changed {
			compactRefusedClear(opt.conv)
		} else if refused {
			compactRefusedNote(opt.conv, opt.used, msgs)
		}
	}
	return out, changed
}

// --- refus mémorisés --------------------------------------------------------

// compactRefusal : une compaction refusée faute de réduction, à at jetons de
// contexte, sur un historique de n messages d'empreinte hash. En mémoire
// seulement : un redémarrage retente.
type compactRefusal struct {
	at, window int
	model      string
	n          int
	hash       uint64
}

// historyHash : empreinte d'un historique (celle de perfPrefix).
func historyHash(msgs []Message) uint64 {
	if len(msgs) == 0 {
		return 0
	}
	p := perfPrefix(msgs)
	return p[len(p)-1]
}

var compactRefused struct {
	mu sync.Mutex
	m  map[string]compactRefusal
}

const compactRefusedMax = 64

// compactRefusedSkip : la dernière compaction de conv a été refusée, msgs ne
// fait que prolonger l'historique d'alors, et le contexte n'a pas grossi de
// 10 % depuis. Jamais à 90 % de la fenêtre ou plus ; oublié dès que
// l'historique a été réécrit (compaction, édition, régénération, discussion
// vidée), que le contexte baisse, ou que le modèle ou la fenêtre changent.
func compactRefusedSkip(conv string, used int, msgs []Message) bool {
	if conv == "" || used <= 0 || !compactContinuationOn(ReadConfig()) {
		return false
	}
	model, window := engineMainNow()
	compactRefused.mu.Lock()
	r, ok := compactRefused.m[conv]
	compactRefused.mu.Unlock()
	if !ok {
		return false
	}
	if r.model != model || r.window != window || used < r.at || used >= r.at+r.at/10 ||
		len(msgs) < r.n || historyHash(msgs[:r.n]) != r.hash {
		compactRefusedClear(conv)
		return false
	}
	if used*10 >= window*9 {
		return false
	}
	fmt.Fprintf(os.Stderr, "[compact] pas de nouvel essai : refusée à %d jetons, contexte à %d\n", r.at, used)
	return true
}

func compactRefusedNote(conv string, used int, msgs []Message) {
	if conv == "" || used <= 0 {
		return
	}
	model, window := engineMainNow()
	hash := historyHash(msgs)
	compactRefused.mu.Lock()
	defer compactRefused.mu.Unlock()
	if compactRefused.m == nil {
		compactRefused.m = map[string]compactRefusal{}
	}
	if _, ok := compactRefused.m[conv]; !ok && len(compactRefused.m) >= compactRefusedMax {
		for k := range compactRefused.m {
			delete(compactRefused.m, k)
			break
		}
	}
	compactRefused.m[conv] = compactRefusal{at: used, window: window, model: model, n: len(msgs), hash: hash}
}

func compactRefusedClear(conv string) {
	compactRefused.mu.Lock()
	delete(compactRefused.m, conv)
	compactRefused.mu.Unlock()
}

// --- échecs par modèle ------------------------------------------------------

// compactContFails : échecs de la continuation par modèle, jusqu'au
// redémarrage. À compactContMaxFails, elle n'est plus tentée pour ce modèle.
var compactContFails struct {
	mu sync.Mutex
	m  map[string]int
}

const compactContMaxFails = 2

func compactContDisabled() bool {
	model, _ := engineMainNow()
	compactContFails.mu.Lock()
	defer compactContFails.mu.Unlock()
	return compactContFails.m[model] >= compactContMaxFails
}

// compactContFail : hard = refus du gabarit ou appel d'outil, qui se
// reproduiraient à l'identique : suspendue tout de suite.
func compactContFail(hard bool) {
	model, _ := engineMainNow()
	compactContFails.mu.Lock()
	defer compactContFails.mu.Unlock()
	if compactContFails.m == nil {
		compactContFails.m = map[string]int{}
	}
	n := compactContFails.m[model] + 1
	if hard {
		n = compactContMaxFails
	}
	if n >= compactContMaxFails && compactContFails.m[model] < compactContMaxFails {
		fmt.Fprintf(os.Stderr, "[compact] continuation suspendue pour %s jusqu'au redémarrage : résumés sur transcription\n", model)
	}
	compactContFails.m[model] = n
}

func compactContOK() {
	model, _ := engineMainNow()
	compactContFails.mu.Lock()
	delete(compactContFails.m, model)
	compactContFails.mu.Unlock()
}

// --- la requête -------------------------------------------------------------

// compactWireIndex : position dans view.wire du message msgs[i] de la vue
// modèle (len(view.wire) pour i == view.n : toute la vue), -1 si les deux vues
// ne se correspondent pas. L'assemblage n'ajoute ou ne déplace que des
// messages système (prompt, skills, bloc projet, rappel des pages lues) ; les
// autres se suivent un pour un, dans le même ordre — vérifié rôle par rôle,
// appel par appel, plutôt que supposé.
func compactWireIndex(msgs []Message, view *compactView, i int) int {
	if view == nil || view.n > len(msgs) || i < 0 || i > view.n {
		return -1
	}
	var a, b []int
	for j, m := range msgs[:view.n] {
		if m.Role != "system" {
			a = append(a, j)
		}
	}
	for j, m := range view.wire {
		if m.Role != "system" {
			b = append(b, j)
		}
	}
	if len(a) != len(b) {
		return -1
	}
	at := -1
	for j := range a {
		m, w := msgs[a[j]], view.wire[b[j]]
		if m.Role != w.Role || m.ToolCallID != w.ToolCallID || len(m.ToolCalls) != len(w.ToolCalls) {
			return -1
		}
		if a[j] == i {
			at = b[j]
		}
	}
	if i == view.n {
		return len(view.wire)
	}
	return at
}

// clipRunes : s sur une ligne, coupé à n caractères.
func clipRunes(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// compactBoundaryDesc désigne le premier message gardé, m (vue modèle), qui
// est wire[wi] : son rôle, et l'appel d'outil ou le début du texte. Un début
// déjà vu plus haut est allongé, pour rester sans ambiguïté.
func compactBoundaryDesc(wire []Message, wi int, m Message) string {
	if m.Role == "assistant" && strings.TrimSpace(msgText(m)) == "" && len(m.ToolCalls) > 0 {
		f := m.ToolCalls[0].Function
		return fmt.Sprintf("your call to the tool %s(%s)", f.Name, clipRunes(f.Arguments, 120))
	}
	who := "the user message"
	switch m.Role {
	case "assistant":
		who = "your message"
	case "tool":
		who = "the tool result"
	}
	m, _ = withoutCtxUpdate(m)
	text := msgText(m)
	snip := clipRunes(text, 80)
	for _, w := range wire[:wi] {
		if strings.Contains(strings.Join(strings.Fields(msgText(w)), " "), strings.TrimSuffix(snip, "…")) {
			snip = clipRunes(text, 200)
			break
		}
	}
	return fmt.Sprintf("%s that starts with «%s»", who, snip)
}

// compactContPrefix ouvre la demande de résumé : jamais rangée, elle ne vit
// que dans la requête.
const compactContPrefix = "[COMPACTION]"

// compactContInstruction : la demande ajoutée en fin de requête. kept =
// messages gardés tels quels à partir de la frontière (0 : tout est résumé).
func compactContInstruction(kept int, desc string, code bool) string {
	var b strings.Builder
	b.WriteString(compactContPrefix)
	b.WriteString(" Pause the task: do not answer and do not call any tool. The context is full: the older part of this conversation will be replaced by the summary you write now, and your past reasoning will be lost — this summary is the only memory kept of that part.\n")
	if kept > 0 {
		fmt.Fprintf(&b, "Summarize every message BEFORE %s. That message and the ones after it (the last %d) stay verbatim after your summary: mention them briefly at most, but never leave out anything that comes before. Write the STATE OF PROGRESS as of that message, not later.\n", desc, kept)
	} else {
		b.WriteString("Summarize the whole conversation above.\n")
	}
	b.WriteString("Leave out the system instructions and any <project_context> or <context_update> block: they are sent again separately.\n\n")
	b.WriteString(compactSummaryRules)
	if code {
		b.WriteString(codeSummaryRules)
	}
	return b.String()
}

// summarizeContinuation demande le résumé du torse [.., tailStart) en
// prolongeant la requête du tour (view). Toute erreur fait retomber
// l'appelant sur la transcription.
func summarizeContinuation(ctx context.Context, msgs []Message, tailStart int, view *compactView, code bool) (string, error) {
	ep := resolveChatEndpoint()
	if ep.External {
		return "", errors.New("preset externe")
	}
	if tailStart > view.n {
		return "", errors.New("frontière hors de la vue")
	}
	// Relu à l'envoi : une requête partie depuis la construction de la vue
	// aurait pris le slot.
	if !engineSlotHolds(view.conv) {
		return "", errors.New("slot repris entre-temps")
	}
	wi := compactWireIndex(msgs, view, tailStart)
	if wi < 0 {
		return "", errors.New("vue d'envoi désalignée")
	}
	kept, desc := len(view.wire)-wi, ""
	if kept > 0 {
		desc = compactBoundaryDesc(view.wire, wi, msgs[tailStart])
	}
	instr := Message{Role: "user", Content: compactContInstruction(kept, desc, code)}
	// Marge : contexte réel, la demande, le résumé, et 5 % de la fenêtre pour
	// ce que l'estimation ne voit pas.
	if need := view.used + msgTokens(instr) + compactSummaryBudget() + ctxWindow()*5/100; need > ctxWindow() {
		return "", fmt.Errorf("marge insuffisante : %d jetons pour une fenêtre de %d", need, ctxWindow())
	}
	// Arguments du gabarit du tour, réflexion coupée : sur les gabarits
	// hybrides (Qwen3…), enable_thinking ne change que l'ouverture de la
	// réponse ; reasoning_effort, rendu dans le préfixe par gpt-oss, reste
	// celui du tour.
	kwargs := map[string]any{}
	for k, v := range view.kwargs {
		kwargs[k] = v
	}
	kwargs["enable_thinking"] = false
	sent := append(append([]Message(nil), view.wire...), instr)
	// tool_choice « none » : les outils restent rendus dans le prompt (le
	// préfixe en cache tient), aucun appel n'est attendu.
	payload := buildChatPayload(ep, sent, 0.2, chatPayloadOpts{
		tools: view.tools, toolChoiceNone: true, effort: view.effort, kwargs: kwargs, noSampling: true,
	})
	payload["stream"] = false
	delete(payload, "stream_options")
	payload["max_tokens"] = compactSummaryBudget()
	// SIDE_SLOT : elle prolonge le prompt du slot de la discussion, elle y va.
	setEngineSlot(payload, engineSlotFor(ep, true))
	ch, err := postSummary(ctx, ep, payload, perfTag{kind: perfCompact, conv: view.conv})
	if err != nil {
		// Refus de la requête elle-même (gabarit à alternance stricte, champ
		// inconnu…) : il se reproduirait à chaque compaction. Une fenêtre
		// pleine, un moteur en chargement ou une coupure réseau, non.
		var he *summaryHTTPError
		if errors.As(err, &he) && (he.status == 400 || he.status == 422 || he.status == 500) && !contextOverflow(he.body, nil) {
			compactContFail(true)
		}
		return "", err
	}
	if len(ch.Message.ToolCalls) > 0 {
		compactContFail(true)
		return "", errors.New("appel d'outil émis au lieu d'un résumé")
	}
	if textualToolCallSnippet(ch.Message.Content) != "" {
		compactContFail(false)
		return "", errors.New("appel d'outil écrit en texte")
	}
	s, err := cleanSummary(ch.Message.Content, ch.Message.ReasoningContent, ch.FinishReason)
	if err != nil {
		compactContFail(false)
		return "", err
	}
	if summaryLooksEmpty(s) {
		compactContFail(false)
		return "", errors.New("résumé vide")
	}
	compactContOK()
	return s, nil
}
