package loki

import (
	"os"
	"testing"
)

// La case « chiffré » (health.Fully) ne doit être vraie QUE si TOUT est chiffré.
func TestFullyReflectsRealState(t *testing.T) {
	testHome(t)
	clearMemDEK()
	if err := MemAdd("a.md", "# A\n"); err != nil {
		t.Fatal(err)
	}
	_ = putBytes(bkChat, convKey("c1"), []byte(`{"id":"c1","log":[]}`))
	if _, err := EnableMemEncryption("pw"); err != nil {
		t.Fatal(err)
	}
	if !memHealth().Fully {
		t.Fatal("après chiffrement complet, Fully devrait être vrai")
	}
	// Une page en clair réapparue → Fully faux.
	p, _ := safeMemPath("a.md")
	if err := os.WriteFile(p, []byte("# clair"), 0o600); err != nil {
		t.Fatal(err)
	}
	if memHealth().Fully {
		t.Fatal("une page en clair aurait dû rendre Fully faux")
	}
	// Remets la page chiffrée, salis une discussion → Fully faux aussi.
	if err := writeMemFile("a.md", []byte("# A\n")); err != nil {
		t.Fatal(err)
	}
	_ = putBytes(bkChat, convKey("c2"), []byte("journal en clair"))
	if memHealth().Fully {
		t.Fatal("une discussion en clair aurait dû rendre Fully faux")
	}
}

// Les discussions (journal + index) et les blocs archivés doivent être chiffrés
// à l'activation, illisibles verrouillé, et intacts après déchiffrement.
func TestConversationEncryptionLifecycle(t *testing.T) {
	testHome(t)
	clearMemDEK()

	// Une discussion avec son journal et son entrée d'index.
	_ = putStoreBytes(bkChat, convKey("c1"), []byte(`{"id":"c1","log":[{"seq":1}],"messages":[]}`))
	convIndexSave([]convMeta{{ID: "c1", Title: "Client confidentiel", Created: 1, Updated: 2}})
	// Une page mémoire, pour activer le chiffrement (il chiffre tout).
	if err := MemAdd("note.md", "# Note\n"); err != nil {
		t.Fatal(err)
	}

	if _, err := EnableMemEncryption("pw"); err != nil {
		t.Fatal(err)
	}

	// Sur disque, journal ET index doivent porter le magic.
	if raw := getBytes(bkChat, convKey("c1")); !looksEncrypted(raw) {
		t.Fatal("journal de discussion non chiffré sur disque")
	}
	if raw := getBytes(bkChat, ckIndex); !looksEncrypted(raw) {
		t.Fatal("index des discussions non chiffré sur disque")
	}

	// Déverrouillé : lisible.
	if idx := convIndex(); len(idx) != 1 || idx[0].Title != "Client confidentiel" {
		t.Fatalf("index illisible alors que déverrouillé: %+v", idx)
	}

	// Verrouillé : plus rien de lisible, mais rien de perdu.
	clearMemDEK()
	if idx := convIndex(); len(idx) != 0 {
		t.Fatal("les discussions devraient être masquées quand verrouillé")
	}
	if b := storeBytes(bkChat, convKey("c1")); len(b) != 0 {
		t.Fatal("le journal devrait être illisible quand verrouillé")
	}
	// putStoreBytes doit REFUSER d'écrire du clair quand verrouillé.
	if err := putStoreBytes(bkChat, convKey("c1"), []byte("clair")); err != errStoreLocked {
		t.Fatalf("écriture clair aurait dû être refusée, obtenu %v", err)
	}
	if raw := getBytes(bkChat, convKey("c1")); !looksEncrypted(raw) {
		t.Fatal("le blob chiffré a été écrasé alors que verrouillé")
	}

	// Re-déverrouille et déchiffre : tout revient en clair et intact.
	v, _ := loadVault()
	dek, _, err := v.unlockWith("pw")
	if err != nil {
		t.Fatal(err)
	}
	setMemDEK(dek)
	if err := DisableMemEncryption(); err != nil {
		t.Fatal(err)
	}
	if raw := getBytes(bkChat, convKey("c1")); looksEncrypted(raw) {
		t.Fatal("journal encore chiffré après déchiffrement")
	}
	if idx := convIndex(); len(idx) != 1 || idx[0].Title != "Client confidentiel" {
		t.Fatal("index perdu après déchiffrement")
	}
}

// Un bloc archivé au compactage est du verbatim de conversation : chiffré comme
// le reste, et hors de portée tant que la mémoire est verrouillée.
func TestRecallBlocksFollowEncryption(t *testing.T) {
	testHome(t)
	clearMemDEK()
	if err := MemAdd("note.md", "# Note\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := EnableMemEncryption("pw"); err != nil {
		t.Fatal(err)
	}
	id, err := archiveRecallBlock("tool: lecture", "tool", "contenu très bavard")
	if err != nil {
		t.Fatal(err)
	}
	if blk, ok := recallGet(id); !ok || blk.Content != "contenu très bavard" {
		t.Fatal("bloc illisible alors que déverrouillé")
	}
	if hits := recallSearch("bavard", 5); len(hits) != 1 {
		t.Fatalf("la recherche devrait trouver le bloc déverrouillé, obtenu %d", len(hits))
	}
	clearMemDEK()
	if _, ok := recallGet(id); ok {
		t.Fatal("bloc lisible alors que verrouillé")
	}
	if hits := recallSearch("bavard", 5); len(hits) != 0 {
		t.Fatal("la recherche ne devrait rien trouver quand verrouillé")
	}
}
