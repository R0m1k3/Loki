package loki

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Optimiseur sans perte (« loki tune », bouton « Optimiser » de l'éditeur de
// preset) — la partie PURE : ce qu'on a le droit de changer, les essais à
// tenter, la comparaison des lignes de commande, le score, la réécriture du
// preset. Le déroulé (moteur d'essai, mesures, verrou) vit dans
// backend_tune_run.go.
//
// Principe : chercher, pour CETTE machine et CE build du moteur, le placement,
// les lots, les threads (et, sur demande, la spéculation) qui rendent un tour de
// conversation le plus court — sans rien changer à ce que calcule le modèle.
// Seuls des réglages d'ordonnancement bougent : la sortie reste équivalente en
// distribution (les sommes flottantes ne sont pas identiques au bit près d'un
// lot, d'un placement ou d'un nombre de threads à l'autre — c'est déjà le cas
// entre deux tailles de prompt) ; la spéculation vérifie chaque jeton par le
// modèle, avec son propre échantillonnage.
//
// Jamais touchés : modèle et quantification, contexte, types du cache KV
// (même déjà quantifié : c'est alors signalé, jamais modifié), projecteur
// vision, raisonnement, échantillonnage, nombre de slots, et tout drapeau
// d'EXTRA_ARGS hors de la liste blanche. Un essai dont la ligne de commande
// diffère de la référence ailleurs que sur les drapeaux réglés est écarté
// (« dénature ») avant même d'être chargé.

const (
	tuneTrialEnv  = "LOKI_TUNE_TRIAL"  // dossier de l'essai : « loki serve » y lit sa configuration
	tuneDryRunEnv = "LOKI_TUNE_DRYRUN" // « loki serve » compose la ligne de commande, l'écrit, et s'arrête
)

// tuneKeys : les clés du preset que l'optimiseur peut écrire. Toute autre clé
// reste identique à l'octet près (tuneCfgCheck).
var tuneKeys = map[string]bool{
	"BATCH": true, "UBATCH": true, "THREADS": true, "THREADS_BATCH": true,
	"FIT_TARGET": true, "OP_OFFLOAD_MIN_BATCH": true, "CUDA_LAUNCH_QUEUES": true,
	"EXTRA_ARGS": true, // jeton par jeton, liste blanche seulement (tuneFlags)
}

// tuneOptInKeys : seulement si l'utilisateur coche « inclure les options
// opt-in » — décodage spéculatif et graphes CUDA, eux-mêmes opt-in.
var tuneOptInKeys = map[string]bool{"SPEC": true, "SPEC_N_MAX": true, "CUDA_GRAPH_OPT": true}

// tuneNeverKeys : ce que l'optimiseur ne touche jamais. Documentaire, et vérifié
// par les tests : aucune n'est dans tuneKeys ni tuneOptInKeys.
var tuneNeverKeys = []string{"MODEL", "CTX", "KV_TYPE", "KV_TYPE_K", "KV_TYPE_V", "MMPROJ",
	"REASONING", "REASONING_BUDGET", "REASONING_EFFORT", "REASONING_PRESERVE", "REASONING_ECHO",
	"TEMP", "TOP_P", "TOP_K", "MIN_P", "PRESENCE_PENALTY", "REPEAT_PENALTY",
	"PARALLEL", "SIDE_SLOT", "MODEL_DRAFT", "SPEC_SAMPLING", "NGL", "SPLIT_MODE",
	"CUDA_VISIBLE_DEVICES", "BIN", "HOST", "PORT"}

// tuneFlag : un drapeau d'EXTRA_ARGS que l'optimiseur peut retirer ou régler.
type tuneFlag struct {
	names []string
	arity int  // 0 = interrupteur, 1 = une valeur
	optIn bool // seulement avec « inclure les options opt-in »
}

// tuneFlags : la liste blanche. Placement des experts (-ot : seulement les
// éléments qui visent les experts), découpe entre cartes, --fit, lots,
// threads ; spéculation et échantillonnage sur GPU en opt-in.
var tuneFlags = []tuneFlag{
	{names: []string{"-ot", "--override-tensor"}, arity: 1},
	{names: []string{"-ncmoe", "--n-cpu-moe"}, arity: 1},
	{names: []string{"-cmoe", "--cpu-moe"}},
	{names: []string{"-sm", "--split-mode"}, arity: 1},
	{names: []string{"-ts", "--tensor-split"}, arity: 1},
	{names: []string{"-mg", "--main-gpu"}, arity: 1},
	{names: []string{"-fit", "--fit"}, arity: 1},
	{names: []string{"-fitt", "--fit-target"}, arity: 1},
	{names: []string{"-b", "--batch-size"}, arity: 1},
	{names: []string{"-ub", "--ubatch-size"}, arity: 1},
	{names: []string{"-t", "--threads"}, arity: 1},
	{names: []string{"-tb", "--threads-batch"}, arity: 1},
	{names: []string{"--spec-draft-n-max", "--draft-max", "--draft-n"}, arity: 1, optIn: true},
	{names: []string{"-bs", "--backend-sampling"}, optIn: true},
}

// tuneForbiddenFlags : jamais ajoutés, retirés ni modifiés — copiés tels quels,
// dans l'ordre. Documentaire et testé : aucun n'est dans tuneFlags.
var tuneForbiddenFlags = []string{"-ctk", "--cache-type-k", "-ctv", "--cache-type-v", "-fa", "--flash-attn",
	"-c", "--ctx-size", "--temp", "--top-k", "--top-p", "--min-p", "--samplers", "--repeat-penalty",
	"--repeat-last-n", "--dry-multiplier", "--reasoning", "--reasoning-budget", "--reasoning-format",
	"--chat-template", "--chat-template-file", "--jinja", "--cache-reuse", "--context-shift", "--swa-full",
	"-kvu", "--kv-unified", "--keep", "-np", "--parallel", "-m", "--model", "--mmproj", "-ngl", "--n-gpu-layers"}

// tuneFlagFor : l'entrée de la liste blanche pour ce nom, ou nil.
func tuneFlagFor(name string, optIn bool) *tuneFlag {
	for i := range tuneFlags {
		f := &tuneFlags[i]
		if f.optIn && !optIn {
			continue
		}
		for _, n := range f.names {
			if n == name {
				return f
			}
		}
	}
	return nil
}

// --- EXTRA_ARGS jeton par jeton ----------------------------------------------

// argSpan : un argument d'EXTRA_ARGS et sa place exacte dans la chaîne
// (guillemets compris). Les réécritures coupent et recollent la chaîne
// d'origine : un jeton hors liste blanche ressort à l'octet près, guillemets et
// espacement compris.
type argSpan struct {
	start, end int
	val        string
}

// argSpans découpe comme splitArgs (mêmes règles de guillemets, mêmes jetons),
// en gardant les positions. Les tests vérifient l'égalité des deux découpages.
func argSpans(s string) []argSpan {
	var out []argSpan
	var cur strings.Builder
	var quote rune
	start := -1
	flush := func(end int) {
		if cur.Len() > 0 {
			out = append(out, argSpan{start, end, cur.String()})
			cur.Reset()
		}
		start = -1
	}
	for i, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				if cur.Len() == 0 {
					// « "" » : un argument vide explicite, comme splitArgs.
					out = append(out, argSpan{start, i + 1, ""})
					start = -1
				}
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			if start < 0 {
				start = i
			}
			quote = r
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush(i)
		default:
			if start < 0 {
				start = i
			}
			cur.WriteRune(r)
		}
	}
	flush(len(s))
	return out
}

