package loki

// llm_injected.go — messages que LOKI glisse dans le fil (rappels, relances),
// et leur place dans l'historique.
//
// Ces rappels partaient autrefois dans la vue du modèle SANS entrer dans
// l'historique persistant : au tour suivant, llama-server recevait un fil où ils
// avaient disparu, le préfixe divergeait juste avant eux et tout ce qui suivait
// (souvent une longue boucle d'outils) était recalculé. Le modèle relisait en
// prime une histoire qu'il n'avait jamais vue : des résultats d'outils suivis
// d'une réponse finale, sans la consigne qui l'avait provoquée.
//
// Persistés tels qu'envoyés, ils ont un rôle `user` sans être des demandes de
// l'utilisateur : tout ce qui lit « le dernier message user » comme la tâche en
// cours (compaction, vérificateur, export, titre) doit donc les reconnaître et
// les sauter — c'est isLokiInjected.

import "strings"

// lokiNotePrefix ouvre les rappels de loki (budget d'outils, consigne de la
// relance après un 500) : une marque stable, lisible par le modèle comme une
// note du système et non comme la parole de l'utilisateur.
const lokiNotePrefix = "[system] "

// Relances « pensé sans agir » (voir runChat). Gardées en constantes : leur
// texte exact sert aussi à les reconnaître dans l'historique.
const (
	thinkNudgeFirst = "You reasoned but did not call a tool or answer. Act NOW: call the appropriate tool directly (e.g. mem_search/mem_read/bash), or give your final answer if you already have the info. Don't explain, act."
	thinkNudgeStuck = "You are stuck re-describing the same plan without executing it. Stop reasoning. In your NEXT message, either call ONE tool right now, or write your final answer in plain text using only what you already know — no more planning, no more thinking, act or answer this instant."
)

// retryCorrectiveLead ouvre la consigne de relance « appel écrit en texte »
// (code_retry.go), déjà persistée avant ce fichier.
const retryCorrectiveLead = "Your last answer contained a TOOL CALL WRITTEN AS TEXT — it was never executed:\n"

// toolsOffHint : consigne de la relance après un 500 (appel d'outil que
// llama.cpp n'a pas su parser). Jamais persistée : elle ne vaut que pour la
// relance qui suit.
const toolsOffHint = "Do not call any more tools. Answer now, directly, in the user's language, using only the information already gathered."

// isLokiInjected dit si un message `user` vient de loki et non de
// l'utilisateur.
func isLokiInjected(m Message) bool {
	if m.Role != "user" {
		return false
	}
	s, ok := m.Content.(string)
	if !ok {
		return false
	}
	return strings.HasPrefix(s, lokiNotePrefix) || s == thinkNudgeFirst || s == thinkNudgeStuck ||
		strings.HasPrefix(s, retryCorrectiveLead)
}

// appendNudge ajoute un rappel de loki à la vue du modèle et, quand c'est sans
// risque, à l'historique persistant — exactement tel qu'envoyé, pour que le
// tour suivant rejoue les mêmes octets et retrouve son préfixe en cache.
//
// Sauf s'il suit directement un autre message `user` (le message de
// l'utilisateur à l'étape 0, un autre rappel, un ajout en cours de réponse) :
// les gabarits qui exigent l'alternance stricte user/assistant (Gemma,
// Mistral) lèvent une exception sur deux `user` consécutifs. Éphémère, cette
// paire ne coûte qu'une étape ; persistée, elle ferait échouer chaque tour
// suivant de la conversation. Le rappel reste alors éphémère, comme avant.
func appendNudge(messages, extra []Message, text string) ([]Message, []Message) {
	m := Message{Role: "user", Content: text}
	afterUser := len(messages) > 0 && messages[len(messages)-1].Role == "user"
	messages = append(messages, m)
	if !afterUser {
		extra = append(extra, m)
	}
	return messages, extra
}

// withTrailingHint renvoie une COPIE de msgs dont le dernier message porte en
// plus la consigne hint, à la fin. Sert à la relance après un 500 : poser la
// consigne en tête (steerSystem) modifiait le message système, donc tout le
// prompt était à recalculer ; à la fin, le préfixe déjà en cache reste valide.
//
// Ajoutée au contenu du dernier message (résultat d'outil ou message user)
// plutôt qu'en nouveau message : deux `user` d'affilée cassent les gabarits
// stricts, et sur Qwen3.5 un nouveau message user devient la « dernière
// question » et fait re-rendre toute la boucle du tour. Les messages ne sont
// jamais modifiés en place : ils appartiennent à l'historique.
func withTrailingHint(msgs []Message, hint string) []Message {
	note := "\n\n" + lokiNotePrefix + hint
	out := append([]Message(nil), msgs...)
	if n := len(out); n > 0 && (out[n-1].Role == "tool" || out[n-1].Role == "user") {
		last := out[n-1]
		switch c := last.Content.(type) {
		case string:
			last.Content = c + note
			out[n-1] = last
			return out
		case []any:
			last.Content = append(append([]any(nil), c...), map[string]any{"type": "text", "text": note})
			out[n-1] = last
			return out
		case []map[string]any:
			last.Content = append(append([]map[string]any(nil), c...), map[string]any{"type": "text", "text": note})
			out[n-1] = last
			return out
		}
	}
	// Dernier message d'une autre forme (cas théorique) : un message à part.
	return append(out, Message{Role: "user", Content: lokiNotePrefix + hint})
}
