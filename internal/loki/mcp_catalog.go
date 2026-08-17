package loki

// Catalogue MCP embarqué.
//
// Ajouter un serveur MCP demandait jusqu'ici d'écrire soi-même la commande et
// ses arguments (`npx -y @scope/paquet …`) dans la modale. Le catalogue fournit
// une liste de serveurs connus, déjà renseignés : un clic préremplit la modale,
// l'utilisateur relit la commande et enregistre.
//
// Le catalogue est un JSON EMBARQUÉ dans le binaire (go:embed), pas un appel à
// un annuaire distant : cohérent avec un Loki qui tourne hors ligne, et rien de
// ce qui s'exécute sur la machine ne vient du réseau. Pour proposer un serveur
// de plus : éditer mcp_catalog.json et recompiler.
//
// SÉCURITÉ : le catalogue ne fait qu'AFFICHER une commande. Il n'installe ni
// n'exécute rien tout seul — c'est l'enregistrement du serveur par
// l'utilisateur, via la modale existante, qui décide. Même niveau de confiance
// que l'outil `bash` du mode agent (voir l'en-tête de mcp_config.go).

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

//go:embed mcp_catalog.json
var mcpCatalogRaw []byte

// MCPCatalogEntry décrit un serveur proposé par le catalogue. Les champs de
// transport reprennent ceux de MCPServerConfig : l'UI peut les recopier tels
// quels dans la modale d'ajout.
type MCPCatalogEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	Description string `json:"description"`
	Homepage    string `json:"homepage,omitempty"`

	// Runtime : binaire nécessaire pour lancer le serveur ("node" → npx,
	// "python" → uvx). Sert à signaler une entrée non installable ici.
	Runtime string            `json:"runtime,omitempty"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`

	// Secrets : variables d'environnement que l'utilisateur DOIT renseigner
	// (jetons, clés d'API). L'UI les met en évidence ; elles sont volontairement
	// vides dans le catalogue.
	Secrets []string `json:"secrets,omitempty"`

	// Renseignés à la lecture, pas dans le fichier.
	Installed bool `json:"installed"`
	RuntimeOK bool `json:"runtimeOk"`
}

type mcpCatalogFile struct {
	Version int               `json:"version"`
	Servers []MCPCatalogEntry `json:"servers"`
}

var (
	mcpCatalogOnce    sync.Once
	mcpCatalogParsed  []MCPCatalogEntry
	mcpCatalogParseEr error
)

// mcpCatalogEntries décode le JSON embarqué une seule fois. Le contenu est figé
// dans le binaire : inutile de le relire à chaque appel.
func mcpCatalogEntries() ([]MCPCatalogEntry, error) {
	mcpCatalogOnce.Do(func() {
		var f mcpCatalogFile
		if err := json.Unmarshal(mcpCatalogRaw, &f); err != nil {
			mcpCatalogParseEr = fmt.Errorf("catalogue MCP illisible : %w", err)
			return
		}
		mcpCatalogParsed = f.Servers
	})
	return mcpCatalogParsed, mcpCatalogParseEr
}

// mcpRuntimeAvailable indique si le binaire qui lance ce type de serveur est
// présent. L'image conteneur embarque Node ; uv/uvx peut manquer.
func mcpRuntimeAvailable(runtime string) bool {
	switch runtime {
	case "node":
		return lookPathOK("npx")
	case "python":
		return lookPathOK("uvx")
	default:
		return true
	}
}

func lookPathOK(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

// MCPCatalog renvoie les entrées du catalogue, chemins résolus et marquées
// « déjà installée » (un serveur configuré porte le même nom que l'entrée).
func MCPCatalog() ([]MCPCatalogEntry, error) {
	entries, err := mcpCatalogEntries()
	if err != nil {
		return nil, err
	}
	configured, err := LoadMCPConfig()
	if err != nil {
		// Une config MCP illisible ne doit pas vider le catalogue : on affiche
		// les entrées sans savoir lesquelles sont déjà là.
		configured = map[string]MCPServerConfig{}
	}

	out := make([]MCPCatalogEntry, 0, len(entries))
	for _, e := range entries {
		e.Args = expandCatalogSlice(e.Args)
		e.Env = expandCatalogMap(e.Env)
		_, e.Installed = configured[e.ID]
		e.RuntimeOK = mcpRuntimeAvailable(e.Runtime)
		out = append(out, e)
	}
	return out, nil
}

// expandCatalogToken remplace les jetons de chemin du catalogue. Seul
// {workspace} existe : c'est le dossier de travail de l'agent, le seul chemin
// qu'un serveur du catalogue a une raison légitime de recevoir par défaut.
func expandCatalogToken(s string) string {
	if !strings.Contains(s, "{workspace}") {
		return s
	}
	return strings.ReplaceAll(s, "{workspace}", workspaceDir())
}

func expandCatalogSlice(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = expandCatalogToken(s)
	}
	return out
}

func expandCatalogMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return in
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = expandCatalogToken(v)
	}
	return out
}
