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
