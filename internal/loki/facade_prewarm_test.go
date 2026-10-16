package loki

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// moteurSain : un llama-server de test qui répond à /health.
func moteurSain(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}
}

func prewarmPost(t *testing.T, path, body string) (int, map[string]any) {
	t.Helper()
	w := appelOAIInterne(t, "POST", path, body)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// Le moteur est-il libre d'un vrai travail, et sur quel slot viser ?
func TestFacadePrewarmCheck(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	check := func() map[string]any {
		_, out := prewarmPost(t, "/internal/v1/prewarm/check", `{}`)
		return out
	}
	if out := check(); out["ok"] != false || out["why"] != "clé absente" {
		t.Fatalf("sans la clé : %v", out)
	}
	if err := SetConfigKey("PREWARM", "on"); err != nil {
		t.Fatal(err)
	}
	moteurSain(t)
	prevSlots, prevNow := prewarmSlots, prewarmNow
	t.Cleanup(func() { prewarmSlots, prewarmNow = prevSlots, prevNow })
	prewarmSlots = func(chatEndpoint) int { return 1 }
	prewarmNow = func() time.Time { return time.Date(2026, 10, 16, 14, 0, 0, 0, time.Local) }
	if out := check(); out["ok"] != true || out["slot"] != float64(-1) {
		t.Fatalf("un seul slot : %v", out)
	}
	prewarmSlots = func(chatEndpoint) int { return 4 }
	if out := check(); out["ok"] != false || out["why"] != "plusieurs slots" {
		t.Fatalf("quatre slots : %v", out)
	}
	prewarmSlots = func(chatEndpoint) int { return 1 }
	prewarmNow = func() time.Time { return time.Date(2026, 10, 16, 23, 55, 0, 0, time.Local) }
	if out := check(); out["why"] != "minuit proche" {
		t.Fatalf("minuit : %v", out)
	}
	prewarmNow = func() time.Time { return time.Date(2026, 10, 16, 14, 0, 0, 0, time.Local) }
	sideJobs.Add(1)
	out := check()
	sideJobs.Add(-1)
	if out["why"] != "moteur occupé" {
		t.Fatalf("travail annexe : %v", out)
	}
	if err := SetConfigKey("PORT", "1"); err != nil {
		t.Fatal(err)
	}
	if out := check(); out["why"] != "moteur indisponible" {
		t.Fatalf("moteur muet : %v", out)
	}
}

// Inscrit comme LA requête en vol ; annulé par toute autre requête (la
// façade l'apprend), sauf un tour qui le prolonge ; rendu à la fin.
func TestFacadePrewarmEnVol(t *testing.T) {
	testHome(t)
	f := demarrerFacade(t)
	begin := func() (bool, float64) {
		_, out := prewarmPost(t, "/internal/v1/prewarm/begin", `{"conv":"c1","n":3,"prefix":11,"opts":22}`)
		id, _ := out["id"].(float64)
		return out["ok"] == true, id
	}
	ok, id := begin()
	if !ok || id == 0 || !prewarmLive.Load() || enVol() != 1 {
		t.Fatalf("inscription : ok=%v id=%v en vol=%d", ok, id, enVol())
	}
	if ok, _ := begin(); ok {
		t.Fatal("deux préchauffages en vol")
	}
	// Un tour de la façade qui le prolonge : gardé.
	w := appelOAIInterne(t, "POST", "/internal/v1/oai/begin", `{"main_conv":"c1","prewarm_keep":true}`)
	var tk struct {
		Ticket uint64 `json:"ticket"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &tk)
	if !prewarmLive.Load() {
		t.Fatal("préchauffage annulé par le tour qui le prolonge")
	}
	appelOAIInterne(t, "POST", "/internal/v1/oai/end", fmt.Sprintf(`{"ticket":%d}`, tk.Ticket))
	// Toute autre requête : annulé, la façade coupe sa connexion.
	w = appelOAIInterne(t, "POST", "/internal/v1/oai/begin", `{"main_conv":"c1"}`)
	_ = json.Unmarshal(w.Body.Bytes(), &tk)
	if prewarmLive.Load() {
		t.Fatal("préchauffage gardé malgré une autre requête")
	}
	deadline := time.Now().Add(2 * time.Second)
	want := fmt.Sprintf(`POST /internal/facade/v1/chat/prewarm/cancel jeton {"id":%d}`, int(id))
	for !recuContient(f.recu(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("annulation non relayée : %v", f.recu())
		}
		time.Sleep(10 * time.Millisecond)
	}
	appelOAIInterne(t, "POST", "/internal/v1/oai/end", fmt.Sprintf(`{"ticket":%d}`, tk.Ticket))
	// La fin rend la place, une seule fois.
	if code, _ := prewarmPost(t, "/internal/v1/prewarm/end", fmt.Sprintf(`{"id":%d,"total":900,"cached":800}`, int(id))); code != 204 {
		t.Fatalf("fin : %d", code)
	}
	if enVol() != 0 {
		t.Fatalf("encore %d requête(s) en vol", enVol())
	}
	if code, _ := prewarmPost(t, "/internal/v1/prewarm/end", fmt.Sprintf(`{"id":%d}`, int(id))); code != 404 {
		t.Fatalf("fin répétée : %d", code)
	}
	// Une requête déjà en vol : rien ne part.
	w = appelOAIInterne(t, "POST", "/internal/v1/oai/begin", `{"main_conv":"c1"}`)
	_ = json.Unmarshal(w.Body.Bytes(), &tk)
	if ok, _ := begin(); ok {
		t.Fatal("préchauffage inscrit malgré une requête en vol")
	}
	appelOAIInterne(t, "POST", "/internal/v1/oai/end", fmt.Sprintf(`{"ticket":%d}`, tk.Ticket))
}

// Après une tâche planifiée, le déclencheur part à la façade.
func TestFacadePrewarmApresTache(t *testing.T) {
	testHome(t)
	f := demarrerFacade(t)
	if err := SetConfigKey("PREWARM", "on"); err != nil {
		t.Fatal(err)
	}
	conv.prewarmKick(prewarmAfterTask)
	deadline := time.Now().Add(2 * time.Second)
	want := `POST /internal/facade/v1/chat/prewarm jeton {"why":"après-tâche"}`
	for !recuContient(f.recu(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("déclencheur non relayé : %v", f.recu())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func recuContient(l []string, s string) bool {
	for _, x := range l {
		if strings.TrimSpace(x) == s {
			return true
		}
	}
	return false
}
