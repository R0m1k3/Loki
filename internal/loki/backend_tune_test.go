package loki

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Aide d'un moteur récent, réduite à ce que lisent les tests de capacité.
const tuneTestHelp = `-ngl,   --gpu-layers, --n-gpu-layers N   max. number of layers to store in VRAM, either an exact number, 'auto', or 'all'
-fit,   --fit [on|off]                      whether to adjust unset arguments to fit in device memory
-fitt,  --fit-target MiB0,MiB1,...          target margin per device for --fit
--spec-type [none,draft-simple,draft-mtp,ngram-mod]
--spec-draft-n-max N
-bs,    --backend-sampling
--load-mode {auto,none,mmap,mlock,mmap+mlock,dio}
`

// Le découpage avec positions doit donner exactement les jetons de splitArgs :
// c'est ce qui garantit qu'une réécriture ne touche qu'aux jetons visés.
func TestTuneArgSpansMatchSplitArgs(t *testing.T) {
	for _, s := range []string{
		"", "  ", "-b 2048", `--chat-template-file "/mes modèles/tpl.jinja" -ub 512`,
		`-ot 'blk\.(1[0-9])\.ffn_.*_exps\.=CPU' --n-cpu-moe 40`, `"" -x`, `""x y`, `a""b`, `'a b'c "d"`,
		"--cache-type-k q8_0\t--flash-attn on\n-t 8", `"non fermé -b 4`, `x"y z"w -ub=1024`,
	} {
		var got []string
		for _, sp := range argSpans(s) {
			got = append(got, sp.val)
		}
		want := splitArgs(s)
		if len(got) == 0 && len(want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q : argSpans %q, splitArgs %q", s, got, want)
		}
	}
}

// Les jetons hors liste blanche ressortent à l'octet près, guillemets compris,
// quelle que soit la réécriture.
func TestTuneExtraEditsKeepOtherTokens(t *testing.T) {
	keep := []string{"--cache-type-k q8_0", "--flash-attn on", `--chat-template-file "/a b/t.jinja"`, "-ctv q8_0"}
	extra := `-ot "blk\.([0-9]+)\.ffn_.*_exps\.=CPU,per_layer_token_embd=CPU" --cache-type-k q8_0 --n-cpu-moe 40 ` +
		`--flash-attn on -ub 512 --chat-template-file "/a b/t.jinja" -ts 0.9,0.1 -ctv q8_0 -cmoe`
	edits := map[string]string{
		"set -ub":     tuneExtraSet(extra, knobUBatch.flags, "2048"),
		"drop -ub":    tuneExtraSet(extra, knobUBatch.flags, ""),
		"add -t":      tuneExtraSet(extra, knobThreads.flags, "7"),
		"drop -ts":    tuneExtraDrop(extra, 1, "-ts", "--tensor-split"),
		"add switch":  tuneExtraAddSwitch(extra, "--backend-sampling"),
		"drop expert": func() string { s, _ := tuneExtraDropExperts(extra); return s }(),
	}
	for name, out := range edits {
		for _, k := range keep {
			if !strings.Contains(out, k) {
				t.Errorf("%s : %q perdu dans %q", name, k, out)
			}
		}
		if err := tuneCfgCheck(map[string]string{"EXTRA_ARGS": extra}, map[string]string{"EXTRA_ARGS": out}, true); err != nil {
			t.Errorf("%s : %v (%q)", name, err, out)
		}
	}
	if out := edits["set -ub"]; !strings.Contains(out, "-ub 2048") || strings.Contains(out, "-ub 512") {
		t.Errorf("-ub non réglé sur place : %q", out)
	}
	out, removed := tuneExtraDropExperts(extra)
	if strings.Contains(out, "exps") || strings.Contains(out, "--n-cpu-moe") || strings.Contains(out, "-cmoe") {
		t.Errorf("experts encore placés à la main : %q", out)
	}
	if !strings.Contains(out, "-ot per_layer_token_embd=CPU") || len(removed) != 3 {
		t.Errorf("élément non expert du -ot perdu, ou retraits mal comptés : %q %q", out, removed)
	}
	if got := tuneExtraSet("-ub=512 -b 4096", knobUBatch.flags, "1024"); got != "-ub=1024 -b 4096" {
		t.Errorf("forme -ub= : %q", got)
	}
}

