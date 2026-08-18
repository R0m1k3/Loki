package loki

// code_roles.go — registre des rôles du mode code. Chaque rôle est un prompt
// Markdown EMBARQUÉ dans le binaire (agents/*.md), comme le catalogue MCP :
// pas d'appel réseau, pour en changer on édite le fichier et on recompile.
// Démarche reprise du registre d'agents d'OpenFox (builder/planner/explorer/
// verifier/code-reviewer — voir NOTICE.md), prompts réécrits COURTS : un
// préambule verbeux fait sur-raisonner les modèles locaux (voir le commentaire
// de baseSystemPrompt).

import (
	"embed"
	"strings"
)

//go:embed agents/*.md
var rolesFS embed.FS

// roleNames : les rôles connus. builder et verifier sont pilotés par la boucle
// du mode code (code_verify.go) ; planner s'active quand l'utilisateur demande
// un plan ; explorer et code-reviewer sont disponibles pour des passes
// manuelles futures.
var roleNames = []string{"builder", "planner", "explorer", "verifier", "code-reviewer"}

// rolePrompt renvoie le prompt d'un rôle, "" si inconnu.
func rolePrompt(name string) string {
	b, err := rolesFS.ReadFile("agents/" + name + ".md")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// planIntentRe-like : l'utilisateur demande-t-il un PLAN plutôt qu'une
// implémentation ? Heuristique simple sur le début du message.
func wantsPlan(text string) bool {
	low := strings.ToLower(text)
	for _, kw := range []string{"fais un plan", "fait un plan", "propose un plan", "planifie", "plan d'implémentation", "plan d'action", "make a plan", "plan first"} {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}
