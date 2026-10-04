package loki

// llm_reasoning_echo.go — REASONING_ECHO : renvoyer au moteur LOCAL le
// raisonnement que le modèle a lui-même produit (reasoning_content), au lieu de
// lui montrer ses étapes passées avec des blocs de réflexion vides.
//
// Pourquoi c'est utile. Les gabarits Qwen3.5/3.6 (et d'autres) rendent
// `<think>…</think>` pour chaque message assistant situé après la dernière
// question de l'utilisateur : c'est le format entrelacé sur lequel ils ont été
// entraînés. Loki ne gardait que le texte et les appels d'outils, donc à chaque
// étape d'une boucle d'outils le modèle relisait ses étapes précédentes sans leur
// raisonnement, et le moteur recalculait le dernier message assistant (le rendu
// ne correspondait plus à ce qu'il avait généré). Avec un gabarit qui sait
// conserver la réflexion des tours passés (REASONING_PRESERVE=on, Qwen3.6), le
// préfixe reste en plus stable d'un message utilisateur à l'autre. Qwen3.5 n'a
// pas ce réglage : le gain y reste interne au tour.
//
// Pourquoi c'est OPT-IN (off par défaut). Le raisonnement renvoyé occupe du
// contexte : une longue boucle d'outils atteint la compaction (avec perte) plus
// tôt, même sans REASONING_PRESERVE. Et certains gabarits le refusent (gpt-oss
// sur un moteur ancien lève « Cannot pass both content and thinking »). Sans la
// clé, rien ne change : rien n'est capturé, rien n'est envoyé, à l'octet près.
//
// Les règles, chacune contre un piège précis :
//   - moteur local seulement : une API externe (OpenAI, DeepSeek) refuserait ou
//     interpréterait le champ à sa façon ;
//   - seulement le raisonnement que le SERVEUR a séparé (reasoning_content du
//     flux). Le découpage « </think> » fait chez nous reste dans le contenu, où
//     le gabarit le redécoupe lui-même : poser aussi ReasoningContent le ferait
//     rendre deux fois ;
//   - chaque raisonnement porte le nom du modèle qui l'a produit, et ne repart
//     que vers ce modèle-là : après un changement de modèle, un gabarit qui rend
//     les anciens raisonnements (DeepSeek-V4 avec outils, MiniMax) présenterait
//     sinon la réflexion d'une autre famille comme la sienne ;
//   - jamais de message assistant qui ne porterait que du raisonnement (llama.cpp
//     répond « Expected 'content' or 'tool_calls' ») ;
//   - un seul point de sortie (echoMessages, appelé par runChat juste avant le
//     marshal) : c'est là que tout est retiré quand la règle ne s'applique pas,
//     y compris l'étiquette du modèle, qui ne quitte jamais Loki ;
//   - si le gabarit refuse (exception), le tour est rejoué UNE fois sans
//     raisonnement, et le refus est retenu pour ce modèle jusqu'au redémarrage,
//     avant toute coupure d'outils ;
//   - si le prompt déborde, on retente d'abord sans raisonnement — exactement la
//     requête d'avant cette clé — avant toute compaction.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// reasoningEchoEnabled : la clé REASONING_ECHO demande-t-elle l'écho ?
func reasoningEchoEnabled(cfg map[string]string) bool {
	switch strings.ToLower(strings.TrimSpace(cfg["REASONING_ECHO"])) {
	case "on", "1", "true", "yes", "oui":
		return true
	}
	return false
}

// reasoningEchoModel : l'étiquette du modèle chargé, posée sur chaque
// raisonnement capturé. Le nom du fichier, pas le chemin : le même modèle
// déplacé ou monté ailleurs reste le même. Vide = inconnu, rien ne part.
func reasoningEchoModel(cfg map[string]string) string {
	m := strings.TrimSpace(cfg["MODEL"])
	if m == "" {
		return ""
	}
	return filepath.Base(m)
}

// echoRefused : modèles dont le gabarit a refusé un raisonnement renvoyé, pour
// toute la vie du processus (même principe que effortFallbacks).
var echoRefused sync.Map

func echoRefuse(model string) {
	if model == "" {
		return
	}
	if _, loaded := echoRefused.LoadOrStore(model, true); !loaded {
		fmt.Fprintf(os.Stderr, "[reasoning] gabarit de %s : raisonnement renvoyé refusé — REASONING_ECHO suspendu pour ce modèle jusqu'au redémarrage\n", model)
	}
}

func echoIsRefused(model string) bool {
	_, ok := echoRefused.Load(model)
	return ok
}

// echoPolicy : ce qui part au moteur pour cette requête.
//   - on : renvoyer le raisonnement (clé posée, moteur local, modèle connu, pas
//     refusé par le gabarit, et la sonde ne dit pas qu'il n'est jamais rendu) ;
//   - model : seul le raisonnement étiqueté de ce modèle repart ;
//   - keepsPast : le gabarit rend aussi le raisonnement des messages d'AVANT la
//     dernière question (sonde « oui » ou inconnue). Ne sert qu'au comptage :
//     un raisonnement que le gabarit écarte ne coûte rien au prompt.
type echoPolicy struct {
	on        bool
	model     string
	keepsPast bool
}

// currentEchoPolicy lit la configuration, la mémoire des refus et le dernier
// verdict de la sonde de gabarit (chat_tplprobe.go).
func currentEchoPolicy() echoPolicy {
	return echoPolicyFor(ReadConfig())
}