// extraFlagSpan : une occurrence d'un drapeau, avec sa valeur éventuelle.
type extraFlagSpan struct {
	name   string
	flag   argSpan
	value  *argSpan // nil : interrupteur, « nom=valeur », ou valeur manquante
	eqVal  string   // valeur de la forme « nom=valeur »
	hasEq  bool
	tokens []argSpan // les jetons qui la composent
}

// extraFlags repère, dans EXTRA_ARGS, les occurrences des drapeaux nommés.
// arity : 1 si le drapeau prend une valeur.
func extraFlags(spans []argSpan, arity int, names ...string) []extraFlagSpan {
	var out []extraFlagSpan
	for i := 0; i < len(spans); i++ {
		name, val, hasEq := strings.Cut(spans[i].val, "=")
		hit := false
		for _, n := range names {
			if name == n {
				hit = true
			}
		}
		if !hit {
			continue
		}
		f := extraFlagSpan{name: name, flag: spans[i], eqVal: val, hasEq: hasEq, tokens: []argSpan{spans[i]}}
		if arity == 1 && !hasEq && i+1 < len(spans) {
			v := spans[i+1]
			f.value = &v
			f.tokens = append(f.tokens, v)
			i++
		}
		out = append(out, f)
	}
	return out
}

// spliceEdit : un morceau de la chaîne d'origine remplacé (repl vide = retiré).
type spliceEdit struct {
	start, end int
	repl       string
}

