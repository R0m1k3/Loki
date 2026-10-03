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
	// retryCustom : la surcharge `retry_patterns` est active. Ses motifs sont
	// quelconques (\A, (?s).*? sur des Ko…) : le balayage fenêtré pendant le
	// flux (textualToolCallFrom) ne vaut que pour les motifs embarqués.
	retryCustom bool
)

func retryPatterns() []*regexp.Regexp {
	retryOnce.Do(func() { retryRes, retryCustom = compileRetryPatterns() })
	return retryRes
}

func compileRetryPatterns() ([]*regexp.Regexp, bool) {
	pats := defaultRetryPatterns
	custom := false
	// Surcharge optionnelle : clé d'état `retry_patterns` = tableau JSON de
	// regex. Pas d'UI dédiée — c'est un réglage d'expert, posé via
	// `loki config` ou l'API ; l'embarqué couvre les cas connus.
	if raw := getStr(bkState, "retry_patterns"); raw != "" {
		var over []string
		if json.Unmarshal([]byte(raw), &over) == nil && len(over) > 0 {
			pats, custom = over, true
		}
	}
	var out []*regexp.Regexp
	for _, p := range pats {
		if re, err := regexp.Compile("(?mi)" + p); err == nil {
			out = append(out, re)
		}
	}
	return out, custom
}

// textualToolCall dit si le texte final du tour contient un appel d'outil
// écrit en toutes lettres au lieu d'être émis par le protocole.
func textualToolCall(content string) bool {
	return textualToolCallSnippet(content) != ""
}

// retryScanMargin / retryScanMaxBack : marge relue avant la fin du balayage
// précédent, et recul au-delà duquel on retombe sur le balayage complet.
const (
	retryScanMargin  = 512
	retryScanMaxBack = 16 << 10
)

// textualToolCallFrom : textualToolCall pendant le flux, quand content[:scanned]
// a DÉJÀ été balayé sans rien trouver. Relire toute la réponse à chaque morceau
// coûtait jusqu'à 360 µs par jeton sur 80 Ko — du CPU volé au décodage quand
// des experts tournent sur le processeur. Une correspondance neuve finit
// forcément après scanned : on ne relit que la fin, à partir d'un vrai début de
// ligne (sinon `^` verrait un début de ligne au milieu d'une phrase et
// relancerait un tour parfaitement bon). Motifs de la surcharge : balayage
// complet, rien ne borne leur portée.
func textualToolCallFrom(content string, scanned int) bool {
	if content == "" {
		return false
	}
	res := retryPatterns()
	start := 0
	if !retryCustom {
		start = retryScanStart(content, scanned)
	}
	w := content[start:]
	for _, re := range res {
		if re.MatchString(w) {
			return true
		}
	}
	return false
}

// retryScanStart : début de la fenêtre à relire. On part de scanned − marge,
// puis on recule tant qu'on est dans une suite que les motifs embarqués peuvent
// traverser sur toute sa longueur (\w+, \s*, `{"name":`, multioctet) : un
// `<function=` suivi de 600 lettres, ou un `{"name":` noyé dans les blancs,
// reste donc entier dans la fenêtre. Enfin, début de la ligne. Recul trop long
// ou ligne sans fin : 0, le balayage complet d'avant.
func retryScanStart(content string, scanned int) int {
	if scanned > len(content) {
		scanned = len(content)
	}
	p := scanned - retryScanMargin
	if p <= 0 {
		return 0
	}
	floor := scanned - retryScanMaxBack
	for p > 0 && retryRunByte(content[p-1]) {
		p--
		if p < floor {
			return 0
		}
	}
	i := strings.LastIndexByte(content[:p], '\n')
	if i < 0 || i+1 < floor {
		return 0
	}
	return i + 1
}

func retryRunByte(c byte) bool {
	switch {
	case c >= 0x80, c == '_', c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	}
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '"', ':', '{':
		return true
	}
	return false
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
