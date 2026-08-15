package loki

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Le dossier de captures ne doit pas croître sans fin — mais la capture qui
// vient d'être prise doit survivre au ménage, même seule et plus lourde que le
// plafond : sinon le modèle renvoie à l'utilisateur un lien vers un fichier que
// pruneCaptures a effacé dans la foulée.
func TestPruneCapturesGardeLaDerniere(t *testing.T) {
	dir := t.TempDir()

	// 25 captures d'âges croissants, au-delà du plafond en NOMBRE.
	var last string
	for i := 0; i < 25; i++ {
		p := filepath.Join(dir, fmt.Sprintf("shot-%02d.jpg", i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		// mtime croissant : shot-00 est la plus ancienne.
		mt := time.Now().Add(time.Duration(i-25) * time.Minute)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		last = p
	}
	pruneCaptures(dir, last)

	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != maxCaptureFiles {
		t.Fatalf("%d fichiers restants, attendu %d", len(ents), maxCaptureFiles)
	}
	if _, err := os.Stat(last); err != nil {
		t.Fatalf("la capture la plus récente a été supprimée : %v", err)
	}
	// Les plus anciennes doivent être parties, pas les récentes.
	if _, err := os.Stat(filepath.Join(dir, "shot-00.jpg")); err == nil {
		t.Fatal("la capture la plus ancienne aurait dû être supprimée")
	}
}

// Supprimer une discussion doit emporter ses captures : sans ça, des images que
// plus aucun message n'affiche restent sur le disque pour toujours. Et elle ne
// doit emporter QUE les siennes.
func TestConvDeleteSupprimeLesCaptures(t *testing.T) {
	t.Setenv("LOKI_HOME", t.TempDir())

	a := convEnsureActive()
	b := convNew() // b devient active, a reste

	shot := func(id string) string {
		dir, _ := captureDirFor(id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "vue.jpg")
		if err := os.WriteFile(p, []byte("jpeg"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	shotA, shotB := shot(a), shot(b)

	if err := convDelete(a); err != nil {
		t.Fatalf("suppression de la discussion : %v", err)
	}
	if _, err := os.Stat(shotA); err == nil {
		t.Fatal("la capture de la discussion supprimée est toujours là")
	}
	if _, err := os.Stat(shotB); err != nil {
		t.Fatalf("la capture d'une AUTRE discussion a été supprimée : %v", err)
	}
}

// Une capture unique plus lourde que le plafond en OCTETS ne doit pas s'effacer
// elle-même : il ne resterait alors rien à montrer.
func TestPruneCapturesNEffacePasUneCaptureUniqueTropLourde(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "enorme.jpg")
	if err := os.WriteFile(p, make([]byte, maxCaptureBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	pruneCaptures(dir, p)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("la capture courante a été supprimée : %v", err)
	}
}
