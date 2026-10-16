package loki

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Partage des couches (#121 amont) : la clé « gpu » liste les deux cartes (le
// serveur de Strata en déduit --layer-split), et rien du mode d'aide ne subsiste.
func TestStrataBuildConfigLayerSplit(t *testing.T) {
	base := map[string]any{
		"args":   []any{"--pack", "/p", "--vram-reserve-mib", "900"},
		"vision": map[string]any{"exe": "/v"},
	}
	cfg := map[string]string{"STRATA_MAIN_GPU": "1", "STRATA_HELPER_GPU": "0", "STRATA_SPLIT": "layers"}
	out, err := strataBuildConfig(base, cfg, "", "drop")
	if err != nil {
		t.Fatal(err)
	}
	g, ok := out["gpu"].([]any)
	if !ok || len(g) != 2 || g[0] != 1 || g[1] != 0 {
		t.Fatalf("gpu = %v, attendu [1 0] (principale d'abord, numéros nvidia-smi)", out["gpu"])
	}
	a := strataArgs(t, out)
	if strataHasArg(a, "--remote-expert-opt") || strataHasArg(a, "--expert-cache-device1") {
		t.Errorf("options du mode d'aide en partage des couches : %v", a)
	}
	if argAfter(a, "--vram-reserve-mib") != "900" {
		t.Errorf("la réserve de VRAM de l'installeur doit rester : %v", a)
	}
	if argAfter(a, "--short-read") != "150" {
		t.Errorf("lecture des prompts par la carte d'aide en partage des couches : %v", a)
	}
	env := out["env"].(map[string]any)
	if env["STRATA_REMOTE_DROP"] != nil || env["STRATA_PREFILL_HELPER_SOURCE"] != nil {
		t.Errorf("experts hors RAM ou source d'aide (mode d'aide) en partage des couches : %v", env)
	}
	if _, has := out["vision"].(map[string]any)["cuda_device"]; has {
		t.Errorf("encodeur d'images déplacé sur la seconde carte en partage des couches")
	}

	// Sans la clé, rien ne change : le mode d'aide reste le défaut.
	delete(cfg, "STRATA_SPLIT")
	out, _ = strataBuildConfig(base, cfg, "", "drop")
	if _, has := out["gpu"]; has {
		t.Errorf("clé gpu posée en mode d'aide : %v", out["gpu"])
	}
	if !strataHasArg(strataArgs(t, out), "--remote-expert-opt") {
		t.Errorf("mode d'aide perdu sans STRATA_SPLIT")
	}
	// Seconde carte coupée : pas de partage même si la clé traîne.
	cfg["STRATA_SPLIT"], cfg["STRATA_HELPER"] = "layers", "0"
	if strataLayerSplit(cfg) {
		t.Errorf("partage des couches avec la seconde carte coupée")
	}
}

// En partage des couches, la seconde carte ne garde pas d'experts : le mode
// hors RAM (qui compte sur elle pour en loger une part) n'est jamais choisi.
func TestStrataResolveExpertsLayerSplit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOKI_HOME", home)
	pack := filepath.Join(home, "pack")
	_ = os.MkdirAll(pack, 0o755)
	_ = os.WriteFile(filepath.Join(pack, "experts.bin"), []byte("x"), 0o644)
	_ = os.MkdirAll(filepath.Join(strataSrcDir(), "engine-cuda12"), 0o755)
	_ = os.WriteFile(strataEngineMarker(), []byte(strataEngineAsset+"\n"), 0o644)
	args := []string{"--pack", pack}
	cfg := map[string]string{"STRATA_CONFIG": "/x/strata-swift-iq3_xxs.json", "STRATA_HELPER_GPU": "1", "STRATA_DROP": "1"}
	if got := strataResolveExperts(cfg, args, 44, 12); got != "drop" {
		t.Fatalf("mode d'aide : %s, attendu drop", got)
	}
	cfg["STRATA_SPLIT"] = "layers"
	if got := strataResolveExperts(cfg, args, 44, 12); got != "arena" {
		t.Errorf("partage des couches : %s, attendu arena", got)
	}
	cfg["STRATA_EXPERTS"] = "ram"
	if got := strataResolveExperts(cfg, args, 44, 12); got != "ram" {
		t.Errorf("partage des couches, experts en RAM : %s, attendu ram", got)
	}
}

func TestStrataApplySettingsSplit(t *testing.T) {
	in := "ENGINE=strata\nSTRATA_HELPER_GPU=0\n"
	req := strataSettingsReq{Ctx: 131072, KV: "fp16", Spec: 3, Vision: true, Helper: true, Split: "layers"}
	out, err := strataApplySettings(in, req)
	if err != nil || !strings.Contains(out, "STRATA_SPLIT=layers\n") {
		t.Fatalf("STRATA_SPLIT=layers attendu : %q (%v)", out, err)
	}
	req.Split = "helper"
	if out, _ = strataApplySettings(out, req); strings.Contains(out, "STRATA_SPLIT") {
		t.Errorf("retour au mode d'aide : la clé doit disparaître : %q", out)
	}
	// un ancien client n'envoie pas « split » : la clé est conservée
	req.Split = ""
	out, _ = strataApplySettings("STRATA_SPLIT=layers\n", req)
	if !strings.Contains(out, "STRATA_SPLIT=layers") {
		t.Errorf("clé effacée par un client qui ne la connaît pas : %q", out)
	}
	req.Split = "nimporte"
	if _, err := strataApplySettings(in, req); err == nil {
		t.Errorf("répartition inconnue acceptée")
	}
}

func TestStrataDetailsForLayerSplit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOKI_HOME", home)
	base := filepath.Join(home, "base.json")
	_ = os.WriteFile(base, []byte(`{"args":["--pack","/p"],"vision":{"exe":"v"}}`), 0o644)
	_ = os.MkdirAll(presetsDir(), 0o755)
	preset := "ENGINE=strata\nSTRATA_CONFIG=" + base + "\nSTRATA_MAIN_GPU=1\nSTRATA_HELPER_GPU=0\nSTRATA_SPLIT=layers\n"
	_ = os.WriteFile(filepath.Join(presetsDir(), "X.env"), []byte(preset), 0o644)
	env := strataEnv{GPUs: []strataGPU{{Index: 0, Name: "NVIDIA GeForce RTX 3060", VRAMGB: 12}, {Index: 1, Name: "NVIDIA GeForce RTX 3060", VRAMGB: 12}}}
	d := strataDetailsFor("X", env)
	if d == nil {
		t.Fatal("pas de détails")
	}
	if !d.Split || !d.HelperOn || d.HelperGPU == "" || d.VisionGPU != d.MainGPU || d.ShortRead != "150" {
		t.Errorf("%+v", *d)
	}
}