// Un drapeau interdit (cache KV, flash-attn, contexte…) ne fait jamais partie de
// la liste blanche, et une clé intouchable n'est jamais réglable.
func TestTuneWhitelistDisjoint(t *testing.T) {
	for _, f := range tuneForbiddenFlags {
		if tuneFlagFor(f, true) != nil {
			t.Errorf("%s est à la fois interdit et réglable", f)
		}
	}
	for _, k := range tuneNeverKeys {
		if tuneKeys[k] || tuneOptInKeys[k] {
			t.Errorf("%s est à la fois intouchable et réglable", k)
		}
	}
	base := map[string]string{"CTX": "32768", "EXTRA_ARGS": "-ub 512 --cache-type-k q8_0"}
	for _, c := range []map[string]string{
		{"CTX": "65536", "EXTRA_ARGS": "-ub 512 --cache-type-k q8_0"},
		{"CTX": "32768", "EXTRA_ARGS": "-ub 512 --cache-type-k f16"},
		{"CTX": "32768", "EXTRA_ARGS": "-ub 512"},
		{"CTX": "32768", "EXTRA_ARGS": "-ub 512 --cache-type-k q8_0", "KV_TYPE": "q4_0"},
		{"CTX": "32768", "EXTRA_ARGS": "-ub 512 --cache-type-k q8_0", "SPEC": "auto"}, // opt-in non coché
	} {
		if tuneCfgCheck(base, c, false) == nil {
			t.Errorf("changement interdit accepté : %v", c)
		}
	}
	if err := tuneCfgCheck(base, map[string]string{"CTX": "32768", "EXTRA_ARGS": "-ub 512 --cache-type-k q8_0", "SPEC": "auto"}, true); err != nil {
		t.Errorf("SPEC refusé avec l'opt-in : %v", err)
	}
}

func tuneRichCfg() map[string]string {
	return map[string]string{
		"MODEL": "big.gguf", "CTX": "65536", "KV_TYPE": "q8_0", "MMPROJ": "mmproj.gguf", "REASONING": "on",
		"REASONING_BUDGET": "-1", "TEMP": "0.6", "TOP_K": "20", "PARALLEL": "1", "SPEC": "mtp",
		"EXTRA_ARGS": `-ot "blk\.([0-9]+)\.ffn_.*_exps\.=CPU" --n-cpu-moe 40 --cache-type-k q8_0 --flash-attn on -ub 512 -t 8 -ts 0.6,0.4`,
	}
}

func tuneRichEnv() tuneEnv {
	return tuneEnv{Help: tuneTestHelp, GPUs: 2, VRAMMiB: 28 << 10, ModelBytes: 80 << 30, MoE: true, PhysCores: 8,
		SMI: true, Placement: true, OptIn: true}
}

// La grille : chaque essai ne change que des clés et des drapeaux permis, ne
// touche à aucune clé intouchable, et garde les drapeaux interdits à l'octet.
func TestTuneGridRespectsNeverTouch(t *testing.T) {
	base := tuneRichCfg()
	env := tuneRichEnv()
	total := 0
	for _, st := range tuneStages {
		cands, why := st.Gen(base, env)
		if why != "" {
			continue
		}
		for _, c := range cands {
			total++
			if err := tuneCfgCheck(base, c.Cfg, env.OptIn); err != nil {
				t.Errorf("%s / %s : %v", st.Name, c.Label, err)
			}
			for _, k := range tuneNeverKeys {
				if c.Cfg[k] != base[k] {
					t.Errorf("%s / %s : %s modifié (%q → %q)", st.Name, c.Label, k, base[k], c.Cfg[k])
				}
			}
			for _, k := range []string{"--cache-type-k q8_0", "--flash-attn on"} {
				if !strings.Contains(c.Cfg["EXTRA_ARGS"], k) {
					t.Errorf("%s / %s : %q perdu", st.Name, c.Label, k)
				}
			}
		}
	}
	if total == 0 {
		t.Fatal("aucun essai généré")
	}
}