func echoPolicyFor(cfg map[string]string) echoPolicy {
	p := echoPolicy{model: reasoningEchoModel(cfg), keepsPast: true}
	if !reasoningEchoEnabled(cfg) || isExternalConfig(cfg) || p.model == "" || echoIsRefused(p.model) {
		return echoPolicy{}
	}
	p.on = true
	// Verdict de la sonde, s'il concerne bien CE modèle. « jamais rendu » :
	// renvoyer ne servirait à rien, on s'abstient. Inconnu : l'utilisateur a
	// demandé l'écho, on envoie (le filet d'erreur de gabarit reste là).
	if r, ok := tplCapsCurrent(); ok && r.Model == p.model {
		if r.RendersReasoning == tplNo {
			p.on = false
		}
		if r.PreservesHistory == tplNo {
			p.keepsPast = false
		}
	}
	return p
}

// hasBody : le message assistant porte du texte ou des appels d'outils — ce
// sans quoi llama.cpp refuse un message assistant.
func hasBody(m Message) bool {
	if len(m.ToolCalls) > 0 {
		return true
	}
	switch v := m.Content.(type) {
	case nil:
		return false
	case string:
		return v != ""
	}
	return true
}

// echoable : ce raisonnement-là peut-il partir sous cette politique ?
func echoable(m Message, p echoPolicy) bool {
	return p.on && m.Role == "assistant" && m.ReasoningContent != "" &&
		m.ReasoningModel == p.model && hasBody(m)
}

// echoMessages : LE point de sortie. Rend les messages tels qu'ils partent au
// moteur, et le nombre de raisonnements renvoyés. Aucun message ne porte de
// raisonnement (cas de toujours, clé absente) : la tranche est rendue telle
// quelle, sans copie — la requête est identique à l'octet près. Sinon, copie :
// l'étiquette du modèle est toujours retirée, le raisonnement l'est quand il
// ne doit pas partir.
func echoMessages(msgs []Message, p echoPolicy) ([]Message, int) {
	has := false
	for i := range msgs {
		if msgs[i].ReasoningContent != "" || msgs[i].ReasoningModel != "" {
			has = true
			break
		}
	}
	if !has {
		return msgs, 0
	}
	out := make([]Message, len(msgs))
	n := 0
	for i, m := range msgs {
		keep := echoable(m, p)
		m.ReasoningModel = ""
		if keep {
			n++
		} else {
			m.ReasoningContent = ""
		}
		out[i] = m
	}
	return out, n
}

// echoTokens : jetons que le raisonnement renvoyé ajoute au prompt, message par
// message (nil = aucun). Compte ce qui part ET que le gabarit rend : après la
// dernière question toujours (rendu au sein du tour par tous les gabarits
// entrelacés), avant elle seulement si le gabarit conserve le passé ou qu'on ne
// le sait pas — dans le doute on compte, le sens sans danger.
func echoTokens(msgs []Message) []int {
	found := false
	for i := range msgs {
		if msgs[i].ReasoningContent != "" {
			found = true
			break
		}
	}
	if !found {
		return nil // cas de toujours : pas même une lecture de configuration
	}
	return echoTokensFor(msgs, currentEchoPolicy())
}

func echoTokensFor(msgs []Message, p echoPolicy) []int {
	if !p.on {
		return nil
	}
	lastUser := -1
	for i, m := range msgs {
		if m.Role == "user" {
			lastUser = i
		}
	}
	var out []int
	for i, m := range msgs {
		if !echoable(m, p) || (i < lastUser && !p.keepsPast) {
			continue
		}
		if out == nil {
			out = make([]int, len(msgs))
		}
		out[i] = len(m.ReasoningContent) / 4
	}
	return out
}

// stripReasoning : copie des messages sans aucun raisonnement (étiquette
// comprise) — ce que Loki envoyait avant REASONING_ECHO.
func stripReasoning(msgs []Message) []Message {
	out, _ := echoMessages(msgs, echoPolicy{})
	return out
}

// echoTemplateError : le moteur a-t-il refusé la requête à cause du gabarit,
// d'une façon que le raisonnement renvoyé peut expliquer ? Exception levée par
// le gabarit (raise_exception, « Cannot pass both content and thinking » de
// gpt-oss), message assistant jugé invalide, messages illisibles. Le refus du
// niveau de raisonnement, lui aussi levé par le gabarit, a son propre filet
// (llm_effort.go) et n'est pas concerné.
func echoTemplateError(status int, body string) bool {
	if status != 500 && status != 400 {
		return false
	}
	if _, isEffort := effortRejection(body); isEffort {
		return false
	}
	low := strings.ToLower(body)
	// Appel d'outil mal formé (« Failed to parse input at pos N: … ») : le
	// texte du modèle y est cité, il peut dire « thinking » sans que le gabarit
	// y soit pour rien. Le filet des outils s'en charge, pas celui-ci — sinon
	// une relance chanceuse ferait couper la clé pour ce modèle à tort.
	if strings.Contains(low, "failed to parse input") {
		return false
	}
	for _, k := range []string{"raise_exception", "expected 'content' or 'tool_calls'", "failed to parse messages"} {
		if strings.Contains(low, k) {
			return true
		}
	}
	// « thinking » seul ne dit rien : seulement dans une erreur du gabarit.
	return strings.Contains(low, "thinking") && (strings.Contains(low, "jinja") || strings.Contains(low, "template"))
}

// ReasoningEcho : raisonnement séparé de la complétion qui porte la réponse
// finale, et le modèle qui l'a produit. L'appelant le range avec cette réponse.
type ReasoningEcho struct {
	Text  string
	Model string
}

// withEcho pose le raisonnement final sur le message de réponse.
func withEcho(m Message, e *ReasoningEcho) Message {
	if e != nil && e.Text != "" && hasBody(m) {
		m.ReasoningContent, m.ReasoningModel = e.Text, e.Model
	}
	return m
}
