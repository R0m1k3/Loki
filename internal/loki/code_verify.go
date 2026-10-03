package loki

// code_verify.go — la boucle du contrat en mode code : build → VÉRIFICATION →
// correction → re-vérification, jusqu'à ce que tous les critères passent ou
// que le plafond de cycles soit atteint. Reprise de la boucle
// builder/verifier d'OpenFox (voir NOTICE.md), adaptée au fil unique de Loki :
//
//   - la passe de vérification tourne sur un CONTEXTE ISOLÉ (prompt du rôle +
//     critères + tâche), pas sur l'historique complet — même modèle, mais il
//     n'a pas « écrit » le code sous les yeux, et surtout ça ne gonfle pas le
//     contexte de la discussion ;
//   - seule cette passe a le droit de marquer un critère `passed`
//     (toolCriteria, allowPass) ;
//   - les corrections, elles, repartent dans l'historique NORMAL du fil : le
//     builder doit se souvenir de ce qu'il a fait.

import (
	"context"
	"fmt"
	"strings"
)

// codeMaxFixTurns : nombre de tours de CORRECTION après échec de vérification.
// Au-delà, on s'arrête et on dit ce qui manque — un modèle qui n'y arrive pas
// en 2 corrections tourne en rond, l'utilisateur doit reprendre la main.
const codeMaxFixTurns = 2

// codeMaxContinue : relances du builder qui a rendu la main avec des critères
// encore ouverts (« ensuite je vais… ») avant de vérifier quand même. Reprise
// du buildAgentNudge d'OpenFox : une passe de vérification sur un travail
// inachevé, c'est un prefill complet, le cache KV du fil évincé, et des
// « failed » qui ne disent que « pas encore fait ».
const codeMaxContinue = 2

// codeVerifyLoop est appelé par generate() à la FIN d'un tour de build en mode
// code. Il ne fait rien s'il n'y a pas de contrat (aucun critère), ni si le
// tour s'est terminé sur une question à l'utilisateur (asked) : sa réponse
// passe avant toute vérification.
func (c *Conversation) codeVerifyLoop(ctx context.Context, caps Caps, temperature float64, epoch int, asked bool) {
	if !caps.Code || caps.Role == "verifier" || ctx.Err() != nil || asked {
		return
	}
	convID := convEnsureActive()
	list := critList(convID)
	if len(list) == 0 || critAllPassed(list) {
		return
	}
	// En phase de PLAN, on ne vérifie pas : rien n'a été construit.
	if caps.Role == "planner" {
		return
	}
	// Le builder vérifie lui-même puis marque « completed » ce qu'il estime
	// fait. Tant qu'il reste des critères ouverts, on le relance plutôt que de
	// vérifier un travail inachevé.
	for n := 0; n < codeMaxContinue; n++ {
		open := critOpen(list)
		if len(open) == 0 {
			break
		}
		msg := "Not done yet — these acceptance criteria are still open:\n" + critRender(open) +
			"\n\nContinue working on them. Once one is done and checked, mark it completed (criteria action=set, status=completed). If you need the user's decision, use ask."
		if c.runBuilderTurn(ctx, caps, temperature, epoch, msg) || ctx.Err() != nil {
			return // question posée à l'utilisateur, ou arrêt
		}
		list = critList(convID)
		if critAllPassed(list) {
			return
		}
	}
	for fix := 0; ; fix++ {
		if ctx.Err() != nil {
			return
		}
		c.runVerifyPass(ctx, caps, temperature, epoch)
		list = critList(convID)
		if critAllPassed(list) {
			c.appendDelta(epoch, map[string]any{"verify_done": "passed"})
			return
		}
		if fix >= codeMaxFixTurns {
			c.appendDelta(epoch, map[string]any{"verify_done": "gave_up"})
			// Le fil doit dire où on en est — sinon l'utilisateur voit une
			// vérification échouée et aucun mot dessus.
			c.appendDelta(epoch, map[string]any{"content": "\n\n_(vérification : " +
				fmt.Sprint(critPending(list)) + " critère(s) non satisfaits après " +
				fmt.Sprint(codeMaxFixTurns) + " corrections — voir le panneau des critères)_"})
			return
		}
		if c.runFixTurn(ctx, caps, temperature, epoch, list) {
			return // le builder attend une réponse de l'utilisateur
		}
	}
}

