package loki

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func leaseJSON(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("réponse illisible %q : %v", body, err)
	}
	return m
}

// libérerVerrou rend le verrou de génération quoi qu'un test ait laissé.
func libérerVerrou(t *testing.T) {
	t.Cleanup(func() {
		conv.mu.Lock()
		conv.extLeaseClearLocked()
		conv.Generating, conv.benching, conv.cancel, conv.leaseWhy = false, false, nil, nil
		conv.mu.Unlock()
	})
}

// Le bail de la façade EST le verrou de génération : tant qu'il tient, une
// mesure (bench, optimiseur) ou un second bail est refusé.
func TestBailInterneVerrouPartage(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	libérerVerrou(t)

	w := appelOAIInterne(t, "POST", "/internal/v1/engine/lease", `{"ttl_ms":60000}`)
	if w.Code != 200 {
		t.Fatalf("bail : %d %s", w.Code, w.Body.String())
	}
	m := leaseJSON(t, w.Body.String())
	id, _ := m["lease"].(string)
	if len(id) != 32 || m["ttl_ms"] != float64(60000) {
		t.Fatalf("bail : %v", m)
	}
	if !conv.isGenerating() {
		t.Fatal("le bail ne tient pas le verrou de génération")
	}
	if _, err := conv.measureLease(func() {}, errBenchBusy); err != ErrBusy {
		t.Fatalf("bench accepté pendant le bail : %v", err)
	}
	if w := appelOAIInterne(t, "POST", "/internal/v1/engine/lease", ""); w.Code != 409 ||
		!strings.Contains(w.Body.String(), ErrBusy.Error()) {
		t.Fatalf("second bail : %d %s", w.Code, w.Body.String())
	}

	// Renouvelé ; le stop passé par le Go remonte à la façade.
	w = appelOAIInterne(t, "POST", "/internal/v1/engine/lease/renew", `{"lease":"`+id+`"}`)
	if w.Code != 200 || leaseJSON(t, w.Body.String())["stop"] != false {
		t.Fatalf("renouvellement : %d %s", w.Code, w.Body.String())
	}
	conv.Stop()
	w = appelOAIInterne(t, "POST", "/internal/v1/engine/lease/renew", `{"lease":"`+id+`"}`)
	if w.Code != 200 || leaseJSON(t, w.Body.String())["stop"] != true {
		t.Fatalf("stop non transmis : %d %s", w.Code, w.Body.String())
	}
	if w := appelOAIInterne(t, "POST", "/internal/v1/engine/lease/renew", `{"lease":"autre"}`); w.Code != 410 {
		t.Fatalf("bail inconnu renouvelé : %d", w.Code)
	}

	// Rendu : le verrou est libre, le bail est mort, rendre deux fois est sans effet.
	for i := 0; i < 2; i++ {
		if w := appelOAIInterne(t, "POST", "/internal/v1/engine/lease/release", `{"lease":"`+id+`"}`); w.Code != 204 {
			t.Fatalf("libération : %d", w.Code)
		}
	}
	if conv.isGenerating() {
		t.Fatal("verrou toujours tenu après la libération")
	}
	if w := appelOAIInterne(t, "POST", "/internal/v1/engine/lease/renew", `{"lease":"`+id+`"}`); w.Code != 410 {
		t.Fatalf("bail rendu renouvelé : %d", w.Code)
	}
}

// Un bail rendu après coup ne libère pas le verrou d'un autre détenteur.
func TestBailInterneNeLibereQueLeSien(t *testing.T) {
	testHome(t)
	libérerVerrou(t)
	id, err := conv.extLeaseAcquire(time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	conv.Reset() // rompt le bail, comme un tour Go
	if _, ok := conv.extLeaseRenew(id); ok {
		t.Fatal("bail encore valide après Reset")
	}
	release, err := conv.measureLease(func() {}, errBenchBusy)
	if err != nil {
		t.Fatalf("verrou non rendu par Reset : %v", err)
	}
	defer release()
	conv.extLeaseRelease(id)
	if !conv.isGenerating() {
		t.Fatal("un bail périmé a libéré le verrou du bench")
	}
}

// Une façade tuée en plein tour ne garde pas le moteur : sans renouvellement,
// le bail expire et rend le verrou.
func TestBailInterneExpire(t *testing.T) {
	testHome(t)
	libérerVerrou(t)
	id, err := conv.extLeaseAcquire(50*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for conv.isGenerating() {
		if time.Now().After(deadline) {
			t.Fatal("bail jamais expiré")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := conv.extLeaseRenew(id); ok {
		t.Fatal("bail expiré encore renouvelable")
	}
}

// Moteur tenu par le Go : le refus dit qui le tient, et /engine/stop l'arrête.
func TestBailInterneRefusEtStop(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	libérerVerrou(t)
	ctx, cancel := context.WithCancel(context.Background())
	release, err := conv.measureLease(cancel, errBenchBusy)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	w := appelOAIInterne(t, "POST", "/internal/v1/engine/lease", "")
	if w.Code != 409 || leaseJSON(t, w.Body.String())["error"] != errBenchBusy.Error() {
		t.Fatalf("refus : %d %s", w.Code, w.Body.String())
	}
	if w := appelOAIInterne(t, "POST", "/internal/v1/engine/stop", ""); w.Code != 204 {
		t.Fatalf("stop : %d", w.Code)
	}
	if ctx.Err() == nil {
		t.Fatal("le bench n'a pas été arrêté")
	}
	if w := appelOAIInterne(t, "GET", "/internal/v1/engine/stop", ""); w.Code != 405 {
		t.Fatalf("GET accepté : %d", w.Code)
	}
}
