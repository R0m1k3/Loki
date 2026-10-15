package loki

import (
	"encoding/json"
	"io"
	"net/http"
)

// Télémétrie d'une complétion d'un tour tenu par la façade Rust
// (LOKI_CHAT=rust). La boucle Go range chaque complétion dans l'anneau de
// perf_log.go (perfRecord) : c'est lui qui nourrit /api/perf/summary et
// l'alerte « cache perdu » posée sur les stats finales d'une complétion. La
// boucle Rust lui envoie la même chose, ici, au même moment.
//
//	POST /internal/v1/turn/perf
//	{"kind":"main","conv":"c…","iter":0,"complete":true,"external":false,
//	 "stats":{…StatsEvent…},"prefix":[…empreintes…],
//	 "probe":{"tools":[…],"kwargs":{…},"effort":""}}
//	→ {"kind":"main","lost":N,"lost_after":"…","lost_alert":true}
//
// prefix : empreintes cumulées des messages envoyés (perfPrefix), calculées
// par l'appelant — elles n'ont à être cohérentes qu'avec ses propres envois,
// d'une complétion à la suivante de la même discussion. probe : présent quand
// la boucle Go aurait lancé la sonde du gabarit (requête de forme normale du
// fil principal, moteur local).

const internalTurnPerfMaxBody = 8 << 20

func handleInternalTurnPerf(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST attendu"})
		return
	}
	var in struct {
		Kind     string     `json:"kind"`
		Conv     string     `json:"conv"`
		Iter     int        `json:"iter"`
		Complete bool       `json:"complete"`
		External bool       `json:"external"`
		Stats    StatsEvent `json:"stats"`
		Prefix   []uint64   `json:"prefix"`
		Probe    *struct {
			Tools  []Tool         `json:"tools"`
			Kwargs map[string]any `json:"kwargs"`
			Effort string         `json:"effort"`
		} `json:"probe"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, internalTurnPerfMaxBody+1))
	if err != nil || len(raw) > internalTurnPerfMaxBody || json.Unmarshal(raw, &in) != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "télémétrie illisible"})
		return
	}
	if in.Kind == "" {
		in.Kind = perfMain
	}
	var prefix []uint64
	if in.Complete {
		prefix = in.Prefix
	}
	rec := perfRecord(perfRecFromStats(perfTag{kind: in.Kind, conv: in.Conv}, in.Iter, in.Complete, in.Stats), prefix)
	alert := false
	if in.Complete && !in.External && ((rec.Lost != nil && *rec.Lost > 0) || (rec.Cached != nil && *rec.Cached == 0)) {
		alert = perfAlert(rec, perfLostAlertAt(ReadConfig()))
	}
	if in.Complete && !in.External && in.Probe != nil {
		tplProbeKick(in.Probe.Tools, in.Probe.Kwargs, in.Probe.Effort)
	}
	sendJSON(w, http.StatusOK, map[string]any{
		"kind": rec.Kind, "lost": rec.Lost, "lost_after": rec.LostAfter, "lost_alert": alert,
	})
}
