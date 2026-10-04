package loki

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const gib = int64(1) << 30

// Un -ot ne compte comme experts en RAM que s'il vise les experts ET la RAM ;
// celui de la couche d'entrée (per_layer_token_embd) n'en est pas.
func TestCpuExperts(t *testing.T) {
	cases := []struct {
		extra string
		env   map[string]string
		want  bool
	}{
		{`-ot per_layer_token_embd.weight=CPU`, nil, false},
		{`-ot "blk\..*\.ffn_.*_exps\.=CPU"`, nil, true},
		{`--override-tensor=per_layer_token_embd.weight=CPU,\.ffn_(up|down)_exps=CPU`, nil, true},
		{`-ot \.ffn_.*_exps=CUDA1`, nil, false},
		{`-cmoe`, nil, true},
		{`--n-cpu-moe 0`, nil, false},
		{`--n-cpu-moe 40`, nil, true},
		{`-ncmoe=12`, nil, true},
		{``, map[string]string{"LLAMA_ARG_N_CPU_MOE": "20"}, true},
		{``, map[string]string{"LLAMA_ARG_CPU_MOE": "1"}, true},
		{`-nkvo -ngl 20`, nil, false},
	}
	for _, c := range cases {
		if got := cpuExperts(splitArgs(c.extra), c.env) != ""; got != c.want {
			t.Errorf("cpuExperts(%q, %v) = %v, want %v", c.extra, c.env, got, c.want)
		}
	}
}

// moeNotes : les avis, au bon endroit et seulement là.
func TestMoeNotes(t *testing.T) {
	moe := &GGUFInfo{ExpertCount: 128, BlockCount: 48}
	big := serveSysInfo{Help: helpFit, GGUF: moe, ModelBytes: 82 * gib, VRAMMiB: 28 << 10, GPUs: 2}
	cases := []struct {
		name string
		cfg  map[string]string
		si   serveSysInfo
		want string // sous-chaîne attendue ; vide = aucune note
	}{
		{name: "modèle dense : rien",
			cfg: map[string]string{"EXTRA_ARGS": "--n-cpu-moe 40"},
			si:  serveSysInfo{Help: helpFit, GGUF: &GGUFInfo{BlockCount: 64}, ModelBytes: 82 * gib, VRAMMiB: 28 << 10}},
		{name: "placement manuel, UBATCH 512 : ne pas monter UBATCH seul",
			cfg: map[string]string{"EXTRA_ARGS": "--n-cpu-moe 40"}, si: big, want: "monte --n-cpu-moe d'abord"},
		{name: "placement manuel, UBATCH 2048 : --fit ne les place pas, sans conseil de lot",
			cfg: map[string]string{"EXTRA_ARGS": "--n-cpu-moe 40", "UBATCH": "2048"}, si: big, want: "--fit ne les place pas"},
		{name: "placement auto, MoE plus gros que la VRAM, UBATCH 512 : UBATCH 2048+",
			cfg: map[string]string{}, si: big, want: "UBATCH 2048+ recommandé"},
		{name: "placement auto, UBATCH 4096 : rien",
			cfg: map[string]string{"UBATCH": "4096", "BATCH": "4096"}, si: big},
		{name: "-ub dans EXTRA_ARGS l'emporte sur UBATCH",
			cfg: map[string]string{"UBATCH": "512", "EXTRA_ARGS": "-ub 2048"}, si: big},
		{name: "le modèle tient en VRAM : rien",
			cfg: map[string]string{},
			si:  serveSysInfo{Help: helpFit, GGUF: moe, ModelBytes: 20 * gib, VRAMMiB: 28 << 10, GPUs: 2}},
		{name: "moteur sans --fit : garder --n-cpu-moe",
			cfg: map[string]string{}, si: serveSysInfo{Help: helpOld, GGUF: moe, ModelBytes: 82 * gib, VRAMMiB: 28 << 10},
			want: "garde --n-cpu-moe"},
		{name: "moteur sans --fit, NGL partiel : l'utilisateur a tranché",
			cfg: map[string]string{"NGL": "20"}, si: serveSysInfo{Help: helpOld, GGUF: moe, ModelBytes: 82 * gib, VRAMMiB: 28 << 10}},
		{name: "aide illisible : rien",
			cfg: map[string]string{}, si: serveSysInfo{GGUF: moe, ModelBytes: 82 * gib, VRAMMiB: 28 << 10}},
		{name: "-ot de la seule couche d'entrée : --fit coupé, experts tous sur GPU",
			cfg: map[string]string{"EXTRA_ARGS": "-ot per_layer_token_embd.weight=CPU"}, si: big,
			want: "-ot qui ne vise pas les experts"},
		{name: "VRAM inconnue : rien",
			cfg: map[string]string{}, si: serveSysInfo{Help: helpFit, GGUF: moe, ModelBytes: 82 * gib}},
		{name: "macOS, mémoire unifiée : rien",
			cfg: map[string]string{}, si: serveSysInfo{Help: helpFit, GGUF: moe, ModelBytes: 82 * gib, VRAMMiB: 28 << 10, UnifiedMem: true}},
		{name: "preset externe : rien",
			cfg: map[string]string{extKeyFlag: "1", extKeyURL: "https://api.example.com/v1", "EXTRA_ARGS": "--n-cpu-moe 40"}, si: big},
	}
	for _, c := range cases {
		notes := moeNotes(c.cfg, splitArgs(c.cfg["EXTRA_ARGS"]), c.si)
		switch {
		case c.want == "" && len(notes) != 0:
			t.Errorf("%s : notes inattendues %q", c.name, notes)
		case c.want != "" && (len(notes) != 1 || !strings.Contains(notes[0], c.want)):
			t.Errorf("%s : notes %q, attendu %q", c.name, notes, c.want)
		}
	}
	// Placement manuel en UBATCH 2048 : pas de conseil de lot.
	n := moeNotes(map[string]string{"UBATCH": "2048"}, splitArgs("--n-cpu-moe 40"), big)
	if len(n) != 1 || strings.Contains(n[0], "UBATCH") {
		t.Errorf("conseil de lot en trop : %q", n)
	}
}

