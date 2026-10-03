package loki

import (
	"reflect"
	"testing"
)

// Deux aides de moteur fabriquées, réduites aux lignes que lisent les tests de
// capacité : un llama.cpp récent (--load-mode, « -ngl auto », --reasoning) et
// un ancien qui ne connaît que les drapeaux d'origine.
const (
	helpRecent = `-ngl,   --gpu-layers, --n-gpu-layers N      max. number of layers to store in VRAM, either an exact number,
                                        'auto', or 'all' (default: auto)
-lm,    --load-mode MODE                how to load the model: auto, mmap, mmap+mlock, mlock, none, dio
-rea,   --reasoning [on|off|auto]       use reasoning/thinking in the chat (default: auto)
`
	helpOld = `-ngl,   --gpu-layers, --n-gpu-layers N      number of layers to store in VRAM
--mlock                                 force system to keep model in RAM
--no-mmap                               do not memory-map model
`
)

// TestBuildServeArgs fige la ligne de commande produite pour les presets
// courants : la moindre différence d'ordre ou de valeur casse le test. Le
// moteur retient la DERNIÈRE occurrence d'un drapeau, donc l'ordre compte
// autant que les valeurs — c'est la base sur laquelle les réglages suivants
// viendront s'ajouter, chacun avec sa ligne ici.
func TestBuildServeArgs(t *testing.T) {
	const bin, model = "/opt/llama/llama-server", "/models/Qwen3.6-27B-Q4_K_M.gguf"
	base := []string{bin, "-m", model,
		"-c", "32768", "-b", "2048", "-ub", "512",
		"--host", "0.0.0.0", "--port", "8080"}
	with := func(tail ...string) []string {
		return append(append([]string{}, base...), tail...)
	}
	// withThreads insère -t / -tb à leur place, juste après -c.
	withThreads := func(threads []string, tail ...string) []string {
		out := append(append([]string{}, base[:5]...), threads...)
		return append(append(out, base[5:]...), tail...)
	}
	cases := []struct {
		name      string
		cfg       map[string]string
		si        serveSysInfo
		want      []string
		wantEnv   map[string]string
		wantNotes int
	}{
		{
			name: "dense par défaut, moteur récent : 999 devient auto, et on le dit",
			cfg:  map[string]string{"NGL": "999"},
			si:   serveSysInfo{Help: helpRecent},
			want: with("--parallel", "1", "-ngl", "auto"), wantNotes: 1,
		},
		{
			name: "dense par défaut, moteur ancien : 999 reste 999",
			cfg:  map[string]string{"NGL": "999"},
			si:   serveSysInfo{Help: helpOld},
			want: with("--parallel", "1", "-ngl", "999"),
		},
		{
			name: "clé NGL absente, aide illisible : forme historique",
			cfg:  map[string]string{},
			si:   serveSysInfo{},
			want: with("--parallel", "1", "-ngl", "999"),
		},
		{
			name: "NGL forcé à un nombre : jamais touché",
			cfg:  map[string]string{"NGL": "28"},
			si:   serveSysInfo{Help: helpRecent},
			want: with("--parallel", "1", "-ngl", "28"),
		},
		{
			name: "NGL=auto : aucun drapeau",
			cfg:  map[string]string{"NGL": "auto"},
			si:   serveSysInfo{Help: helpRecent},
			want: with("--parallel", "1"),
		},
		{
			name: "réglages du preset et cache KV commun",
			cfg: map[string]string{"CTX": "65536", "THREADS": "8", "THREADS_BATCH": "16", "BATCH": "4096",
				"UBATCH": "1024", "HOST": "127.0.0.1", "PORT": "9090", "PARALLEL": "2", "NGL": "all",
				"KV_TYPE": "q8_0"},
			si: serveSysInfo{Help: helpRecent},
			want: []string{bin, "-m", model,
				"-c", "65536", "-t", "8", "-tb", "16", "-b", "4096", "-ub", "1024",
				"--host", "127.0.0.1", "--port", "9090",
				"--parallel", "2", "-ngl", "all", "-ctk", "q8_0", "-ctv", "q8_0"},
		},
		{
			name: "THREADS=0 et THREADS_BATCH=0 : aucun drapeau, llama.cpp prend ses cœurs physiques",
			cfg:  map[string]string{"NGL": "28", "THREADS": "0", "THREADS_BATCH": "0"},
			si:   serveSysInfo{Help: helpRecent},
			want: with("--parallel", "1", "-ngl", "28"),
		},
		{
			name: "THREADS=6 seul : -t 6, -tb laissé au moteur (il recopie -t)",
			cfg:  map[string]string{"NGL": "28", "THREADS": "6"},
			si:   serveSysInfo{Help: helpRecent},
			want: withThreads([]string{"-t", "6"}, "--parallel", "1", "-ngl", "28"),
		},
		{
			name: "THREADS_BATCH seul : -tb sans -t",
			cfg:  map[string]string{"NGL": "28", "THREADS_BATCH": "12"},
			si:   serveSysInfo{Help: helpRecent},
			want: withThreads([]string{"-tb", "12"}, "--parallel", "1", "-ngl", "28"),
		},
		{
			name: "THREADS illisible ou négatif : ignoré, et dit",
			cfg:  map[string]string{"NGL": "28", "THREADS": "auto", "THREADS_BATCH": "-1"},
			si:   serveSysInfo{Help: helpRecent},
			want: with("--parallel", "1", "-ngl", "28"), wantNotes: 2,
		},
		{
			name: "-t=6 dans EXTRA_ARGS : pas de -t de Loki",
			cfg:  map[string]string{"NGL": "28", "THREADS": "8", "THREADS_BATCH": "16", "EXTRA_ARGS": "-t=6 --threads-batch 10"},
			si:   serveSysInfo{Help: helpRecent},
			want: with("--parallel", "1", "-ngl", "28", "-t=6", "--threads-batch", "10"),
		},
		{
			name: "conteneur à l'étroit : -t de la sonde, et une note",
			cfg:  map[string]string{"NGL": "28"},
			si:   serveSysInfo{Help: helpRecent, CPU: cpuBudget{N: 4, Engine: 8, Why: "quota cgroup de 4 CPU"}},
			want: withThreads([]string{"-t", "4"}, "--parallel", "1", "-ngl", "28"), wantNotes: 1,
		},
		{
			name: "THREADS explicite : la sonde se tait",
			cfg:  map[string]string{"NGL": "28", "THREADS": "6"},
			si:   serveSysInfo{Help: helpRecent, CPU: cpuBudget{N: 4, Engine: 8, Why: "quota"}},
			want: withThreads([]string{"-t", "6"}, "--parallel", "1", "-ngl", "28"),
		},
		{
			name: "masque d'affinité dans EXTRA_ARGS : la sonde se tait",
			cfg:  map[string]string{"NGL": "28", "EXTRA_ARGS": "-Cr 0-3"},
			si:   serveSysInfo{Help: helpRecent, CPU: cpuBudget{N: 4, Engine: 8, Why: "quota"}},
			want: with("--parallel", "1", "-ngl", "28", "-Cr", "0-3"),
		},
		{
			name: "cache KV séparé K/V",
			cfg:  map[string]string{"NGL": "28", "KV_TYPE": "q8_0", "KV_TYPE_V": "q4_0"},
			si:   serveSysInfo{Help: helpRecent},
			want: with("--parallel", "1", "-ngl", "28", "-ctk", "q8_0", "-ctv", "q4_0"),
		},
		{
			// Preset MoE typique : experts sur CPU via EXTRA_ARGS, qui pose aussi
			// --parallel, -ngl et le cache KV. Loki n'ajoute alors ni --parallel ni
			// -ngl, et EXTRA_ARGS ferme la marche (la dernière occurrence gagne).
			name: "MoE : EXTRA_ARGS avec -ot, --n-cpu-moe, -ctk q8_0",
			cfg: map[string]string{"NGL": "999", "PARALLEL": "1",
				"EXTRA_ARGS": `-ngl 99 --parallel 1 -ot "blk\.(\d+)\.ffn_.*_exps=CPU" --n-cpu-moe 30 -ctk q8_0 -ctv q8_0 -fa on`},
			si: serveSysInfo{Help: helpRecent},
			want: with("-ngl", "99", "--parallel", "1", "-ot", `blk\.(\d+)\.ffn_.*_exps=CPU`,
				"--n-cpu-moe", "30", "-ctk", "q8_0", "-ctv", "q8_0", "-fa", "on"),
		},
		{
			name: "--n-gpu-layers=N dans EXTRA_ARGS : pas de -ngl en double",
			cfg:  map[string]string{"NGL": "999", "EXTRA_ARGS": "--n-gpu-layers=40 -np 2"},
			si:   serveSysInfo{Help: helpRecent},
			want: with("--n-gpu-layers=40", "-np", "2"),
		},
		{
			name: "raisonnement actif : budget illimité par défaut",
			cfg:  map[string]string{"NGL": "28", "REASONING": "on"},
			si:   serveSysInfo{Help: helpRecent},
			want: with("--parallel", "1", "-ngl", "28", "--reasoning", "on", "--reasoning-budget", "-1"),
		},
		{
			name: "raisonnement auto avec budget choisi",
			cfg:  map[string]string{"NGL": "28", "REASONING": " auto ", "REASONING_BUDGET": "4096"},
			si:   serveSysInfo{Help: helpRecent},
			want: with("--parallel", "1", "-ngl", "28", "--reasoning", "auto", "--reasoning-budget", "4096"),
		},
		{
			name: "raisonnement interdit, moteur qui connaît le drapeau",
			cfg:  map[string]string{"NGL": "28", "REASONING": "off"},
			si:   serveSysInfo{Help: helpRecent},
			want: with("--parallel", "1", "-ngl", "28", "--reasoning", "off"),
		},
		{
			name: "raisonnement interdit, vieux moteur : aucun drapeau, une note",
			cfg:  map[string]string{"NGL": "28", "REASONING": "off"},
			si:   serveSysInfo{Help: helpOld},
			want: with("--parallel", "1", "-ngl", "28"), wantNotes: 1,
		},
		{
			name: "vision et clé d'API, avant EXTRA_ARGS",
			cfg:  map[string]string{"NGL": "28", "EXTRA_ARGS": "-fa on"},
			si:   serveSysInfo{Help: helpRecent, MMProj: "/models/mmproj-F16.gguf", APIKey: "secret"},
			want: with("--parallel", "1", "-ngl", "28", "--mmproj", "/models/mmproj-F16.gguf",
				"--api-key", "secret", "-fa", "on"),
		},
		{
			name: "drapeaux de chargement anciens → --load-mode sur moteur récent",
			cfg:  map[string]string{"NGL": "28", "EXTRA_ARGS": "--mlock --no-mmap -fa on"},
			si:   serveSysInfo{Help: helpRecent},
			want: with("--parallel", "1", "-ngl", "28", "-fa", "on", "--load-mode", "mlock"),
		},
		{
			name: "--load-mode retraduit pour un moteur ancien",
			cfg:  map[string]string{"NGL": "28", "EXTRA_ARGS": "--load-mode none"},
			si:   serveSysInfo{Help: helpOld},
			want: with("--parallel", "1", "-ngl", "28", "--no-mmap"),
		},
		{
			// dio n'existe pas sur l'ancien moteur : retiré, et dit par une note
			// RENDUE (buildServeArgs n'écrit rien elle-même).
			name: "--load-mode dio sur moteur ancien : retiré, une note",
			cfg:  map[string]string{"NGL": "28", "EXTRA_ARGS": "--load-mode dio -fa on"},
			si:   serveSysInfo{Help: helpOld},
			want: with("--parallel", "1", "-ngl", "28", "-fa", "on"), wantNotes: 1,
		},
		{
			name:    "sélection GPU : variables d'environnement, ligne inchangée",
			cfg:     map[string]string{"NGL": "28", "CUDA_VISIBLE_DEVICES": "1,0"},
			si:      serveSysInfo{Help: helpRecent},
			want:    with("--parallel", "1", "-ngl", "28"),
			wantEnv: map[string]string{"CUDA_VISIBLE_DEVICES": "1,0", "CUDA_DEVICE_ORDER": "PCI_BUS_ID"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.si.Model = model
			args, env, notes := buildServeArgs(c.cfg, splitArgs(c.cfg["EXTRA_ARGS"]), bin, c.si)
			if !reflect.DeepEqual(args, c.want) {
				t.Fatalf("args\n got %q\nwant %q", args, c.want)
			}
			wantEnv := c.wantEnv
			if wantEnv == nil {
				wantEnv = map[string]string{}
			}
			if !reflect.DeepEqual(env, wantEnv) {
				t.Fatalf("env = %v, want %v", env, wantEnv)
			}
			if len(notes) != c.wantNotes {
				t.Fatalf("notes = %q, en attendait %d", notes, c.wantNotes)
			}
		})
	}
}

