package loki

import (
	"net/http"
	"time"
)

// facade_perf.go — la télémétrie des complétions de la façade (lot 29 de la
// migration). Quand la façade tient la conversation (LOKI_CHAT=rust), ses
// complétions — tours, vérifications, résumés de compaction — rejoignent
// l'anneau du Go (perf_log.go) : /api/perf/summary les compte, la perte de
// cache se calcule contre la complétion précédente de la même discussion, et
// l'alerte suit le seuil du preset actif (perfLostAlertAt), comme pour un
// tour du Go.

// handleInternalPerfRecord : POST {rec, prefix, external} → {seq, lost,
// lost_after, alert}. rec : une complétion (perfRec, sans at ni seq) ;
// prefix : les empreintes cumulées des messages envoyés (perfPrefix), vide
// pour une complétion incomplète ; external : preset externe (jamais
// d'alerte).
func handleInternalPerfRecord(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Rec      perfRec  `json:"rec"`
		Prefix   []uint64 `json:"prefix"`
		External bool     `json:"external"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	in.Rec.At, in.Rec.Seq, in.Rec.Lost, in.Rec.LostAfter = time.Time{}, 0, nil, ""
	rec := perfRecord(in.Rec, in.Prefix)
	alert := false
	if rec.Complete && !in.External && ((rec.Lost != nil && *rec.Lost > 0) || (rec.Cached != nil && *rec.Cached == 0)) {
		alert = perfAlert(rec, perfLostAlertAt(ReadConfig()))
	}
	sendJSON(w, http.StatusOK, map[string]any{"seq": rec.Seq, "lost": rec.Lost, "lost_after": rec.LostAfter, "alert": alert})
}
