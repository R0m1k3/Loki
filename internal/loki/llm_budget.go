package loki

// llm_budget.go — budget SOUPLE d'appels d'outils par tour.
//
// Le plafond dur (TOOL_LIMIT) et l'anti-boucle ont été retirés en v0.6.3 : ils
// coupaient des tours parfaitement légitimes, une recherche enchaînant sans
// problème des dizaines d'appels. Mais il ne restait plus RIEN entre « le
// modèle converge » et « le modèle tourne en rond pendant une heure » — vu en
// production : ~50 appels d'outil, 55 minutes, le modèle relisant cinq fois les
// mêmes fonctions pour re-dériver une conclusion qu'il avait déjà écrite.
//
// La déduplication d'appels identiques (repeatedCallResult) ne rattrape pas ce
// cas : sa clé est « nom + arguments bruts », et relire le même fichier par
// tranches (`sed -n '1,80p'` puis `sed -n '80,160p'`) produit des clés
// différentes. Élargir cette clé à la CIBLE serait pire que le mal — deux
// tranches d'un même fichier renvoient un contenu différent, les confondre
// casserait toute lecture paginée légitime.
//
// D'où un budget qui ne coupe rien : au-delà d'un palier, on RAPPELLE au modèle
// combien d'appels il a déjà faits et on lui demande de conclure. Le tour reste
// entièrement sous son contrôle — c'est de la pression, pas une barrière.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// agentBudgetDefault : nombre d'appels d'outils au-delà duquel un tour cesse
// d'être une recherche et commence à ressembler à du sur-place. Assez haut pour
// qu'une vraie enquête (lire dix fichiers, croiser trois pages web) passe sans
// jamais voir le rappel.
const agentBudgetDefault = 24

// agentBudget lit le palier dans config.env (AGENT_BUDGET). 0 — ou une valeur
// off/false/no/non — désactive complètement le mécanisme.
func agentBudget() int {
	v := strings.ToLower(strings.TrimSpace(ReadConfig()["AGENT_BUDGET"]))
	switch v {
	case "":
		return agentBudgetDefault
	case "off", "false", "no", "non", "disable", "disabled":
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return n
	}
	return agentBudgetDefault
}

// budgetNudge renvoie le rappel à injecter quand le tour vient de franchir un
// palier, ou "" s'il n'y a rien à dire.
//
//	calls  : appels d'outils exécutés depuis le début du tour
//	budget : taille d'un palier (0 = mécanisme désactivé)
//	sent   : rappels déjà envoyés dans ce tour
//
// Les paliers sont à budget, 2×budget, 3×budget… : la pression revient donc
// régulièrement au lieu de s'éteindre après un avertissement ignoré. Le coût en
// contexte est négligeable (une phrase toutes les 24 requêtes) et le message est
// AJOUTÉ EN FIN d'historique, ce qui laisse intact le préfixe déjà en cache côté
// llama-server.
//
// Le texte est en anglais comme le reste du protocole d'outils : c'est la langue
// dans laquelle les modèles suivent le mieux une consigne impérative, quelle que
// soit celle de la conversation.
func budgetNudge(calls, budget, sent int) string {
	if budget <= 0 || calls < budget*(sent+1) {
		return ""
	}
	n := fmt.Sprintf("%d tool calls", calls)
	switch sent {
	case 0:
		return "[system] You have made " + n + " in this single turn. That is a lot: you almost certainly have enough to answer already. " +
			"Stop exploring and write your final answer now, unless exactly one specific call is genuinely still missing."
	case 1:
		return "[system] " + n + " now, and you are going in circles. Re-reading a file you have already read, or re-deriving a conclusion you have already written, adds nothing. " +
			"Answer NOW with what you have. If something remains uncertain, say so in the answer instead of investigating further."
	default:
		return "[system] " + n + ". Stop. Write your final answer in this message. Do not call another tool."
	}
}

// logBudget trace UNE ligne par rappel sur la sortie d'erreur (donc dans
// `journalctl -u loki-ui`), comme logCompact. Sans ça, un tour qui part en
// vrille est invisible côté serveur : on ne voit qu'une génération qui dure.
func logBudget(calls, budget, sent int) {
	fmt.Fprintf(os.Stderr, "[agent] %d appels d'outils sur ce tour (palier=%d) — rappel n°%d envoyé au modèle\n",
		calls, budget, sent)
}