// Chaque axe n'existe que si le moteur, la machine et l'utilisateur le
// permettent.
func TestTuneGridGating(t *testing.T) {
	base := tuneRichCfg()
	gen := func(name string, env tuneEnv) ([]tuneCand, string) {
		for _, st := range tuneStages {
			if st.Name == name {
				return st.Gen(base, env)
			}
		}
		t.Fatalf("étape %s inconnue", name)
		return nil, ""
	}
	env := tuneRichEnv()
	env.Placement = false
	if _, why := gen("placement", env); why == "" {
		t.Error("placement essayé sans « inclure le placement »")
	}
	env = tuneRichEnv()
	env.Help = strings.ReplaceAll(tuneTestHelp, "--fit", "--nofit")
	if _, why := gen("placement", env); why == "" {
		t.Error("placement essayé sur un moteur sans --fit")
	}
	env = tuneRichEnv()
	if c, why := gen("placement", env); why != "" || len(c) != 1 {
		t.Fatalf("placement attendu : %q", why)
	}
	// Marges --fit : refusées tant que le placement manuel coupe --fit, ou sans
	// --fit-target dans l'aide.
	if _, why := gen("marges", env); why == "" {
		t.Error("marges essayées alors que -ot / -ts coupent --fit")
	}
	plain := map[string]string{"MODEL": "m.gguf", "CTX": "32768"}
	for _, st := range tuneStages {
		if st.Name != "marges" {
			continue
		}
		if c, why := st.Gen(plain, env); why != "" || len(c) != 3 {
			t.Errorf("marges sur 2 cartes : %d essai(s), %q", len(c), why)
		}
		e2 := env
		e2.Help = strings.ReplaceAll(tuneTestHelp, "--fit-target", "--fit-x")
		if _, why := st.Gen(plain, e2); why == "" {
			t.Error("marges essayées sans --fit-target")
		}
		e2 = env
		e2.SMI = false
		if _, why := st.Gen(plain, e2); why == "" {
			t.Error("marges essayées sans mesure de VRAM")
		}
	}
	// Lots : sans mesure de VRAM, jamais de micro-lot plus grand.
	env = tuneRichEnv()
	env.SMI = false
	cands, _ := gen("lots", env)
	for _, c := range cands {
		if serveUBatch(c.Cfg, splitArgs(c.Cfg["EXTRA_ARGS"])) > 512 {
			t.Errorf("micro-lot monté sans mesure de VRAM : %s", c.Label)
		}
	}
	// -ub dans EXTRA_ARGS : réglé là, jamais par une clé qu'il écraserait.
	cands, _ = gen("lots", tuneRichEnv())
	for _, c := range cands {
		if c.Cfg["UBATCH"] != "" {
			t.Errorf("%s : UBATCH posé alors qu'EXTRA_ARGS fixe -ub", c.Label)
		}
	}
	// Deux cartes tout GPU : le lot reste plus grand que le micro-lot.
	dense := map[string]string{"MODEL": "m.gguf"}
	env2 := tuneEnv{Help: tuneTestHelp, GPUs: 2, VRAMMiB: 28 << 10, ModelBytes: 10 << 30, SMI: true}
	for _, st := range tuneStages {
		if st.Name != "lots" {
			continue
		}
		cs, _ := st.Gen(dense, env2)
		for _, c := range cs {
			b, _ := strconv.Atoi(knobBatch.get(c.Cfg))
			ub, _ := strconv.Atoi(knobUBatch.get(c.Cfg))
			if b <= ub {
				t.Errorf("%s : lot pas plus grand que le micro-lot sur 2 cartes", c.Label)
			}
		}
	}
	for _, name := range []string{"threads", "délestage"} {
		for _, st := range tuneStages {
			if st.Name == name {
				if _, why := st.Gen(dense, env2); why == "" {
					t.Errorf("%s essayé alors que tous les poids sont sur GPU", name)
				}
			}
		}
	}
	// Opt-in : rien sans la case ; avec, --backend-sampling et SPEC_N_MAX.
	env = tuneRichEnv()
	env.OptIn = false
	if _, why := gen("opt-in", env); why == "" {
		t.Error("opt-in essayé sans la case")
	}
	cands, _ = gen("opt-in", tuneRichEnv())
	labels := map[string]bool{}
	for _, c := range cands {
		labels[c.Label] = true
	}
	for _, want := range []string{"--backend-sampling", "SPEC_N_MAX=2", "SPEC_N_MAX=4", "CUDA_GRAPH_OPT=on"} {
		if !labels[want] {
			t.Errorf("opt-in : %s manquant (%v)", want, labels)
		}
	}
	// Threads : « omis » = aucun -t, jamais THREADS=0.
	cands, _ = gen("threads", tuneRichEnv())
	found := false
	for _, c := range cands {
		if c.Label == "threads omis (cœurs physiques)" {
			found = true
			if hasAnyFlag(splitArgs(c.Cfg["EXTRA_ARGS"]), "-t") || c.Cfg["THREADS"] != "" {
				t.Errorf("threads omis mais -t ou THREADS encore là : %v", c.Cfg)
			}
		}
	}
	if !found {
		t.Error("candidat « threads omis » absent")
	}
}

// La comparaison des lignes de commande : un réglage permis passe, un drapeau
// de fidélité ou une variable non réglée changés sont « dénature ».
func TestTuneDenature(t *testing.T) {
	base := tuneLaunch{Args: []string{"/bin/llama-server", "-m", "/m.gguf", "-c", "65536", "-b", "2048", "-ub", "512",
		"--host", "127.0.0.1", "--port", "41000", "--parallel", "1", "-ctk", "q8_0", "-ot", "exps=CPU,per_layer_token_embd=CPU",
		"--cache-ram", "8192", "--flash-attn", "on"},
		Env: map[string]string{"CUDA_VISIBLE_DEVICES": "1,0", "CUDA_SCALE_LAUNCH_QUEUES": "4x"}}
	ok := base
	ok.Args = []string{"/bin/llama-server", "-m", "/m.gguf", "-c", "65536", "-b", "4096", "-ub", "2048",
		"--host", "127.0.0.1", "--port", "41001", "--parallel", "1", "-ctk", "q8_0", "-ot", "per_layer_token_embd=CPU",
		"--cache-ram", "12000", "--flash-attn", "on", "-t", "7"}
	ok.Env = map[string]string{"CUDA_VISIBLE_DEVICES": "1,0", "GGML_OP_OFFLOAD_MIN_BATCH": "128"}
	if why := tuneDenature(base, ok, false); why != "" {
		t.Errorf("réglages permis jugés dénaturants : %s", why)
	}
	for name, mut := range map[string]func(l *tuneLaunch){
		"ctx":     func(l *tuneLaunch) { l.Args[4] = "32768" },
		"kv":      func(l *tuneLaunch) { l.Args[16] = "f16" },
		"ot":      func(l *tuneLaunch) { l.Args[18] = "exps=CPU" },
		"fa":      func(l *tuneLaunch) { l.Args[len(l.Args)-3] = "off" },
		"slots":   func(l *tuneLaunch) { l.Args[14] = "2" },
		"cuda":    func(l *tuneLaunch) { l.Env["CUDA_VISIBLE_DEVICES"] = "0,1" },
		"graphes": func(l *tuneLaunch) { l.Env["GGML_CUDA_GRAPH_OPT"] = "1" },
		"spec":    func(l *tuneLaunch) { l.Args = append(l.Args, "--spec-draft-n-max", "4") },
	} {
		c := ok
		c.Args = append([]string(nil), ok.Args...)
		c.Env = map[string]string{}
		for k, v := range ok.Env {
			c.Env[k] = v
		}
		mut(&c)
		if tuneDenature(base, c, false) == "" {
			t.Errorf("%s : changement non vu", name)
		}
	}
	// Opt-in coché : la spéculation et les graphes CUDA deviennent réglables.
	c := ok
	c.Args = append(append([]string(nil), ok.Args...), "--spec-draft-n-max", "4")
	c.Env = map[string]string{"CUDA_VISIBLE_DEVICES": "1,0", "GGML_CUDA_GRAPH_OPT": "1"}
	if why := tuneDenature(base, c, true); why != "" {
		t.Errorf("opt-in : %s", why)
	}
}

