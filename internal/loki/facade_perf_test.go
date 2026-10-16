package loki

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// La télémétrie des complétions de la façade rejoint l'anneau du Go : perte
// de cache calculée contre la complétion précédente de la même discussion,
// ce qui s'est intercalé, et l'alerte au seuil du preset actif.
func TestFacadePerf(t *testing.T) {
	testHome(t)
	post := func(body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		handleInternalPerfRecord(rec, httptest.NewRequest("POST", "/internal/v1/perf/record", strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	// Première complétion : rien à comparer. Ce que la façade enverrait en
	// trop (at, seq, lost) est ignoré.
	code, out := post(`{"rec":{"kind":"main","conv":"fp1","iter":0,"complete":true,"total":5000,"cached":0,"new":5000,
		"seq":99,"lost":7,"lost_after":"x"},"prefix":[11,22]}`)
	if code != 200 || out["lost"] != nil || out["lost_after"] != "" || out["alert"] != false {
		t.Fatalf("première : %d %v", code, out)
	}
	seq := out["seq"].(float64)
	if seq == 99 || seq <= 0 {
		t.Fatalf("seq : %v", seq)
	}
	// Une tâche passe entre deux.
	perfRecord(perfRec{Kind: perfTask, Conv: "t", Complete: true, Total: 10}, nil)
	// La suivante prolonge la première, mais n'en reprend que 100 jetons.
	code, out = post(`{"rec":{"kind":"main","conv":"fp1","iter":1,"complete":true,"total":6000,"cached":100,"new":5900},"prefix":[11,22,33]}`)
	if code != 200 || out["lost"] != float64(4900) || out["lost_after"] != perfTask || out["alert"] != true {
		t.Fatalf("perte : %d %v", code, out)
	}
	if recs := perfLog.snapshot(); recs[len(recs)-1].Conv != "fp1" || recs[len(recs)-1].Iter != 1 || recs[len(recs)-1].New != 5900 {
		t.Fatalf("relevé rangé : %+v", recs[len(recs)-1])
	}
	// Preset externe : la perte est rangée, jamais signalée.
	code, out = post(`{"rec":{"kind":"main","conv":"fp1","iter":2,"complete":true,"total":7000,"cached":100},"prefix":[11,22,33,44],"external":true}`)
	if code != 200 || out["lost"] != float64(5900) || out["alert"] != false {
		t.Fatalf("externe : %d %v", code, out)
	}
	// Incomplète : ni perte ni alerte.
	code, out = post(`{"rec":{"kind":"main","conv":"fp1","iter":3,"complete":false,"cached":0}}`)
	if code != 200 || out["lost"] != nil || out["alert"] != false {
		t.Fatalf("incomplète : %d %v", code, out)
	}
	if code, _ := post(`{"rec":`); code != 400 {
		t.Fatalf("corps illisible : %d", code)
	}
	rec := httptest.NewRecorder()
	handleInternalPerfRecord(rec, httptest.NewRequest("GET", "/internal/v1/perf/record", nil))
	if rec.Code != 405 {
		t.Fatalf("GET : %d", rec.Code)
	}
}
