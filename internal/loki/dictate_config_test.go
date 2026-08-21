package loki

import (
	"testing"
	"time"
)

func TestDictateCfgDefauts(t *testing.T) {
	c := DictateCfg{}.withDefauts()
	if c.Model != "large-v3-turbo-q5_0" {
		t.Errorf("modèle par défaut = %q, attendu large-v3-turbo-q5_0", c.Model)
	}
	if c.Lang != "fr" {
		t.Errorf("langue par défaut = %q, attendu fr — la détection auto se trompe sur les tranches courtes", c.Lang)
	}
	if c.Device != "cpu" {
		t.Errorf("matériel par défaut = %q, attendu cpu (aucun GPU supposé)", c.Device)
	}
	if c.Reactivity != "moyen" {
		t.Errorf("réactivité par défaut = %q, attendu moyen", c.Reactivity)
	}
}

func TestCudaVisibleDevices(t *testing.T) {
	cas := []struct{ device, want string }{
		{"cpu", ""},
		{"0", "0"},
		{"1", "1"},
		{"", ""},
		{"bidon", ""},
		{"-1", ""},
	}
	for _, c := range cas {
		got := DictateCfg{Device: c.device}.cudaVisibleDevices()
		if got != c.want {
			t.Errorf("cudaVisibleDevices(%q) = %q, attendu %q", c.device, got, c.want)
		}
	}
}

func TestChunkBounds(t *testing.T) {
	cas := []struct {
		react    string
		min, max time.Duration
	}{
		{"court", 1000 * time.Millisecond, 5 * time.Second},
		{"moyen", 1500 * time.Millisecond, 8 * time.Second},
		{"long", 3 * time.Second, 15 * time.Second},
		{"inconnu", 1500 * time.Millisecond, 8 * time.Second},
	}
	for _, c := range cas {
		min, max := DictateCfg{Reactivity: c.react}.chunkBounds()
		if min != c.min || max != c.max {
			t.Errorf("chunkBounds(%q) = %v/%v, attendu %v/%v", c.react, min, max, c.min, c.max)
		}
	}
}

func TestDictateCfgAllerRetour(t *testing.T) {
	testHome(t)
	in := DictateCfg{Model: "medium-q5_0", Lang: "en", Device: "1", Reactivity: "long"}
	if err := dictateCfgSave(in); err != nil {
		t.Fatalf("enregistrement : %v", err)
	}
	if out := dictateCfgLoad(); out != in {
		t.Errorf("relu %+v, attendu %+v", out, in)
	}
}

// Un modèle hors catalogue accepté en silence ne se manifesterait qu'au premier
// clic sur le micro, loin du geste qui l'a causé.
func TestDictateCfgSaveRefuseModeleInconnu(t *testing.T) {
	testHome(t)
	if err := dictateCfgSave(DictateCfg{Model: "modele-inexistant", Lang: "fr", Device: "cpu", Reactivity: "moyen"}); err == nil {
		t.Error("un modèle hors catalogue doit être refusé")
	}
}

// Sans réglage enregistré, la lecture doit rendre les défauts — et non le zéro
// de la structure, qui donnerait un modèle vide à whisper-server.
func TestDictateCfgLoadSansRien(t *testing.T) {
	testHome(t)
	c := dictateCfgLoad()
	if c.Model == "" || c.Lang == "" || c.Device == "" || c.Reactivity == "" {
		t.Errorf("lecture à vide = %+v, aucun champ ne doit rester vide", c)
	}
}
