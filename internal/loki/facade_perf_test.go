package loki

import (
	"strings"
	"testing"
)

// Lot 39 : quand la façade tient la conversation, les complétions du Go vont
// dans son anneau, et l'optimiseur le lit ; façade en panne, l'anneau du Go
// prend le relais. /api/perf/summary lui est remis.
func TestFacadePerfAnneau(t *testing.T) {
	testHome(t)
	freshPerf(t)
	f := demarrerFacade(t)
	f.internalStatus = 200
	f.internalBody = `{"rec":{"at":"2026-10-16T08:00:00Z","seq":7,"kind":"subagent","conv":"c1","iter":0,"complete":true,"total":10,"cached":2,"lost":3,"lost_after":"main"}}`
	got := perfRecord(perfRec{Kind: perfSubagent, Conv: "c1", Complete: true, Total: 10, Cached: iptr(2)}, []uint64{1, 2})
	if got.Seq != 7 || got.Lost == nil || *got.Lost != 3 || got.LostAfter != "main" {
		t.Fatalf("rangée par la façade : %+v", got)
	}
	if n := len(perfLog.snapshot()); n != 0 {
		t.Fatalf("rangée aussi ici : %d", n)
	}
	vu := f.recu()
	if len(vu) != 1 || !strings.HasPrefix(vu[0], "POST /internal/facade/v1/perf/record jeton ") ||
		!strings.Contains(vu[0], `"prefix":[1,2]`) || !strings.Contains(vu[0], `"kind":"subagent"`) {
		t.Fatalf("envoi : %v", vu)
	}

	f.internalBody = `{"recs":[{"at":"2026-10-16T08:00:00Z","seq":1,"kind":"main","iter":0,"complete":true,"total":40}]}`
	if s := perfSnapshot(); len(s) != 1 || s[0].Total != 40 {
		t.Fatalf("anneau de la façade : %+v", s)
	}

	// Façade en panne : l'anneau d'ici.
	f.internalStatus = 500
	got = perfRecord(perfRec{Kind: perfForeign}, nil)
	if got.Seq != 1 || got.Kind != perfForeign {
		t.Fatalf("repli : %+v", got)
	}
	if s := perfSnapshot(); len(s) != 1 || s[0].Kind != perfForeign {
		t.Fatalf("repli de l'anneau : %+v", s)
	}
	if !facadeChatRoutes["/api/perf/summary"] {
		t.Fatal("/api/perf/summary n'est pas remis à la façade")
	}
}
