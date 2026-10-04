package loki

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

const helpCacheRAM = helpRecent + `-cram,  --cache-ram N                   set the maximum cache size in MiB (default: 8192, -1 - no limit, 0 - disable)
--slot-save-path PATH                   path to save slot kv cache (default: disabled)
`

// denseGPU : un 27B dense de 17 Go sur deux cartes de 24 Go, machine de 64 Go.
// État : 64 couches × 4 têtes KV × (256+256) × 2 octets = 0,25 Mio par jeton.
func denseGPU() serveSysInfo {
	return serveSysInfo{
		Help:  helpCacheRAM,
		Model: "/models/dense.gguf",
		GGUF: &GGUFInfo{Arch: "qwen3", BlockCount: 64, HeadCountKV: 4, KeyLen: 256, ValLen: 256,
			ContextLength: 262144},
		ModelBytes: 17 << 30,
		RAMMiB:     65536, RAMAvailMiB: 60000,
		VRAMMiB: 2 * 24576,
		ArgEnv:  map[string]string{},
	}
}

func TestCacheRAMArgs(t *testing.T) {
	cases := []struct {
		name  string
		cfg   map[string]string
		extra []string
		si    func(*serveSysInfo)
		want  []string
		notes int
	}{
		{name: "dense sur GPU, 64 Go, 65k : besoin 40 Gio, plafonné à 30 % de la RAM",
			cfg: map[string]string{"CTX": "65536"}, want: []string{"--cache-ram", "19660"}, notes: 1},
		{name: "contexte court : le défaut du moteur suffit",
			cfg: map[string]string{"CTX": "8192"}},
		{name: "cache q8_0 : l'état pèse deux fois moins, besoin sous le plafond",
			cfg: map[string]string{"CTX": "32768", "KV_TYPE": "q8_0"}, want: []string{"--cache-ram", "10880"}, notes: 1},
		{name: "même contexte en f16 : plafonné",
			cfg: map[string]string{"CTX": "32768"}, want: []string{"--cache-ram", "19660"}, notes: 1},
		{name: "-c d'EXTRA_ARGS l'emporte sur CTX",
			cfg: map[string]string{"CTX": "65536"}, extra: []string{"-c", "8192"}},
		{name: "MoE, experts sur CPU par -ot : jamais touché (pas de 4096)",
			cfg: map[string]string{"CTX": "65536"}, extra: []string{"-ot", "exps=CPU"}},
		{name: "-ot mixte avec un élément sur CPU",
			cfg: map[string]string{"CTX": "65536"}, extra: []string{"-ot", "a=CUDA0,b=CPU"}},
		{name: "-ot vers une autre carte seulement : tout reste en VRAM",
			cfg: map[string]string{"CTX": "65536"}, extra: []string{"-ot", "blk\\.6.*=CUDA1"},
			want: []string{"--cache-ram", "19660"}, notes: 1},
		{name: "--n-cpu-moe",
			cfg: map[string]string{"CTX": "65536"}, extra: []string{"--n-cpu-moe", "30"}},
		{name: "LLAMA_ARG_N_CPU_MOE",
			cfg: map[string]string{"CTX": "65536"}, si: func(s *serveSysInfo) { s.ArgEnv["LLAMA_ARG_N_CPU_MOE"] = "20" }},
		{name: "cache KV sur CPU",
			cfg: map[string]string{"CTX": "65536"}, extra: []string{"-nkvo"}},
		{name: "NGL plus petit que le modèle",
			cfg: map[string]string{"CTX": "65536", "NGL": "40"}},
		{name: "NGL=999 : toutes les couches",
			cfg: map[string]string{"CTX": "65536", "NGL": "999"}, want: []string{"--cache-ram", "19660"}, notes: 1},
		{name: "modèle trop gros pour la VRAM",
			cfg: map[string]string{"CTX": "65536"}, si: func(s *serveSysInfo) { s.ModelBytes = 40 << 30 }},
		{name: "pas de VRAM connue (macOS, AMD, sans nvidia-smi)",
			cfg: map[string]string{"CTX": "65536"}, si: func(s *serveSysInfo) { s.VRAMMiB = 0 }},
		{name: "GGUF illisible",
			cfg: map[string]string{"CTX": "65536"}, si: func(s *serveSysInfo) { s.GGUF = nil }},
		{name: "RAM libre trop juste",
			cfg: map[string]string{"CTX": "65536"}, si: func(s *serveSysInfo) { s.RAMAvailMiB = 12000 }},
		{name: "CACHE_RAM fixé : passé tel quel",
			cfg: map[string]string{"CTX": "65536", "CACHE_RAM": "12000"}, want: []string{"--cache-ram", "12000"}},
		{name: "CACHE_RAM=-1 : sans limite, passé tel quel",
			cfg: map[string]string{"CACHE_RAM": "-1"}, want: []string{"--cache-ram", "-1"}},
		{name: "CACHE_RAM=0 : cache coupé",
			cfg: map[string]string{"CACHE_RAM": "0"}, want: []string{"--cache-ram", "0"}},
		{name: "CACHE_RAM=auto",
			cfg: map[string]string{"CTX": "65536", "CACHE_RAM": "auto"}, want: []string{"--cache-ram", "19660"}, notes: 1},
		{name: "CACHE_RAM illisible : ignoré, et dit",
			cfg: map[string]string{"CACHE_RAM": "beaucoup"}, notes: 1},
		{name: "-cram d'EXTRA_ARGS : Loki se tait",
			cfg: map[string]string{"CTX": "65536", "CACHE_RAM": "12000"}, extra: []string{"-cram", "2048"}},
		{name: "LLAMA_ARG_CACHE_RAM : la ligne de commande l'écraserait, Loki se tait",
			cfg: map[string]string{"CTX": "65536"}, si: func(s *serveSysInfo) { s.ArgEnv["LLAMA_ARG_CACHE_RAM"] = "4096" }},
		{name: "LLAMA_ARG_CACHE_RAM et CACHE_RAM : la variable gagne, et on le dit",
			cfg: map[string]string{"CACHE_RAM": "12000"}, si: func(s *serveSysInfo) { s.ArgEnv["LLAMA_ARG_CACHE_RAM"] = "4096" }, notes: 1},
		{name: "moteur sans --cache-ram : rien, même fixé",
			cfg: map[string]string{"CACHE_RAM": "12000"}, si: func(s *serveSysInfo) { s.Help = helpRecent }, notes: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			si := denseGPU()
			if c.si != nil {
				c.si(&si)
			}
			got, notes := cacheRAMArgs(c.cfg, c.extra, si)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("args = %v, attendu %v (notes %v)", got, c.want, notes)
			}
			if len(notes) != c.notes {
				t.Fatalf("notes = %v, attendu %d", notes, c.notes)
			}
		})
	}
}

