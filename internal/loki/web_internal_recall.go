package loki

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
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
//
// Les outils recall et recall_search de la façade (lot 46) lisent l'archive
// ici ; le reste (id tolérant, score, mise en forme) est chez elle :
//
//	GET /internal/v1/recall/block?seq=7 → 200 bloc · 404 absent ou illisible
//	GET /internal/v1/recall/blocks      → 200 [blocs lisibles, par séquence]

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

func handleInternalRecallBlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET attendu"})
		return
	}
	seq, err := strconv.ParseUint(r.URL.Query().Get("seq"), 10, 64)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "seq illisible"})
		return
	}
	blk, ok := recallGetSeq(seq)
	if !ok {
		sendJSON(w, http.StatusNotFound, map[string]any{"error": "bloc introuvable"})
		return
	}
	sendJSON(w, http.StatusOK, blk)
}

func handleInternalRecallBlocks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET attendu"})
		return
	}
	blks := recallAll()
	if blks == nil {
		blks = []recallBlock{}
	}
	sendJSON(w, http.StatusOK, blks)
}