// loadModeRisk : un avertissement d'ordinaire, un refus seulement quand la
// borne basse des poids en RAM dépasse 90 % de la RAM effective.
func TestLoadModeRisk(t *testing.T) {
	cases := []struct {
		name         string
		cfg          map[string]string
		args         string
		si           serveSysInfo
		warn, refuse bool
	}{
		{name: "mmap : rien",
			args: "--load-mode mmap", si: serveSysInfo{ModelBytes: 82 * gib, VRAMMiB: 16 << 10, RAMMiB: 60 << 10}},
		{name: "sans drapeau : rien",
			si: serveSysInfo{ModelBytes: 82 * gib, VRAMMiB: 16 << 10, RAMMiB: 60 << 10}},
		{name: "none, 66 Gio certains en RAM pour 60 : refus (#26110)",
			args: "--load-mode none", si: serveSysInfo{ModelBytes: 82 * gib, EngineVRAMMiB: 16 << 10, RAMMiB: 60 << 10}, refuse: true},
		{name: "même chose, LOAD_GUARD=off : avertissement seulement",
			cfg: map[string]string{"LOAD_GUARD": "off"}, args: "--load-mode none",
			si: serveSysInfo{ModelBytes: 82 * gib, EngineVRAMMiB: 16 << 10, RAMMiB: 60 << 10}, warn: true},
		{name: "mlock, 54 Gio en RAM pour 64 : avertissement",
			args: "--load-mode mlock", si: serveSysInfo{ModelBytes: 82 * gib, VRAMMiB: 28 << 10, RAMMiB: 64 << 10}, warn: true},
		{name: "dense tout en VRAM sur une machine à peu de RAM : rien (pas de faux positif)",
			args: "--load-mode none", si: serveSysInfo{ModelBytes: 16 * gib, VRAMMiB: 24 << 10, RAMMiB: 8 << 10}},
		{name: "VRAM inconnue : avertissement « jusqu'à », jamais de refus",
			args: "--load-mode dio", si: serveSysInfo{ModelBytes: 82 * gib, RAMMiB: 60 << 10}, warn: true},
		{name: "macOS, mémoire unifiée : tout le modèle compte",
			args: "--load-mode mmap+mlock", si: serveSysInfo{ModelBytes: 60 * gib, RAMMiB: 64 << 10, UnifiedMem: true}, refuse: true},
		{name: "moteur ancien : --no-mmap",
			args: "--no-mmap", si: serveSysInfo{ModelBytes: 82 * gib, EngineVRAMMiB: 16 << 10, RAMMiB: 60 << 10}, refuse: true},
		{name: "moteur ancien : --mlock --no-mmap",
			args: "--mlock --no-mmap", si: serveSysInfo{ModelBytes: 82 * gib, VRAMMiB: 28 << 10, RAMMiB: 64 << 10}, warn: true},
		{name: "LLAMA_ARG_NO_MMAP dans l'environnement",
			si: serveSysInfo{ModelBytes: 82 * gib, EngineVRAMMiB: 16 << 10, RAMMiB: 60 << 10,
				ArgEnv: map[string]string{"LLAMA_ARG_NO_MMAP": "1"}}, refuse: true},
		// Corrections de relecture du lot 2 : pas de refus sur une borne fausse.
		{name: "LLAMA_ARG_NO_MMAP sans effet sur un moteur à --load-mode : rien",
			si: serveSysInfo{Help: "--load-mode", ModelBytes: 82 * gib, VRAMMiB: 16 << 10, RAMMiB: 60 << 10,
				ArgEnv: map[string]string{"LLAMA_ARG_NO_MMAP": "1"}}},
		{name: "--rpc : une part des poids part ailleurs, avis seulement",
			args: "--load-mode none --rpc 10.0.0.2:50052", si: serveSysInfo{ModelBytes: 82 * gib, VRAMMiB: 16 << 10, RAMMiB: 60 << 10}, warn: true},
		{name: "LLAMA_ARG_RPC : de même",
			args: "--load-mode none", si: serveSysInfo{ModelBytes: 82 * gib, VRAMMiB: 16 << 10, RAMMiB: 60 << 10,
				ArgEnv: map[string]string{"LLAMA_ARG_RPC": "10.0.0.2:50052"}}, warn: true},
		{name: "limite du conteneur (RAM effective) : refus",
			args: "--load-mode none", si: serveSysInfo{ModelBytes: 40 * gib, EngineVRAMMiB: 16 << 10, RAMMiB: 16 << 10}, refuse: true},
		// Lot 3 : la VRAM comptée est celle des cartes du moteur ; nvidia-smi
		// seul ne fait jamais refuser.
		{name: "VRAM NVIDIA seule, cartes du moteur non lues : avertissement, jamais de refus",
			args: "--load-mode none", si: serveSysInfo{ModelBytes: 82 * gib, VRAMMiB: 16 << 10, RAMMiB: 60 << 10}, warn: true},
		{name: "Vulkan, NVIDIA 12 Gio + AMD 16 Gio listées par le moteur : rien (faux refus sur la seule VRAM NVIDIA)",
			args: "--load-mode none", si: serveSysInfo{ModelBytes: 70 * gib, VRAMMiB: 12 << 10, EngineVRAMMiB: 28 << 10, RAMMiB: 60 << 10}},
		{name: "taille inconnue : rien",
			args: "--load-mode none", si: serveSysInfo{VRAMMiB: 16 << 10, RAMMiB: 16 << 10}},
	}
	for _, c := range cases {
		cfg := c.cfg
		if cfg == nil {
			cfg = map[string]string{}
		}
		warn, refuse := loadModeRisk(cfg, splitArgs(c.args), c.si)
		if (warn != "") != c.warn || (refuse != "") != c.refuse {
			t.Errorf("%s : warn=%q refuse=%q", c.name, warn, refuse)
		}
		if refuse != "" && !strings.Contains(refuse, "LOAD_GUARD=off") {
			t.Errorf("%s : refus sans clé d'échappement : %q", c.name, refuse)
		}
	}
}

