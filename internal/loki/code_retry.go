package loki

// code_retry.go — filets « auto-retry » (repris des auto-retry patterns
// d'OpenFox, réécrits en Go — voir NOTICE.md) : quand le modèle termine son
// tour sur un texte qui contient un APPEL D'OUTIL TEXTUEL (halluciné, donc
// jamais exécuté), on relance le tour UNE fois avec une consigne corrective au
// lieu de laisser l'utilisateur lire un pseudo-JSON d'appel d'outil.
//
// Typique des petits modèles très quantifiés : `default_api:bash`,
// `<tool_call>{...}</tool_call>`, un bloc ```tool_code```… Le modèle VOULAIT
// agir ; il a juste raté la syntaxe du protocole. Le nudge « pensé sans agir »
// existant ne les attrapait pas : le tour a bien produit du contenu.

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// defaultRetryPatterns : motifs d'appels d'outils textuels. Volontairement
// serrés — un faux positif relancerait un tour parfaitement bon.
var defaultRetryPatterns = []string{
	`<tool_call>`,
	`</tool_call>`,
	`<\|tool_call\|>`,
	"```tool_code",
	"```tool_call",
	`\bdefault_api[.:]\w+`,
	`\bfunctions\.\w+\s*\(`,
	`^\s*\{"name":\s*"(bash|read|grep|glob|edit|write|mem_\w+|web_\w+|git_\w+|criteria|ask)"`,
	`\[TOOL_REQUEST\]`,
	// Format XML de Qwen3-Coder, quand le gabarit du moteur ne l'a pas converti
	// en tool_calls (OpenFox 2.0.15x) : <function=bash><parameter=command>…
	`<function=\w+>`,
	`<parameter=\w+>`,
}

// Compilés à la première utilisation, pas à l'init du package : la surcharge
// vit dans la base bbolt, qu'on ne veut pas ouvrir avant que le process ait
// choisi son rôle (CLI, serveur…).
var (
	retryOnce sync.Once
	retryRes  []*regexp.Regexp
)

func retryPatterns() []*regexp.Regexp {
	retryOnce.Do(func() { retryRes = compileRetryPatterns() })
	return retryRes
}

func compileRetryPatterns() []*regexp.Regexp {
	pats := defaultRetryPatterns
	// Surcharge optionnelle : clé d'état `retry_patterns` = tableau JSON de
	// regex. Pas d'UI dédiée — c'est un réglage d'expert, posé via
	// `loki config` ou l'API ; l'embarqué couvre les cas connus.
	if raw := getStr(bkState, "retry_patterns"); raw != "" {
		var custom []string
		if json.Unmarshal([]byte(raw), &custom) == nil && len(custom) > 0 {
			pats = custom
		}
	}
	var out []*regexp.Regexp
	for _, p := range pats {
		if re, err := regexp.Compile("(?mi)" + p); err == nil {
			out = append(out, re)
		}
	}
	return out
}

// textualToolCall dit si le texte final du tour contient un appel d'outil
// écrit en toutes lettres au lieu d'être émis par le protocole.
func textualToolCall(content string) bool {
	return textualToolCallSnippet(content) != ""
}

// textualToolCallSnippet renvoie l'extrait fautif (le motif reconnu et un peu
// de ce qui le suit), "" si aucun. Cité dans la consigne corrective : le
// modèle voit CE qu'il a mal écrit au lieu d'une remontrance abstraite.
func textualToolCallSnippet(content string) string {
	if content == "" {
		return ""
	}
	for _, re := range retryPatterns() {
		if loc := re.FindStringIndex(content); loc != nil {
			end := loc[1] + 80
			if end > len(content) {
				end = len(content)
			}
			for end < len(content) && !utf8.RuneStart(content[end]) {
				end++
			}
			return strings.TrimSpace(content[loc[0]:end])
		}
	}
	return ""
}

// maxPatternRetries : relances « appel écrit en texte » consécutives. Le
// compteur repart à zéro après chaque appel d'outil réussi (OpenFox) : un long
// tour d'agent peut rater la syntaxe plusieurs fois, mais pas en boucle.
const maxPatternRetries = 3

// retryCorrective : le message réinjecté pour relancer le tour, avec l'extrait
// fautif.
func retryCorrective(snippet string) string {
	return retryCorrectiveLead + snippet +
		"\nTool calls must go through the tool-call protocol, never in the answer text. " +
		"Redo it now: emit the real tool call, or answer directly without pretending to call a tool."
}