// Hybride (Qwen3.5) : 16 couches d'attention sur 64, état récurrent des 48
// autres, compté avec ses points de reprise.
func TestCacheRAMAutoHybride(t *testing.T) {
	si := denseGPU()
	layers := make([]int, 64)
	for i := range layers {
		if (i+1)%4 == 0 {
			layers[i] = 4
		}
	}
	si.GGUF = &GGUFInfo{Arch: "qwen35", BlockCount: 64, HeadCountKV: 4, HeadCountKVLayers: layers,
		KeyLen: 256, ValLen: 256, FullAttnInterval: 4, Hybrid: true,
		SSMConv: 4, SSMInner: 6144, SSMState: 128, SSMGroups: 16}
	perTok, rec := ggufStateBytes(*si.GGUF, "", "")
	if perTok != 65536 || rec != 156893184 {
		t.Fatalf("état = %v o/jeton, %d o récurrents", perTok, rec)
	}
	// 4 Gio de KV + 10 états récurrents (9 points de reprise + l'état courant),
	// × 2,5.
	n, why := cacheRAMAuto(map[string]string{"CTX": "65536"}, nil, si)
	if n != 13981 {
		t.Fatalf("cacheRAMAuto = %d (%s), attendu 13981", n, why)
	}
}

// Sans key_length, llama.cpp prend n_embd / n_head : nous aussi.
func TestGGUFStateBytesReplis(t *testing.T) {
	g := GGUFInfo{BlockCount: 2, HeadCountKV: 2, EmbeddingLength: 1024, HeadCount: 8}
	if p, _ := ggufStateBytes(g, "", ""); p != 2*2*(128+128)*2 {
		t.Fatalf("repli n_embd/n_head : %v", p)
	}
	if p, _ := ggufStateBytes(GGUFInfo{BlockCount: 2, HeadCountKV: 2}, "", ""); p != 0 {
		t.Fatalf("dimensions inconnues : %v, attendu 0", p)
	}
}

func TestVRAMFromSMI(t *testing.T) {
	const out = "0, 24576\n1, 16384\n"
	cases := []struct {
		cvd  string
		pci  bool
		want int64
	}{
		{"", false, 40960},
		{"1", true, 16384},
		{"0,1", true, 40960},
		{"1", false, 16384}, // ordre inconnu : la plus petite carte
		{"0", false, 16384},
		{"GPU-1234", true, 0},
		{"2", true, 0},
	}
	for _, c := range cases {
		if got := vramFromSMI(out, c.cvd, c.pci); got != c.want {
			t.Errorf("vramFromSMI(%q, %v) = %d, attendu %d", c.cvd, c.pci, got, c.want)
		}
	}
	if got := vramFromSMI("[N/A]\n", "", false); got != 0 {
		t.Errorf("sortie illisible : %d", got)
	}
}

