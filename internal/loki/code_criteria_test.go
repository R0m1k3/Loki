package loki

import (
	"strings"
	"testing"
)

// Cycle de vie des critères : add → set → clear, avec la garde allowPass.
func TestCriteresCycleDeVie(t *testing.T) {
	withWorkspace(t)
	id := convEnsureActive()

	out := toolCriteria(map[string]any{"action": "add", "texts": []any{"le test passe", "le build compile"}}, false)
	if !strings.Contains(out, "2 critère(s)") {
		t.Fatalf("ajout raté : %s", out)
	}
	list := critList(id)
	if len(list) != 2 || list[0].ID != 1 || list[1].ID != 2 {
		t.Fatalf("liste inattendue : %+v", list)
	}
	// Le builder (allowPass=false) ne peut PAS s'auto-valider.
	out = toolCriteria(map[string]any{"action": "set", "id": float64(1), "status": "passed"}, false)
	if !strings.Contains(out, "[refusé]") {
		t.Fatalf("auto-validation acceptée : %s", out)
	}
	// Le vérificateur (allowPass=true), si.
	out = toolCriteria(map[string]any{"action": "set", "id": float64(1), "status": "passed"}, true)
	if !strings.Contains(out, "passed") || strings.Contains(out, "[refusé]") {
		t.Fatalf("validation vérificateur refusée : %s", out)
	}
	// failed reste ouvert à tous (le builder peut constater un échec).
	out = toolCriteria(map[string]any{"action": "set", "id": float64(2), "status": "failed", "note": "le build casse"}, false)
	if !strings.Contains(out, "failed") {
		t.Fatalf("échec non enregistré : %s", out)
	}
	list = critList(id)
	if critAllPassed(list) {
		t.Fatal("allPassed avec un failed")
	}
	if critPending(list) != 1 {
		t.Fatalf("pending = %d, attendu 1", critPending(list))
	}
	// Rendu : les statuts sont visibles.
	r := critRender(list)
	if !strings.Contains(r, "[✓] #1") || !strings.Contains(r, "[✗] #2") || !strings.Contains(r, "le build casse") {
		t.Fatalf("rendu incomplet :\n%s", r)
	}
	toolCriteria(map[string]any{"action": "clear"}, false)
	if len(critList(id)) != 0 {
		t.Fatal("clear sans effet")
	}
}

// Une liste vide n'est pas un contrat rempli.
func TestContratVide(t *testing.T) {
	if critAllPassed(nil) {
		t.Fatal("liste vide considérée comme remplie")
	}
}

// Le plafond de critères tient.
func TestCriteresPlafond(t *testing.T) {
	withWorkspace(t)
	id := convEnsureActive()
	var texts []any
	for i := 0; i < critMaxCount+10; i++ {
		texts = append(texts, "critère")
	}
	toolCriteria(map[string]any{"action": "add", "texts": texts}, false)
	if n := len(critList(id)); n != critMaxCount {
		t.Fatalf("%d critères, plafond %d", n, critMaxCount)
	}
}
