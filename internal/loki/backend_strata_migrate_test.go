package loki

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Moteur 1.1 : avec une carte d'aide, la lecture d'un prompt passe par elle
// (source « helper ») et le chemin direct ne garde que les ajouts sous 48 jetons.
func TestStrataBuildConfigHelperPrompt(t *testing.T) {
	base := map[string]any{"args": []any{"--pack", "/p"}}
	out, _ := strataBuildConfig(base, map[string]string{"STRATA_HELPER_GPU": "0"}, "", "ram")
	if argAfter(strataArgs(t, out), "--short-read") != "48" || out["env"].(map[string]any)["STRATA_PREFILL_HELPER_SOURCE"] != "1" {
		t.Errorf("carte d'aide : lecture courte et source d'aide attendues : %v %v", strataArgs(t, out), out["env"])
	}
	// carte d'aide coupée ou absente : réglages d'avant
	for _, cfg := range []map[string]string{{}, {"STRATA_HELPER_GPU": "0", "STRATA_HELPER": "0"}} {
		out, _ = strataBuildConfig(base, cfg, "", "ram")
		if argAfter(strataArgs(t, out), "--short-read") != "150" || out["env"].(map[string]any)["STRATA_PREFILL_HELPER_SOURCE"] != nil {
			t.Errorf("une carte (%v) : %v %v", cfg, strataArgs(t, out), out["env"])
		}
	}
}

// Mode hors RAM : réserve de VRAM de la carte d'aide abaissée, contexte plafonné.
func TestStrataBuildConfigDropReserveAndCtx(t *testing.T) {
	base := map[string]any{"args": []any{"--pack", "/p"}, "env": map[string]any{}}
	cfg := map[string]string{"STRATA_HELPER_GPU": "0", "CTX": "167936"}
	out, _ := strataBuildConfig(base, cfg, "", "drop")
	if out["env"].(map[string]any)["STRATA_REMOTE_DROP_RESERVE_MIB"] != "640" {
		t.Errorf("réserve de la carte d'aide : %v", out["env"])
	}
	if got := argAfter(strataArgs(t, out), "--max-context"); got != "131072" {
		t.Errorf("contexte hors RAM non plafonné : %s", got)
	}
	// hors du mode hors RAM : le contexte demandé
	for _, mode := range []string{"ram", "arena", "disk"} {
		out, _ = strataBuildConfig(base, cfg, "", mode)
		if got := argAfter(strataArgs(t, out), "--max-context"); got != "167936" {
			t.Errorf("%s : contexte plafonné sans mode hors RAM : %s", mode, got)
		}
		if out["env"].(map[string]any)["STRATA_REMOTE_DROP_RESERVE_MIB"] != nil {
			t.Errorf("%s : réserve du mode hors RAM posée : %v", mode, out["env"])
		}
	}
}