// Sans ces clés, la ligne et l'environnement ne bougent pas quand le modèle
// est un MoE : seules des notes s'ajoutent.
func TestBuildServeArgsMoeNotesOnly(t *testing.T) {
	const bin, model = "/opt/llama/llama-server", "/models/Qwen-MoE-Q4.gguf"
	cfg := map[string]string{"CTX": "65536", "EXTRA_ARGS": "--n-cpu-moe 40 --load-mode none"}
	extra := splitArgs(cfg["EXTRA_ARGS"])
	plain := serveSysInfo{Help: helpFit, Model: model}
	moe := plain
	moe.GGUF, moe.ModelBytes, moe.VRAMMiB, moe.RAMMiB = &GGUFInfo{ExpertCount: 128}, 82*gib, 28<<10, 64<<10
	a1, e1, n1 := buildServeArgs(cfg, extra, bin, plain)
	a2, e2, n2 := buildServeArgs(cfg, extra, bin, moe)
	if !reflect.DeepEqual(a1, a2) || !reflect.DeepEqual(e1, e2) {
		t.Fatalf("la ligne a bougé :\n%q\n%q\nenv %v / %v", a1, a2, e1, e2)
	}
	if len(n2) != len(n1)+2 {
		t.Fatalf("notes attendues (placement manuel + chargement) : %q", n2)
	}
}

