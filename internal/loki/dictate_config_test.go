package loki

import (
	"testing"
)

// Les défauts sont appliqués à la LECTURE : un réglage enregistré par une
// version antérieure, sans le champ, doit être comblé plutôt que de produire un
// modèle vide passé en argument au serveur.
func TestDefautsCombles(t *testing.T) {
	testHome(t)
	c := DictateCfg{}.withDefauts()
	if _, ok := asrCatalogue[c.Model]; !ok {
		t.Errorf("modèle par défaut %q hors catalogue", c.Model)
	}
	if c.Reactivity == "" {
		t.Error("réactivité par défaut vide")
	}
}

// Le passage de whisper à Parakeet laisse des réglages enregistrés dont le
// Model ne veut plus rien dire (« large-v3-turbo-q5_0 »). Ils doivent retomber
// sur le défaut, sans quoi la dictée serait morte après mise à jour, avec pour
// seul indice « modèle de dictée inconnu » au premier clic sur le micro.
func TestModeleWhisperHeriteRetombeSurLeDefaut(t *testing.T) {
	testHome(t)
	c := DictateCfg{Model: "large-v3-turbo-q5_0", Reactivity: "court"}.withDefauts()
	if _, ok := asrCatalogue[c.Model]; !ok {
		t.Fatalf("modèle hérité %q non corrigé", c.Model)
	}
	if c.Reactivity != "court" {
		t.Errorf("réactivité = %q, elle devait être conservée", c.Reactivity)
	}
}

// Valider à l'ÉCRITURE : un modèle hors catalogue accepté en silence ne se
// manifesterait qu'au premier clic sur le micro, loin du geste qui l'a causé.
func TestSaveRefuseModeleInconnu(t *testing.T) {
	testHome(t)
	if err := dictateCfgSave(DictateCfg{Model: "nawak", Reactivity: "moyen"}); err == nil {
		t.Error("un modèle hors catalogue a été accepté")
	}
}

func TestSaveLoadAllerRetour(t *testing.T) {
	testHome(t)
	want := DictateCfg{Model: "parakeet-tdt-0.6b-v2", Reactivity: "long"}
	if err := dictateCfgSave(want); err != nil {
		t.Fatal(err)
	}
	if got := dictateCfgLoad(); got != want {
		t.Errorf("relu %+v, attendu %+v", got, want)
	}
}

// Les bornes de découpage pilotent la latence perçue : plus la réserve est
// courte, plus le texte arrive vite, au prix d'un contexte plus maigre pour le
// modèle. L'ordre entre les trois réglages est ce qui doit tenir.
func TestChunkBounds(t *testing.T) {
	court, _ := DictateCfg{Reactivity: "court"}.chunkBounds()
	moyen, _ := DictateCfg{Reactivity: "moyen"}.chunkBounds()
	long, longMax := DictateCfg{Reactivity: "long"}.chunkBounds()
	if !(court < moyen && moyen < long) {
		t.Errorf("réserves non croissantes : court=%v moyen=%v long=%v", court, moyen, long)
	}
	if longMax <= long {
		t.Error("la coupure forcée doit être plus grande que la réserve minimale")
	}
	// Une valeur inconnue doit retomber sur le réglage moyen, pas sur zéro : une
	// réserve nulle couperait à chaque échantillon.
	inconnu, _ := DictateCfg{Reactivity: "nawak"}.chunkBounds()
	if inconnu != moyen {
		t.Errorf("réactivité inconnue = %v, attendu le réglage moyen %v", inconnu, moyen)
	}
	if inconnu <= 0 {
		t.Error("réserve nulle : la dictée couperait à chaque échantillon")
	}
}