// Un modèle installé avec le paquet 1.0 passe au paquet courant à son premier
// lancement, sans réinstallation.
func TestStrataMigrate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("lien symbolique : Strata ne tourne que sous Linux")
	}
	home := t.TempDir()
	t.Setenv("LOKI_HOME", home)
	oldDir := filepath.Join(strataHome(), "ajean-moe-1.0")
	newDir := strataSrcDir()
	for _, d := range []string{filepath.Join(oldDir, ".venv", "bin"), filepath.Join(newDir, "engine-cuda12"), presetsDir()} {
		_ = os.MkdirAll(d, 0o755)
	}
	_ = os.WriteFile(strataVenvPython(oldDir), []byte("#!/bin/sh\n"), 0o755)
	for _, d := range []string{oldDir, newDir} {
		_ = os.WriteFile(filepath.Join(d, "requirements.txt"), []byte("numpy\n"), 0o644)
	}
	_ = os.WriteFile(filepath.Join(newDir, "setup.py"), []byte(""), 0o644)
	_ = os.WriteFile(strataEngineMarker(), []byte(strataEngineAsset+"\n"), 0o644)
	old := filepath.Join(oldDir, "strata-swift-iq3_xxs.json")
	_ = os.WriteFile(old, []byte(`{"exe": "`+filepath.Join(oldDir, "engine-cuda12", "strata")+`", "args": []}`), 0o644)
	preset := filepath.Join(presetsDir(), "P.env")
	_ = os.WriteFile(preset, []byte("ENGINE=strata\nSTRATA_CONFIG="+old+"\nCTX=1\n"), 0o644)
	other := filepath.Join(presetsDir(), "L.env")
	_ = os.WriteFile(other, []byte("MODEL=/m.gguf\n"), 0o644)
	if err := SetConfigKey("STRATA_CONFIG", old); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]string{"ENGINE": "strata", "STRATA_CONFIG": old}
	if err := strataMigrate(cfg); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(newDir, "strata-swift-iq3_xxs.json")
	if cfg["STRATA_CONFIG"] != want || ReadConfig()["STRATA_CONFIG"] != want {
		t.Fatalf("config vivante : %q", cfg["STRATA_CONFIG"])
	}
	b, _ := os.ReadFile(want)
	if strings.Contains(string(b), oldDir) || !strings.Contains(string(b), filepath.Join(newDir, "engine-cuda12")) {
		t.Errorf("chemins non réécrits : %s", b)
	}
	if p, _ := os.ReadFile(preset); !strings.Contains(string(p), "STRATA_CONFIG="+want+"\n") {
		t.Errorf("preset non réécrit : %s", p)
	}
	if p, _ := os.ReadFile(other); string(p) != "MODEL=/m.gguf\n" {
		t.Errorf("preset d'un autre moteur touché : %s", p)
	}
	if !isFile(strataVenvPython(newDir)) {
		t.Error("environnement Python non repris")
	}
	// déjà à jour : rien à faire
	if err := strataMigrate(cfg); err != nil || cfg["STRATA_CONFIG"] != want {
		t.Errorf("seconde passe : %v %q", err, cfg["STRATA_CONFIG"])
	}
}

// Dépendances Python changées : rien n'est réécrit, l'ancienne version tourne.
func TestStrataMigrateRequirementsChanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOKI_HOME", home)
	oldDir := filepath.Join(strataHome(), "ajean-moe-1.0")
	newDir := strataSrcDir()
	for _, d := range []string{filepath.Join(oldDir, ".venv", "bin"), newDir} {
		_ = os.MkdirAll(d, 0o755)
	}
	_ = os.WriteFile(strataVenvPython(oldDir), []byte("#!/bin/sh\n"), 0o755)
	_ = os.WriteFile(filepath.Join(oldDir, "requirements.txt"), []byte("numpy\n"), 0o644)
	_ = os.WriteFile(filepath.Join(newDir, "requirements.txt"), []byte("numpy\ntorch\n"), 0o644)
	_ = os.WriteFile(filepath.Join(newDir, "setup.py"), []byte(""), 0o644)
	old := filepath.Join(oldDir, "strata-iq3_xxs.json")
	_ = os.WriteFile(old, []byte(`{"args": []}`), 0o644)
	cfg := map[string]string{"STRATA_CONFIG": old}
	if err := strataMigrate(cfg); err == nil || !strings.Contains(err.Error(), "dépendances") {
		t.Fatalf("erreur de dépendances attendue : %v", err)
	}
	if cfg["STRATA_CONFIG"] != old || isFile(filepath.Join(newDir, "strata-iq3_xxs.json")) {
		t.Errorf("config réécrite malgré l'échec : %q", cfg["STRATA_CONFIG"])
	}
	// un preset hors de strata/ (copié à la main) n'est pas touché
	cfg = map[string]string{"STRATA_CONFIG": filepath.Join(home, "ailleurs", "ajean-moe-1.0", "strata-x.json")}
	if err := strataMigrate(cfg); err != nil {
		t.Errorf("config hors de strata/ : %v", err)
	}
}
