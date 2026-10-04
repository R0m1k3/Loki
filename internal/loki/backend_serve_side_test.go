package loki

import (
	"reflect"
	"strings"
	"testing"
)

// helpSide : un moteur qui connaît le cache KV non unifié.
const helpSide = helpCacheRAM + `-kvu,   --kv-unified, -no-kvu, --no-kv-unified
                                        use single unified KV buffer shared across all sequences
`

// sideCfg : le preset de denseGPU (0,25 Mio d'état par jeton) à 32k, avec ou
// sans SIDE_SLOT.
func sideCfg(kv ...string) map[string]string {
	cfg := map[string]string{"CTX": "32768", "NGL": "999"}
	for i := 0; i+1 < len(kv); i += 2 {
		cfg[kv[i]] = kv[i+1]
	}
	return cfg
}

func sideSI() serveSysInfo {
	si := denseGPU()
	si.Help = helpSide
	return si
}

// Sans la clé (ou à off), la ligne de commande est celle d'avant, à l'octet
// près, notes comprises.
func TestSideSlotDefautIdentique(t *testing.T) {
	for _, v := range []string{"", "off", "0"} {
		cfg := sideCfg()
		if v != "" {
			cfg["SIDE_SLOT"] = v
		}
		got, _, notes := buildServeArgs(cfg, nil, "/bin/llama-server", sideSI())
		want, _, wantNotes := buildServeArgs(sideCfg(), nil, "/bin/llama-server", sideSI())
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(notes, wantNotes) {
			t.Fatalf("SIDE_SLOT=%q change la ligne :\n%v\n%v", v, got, want)
		}
		if argValue(got, "--parallel") != "1" || argValue(got, "-c") != "32768" {
			t.Fatalf("défaut : %v", got)
		}
		for _, a := range got {
			if a == "--no-kv-unified" {
				t.Fatal("--no-kv-unified sans la clé")
			}
		}
	}
}

// Accepté : deux slots de CTX jetons chacun, flux KV séparés — le pool fait
// le double, aucun slot ne peut déborder sur l'autre.
func TestSideSlotAccepte(t *testing.T) {
	for _, cfg := range []map[string]string{sideCfg("SIDE_SLOT", "on"), sideCfg("SIDE_SLOT", "on", "PARALLEL", "2")} {
		args, _, notes := buildServeArgs(cfg, nil, "/bin/llama-server", sideSI())
		if argValue(args, "-c") != "65536" || argValue(args, "--parallel") != "2" {
			t.Fatalf("args = %v", args)
		}
		n := 0
		for i, a := range args {
			if a == "--no-kv-unified" {
				n++
				if args[i-1] != "2" || args[i-2] != "--parallel" {
					t.Fatalf("--no-kv-unified mal placé : %v", args)
				}
			}
			if a == "--kv-unified-per-slot" || a == "-kvu" || a == "--kv-unified" {
				t.Fatalf("drapeau inattendu %s", a)
			}
		}
		if n != 1 {
			t.Fatalf("--no-kv-unified × %d", n)
		}
		found := false
		for _, s := range notes {
			if strings.HasPrefix(s, "SIDE_SLOT : 2 slots de 32768 jetons") && strings.Contains(s, "~8192 Mio") {
				found = true
			}
		}
		if !found {
			t.Fatalf("note de VRAM absente : %q", notes)
		}
	}
}

// Refusé : la ligne est exactement celle sans la clé, plus une note qui dit
// pourquoi.
func TestSideSlotRefus(t *testing.T) {
	cases := []struct {
		name  string
		cfg   map[string]string
		extra []string
		si    func(*serveSysInfo)
		why   string
	}{
		{name: "PARALLEL=3", cfg: sideCfg("SIDE_SLOT", "on", "PARALLEL", "3"), why: "PARALLEL=3"},
		{name: "-c dans EXTRA_ARGS", extra: []string{"-c", "16384"}, why: "-c "},
		{name: "--ctx-size=", extra: []string{"--ctx-size=16384"}, why: "--ctx-size"},
		{name: "-np", extra: []string{"-np", "4"}, why: "-np"},
		{name: "-kvu", extra: []string{"-kvu"}, why: "-kvu"},
		{name: "--no-kv-unified", extra: []string{"--no-kv-unified"}, why: "--no-kv-unified"},
		{name: "--kv-unified-per-slot", extra: []string{"--kv-unified-per-slot", "8192"}, why: "--kv-unified-per-slot"},
		{name: "--cache-idle-slots", extra: []string{"--cache-idle-slots"}, why: "--cache-idle-slots"},
		{name: "--no-cache-idle-slots", extra: []string{"--no-cache-idle-slots"}, why: "--no-cache-idle-slots"},
		{name: "variable LLAMA_ARG_N_PARALLEL", si: func(si *serveSysInfo) { si.ArgEnv["LLAMA_ARG_N_PARALLEL"] = "4" }, why: "LLAMA_ARG_N_PARALLEL"},
		{name: "variable LLAMA_ARG_KV_UNIFIED", si: func(si *serveSysInfo) { si.ArgEnv["LLAMA_ARG_KV_UNIFIED"] = "1" }, why: "LLAMA_ARG_KV_UNIFIED"},
		{name: "moteur sans --no-kv-unified", si: func(si *serveSysInfo) { si.Help = helpCacheRAM }, why: "--no-kv-unified"},
		{name: "CTX=0", cfg: sideCfg("SIDE_SLOT", "on", "CTX", "0"), why: "CTX"},
		{name: "VRAM trop juste à 64k", cfg: sideCfg("SIDE_SLOT", "on", "CTX", "65536"), why: "VRAM insuffisante"},
		{name: "VRAM inconnue", si: func(si *serveSysInfo) { si.VRAMMiB = 0 }, why: "VRAM"},
		{name: "GGUF illisible", si: func(si *serveSysInfo) { si.GGUF = nil }, why: "GGUF"},
		{name: "experts sur CPU", extra: []string{"--n-cpu-moe", "20"}, why: "CPU"},
		{name: "-ot sur CPU", extra: []string{"-ot", "exps=CPU"}, why: "CPU"},
		{name: "NGL partiel", cfg: sideCfg("SIDE_SLOT", "on", "NGL", "30"), why: "couches GPU"},
	}
	for _, c := range cases {
		cfg := c.cfg
		if cfg == nil {
			cfg = sideCfg("SIDE_SLOT", "on")
		}
		si := sideSI()
		if c.si != nil {
			c.si(&si)
		}
		_, _, why := sideSlotPlan(cfg, c.extra, si)
		if !strings.Contains(why, c.why) {
			t.Errorf("%s : why = %q, attendu %q", c.name, why, c.why)
		}
		got, _, notes := buildServeArgs(cfg, c.extra, "/bin/llama-server", si)
		off := map[string]string{}
		for k, v := range cfg {
			if k != "SIDE_SLOT" {
				off[k] = v
			}
		}
		want, _, wantNotes := buildServeArgs(off, c.extra, "/bin/llama-server", si)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s : refus qui change la ligne\n%v\n%v", c.name, got, want)
		}
		if len(notes) != len(wantNotes)+1 {
			t.Errorf("%s : notes %q", c.name, notes)
		}
	}
}