// La copie en placement auto du preset MoE de l'utilisateur.
func TestAutoPlacementPreset(t *testing.T) {
	src := "# NAME=QWEN 3.6 MOE\n# experts à la main\nMODEL=Qwen-MoE-Q4.gguf\nCTX=65536\nNGL=999\nUBATCH=512\n" +
		`EXTRA_ARGS="-ot per_layer_token_embd.weight=CPU --n-cpu-moe 40 --cache-type-k q8_0 --cache-type-v q8_0 --jinja --mlock --chat-template-file '/mes modèles/tpl.jinja'"` + "\n"
	out, changes, err := autoPlacementPreset(src)
	if err != nil {
		t.Fatal(err)
	}
	cfg := parseEnv(out)
	wantExtra := []string{"--cache-type-k", "q8_0", "--cache-type-v", "q8_0", "--jinja",
		"--chat-template-file", "/mes modèles/tpl.jinja", "--load-mode", "mmap"}
	if got := splitArgs(cfg["EXTRA_ARGS"]); !reflect.DeepEqual(got, wantExtra) {
		t.Errorf("EXTRA_ARGS = %q\nwant %q", got, wantExtra)
	}
	if cfg["UBATCH"] != "2048" || cfg["BATCH"] != "4096" || cfg["CTX"] != "65536" || cfg["MODEL"] != "Qwen-MoE-Q4.gguf" {
		t.Errorf("clés : %v", cfg)
	}
	if _, ok := cfg["NGL"]; ok {
		t.Errorf("NGL gardé : %v", cfg)
	}
	if !strings.Contains(out, "# experts à la main") || !strings.HasPrefix(out, "# NAME=QWEN 3.6 MOE") {
		t.Errorf("commentaires perdus :\n%s", out)
	}
	if len(changes) == 0 || !strings.Contains(strings.Join(changes, "\n"), "cache KV conservé") {
		t.Errorf("changements : %q", changes)
	}
	// L'original n'est pas touché (chaîne passée par valeur, mais on vérifie la
	// fonction : elle ne rend rien d'autre que la copie).
	if parseEnv(src)["UBATCH"] != "512" {
		t.Error("source modifiée")
	}

	refus := []struct{ name, content string }{
		{"rien à migrer", "CTX=65536\nEXTRA_ARGS=--jinja\n"},
		{"CTX=0", "CTX=0\nEXTRA_ARGS=--n-cpu-moe 40\n"},
		{"-ot hors experts restant", `CTX=65536` + "\n" + `EXTRA_ARGS="-ot blk\.0\.attn.*=CUDA0,\.ffn_.*_exps=CPU"` + "\n"},
		{"-sm row", "CTX=65536\nEXTRA_ARGS=\"--n-cpu-moe 40 -sm row\"\n"},
		{"SPLIT_MODE=tensor", "CTX=65536\nSPLIT_MODE=tensor\nEXTRA_ARGS=--n-cpu-moe 40\n"},
		{"--n-cpu-ffn", "CTX=65536\nEXTRA_ARGS=\"--n-cpu-moe 40 --n-cpu-ffn 4\"\n"},
		{"preset externe", extKeyFlag + "=1\n" + extKeyURL + "=https://api.example.com/v1\nEXTRA_ARGS=--n-cpu-moe 40\n"},
	}
	for _, r := range refus {
		if _, _, err := autoPlacementPreset(r.content); err == nil {
			t.Errorf("%s : accepté", r.name)
		}
	}
	// --cpu-moe et -ngl / --tensor-split / --fit off retirés ensemble.
	out, _, err = autoPlacementPreset("CTX=32768\nEXTRA_ARGS=\"-cmoe -ngl 99 -ts 1,1 --fit off -ub 512\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := parseEnv(out)["EXTRA_ARGS"]; got != "--load-mode mmap" {
		t.Errorf("EXTRA_ARGS = %q", got)
	}
}

