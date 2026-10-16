package loki

import "testing"

// Après une compaction en cours de tour qui n'a rien réduit, l'étape suivante
// ne relance pas un résumé tant que le contexte n'a pas grossi de 5 % de la
// fenêtre — sauf à 90 % de la fenêtre ou plus.
func TestCompactRetryBackoff(t *testing.T) {
	testHome(t)
	if err := putStr(bkConfig, "CTX", "100000"); err != nil {
		t.Fatal(err)
	}
	if !compactRetryDue(0, 70000, nil) {
		t.Fatal("sans échec précédent, l'essai doit être permis")
	}
	next := compactNextRetry(false, 70000, nil)
	if next != 75000 {
		t.Fatalf("prochain essai à %d, attendu 75000", next)
	}
	if compactRetryDue(next, 72000, nil) {
		t.Fatal("relancé alors que le contexte n'a presque pas grossi")
	}
	if !compactRetryDue(next, 75000, nil) {
		t.Fatal("pas relancé alors que le contexte a grossi de 5 % de la fenêtre")
	}
	if !compactRetryDue(200000, 90000, nil) {
		t.Fatal("à 90 % de la fenêtre, l'essai doit toujours être permis")
	}
	if compactNextRetry(true, 70000, nil) != 0 {
		t.Fatal("une compaction réussie doit lever le garde-fou")
	}
}
