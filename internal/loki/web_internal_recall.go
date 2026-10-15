package loki

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// API interne de l'archive recall (lot 6 de la migration Rust).
//
// La compaction range VERBATIM les gros blocs du torse qu'elle résume, pour
// que l'outil recall puisse les rendre plus tard. Le numéro d'un bloc vient de
// la séquence bbolt du bucket : seul ce process peut le donner. Une
// compaction faite par la façade fait donc archiver ici.
//
//	POST /internal/v1/recall/archive  {"label":…,"role":…,"content":…}
//	→ 200 {"id":"r7"} · 423 mémoire verrouillée (rien d'écrit en clair)

const internalRecallMaxBody = 32 << 20

func handleInternalRecallArchive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST attendu"})
		return
	}
	var in struct {
		Label   string `json:"label"`
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, internalRecallMaxBody+1))
	if err != nil || len(raw) > internalRecallMaxBody || json.Unmarshal(raw, &in) != nil || in.Content == "" {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "bloc illisible"})
		return
	}
	id, err := archiveRecallBlock(in.Label, in.Role, in.Content)
	if errors.Is(err, errMemLocked) {
		sendJSON(w, http.StatusLocked, map[string]any{"error": err.Error()})
		return
	}
	if err != nil {
		sendJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{"id": id})
}