// presetSetKey : remplace, ajoute, retire — sans toucher aux voisines (BATCH
// n'est pas UBATCH) ni aux commentaires.
func TestPresetSetKey(t *testing.T) {
	src := "# BATCH=1\nUBATCH=512\nBATCH=2048\n"
	if got := presetSetKey(src, "BATCH", "4096"); got != "# BATCH=1\nUBATCH=512\nBATCH=4096\n" {
		t.Errorf("remplacement : %q", got)
	}
	if got := presetSetKey(src, "NGL", "auto"); got != src+"NGL=auto\n" {
		t.Errorf("ajout : %q", got)
	}
	if got := presetSetKey("A=1", "B", "2"); got != "A=1\nB=2\n" {
		t.Errorf("ajout sans fin de ligne : %q", got)
	}
	if got := presetSetKey(src, "UBATCH", ""); got != "# BATCH=1\nBATCH=2048\n" {
		t.Errorf("retrait : %q", got)
	}
	if got := presetSetKey("X=\"a b\"\n", "X", "c d"); parseEnv(got)["X"] != "c d" {
		t.Errorf("guillemets : %q", got)
	}
	// Clé en double : parseEnv retient la dernière, toutes sont réécrites.
	if got := presetSetKey("NGL=999\nCTX=1\nexport NGL=40\n", "NGL", "auto"); parseEnv(got)["NGL"] != "auto" {
		t.Errorf("doublon : %q", got)
	}
	if got := presetSetKey("NGL=999\nCTX=1\nNGL=40", "NGL", ""); got != "CTX=1\n" {
		t.Errorf("retrait des doublons : %q", got)
	}
}

// joinArgs relu par splitArgs rend les mêmes arguments.
func TestJoinArgsRoundTrip(t *testing.T) {
	args := []string{"--jinja", "--chat-template-file", "/mes modèles/tpl.jinja", "-ot", `blk\.(1|2)\.ffn=CPU`, "", "it's here"}
	if got := splitArgs(joinArgs(args)); !reflect.DeepEqual(got, args) {
		t.Errorf("aller-retour : %q", got)
	}
}

// Le refus de LOAD_GUARD s'arrête avant tout chargement : l'interface doit le
// dire plutôt qu'un « chargement… » sans fin, même après un chargement réussi
// plus haut dans le journal.
func TestModelLoadErrorLoadGuard(t *testing.T) {
	_, refuse := loadModeRisk(map[string]string{}, splitArgs("--load-mode none"),
		serveSysInfo{ModelBytes: 82 * gib, EngineVRAMMiB: 16 << 10, RAMMiB: 60 << 10})
	log := "llama_model_load: loading model\nmain: model loaded\nserver is listening\n[ERREUR] " + refuse + "\n"
	if got := modelLoadErrorFrom(log); !strings.Contains(got, "LOAD_GUARD=off") {
		t.Errorf("refus non signalé : %q", got)
	}
	// Une tentative suivante qui charge efface le refus.
	if got := modelLoadErrorFrom(log + "loading model\nmodel loaded\n"); got != "" {
		t.Errorf("refus ancien encore signalé : %q", got)
	}
}