func TestTuneLogFacts(t *testing.T) {
	log := "load_tensors: offloaded 40/49 layers to GPU\n" +
		"llama_kv_cache: size = 1024.00 MiB (  8192 cells,  32 layers,  1/1 seqs), K (q8_0):  512.00 MiB, V (q8_0):  512.00 MiB\n" +
		"load_tensors: offloaded 49/49 layers to GPU\n"
	off, total, kv := tuneLogFacts(log)
	if off != 49 || total != 49 || kv != "q8_0/q8_0" {
		t.Errorf("faits du journal : %d/%d %q", off, total, kv)
	}
	if off, _, kv := tuneLogFacts("rien"); off != -1 || kv != "" {
		t.Errorf("journal vide : %d %q", off, kv)
	}
	if !strings.Contains(tuneLoadFailure("ggml_backend_cuda_buffer_type_alloc_buffer: cudaMalloc failed: out of memory"), "OOM") {
		t.Error("OOM non reconnu")
	}
}

// Le score et le départage : un gain ne compte qu'au-delà de 3 % ET de
// l'écart entre passages, et sur deux passages.
func TestTuneScoreAndTieBreak(t *testing.T) {
	w := tuneWeights{K: 2000, G: 600, FCold: 0.05}
	res := &benchResult{PromptPerSecond: 1000, PredictedPerSec: 50, Depth: &benchDepth{Target: 32768,
		Cold:         &benchTurn{PromptPerSecond: 800},
		DecodePerSec: 40, CachedPerSec: 999,
		Turns: []benchTurn{{New: 2100, Cached: 30000, Expected: 30048, PromptMs: 2000},
			{New: 2100, Cached: 32300, Expected: 32348, PromptMs: 2000}}}}
	r, err := tuneRunFrom(res, w)
	if err != nil {
		t.Fatal(err)
	}
	// Jetons neufs réels : (2100+30000-30048)+(2100+32300-32348) = 4104 en 4 s.
	if r.CachedPP < 1025 || r.CachedPP > 1027 {
		t.Errorf("prefill des tours : %.1f", r.CachedPP)
	}
	want := 2000/r.CachedPP + 600/40.0 + 0.05*32768/800
	if d := r.TurnSec - want; d > 1e-9 || d < -1e-9 {
		t.Errorf("durée d'un tour : %.3f au lieu de %.3f", r.TurnSec, want)
	}
	res.Depth.Partial = "contexte plein"
	if _, err := tuneRunFrom(res, w); err == nil {
		t.Error("mesure partielle acceptée")
	}
	base := []float64{20, 20.4}
	if _, _, ok := tuneWins(base, []float64{19.7, 19.8}); ok {
		t.Error("gain de ~2 % accepté (sous le seuil)")
	}
	if _, _, ok := tuneWins(base, []float64{18}); ok {
		t.Error("un seul passage suffit à gagner")
	}
	if g, _, ok := tuneWins(base, []float64{18, 18.1}); !ok || g < 0.09 {
		t.Errorf("gain net refusé (%.3f)", g)
	}
	// Écart entre passages plus grand que le gain : refusé.
	if _, need, ok := tuneWins([]float64{18, 22}, []float64{18.5, 18.6}); ok || need < 0.19 {
		t.Errorf("bruit non pris en compte (seuil %.2f)", need)
	}
	if !tuneWorthRepeat(base, 19) || tuneWorthRepeat(base, 19.8) {
		t.Error("confirmation mal décidée")
	}
}

// La réécriture du preset ne change que les clés réglées ; commentaires, NAME
// et autres clés restent, EXTRA_ARGS garde ses jetons interdits.
func TestTunePatchPreset(t *testing.T) {
	content := "# NAME=Gros MoE\n# commentaire\nMODEL=big.gguf\nCTX=65536\nUBATCH=512\n" +
		`EXTRA_ARGS="--cache-type-k q8_0 --flash-attn on -t 8"` + "\n"
	base := parseEnv(content)
	win := knobThreads.set(base, "")
	win = setKey(win, "UBATCH", "2048")
	win = setKey(win, "BATCH", "4096")
	out, err := tunePatchPreset(content, base, win)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"# NAME=Gros MoE", "# commentaire", "MODEL=big.gguf", "CTX=65536", "UBATCH=2048", "BATCH=4096",
		"--cache-type-k q8_0 --flash-attn on"} {
		if !strings.Contains(out, s) {
			t.Errorf("%q absent de :\n%s", s, out)
		}
	}
	if strings.Contains(out, "-t 8") {
		t.Errorf("-t non retiré :\n%s", out)
	}
	bad := setKey(base, "CTX", "32768")
	if _, err := tunePatchPreset(content, base, bad); err == nil {
		t.Error("clé intouchable réécrite")
	}
}

