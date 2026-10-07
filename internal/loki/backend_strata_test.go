package loki

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func strataArgs(t *testing.T, cfg map[string]any) []string {
	t.Helper()
	var out []string
	for _, a := range cfg["args"].([]any) {
		out = append(out, a.(string))
	}
	return out
}

func argAfter(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func TestStrataBuildConfigApplyTuning(t *testing.T) {
	base := map[string]any{
		"exe":  "/x/engine-cuda12/strata",
		"args": []any{"--pack", "/p", "--prefill", "8192", "--max-context", "131072"},
		"port": 8080.0,
	}
	out, err := strataBuildConfig(base, map[string]string{"PORT": "8081", "CTX": "65536"}, "k", "ram")
	if err != nil {
		t.Fatal(err)
	}
	a := strataArgs(t, out)
	for flag, want := range map[string]string{"--prefill": "16384", "--short-read": "150", "--prompt-cache-root": "256", "--max-context": "65536", "--pack": "/p"} {
		if got := argAfter(a, flag); got != want {
			t.Errorf("%s = %q, attendu %q (args %v)", flag, got, want, a)
		}
	}
	if strings.Count(strings.Join(a, " "), "--prefill") != 1 {
		t.Errorf("--prefill dupliqué : %v", a)
	}
	if strataHasArg(a, "--remote-expert-opt") || strataHasArg(a, "--expert-cache-device1") {
		t.Errorf("aide 2e carte sans 2e carte : %v", a)
	}
	if out["port"] != 8081 || out["api_key"] != "k" || out["host"] != "127.0.0.1" {
		t.Errorf("port/clé/hôte : %v %v %v", out["port"], out["api_key"], out["host"])
	}
	// la config de l'installeur n'est pas modifiée
	if base["args"].([]any)[3] != "8192" {
		t.Error("la config de base a été modifiée")
	}
}

func TestStrataBuildConfigHelperGPU(t *testing.T) {
	base := map[string]any{"args": []any{"--pack", "/p", "--mmap-experts", "--vram-reserve-mib", "700"}, "gpu": 1.0,
		"vision": map[string]any{"exe": "v", "gpu": true}}
	out, err := strataBuildConfig(base, map[string]string{"STRATA_HELPER_GPU": "0"}, "", "ram")
	if err != nil {
		t.Fatal(err)
	}
	a := strataArgs(t, out)
	if argAfter(a, "--expert-cache-device1") != "auto" || !strataHasArg(a, "--remote-expert-opt") {
		t.Errorf("aide 2e carte absente : %v", a)
	}
	if strataHasArg(a, "--vram-reserve-mib") || strataHasArg(a, "700") {
		t.Errorf("réserve de VRAM gardée : %v", a)
	}
	if _, has := out["api_key"]; has {
		t.Error("api_key posée sans clé")
	}
	if _, has := out["gpu"]; has {
		t.Error("clé gpu gardée avec une carte d'aide (le serveur en ferait un --layer-split)")
	}
	if out["vision"].(map[string]any)["cuda_device"] != 0 {
		t.Errorf("encodeur pas sur la carte d'aide : %v", out["vision"])
	}
	if out["env"].(map[string]any)["STRATA_PLE_BATCH"] != "0" {
		t.Errorf("STRATA_PLE_BATCH absent : %v", out["env"])
	}
}

func TestStrataCudaDevices(t *testing.T) {
	cases := []struct {
		main, helper, want string
	}{
		{"1", "0", "1,0"},
		{"0", "-1", "0"},
		{"0", "", "0"},
		{"", "", ""},
	}
	for _, c := range cases {
		got := strataCudaDevices(map[string]string{"STRATA_MAIN_GPU": c.main, "STRATA_HELPER_GPU": c.helper})
		if got != c.want {
			t.Errorf("main=%q helper=%q : %q, attendu %q", c.main, c.helper, got, c.want)
		}
	}
}

func TestStrataRecommend(t *testing.T) {
	// la machine de test : 5060 Ti 16 Go + 3070 8 Go, 47 Go de RAM, disque large
	env := strataEnv{Supported: true, Main: 1, Helper: 0, RAMGB: 47, AvailGB: 47, DiskFreeGB: 400,
		GPUs: []strataGPU{{Index: 0, VRAMGB: 8, Arch: 86}, {Index: 1, VRAMGB: 16, Arch: 120}}}
	if got := strataRecommend("swift", env); got != "IQ3_XXS" {
		t.Errorf("swift : %q, attendu IQ3_XXS", got)
	}
	// IQ3_S demande de garder 50 Go d'experts en RAM : pas sur 47 Go
	if got := strataRecommend("qwen", env); got != "IQ3_XXS" {
		t.Errorf("qwen 47 Go : %q, attendu IQ3_XXS", got)
	}
	f := strataFitFor(strataQuants["IQ3_XXS"], env, 0)
	// trop juste pour tout garder en RAM : experts lus depuis le disque (le mode
	// hors RAM demande un experts.bin, qu'une installation neuve n'a pas)
	if !f.OK || !f.Mmap || f.Drop {
		t.Errorf("IQ3_XXS sur 47 Go : ok=%v mmap=%v drop=%v (attendu ok, disque)", f.OK, f.Mmap, f.Drop)
	}
	// 64 Go : IQ3_S tient en RAM
	env.RAMGB, env.AvailGB = 64, 64
	if got := strataRecommend("qwen", env); got != "IQ3_S" {
		t.Errorf("qwen 64 Go : %q, attendu IQ3_S", got)
	}
	// 16 Go de RAM et une seule carte de 8 Go : rien ne tient
	small := strataEnv{Supported: true, Main: 0, Helper: -1, RAMGB: 16, DiskFreeGB: 400,
		GPUs: []strataGPU{{Index: 0, VRAMGB: 8, Arch: 86}}}
	if got := strataRecommend("swift", small); got != "" {
		t.Errorf("petite machine : %q, attendu aucune recommandation", got)
	}
	// disque trop petit
	env.DiskFreeGB = 50
	if f := strataFitFor(strataQuants["Q2_0"], env, 0); f.OK {
		t.Error("Q2_0 accepté avec 50 Go de disque")
	}
}

func TestStrataPresetName(t *testing.T) {
	if got := strataPresetName("swift", "IQ3_XXS"); got != "STRATA FLASH NEXT SWIFT 1.5 IQ3_XXS" {
		t.Errorf("%q", got)
	}
	if got := strataPresetName("qwen", "Q2_0"); got != "STRATA FLASH NEXT CLASSIQUE Q2_0" {
		t.Errorf("%q", got)
	}
}

func TestStrataFitCountsExistingData(t *testing.T) {
	env := strataEnv{Supported: true, Main: 1, Helper: 0, RAMGB: 47, DiskFreeGB: 32,
		GPUs: []strataGPU{{Index: 0, VRAMGB: 8, Arch: 86}, {Index: 1, VRAMGB: 16, Arch: 120}}}
	if f := strataFitFor(strataQuants["IQ3_XXS"], env, 120); !f.OK {
		t.Errorf("données déjà présentes, refusé quand même : %s", f.Why)
	}
	if f := strataFitFor(strataQuants["IQ3_XXS"], env, 0); f.OK {
		t.Error("32 Go libres sans données : accepté")
	}
}

func TestStrataBuildConfigPresetOverrides(t *testing.T) {
	base := map[string]any{"args": []any{"--kv", "int8", "--spec", "4"}}
	out, _ := strataBuildConfig(base, map[string]string{"STRATA_KV": "fp16", "STRATA_SPEC": "3"}, "", "ram")
	a := strataArgs(t, out)
	if argAfter(a, "--kv") != "fp16" || argAfter(a, "--spec") != "3" {
		t.Errorf("%v", a)
	}
}

func TestStrataDetailsFor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOKI_HOME", home)
	base := filepath.Join(home, "base.json")
	if err := os.WriteFile(base, []byte(`{"args":["--pack","/p","--kv","int8","--spec","4","--max-context","131072"],"vision":{"exe":"v"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(presetsDir(), 0o755)
	preset := "ENGINE=strata\nSTRATA_CONFIG=" + base + "\nSTRATA_MAIN_GPU=1\nSTRATA_HELPER_GPU=0\nSTRATA_MMAP=1\nSTRATA_KV=fp16\nSTRATA_KV_RESIDENT=32768\nSTRATA_SPEC=3\n"
	if err := os.WriteFile(filepath.Join(presetsDir(), "X.env"), []byte(preset), 0o644); err != nil {
		t.Fatal(err)
	}
	env := strataEnv{GPUs: []strataGPU{{Index: 0, Name: "NVIDIA GeForce RTX 3070", VRAMGB: 8}, {Index: 1, Name: "NVIDIA GeForce RTX 5060 Ti", VRAMGB: 16}}}
	d := strataDetailsFor("X", env)
	if d == nil {
		t.Fatal("pas de détails")
	}
	if d.KV != "fp16" || d.KVResident != "32768" || d.Spec != "3" || d.Ctx != "131072" || !d.Mmap ||
		d.MainGPU != "RTX 5060 Ti (16 Go)" || d.HelperGPU != "RTX 3070 (8 Go)" || d.VisionGPU != "RTX 3070 (8 Go)" ||
		d.ShortRead != "150" || d.CacheRoot != "256" || d.Prefill != "16384" {
		t.Errorf("%+v", *d)
	}
}

func TestStrataHaveGBPerChoice(t *testing.T) {
	t.Setenv("LOKI_HOME", t.TempDir())
	write := func(rel string, n int) {
		p := filepath.Join(strataDataDir(), rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("models/swift-IQ3_XXS/a.gguf", 3000)
	write("packs/swift-iq3_xxs/experts.bin", 2000)
	write("mtp/rt/x", 500)
	write("models/IQ2_XS/b.gguf", 7000) // un autre choix : ne compte pas pour swift IQ3_XXS
	if got := strataHaveGB("swift", "IQ3_XXS") * 1e9; got != 5500 {
		t.Errorf("swift IQ3_XXS : %v octets, attendu 5500", got)
	}
	if got := strataHaveGB("swift", "IQ2_XS") * 1e9; got != 500 {
		t.Errorf("swift IQ2_XS : %v octets, attendu 500 (seulement la tête MTP commune)", got)
	}
}

func TestStrataApplySettings(t *testing.T) {
	in := "# NAME=x\nENGINE=strata\nCTX=131072\nSTRATA_KV=fp16\nSTRATA_SPEC=3\nSTRATA_HELPER_GPU=0\n"
	out, err := strataApplySettings(in, strataSettingsReq{Ctx: 65536, KV: "int8", Spec: 4, Vision: false, Helper: false})
	if err != nil {
		t.Fatal(err)
	}
	cfg := parseEnv(out)
	if cfg["CTX"] != "65536" || cfg["STRATA_KV"] != "int8" || cfg["STRATA_SPEC"] != "4" || cfg["STRATA_VISION"] != "0" ||
		cfg["STRATA_HELPER"] != "0" || cfg["STRATA_HELPER_GPU"] != "0" || cfg["ENGINE"] != "strata" {
		t.Errorf("%q", out)
	}
	if strings.Count(out, "CTX=") != 1 {
		t.Errorf("CTX dupliqué : %q", out)
	}
	if strataHelperOn(cfg) || strataCudaDevices(map[string]string{"STRATA_MAIN_GPU": "1", "STRATA_HELPER_GPU": "0", "STRATA_HELPER": "0"}) != "1" {
		t.Error("carte d'aide coupée mais encore utilisée")
	}
	base := map[string]any{"args": []any{"--vision", "--pack", "/p"}, "vision": map[string]any{"exe": "v"}}
	b, _ := strataBuildConfig(base, cfg, "", "ram")
	if strataHasArg(strataArgs(t, b), "--vision") || b["vision"] != nil || strataHasArg(strataArgs(t, b), "--expert-cache-device1") {
		t.Errorf("vision / aide coupées mais présentes : %v", b)
	}
	for _, bad := range []strataSettingsReq{{Ctx: 1000, KV: "fp16", Spec: 3}, {Ctx: 32768, KV: "q2", Spec: 3}, {Ctx: 32768, KV: "fp16", Spec: 9}} {
		if _, err := strataApplySettings(in, bad); err == nil {
			t.Errorf("accepté : %+v", bad)
		}
	}
}

func TestEngineOf(t *testing.T) {
	cases := map[string]string{"": engineLlamacpp, "llamacpp": engineLlamacpp, "strata": engineStrata, " Strata ": engineStrata, "moe": engineStrata, "autre": engineLlamacpp}
	for in, want := range cases {
		if got := engineOf(map[string]string{"ENGINE": in}); got != want {
			t.Errorf("ENGINE=%q : %q, attendu %q", in, got, want)
		}
	}
}

func TestModelLoadErrorStrata(t *testing.T) {
	ok := strataServeMarker + " (paquet AJEAN MoE 1.0)\nloading the model (the first start takes a minute or two) ...\nready: http://127.0.0.1:8080/v1  (OpenAI: /v1/chat/completions)\n"
	if got := modelLoadErrorFrom(ok); got != "" {
		t.Errorf("Strata prêt signalé en échec : %q", got)
	}
	crash := strataServeMarker + "\nTraceback (most recent call last):\n  File \"server.py\", line 1\nRuntimeError: boom\n"
	if got := modelLoadErrorFrom(crash); !strings.Contains(got, "Strata") {
		t.Errorf("trace Python non signalée : %q", got)
	}
	// une trace d'un lancement PRÉCÉDENT ne compte pas
	if got := modelLoadErrorFrom(crash + strataServeMarker + "\nloading the model ...\n"); got != "" {
		t.Errorf("ancienne trace prise pour l'échec en cours : %q", got)
	}
}

// Où vont les experts : décidé sur la RAM DISPONIBLE. Vécu : 62 Go dont 18 pris
// ailleurs, mode « hors RAM » choisi sur la RAM totale, moteur tué par le noyau.
func TestStrataResolveExperts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOKI_HOME", home)
	pack := filepath.Join(home, "pack")
	_ = os.MkdirAll(pack, 0o755)
	args := []string{"--pack", pack}
	cfg := map[string]string{"STRATA_CONFIG": "/x/strata-swift-iq3_xxs.json", "STRATA_HELPER_GPU": "1", "STRATA_DROP": "1"}
	// IQ3_XXS : 42,9 Go d'experts
	cases := []struct {
		experts string
		avail   float64
		want    string
	}{
		{"", 44, "disk"}, // le cas vécu : 44 Go disponibles, pas de experts.bin
		{"", 60, "ram"},  // assez de RAM pour tout charger avec la marge
		{"auto", 52.8, "disk"},
		{"disk", 60, "disk"}, // choix de l'utilisateur
		{"ram", 20, "ram"},   // choix de l'utilisateur, même à l'étroit
	}
	for _, c := range cases {
		cfg["STRATA_EXPERTS"] = c.experts
		if got := strataResolveExperts(cfg, args, c.avail, 12); got != c.want {
			t.Errorf("experts=%q dispo=%.0f : %s, attendu %s", c.experts, c.avail, got, c.want)
		}
	}
	// experts.bin présent (moteur du paquet) : le mode hors RAM et le mappage deviennent possibles
	_ = os.WriteFile(filepath.Join(pack, "experts.bin"), []byte("x"), 0o644)
	_ = os.MkdirAll(filepath.Join(strataSrcDir(), "engine-cuda12"), 0o755)
	_ = os.WriteFile(strataEngineMarker(), []byte(strataEngineAsset+"\n"), 0o644)
	cfg["STRATA_EXPERTS"] = ""
	if got := strataResolveExperts(cfg, args, 44, 12); got != "drop" {
		t.Errorf("44 Go + carte d'appoint 12 Go + experts.bin : %s, attendu drop", got)
	}
	if got := strataResolveExperts(cfg, args, 30, 12); got != "arena" {
		t.Errorf("30 Go + experts.bin : %s, attendu arena", got)
	}
	cfg["STRATA_HELPER"] = "0"
	if got := strataResolveExperts(cfg, args, 44, 12); got != "arena" {
		t.Errorf("carte d'appoint coupée : %s, attendu arena", got)
	}
	cfg["STRATA_EXPERTS"] = "disk"
	if got := strataResolveExperts(cfg, args, 60, 12); got != "arena" {
		t.Errorf("disque demandé avec experts.bin : %s, attendu arena", got)
	}
	// quant inconnu : on ne parie pas sur la RAM
	if got := strataResolveExperts(map[string]string{"STRATA_CONFIG": "/x/autre.json"}, nil, 500, 0); got != "disk" {
		t.Errorf("quant inconnu : %s, attendu disk", got)
	}
}

func TestStrataBuildConfigModes(t *testing.T) {
	base := map[string]any{"args": []any{"--pack", "/p", "--resident-experts", "--pcie-frac", "0.3"}, "env": map[string]any{"X": "1"}}
	cfg := map[string]string{"STRATA_HELPER_GPU": "1"}
	env := func(m map[string]any) map[string]any { return m["env"].(map[string]any) }

	out, _ := strataBuildConfig(base, cfg, "", "disk")
	a := strataArgs(t, out)
	if !strataHasArg(a, "--mmap-experts") || strataHasArg(a, "--resident-experts") || argAfter(a, "--pcie-frac") != "0" ||
		env(out)["STRATA_ARENA_MMAP"] != nil || env(out)["X"] != "1" {
		t.Errorf("disk : %v %v", a, env(out))
	}
	out, _ = strataBuildConfig(base, cfg, "", "ram")
	a = strataArgs(t, out)
	if strataHasArg(a, "--mmap-experts") || strataHasArg(a, "--resident-experts") || env(out)["STRATA_ARENA_MMAP"] != nil || env(out)["STRATA_REMOTE_DROP"] != nil {
		t.Errorf("ram : %v %v", a, env(out))
	}
	out, _ = strataBuildConfig(base, cfg, "", "arena")
	if a = strataArgs(t, out); argAfter(a, "--pcie-frac") != "0" || env(out)["STRATA_ARENA_MMAP"] != "1" {
		t.Errorf("arena : %v %v", a, env(out))
	}
	out, _ = strataBuildConfig(base, cfg, "", "drop")
	if a = strataArgs(t, out); strataHasArg(a, "--pcie-frac") || env(out)["STRATA_REMOTE_DROP"] != "1" {
		t.Errorf("drop : la part PCIe doit être automatique : %v %v", a, env(out))
	}
	// drop sans carte d'appoint : rien de spécial, c'est le mode ram
	out, _ = strataBuildConfig(base, map[string]string{"STRATA_HELPER_GPU": "-1"}, "", "drop")
	if env(out)["STRATA_REMOTE_DROP"] != nil {
		t.Error("mode hors RAM sans carte d'appoint")
	}
}

func TestStrataApplySettingsExperts(t *testing.T) {
	in := "ENGINE=strata\nSTRATA_EXPERTS=auto\n"
	r := strataSettingsReq{Ctx: 131072, KV: "fp16", Spec: 3, Experts: "disk"}
	out, err := strataApplySettings(in, r)
	if err != nil || parseEnv(out)["STRATA_EXPERTS"] != "disk" {
		t.Fatalf("%v %q", err, out)
	}
	r.Experts = "nvme"
	if _, err := strataApplySettings(in, r); err == nil {
		t.Error("placement inconnu accepté")
	}
	r.Experts = ""
	if out, _ := strataApplySettings(in, r); parseEnv(out)["STRATA_EXPERTS"] != "auto" {
		t.Errorf("vide doit laisser le choix en place : %q", out)
	}
}
