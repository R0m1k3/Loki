package loki

import (
	"net/http"
	"strings"
)

// API interne de l'état des outils (lot 7 de la migration Rust).
//
// La façade exécute elle-même les outils fichiers et le shell (read, grep,
// glob, write, edit, bash, ask) quand elle tient la conversation. Ce qu'ils
// partagent avec le reste de Loki reste ici, en un seul exemplaire : le
// dossier de travail de la discussion, le dossier mémoire interdit au shell,
// la cible distante, le suivi « lu avant d'écrire » (lu aussi par les
// sous-agents et les tâches, qui tournent ici) et les diagnostics LSP.
//
//	POST /internal/v1/tool/context {}
//	→ {"cwd":"…","memory_dir":"…","target":"slug|"}
//	POST /internal/v1/tool/tracker {"op":"read","path":"/abs"}            → {}
//	POST /internal/v1/tool/tracker {"op":"check","path":"/abs","for_write":true}
//	→ {"refusal":"[refusé] …"|""}
//	POST /internal/v1/tool/wrote {"path":"/abs","caps":{…}}
//	→ {"lsp":"\n\n[diagnostics LSP] …"|""}
//
// wrote : après une écriture réussie (write/edit) — note du tracker, puis le
// bloc des diagnostics LSP que la boucle ajoute au résultat de l'outil.

func handleInternalToolContext(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	if !readInternalJSON(w, r, &in) {
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{
		"cwd":        agentCwd(),
		"memory_dir": memoryDir(),
		"target":     agentTargetSlug(),
	})
}

func handleInternalToolTracker(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Op       string `json:"op"`
		Path     string `json:"path"`
		ForWrite bool   `json:"for_write"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Path) == "" {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "chemin manquant"})
		return
	}
	switch in.Op {
	case "read":
		trackerNoteRead(in.Path)
		sendJSON(w, http.StatusOK, map[string]any{})
	case "check":
		sendJSON(w, http.StatusOK, map[string]any{"refusal": trackerCheck(in.Path, in.ForWrite)})
	default:
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "op inconnue"})
	}
}

func handleInternalToolWrote(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string `json:"path"`
		Caps Caps   `json:"caps"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Path) == "" {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "chemin manquant"})
		return
	}
	trackerNoteWrite(in.Path)
	sendJSON(w, http.StatusOK, map[string]any{"lsp": lspDiagBlock(in.Path, in.Caps)})
}
