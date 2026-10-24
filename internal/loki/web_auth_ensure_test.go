package loki

import (
	"strings"
	"testing"
)

// Sans clé (installation neuve, défaut), le démarrage en génère une et la
// range là où l'API la lit : /api/* n'est plus jamais ouvert par défaut.
func TestEnsureWebKeyGeneratesWhenMissing(t *testing.T) {
	testHome(t)
	gen, err := ensureWebKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gen, "loki-web-") || len(gen) < 40 {
		t.Fatalf("clé générée = %q", gen)
	}
	if k, err := readWebKeyErr(); err != nil || k != gen {
		t.Fatalf("clé lue = %q, %v ; attendu %q", k, err, gen)
	}
	// Second démarrage : rien de nouveau, la clé reste.
	if again, err := ensureWebKey(); err != nil || again != "" {
		t.Fatalf("second appel = %q, %v ; attendu rien", again, err)
	}
	if k, _ := readWebKeyErr(); k != gen {
		t.Fatalf("clé modifiée au second démarrage : %q", k)
	}
}

// Une clé déjà définie par le propriétaire n'est jamais remplacée.
func TestEnsureWebKeyKeepsExistingKey(t *testing.T) {
	testHome(t)
	if err := putStr(bkState, "web_key", "ma-cle"); err != nil {
		t.Fatal(err)
	}
	if gen, err := ensureWebKey(); err != nil || gen != "" {
		t.Fatalf("ensureWebKey = %q, %v", gen, err)
	}
	if k, _ := readWebKeyErr(); k != "ma-cle" {
		t.Fatalf("clé = %q", k)
	}
}

// Un retrait explicite (loki set-web-key off) est respecté ; redéfinir une clé
// annule ce choix.
func TestEnsureWebKeyRespectsExplicitRemoval(t *testing.T) {
	testHome(t)
	if err := cmdSetWebKey([]string{"off"}); err != nil {
		t.Fatal(err)
	}
	if gen, err := ensureWebKey(); err != nil || gen != "" {
		t.Fatalf("ensureWebKey après retrait = %q, %v", gen, err)
	}
	if err := cmdSetWebKey([]string{"autre"}); err != nil {
		t.Fatal(err)
	}
	if getStr(bkState, webKeyOpenFlag) == "1" {
		t.Fatal("le retrait explicite aurait dû être annulé")
	}
}
