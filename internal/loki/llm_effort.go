package loki

// llm_effort.go — repli quand le gabarit du modèle REFUSE la valeur de
// `reasoning_effort`.
//
// L'hypothèse d'origine (backend_models.go) était : un gabarit jinja qui ne lit
// pas `reasoning_effort` l'ignore sans erreur, donc rien à prévoir côté loki.
// Elle tombe dès qu'un gabarit VALIDE la valeur au lieu de la subir. Vu en
// production sur Qwen3.8-27B :
//
//	{"error":{"code":500,"message":"... raise_exception('Unexpected reasoning
//	effort ' ~ reasoning_effort) ... Unexpected reasoning effort high. Supported
//	types are xhigh (default), medium, and low."}}
//
// Ce gabarit connaît xhigh/medium/low mais PAS « high » — le niveau que
// l'interface propose et enregistre par défaut. Résultat : chaque message part
// en 500, le tour meurt, et le message d'erreur affiché est une trace jinja.
// Pire, le 500 arrivait dans la branche « prompt trop long » de runChat, donc
// loki compactait l'historique pour rien avant d'abandonner.
//
// D'où ce fichier : reconnaître ce refus, traduire le niveau demandé vers celui
// que le gabarit accepte (le plus proche sur l'échelle), rejouer le tour SANS
// toucher à l'historique, et retenir la traduction pour ce modèle afin de ne
// pas repayer l'aller-retour à chaque message.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// effortLadder : les niveaux connus, du plus faible au plus fort. Sert à choisir
// un remplaçant PROCHE quand le gabarit refuse la valeur demandée — « high »
// devient « xhigh » (voisin immédiat) plutôt que « low ».
//
// La liste vaut pour tous les gabarits croisés jusqu'ici : gpt-oss lit
// low/medium/high, Qwen3.8 lit low/medium/xhigh, certains ajoutent « minimal ».
// Une valeur hors échelle n'est pas classable : on retire alors le champ plutôt
// que d'inventer un niveau.
var effortLadder = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

// effortRank renvoie la position d'un niveau sur l'échelle, ou -1 s'il est
// inconnu.
func effortRank(v string) int {
	v = strings.ToLower(strings.TrimSpace(v))
	for i, s := range effortLadder {
		if s == v {
			return i
		}
	}
	return -1
}

// effortWords capte les niveaux cités dans un message d'erreur. Les bornes de
// mot évitent de lire « high » à l'intérieur de « xhigh ».
var effortWords = regexp.MustCompile(`\b(none|minimal|low|medium|high|xhigh)\b`)

// effortRejection dit si le corps d'erreur renvoyé par llama-server est un refus
// de la valeur de `reasoning_effort` par le gabarit, et renvoie les niveaux que
// ce gabarit déclare accepter (éventuellement vide : tous ne les listent pas).
//
// La détection reste volontairement large — le texte vient du gabarit du modèle,
// pas de llama.cpp, donc sa formulation change d'un modèle à l'autre. Le seul
// invariant : il parle de reasoning effort et dit que la valeur ne va pas.
func effortRejection(body string) (supported []string, ok bool) {
	low := strings.ToLower(body)
	if !strings.Contains(low, "reasoning effort") && !strings.Contains(low, "reasoning_effort") {
		return nil, false
	}
	refus := false
	for _, w := range []string{"unexpected", "unsupported", "not supported", "invalid", "unknown"} {
		if strings.Contains(low, w) {
			refus = true
			break
		}
	}
	if !refus {
		return nil, false
	}
	// Les niveaux acceptés sont annoncés APRÈS « supported » (« Supported types
	// are xhigh (default), medium, and low. »). Ne lire que cette fin de message
	// évite de prendre pour une liste le niveau refusé, cité juste avant.
	if i := strings.LastIndex(low, "supported"); i >= 0 {
		for _, m := range effortWords.FindAllString(low[i:], -1) {
			if effortRank(m) >= 0 && !slicesHas(supported, m) {
				supported = append(supported, m)
			}
		}
	}
	return supported, true
}

func slicesHas(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// nearestEffort traduit le niveau demandé vers le plus proche parmi ceux que le
// gabarit accepte. Renvoie "" quand il n'y a rien de sensé à envoyer : le champ
// est alors simplement retiré de la requête, et le gabarit reprend son défaut.
//
// À égalité de distance on prend le niveau le PLUS FORT : demander « haute » et
// se retrouver avec « moyenne » dégrade la réponse en silence, alors qu'un cran
// au-dessus ne coûte que du temps de génération.
//
// « none » n'est jamais traduit : c'est une consigne de couper le raisonnement,
// pas une intensité. Un gabarit qui la refuse reçoit déjà `enable_thinking:
// false` par chat_template_kwargs (backend_models.go), qui dit la même chose
// dans une langue que tous comprennent.
func nearestEffort(want string, supported []string) string {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" || want == "none" {
		return ""
	}
	rank := effortRank(want)
	if rank < 0 {
		return ""
	}
	best, bestRank := "", -1
	for _, s := range supported {
		r := effortRank(s)
		if r < 0 || s == want {
			continue // s == want : le gabarit ne peut pas à la fois refuser et accepter
		}
		if best == "" || abs(r-rank) < abs(bestRank-rank) || (abs(r-rank) == abs(bestRank-rank) && r > bestRank) {
			best, bestRank = s, r
		}
	}
	return best
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// effortFromRejection combine les deux : à partir du corps d'erreur et du niveau
// demandé, renvoie le niveau à réessayer ("" = ne rien envoyer) et true si le
// refus a bien été reconnu.
func effortFromRejection(body, want string) (string, bool) {
	supported, ok := effortRejection(body)
	if !ok {
		return "", false
	}
	return nearestEffort(want, supported), true
}

// effortFallbacks retient les traductions apprises, par modèle : sans ça chaque
// message repaierait un aller-retour 500 avant de tomber sur la bonne valeur.
// Clé = fichier de modèle + niveau demandé ; la valeur "" est significative
// (= ne rien envoyer), d'où sync.Map plutôt qu'une map avec test de zéro.
var effortFallbacks sync.Map

func effortKey(want string) string {
	return filepath.Base(strings.TrimSpace(ReadConfig()["MODEL"])) + "\x00" + want
}

// effortResolve renvoie le niveau réellement envoyable pour la valeur demandée,
// c'est-à-dire la traduction déjà apprise pour ce modèle s'il y en a une.
func effortResolve(want string) string {
	if want == "" {
		return ""
	}
	if v, ok := effortFallbacks.Load(effortKey(want)); ok {
		return v.(string)
	}
	return want
}

// effortRemember enregistre la traduction pour les messages suivants.
func effortRemember(want, got string) {
	if want == "" {
		return
	}
	effortFallbacks.Store(effortKey(want), got)
}

// logEffortFallback trace le repli sur stderr : sinon l'intensité choisie dans
// l'interface n'est pas celle qui part au moteur, sans que rien ne le dise.
func logEffortFallback(want, got string) {
	if got == "" {
		fmt.Fprintf(os.Stderr, "[reasoning] gabarit du modèle : intensité %q refusée — champ retiré pour ce modèle\n", want)
		return
	}
	fmt.Fprintf(os.Stderr, "[reasoning] gabarit du modèle : intensité %q refusée — repli sur %q pour ce modèle\n", want, got)
}
