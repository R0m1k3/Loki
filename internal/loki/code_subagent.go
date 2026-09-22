package loki

// code_subagent.go — l'outil `subagent` du mode code : déléguer une question
// bornée à un RÔLE qui travaille dans SON PROPRE contexte, et n'en ramener que
// la réponse.
//
// Repris de l'idée des sous-agents d'OpenFox (voir NOTICE.md), réécrit pour le
// fil unique de Loki, sur la mécanique déjà en place pour la passe de
// vérification (code_verify.go : un runChat isolé, non persisté).
//
// Pourquoi ça compte sur un modèle LOCAL : la fenêtre de contexte est petite.
// « Trouve où est géré le cache » coûte dix lectures de fichiers qui restent
// ensuite dans l'historique jusqu'à la compaction, alors que seule la RÉPONSE
// comptait. Le sous-agent paie ces lectures dans un contexte jetable et ne rend
// que le résultat : le fil du builder ne grossit que d'un résultat d'outil.
//
// Trois rôles, tous en LECTURE SEULE (voir EnabledTools) : explorer (cartographie
// le code), code-reviewer (relit un changement), planner (découpe un travail).
// Aucun n'écrit : ce qui modifie le dépôt reste dans le fil principal, sous les
// yeux de l'utilisateur.

import (
	"context"
	"strings"
)

// subagentRoles : les rôles délégables. `verifier` n'en fait PAS partie — cette
// passe est pilotée par la boucle du contrat (code_verify.go), c'est la seule
// habilitée à marquer un critère passé, et la laisser s'appeler à la demande
// permettrait au builder de se décerner son propre satisfecit.
var subagentRoles = []string{"explorer", "code-reviewer", "planner"}

func isSubagentRole(r string) bool {
	for _, x := range subagentRoles {
		if x == r {
			return true
		}
	}
	return false
}

// subagentMaxOutput borne ce que le sous-agent ramène dans le contexte de
// l'appelant. Au-delà, l'économie de contexte serait perdue — c'est un rapport,
// pas un vidage de fichiers.
const subagentMaxOutput = 6000

func subagentTool() Tool {
	return Tool{Type: "function", Function: ToolFunction{
		Name: "subagent",
		// Description au plus court : elle part dans CHAQUE requête (budget du
		// préambule, cf. TestSystemPromptStaysLean).
		Description: "Délègue une recherche ou une relecture à un rôle qui travaille dans son propre contexte et ne rend que sa réponse. Lecture seule.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"role": map[string]any{
					"type":        "string",
					"enum":        subagentRoles,
					"description": "explorer (trouver/cartographier), code-reviewer (relire un changement), planner (découper)",
				},
				"task": map[string]any{"type": "string", "description": "La question, précise et autonome : le rôle ne voit pas la discussion."},
			},
			"required": []string{"role", "task"},
		},
	}}
}

// toolSubagent exécute la délégation. Le contexte du sous-agent est ISOLÉ : le
// prompt de son rôle, le briefing machine, et la tâche — rien de l'historique
// du fil, que précisément on ne veut pas payer deux fois. Sa trace n'est ni
// persistée ni diffusée : seul son texte final revient.
func toolSubagent(ctx context.Context, args map[string]any, parent Caps) string {
	role, _ := args["role"].(string)
	task, _ := args["task"].(string)
	role = strings.TrimSpace(role)
	task = strings.TrimSpace(task)
	if !isSubagentRole(role) {
		return "[erreur] rôle inconnu : " + role + " (attendu : " + strings.Join(subagentRoles, ", ") + ")"
	}
	if task == "" {
		return "[erreur] tâche vide — décris ce que le rôle doit chercher ou relire"
	}
	// Garde-fou anti-récursion : un sous-agent ne délègue pas à son tour. Sans
	// ça, un modèle qui boucle ouvre autant de contextes que de tours, et la
	// facture en jetons devient exponentielle pour une question unique.
	if parent.Role != "" && parent.Role != "builder" {
		return "[erreur] un sous-agent ne peut pas en appeler un autre — réponds avec ce que tu as"
	}

	caps := Caps{Agent: true, Code: true, Role: role, Mem: MemOff}
	// Cadrage : le rôle ne voit ni la discussion ni l'utilisateur. Son prompt de
	// rôle parle à un humain (« termine en demandant confirmation ») ; ici son
	// interlocuteur est le builder, et sa réponse est un résultat d'outil.
	framing := "\n\nAnswer with your findings only: what you found and where (file:line). No preamble."
	if role == "planner" {
		framing = "\n\nYou are a delegated pass: nobody will answer you. Record the acceptance criteria with the criteria tool, then give the ordered steps. Do not ask for confirmation."
	}
	msgs := []Message{
		{Role: "system", Content: rolePrompt(role) + "\n\n" + machineSystemPrompt(caps)},
		{Role: "user", Content: task + framing},
	}
	var out strings.Builder
	// Trace jetable : aucun forwardStream, rien de persisté. L'utilisateur voit
	// la bulle de l'outil `subagent` et son rapport, pas les dix lectures.
	if _, err := runChat(ctx, msgs, 0, caps, func(ev StreamEvent) bool {
		if ev.Content != "" {
			out.WriteString(ev.Content)
		}
		return true
	}); err != nil {
		return "[erreur] sous-agent " + role + " : " + err.Error()
	}
	res := strings.TrimSpace(out.String())
	if res == "" {
		return "[erreur] le sous-agent " + role + " n'a rien répondu"
	}
	if r := []rune(res); len(r) > subagentMaxOutput {
		res = string(r[:subagentMaxOutput]) + "\n…[tronqué]"
	}
	return "[" + role + "]\n" + res
}