// Un refus de LOAD_GUARD ne doit pas être relancé en boucle : l'unité systemd
// écrite par « loki install » exclut son code de sortie, une unité d'avant ne
// le fait pas.
func TestUnitPreventsRefusalRestart(t *testing.T) {
	for _, c := range []struct {
		unit string
		want bool
	}{
		{"[Service]\nRestart=on-failure\nRestartSec=3\n", false},
		{"[Service]\nRestart=on-failure\nRestartPreventExitStatus=78\n", true},
		{"[Service]\n  RestartPreventExitStatus = 1 78 SIGKILL\n", true},
		{"[Service]\nRestartPreventExitStatus=178\n", false},
		{"[Service]\n# RestartPreventExitStatus=78\n", false},
	} {
		if got := unitPreventsRefusalRestart(c.unit); got != c.want {
			t.Errorf("%q : %v, attendu %v", c.unit, got, c.want)
		}
	}
}

// Le refus sort avec son propre code (pas 1), y compris enveloppé ; une erreur
// ordinaire garde 1. Le moteur d'essai garde toujours le vrai code.
func TestLoadGuardRefusalExitStatus(t *testing.T) {
	err := loadGuardRefusal("refus ; "+loadGuardMarker, true)
	if got := exitStatusOf(err); got != loadGuardExitStatus {
		t.Errorf("essai : code %d, attendu %d", got, loadGuardExitStatus)
	}
	if !strings.Contains(err.Error(), loadGuardMarker) {
		t.Errorf("raison perdue : %q", err)
	}
	if got := exitStatusOf(fmt.Errorf("enveloppé : %w", err)); got != loadGuardExitStatus {
		t.Errorf("enveloppé : code %d", got)
	}
	if got := exitStatusOf(errors.New("modèle introuvable")); got != 1 {
		t.Errorf("erreur ordinaire : code %d, attendu 1", got)
	}
	// Hors superviseur (les tests ne tournent ni sous systemd ni sous launchd) :
	// le vrai code aussi.
	if restarts, _ := supervisorRestartsRefusal(); !restarts {
		if got := exitStatusOf(loadGuardRefusal("refus", false)); got != loadGuardExitStatus {
			t.Errorf("hors superviseur : code %d", got)
		}
	}
}

// Le refus envisagé sur la VRAM NVIDIA attend les cartes du moteur ; rien
// n'est lu sans refus en vue.
func TestLoadGuardNeedsDevices(t *testing.T) {
	none := splitArgs("--load-mode none")
	base := serveSysInfo{ModelBytes: 82 * gib, VRAMMiB: 16 << 10, RAMMiB: 60 << 10}
	if !loadGuardNeedsDevices(map[string]string{}, none, base) {
		t.Error("refus en vue sur nvidia-smi : cartes du moteur non demandées")
	}
	confirmed := base
	confirmed.EngineVRAMMiB = 16 << 10
	for name, c := range map[string]struct {
		cfg  map[string]string
		args []string
		si   serveSysInfo
	}{
		"déjà lues":      {map[string]string{}, none, confirmed},
		"LOAD_GUARD=off": {map[string]string{"LOAD_GUARD": "off"}, none, base},
		"mmap":           {map[string]string{}, splitArgs("--load-mode mmap"), base},
		"VRAM inconnue":  {map[string]string{}, none, serveSysInfo{ModelBytes: 82 * gib, RAMMiB: 60 << 10}},
		"loin du seuil":  {map[string]string{}, none, serveSysInfo{ModelBytes: 30 * gib, VRAMMiB: 16 << 10, RAMMiB: 60 << 10}},
	} {
		if loadGuardNeedsDevices(c.cfg, c.args, c.si) {
			t.Errorf("%s : cartes demandées sans refus en vue", name)
		}
	}
}

// La VRAM des cartes du moteur : toutes additionnées, inconnue dès qu'une
// carte annonce 0 Mio (déjà pleine) ; devices.json relu en JSON (float64).
func TestEngineDevsVRAM(t *testing.T) {
	mixed := splitDevsFrom([]map[string]any{
		{"id": "Vulkan0", "total_mib": 12288},
		{"id": "Vulkan1", "total_mib": float64(16368)},
	})
	if got := engineDevsVRAM(mixed); got != 12288+16368 {
		t.Errorf("cartes mixtes : %d", got)
	}
	if got := engineDevsVRAM([]splitDev{{"CUDA0", 12288}, {"CUDA1", 0}}); got != 0 {
		t.Errorf("carte à 0 Mio : %d, attendu inconnu", got)
	}
	if got := engineDevsVRAM(nil); got != 0 {
		t.Errorf("liste vide : %d", got)
	}
}