// applyEdits applique des remplacements disjoints. Un retrait emporte les
// blancs qui le précèdent (ou, en tête de chaîne, ceux qui le suivent) : pas de
// double espace à la place d'un drapeau retiré. Le reste n'est pas touché.
func applyEdits(s string, edits []spliceEdit) string {
	if len(edits) == 0 {
		return s
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var b strings.Builder
	last := 0
	for _, e := range edits {
		start, end := e.start, e.end
		if e.repl == "" {
			ws := start
			for ws > last && (s[ws-1] == ' ' || s[ws-1] == '\t') {
				ws--
			}
			if ws == start || ws == 0 {
				for end < len(s) && (s[end] == ' ' || s[end] == '\t') {
					end++
				}
				if ws == 0 {
					start = 0
				}
			} else {
				start = ws
			}
		}
		if start < last {
			start = last
		}
		b.WriteString(s[last:start])
		b.WriteString(e.repl)
		last = end
	}
	b.WriteString(s[last:])
	return strings.TrimSpace(b.String())
}

// quoteArg : un argument tel que splitArgs le relira d'un tenant.
func quoteArg(a string) string { return joinArgs([]string{a}) }

// tuneExtraSet règle un drapeau à valeur dans EXTRA_ARGS : la DERNIÈRE
// occurrence (celle que retient le moteur) voit sa valeur remplacée sur place ;
// absent, le drapeau est ajouté en fin de chaîne sous son premier nom. val vide
// retire toutes les occurrences.
func tuneExtraSet(s string, names []string, val string) string {
	occ := extraFlags(argSpans(s), 1, names...)
	if val == "" {
		var edits []spliceEdit
		for _, o := range occ {
			for _, t := range o.tokens {
				edits = append(edits, spliceEdit{t.start, t.end, ""})
			}
		}
		return applyEdits(s, edits)
	}
	if len(occ) == 0 {
		add := names[0] + " " + quoteArg(val)
		if strings.TrimSpace(s) == "" {
			return add
		}
		return strings.TrimRight(s, " \t") + " " + add
	}
	o := occ[len(occ)-1]
	switch {
	case o.hasEq:
		return applyEdits(s, []spliceEdit{{o.flag.start, o.flag.end, o.name + "=" + quoteArg(val)}})
	case o.value != nil:
		return applyEdits(s, []spliceEdit{{o.value.start, o.value.end, quoteArg(val)}})
	default:
		return s[:o.flag.end] + " " + quoteArg(val) + s[o.flag.end:]
	}
}

// tuneExtraDrop retire toutes les occurrences des drapeaux (valeur comprise).
func tuneExtraDrop(s string, arity int, names ...string) string {
	var edits []spliceEdit
	for _, o := range extraFlags(argSpans(s), arity, names...) {
		for _, t := range o.tokens {
			edits = append(edits, spliceEdit{t.start, t.end, ""})
		}
	}
	return applyEdits(s, edits)
}

// tuneExtraAddSwitch ajoute un interrupteur absent (--backend-sampling).
func tuneExtraAddSwitch(s, name string) string {
	if strings.TrimSpace(s) == "" {
		return name
	}
	return strings.TrimRight(s, " \t") + " " + name
}

// tuneExtraDropExperts retire le placement MANUEL des experts : les éléments
// d'un -ot qui visent les experts (les autres restent, sur place), --n-cpu-moe,
// --cpu-moe. Rend aussi ce qui a été retiré, pour le dire.
func tuneExtraDropExperts(s string) (string, []string) {
	spans := argSpans(s)
	var edits []spliceEdit
	var removed []string
	for _, o := range extraFlags(spans, 1, "-ot", "--override-tensor") {
		v := o.eqVal
		if !o.hasEq {
			if o.value == nil {
				continue
			}
			v = o.value.val
		}
		var rest []string
		hit := false
		for _, el := range strings.Split(v, ",") {
			el = strings.TrimSpace(el)
			pat, _, _ := strings.Cut(el, "=")
			switch {
			case el == "":
			case otTargetsExperts(pat):
				removed = append(removed, o.name+" "+el)
				hit = true
			default:
				rest = append(rest, el)
			}
		}
		if !hit {
			continue // rien visant les experts : intouché
		}
		switch {
		case len(rest) == 0:
			for _, t := range o.tokens {
				edits = append(edits, spliceEdit{t.start, t.end, ""})
			}
		case o.hasEq:
			edits = append(edits, spliceEdit{o.flag.start, o.flag.end, o.name + "=" + quoteArg(strings.Join(rest, ","))})
		default:
			edits = append(edits, spliceEdit{o.value.start, o.value.end, quoteArg(strings.Join(rest, ","))})
		}
	}
	for _, o := range extraFlags(spans, 1, "-ncmoe", "--n-cpu-moe") {
		v := o.eqVal
		if o.value != nil {
			v = o.value.val
		}
		removed = append(removed, o.name+" "+v)
		for _, t := range o.tokens {
			edits = append(edits, spliceEdit{t.start, t.end, ""})
		}
	}
	for _, o := range extraFlags(spans, 0, "-cmoe", "--cpu-moe") {
		removed = append(removed, o.name)
		edits = append(edits, spliceEdit{o.flag.start, o.flag.end, ""})
	}
	return applyEdits(s, edits), removed
}

// --- réglages : par la clé, ou par le drapeau qui l'écrase ---------------------

// tuneKnob : une valeur réglable, par sa clé du preset ou par le drapeau
// d'EXTRA_ARGS qui l'emporterait sur elle. Écrire la clé quand EXTRA_ARGS porte
// déjà le drapeau ne changerait rien au moteur — et l'essai mesurerait autre
// chose que ce que le preset appliqué reproduira. On règle donc le drapeau, sur
// place.
type tuneKnob struct {
	key   string
	flags []string
}

var (
	knobBatch     = tuneKnob{"BATCH", []string{"-b", "--batch-size"}}
	knobUBatch    = tuneKnob{"UBATCH", []string{"-ub", "--ubatch-size"}}
	knobThreads   = tuneKnob{"THREADS", []string{"-t", "--threads"}}
	knobFitTarget = tuneKnob{"FIT_TARGET", []string{"-fitt", "--fit-target"}}
	knobSpecN     = tuneKnob{"SPEC_N_MAX", []string{"--spec-draft-n-max"}}
)

// get : la valeur effective (drapeau d'EXTRA_ARGS, sinon la clé).
func (k tuneKnob) get(cfg map[string]string) string {
	if v := flagValue(splitArgs(cfg["EXTRA_ARGS"]), k.flags...); v != "" {
		return v
	}
	return strings.TrimSpace(cfg[k.key])
}

// set rend une COPIE de cfg où la valeur vaut val ("" = retirée).
func (k tuneKnob) set(cfg map[string]string, val string) map[string]string {
	out := copyCfg(cfg)
	if hasAnyFlag(splitArgs(cfg["EXTRA_ARGS"]), k.flags...) {
		out["EXTRA_ARGS"] = tuneExtraSet(cfg["EXTRA_ARGS"], k.flags, val)
		if out["EXTRA_ARGS"] == "" {
			delete(out, "EXTRA_ARGS")
		}
		return out
	}
	if val == "" {
		delete(out, k.key)
	} else {
		out[k.key] = val
	}
	return out
}

func copyCfg(cfg map[string]string) map[string]string {
	out := make(map[string]string, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return out
}

// setKey : copie de cfg avec key = val ("" = retirée).
func setKey(cfg map[string]string, key, val string) map[string]string {
	out := copyCfg(cfg)
	if val == "" {
		delete(out, key)
	} else {
		out[key] = val
	}
	return out
}

// --- les essais ------------------------------------------------------------------

// tuneEnv : ce que l'optimiseur sait de la machine et de ce que l'utilisateur
// a coché.
type tuneEnv struct {
	Help       string
	GPUs       int   // cartes visibles par le moteur ; 0 = inconnu
	VRAMMiB    int64 // VRAM totale des cartes visibles ; 0 = inconnue
	ModelBytes int64
	MoE        bool              // le GGUF annonce des experts
	PhysCores  int               // cœurs physiques ; 0 = inconnu (axe threads sauté)
	SMI        bool              // VRAM libre mesurable par carte (nvidia-smi)
	ArgEnv     map[string]string // LLAMA_ARG_* de l'environnement
	UserEnv    map[string]string // GGML_* déjà posés (intouchés)
	QueuesEnv  bool              // CUDA_SCALE_LAUNCH_QUEUES déjà posé
	Placement  bool              // « inclure le placement » : EXTRA_ARGS réécrit pour --fit
	OptIn      bool              // « inclure les options opt-in »
}

// tuneCand : un essai — la configuration COMPLÈTE à lancer (copie + surcharges).
type tuneCand struct {
	Stage string            `json:"stage"`
	Label string            `json:"label"`
	Cfg   map[string]string `json:"-"`
}

// tuneStage : une étape de la recherche, appliquée à la meilleure configuration
// du moment (descente coordonnée). why : pourquoi l'étape est sautée.
type tuneStage struct {
	Name string
	Gen  func(cfg map[string]string, env tuneEnv) (cands []tuneCand, why string)
}

// tuneStages : l'ordre de la recherche. Le placement d'abord (il décide de ce
// qui reste en RAM, donc de l'intérêt des étapes suivantes), puis les lots,
// puis ce qui ne compte que pour des poids sur CPU, puis l'opt-in.
var tuneStages = []tuneStage{
	{"placement", tunePlacementCands},
	{"marges", tuneFitTargetCands},
	{"lots", tuneBatchCands},
	{"délestage", tuneOffloadCands},
	{"threads", tuneThreadCands},
	{"files CUDA", tuneQueueCands},
	{"opt-in", tuneOptInCands},
}

// tuneStageWanted : l'étape fait-elle partie de celles demandées ? only vide =
// toutes. Placement et opt-in ne se filtrent pas ici : ils ont leur case, et
// leurs générateurs refusent sans elle. Noms comparés sans accent ni casse, par
// leur premier mot (« files CUDA » = « files », « délestage » = « delestage »).
func tuneStageWanted(name string, only []string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.NewReplacer("é", "e", "è", "e", "ê", "e").Replace(s)
		if i := strings.IndexAny(s, " -"); i > 0 && s != "opt-in" {
			s = s[:i]
		}
		return s
	}
	n := norm(name)
	if len(only) == 0 || n == "placement" || n == "opt-in" {
		return true
	}
	for _, o := range only {
		if norm(o) == n {
			return true
		}
	}
	return false
}

func tuneSI(env tuneEnv) serveSysInfo {
	a := env.ArgEnv
	if a == nil {
		a = map[string]string{}
	}
	return serveSysInfo{Help: env.Help, ArgEnv: a, UserEnv: env.UserEnv}
}

// tuneWeightsOnCPU : des poids resteront-ils en RAM ? Placés à la main, ou un
// modèle plus gros que la VRAM (--fit en laisse alors une part sur CPU).
func tuneWeightsOnCPU(cfg map[string]string, env tuneEnv) bool {
	if cpuWeights(cfg, splitArgs(cfg["EXTRA_ARGS"]), env.ArgEnv, 0) != "" {
		return true
	}
	return env.VRAMMiB > 0 && env.ModelBytes > env.VRAMMiB<<20
}

// tunePlacementCands : le placement manuel (experts au CPU, --tensor-split)
// remplacé par --fit. Seulement si l'utilisateur l'a coché : c'est une
// réécriture d'EXTRA_ARGS, montrée et confirmée à part à l'application.
func tunePlacementCands(cfg map[string]string, env tuneEnv) ([]tuneCand, string) {
	if !env.Placement {
		return nil, "non demandé (« inclure le placement »)"
	}
	extra := splitArgs(cfg["EXTRA_ARGS"])
	if !helpSupportsFit(env.Help) {
		return nil, "ce moteur ne connaît pas --fit"
	}
	if c, ok := ctxFixed(cfg, extra); !ok {
		return nil, "contexte « " + c + " » non chiffré : --fit pourrait le réduire"
	}
	if splitModeKey(cfg) == "tensor" {
		return nil, "SPLIT_MODE=tensor"
	}
	moe := cpuExperts(extra, env.ArgEnv) != ""
	ts := hasAnyFlag(extra, "-ts", "--tensor-split") && env.GPUs >= 2
	if !moe && !ts {
		return nil, "rien n'est placé à la main"
	}
	e := cfg["EXTRA_ARGS"]
	label := "placement auto (--fit)"
	if moe {
		e, _ = tuneExtraDropExperts(e)
		label += " au lieu des experts placés à la main"
	}
	if ts {
		e = tuneExtraDrop(e, 1, "-ts", "--tensor-split")
		label += " au lieu de --tensor-split"
	}
	nc := setKey(cfg, "EXTRA_ARGS", e)
	if why := fitBlocker(nc, splitArgs(e), tuneSI(env)); why != "" {
		return nil, "--fit resterait inactif (" + why + ")"
	}
	return []tuneCand{{Stage: "placement", Label: label, Cfg: nc}}, ""
}

// tuneFitTargetCands : la marge que --fit laisse libre par carte. Sur deux
// cartes inégales, plus de marge sur l'une pousse des couches vers l'autre.
func tuneFitTargetCands(cfg map[string]string, env tuneEnv) ([]tuneCand, string) {
	extra := splitArgs(cfg["EXTRA_ARGS"])
	switch {
	case env.GPUs < 2:
		return nil, "une seule carte"
	case !env.SMI:
		return nil, "VRAM libre non mesurable (nvidia-smi)"
	case !strings.Contains(env.Help, "--fit-target"):
		return nil, "ce moteur ne connaît pas --fit-target"
	}
	if _, ok := ctxFixed(cfg, extra); !ok {
		return nil, "contexte non chiffré"
	}
	if why := fitBlocker(cfg, extra, tuneSI(env)); why != "" {
		return nil, "--fit inactif (" + why + ")"
	}
	vals := []string{"1536", "2048"}
	if env.GPUs == 2 {
		vals = []string{"1024,2048", "2048,1024", "1536"}
	}
	cur := knobFitTarget.get(cfg)
	var out []tuneCand
	for _, v := range vals {
		if v == cur {
			continue
		}
		out = append(out, tuneCand{Stage: "marges", Label: "FIT_TARGET=" + v, Cfg: knobFitTarget.set(cfg, v)})
	}
	return out, ""
}

// tuneBatchCands : micro-lot et lot. Un micro-lot plus grand prend plus de VRAM
// de calcul : sans mesure de la VRAM libre, on ne monte pas. Sur deux cartes
// tout GPU, le lot reste plus grand que le micro-lot, sans quoi le pipeline
// entre cartes n'a rien à se passer.
func tuneBatchCands(cfg map[string]string, env tuneEnv) ([]tuneCand, string) {
	extra := splitArgs(cfg["EXTRA_ARGS"])
	curUB := serveUBatch(cfg, extra)
	curB, _ := strconv.Atoi(firstNonEmpty(knobBatch.get(cfg), "2048"))
	ubs := []int{512, 1024, 2048}
	if env.MoE {
		ubs = append(ubs, 4096)
	}
	multi := env.GPUs >= 2 && !tuneWeightsOnCPU(cfg, env)
	var out []tuneCand
	for _, ub := range ubs {
		if !env.SMI && ub > curUB {
			continue
		}
		seen := map[int]bool{}
		for _, b := range []int{max(2048, ub), min(2*ub, 8192)} {
			if seen[b] || b < ub || (multi && b <= ub) || (ub == curUB && b == curB) {
				continue
			}
			seen[b] = true
			c := knobUBatch.set(cfg, strconv.Itoa(ub))
			c = knobBatch.set(c, strconv.Itoa(b))
			out = append(out, tuneCand{Stage: "lots", Label: fmt.Sprintf("UBATCH=%d BATCH=%d", ub, b), Cfg: c})
		}
	}
	if len(out) == 0 {
		return nil, "rien à essayer"
	}
	return out, ""
}

// tuneOffloadCands : seuil d'envoi au GPU des poids restés en RAM. N'a de sens
// qu'avec des poids sur CPU.
func tuneOffloadCands(cfg map[string]string, env tuneEnv) ([]tuneCand, string) {
	if !tuneWeightsOnCPU(cfg, env) {
		return nil, "tous les poids sur GPU"
	}
	if env.UserEnv["GGML_OP_OFFLOAD_MIN_BATCH"] != "" {
		return nil, "GGML_OP_OFFLOAD_MIN_BATCH déjà posé dans l'environnement"
	}
	cur := firstNonEmpty(strings.TrimSpace(cfg["OP_OFFLOAD_MIN_BATCH"]), "32")
	var out []tuneCand
	for _, v := range []string{"32", "128", "512"} {
		if v == cur {
			continue
		}
		out = append(out, tuneCand{Stage: "délestage", Label: "OP_OFFLOAD_MIN_BATCH=" + v,
			Cfg: setKey(cfg, "OP_OFFLOAD_MIN_BATCH", v)})
	}
	return out, ""
}

// tuneThreadCands : threads CPU, seulement quand des poids tournent sur CPU.
// « Omis » = aucun -t : llama.cpp prend ses cœurs physiques — pas THREADS=0,
// qui voulait dire « tous les threads logiques » avant le lot 1.
func tuneThreadCands(cfg map[string]string, env tuneEnv) ([]tuneCand, string) {
	if !tuneWeightsOnCPU(cfg, env) {
		return nil, "tous les poids sur GPU"
	}
	if env.PhysCores <= 1 {
		return nil, "nombre de cœurs physiques inconnu"
	}
	cur := knobThreads.get(cfg)
	if cur == "0" {
		cur = ""
	}
	var out []tuneCand
	seen := map[string]bool{cur: true}
	for _, n := range []int{0, env.PhysCores - 1, env.PhysCores / 2} {
		v := ""
		if n > 0 {
			v = strconv.Itoa(n)
		}
		if seen[v] || n < 0 {
			continue
		}
		seen[v] = true
		label := "THREADS=" + v
		if v == "" {
			label = "threads omis (cœurs physiques)"
		}
		out = append(out, tuneCand{Stage: "threads", Label: label, Cfg: knobThreads.set(cfg, v)})
	}
	return out, ""
}

// tuneQueueCands : files de lancement CUDA, sur deux cartes ou plus.
func tuneQueueCands(cfg map[string]string, env tuneEnv) ([]tuneCand, string) {
	if env.GPUs < 2 {
		return nil, "une seule carte"
	}
	if env.QueuesEnv {
		return nil, "CUDA_SCALE_LAUNCH_QUEUES déjà posé dans l'environnement"
	}
	cur := strings.ToLower(strings.TrimSpace(cfg["CUDA_LAUNCH_QUEUES"]))
	var out []tuneCand
	for _, v := range []string{"off", "4x"} {
		if v == cur {
			continue
		}
		out = append(out, tuneCand{Stage: "files CUDA", Label: "CUDA_LAUNCH_QUEUES=" + v,
			Cfg: setKey(cfg, "CUDA_LAUNCH_QUEUES", v)})
	}
	return out, ""
}

// tuneOptInCands : décodage spéculatif, graphes CUDA, échantillonnage sur GPU —
// seulement si l'utilisateur l'a coché. La spéculation reste exacte (chaque
// jeton vérifié par le modèle) ; elle est notée sur la prose ET sur le code.
func tuneOptInCands(cfg map[string]string, env tuneEnv) ([]tuneCand, string) {
	if !env.OptIn {
		return nil, "non demandé (« inclure les options opt-in »)"
	}
	extra := splitArgs(cfg["EXTRA_ARGS"])
	var out []tuneCand
	mode := specMode(cfg)
	if mode == "off" && helpSupportsMTP(env.Help) && !specUserSet(extra, env.ArgEnv) {
		out = append(out, tuneCand{Stage: "opt-in", Label: "SPEC=auto", Cfg: setKey(cfg, "SPEC", "auto")})
	}
	if mode != "off" && mode != "" && mode != "ngram" && strings.Contains(env.Help, "--spec-draft-n-max") {
		cur := firstNonEmpty(knobSpecN.get(cfg), "3")
		for _, v := range []string{"2", "3", "4"} {
			if v != cur {
				out = append(out, tuneCand{Stage: "opt-in", Label: "SPEC_N_MAX=" + v, Cfg: knobSpecN.set(cfg, v)})
			}
		}
	}
	if !strings.EqualFold(strings.TrimSpace(cfg["CUDA_GRAPH_OPT"]), "on") && env.UserEnv["GGML_CUDA_GRAPH_OPT"] == "" {
		out = append(out, tuneCand{Stage: "opt-in", Label: "CUDA_GRAPH_OPT=on", Cfg: setKey(cfg, "CUDA_GRAPH_OPT", "on")})
	}
	if strings.Contains(env.Help, "--backend-sampling") && !hasAnyFlag(extra, "-bs", "--backend-sampling") &&
		!envTruthy(env.ArgEnv["LLAMA_ARG_BACKEND_SAMPLING"]) && splitModeKey(cfg) != "tensor" {
		out = append(out, tuneCand{Stage: "opt-in", Label: "--backend-sampling",
			Cfg: setKey(cfg, "EXTRA_ARGS", tuneExtraAddSwitch(cfg["EXTRA_ARGS"], "--backend-sampling"))})
	}
	if len(out) == 0 {
		return nil, "rien à essayer"
	}
	return out, ""
}

// --- garde-fous ----------------------------------------------------------------

// tuneCoreArgs : la ligne de commande sans ce que l'optimiseur a le droit de
// régler. Deux lignes au même cœur ne diffèrent que par l'ordonnancement.
// Adresse, port, clé d'API, dossier des slots et taille auto du cache RAM (lue
// sur la RAM libre du moment) sont neutralisés : propres à l'essai ou au
// moment, sans effet sur le calcul. En opt-in, la spéculation entière
// (--spec-*, -md) sort aussi du cœur.
func tuneCoreArgs(args []string, optIn bool) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasEq := strings.Cut(a, "=")
		next := func() string {
			if hasEq {
				return val
			}
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch name {
		case "--host", "--port", "--api-key", "--slot-save-path", "-cram", "--cache-ram":
			next()
			out = append(out, name, "*")
			continue
		}
		if optIn && (strings.HasPrefix(name, "--spec-") || name == "-md" || name == "--model-draft") {
			next()
			continue
		}
		f := tuneFlagFor(name, optIn)
		if f == nil {
			out = append(out, a)
			continue
		}
		if f.arity == 0 {
			continue
		}
		v := next()
		if name == "-ot" || name == "--override-tensor" {
			// Seuls les éléments qui visent les experts sont réglables.
			var rest []string
			for _, el := range strings.Split(v, ",") {
				pat, _, _ := strings.Cut(strings.TrimSpace(el), "=")
				if strings.TrimSpace(el) != "" && !otTargetsExperts(pat) {
					rest = append(rest, strings.TrimSpace(el))
				}
			}
			if len(rest) > 0 {
				out = append(out, name, strings.Join(rest, ","))
			}
		}
	}
	return out
}

