package loki

// web_tracker.go — l'API du panneau des trackers. Même forme que le hub des
// projets : un endpoint verbe-orienté, les invariants restant dans tracker.go.
//
// Différence avec la vue MODÈLE (trackerView) : l'UI reçoit des données
// structurées, pas de la prose. Le modèle a besoin d'un texte qui lui dit comment
// zoomer ; un panneau, lui, sait afficher une liste.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// trackerDTO : un tracker pour le panneau. Events n'est rempli que sur le détail
// d'un tracker — la liste ne charge jamais les points, sans quoi ouvrir le
// panneau lirait tout l'historique de tous les trackers.
type trackerDTO struct {
	Slug     string         `json:"slug"`
	Name     string         `json:"name"`
	Count    int            `json:"count"`
	FirstTS  int64          `json:"first_ts"`
	LastTS   int64          `json:"last_ts"`
	LastText string         `json:"last_text"`
	Events   []TrackerEvent `json:"events,omitempty"`
}

func trackerDTOs() []trackerDTO {
	list := trackerList()
	out := make([]trackerDTO, 0, len(list))
	for _, m := range list {
		out = append(out, trackerDTO{
			Slug: m.Slug, Name: m.Name, Count: m.Count,
			FirstTS: m.FirstTS, LastTS: m.LastTS, LastText: m.LastText,
		})
	}
	return out
}

// handleTracker :
//
//	GET             → {ok, trackers}            (liste, sans les points)
//	GET ?slug=x     → {ok, tracker}             (détail, avec ses points)
//	POST {action…}  → add | edit | delete | delete_tracker | rename | move
func handleTracker(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		slug := strings.TrimSpace(r.URL.Query().Get("slug"))
		if slug == "" {
			sendJSON(w, 200, map[string]any{"ok": true, "trackers": trackerDTOs()})
			return
		}
		s, ok := trackerLoad(slug)
		if !ok {
			sendJSON(w, 404, map[string]any{"ok": false, "error": "tracker introuvable"})
			return
		}
		d := trackerDTO{Slug: slug, Name: s.Name, Count: len(s.Events), Events: s.Events}
		if n := len(s.Events); n > 0 {
			d.FirstTS = s.Events[0].TS
			d.LastTS = s.Events[n-1].TS
			d.LastText = s.Events[n-1].Text
		}
		sendJSON(w, 200, map[string]any{"ok": true, "tracker": d})
	case http.MethodPost:
		var body struct {
			Action string `json:"action"`
			Slug   string `json:"slug"`
			Name   string `json:"name"`
			When   string `json:"when"`
			Text   string `json:"text"`
			ID     string `json:"id"`
			To     string `json:"to"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			sendJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		slug := strings.TrimSpace(body.Slug)
		var err error
		switch strings.ToLower(strings.TrimSpace(body.Action)) {
		case "add":
			_, err = trackerAdd(body.Name, body.When, body.Text)
		case "edit":
			err = trackerEditEvent(slug, body.ID, body.When, body.Text)
		case "delete":
			err = trackerDeleteEvent(slug, body.ID)
		case "delete_tracker":
			err = trackerDelete(slug)
		case "rename":
			err = trackerRename(slug, body.Name)
		case "move":
			err = trackerMoveToProject(slug, strings.TrimSpace(body.To))
		default:
			err = fmt.Errorf("action inconnue : %q", body.Action)
		}
		if err != nil {
			sendJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		sendJSON(w, 200, map[string]any{"ok": true, "trackers": trackerDTOs()})
	default:
		sendJSON(w, 405, map[string]any{"ok": false, "error": "méthode non autorisée"})
	}
}
