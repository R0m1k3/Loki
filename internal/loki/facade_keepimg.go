package loki

import (
	"net/http"
)

// facade_keepimg.go — KEEP_TURN_IMAGES pour le tour tenu par la façade (lot 32
// de la migration). L'état du tour (images à l'essai, mesure, budget) vit dans
// la façade ; ce qui touche aux fichiers et à la vision reste ici :
//
//   - /internal/v1/turn/keepimg : la vision est-elle réellement active
//     (visionEnabled, engineSeesImages) et le budget des images gardées
//     (keepImgBudget) ;
//   - /internal/v1/turn/keepimg/store : une image relayée rangée par
//     référence (refRelayImage : fichier chiffré si la mémoire l'est, date
//     rafraîchie, relu à l'octet près).

// handleInternalTurnKeepImg : POST {} → {"vision":bool,"budget":n}.
func handleInternalTurnKeepImg(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	if !readInternalJSON(w, r, &in) {
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{
		"vision": visionEnabled() && engineSeesImages(),
		"budget": keepImgBudget(),
	})
}

// handleInternalTurnKeepImgStore : POST {"msg":Message} →
// {"ok":true,"msg":Message,"weight":octets} ou {"ok":false} (format inconnu,
// disque plein, mémoire verrouillée, relecture différente : l'image reste
// éphémère).
func handleInternalTurnKeepImgStore(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Msg Message `json:"msg"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	m, weight, ok := refRelayImage(in.Msg)
	if !ok {
		sendJSON(w, http.StatusOK, map[string]any{"ok": false})
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": m, "weight": weight})
}