func TestCgroupMemMiB(t *testing.T) {
	v2 := fstest.MapFS{
		"sys/fs/cgroup/memory.max":     {Data: []byte("17179869184\n")},
		"sys/fs/cgroup/memory.current": {Data: []byte("4294967296\n")},
	}
	if l, c := cgroupMemMiB(v2); l != 16384 || c != 4096 {
		t.Fatalf("v2 = %d/%d", l, c)
	}
	if l, _ := cgroupMemMiB(fstest.MapFS{"sys/fs/cgroup/memory.max": {Data: []byte("max\n")}}); l != 0 {
		t.Fatalf("v2 sans limite = %d", l)
	}
	v1 := fstest.MapFS{"sys/fs/cgroup/memory/memory.limit_in_bytes": {Data: []byte("9223372036854771712\n")}}
	if l, _ := cgroupMemMiB(v1); l != 0 {
		t.Fatalf("v1 sans limite = %d", l)
	}
}

func TestSlotSaveArgs(t *testing.T) {
	want := []string{"--slot-save-path", "/data/slots"}
	cases := []struct {
		name  string
		cfg   map[string]string
		extra []string
		si    func(*serveSysInfo)
		want  []string
	}{
		{name: "boucle locale", cfg: map[string]string{"HOST": "127.0.0.1"}, want: want},
		{name: "--host ::1 dans EXTRA_ARGS", extra: []string{"--host", "::1"}, want: want},
		{name: "0.0.0.0 sans clé : ouvert à tous, pas de drapeau", cfg: map[string]string{}},
		{name: "0.0.0.0 avec clé d'API", cfg: map[string]string{"HOST": "0.0.0.0"},
			si: func(s *serveSysInfo) { s.APIKey = "k" }, want: want},
		{name: "EXTRA_ARGS le pose déjà", cfg: map[string]string{"HOST": "127.0.0.1"},
			extra: []string{"--slot-save-path", "/ailleurs"}},
		{name: "CACHE_ISOLATE=off", cfg: map[string]string{"HOST": "127.0.0.1", "CACHE_ISOLATE": "off"}},
		{name: "cache coupé : rien à isoler", cfg: map[string]string{"HOST": "127.0.0.1", "CACHE_RAM": "0"}},
		{name: "cache coupé par EXTRA_ARGS", cfg: map[string]string{"HOST": "127.0.0.1"}, extra: []string{"-cram", "0"}},
		{name: "moteur sans le drapeau", cfg: map[string]string{"HOST": "127.0.0.1"},
			si: func(s *serveSysInfo) { s.Help = helpRecent }},
		{name: "dossier indisponible", cfg: map[string]string{"HOST": "127.0.0.1"},
			si: func(s *serveSysInfo) { s.SlotDir = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			si := serveSysInfo{Help: helpCacheRAM, SlotDir: "/data/slots", ArgEnv: map[string]string{}}
			if c.si != nil {
				c.si(&si)
			}
			if got := slotSaveArgs(c.cfg, c.extra, si); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("slotSaveArgs = %v, attendu %v", got, c.want)
			}
		})
	}
}

// Place dans la ligne complète : après --api-key, avant EXTRA_ARGS qui garde le
// dernier mot. Sans aide qui les connaisse, la ligne historique est intacte
// (TestBuildServeArgs).
func TestBuildServeArgsCacheRAM(t *testing.T) {
	si := denseGPU()
	si.APIKey, si.SlotDir = "k", "/data/slots"
	args, _, _ := buildServeArgs(map[string]string{"CTX": "65536", "NGL": "999"}, []string{"--jinja"}, "/bin/llama-server", si)
	got := strings.Join(args, " ")
	if !strings.HasSuffix(got, "--api-key k --cache-ram 19660 --slot-save-path /data/slots --jinja") {
		t.Fatalf("ligne = %s", got)
	}
}

func TestPrepareSlotDir(t *testing.T) {
	home := t.TempDir()
	dir := prepareSlotDir(home, false)
	if dir != filepath.Join(home, "slots") {
		t.Fatalf("dossier = %q", dir)
	}
	stray := filepath.Join(dir, "slot0.bin")
	if err := os.WriteFile(stray, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if prepareSlotDir(home, false) == "" {
		t.Fatal("second appel")
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("un fichier laissé dans slots/ doit être purgé au lancement")
	}
	// Un fichier à la place du dossier : pas de drapeau, plutôt qu'un moteur
	// qui refuse de démarrer.
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "slots"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := prepareSlotDir(bad, false); got != "" {
		t.Fatalf("slots est un fichier : %q", got)
	}
}
