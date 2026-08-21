package loki

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogueContientLeDefautEtLAncien(t *testing.T) {
	// Le défaut doit exister, sinon dictateCfgSave refuse sa propre valeur par
	// défaut et plus aucun réglage n'est enregistrable.
	if _, ok := whisperCatalogue["large-v3-turbo-q5_0"]; !ok {
		t.Error("le modèle par défaut large-v3-turbo-q5_0 est absent du catalogue")
	}
	// small-q5_1 est le modèle déjà téléchargé sur les installations
	// existantes : le retirer le rendrait insélectionnable alors que son
	// fichier est là.
	if _, ok := whisperCatalogue["small-q5_1"]; !ok {
		t.Error("small-q5_1 est absent : les installations existantes l'ont déjà sur disque")
	}
}

func TestModelPathEtURL(t *testing.T) {
	testHome(t)
	p := whisperModelPathFor("small-q5_1")
	if filepath.Base(p) != "ggml-small-q5_1.bin" {
		t.Errorf("chemin = %q, le fichier doit s'appeler ggml-small-q5_1.bin", p)
	}
	// Le chemin DOIT rester celui d'avant, sinon les installations existantes
	// re-téléchargent 190 Mo pour rien.
	if filepath.Base(filepath.Dir(p)) != "whisper" {
		t.Errorf("chemin = %q, le dossier doit rester <LOKI_HOME>/whisper", p)
	}
	u := whisperModelURLFor("small-q5_1")
	if !strings.HasSuffix(u, "/ggml-small-q5_1.bin") {
		t.Errorf("URL = %q, doit finir par /ggml-small-q5_1.bin", u)
	}
	if !strings.HasPrefix(u, "https://") {
		t.Errorf("URL = %q, doit être en HTTPS", u)
	}
}

func TestModelPathInconnuVide(t *testing.T) {
	testHome(t)
	if p := whisperModelPathFor("nawak"); p != "" {
		t.Errorf("un modèle hors catalogue doit donner un chemin vide, pas %q", p)
	}
	if u := whisperModelURLFor("nawak"); u != "" {
		t.Errorf("URL d'un modèle inconnu = %q, attendu vide", u)
	}
	if whisperModelPresent("nawak") {
		t.Error("un modèle inconnu ne peut pas être présent")
	}
}

func TestCatalogueTrieParTaille(t *testing.T) {
	testHome(t)
	l := whisperCatalogueTrie()
	if len(l) < 4 {
		t.Fatalf("catalogue de %d entrées, au moins 4 attendues", len(l))
	}
	var prec int64
	for i, m := range l {
		o, _ := m["octets"].(int64)
		if i > 0 && o < prec {
			t.Errorf("entrée %d : catalogue non trié par taille croissante", i)
		}
		prec = o
		for _, clef := range []string{"id", "nom", "octets", "vram_mo", "present"} {
			if _, ok := m[clef]; !ok {
				t.Errorf("entrée %d : clé %q manquante — l'UI en a besoin", i, clef)
			}
		}
	}
}
