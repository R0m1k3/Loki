package loki

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

// parity_tune_test.go — journal de référence de la partie PURE de
// l'optimiseur (lot 28 de la migration Rust) : découpe et réécriture
// d'EXTRA_ARGS, réglages par clé ou par drapeau, essais de chaque étape selon
// la machine et les cases cochées, garde-fous (cœur de la ligne, dénature,
// clés permises), diff et réécriture du preset, journal d'un essai, score et
// départage. Rangé dans rust/loki-rs/tests/parity_tune/core.json ;
// tests/tune_core.rs le rejoue.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteTuneCore

const parityTuneCoreFile = "../../rust/loki-rs/tests/parity_tune/core.json"

func parityTuneCands(c []tuneCand) []map[string]any {
	out := []map[string]any{}
	for _, x := range c {
		out = append(out, map[string]any{"stage": x.Stage, "label": x.Label, "cfg": x.Cfg})
	}
	return out
}

func TestPariteTuneCore(t *testing.T) {
	journal := map[string]any{}

	// --- EXTRA_ARGS ---------------------------------------------------------
	extras := []string{
		"", "  ", "-b 2048", `--chat-template-file "/mes modèles/tpl.jinja" -ub 512`,
		`-ot 'blk\.(1[0-9])\.ffn_.*_exps\.=CPU' --n-cpu-moe 40`,
		`-ot "blk\.([0-9]+)\.ffn_.*_exps\.=CPU,per_layer_token_embd=CPU" --cache-type-k q8_0 --n-cpu-moe 40 ` +
			`--flash-attn on -ub 512 --chat-template-file "/a b/t.jinja" -ts 0.9,0.1 -ctv q8_0 -cmoe`,
		"-ub=1024 -t 8 -b 4096", "  -ts 0.9,0.1  --flash-attn on", "-ts=1,1 -mg 0",
		"-ot exps=CPU,per_layer_token_embd=CPU -cmoe --cpu-moe -ncmoe=12",
		"-ot=per_layer_token_embd=CPU", "--override-tensor exps=CPU", "-ot", "-t", "-t 8 -t 6", "-ub",
		`"" -x`, `""x y`, `a""b`, `'a b'c "d"`, "--cache-type-k q8_0\t--flash-attn on\n-t 8",
		`"non fermé -b 4`, `x"y z"w -ub=1024`, "-bs", "--backend-sampling -t 4", "-b 512\t-ub 256",
		"-ncmoe", "--n-cpu-moe 0 --cpu-moe",
	}
	var ex []map[string]any
	for _, s := range extras {
		var spans [][]any
		for _, sp := range argSpans(s) {
			spans = append(spans, []any{sp.start, sp.end, sp.val})
		}
		dropped, removed := tuneExtraDropExperts(s)
		ex = append(ex, map[string]any{
			"in":           s,
			"spans":        spans,
			"split":        splitArgs(s),
			"set_ub":       tuneExtraSet(s, knobUBatch.flags, "2048"),
			"drop_ub":      tuneExtraSet(s, knobUBatch.flags, ""),
			"set_t":        tuneExtraSet(s, knobThreads.flags, "7"),
			"set_t_space":  tuneExtraSet(s, knobThreads.flags, "a b"),
			"set_b":        tuneExtraSet(s, knobBatch.flags, "4096"),
			"drop_ts":      tuneExtraDrop(s, 1, "-ts", "--tensor-split"),
			"drop_cmoe":    tuneExtraDrop(s, 0, "-cmoe", "--cpu-moe"),
			"add_bs":       tuneExtraAddSwitch(s, "--backend-sampling"),
			"drop_experts": dropped,
			"removed":      removed,
			"core":         tuneCoreArgs(splitArgs(s), false),
			"core_optin":   tuneCoreArgs(splitArgs(s), true),
		})
	}
	journal["extra"] = ex

	// --- réglages ------------------------------------------------------------
	cfgs := []map[string]string{
		{},
		{"UBATCH": "1024", "BATCH": "4096", "THREADS": "0", "FIT_TARGET": "1536", "SPEC_N_MAX": "2"},
		{"EXTRA_ARGS": "-ub 512 -t 8", "UBATCH": "1024"},
		{"EXTRA_ARGS": "-ub=256 --threads 6 -b 1024 -fitt 2048 --spec-draft-n-max 5"},
		{"EXTRA_ARGS": "-ub 512", "UBATCH": " 128 "},
		{"EXTRA_ARGS": "-t"},
		tuneRichCfg(),
	}
	knobs := map[string]tuneKnob{"BATCH": knobBatch, "UBATCH": knobUBatch, "THREADS": knobThreads,
		"FIT_TARGET": knobFitTarget, "SPEC_N_MAX": knobSpecN}
	var kn []map[string]any
	for _, c := range cfgs {
		get, set, unset := map[string]string{}, map[string]any{}, map[string]any{}
		for name, k := range knobs {
			get[name] = k.get(c)
			set[name] = k.set(c, "99")
			unset[name] = k.set(c, "")
		}
		kn = append(kn, map[string]any{"cfg": c, "get": get, "set": set, "unset": unset})
	}
	journal["knobs"] = kn

	// --- essais ---------------------------------------------------------------
	type envCase struct {
		name string
		env  tuneEnv
	}
	rich := tuneRichEnv()
	envs := []envCase{
		{"riche", rich},
		{"sans placement", func() tuneEnv { e := tuneRichEnv(); e.Placement = false; return e }()},
		{"sans --fit", func() tuneEnv {
			e := tuneRichEnv()
			e.Help = strings.ReplaceAll(tuneTestHelp, "--fit", "--nofit")
			return e
		}()},
		{"sans --fit-target", func() tuneEnv {
			e := tuneRichEnv()
			e.Help = strings.ReplaceAll(tuneTestHelp, "--fit-target", "--fit-x")
			return e
		}()},
		{"sans nvidia-smi", func() tuneEnv { e := tuneRichEnv(); e.SMI = false; return e }()},
		{"sans opt-in", func() tuneEnv { e := tuneRichEnv(); e.OptIn = false; return e }()},
		{"une carte", func() tuneEnv { e := tuneRichEnv(); e.GPUs = 1; return e }()},
		{"trois cartes", func() tuneEnv { e := tuneRichEnv(); e.GPUs = 3; return e }()},
		{"cœurs inconnus", func() tuneEnv { e := tuneRichEnv(); e.PhysCores = 0; return e }()},
		{"deux cœurs", func() tuneEnv { e := tuneRichEnv(); e.PhysCores = 2; return e }()},
		{"environnement posé", func() tuneEnv {
			e := tuneRichEnv()
			e.UserEnv = map[string]string{"GGML_OP_OFFLOAD_MIN_BATCH": "64", "GGML_CUDA_GRAPH_OPT": "1"}
			e.QueuesEnv = true
			e.ArgEnv = map[string]string{"LLAMA_ARG_BACKEND_SAMPLING": "1"}
			return e
		}()},
		{"dense deux cartes", tuneEnv{Help: tuneTestHelp, GPUs: 2, VRAMMiB: 28 << 10, ModelBytes: 10 << 30, SMI: true,
			PhysCores: 8, Placement: true, OptIn: true}},
		{"modèle plus gros que la VRAM", tuneEnv{Help: tuneTestHelp, GPUs: 1, VRAMMiB: 8 << 10, ModelBytes: 20 << 30, SMI: true,
			PhysCores: 8}},
		{"moteur ancien", tuneEnv{GPUs: 1, SMI: true, PhysCores: 4, Placement: true, OptIn: true}},
	}
	candCfgs := []struct {
		name string
		cfg  map[string]string
	}{
		{"riche", tuneRichCfg()},
		{"simple", map[string]string{"MODEL": "m.gguf", "CTX": "32768"}},
		{"contexte non chiffré", map[string]string{"MODEL": "m.gguf", "EXTRA_ARGS": "-ot exps=CPU"}},
		{"split tensor", map[string]string{"MODEL": "m.gguf", "CTX": "32768", "SPLIT_MODE": "tensor", "EXTRA_ARGS": "-ts 1,1"}},
		{"lots posés", map[string]string{"MODEL": "m.gguf", "CTX": "8192", "UBATCH": "1024", "BATCH": "2048",
			"OP_OFFLOAD_MIN_BATCH": "128", "CUDA_LAUNCH_QUEUES": "4X", "THREADS": "7", "FIT_TARGET": "1536"}},
		{"spéculation", map[string]string{"MODEL": "m.gguf", "CTX": "8192", "SPEC": "draft", "SPEC_N_MAX": "4",
			"CUDA_GRAPH_OPT": "On", "EXTRA_ARGS": "--n-cpu-moe 10 -bs"}},
		{"ngram", map[string]string{"MODEL": "m.gguf", "CTX": "8192", "SPEC": "ngram"}},
		{"cpu-moe", map[string]string{"MODEL": "m.gguf", "CTX": "16384", "EXTRA_ARGS": "-cmoe -t 0"}},
	}
	var cs []map[string]any
	for _, cc := range candCfgs {
		for _, ec := range envs {
			stages := map[string]any{}
			for _, st := range tuneStages {
				c, why := st.Gen(cc.cfg, ec.env)
				stages[st.Name] = map[string]any{"why": why, "cands": parityTuneCands(c)}
			}
			e := ec.env
			cs = append(cs, map[string]any{"cfg_name": cc.name, "env_name": ec.name, "cfg": cc.cfg,
				"env": map[string]any{"help": e.Help, "gpus": e.GPUs, "vram_mib": e.VRAMMiB, "model_bytes": e.ModelBytes,
					"moe": e.MoE, "phys_cores": e.PhysCores, "smi": e.SMI, "arg_env": e.ArgEnv, "user_env": e.UserEnv,
					"queues_env": e.QueuesEnv, "placement": e.Placement, "opt_in": e.OptIn},
				"weights_on_cpu": tuneWeightsOnCPU(cc.cfg, ec.env),
				"stages":         stages})
		}
	}
	journal["cands"] = cs
	var sw []map[string]any
	for _, name := range []string{"placement", "marges", "lots", "délestage", "threads", "files CUDA", "opt-in"} {
		for _, only := range [][]string{nil, {"lots"}, {"Délestage"}, {"files"}, {"FILES CUDA"}, {"marges-x"}, {" threads "}, {"opt"}, {"x"}} {
			sw = append(sw, map[string]any{"name": name, "only": only, "wanted": tuneStageWanted(name, only)})
		}
	}
	journal["stage_wanted"] = sw

	// --- garde-fous ------------------------------------------------------------
	base := tuneLaunch{Args: []string{"/bin/llama-server", "-m", "/m.gguf", "-c", "65536", "-b", "2048", "-ub", "512",
		"--host", "127.0.0.1", "--port", "41000", "--parallel", "1", "-ctk", "q8_0", "-ot", "exps=CPU,per_layer_token_embd=CPU",
		"--cache-ram", "8192", "--flash-attn", "on"},
		Env: map[string]string{"CUDA_VISIBLE_DEVICES": "1,0", "CUDA_SCALE_LAUNCH_QUEUES": "4x"}}
	ok := tuneLaunch{Args: []string{"/bin/llama-server", "-m", "/m.gguf", "-c", "65536", "-b", "4096", "-ub", "2048",
		"--host", "127.0.0.1", "--port", "41001", "--parallel", "1", "-ctk", "q8_0", "-ot", "per_layer_token_embd=CPU",
		"--cache-ram", "12000", "--flash-attn", "on", "-t", "7"},
		Env: map[string]string{"CUDA_VISIBLE_DEVICES": "1,0", "GGML_OP_OFFLOAD_MIN_BATCH": "128"}}
	clone := func(l tuneLaunch) tuneLaunch {
		c := tuneLaunch{Args: append([]string(nil), l.Args...), Env: map[string]string{}}
		for k, v := range l.Env {
			c.Env[k] = v
		}
		return c
	}
	var dn []map[string]any
	add := func(name string, b, c tuneLaunch) {
		dn = append(dn, map[string]any{"name": name, "base": b, "cand": c,
			"denature": tuneDenature(b, c, false), "denature_optin": tuneDenature(b, c, true),
			"same_key": tuneLaunchKey(b) == tuneLaunchKey(c)})
	}
	add("réglages permis", base, ok)
	add("identique", base, clone(base))
	muts := map[string]func(l *tuneLaunch){
		"contexte":    func(l *tuneLaunch) { l.Args[4] = "32768" },
		"cache kv":    func(l *tuneLaunch) { l.Args[16] = "f16" },
		"ot":          func(l *tuneLaunch) { l.Args[18] = "exps=CPU" },
		"fa":          func(l *tuneLaunch) { l.Args[len(l.Args)-3] = "off" },
		"slots":       func(l *tuneLaunch) { l.Args[14] = "2" },
		"cuda":        func(l *tuneLaunch) { l.Env["CUDA_VISIBLE_DEVICES"] = "0,1" },
		"graphes":     func(l *tuneLaunch) { l.Env["GGML_CUDA_GRAPH_OPT"] = "1" },
		"spéculation": func(l *tuneLaunch) { l.Args = append(l.Args, "--spec-draft-n-max", "4") },
		"modèle ébauche": func(l *tuneLaunch) {
			l.Args = append(l.Args, "-md", "/d.gguf")
		},
		"argument de plus": func(l *tuneLaunch) { l.Args = append(l.Args, "--jinja") },
		"argument de moins": func(l *tuneLaunch) {
			l.Args = l.Args[:len(l.Args)-2]
		},
		"port et clé": func(l *tuneLaunch) {
			l.Args = append(l.Args, "--api-key", "s3cret", "--slot-save-path", "/tmp/x", "--port=9")
		},
		"cache-ram": func(l *tuneLaunch) { l.Args = append(l.Args, "-cram", "4096") },
	}
	names := make([]string, 0, len(muts))
	for n := range muts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		c := clone(ok)
		muts[name](&c)
		add(name, base, c)
	}
	journal["denature"] = dn

	rc := tuneRichCfg()
	var cc []map[string]any
	check := func(name string, b, c map[string]string) {
		e1, e2 := tuneCfgCheck(b, c, false), tuneCfgCheck(b, c, true)
		s1, s2 := "", ""
		if e1 != nil {
			s1 = e1.Error()
		}
		if e2 != nil {
			s2 = e2.Error()
		}
		cc = append(cc, map[string]any{"name": name, "base": b, "cand": c, "check": s1, "check_optin": s2,
			"diff": tuneDiff(b, c)})
	}
	check("identique", rc, copyCfg(rc))
	check("lots", rc, setKey(setKey(rc, "UBATCH", "2048"), "BATCH", "4096"))
	check("contexte", rc, setKey(rc, "CTX", "32768"))
	check("spec", rc, setKey(rc, "SPEC", "auto"))
	check("graphes", rc, setKey(rc, "CUDA_GRAPH_OPT", "on"))
	check("retrait", rc, setKey(rc, "TEMP", ""))
	check("extra permis", rc, knobThreads.set(rc, "4"))
	check("extra interdit", rc, setKey(rc, "EXTRA_ARGS", strings.Replace(rc["EXTRA_ARGS"], "q8_0", "f16", 1)))
	check("extra placement", rc, setKey(rc, "EXTRA_ARGS", func() string { s, _ := tuneExtraDropExperts(rc["EXTRA_ARGS"]); return s }()))
	check("spéculation dans extra", rc, setKey(rc, "EXTRA_ARGS", rc["EXTRA_ARGS"]+" --spec-draft-n-max 4"))
	check("extra vide", map[string]string{"EXTRA_ARGS": "-t 8"}, map[string]string{})
	journal["cfg_check"] = cc

	content := "# NAME=Gros MoE\n# commentaire\nMODEL=big.gguf\nCTX=65536\nUBATCH=512\n" +
		`EXTRA_ARGS="--cache-type-k q8_0 --flash-attn on -t 8"` + "\n"
	pb := parseEnv(content)
	var pp []map[string]any
	patch := func(name string, win map[string]string) {
		out, err := tunePatchPreset(content, pb, win)
		e := ""
		if err != nil {
			e = err.Error()
		}
		pp = append(pp, map[string]any{"name": name, "win": win, "out": out, "error": e})
	}
	patch("threads retirés, lots", setKey(setKey(knobThreads.set(pb, ""), "UBATCH", "2048"), "BATCH", "4096"))
	patch("contexte", setKey(pb, "CTX", "32768"))
	patch("rien", copyCfg(pb))
	patch("opt-in", setKey(pb, "SPEC", "auto"))
	patch("retrait d'EXTRA_ARGS", setKey(pb, "EXTRA_ARGS", ""))
	journal["patch"] = struct {
		Content string           `json:"content"`
		Cases   []map[string]any `json:"cases"`
	}{content, pp}

	// --- journal d'un essai ------------------------------------------------------
	logs := []string{
		"load_tensors: offloaded 40/49 layers to GPU\n" +
			"llama_kv_cache: size = 1024.00 MiB (  8192 cells,  32 layers,  1/1 seqs), K (q8_0):  512.00 MiB, V (q8_0):  512.00 MiB\n" +
			"load_tensors: offloaded 49/49 layers to GPU\n",
		"rien",
		"",
		"K (F16): 1 MiB, V (f16): 1 MiB\nK (q4_0): 1, V (q8_0): 2\nK (f16): x V (f16): y",
		"ggml_backend_cuda_buffer_type_alloc_buffer: cudaMalloc failed: out of memory",
		"common_fit: failed to fit model\n",
		"llama_model_load: error loading model: bad magic\n",
		"ligne 1\nderniere ligne de journal   \n\n",
		"\n  \n",
		"x\n" + strings.Repeat("é", 100),
		"x\n" + strings.Repeat("a", 159) + "é" + strings.Repeat("b", 20),
	}
	var lf []map[string]any
	for _, l := range logs {
		off, total, kv := tuneLogFacts(l)
		lf = append(lf, map[string]any{"log": l, "offloaded": off, "total": total, "kv": kv, "failure": tuneLoadFailure(l)})
	}
	journal["logs"] = lf

	// --- score et départage ------------------------------------------------------
	intp := func(n int) *int { return &n }
	var recSets []map[string]any
	weights := func(name string, recs []perfRec) {
		recSets = append(recSets, map[string]any{"name": name, "recs": recs, "weights": tuneWeightsFrom(recs)})
	}
	weights("vide", nil)
	var few []perfRec
	for i := 0; i < 19; i++ {
		few = append(few, perfRec{Kind: perfMain, Complete: true, New: 1000 + i, Gen: 300})
	}
	weights("trop peu", few)
	var many []perfRec
	for i := 0; i < 30; i++ {
		r := perfRec{Kind: perfMain, Complete: true, New: 500 + 37*i, Gen: 100 + 13*i, Total: 4000 + 100*i}
		if i%3 == 0 {
			r.Cached = intp(100)
		} else {
			r.Cached = intp(r.Total - 200)
		}
		many = append(many, r)
	}
	many = append(many, perfRec{Kind: perfBench, Complete: true, New: 99999, Gen: 99999},
		perfRec{Kind: perfMain, Complete: false, New: 99999, Gen: 99999})
	weights("télémétrie", many)
	var bounds []perfRec
	for i := 0; i < 21; i++ {
		bounds = append(bounds, perfRec{Kind: perfTask, Complete: true, New: 50, Gen: 9000, Total: 100, Cached: intp(0)})
	}
	weights("bornes", bounds)
	var even []perfRec
	for i := 0; i < 20; i++ {
		even = append(even, perfRec{Kind: perfMain, Complete: true, New: 1000 + 10*i, Gen: 200 + i, Total: 2048, Cached: intp(204)})
	}
	weights("pair, froid connu", even)
	journal["weights"] = recSets

	w := tuneWeights{K: 2000, G: 600, FCold: 0.05}
	deep := func() *benchResult {
		return &benchResult{PromptPerSecond: 1000, PredictedPerSec: 50, Depth: &benchDepth{Target: 32768,
			Cold:         &benchTurn{PromptPerSecond: 800},
			DecodePerSec: 40, CachedPerSec: 999,
			Turns: []benchTurn{{New: 2100, Cached: 30000, Expected: 30048, PromptMs: 2000},
				{New: 2100, Cached: 32300, Expected: 32348, PromptMs: 2000}}}}
	}
	var runs []map[string]any
	score := func(name string, r *benchResult) {
		run, err := tuneRunFrom(r, w)
		e := ""
		if err != nil {
			e = err.Error()
		}
		runs = append(runs, map[string]any{"name": name, "bench": r, "run": run, "error": e})
	}
	score("complet", deep())
	score("vide", nil)
	score("sans decode", &benchResult{PromptPerSecond: 1000})
	score("rapide", &benchResult{PromptPerSecond: 1000, PredictedPerSec: 50})
	score("profondeur sautée", &benchResult{PromptPerSecond: 1000, PredictedPerSec: 50, Depth: &benchDepth{Skipped: "petit"}})
	score("partiel", func() *benchResult { r := deep(); r.Depth.Partial = "contexte plein"; return r }())
	score("sans froid", func() *benchResult { r := deep(); r.Depth.Cold = nil; return r }())
	score("froid nul", func() *benchResult { r := deep(); r.Depth.Cold.PromptPerSecond = 0; return r }())
	score("sans tours", func() *benchResult { r := deep(); r.Depth.Turns = nil; return r }())
	score("sans decode à D", func() *benchResult { r := deep(); r.Depth.DecodePerSec = 0; return r }())
	score("cache inconnu", func() *benchResult { r := deep(); r.Depth.Turns[1].Cached = -1; return r }())
	score("cache inconnu, rien d'autre", func() *benchResult {
		r := deep()
		r.Depth.Turns[1].Cached = -1
		r.Depth.CachedPerSec = 0
		return r
	}())
	score("rien de neuf", func() *benchResult {
		r := deep()
		for i := range r.Depth.Turns {
			r.Depth.Turns[i].New = 48
		}
		return r
	}())
	journal["runs"] = runs

	var wn []map[string]any
	for _, c := range [][2][]float64{
		{{20, 20.4}, {19.7, 19.8}}, {{20, 20.4}, {18}}, {{20, 20.4}, {18, 18.1}}, {{18, 22}, {18.5, 18.6}},
		{{}, {1, 2}}, {{10}, {}}, {{10, 10}, {9.6, 9.6}}, {{10, 10}, {9.7, 9.7}}, {{0}, {1, 1}}, {{10, 12}, {8, 8.2, 8.1}},
	} {
		gain, need, ok := tuneWins(c[0], c[1])
		wn = append(wn, map[string]any{"base": c[0], "cand": c[1], "gain": gain, "need": need, "wins": ok,
			"spread_base": tuneSpread(c[0]), "mean_cand": tuneMean(c[1])})
	}
	journal["wins"] = wn
	var wr []map[string]any
	for _, c := range []struct {
		base  []float64
		first float64
	}{{[]float64{20, 20.4}, 19}, {[]float64{20, 20.4}, 19.8}, {[]float64{20}, 19.39}, {[]float64{20}, 19.41}, {nil, 1}} {
		wr = append(wr, map[string]any{"base": c.base, "first": c.first, "worth": tuneWorthRepeat(c.base, c.first)})
	}
	journal["worth_repeat"] = wr

	var hr []map[string]any
	for _, c := range []struct {
		gpus    []gpuStat
		visible []int
	}{
		{nil, nil},
		{[]gpuStat{{Total: 24576, Used: 20000}, {Total: 12288, Used: 1000}}, nil},
		{[]gpuStat{{Total: 24576, Used: 20000}, {Total: 12288, Used: 1000}}, []int{1}},
		{[]gpuStat{{Total: 0, Used: 0}, {Total: 8192, Used: 9000}}, nil},
		{[]gpuStat{{Total: 8192, Used: 100}}, []int{3}},
	} {
		free, ok := tuneHeadroom(c.gpus, c.visible)
		var g [][2]int
		for _, x := range c.gpus {
			g = append(g, [2]int{x.Total, x.Used})
		}
		hr = append(hr, map[string]any{"gpus": g, "visible": c.visible, "free": free, "ok": ok})
	}
	journal["headroom"] = hr
	var vg []map[string]any
	for _, s := range []string{"", "0", "1,0", " 2 , 3 ", "GPU-abc", "0,,1", "+1,-2"} {
		vg = append(vg, map[string]any{"in": s, "out": tuneVisibleGPUs(map[string]string{"CUDA_VISIBLE_DEVICES": s})})
	}
	journal["visible"] = vg
	var fa []map[string]any
	for _, a := range [][]string{
		{"/b", "--host", "0.0.0.0", "--port", "8080", "-m", "x"},
		{"/b", "--host=::", "--port=1", "--port", "2", "--host"},
		{"/b", "--hostname", "x", "-p", "3"},
	} {
		fa = append(fa, map[string]any{"in": a, "out": tuneForceAddr(a, "127.0.0.1", "41234")})
	}
	journal["force_addr"] = fa

	// --- configuration d'un essai --------------------------------------------------
	var tc []map[string]any
	for _, c := range []map[string]string{
		{"MODEL": "m.gguf", "EXTRA_ARGS": `-ot "a b" -t 8`, "PORT": "41000"},
		{"MODEL": "x\ny"},
		{"NAME": "é ü", "A": "  b  "},
		{"A": `"q"`, "B": "c'd", "C": "#h"},
	} {
		dir := t.TempDir()
		err := writeTuneTrialConfig(dir, c)
		e, text := "", ""
		if err != nil {
			e = err.Error()
		} else {
			b, _ := os.ReadFile(dir + "/config.env")
			text = string(b)
		}
		tc = append(tc, map[string]any{"cfg": c, "error": e, "text": text})
	}
	journal["trial_config"] = tc

	// --- Verrou : contenu du fichier, clé de prise, binaires -----------------
	owners := []tuneOwner{
		{Owner: tuneProc{PID: 42}, Via: "web", Phase: "essais", Since: 1700000000},
		{Owner: tuneProc{PID: 42, Start: "b0c1/123", Exes: []string{"/usr/bin/loki-rs"}}, Via: "apply",
			Phase: "application", Since: 1700000000, Nonce: 1700000000123456789,
			Trial:         &tuneProc{PID: 43, Start: "b0c1/456", Exes: []string{"/usr/bin/loki", "/opt/llama-server"}},
			MainWasActive: true, Backup: "/home/l/tune/backup/Dense-1.env", PresetID: "Dense", PresetName: "Dense"},
	}
	lk := []map[string]any{}
	for _, o := range owners {
		b, _ := json.Marshal(o)
		lk = append(lk, map[string]any{"json": string(b), "key": tuneHeldKey(o)})
	}
	journal["lock_owners"] = lk
	se := []map[string]any{}
	for _, e := range [][2]string{
		{"/usr/local/bin/loki", "/usr/local/bin/loki (deleted)"}, {"/usr/local/bin/loki", "/usr/bin/llama-server"},
		{`C:\Loki\loki.exe`, "LOKI"}, {"", "loki"}, {"/a/llama-server", "llama-server"}, {"/a/b/", "b"},
	} {
		se = append(se, map[string]any{"a": e[0], "b": e[1], "same": sameExe(e[0], e[1])})
	}
	journal["same_exe"] = se

	// --- Résultat tel qu'enregistré et montré --------------------------------
	run := tuneRun{TurnSec: 3.25, ColdPP: 812.5, CachedPP: 1500, TG: 42.75, ProsePP: 2000, ProseTG: 45.5, Depth: 8192}
	results := []tuneResult{{}, {
		PresetID: "Dense", PresetName: "Dense", BaseFP: "abc", PresetFP: "def", Engine: "b7000-x", EngineBuild: 7000,
		Weights: tuneWeights{K: 2000, G: 600, FCold: 0.05, From: "défaut"}, Depth: 8192, NCtx: 16384, Slots: 1,
		Baseline: tuneTrial{Stage: "référence", Label: "configuration actuelle", Status: "mesuré",
			Runs: []tuneRun{run, {TurnSec: 3.5, ProsePP: 1900, ProseTG: 44}}, TurnSec: 3.375, Headroom: -1, Sec: 61.5,
			Offloaded: "33/33"},
		Trials: []tuneTrial{
			{Stage: "lots", Label: "UBATCH=512 BATCH=1024", Status: "retenu", Why: "−7.3 % par tour (seuil 3.0 %)",
				Runs: []tuneRun{run}, TurnSec: 3.125, Gain: 0.0734, Headroom: 512, Changes: []string{"UBATCH 2048 → 512"}, Sec: 30},
			{Stage: "lots", Label: "UBATCH=2048 BATCH=4096", Status: "écarté", Why: "dénature : x", Headroom: -1},
		},
		Best: "UBATCH=512 BATCH=1024", Gain: 0.0734, Set: map[string]string{"UBATCH": "512", "BATCH": "1024"},
		Old: map[string]string{"UBATCH": "2048", "BATCH": ""}, Changes: []string{"UBATCH 2048 → 512", "BATCH + 1024"},
		Placement: true, Partial: "annulé", Skipped: []string{"marges : une seule carte"}, Notes: []string{"n"},
		OptIn: true, At: 1700000000,
	}}
	rs := []string{}
	for _, r := range results {
		b, _ := json.Marshal(r)
		rs = append(rs, string(b))
	}
	journal["results"] = rs

	// --- Après application : la configuration est-elle l'essai retenu ? ------
	ab := map[string]string{"MODEL": "m.gguf", "UBATCH": "512", "EXTRA_ARGS": "-t 8"}
	ares := &tuneResult{BaseFP: configFingerprint(ab), Set: map[string]string{"UBATCH": "2048", "EXTRA_ARGS": ""},
		Old: map[string]string{"UBATCH": "512", "EXTRA_ARGS": "-t 8"}}
	ap := []map[string]any{}
	for _, now := range []map[string]string{
		{"MODEL": "m.gguf", "UBATCH": "2048"}, {"MODEL": "m.gguf", "UBATCH": "1024"},
		{"MODEL": "autre.gguf", "UBATCH": "2048"}, {"MODEL": "m.gguf", "UBATCH": "2048", "EXTRA_ARGS": "-t 4"},
		{"MODEL": "m.gguf", "UBATCH": "2048", "PORT": "9"},
	} {
		ap = append(ap, map[string]any{"now": now, "why": tuneAppliedMismatch(now, ares)})
	}
	journal["applied"] = map[string]any{"base_fp": ares.BaseFP, "set": ares.Set, "old": ares.Old, "cases": ap}

	got, err := parityJSON(journal)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll("../../rust/loki-rs/tests/parity_tune", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityTuneCoreFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityTuneCoreFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("l'optimiseur ne produit plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteTuneCore,\n"+
			"puis cargo test --test tune_core dit si le port Rust suit.", parityTuneCoreFile)
	}
}
