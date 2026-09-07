package loki

// web_projects.go — l'API du hub des projets. Un seul endpoint verbe-orienté
// (/api/projects) plutôt qu'une route par action : le hub fait six choses sur le
// même objet, six routes n'auraient rien clarifié.
//
// Toutes les mutations passent par projects.go, qui porte les invariants (slug
// unique, dernier projet indestructible, réassignation de l'actif).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// projectDTO est la vue d'un projet renvoyée à l'UI : le registre, plus ce qui
// se compte (discussions, pages, trackers) pour que le hub soit lisible sans
// ouvrir chaque projet.
type projectDTO struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Desc     string `json:"desc,omitempty"`
	Created  int64  `json:"created"`
	Convs    int    `json:"convs"`
	Pages    int    `json:"pages"`
	Trackers int    `json:"trackers"`
	Active   bool   `json:"active"`
}

func projectDTOs() []projectDTO {
	active := activeProjectSlug()
	// Un seul parcours de l'index des discussions pour tous les projets : compter
	// projet par projet relirait la base autant de fois qu'il y a de projets.
	convs := map[string]int{}
	for _, m := range convIndex() {
		convs[convProjectOf(m)]++
	}
	out := make([]projectDTO, 0, len(listProjects()))
	for _, p := range listProjects() {
		d := projectDTO{
			Slug: p.Slug, Name: p.Name, Desc: p.Desc, Created: p.CreatedAt,
			Convs: convs[p.Slug], Trackers: len(trackerKeysForProject(p.Slug)),
			Active: p.Slug == active,
		}
		// Les pages se comptent dans le dossier du projet : on emprunte memoryDir
		// le temps du comptage plutôt que de dupliquer la lecture du dossier.
		// MEMORY.md est exclu — c'est l'index du projet, pas une page de contenu :
		// l'annoncer ferait afficher « 1 page » sur un projet tout neuf et vide.
		withProject(p.Slug, func() {
			for _, m := range MemList() {
				if !isIndexFile(m.Name) {
					d.Pages++
				}
			}
		})
		out = append(out, d)
	}
	return out
}

// handleProjects :
//
//	GET                      → {ok, projects, active}
//	POST {action, ...}       → create | rename | desc | delete | switch
func handleProjects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sendJSON(w, 200, map[string]any{"ok": true, "projects": projectDTOs(), "active": activeProjectSlug()})
	case http.MethodPost:
		var body struct {
			Action string `json:"action"`
			Slug   string `json:"slug"`
			Name   string `json:"name"`
			Desc   string `json:"desc"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			sendJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		slug := strings.TrimSpace(body.Slug)
		var err error
		switch strings.ToLower(strings.TrimSpace(body.Action)) {
		case "create":
			_, err = createProject(body.Name)
		case "rename":
			err = renameProject(slug, body.Name)
		case "desc":
			err = setProjectDesc(slug, body.Desc)
		case "delete":
			// Une génération en cours travaille dans la mémoire du projet actif :
			// supprimer sous ses pieds la laisserait écrire dans le vide.
			if conv.isGenerating() {
				sendJSON(w, 409, map[string]any{"ok": false, "error": "une génération est en cours"})
				return
			}
			err = deleteProject(slug)
		case "switch":
			if conv.isGenerating() {
				sendJSON(w, 409, map[string]any{"ok": false, "error": "une génération est en cours"})
				return
			}
			err = projectSwitch(slug)
		default:
			err = fmt.Errorf("action inconnue : %q", body.Action)
		}
		if err != nil {
			sendJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		sendJSON(w, 200, map[string]any{"ok": true, "projects": projectDTOs(), "active": activeProjectSlug()})
	default:
		sendJSON(w, 405, map[string]any{"ok": false, "error": "méthode non autorisée"})
	}
}

// handleMemMove déplace une page mémoire vers un autre projet. Séparé de
// /api/projects : c'est une opération sur une PAGE, pas sur un projet.
func handleMemMove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, 405, map[string]any{"ok": false, "error": "méthode non autorisée"})
		return
	}
	var body struct {
		Name string `json:"name"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		sendJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := moveMemPage(body.Name, activeProjectSlug(), strings.TrimSpace(body.To)); err != nil {
		sendJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sendJSON(w, 200, map[string]any{"ok": true})
}
