package loki

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
)

// facade_perf.go — l'anneau de télémétrie tenu par la façade (lot 39 de la
// migration Rust). Quand elle tient la conversation, toutes les complétions y
// sont rangées :
//
//   - celles de ses tours, directement ;
//   - celles que le Go fait encore (sous-agents, tâches IA, bench, compaction
//     et préchauffage de ses tours, clients externes de /v1), par
//     /internal/facade/v1/perf/record.
//
// /api/perf/summary est servi par elle (facadeChatRoutes) et l'optimiseur lit
// l'anneau par perfSnapshot. Façade injoignable : la complétion est rangée
// ici, comme sans façade — la télémétrie ne fait jamais échouer un tour.

// facadePerfJSON appelle l'API de télémétrie de la façade et décode sa
// réponse.
func facadePerfJSON(path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+facadeAddr()+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(internalTokenHeader, internalToken())
	resp, err := facadeClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("façade : %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// facadePerfRecord range une complétion dans l'anneau de la façade et la rend
// telle que rangée (numéro, perte de cache, ce qui s'est intercalé).
func facadePerfRecord(r perfRec, prefix []uint64) (perfRec, bool) {
	var out struct {
		Rec perfRec `json:"rec"`
	}
	err := facadePerfJSON("/internal/facade/v1/perf/record", map[string]any{"rec": r, "prefix": prefix}, &out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[perf] complétion non transmise à la façade : %v\n", err)
		return perfRec{}, false
	}
	return out.Rec, true
}

// perfSnapshot : l'anneau, de la plus ancienne complétion à la plus récente —
// celui de la façade quand elle le tient.
func perfSnapshot() []perfRec {
	if facadeChat() {
		var out struct {
			Recs []perfRec `json:"recs"`
		}
		if err := facadePerfJSON("/internal/facade/v1/perf/snapshot", map[string]any{}, &out); err == nil {
			return out.Recs
		}
	}
	return perfLog.snapshot()
}
