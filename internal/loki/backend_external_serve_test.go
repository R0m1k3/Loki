package loki

import "testing"

// Preset externe actif : `loki serve` n'a aucun moteur local à lancer. Il
// sortait en erreur « MODEL non défini », que systemd (Restart=on-failure)
// relançait en boucle (amont #95).
func TestServeExitsCleanlyForExternalPreset(t *testing.T) {
	testHome(t)
	if err := WriteConfig(map[string]string{extKeyFlag: "1", "PORT": "8080"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdServe(nil); err != nil {
		t.Fatalf("loki serve sur un preset externe : %v", err)
	}
}

// Le pré-vol refusait `loki start` / `loki restart` sur un preset externe,
// en invitant à réinstaller llama.cpp.
func TestPreflightAcceptsExternalPreset(t *testing.T) {
	testHome(t)
	if err := WriteConfig(map[string]string{extKeyFlag: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := preflightEngine(); err != nil {
		t.Fatalf("pré-vol refusé sur un preset externe : %v", err)
	}
}

// Strata : le pré-vol vérifie sa configuration à lui, pas BIN/MODEL.
func TestPreflightStrataWithoutConfig(t *testing.T) {
	testHome(t)
	if err := WriteConfig(map[string]string{"ENGINE": "strata"}); err != nil {
		t.Fatal(err)
	}
	err := preflightEngine()
	if err == nil {
		t.Fatal("un preset Strata sans STRATA_CONFIG devrait être refusé")
	}
	if want := "STRATA_CONFIG"; !contains(err.Error(), want) {
		t.Fatalf("message attendu sur %s : %v", want, err)
	}
}
