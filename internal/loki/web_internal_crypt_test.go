package loki

import (
	"bytes"
	"testing"
)

// Sceller puis ouvrir par l'API interne rend le clair ; la clé ne sort pas.
func TestCryptInterneAllerRetour(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)

	// Chiffrement coupé : scellé = clair, et un clair s'ouvre tel quel.
	w := appelOAIInterne(t, "POST", "/internal/v1/crypt/seal", "bonjour")
	if w.Code != 200 || w.Body.String() != "bonjour" {
		t.Fatalf("seal sans chiffrement : %d %q", w.Code, w.Body.String())
	}
	if w := appelOAIInterne(t, "POST", "/internal/v1/crypt/open", "bonjour"); w.Body.String() != "bonjour" {
		t.Fatalf("open d'un clair : %q", w.Body.String())
	}

	// Chiffrement actif, mémoire verrouillée : 423, jamais d'écriture en clair.
	if err := putStr(bkConfig, "MEM_ENCRYPTED", "1"); err != nil {
		t.Fatal(err)
	}
	clearMemDEK()
	if w := appelOAIInterne(t, "POST", "/internal/v1/crypt/seal", "secret"); w.Code != 423 {
		t.Fatalf("seal verrouillé : %d", w.Code)
	}

	// Déverrouillée : scellé ≠ clair, ouverture = clair.
	dek, _ := randBytes(memDEKLen)
	setMemDEK(dek)
	t.Cleanup(clearMemDEK)
	w = appelOAIInterne(t, "POST", "/internal/v1/crypt/seal", "secret")
	sealed := w.Body.Bytes()
	if w.Code != 200 || bytes.Contains(sealed, []byte("secret")) || !looksEncrypted(sealed) {
		t.Fatalf("seal : %d %q", w.Code, sealed)
	}
	w = appelOAIInterne(t, "POST", "/internal/v1/crypt/open", string(sealed))
	if w.Code != 200 || w.Body.String() != "secret" {
		t.Fatalf("open : %d %q", w.Code, w.Body.String())
	}

	// Reverrouillée : un scellé ne s'ouvre plus.
	clearMemDEK()
	if w := appelOAIInterne(t, "POST", "/internal/v1/crypt/open", string(sealed)); w.Code != 423 {
		t.Fatalf("open verrouillé : %d", w.Code)
	}
}
