package loki

// Vérification contre le VRAI registre. Ignoré par défaut (la CI ne doit pas
// dépendre du réseau ni télécharger 170 Mo) :
//
//	LOKI_NET_TEST=1 go test ./internal/loki -run TestMoteurReelDepuisGhcr -v
//
// Ce test existe parce que le faux registre ne peut pas garantir une chose : que
// la DISPOSITION de l'image amont est toujours celle qu'on suppose. Si llama.cpp
// range un jour son moteur autrement — sous-dossier, couche gigantesque, autre
// jeu de bibliothèques — c'est ici qu'on le verra, et pas chez l'utilisateur.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMoteurReelDepuisGhcr(t *testing.T) {
	if os.Getenv("LOKI_NET_TEST") == "" {
		t.Skip("test réseau — LOKI_NET_TEST=1 pour l'exécuter")
	}
	if os.Getenv("LOKI_HOME") == "" {
		t.Setenv("LOKI_HOME", t.TempDir())
	}

	build, tag, err := engineOCILatest("server-cuda")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("dernier build publié : %d → %s", build, tag)
	if build == 0 {
		t.Error("numéro de build absent de l'annotation de l'image")
	}

	bin, err := engineOCIInstall(tag, func(s string) { t.Log(s) }, func(s string) { t.Log("▶ " + s) })
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(bin)
	// Ce que l'éditeur de liens réclamera au lancement. Un seul manquant et le
	// moteur meurt sur « cannot open shared object file ».
	for _, n := range []string{"libllama.so", "libggml.so", "libggml-base.so", "libggml-cuda.so", "libmtmd.so"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s : %v", n, err)
		}
	}
	// Et le binaire doit savoir dire sa version : c'est ce qui pilote la
	// comparaison « installé vs publié » de tout le panneau.
	if got, commit := engineBuildOf(bin); got != build {
		t.Errorf("le moteur installé annonce le build %d (%s), attendu %d", got, commit, build)
	}
}
