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

// codeVerifyLoop est appelé par generate() à la FIN d'un tour de build en mode
// code. Il ne fait rien s'il n'y a pas de contrat (aucun critère).
func (c *Conversation) codeVerifyLoop(ctx context.Context, caps Caps, temperature float64, epoch int) {
	if !caps.Code || caps.Role == "verifier" || ctx.Err() != nil {
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
		c.runFixTurn(ctx, caps, temperature, epoch, list)
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
	// meurt dans cette passe. Seuls les deltas d'affichage sortent.
	_, _ = runChat(ctx, msgs, temperature, vcaps, func(ev StreamEvent) bool {
		c.forwardStream(ev, epoch)
		return true
	})
}

// runFixTurn relance le BUILDER sur l'historique normal avec la liste des
// critères en échec. Sa trace est persistée comme un tour ordinaire.
func (c *Conversation) runFixTurn(ctx context.Context, caps Caps, temperature float64, epoch int, list []Criterion) {
	var failed []Criterion
	for _, cr := range list {
		if cr.Status != "passed" {
			failed = append(failed, cr)
		}
	}
	c.appendDelta(epoch, map[string]any{"role": "builder"})
	defer c.appendDelta(epoch, map[string]any{"role": ""})

	fixMsg := Message{Role: "user", Content: "Verification failed on these criteria:\n" + critRender(failed) +
		"\n\nFix the code so they pass. Address the notes precisely; do not touch what already passed."}

	c.mu.Lock()
	if c.epoch != epoch {
		c.mu.Unlock()
		return
	}
	c.Messages = append(c.Messages, fixMsg)
	msgs := append([]Message(nil), c.Messages...)
	c.mu.Unlock()

	final := msgs
	if sp := readSysPrompt(); sp != "" {
		final = append([]Message{{Role: "system", Content: sp}}, msgs...)
	}
	var content strings.Builder
	extra, _ := runChat(ctx, InjectSkills(final, caps), temperature, caps, func(ev StreamEvent) bool {
		if ev.Content != "" {
			content.WriteString(ev.Content)
		}
		c.forwardStream(ev, epoch)
		return true
	})
	c.mu.Lock()
	if c.epoch == epoch {
		c.Messages = append(c.Messages, extra...)
		if s := content.String(); strings.TrimSpace(s) != "" {
			c.Messages = append(c.Messages, Message{Role: "assistant", Content: s})
		}
	}
	c.mu.Unlock()
	c.persist()
}

// forwardStream relaie les événements d'un runChat secondaire (vérification,
// correction) vers le journal d'affichage — même mapping que generate(), sans
// la gestion de compaction (ces passes n'en déclenchent pas : contexte court).
func (c *Conversation) forwardStream(ev StreamEvent, epoch int) {
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
		}
		if ev.ToolUsed.Image != "" {
			tu["image"] = ev.ToolUsed.Image
		}
		c.appendDelta(epoch, map[string]any{"tool_used": tu})
	case ev.Ask != nil:
		c.appendDelta(epoch, map[string]any{"ask": ev.Ask})
	case ev.Stats != nil:
		if ev.Stats.PromptTokensTotal > 0 {
			c.mu.Lock()
			if c.epoch == epoch {
				c.CtxUsed = ev.Stats.PromptTokensTotal + ev.Stats.GenTokens
			}
			c.mu.Unlock()
		}
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
