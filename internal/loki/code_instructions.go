package loki

// code_instructions.go — consignes du dépôt (AGENTS.md, CLAUDE.md) en mode Code.
// Repris de context/instructions.ts d'OpenFox.
//
// Un dépôt qui documente ses commandes de build et de test, ses conventions,
// ses pièges, le fait dans ces fichiers. Sans eux, le modèle les redécouvrait
// à coups de read et de bash — quand il les redécouvrait. Le contenu part avec
// le contexte du projet : déplacé dans le premier message utilisateur par
// normalizeSystemMessages, il ne touche pas au préfixe système (cache).

import (
	"os"
	"path/filepath"
	"strings"
)

const codeInstructionsPrefix = "Repository instructions"

// codeInstructionsMax : au-delà, on tronque (≈ 1k tokens) — ces fichiers
// peuvent être longs, et le contexte est compté.
const codeInstructionsMax = 4000

// codeInstructionFiles : par ordre de préférence ; le premier trouvé gagne.
var codeInstructionFiles = []string{"AGENTS.md", "CLAUDE.md", ".github/copilot-instructions.md"}

// codeInstructionsMessage renvoie les consignes du dépôt de la discussion
// (racine, ou dépôt cloné détecté par gitRepoDir), ok=false s'il n'y en a pas.
func codeInstructionsMessage(caps Caps) (Message, bool) {
	if !caps.Code {
		return Message{}, false
	}
	dirs := []string{agentCwd()}
	if d, err := gitRepoDir(""); err == nil && d != dirs[0] {
		dirs = append([]string{d}, dirs...)
	}
	for _, dir := range dirs {
		for _, name := range codeInstructionFiles {
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil || len(strings.TrimSpace(string(b))) == 0 {
				continue
			}
			s := strings.TrimSpace(string(b))
			if r := []rune(s); len(r) > codeInstructionsMax {
				s = string(r[:codeInstructionsMax]) + "\n…[tronqué — lis le fichier pour la suite]"
			}
			rel := name
			if r, err := filepath.Rel(agentCwd(), filepath.Join(dir, name)); err == nil {
				rel = filepath.ToSlash(r)
			}
			return Message{Role: "system", Content: codeInstructionsPrefix + " (" + rel + ") — follow them:\n\n" + s}, true
		}
	}
	return Message{}, false
}
