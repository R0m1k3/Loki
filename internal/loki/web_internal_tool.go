package loki

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// API interne d'exécution des outils (lot 6 de la migration Rust).
//
// Une boucle de chat écrite en Rust fera exécuter ses appels d'outils par ce
// process, le temps de porter les outils eux-mêmes (lot 7) : exactement la
// fonction qu'emploie runChatLoop (execTool), donc mêmes garde-fous (commande
// dangereuse, écriture hors du dossier de travail, cible distante…) et mêmes
// résultats.
//
//	POST /internal/v1/tool/exec
//	{"name":"bash","args":{"command":"ls"},"caps":{"Agent":true,"Code":false,…}}
//	→ {"label":"ls","result":"…","diff":[…],"added":N,"removed":N,"vision_img":{…},"ask":{…},
//	   "image":"…","shot":"…","relay":{…}}
//
// image, shot et relay sont ce que la boucle Go tire du résultat APRÈS
// l'exécution, et qui demande le disque ou le navigateur (toolStepExtras) :
// la capture à montrer dans la bulle, l'aperçu en direct du navigateur piloté,
// et le message `user` qui porte l'image au modèle (capture, see_image) —
// éphémère, à ne jamais ranger dans l'historique.
//
// Le contexte de la requête porte l'arrêt : si l'appelant coupe la connexion
// (bouton stop), la commande en cours est interrompue comme dans le Go.

// internalToolMaxBody : les arguments d'un appel (contenu d'un fichier écrit
// par l'agent compris) restent très en dessous.
const internalToolMaxBody = 32 << 20

func handleInternalToolExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST attendu"})
		return
	}
	var in struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
		Caps Caps           `json:"caps"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, internalToolMaxBody+1))
	if err != nil || len(raw) > internalToolMaxBody || json.Unmarshal(raw, &in) != nil || in.Name == "" {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "appel d'outil illisible"})
		return
	}
	if in.Args == nil {
		in.Args = map[string]any{}
	}
	label := toolCallLabel(in.Name, in.Args)
	res := execTool(r.Context(), in.Name, in.Args, label, in.Caps)
	image, shot, relay := toolStepExtras(in.Name, label, res)
	sendJSON(w, http.StatusOK, struct {
		Label string `json:"label"`
		toolExecResult
		Image string   `json:"image,omitempty"`
		Shot  string   `json:"shot,omitempty"`
		Relay *Message `json:"relay,omitempty"`
	}{label, res, image, shot, relay})
}

// toolStepExtras : ce que runChatLoop tire d'un résultat d'outil après
// l'exécution — capture de web_screenshot pour la bulle, aperçu du navigateur
// après une action qui change la page (jamais après une erreur), image relayée
// au modèle (capture avec la vision active, ou see_image réussi).
func toolStepExtras(name, label string, res toolExecResult) (image, shot string, relay *Message) {
	if !strings.HasPrefix(res.Result, "[erreur]") && cuAutoShotTool(name) {
		shot = cuAutoShot()
	}
	if name == "web_screenshot" {
		image = capturedRelPath(res.Result)
		if image != "" {
			if m, ok := screenshotImageMessage(image); ok {
				relay = &m
			}
		}
	}
	if res.VisionImg != nil {
		m := seeImageMessage(label, res.VisionImg)
		relay = &m
	}
	if res.VisionVideo != nil {
		m := seeVideoMessage(label, res.VisionVideo)
		relay = &m
	}
	return image, shot, relay
}