// tuneEnvWhitelist : variables du moteur que l'optimiseur règle.
func tuneEnvTuned(k string, optIn bool) bool {
	switch k {
	case "CUDA_SCALE_LAUNCH_QUEUES", "GGML_OP_OFFLOAD_MIN_BATCH":
		return true
	case "GGML_CUDA_GRAPH_OPT":
		return optIn
	}
	return false
}

// tuneDenature compare la ligne d'un essai à celle de référence, hors liste
// blanche. "" = même calcul ; sinon, la première différence, en clair.
func tuneDenature(base, cand tuneLaunch, optIn bool) string {
	ba, ca := tuneCoreArgs(base.Args, optIn), tuneCoreArgs(cand.Args, optIn)
	for i := 0; i < max(len(ba), len(ca)); i++ {
		var x, y string
		if i < len(ba) {
			x = ba[i]
		}
		if i < len(ca) {
			y = ca[i]
		}
		if x != y {
			return fmt.Sprintf("ligne de commande différente hors réglages permis (« %s » au lieu de « %s »)", y, x)
		}
	}
	keys := map[string]bool{}
	for k := range base.Env {
		keys[k] = true
	}
	for k := range cand.Env {
		keys[k] = true
	}
	for k := range keys {
		if !tuneEnvTuned(k, optIn) && base.Env[k] != cand.Env[k] {
			return fmt.Sprintf("variable %s différente (« %s » au lieu de « %s »)", k, cand.Env[k], base.Env[k])
		}
	}
	return ""
}

