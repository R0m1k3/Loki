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

// Perte de cache calculée contre la complétion précédente de la même
// discussion, ce qui s'est intercalé, alerte au seuil du preset actif ; un
// preset externe ou une complétion incomplète ne la signalent jamais.
func TestTurnPerfPerteEtAlerte(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	post := func(body string) (int, map[string]any) {
		w := appelOAIInterne(t, "POST", "/internal/v1/turn/perf", body)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	code, out := post(`{"kind":"main","conv":"fp1","iter":0,"complete":true,
		"stats":{"prompt_tokens":5000,"prompt_tokens_total":5000,"cache_tokens":0},"prefix":[11,22]}`)
	if code != 200 || out["lost"] != nil || out["lost_after"] != "" {
		t.Fatalf("première : %d %v", code, out)
	}
	// Une tâche passe entre deux.
	perfRecord(perfRec{Kind: perfTask, Conv: "t", Complete: true, Total: 10}, nil)
	// La suivante prolonge la première, mais n'en reprend que 100 jetons.
	code, out = post(`{"kind":"main","conv":"fp1","iter":1,"complete":true,
		"stats":{"prompt_tokens":5900,"prompt_tokens_total":6000,"cache_tokens":100},"prefix":[11,22,33]}`)
	if code != 200 || out["lost"] != float64(4900) || out["lost_after"] != perfTask || out["lost_alert"] != true {
		t.Fatalf("perte : %d %v", code, out)
	}
	if recs := perfLog.snapshot(); recs[len(recs)-1].Conv != "fp1" || recs[len(recs)-1].Iter != 1 || recs[len(recs)-1].New != 5900 {
		t.Fatalf("relevé rangé : %+v", recs[len(recs)-1])
	}
	// Preset externe : la perte est rangée, jamais signalée.
	code, out = post(`{"kind":"main","conv":"fp1","iter":2,"complete":true,"external":true,
		"stats":{"prompt_tokens_total":7000,"cache_tokens":100},"prefix":[11,22,33,44]}`)
	if code != 200 || out["lost"] != float64(5900) || out["lost_alert"] != false {
		t.Fatalf("externe : %d %v", code, out)
	}
	// Incomplète : ses empreintes sont ignorées, ni perte ni alerte.
	code, out = post(`{"kind":"main","conv":"fp1","iter":3,"complete":false,
		"stats":{"cache_tokens":0},"prefix":[11,22,33,44,55]}`)
	if code != 200 || out["lost"] != nil || out["lost_alert"] != false {
		t.Fatalf("incomplète : %d %v", code, out)
	}
	if recs := perfLog.snapshot(); recs[len(recs)-1].Complete {
		t.Fatalf("incomplète rangée complète : %+v", recs[len(recs)-1])
	}
	// Résumé de compaction : sa nature est gardée.
	code, out = post(`{"kind":"compact","conv":"fp1","complete":true,"stats":{"prompt_tokens_total":900}}`)
	if code != 200 || out["kind"] != perfCompact {
		t.Fatalf("compaction : %d %v", code, out)
	}
	// Sans nature : un tour.
	if _, out = post(`{"conv":"fp2","complete":true,"stats":{}}`); out["kind"] != perfMain {
		t.Fatalf("sans nature : %v", out)
	}
	if w := appelOAIInterne(t, "GET", "/internal/v1/turn/perf", ""); w.Code != 405 {
		t.Fatalf("GET : %d", w.Code)
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
