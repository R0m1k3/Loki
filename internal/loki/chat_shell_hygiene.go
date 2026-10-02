package loki

// chat_shell_hygiene.go — ce que le terminal rend au modèle, nettoyé (repris de
// shell.ts / shell-tail.ts d'OpenFox, 2.0.160) :
//
//   - « cmd | tail -N » : le code de sortie devenait celui de tail (0), et un
//     build en échec passait pour réussi. On retire le tail de la commande et
//     on garde nous-mêmes les N dernières lignes : même sortie, vrai code.
//   - séquences ANSI (couleurs, curseur) : du bruit en tokens, illisible ;
//   - « cmd & » en mode Code : le process orphelin n'a ni sortie ni arrêt —
//     bash_bg est fait pour ça.

import (
	"regexp"
	"strconv"
	"strings"
)

// tailPipeRe : « | tail », « | tail -20 », « | tail -n 20 », « | tail -n20 » en
// toute fin de commande.
var tailPipeRe = regexp.MustCompile(`\s*\|\s*tail(?:\s+-n\s*(\d+)|\s+-(\d+))?\s*$`)

// splitTailPipe retire un « | tail -N » final. Renvoie la commande sans lui et
// N (0 = pas de tail ; 10 = tail sans nombre, sa valeur par défaut).
func splitTailPipe(command string) (string, int) {
	m := tailPipeRe.FindStringSubmatchIndex(command)
	if m == nil {
		return command, 0
	}
	n := 10
	for _, g := range []int{2, 4} {
		if m[g] >= 0 {
			if v, err := strconv.Atoi(command[m[g]:m[g+1]]); err == nil && v > 0 {
				n = v
			}
		}
	}
	rest := strings.TrimSpace(command[:m[0]])
	if rest == "" {
		return command, 0
	}
	return rest, n
}

// shellANSIRe : séquences CSI (couleurs, curseur) et OSC (titres de fenêtre).
var shellANSIRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return shellANSIRe.ReplaceAllString(s, "")
}

// trailingBackground : la commande se termine par un « & » de mise en
// arrière-plan (pas « && »).
func trailingBackground(command string) bool {
	c := strings.TrimSpace(command)
	return strings.HasSuffix(c, "&") && !strings.HasSuffix(c, "&&")
}