// tuneLaunchKey : identité d'une ligne de lancement, adresse et port mis à
// part. Deux essais à la même clé lanceraient exactement le même moteur : le
// second n'est pas mesuré.
func tuneLaunchKey(l tuneLaunch) string {
	full := make([]string, 0, len(l.Args))
	for i := 0; i < len(l.Args); i++ {
		switch l.Args[i] {
		case "--host", "--port", "--api-key", "--slot-save-path":
			i++
			continue
		}
		full = append(full, l.Args[i])
	}
	keys := make([]string, 0, len(l.Env))
	for k, v := range l.Env {
		keys = append(keys, k+"="+v)
	}
	sort.Strings(keys)
	return strings.Join(full, "\x00") + "\x01" + strings.Join(keys, "\x00")
}

// tuneCfgCheck : la configuration d'un essai ne diffère de la référence que
// par des clés permises, et son EXTRA_ARGS que par des drapeaux de la liste
// blanche — les autres jetons à l'identique, dans l'ordre. Dernier filet avant
// tout lancement.
func tuneCfgCheck(base, cand map[string]string, optIn bool) error {
	keys := map[string]bool{}
	for k := range base {
		keys[k] = true
	}
	for k := range cand {
		keys[k] = true
	}
	for k := range keys {
		if base[k] == cand[k] {
			continue
		}
		if k == "EXTRA_ARGS" {
			a, b := tuneCoreArgs(splitArgs(base[k]), optIn), tuneCoreArgs(splitArgs(cand[k]), optIn)
			if strings.Join(a, "\x00") != strings.Join(b, "\x00") {
				return fmt.Errorf("EXTRA_ARGS modifié hors liste blanche")
			}
			continue
		}
		if !tuneKeys[k] && !(optIn && tuneOptInKeys[k]) {
			return fmt.Errorf("clé %s intouchable", k)
		}
	}
	return nil
}

