package loki

import (
	"net/http"
)

// facade_slots.go — les slots du moteur pour le tour tenu par la façade (lot 31
// de la migration). La machinerie reste ici, où vivent la configuration, le
// compteur des requêtes en vol et le tampon du slot de la discussion
// (llm_slots.go) ; la façade demande, avant chaque requête au moteur local,
// le slot où l'envoyer.
//
//   - SIDE_SLOT (llm_sideslot.go) : le fil de la discussion sur le slot 0, le
//     reste (vérification, résumé sur transcription) sur le 1, tant que le
//     second slot est en service ; sinon aucun id_slot.
//   - SLOT_PERSIST (llm_slotpersist.go) : l'état gardé à la dernière bascule de
//     preset est rechargé avant la première requête de la discussion, si tout
//     concorde. La sauvegarde, elle, se fait à la bascule (déjà ici) : le slot
//     porte la discussion dès que le moteur a accepté une étape de son tour
//     (/internal/v1/oai/served, engineMarkMain).

// handleInternalTurnSlot : POST {"conv":"…","main":true} → {"slot":0|1|-1}.
// main : requête du fil de la discussion (étape d'un tour, résumé en
// continuation) ; conv : la discussion d'une étape de tour, pour le
// rechargement SLOT_PERSIST — vide pour un résumé. -1 : aucun id_slot.
func handleInternalTurnSlot(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Conv string `json:"conv"`
		Main bool   `json:"main"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	ep := resolveChatEndpoint()
	if in.Main && in.Conv != "" {
		slotPersistRestore(ep, in.Conv)
	}
	sendJSON(w, http.StatusOK, map[string]any{"slot": engineSlotFor(ep, in.Main)})
}
