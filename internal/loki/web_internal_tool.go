package loki

import (
	"encoding/json"
	"io"
	"net/http"
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
//	→ {"label":"ls","result":"…","diff":[…],"added":N,"removed":N,"vision_img":{…},"ask":{…}}
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
	sendJSON(w, http.StatusOK, struct {
		Label string `json:"label"`
		toolExecResult
	}{label, res})
}