// Le verrou : exclusif entre processus, refusé tant que son propriétaire vit,
// écarté quand il est mort ; et serviceAction refuse tout démarrage pendant ce
// temps, sur toutes les plateformes, avant de toucher au système.
func TestTuneLock(t *testing.T) {
	testHome(t)
	alive := true
	prev := tuneProcAlive
	tuneProcAlive = func(p tuneProc) bool { return alive && p.PID == os.Getpid() }
	t.Cleanup(func() { tuneProcAlive = prev })

	if err := tuneGuard(); err != nil {
		t.Fatalf("garde levée sans verrou : %v", err)
	}
	l, err := tuneLockAcquire("cli", true)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(tuneGuard(), errTuneBusy) {
		t.Error("garde muette pendant l'optimisation")
	}
	if _, err := tuneLockAcquire("web", false); err == nil {
		t.Error("second verrou accordé")
	}
	for _, a := range []string{"start", "restart"} {
		if err := serviceAction(a); !errors.Is(err, errTuneBusy) {
			t.Errorf("serviceAction(%s) pendant l'optimisation : %v", a, err)
		}
	}
	if err := SwitchToPreset(filepath.Join(t.TempDir(), "x.env")); !errors.Is(err, errTuneBusy) {
		t.Errorf("bascule de preset pendant l'optimisation : %v", err)
	}
	l.setTrial(&tuneProc{PID: 999999, Start: "x"})
	if o, ok := readTuneOwner(tuneLockPath()); !ok || o.Trial == nil || o.Trial.PID != 999999 || !o.MainWasActive {
		t.Errorf("essai non noté dans le verrou : %+v", o)
	}
	l.release()
	if _, err := os.Stat(tuneLockPath()); !os.IsNotExist(err) {
		t.Error("verrou non rendu")
	}

	// Propriétaire mort : le verrou est périmé, il ne bloque rien et cède la
	// place. L'essai noté n'est pas tué (identité non concordante).
	l, err = tuneLockAcquire("cli", true)
	if err != nil {
		t.Fatal(err)
	}
	l.setTrial(&tuneProc{PID: 999999, Start: "x"})
	alive = false
	if err := tuneGuard(); err != nil {
		t.Errorf("verrou périmé encore bloquant : %v", err)
	}
	if !tuneReapStale() {
		t.Error("MainWasActive perdu au nettoyage")
	}
	if _, err := os.Stat(tuneLockPath()); !os.IsNotExist(err) {
		t.Error("verrou périmé non retiré")
	}
	alive = true
	l2, err := tuneLockAcquire("web", false)
	if err != nil {
		t.Fatalf("verrou refusé après nettoyage : %v", err)
	}
	l2.release()
	// Un verrou d'un autre propriétaire n'est jamais retiré par release.
	l3, _ := tuneLockAcquire("cli", false)
	other := &tuneLock{path: tuneLockPath(), owner: tuneOwner{Owner: tuneProc{PID: os.Getpid(), Start: "autre"}}}
	other.release()
	if _, err := os.Stat(tuneLockPath()); err != nil {
		t.Error("verrou d'autrui retiré")
	}
	l3.release()
}

// Le moteur d'essai ne lit ni n'écrit config.env : « loki serve » à blanc, avec
// la configuration de l'essai, compose sa ligne de commande (adresse privée,
// surcharges) et laisse la configuration active intacte.
func TestTuneTrialNeverWritesConfig(t *testing.T) {
	home := testHome(t)
	for _, k := range []string{"PATH", "LD_LIBRARY_PATH", "DYLD_LIBRARY_PATH", "CUDA_VISIBLE_DEVICES", "CUDA_DEVICE_ORDER",
		"CUDA_SCALE_LAUNCH_QUEUES", "GGML_OP_OFFLOAD_MIN_BATCH", "GGML_CUDA_GRAPH_OPT", tuneTrialEnv, tuneDryRunEnv} {
		t.Setenv(k, os.Getenv(k))
	}
	bin := filepath.Join(home, "llama-server-factice")
	model := filepath.Join(home, "m.gguf")
	for _, p := range []string{bin, model} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	active := map[string]string{"BIN": bin, "MODEL": model, "CTX": "8192", "UBATCH": "512", "PORT": "8080",
		"EXTRA_ARGS": "--cache-type-k q8_0 --port 9999"}
	if err := WriteConfig(active); err != nil {
		t.Fatal(err)
	}
	tr := &tuneTrialer{dir: filepath.Join(home, "tune", "run")}
	trial := knobUBatch.set(active, "2048")
	dir, port, err := tr.prepare(trial)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(tuneTrialEnv, dir)
	t.Setenv(tuneDryRunEnv, "1")
	if err := cmdServe(nil); err != nil {
		t.Fatalf("loki serve à blanc : %v", err)
	}
	if got := ReadConfig(); !reflect.DeepEqual(got, active) {
		t.Errorf("configuration active modifiée :\n%v\nau lieu de\n%v", got, active)
	}
	l, err := readTuneLaunch(dir)
	if err != nil {
		t.Fatal(err)
	}
	if argValue(l.Args, "-ub") != "2048" || argValue(l.Args, "--host") != "127.0.0.1" ||
		argValue(l.Args, "--port") != strconv.Itoa(port) || argValue(l.Args, "--cache-type-k") != "q8_0" {
		t.Errorf("ligne de l'essai inattendue : %q", l.Args)
	}
	for i, a := range l.Args {
		if a == "--port" && l.Args[i+1] == "9999" {
			t.Errorf("--port d'EXTRA_ARGS non forcé : %q", l.Args)
		}
	}
	if l.Probe == nil {
		t.Error("sonde absente de la ligne de l'essai")
	}
	if _, err := os.Stat(filepath.Join(home, "slots")); err == nil {
		t.Error("l'essai a touché au dossier des slots du vrai moteur")
	}
}