// runVerifyPass lance la passe de vérification sur un contexte ISOLÉ.
func (c *Conversation) runVerifyPass(ctx context.Context, caps Caps, temperature float64, epoch int) {
	vcaps := caps
	vcaps.Role = "verifier"
	// Marqueur de rôle pour l'UI : les bulles qui suivent portent le badge.
	c.appendDelta(epoch, map[string]any{"role": "verifier"})
	defer c.appendDelta(epoch, map[string]any{"role": ""})

	convID := convEnsureActive()
	list := critList(convID)
	task := c.lastUserText()
	var user strings.Builder
	user.WriteString("Task:\n" + task + "\n\nAcceptance criteria:\n" + critRender(list))
	user.WriteString("\n\nVerify each non-passed criterion now (git_diff first), and record every verdict with the criteria tool.")

	msgs := []Message{
		{Role: "system", Content: rolePrompt("verifier") + "\n\n" + machineSystemPrompt(vcaps)},
		{Role: "user", Content: user.String()},
	}
	// Trace ISOLÉE : ni persistée dans c.Messages, ni compactée — elle vit et
	// meurt dans cette passe. Seuls les deltas d'affichage sortent. Son état
	// dans le moteur aussi : effacé à la fin de la passe (llm_slots.go).
	defer engineSideJob()()
	_, _ = runChat(withPerfKind(ctx, perfVerify), msgs, temperature, vcaps, func(ev StreamEvent) bool {
		c.forwardStream(ev, epoch, true)
		return true
	})
}

// runFixTurn relance le BUILDER sur l'historique normal avec la liste des
// critères en échec. Sa trace est persistée comme un tour ordinaire. Renvoie
// true si le builder a posé une question à l'utilisateur (ask).
func (c *Conversation) runFixTurn(ctx context.Context, caps Caps, temperature float64, epoch int, list []Criterion) bool {
	var failed []Criterion
	for _, cr := range list {
		if cr.Status != "passed" {
			failed = append(failed, cr)
		}
	}
	return c.runBuilderTurn(ctx, caps, temperature, epoch, "Verification failed on these criteria:\n"+critRender(failed)+
		"\n\nFix the code so they pass. Address the notes precisely; do not touch what already passed. Mark each fixed criterion completed again.")
}

// runBuilderTurn relance le BUILDER sur l'historique normal avec une consigne
// (correction après vérification, ou relance sur des critères ouverts). Sa
// trace est persistée comme un tour ordinaire. Renvoie true si le builder a
// posé une question à l'utilisateur (ask) : la boucle doit alors s'arrêter.
func (c *Conversation) runBuilderTurn(ctx context.Context, caps Caps, temperature float64, epoch int, instruction string) (asked bool) {
	c.appendDelta(epoch, map[string]any{"role": "builder"})
	defer c.appendDelta(epoch, map[string]any{"role": ""})

	fixMsg := Message{Role: "user", Content: instruction}

	c.mu.Lock()
	if c.epoch != epoch {
		c.mu.Unlock()
		return false
	}
	c.Messages = append(c.Messages, fixMsg)
	msgs := append([]Message(nil), c.Messages...)
	c.mu.Unlock()

	// Même contexte que le tour de build : projet (description, index mémoire,
	// trackers) et consignes du dépôt. Sans eux, la reprise changeait le début
	// du premier message utilisateur — tout le cache de prompt à refaire.
	final := append(projectSystemMessages(), msgs...)
	if m, ok := codeInstructionsMessage(caps); ok {
		final = append([]Message{m}, final...)
	}
	if sp := readSysPrompt(); sp != "" {
		final = append([]Message{{Role: "system", Content: sp}}, final...)
	}
	var content strings.Builder
	sawUsage := false
	extra, _ := runChat(ctx, InjectSkills(final, caps), temperature, caps, func(ev StreamEvent) bool {
		if ev.Stats != nil && ev.Stats.PromptTokensTotal > 0 {
			sawUsage = true
		}
		if ev.Content != "" {
			content.WriteString(ev.Content)
		}
		if ev.ToolUsed != nil {
			content.Reset() // déjà dans le message tool_calls (voir generate)
		}
		if ev.Ask != nil {
			asked = true
		}
		c.forwardStream(ev, epoch, false)
		return true
	})
	c.mu.Lock()
	if c.epoch == epoch {
		c.Messages = append(c.Messages, extra...)
		if s := content.String(); strings.TrimSpace(s) != "" {
			c.Messages = append(c.Messages, Message{Role: "assistant", Content: s})
		}
		if sawUsage {
			c.ctxUsedLen = len(c.Messages) // même règle que generate
		}
	}
	c.mu.Unlock()
	c.persist()
	return asked
}

