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

// budgetNudgeEnds : fins des trois rappels de budget (llm_budget.go). Le seul
// préfixe « [system] » ne suffit pas à les reconnaître : un utilisateur peut
// très bien coller un texte qui commence ainsi, et sa demande serait alors
// sautée par la compaction, le vérificateur ou le titre. Le test
// TestIsLokiInjected tient ces fins en phase avec budgetNudge.
var budgetNudgeEnds = []string{
	"unless exactly one specific call is genuinely still missing.",
	"say so in the answer instead of investigating further.",
	"Write your final answer in this message. Do not call another tool.",
}

// isLokiInjected dit si un message `user` vient de loki et non de
// l'utilisateur.
func isLokiInjected(m Message) bool {
	if m.Role != "user" {
		return false
	}
	// Relais d'image d'un outil gardé dans l'historique (KEEP_TURN_IMAGES) :
	// une image montrée par loki, pas une demande. Jamais posé sans la clé.
	if m.ImgRelay {
		return true
	}
	s, ok := m.Content.(string)
	if !ok {
		return false
	}
	if s == thinkNudgeFirst || s == thinkNudgeStuck || s == lokiNotePrefix+toolsOffHint ||
		strings.HasPrefix(s, retryCorrectiveLead) {
		return true
	}
	if !strings.HasPrefix(s, lokiNotePrefix) {
		return false
	}
	for _, end := range budgetNudgeEnds {
		if strings.HasSuffix(s, end) {
			return true
		}
	}
	return false
}

// dropStrayNudges retire de l'historique à persister les rappels de loki
// restés en porte-à-faux : en toute fin (le tour a été arrêté ou a échoué avant
// que le modèle y réponde), ou collés à un autre message `user` (un ajout en
// cours de réponse arrivé juste derrière, ou un rappel éphémère qu'une
// compaction en cours de tour a fait entrer dans l'historique). Gardés, ils
// laisseraient deux `user` d'affilée au tour suivant — exception à chaque tour
// sur les gabarits à alternance stricte (voir appendNudge). Retirés, on
// retrouve l'historique d'avant leur persistance ; le cache ne perd que ce
// qui suivait le rappel, comme avant. Renvoie msgs tel quel s'il n'y a rien à
// retirer, une copie sinon.
func dropStrayNudges(msgs []Message) []Message {
	for {
		drop := -1
		for i, m := range msgs {
			if !isLokiInjected(m) {
				continue
			}
			if i == len(msgs)-1 || msgs[i+1].Role == "user" || (i > 0 && msgs[i-1].Role == "user") {
				drop = i
				break
			}
		}
		if drop < 0 {
			return msgs
		}
		msgs = append(append([]Message(nil), msgs[:drop]...), msgs[drop+1:]...)
	}
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

// nudgeInToolOn : NUDGE_IN_TOOL (opt-in, off par défaut, ZONE GRISE). Sur un
// gabarit dont le rendu d'un tour d'outil change dès qu'un message `user`
// s'ajoute (Qwen3.5 : le nouveau message devient la « dernière question », et
// tous les messages assistant du tour sont rendus autrement), le rappel de
// budget en message à part fait recalculer toute la boucle d'outils du tour.
// Posé au bout du dernier résultat d'outil, il ne coûte que lui-même.
//
// Le prix : une consigne glissée dans une sortie d'outil, que les modèles
// entraînés contre l'injection de prompt peuvent suivre moins bien — d'où
// l'opt-in, à n'activer qu'après avoir comparé l'obéissance aux rappels.
//
// Moteur local seulement, et seulement si la sonde de gabarit
// (chat_tplprobe.go) a conclu « préfixe instable » pour CE modèle et pour la
// forme de CETTE requête (outils, chat_template_kwargs, reasoning_effort : le
// rendu en dépend). « inconnu », un autre modèle ou une forme jamais sondée
// laissent le message à part.
func nudgeInToolOn(cfg map[string]string, ep chatEndpoint, tools []Tool, kwargs map[string]any, effort string) bool {
	switch strings.ToLower(strings.TrimSpace(cfg["NUDGE_IN_TOOL"])) {
	case "on", "1", "true", "yes", "oui":
	default:
		return false
	}
	if ep.External {
		return false
	}
	r, ok := tplCapsFor(newTplShape(tools, kwargs, effort))
	return ok && r.PrefixStable == tplNo && r.Model != "" && r.Model == reasoningEchoModel(cfg)
}

// nudgeIntoTool ajoute le rappel au bout du dernier résultat d'outil, dans la
// vue du modèle ET dans l'historique — le même message, modifié des deux côtés
// à l'identique, pour que le tour suivant rejoue les mêmes octets. Ce résultat
// n'est pas encore parti au moteur : rien de ce qu'il a en cache ne bouge.
// ok=false (dernier message d'une autre forme, historique réécrit entre-temps
// par une compaction) : l'appelant passe par appendNudge. Copies, jamais de
// modification en place.
func nudgeIntoTool(messages, extra []Message, text string) ([]Message, []Message, bool) {
	n, e := len(messages), len(extra)
	if n == 0 || e == 0 {
		return messages, extra, false
	}
	last, kept := messages[n-1], extra[e-1]
	s, ok := last.Content.(string)
	ks, kok := kept.Content.(string)
	if !ok || !kok || s != ks || last.Role != "tool" || kept.Role != "tool" || last.ToolCallID != kept.ToolCallID {
		return messages, extra, false
	}
	last.Content = s + "\n\n" + text
	messages = append(messages[:n-1:n-1], last)
	extra = append(extra[:e-1:e-1], last)
	return messages, extra, true
}
