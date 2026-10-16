package loki

import (
	"net/http"
	"os"
	"strings"
)

// facade_files.go — fichiers de la discussion tenus par la façade Rust
// (LOKI_FILES=rust, lot 11 de la migration ; expérimental, le Go reste le
// défaut) : dépôt des pièces jointes, panneau Fichiers, archive .zip,
// téléchargement et affichage des fichiers produits par l'agent.
//
// Le dossier de la discussion active reste décidé ICI (convWorkspace : la
// discussion active est connue de ce process, que la conversation soit tenue
// par lui ou par la façade) : la façade le demande à chaque requête par
// /internal/v1/conv/workspace. Les .part d'envois en cours sont les siens :
// ce process ne les vide plus au démarrage (la façade le fait au sien).

const facadeFilesEnv = "LOKI_FILES"

// facadeFiles : la façade tient-elle les fichiers de la discussion ?
func facadeFiles() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeFilesEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// facadeFilesRoutes : les routes que la façade sert alors (convfiles.rs).
var facadeFilesRoutes = map[string]bool{
	"/api/chat/upload":      true,
	"/api/chat/files":       true,
	"/api/chat/files/zip":   true,
	"/api/chat/file/delete": true,
	"/api/chat/file":        true,
	"/api/chat/image":       true,
}

// cleanStartupUploadParts vide les envois interrompus au démarrage — sauf quand
// la façade tient les fichiers : ce sont alors les siens.
func cleanStartupUploadParts() {
	if !facadeFiles() {
		cleanStaleUploadParts()
	}
}

// handleInternalConvWorkspace : le dossier de la discussion active (créé au
// besoin), la racine du dossier de travail, et l'identité de la discussion
// (nom de l'archive .zip).
//
//	POST /internal/v1/conv/workspace {} → {"dir","root","id","title"}
func handleInternalConvWorkspace(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	if !readInternalJSON(w, r, &in) {
		return
	}
	id := convEnsureActive()
	title := ""
	for _, m := range convIndex() {
		if m.ID == id {
			title = m.Title
			break
		}
	}
	sendJSON(w, http.StatusOK, map[string]any{
		"dir": convWorkspace(), "root": agentWorkspace(), "id": id, "title": title,
	})
}
