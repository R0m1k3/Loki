package loki

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Un paquet exporté doit se rouvrir avec la clé de pilotage comme avec la clé de
// récupération, et rendre la mémoire à l'identique. C'est le scénario « je
// remonte mon conteneur ailleurs » : rien d'autre que le fichier et la clé.
func TestBackupBundleRoundTrip(t *testing.T) {
	testHome(t)
	clearMemDEK()
	if err := MemAdd("notes.md", "# Notes\nsecret du client\n"); err != nil {
		t.Fatal(err)
	}
	recovery, err := EnableMemEncryption("clé-de-pilotage")
	if err != nil {
		t.Fatal(err)
	}

	v, err := loadVault()
	if err != nil || v == nil {
		t.Fatalf("coffre introuvable après activation: %v", err)
	}
	dek, err := currentDEK()
	if err != nil {
		t.Fatal(err)
	}
	tarData, err := buildBundleTar()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := buildBackupBlob(v, dek, tarData)
	if err != nil {
		t.Fatal(err)
	}
	// Le paquet ne doit rien laisser voir en clair.
	if strings.Contains(string(blob), "secret du client") {
		t.Fatal("le contenu d'une page apparaît en clair dans le paquet")
	}

	// La clé de pilotage l'ouvre…
	if _, err := openBackupBlob(blob, "clé-de-pilotage"); err != nil {
		t.Fatalf("ouverture avec la clé de pilotage: %v", err)
	}
	// …la clé de récupération aussi…
	back, err := openBackupBlob(blob, recovery)
	if err != nil {
		t.Fatalf("ouverture avec la clé de récupération: %v", err)
	}
	// …et une mauvaise clé, non.
	if _, err := openBackupBlob(blob, "pas la bonne"); err == nil {
		t.Fatal("une clé fausse aurait dû être refusée")
	}

	// Page effacée puis restaurée depuis le paquet : contenu intact.
	p, _ := safeMemPath("notes.md")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := restoreBundleTar(back); err != nil {
		t.Fatal(err)
	}
	got, ok := memPageText("notes.md")
	if !ok || !strings.Contains(got, "secret du client") {
		t.Fatalf("page non restaurée: %q (lisible=%v)", got, ok)
	}
	// Le snapshot de sécurité pris avant restauration doit exister.
	if entries, err := os.ReadDir(filepath.Join(LokiHome(), "backups", "memory")); err != nil || len(entries) == 0 {
		t.Fatal("aucun snapshot pris avant la restauration")
	}
}