// forwardStream relaie les événements d'un runChat secondaire (vérification,
// correction) vers le journal d'affichage — même mapping que generate(), sans
// la gestion de compaction (ces passes n'en déclenchent pas : contexte court).
//
// isolated : la passe tourne sur sa propre trace (vérificateur), PAS sur
// l'historique de la discussion. Ses comptes ne disent rien du contexte de
// celle-ci : ils écrasaient CtxUsed avec la petite taille du vérificateur, et
// le contrôle de fin de tour — puis celui du tour suivant — décidaient sur ce
// chiffre-là. La jauge de l'UI ne doit pas bouger non plus (ctx_isolated).
func (c *Conversation) forwardStream(ev StreamEvent, epoch int, isolated bool) {
	switch {
	case ev.Err != nil:
		c.appendDelta(epoch, map[string]any{"error": ev.Err.Error()})
	case ev.ToolUsed != nil:
		tu := map[string]any{
			"name": ev.ToolUsed.Name, "label": ev.ToolUsed.Label,
			"result": ev.ToolUsed.Result, "done": ev.ToolUsed.Done, "typing": ev.ToolUsed.Typing,
		}
		if ev.ToolUsed.Body != "" {
			tu["body"] = ev.ToolUsed.Body
		}
		if len(ev.ToolUsed.Diff) > 0 {
			tu["diff"] = ev.ToolUsed.Diff
			tu["added"] = ev.ToolUsed.Added
			tu["removed"] = ev.ToolUsed.Removed
		}
		if ev.ToolUsed.Image != "" {
			tu["image"] = ev.ToolUsed.Image
		}
		if ev.ToolUsed.ResultChars > 0 {
			tu["result_chars"] = ev.ToolUsed.ResultChars
		}
		if ev.ToolUsed.ResultID != "" {
			tu["result_id"] = ev.ToolUsed.ResultID
		}
		c.appendDelta(epoch, map[string]any{"tool_used": tu})
	case ev.Ask != nil:
		c.appendDelta(epoch, map[string]any{"ask": ev.Ask})
	case ev.Stats != nil:
		if isolated {
			c.appendDelta(epoch, map[string]any{"stats": ev.Stats, "ctx_isolated": true})
			break
		}
		local := !externalActive()
		c.mu.Lock()
		if c.epoch == epoch {
			if ev.Stats.PromptTokensTotal > 0 {
				c.CtxUsed = ev.Stats.ctxAfter()
			}
			if local {
				c.genPeak = max(c.genPeak, ev.Stats.GenTokens)
			}
		}
		c.mu.Unlock()
		c.appendDelta(epoch, map[string]any{"stats": ev.Stats})
	case ev.DropReasoning:
		c.appendDelta(epoch, map[string]any{"drop_reasoning": true})
	case ev.Reasoning != "":
		c.appendDelta(epoch, map[string]any{"reasoning_content": ev.Reasoning})
	case ev.Content != "":
		c.appendDelta(epoch, map[string]any{"content": ev.Content})
	}
}

// lastUserText retrouve le dernier message utilisateur du fil (la tâche).
func (c *Conversation) lastUserText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.Messages) - 1; i >= 0; i-- {
		if c.Messages[i].Role != "user" {
			continue
		}
		if s, ok := c.Messages[i].Content.(string); ok {
			return s
		}
	}
	return "(tâche non retrouvée — vérifie d'après le diff)"
}
