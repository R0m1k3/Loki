package loki

import (
	"context"
	"testing"
)

// historyCut ne garde que les N derniers échanges (un échange commence à `user`).
func TestHistoryCut(t *testing.T) {
	var log []LogEvent
	seq := 0
	add := func(d map[string]any) { seq++; log = append(log, LogEvent{Seq: seq, Delta: d}) }
	for i := 0; i < 5; i++ {
		add(map[string]any{"user": "q"})
		add(map[string]any{"content": "r"})
		add(map[string]any{"turn_done": true})
	}
	// 5 échanges, seq 1..15 ; les 2 derniers commencent aux seq 10 et 13.
	if cut, hidden := historyCut(log, 2); cut != 9 || hidden != 3 {
		t.Fatalf("tail=2 : cut=%d hidden=%d, attendu 9 et 3", cut, hidden)
	}
	if cut, hidden := historyCut(log, 5); cut != 0 || hidden != 0 {
		t.Fatalf("tail=5 (tout tient) : cut=%d hidden=%d, attendu 0 et 0", cut, hidden)
	}
	if cut, hidden := historyCut(log, 0); cut != 0 || hidden != 0 {
		t.Fatalf("tail=0 : pas de coupure attendue (cut=%d hidden=%d)", cut, hidden)
	}
	// Les événements rejoués après la coupure commencent bien au 4e échange.
	evs := coalesceReplay(log, 9)
	if u, _ := evs[0]["user"].(string); u != "q" || evs[0]["seq"] != 10 {
		t.Fatalf("premier événement rejoué inattendu : %v", evs[0])
	}
}

// La coupe réémet le mode (Chat/Code) posé dans la partie masquée.
func TestHistoryHeadCarriesMode(t *testing.T) {
	log := []LogEvent{
		{Seq: 1, Delta: map[string]any{"mode": "code"}},
		{Seq: 2, Delta: map[string]any{"user": "q1"}},
		{Seq: 3, Delta: map[string]any{"user": "q2"}},
	}
	head := historyHead(log, 2, 1)
	if len(head) != 2 || head[0]["history_more"] != 1 || head[1]["mode"] != "code" {
		t.Fatalf("en-tête inattendu : %v", head)
	}
}

// Un client paginé ne reçoit au chargement que les derniers échanges, précédés
// de {history_more} ; un ancien client (tail < 0) reçoit tout.
func TestSubscribeTailPaginates(t *testing.T) {
	c := newTestConv()
	for i := 0; i < 5; i++ {
		c.appendDelta(c.epoch, map[string]any{"user": "q"})
		c.appendDelta(c.epoch, map[string]any{"content": "r"})
	}
	collect := func(tail int) (more any, users int) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c.SubscribeTail(ctx, 0, tail, "", func(m map[string]any) bool {
			if v, ok := m["history_more"]; ok {
				more = v
			}
			if _, ok := m["user"]; ok {
				users++
			}
			if _, ok := m["caught_up"]; ok {
				cancel()
				return false
			}
			return true
		})
		return
	}
	if more, users := collect(2); more != 3 || users != 2 {
		t.Fatalf("tail=2 : history_more=%v, %d échanges rejoués (attendu 3 et 2)", more, users)
	}
	if more, users := collect(-1); more != nil || users != 5 {
		t.Fatalf("ancien client : history_more=%v, %d échanges (attendu rien et 5)", more, users)
	}
}