func TestHelpCapabilities(t *testing.T) {
	if !helpSupportsReasoningFlag(helpRecent) || helpSupportsReasoningFlag(helpOld) {
		t.Fatal("--reasoning mal détecté")
	}
	if !helpSupportsLoadMode(helpRecent) || helpSupportsLoadMode(helpOld) {
		t.Fatal("--load-mode mal détecté")
	}
	if !helpSupportsNGLAuto(helpRecent) || helpSupportsNGLAuto(helpOld) {
		t.Fatal("« -ngl auto » mal détecté")
	}
	// Aide illisible = aucune capacité : on garde la forme historique.
	if helpFitsLayersItself("") || helpSupportsReasoningFlag("") {
		t.Fatal("une aide vide ne doit promettre aucune capacité")
	}
}

func TestServeKVTypes(t *testing.T) {
	for _, c := range []struct {
		cfg  map[string]string
		k, v string
	}{
		{map[string]string{}, "", ""},
		{map[string]string{"KV_TYPE": "q8_0"}, "q8_0", "q8_0"},
		{map[string]string{"KV_TYPE": "q8_0", "KV_TYPE_K": "f16"}, "f16", "q8_0"},
		{map[string]string{"KV_TYPE_V": "q4_0"}, "", "q4_0"},
	} {
		if k, v := serveKVTypes(c.cfg); k != c.k || v != c.v {
			t.Errorf("%v : got %s/%s, want %s/%s", c.cfg, k, v, c.k, c.v)
		}
	}
}
