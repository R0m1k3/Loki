package loki

import (
	"net/http"
	"os"
	"strings"
)

// facade_projects.go — hub des projets tenu par la façade Rust
// (LOKI_PROJECTS=rust, lot 18 de la migration ; expérimental, le Go reste le
// défaut) : liste avec ses comptes, création, renommage, description,
// suppression, bascule (/api/projects).
//
// Le registre reste dans la base (state/projects) et la mémoire de chaque
// projet dans memory/<slug>. Le projet actif passe toujours par le Go
// (/internal/v1/project/activate), qui en garde une copie en RAM. Quand le Go
// tient la conversation, ce qui la touche reste ici :
//
//   - la bascule : discussion du projet quitté enregistrée, la plus récente
//     du projet rejoint ouverte (/internal/v1/project/switch) ;
//   - les discussions d'un projet supprimé (/internal/v1/project/drop-convs).
//
// Quand la façade tient la conversation, elle fait les deux elle-même.

const facadeProjectsEnv = "LOKI_PROJECTS"

// facadeProjects : la façade tient-elle le hub des projets ?
func facadeProjects() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeProjectsEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// facadeProjectsRoutes : les routes que la façade sert alors (projects_api.rs).
var facadeProjectsRoutes = map[string]bool{
	"/api/projects": true,
}

// handleInternalProjectSwitch : POST {slug} → 204, ou 400/409 + {error}.
func handleInternalProjectSwitch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Slug string `json:"slug"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	if conv.isGenerating() {
		sendJSON(w, http.StatusConflict, map[string]any{"error": "une génération est en cours"})
		return
	}
	if err := projectSwitch(strings.TrimSpace(in.Slug)); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleInternalProjectDropConvs : POST {slug} → 200 {dropped} — supprime les
// discussions du projet, comme deleteProject quand le Go tient la conversation.
func handleInternalProjectDropConvs(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Slug string `json:"slug"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	if conv.isGenerating() {
		sendJSON(w, http.StatusConflict, map[string]any{"error": "une génération est en cours"})
		return
	}
	n := 0
	for _, m := range convIndexForProject(strings.TrimSpace(in.Slug)) {
		if convDelete(m.ID) == nil {
			n++
		}
	}
	sendJSON(w, http.StatusOK, map[string]any{"dropped": n})
}
