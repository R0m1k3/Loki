// web_sysprompt.go — prompt système personnalisé de l'utilisateur, persisté
// CÔTÉ SERVEUR (en base) et partagé entre appareils, comme la conversation
// elle-même. Historique : avant la conversation serveur (v0.4.x),
// l'UI envoyait son prompt système dans chaque requête /api/chat ; depuis,
// /api/chat/send ne porte que le message → le champ de l'UI n'avait plus aucun
// effet. Il est maintenant lu ici par la génération (chat_conversation.go), et
// InjectSkills (llm_client.go) le fusionne avec le préambule agent.
package loki

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Le prompt était GLOBAL : un seul texte pour tous les modèles. En pratique il ne
// l'est pas — la consigne qui va bien à un modèle à raisonnement en dessert un
// petit modèle d'instruction, et changer de preset obligeait à réécrire le champ
// à la main. Il est donc rattaché AU PRESET, et bascule avec lui.
//
// Il vit en base (une clé par preset) et NON dans le fichier .env du preset :
// un .env est lu ligne à ligne, il ne peut pas porter un texte multiligne — et
// un prompt système en fait presque toujours plusieurs.
//
// La clé globale historique reste le REPLI : une installation qui avait un prompt
// le garde tant que le preset actif n'en définit pas, plutôt que de se retrouver
// silencieusement sans consigne après mise à jour.
const (
	sysPromptGlobalKey = "sysprompt"
	sysPromptKeyPrefix = "sysprompt:"
)

// activePresetID renvoie l'id du preset actif ("" si aucun ne correspond à la
// configuration courante — config bricolée à la main, ou aucun preset créé).
func activePresetID() string {
	list, err := ListPresets()
	if err != nil {
		return ""
	}
	for _, p := range list {
		if p.Active {
			return p.ID
		}
	}
	return ""
}

// readSysPrompt renvoie le prompt système à appliquer ("" si aucun).
func readSysPrompt() string {
	if id := activePresetID(); id != "" {
		if s := strings.TrimSpace(getStr(bkState, sysPromptKeyPrefix+id)); s != "" {
			return s
		}
	}
	return getStr(bkState, sysPromptGlobalKey)
}

// saveSysPrompt écrit le prompt système DU PRESET ACTIF. Sans preset identifiable,
// on écrit le global — c'est le seul emplacement qui aura un effet, et le champ
// doit rester utilisable sur une installation sans preset.
func saveSysPrompt(text string) error {
	text = strings.TrimSpace(text)
	if id := activePresetID(); id != "" {
		return putStr(bkState, sysPromptKeyPrefix+id, text)
	}
	return putStr(bkState, sysPromptGlobalKey, text)
}

// handleSysPrompt :
//
//	GET  → {ok, text}
//	POST {text} → enregistre ("" = efface) puis renvoie {ok, text}
func handleSysPrompt(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sendJSON(w, 200, map[string]any{"ok": true, "text": readSysPrompt()})
	case http.MethodPost:
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			sendJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if err := saveSysPrompt(body.Text); err != nil {
			sendJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		sendJSON(w, 200, map[string]any{"ok": true, "text": readSysPrompt()})
	default:
		sendJSON(w, 405, map[string]any{"ok": false, "error": "méthode non autorisée"})
	}
}