// Après application, la configuration doit être exactement la référence plus
// les clés réglées.
func TestTuneAppliedMismatch(t *testing.T) {
	base := map[string]string{"MODEL": "m.gguf", "UBATCH": "512", "EXTRA_ARGS": "-t 8"}
	res := &tuneResult{BaseFP: configFingerprint(base), Set: map[string]string{"UBATCH": "2048", "EXTRA_ARGS": ""},
		Old: map[string]string{"UBATCH": "512", "EXTRA_ARGS": "-t 8"}}
	if why := tuneAppliedMismatch(map[string]string{"MODEL": "m.gguf", "UBATCH": "2048"}, res); why != "" {
		t.Errorf("application conforme refusée : %s", why)
	}
	if why := tuneAppliedMismatch(map[string]string{"MODEL": "m.gguf", "UBATCH": "1024"}, res); why == "" {
		t.Error("valeur réglée différente non vue")
	}
	if why := tuneAppliedMismatch(map[string]string{"MODEL": "autre.gguf", "UBATCH": "2048"}, res); why == "" {
		t.Error("changement hors réglages non vu")
	}
}

// Application : la copie ne touche pas au preset d'origine ; le preset lui-même
// est réécrit, le moteur redémarré, et une sonde qui échoue (OOM au premier
// long prompt, faux moteur) ramène l'ancienne version — preset ET configuration.
func TestTuneApplyAndRevert(t *testing.T) {
	testHome(t)
	content := "# NAME=Dense\nMODEL=m.gguf\nUBATCH=512\n"
	if err := os.MkdirAll(presetsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(presetsDir(), "Dense.env"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfig(parseEnv(content)); err != nil {
		t.Fatal(err)
	}
	res := &tuneResult{PresetID: "Dense", PresetName: "Dense", PresetFP: presetFingerprint([]byte(content)),
		BaseFP: configFingerprint(parseEnv(content)), Set: map[string]string{"UBATCH": "2048"},
		Old: map[string]string{"UBATCH": "512"}, Best: "UBATCH=2048"}
	var calls []string
	prevSvc, prevProbe := tuneSvc, tuneProbeFn
	t.Cleanup(func() { tuneSvc, tuneProbeFn = prevSvc, prevProbe })
	tuneSvc = func(a string) error { calls = append(calls, a); return nil }

	if _, err := tuneApply(context.Background(), res, tuneApplyCopy, func(string) {}); err != nil {
		t.Fatalf("copie : %v", err)
	}
	if got, _ := ReadPreset("Dense"); got != content || len(calls) != 0 {
		t.Fatalf("la copie a touché au preset ou au moteur : %q %v", got, calls)
	}
	copyBody, err := ReadPreset("Dense (optimisé)")
	if err != nil || !strings.Contains(copyBody, "UBATCH=2048") {
		t.Fatalf("copie absente ou fausse : %q %v", copyBody, err)
	}

	tuneProbeFn = func(context.Context, *tuneResult, func(string)) error { return errors.New("sonde : out of memory") }
	_, err = tuneApply(context.Background(), res, tuneApplyPreset, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "rétablie") {
		t.Fatalf("échec de la sonde sans retour arrière : %v", err)
	}
	if got, _ := ReadPreset("Dense"); got != content {
		t.Errorf("preset non rétabli : %q", got)
	}
	if ReadConfig()["UBATCH"] != "512" {
		t.Errorf("configuration non rétablie : %v", ReadConfig())
	}
	if !reflect.DeepEqual(calls, []string{"restart", "restart"}) {
		t.Errorf("redémarrages : %v", calls)
	}
	if _, err := os.Stat(tuneLockPath()); !os.IsNotExist(err) {
		t.Error("verrou non rendu après l'application")
	}

	calls = nil
	tuneProbeFn = func(context.Context, *tuneResult, func(string)) error { return nil }
	if _, err := tuneApply(context.Background(), res, tuneApplyPreset, func(string) {}); err != nil {
		t.Fatalf("application : %v", err)
	}
	if got, _ := ReadPreset("Dense"); !strings.Contains(got, "UBATCH=2048") || !strings.Contains(got, "# NAME=Dense") {
		t.Errorf("preset non réglé : %q", got)
	}
	if ReadConfig()["UBATCH"] != "2048" || len(calls) != 1 {
		t.Errorf("configuration %v, redémarrages %v", ReadConfig(), calls)
	}
	// Le preset a changé depuis la mesure : plus rien ne s'applique.
	if _, err := tuneApply(context.Background(), res, tuneApplyPreset, func(string) {}); err == nil {
		t.Error("résultat appliqué sur un preset modifié depuis la mesure")
	}
}

// TestTuneHelperSleep n'est pas un test : relancé par TestTuneReapOrphan comme
// faux essai orphelin, il dort jusqu'à ce qu'on le tue.
func TestTuneHelperSleep(t *testing.T) {
	if os.Getenv("LOKI_TUNE_TEST_SLEEP") != "1" {
		t.Skip("processus auxiliaire")
	}
	time.Sleep(60 * time.Second)
}

// Un essai orphelin (propriétaire du verrou mort) est arrêté avant tout
// démarrage du moteur — seulement si son identité concorde.
func TestTuneReapOrphan(t *testing.T) {
	testHome(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestTuneHelperSleep$")
	cmd.Env = append(os.Environ(), "LOKI_TUNE_TEST_SLEEP=1")
	tuneTrialAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	start, exe, ok := procIdentity(cmd.Process.Pid)
	if !ok {
		t.Fatal("identité du processus auxiliaire illisible")
	}
	// Propriétaire mort : un PID qui ne tourne plus (le test lui-même, avec un
	// instant de démarrage qui n'est pas le sien).
	dead := tuneProc{PID: os.Getpid(), Start: "pas-moi"}
	write := func(trial tuneProc) {
		b, _ := json.Marshal(tuneOwner{Owner: dead, Via: "cli", Trial: &trial})
		if err := os.WriteFile(tuneLockPath(), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Identité qui ne concorde pas (PID recyclé) : on ne tue rien.
	write(tuneProc{PID: cmd.Process.Pid, Start: start + "x", Exes: []string{exe}})
	tuneReapStale()
	select {
	case <-done:
		t.Fatal("processus tué sur la foi du seul PID")
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := os.Stat(tuneLockPath()); !os.IsNotExist(err) {
		t.Fatal("verrou périmé non retiré")
	}
	// Identité complète : arrêté.
	write(tuneProc{PID: cmd.Process.Pid, Start: start, Exes: []string{exe}})
	if err := tuneGuard(); err != nil {
		t.Fatalf("verrou périmé bloquant : %v", err)
	}
	tuneReapStale()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("essai orphelin toujours en vie")
	}
}

func TestTuneStageWanted(t *testing.T) {
	for _, c := range []struct {
		name string
		only []string
		want bool
	}{
		{"lots", nil, true},
		{"délestage", []string{"delestage"}, true},
		{"files CUDA", []string{"files"}, true},
		{"threads", []string{"lots"}, false},
		{"placement", []string{"aucune"}, true}, // sa propre case décide
		{"opt-in", []string{"lots"}, true},
		{"marges", []string{"aucune"}, false},
	} {
		if got := tuneStageWanted(c.name, c.only); got != c.want {
			t.Errorf("%s / %v : %v", c.name, c.only, got)
		}
	}
}

// Corrections de relecture du lot 2 : un verrou qui porte NOTRE PID sans que
// nous en tenions un vient d'un processus mort (PID réattribué après un
// redémarrage, même instant de démarrage relatif au boot) : il ne bloque rien.
// Un verrou illisible tout juste créé est en cours d'écriture : ni retiré ni
// bloquant ; vieux, il est écarté.
func TestTuneLockStaleSelfAndFresh(t *testing.T) {
	testHome(t)
	prev := tuneProcAlive
	tuneProcAlive = func(tuneProc) bool { return true } // l'identité concorde
	t.Cleanup(func() { tuneProcAlive = prev })
	if err := os.MkdirAll(filepath.Dir(tuneLockPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(tuneOwner{Owner: selfProc(), Via: "web", Phase: "essais"})
	if err := os.WriteFile(tuneLockPath(), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tuneGuard(); err != nil {
		t.Fatalf("verrou d'un ancien processus au même PID encore bloquant : %v", err)
	}
	l, err := tuneLockAcquire("cli", false)
	if err != nil {
		t.Fatalf("verrou périmé non écarté : %v", err)
	}
	if !errors.Is(tuneGuard(), errTuneBusy) {
		t.Error("notre propre verrou ne bloque plus")
	}
	l.release()
	if _, held := tuneHeld.Load(tuneHeldKey(l.owner)); held {
		t.Error("verrou rendu encore inscrit comme tenu")
	}

	// Illisible et neuf : laissé en place.
	if err := os.WriteFile(tuneLockPath(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tuneReapStale()
	if _, err := os.Stat(tuneLockPath()); err != nil {
		t.Fatal("verrou en cours d'écriture retiré")
	}
	// Illisible et vieux : écarté.
	old := time.Now().Add(-2 * tuneLockFreshness)
	if err := os.Chtimes(tuneLockPath(), old, old); err != nil {
		t.Fatal(err)
	}
	tuneReapStale()
	if _, err := os.Stat(tuneLockPath()); !os.IsNotExist(err) {
		t.Fatal("verrou illisible abandonné non retiré")
	}
}

// Les binaires remplacés par une mise à jour (« (deleted) » sous Linux) restent
// le même processus.
func TestTuneSameExeDeleted(t *testing.T) {
	if !sameExe("/usr/local/bin/loki", "/usr/local/bin/loki (deleted)") {
		t.Error("binaire remplacé pris pour un autre")
	}
	if sameExe("/usr/local/bin/loki", "/usr/bin/llama-server") {
		t.Error("binaires différents confondus")
	}
}

// Une application interrompue avant sa vérification (Loki tué pendant la
// sonde) est défaite quand le verrou périmé est écarté : preset ET
// configuration reviennent à la version sauvegardée.
func TestTuneUndoInterruptedApply(t *testing.T) {
	testHome(t)
	prev := tuneProcAlive
	tuneProcAlive = func(tuneProc) bool { return false } // propriétaire mort
	t.Cleanup(func() { tuneProcAlive = prev })
	old := "# NAME=Dense\nMODEL=m.gguf\nUBATCH=512\n"
	patched := "# NAME=Dense\nMODEL=m.gguf\nUBATCH=2048\n"
	if err := os.MkdirAll(presetsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(presetsDir(), "Dense.env"), []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfig(parseEnv(patched)); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "Dense.env")
	if err := os.WriteFile(backup, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(tuneLockPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(tuneOwner{Owner: tuneProc{PID: 999999, Start: "x"}, Via: "apply", Phase: "application",
		Backup: backup, PresetID: "Dense", PresetName: "Dense", MainWasActive: true})
	if err := os.WriteFile(tuneLockPath(), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if !tuneReapStale() {
		t.Error("MainWasActive perdu")
	}
	if got, _ := ReadPreset("Dense"); got != old {
		t.Errorf("preset non rétabli : %q", got)
	}
	if ReadConfig()["UBATCH"] != "512" {
		t.Errorf("configuration non rétablie : %v", ReadConfig())
	}
	// Hors phase d'application, rien n'est touché.
	if err := WriteConfig(parseEnv(patched)); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(tuneOwner{Owner: tuneProc{PID: 999999, Start: "x"}, Via: "cli", Phase: "essais",
		Backup: backup, PresetID: "Dense", PresetName: "Dense"})
	if err := os.WriteFile(tuneLockPath(), b, 0o644); err != nil {
		t.Fatal(err)
	}
	tuneReapStale()
	if ReadConfig()["UBATCH"] != "2048" {
		t.Error("configuration modifiée hors phase d'application")
	}
}

// Reprise dans le processus web : un « loki tune » tué sans ses defers ne
// laisse plus le moteur arrêté jusqu'au prochain démarrage de l'interface. Le
// tic suivant écarte le verrou périmé et relance le moteur — une fois, et
// seulement s'il tournait avant. Sans verrou, ni lecture ni relance.
func TestTuneRecoverWatch(t *testing.T) {
	testHome(t)
	alive := true
	prevAlive, prevStart := tuneProcAlive, tuneRecoverStart
	tuneProcAlive = func(p tuneProc) bool { return alive && p.PID == os.Getpid() }
	starts := 0
	tuneRecoverStart = func() { starts++ }
	t.Cleanup(func() { tuneProcAlive, tuneRecoverStart = prevAlive, prevStart })

	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() { tuneRecoverWatch(tick); close(done) }()
	// Un envoi est reçu quand le tic précédent a fini : deux de suite, et le
	// premier est traité en entier.
	step := func() { tick <- time.Now() }
	step()
	step()
	if starts != 0 {
		t.Fatal("relance sans verrou")
	}
	l, err := tuneLockAcquire("cli", true)
	if err != nil {
		t.Fatal(err)
	}
	step()
	step()
	if starts != 0 {
		t.Fatal("relance pendant une optimisation vivante")
	}
	if _, err := os.Stat(tuneLockPath()); err != nil {
		t.Fatal("verrou vivant retiré")
	}
	alive = false // kill -9 : le verrou reste, son propriétaire est mort
	step()
	step()
	if starts != 1 {
		t.Fatalf("relances : %d, attendu 1", starts)
	}
	if _, err := os.Stat(tuneLockPath()); !os.IsNotExist(err) {
		t.Error("verrou périmé non retiré")
	}
	l.release()

	// Le moteur ne tournait pas avant : verrou retiré, pas de relance.
	alive = true
	if _, err := tuneLockAcquire("cli", false); err != nil {
		t.Fatal(err)
	}
	alive = false
	step()
	step()
	close(tick)
	<-done
	if starts != 1 {
		t.Errorf("relance d'un moteur qui ne tournait pas : %d", starts)
	}
	if _, err := os.Stat(tuneLockPath()); !os.IsNotExist(err) {
		t.Error("verrou périmé non retiré")
	}
}
