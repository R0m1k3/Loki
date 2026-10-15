package loki

import (
	"encoding/json"
	"fmt"
	"testing"
)

// La complétion d'un tour tenu par Rust arrive dans l'anneau de télémétrie,
// comme celle de la boucle Go : /api/perf/summary la compte.
func TestTurnPerfDansLAnneau(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	avant := perfSummarize(perfLog.snapshot())
	n0 := 0
	if k := avant.Kinds[perfMain]; k != nil {
		n0 = k.Completions
	}
	body := `{"kind":"main","conv":"c-perf","iter":0,"complete":true,"external":false,
		"stats":{"prompt_tokens":120,"prompt_tokens_total":120,"gen_tokens":8,"cache_tokens":0},
		"prefix":[1,2,3]}`
	w := appelOAIInterne(t, "POST", "/internal/v1/turn/perf", body)
	if w.Code != 200 {
		t.Fatalf("code %d : %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got["kind"] != "main" {
		t.Fatalf("réponse : %s", w.Body.String())
	}
	apres := perfSummarize(perfLog.snapshot())
	if k := apres.Kinds[perfMain]; k == nil || k.Completions != n0+1 {
		t.Fatalf("complétion non rangée : %+v", apres.Kinds[perfMain])
	}
	if w := appelOAIInterne(t, "POST", "/internal/v1/turn/perf", "{"); w.Code != 400 {
		t.Fatalf("corps illisible : %d", w.Code)
	}
}

// Une étape de tour acceptée par le moteur : le slot porte la discussion
// (engineSlotHolds), comme engineMarkMain dans la boucle Go.
func TestOAIServedMarqueLeSlot(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	w := appelOAIInterne(t, "POST", "/internal/v1/oai/begin", `{"main_conv":"c-slot"}`)
	var b struct {
		Ticket uint64 `json:"ticket"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || b.Ticket == 0 {
		t.Fatalf("begin : %s", w.Body.String())
	}
	if engineSlotHolds("c-slot") {
		t.Fatal("slot marqué avant que le moteur ait accepté la requête")
	}
	tk := fmt.Sprintf(`{"ticket":%d}`, b.Ticket)
	if w := appelOAIInterne(t, "POST", "/internal/v1/oai/served", tk); w.Code != 204 {
		t.Fatalf("served : %d", w.Code)
	}
	if !engineSlotHolds("c-slot") {
		t.Fatal("slot non marqué après served")
	}
	if w := appelOAIInterne(t, "POST", "/internal/v1/oai/end", tk); w.Code != 204 {
		t.Fatalf("end : %d", w.Code)
	}
	if w := appelOAIInterne(t, "POST", "/internal/v1/oai/served", tk); w.Code != 404 {
		t.Fatalf("served après end : %d", w.Code)
	}
}
