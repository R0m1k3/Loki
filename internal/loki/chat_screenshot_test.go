package loki

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// Le modèle annonçait « je ne vois pas l'image » alors que la vision était
// activée : sa description d'outil le lui disait, et la capture ne lui était
// jamais transmise. Les deux doivent suivre l'état réel du projecteur.
func TestScreenshotSuitLEtatDeLaVision(t *testing.T) {
	t.Setenv("LOKI_HOME", t.TempDir())

	if got := screenshotVisionNote(); !strings.Contains(got, "ne vois pas") {
		t.Fatalf("sans projecteur, la description doit annoncer l'absence de vision : %q", got)
	}
	// Une capture existe, mais sans projecteur elle ne doit PAS partir au modèle :
	// llama-server rejette un contenu image sans --mmproj.
	dir, rel := captureDirFor(convEnsureActive())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	shot := filepath.Join(dir, "vue.jpg")
	if err := os.WriteFile(shot, []byte("\xff\xd8\xff jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	relPath := rel + "/vue.jpg"
	if _, ok := screenshotImageMessage(relPath); ok {
		t.Fatal("image transmise au modèle alors qu'aucun projecteur n'est configuré")
	}

	// Projecteur configuré → description ET transmission changent.
	if err := SetConfigKey("MMPROJ", "mmproj-test.gguf"); err != nil {
		t.Fatal(err)
	}
	if got := screenshotVisionNote(); strings.Contains(got, "ne vois pas") {
		t.Fatalf("avec projecteur, la description ne doit plus nier la vision : %q", got)
	}
	msg, ok := screenshotImageMessage(relPath)
	if !ok {
		t.Fatal("image non transmise alors que le projecteur est configuré")
	}
	if msg.Role != "user" {
		t.Fatalf("role %q : l'image doit voyager dans un message user, un message tool ne porte que du texte", msg.Role)
	}
	parts, _ := msg.Content.([]map[string]any)
	if len(parts) != 2 || parts[1]["type"] != "image_url" {
		t.Fatalf("contenu multimodal attendu (text + image_url), obtenu %#v", msg.Content)
	}
}

// Le chemin de la capture est extrait du texte rendu par l'outil : si le format
// de ce texte change, le relais vers le modèle casse en silence.
func TestCapturedRelPath(t *testing.T) {
	res := "Capture enregistrée (captures/c1/x.jpg, 42 Ko).\n" +
		"Pour la montrer à l'utilisateur, recopie TELLE QUELLE cette ligne markdown dans ta réponse :\n" +
		"![capture de exemple.com](/api/chat/image?path=captures/c1/x.jpg)"
	if got := capturedRelPath(res); got != "captures/c1/x.jpg" {
		t.Fatalf("chemin extrait %q", got)
	}
	if got := capturedRelPath("[erreur] capture impossible"); got != "" {
		t.Fatalf("une erreur ne doit produire aucun chemin, obtenu %q", got)
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