// tuneDiff : ce qui change d'une configuration à l'autre, clé par clé.
func tuneDiff(base, cand map[string]string) []string {
	keys := map[string]bool{}
	for k := range base {
		keys[k] = true
	}
	for k := range cand {
		keys[k] = true
	}
	var names []string
	for k := range keys {
		if base[k] != cand[k] {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	show := func(v string) string {
		if v == "" {
			return "(absent)"
		}
		return v
	}
	var out []string
	for _, k := range names {
		out = append(out, k+" : "+show(base[k])+" → "+show(cand[k]))
	}
	return out
}

// tunePatchPreset réécrit le preset clé par clé (presetSetKey) : seules les
// clés réglées bougent, le reste du fichier — commentaires, ordre, NAME —
// ne change pas. Vérifié : relu, le preset est l'ancien plus les clés réglées,
// rien d'autre.
func tunePatchPreset(content string, base, win map[string]string) (string, error) {
	keys := map[string]bool{}
	for k := range base {
		keys[k] = true
	}
	for k := range win {
		keys[k] = true
	}
	var changed []string
	for k := range keys {
		if base[k] != win[k] {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	want := parseEnv(content)
	out := content
	for _, k := range changed {
		if !tuneKeys[k] && !tuneOptInKeys[k] {
			return "", fmt.Errorf("clé %s intouchable", k)
		}
		out = presetSetKey(out, k, win[k])
		if win[k] == "" {
			delete(want, k)
		} else {
			want[k] = win[k]
		}
	}
	got := parseEnv(out)
	if len(got) != len(want) {
		return "", fmt.Errorf("réécriture du preset incohérente")
	}
	for k, v := range want {
		if got[k] != v {
			return "", fmt.Errorf("réécriture du preset incohérente sur %s", k)
		}
	}
	return out, nil
}

// --- journal de l'essai ------------------------------------------------------------

var (
	reTuneOffloaded = regexp.MustCompile(`offloaded (\d+)/(\d+) layers to GPU`)
	reTuneKV        = regexp.MustCompile(`K \(([A-Za-z0-9_]+)\):[^\n]*V \(([A-Za-z0-9_]+)\):`)
)

// tuneLogFacts : couches déportées (dernière mention ; -1 = absente) et types
// du cache KV vus dans le journal (« f16/f16 », plusieurs caches dédoublonnés).
func tuneLogFacts(log string) (offloaded, total int, kv string) {
	offloaded, total = -1, -1
	for _, m := range reTuneOffloaded.FindAllStringSubmatch(log, -1) {
		offloaded, _ = strconv.Atoi(m[1])
		total, _ = strconv.Atoi(m[2])
	}
	set := map[string]bool{}
	for _, m := range reTuneKV.FindAllStringSubmatch(log, -1) {
		set[strings.ToLower(m[1])+"/"+strings.ToLower(m[2])] = true
	}
	var kvs []string
	for k := range set {
		kvs = append(kvs, k)
	}
	sort.Strings(kvs)
	return offloaded, total, strings.Join(kvs, ",")
}

// tuneLoadFailure : pourquoi un essai n'a pas chargé, lu dans son journal.
func tuneLoadFailure(log string) string {
	l := strings.ToLower(log)
	switch {
	case strings.Contains(l, "out of memory") || strings.Contains(l, "cudamalloc failed") ||
		strings.Contains(l, "failed to allocate"):
		return "mémoire insuffisante (OOM)"
	case strings.Contains(l, "failed to fit"):
		return "--fit n'a pas su placer le modèle"
	case strings.Contains(l, "failed to load model") || strings.Contains(l, "error loading model"):
		return "échec du chargement du modèle"
	}
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if n := len(lines); n > 0 && lines[n-1] != "" {
		last := strings.TrimSpace(lines[n-1])
		if len(last) > 160 {
			last = last[:160]
		}
		return "moteur arrêté : " + last
	}
	return "moteur arrêté au chargement"
}

// --- mesure et score -----------------------------------------------------------------

// tuneWeights : la forme d'un tour typique. K jetons neufs à lire (cache
// repris), G jetons à écrire, et une part fCold des tours qui relisent tout à
// froid à la profondeur D (compaction, changement de discussion).
type tuneWeights struct {
	K     float64 `json:"k"`
	G     float64 `json:"g"`
	FCold float64 `json:"f_cold"`
	From  string  `json:"from"` // « télémétrie » ou « défaut »
}

var tuneDefaultWeights = tuneWeights{K: 2000, G: 600, FCold: 0.05, From: "défaut"}

// tuneWeightsFrom tire K, G et fCold des médianes de la télémétrie (perf_log.go)
// quand elle en a assez vu ; sinon les valeurs par défaut.
func tuneWeightsFrom(recs []perfRec) tuneWeights {
	var news, gens []float64
	cold, known := 0, 0
	for _, r := range recs {
		if !r.Complete {
			continue
		}
		switch r.Kind {
		case perfBench, perfPrewarm, perfForeign:
			continue
		}
		if r.New > 0 {
			news = append(news, float64(r.New))
		}
		if r.Gen > 0 {
			gens = append(gens, float64(r.Gen))
		}
		if r.Cached != nil && r.Total >= 2048 {
			known++
			if *r.Cached*10 < r.Total {
				cold++
			}
		}
	}
	if len(news) < 20 || len(gens) < 20 {
		return tuneDefaultWeights
	}
	w := tuneWeights{K: *perfMedian(news), G: *perfMedian(gens), FCold: tuneDefaultWeights.FCold, From: "télémétrie"}
	w.K = min(max(w.K, 128), 32768)
	w.G = min(max(w.G, 16), 8192)
	if known >= 20 {
		w.FCold = float64(cold) / float64(known)
	}
	return w
}

// tuneRun : une mesure d'un essai (un passage du bench complet).
type tuneRun struct {
	TurnSec  float64 `json:"turn_sec"`            // durée estimée d'un tour : le score, plus bas = mieux
	ColdPP   float64 `json:"cold_pp,omitempty"`   // prefill à froid à D (t/s)
	CachedPP float64 `json:"cached_pp,omitempty"` // jetons neufs par seconde des tours, reprise réelle comprise
	TG       float64 `json:"tg,omitempty"`        // decode à D (code)
	ProsePP  float64 `json:"prose_pp"`
	ProseTG  float64 `json:"prose_tg"` // decode de la ligne courte (prose)
	Depth    int     `json:"depth,omitempty"`
}

// tuneRunFrom réduit un bench complet à son score. Le prefill des tours compte
// le temps RÉELLEMENT passé pour leurs jetons neufs : un hybride dont le
// micro-lot déplace les points de reprise relit plus de contexte, et le paie.
func tuneRunFrom(res *benchResult, w tuneWeights) (tuneRun, error) {
	if res == nil || res.PredictedPerSec <= 0 || res.PromptPerSecond <= 0 {
		return tuneRun{}, fmt.Errorf("mesure vide")
	}
	r := tuneRun{ProsePP: res.PromptPerSecond, ProseTG: res.PredictedPerSec}
	d := res.Depth
	if d == nil || d.Skipped != "" {
		r.TurnSec = w.K/r.ProsePP + w.G/r.ProseTG
		return r, nil
	}
	if d.Partial != "" {
		return tuneRun{}, fmt.Errorf("mesure partielle : %s", d.Partial)
	}
	if d.Cold == nil || d.Cold.PromptPerSecond <= 0 || len(d.Turns) == 0 || d.DecodePerSec <= 0 {
		return tuneRun{}, fmt.Errorf("mesure en profondeur incomplète")
	}
	var fresh int
	var ms float64
	for _, t := range d.Turns {
		if t.Cached < 0 || t.Expected <= 0 {
			fresh, ms = 0, 0
			break
		}
		fresh += t.New + t.Cached - t.Expected
		ms += t.PromptMs
	}
	r.CachedPP = d.CachedPerSec
	if fresh > 0 && ms > 0 {
		r.CachedPP = float64(fresh) / (ms / 1000)
	}
	if r.CachedPP <= 0 {
		return tuneRun{}, fmt.Errorf("prefill des tours illisible")
	}
	r.ColdPP, r.TG, r.Depth = d.Cold.PromptPerSecond, d.DecodePerSec, d.Target
	r.TurnSec = w.K/r.CachedPP + w.G/r.TG + w.FCold*float64(d.Target)/r.ColdPP
	return r, nil
}

func tuneMean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

// tuneSpread : l'écart relatif entre passages ((max-min)/moyenne).
func tuneSpread(v []float64) float64 {
	if len(v) < 2 {
		return 0
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, x := range v {
		lo, hi = min(lo, x), max(hi, x)
	}
	m := tuneMean(v)
	if m <= 0 {
		return 0
	}
	return (hi - lo) / m
}

// tuneMinGain : en dessous de 3 %, un gain se confond avec le bruit.
const tuneMinGain = 0.03

// tuneWins : le candidat (durées de tour, en secondes) bat-il la référence ?
// Seulement d'au moins max(3 %, l'écart entre passages de l'un ou de l'autre),
// et sur deux passages : un seul bon passage peut être un coup de chance.
func tuneWins(base, cand []float64) (gain, need float64, ok bool) {
	mb := tuneMean(base)
	if mb <= 0 || len(cand) == 0 {
		return 0, tuneMinGain, false
	}
	gain = (mb - tuneMean(cand)) / mb
	need = max(tuneMinGain, tuneSpread(base), tuneSpread(cand))
	return gain, need, len(cand) >= 2 && gain > need
}

// tuneWorthRepeat : un premier passage assez prometteur pour en mériter un
// second (sinon on passe au suivant, le budget est compté).
func tuneWorthRepeat(base []float64, first float64) bool {
	mb := tuneMean(base)
	return mb > 0 && (mb-first)/mb > tuneMinGain
}

// tuneTightMiB : marge VRAM minimale par carte, après le bench, pour qu'un
// essai puisse gagner. Ce que le bench n'exerce pas — encodage d'images du
// projecteur vision, autres conteneurs — vit dans cette marge.
const tuneTightMiB = 768

// tuneVisionReserveMiB : réserve de plus quand MMPROJ est posé (tampons de
// l'encodeur d'images, que le bench texte n'alloue pas). Estimation.
const tuneVisionReserveMiB = 512

// tuneHeadroom : VRAM libre minimale sur les cartes visibles (Mio). visible :
// index nvidia-smi des cartes du moteur ; vide = toutes. ok=false sans mesure.
func tuneHeadroom(gpus []gpuStat, visible []int) (minFree int, ok bool) {
	minFree = math.MaxInt
	for i, g := range gpus {
		if len(visible) > 0 && !containsInt(visible, i) {
			continue
		}
		if g.Total <= 0 {
			continue
		}
		minFree = min(minFree, g.Total-g.Used)
		ok = true
	}
	if !ok {
		return 0, false
	}
	return minFree, true
}

func containsInt(v []int, x int) bool {
	for _, y := range v {
		if y == x {
			return true
		}
	}
	return false
}

// tuneVisibleGPUs : index des cartes de CUDA_VISIBLE_DEVICES (ordre PCI, celui
// de nvidia-smi). Vide = toutes, ou des UUID qu'on ne sait pas relier.
func tuneVisibleGPUs(cfg map[string]string) []int {
	var out []int
	for _, p := range strings.Split(cfg["CUDA_VISIBLE_DEVICES"], ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// --- fichiers de l'essai -------------------------------------------------------------

// tuneLaunch : la ligne de commande composée par « loki serve » pour un essai.
type tuneLaunch struct {
	Bin   string            `json:"bin"`
	Args  []string          `json:"args"`
	Env   map[string]string `json:"env,omitempty"`
	Notes []string          `json:"notes,omitempty"`
	Probe *tuneProbe        `json:"probe,omitempty"`
}

// tuneProbe : ce que « loki serve » a sondé pour composer la ligne — dans le
// contexte exact du moteur (bibliothèques, cartes visibles, environnement).
// L'optimiseur s'en sert pour choisir ses essais.
type tuneProbe struct {
	Help       string            `json:"help"`
	GPUs       int               `json:"gpus"`
	VRAMMiB    int64             `json:"vram_mib"`
	ModelBytes int64             `json:"model_bytes"`
	MoE        bool              `json:"moe"`
	CPUThreads int               `json:"cpu_threads"` // sonde de conteneur (cpusetThreads) ; 0 = rien à dire
	ArgEnv     map[string]string `json:"arg_env,omitempty"`
	UserEnv    map[string]string `json:"user_env,omitempty"`
	QueuesEnv  bool              `json:"queues_env"`
}

// tuneProbeFrom : la sonde, tirée de ce que cmdServe a relevé.
func tuneProbeFrom(si serveSysInfo) *tuneProbe {
	return &tuneProbe{Help: si.Help, GPUs: si.GPUs, VRAMMiB: si.VRAMMiB, ModelBytes: si.ModelBytes,
		MoE: si.GGUF != nil && si.GGUF.ExpertCount > 0, CPUThreads: si.CPU.N,
		ArgEnv: si.ArgEnv, UserEnv: si.UserEnv, QueuesEnv: si.LaunchQueues != ""}
}

// writeTuneLaunch écrit la ligne de l'essai, clé d'API masquée.
func writeTuneLaunch(dir string, l tuneLaunch) error {
	args := append([]string(nil), l.Args...)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--api-key" {
			args[i+1] = "***"
		}
	}
	l.Args = args
	b, err := json.MarshalIndent(l, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "launch.json"), b, 0o600)
}

func readTuneLaunch(dir string) (tuneLaunch, error) {
	var l tuneLaunch
	b, err := os.ReadFile(filepath.Join(dir, "launch.json"))
	if err != nil {
		return l, err
	}
	return l, json.Unmarshal(b, &l)
}

// writeTuneTrialConfig pose la configuration d'un essai dans son dossier — une
// copie, jamais config.env. Relue avant d'être confiée au moteur : une valeur
// que le format ne sait pas représenter serait un autre essai que celui voulu.
func writeTuneTrialConfig(dir string, cfg map[string]string) error {
	text := formatEnv(cfg)
	back := parseEnv(text)
	if len(back) != len(cfg) {
		return fmt.Errorf("configuration d'essai non représentable")
	}
	for k, v := range cfg {
		if back[k] != v {
			return fmt.Errorf("valeur de %s non représentable dans un fichier de configuration", k)
		}
	}
	return os.WriteFile(filepath.Join(dir, "config.env"), []byte(text), 0o600)
}

func readTuneTrialConfig(dir string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "config.env"))
	if err != nil {
		return nil, fmt.Errorf("configuration d'essai illisible : %w", err)
	}
	return parseEnv(string(b)), nil
}

// tuneForceAddr : toutes les occurrences de --host / --port pointent sur
// l'adresse de l'essai (EXTRA_ARGS pourrait en nommer d'autres, et la dernière
// gagne).
func tuneForceAddr(args []string, host, port string) []string {
	out := append([]string(nil), args...)
	for i := 0; i < len(out); i++ {
		name, _, hasEq := strings.Cut(out[i], "=")
		val := ""
		switch name {
		case "--host":
			val = host
		case "--port":
			val = port
		default:
			continue
		}
		if hasEq {
			out[i] = name + "=" + val
		} else if i+1 < len(out) {
			out[i+1] = val
			i++
		}
	}
	return out
}
