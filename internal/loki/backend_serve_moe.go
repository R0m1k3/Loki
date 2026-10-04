package loki

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Experts MoE et chargement du modèle : des avis et un garde-fou, jamais de
// réécriture silencieuse du preset.
//
//	moeNotes             au lancement et dans l'éditeur : experts placés à la
//	                     main (--fit ne les place pas), UBATCH conseillé quand
//	                     --fit laisse des experts en RAM, moteur sans --fit
//	loadModeRisk         --load-mode none/mlock/dio/mmap+mlock (ou --no-mmap,
//	                     --mlock) avec plus de poids en RAM que la machine n'en
//	                     a : avertissement ; refus seulement si l'échec est
//	                     certain, LOAD_GUARD=off pour passer outre
//	autoPlacementPreset  COPIE d'un preset MoE placé à la main, en placement
//	                     automatique ; l'original n'est pas touché
//
// Rien de tout cela ne change ce que calcule le modèle : placement des poids
// et découpage des lots seulement, mêmes types de cache, même contexte.

// otPatterns : chaque motif « regex=tampon » des -ot / --override-tensor (et
// de LLAMA_ARG_OVERRIDE_TENSOR) ; llama.cpp en accepte plusieurs par drapeau,
// séparés par des virgules.
func otPatterns(extra []string, argEnv map[string]string) []string {
	vals := flagValues(extra, "-ot", "--override-tensor")
	if v := argEnv["LLAMA_ARG_OVERRIDE_TENSOR"]; v != "" {
		vals = append(vals, v)
	}
	var out []string
	for _, v := range vals {
		for _, el := range strings.Split(v, ",") {
			if el = strings.TrimSpace(el); el != "" {
				out = append(out, el)
			}
		}
	}
	return out
}

// otTargetsExperts : le motif vise-t-il les tenseurs d'experts (ffn_*_exps,
// ffn_*_chexps) ? Un motif qui ne vise que la couche d'entrée
// (per_layer_token_embd) n'en est pas.
func otTargetsExperts(pattern string) bool {
	return strings.Contains(strings.ToLower(pattern), "exps")
}

// otOnCPU : le tampon du motif est-il la RAM hôte (CPU, CPU_REPACK…) ?
func otOnCPU(el string) bool {
	_, buft, ok := strings.Cut(el, "=")
	return ok && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(buft)), "CPU")
}

// cpuExperts dit comment des experts MoE sont envoyés en RAM à la main, ou
// vide. Plus étroit que cpuWeights : un -ot ne compte que s'il vise les
// experts, et le cache KV ou un NGL partiel n'en sont pas.
func cpuExperts(extra []string, argEnv map[string]string) string {
	for _, el := range otPatterns(extra, argEnv) {
		pat, _, _ := strings.Cut(el, "=")
		if otTargetsExperts(pat) && otOnCPU(el) {
			return "-ot experts=CPU"
		}
	}
	if hasAnyFlag(extra, "--cpu-moe", "-cmoe") || envTruthy(argEnv["LLAMA_ARG_CPU_MOE"]) {
		return "--cpu-moe"
	}
	for _, v := range append(flagValues(extra, "-ncmoe", "--n-cpu-moe"), argEnv["LLAMA_ARG_N_CPU_MOE"]) {
		if v = strings.TrimSpace(v); v != "" && v != "0" {
			return "--n-cpu-moe " + v
		}
	}
	return ""
}

// fitFlagRe : « --fit » lui-même dans l'aide, pas seulement --fit-target.
var fitFlagRe = regexp.MustCompile(`(^|[\s,])--fit([\s,=\[]|$)`)

// helpSupportsFit : ce moteur sait-il placer les poids tout seul (--fit) ?
func helpSupportsFit(help string) bool { return fitFlagRe.MatchString(help) }

// serveUBatch : le micro-lot que verra le moteur (-ub d'EXTRA_ARGS, sinon
// UBATCH, sinon le défaut de Loki). 0 = illisible.
func serveUBatch(cfg map[string]string, extra []string) int {
	s := flagValue(extra, "-ub", "--ubatch-size")
	if s == "" {
		s = strings.TrimSpace(cfg["UBATCH"])
	}
	if s == "" {
		s = "512"
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// gibText : des Mio en « 12,3 Gio ».
func gibText(mib int64) string {
	return strings.Replace(strconv.FormatFloat(float64(mib)/1024, 'f', 1, 64), ".", ",", 1) + " Gio"
}

// moeNotes : ce qu'il faut savoir du placement d'un MoE, pour ce lancement.
// Fonction pure, comme nglArgs : des notes seulement, aucun drapeau.
//
//   - experts placés à la main (-ot visant les experts, --n-cpu-moe,
//     --cpu-moe) : --fit ne les place pas. Monter UBATCH seul y serait un
//     piège : le tampon de calcul grossit sur CHAQUE carte, sans personne pour
//     rééquilibrer, et le moteur finit en « cudaMalloc failed » ;
//   - placement auto, modèle plus gros que la VRAM : --fit laisse des experts
//     en RAM, recopiés vers le GPU à chaque micro-lot du prefill — un UBATCH de
//     2048 ou plus en paie quatre fois moins. --fit, lui, retient de la place
//     pour ce tampon plus gros ;
//   - moteur sans --fit (ancien, fork) : rien ne sort les experts du GPU,
//     garder --n-cpu-moe ;
//   - --fit coupé par un autre -ot : même chose.
//
// Muette sur un modèle dense, un preset externe, une VRAM inconnue (macOS,
// --device, AMD) ou une aide du moteur illisible.
func moeNotes(cfg map[string]string, extra []string, si serveSysInfo) []string {
	if isExternalConfig(cfg) || si.GGUF == nil || si.GGUF.ExpertCount <= 0 {
		return nil
	}
	ub := serveUBatch(cfg, extra)
	if r := cpuExperts(extra, si.ArgEnv); r != "" {
		n := "experts placés à la main (" + r + ") : --fit ne les place pas"
		if ub > 0 && ub <= 512 {
			n += " ; UBATCH plus grand = tampon de calcul plus gros sur chaque GPU : monte --n-cpu-moe d'abord, " +
				"ou passe au placement auto (--fit, « Dupliquer en placement auto » dans l'éditeur)"
		}
		return []string{n}
	}
	modelMiB := si.ModelBytes >> 20
	if modelMiB <= 0 || si.VRAMMiB <= 0 || si.UnifiedMem || strings.TrimSpace(si.Help) == "" {
		return nil
	}
	cards := int64(si.GPUs)
	if cards < 1 {
		cards = 1
	}
	if modelMiB <= si.VRAMMiB-fitTargetMin*cards {
		return nil // tout tient sur GPU : pas d'expert recopié
	}
	sizes := "modèle " + gibText(modelMiB) + ", VRAM " + gibText(si.VRAMMiB)
	if !helpSupportsFit(si.Help) {
		// Un NGL chiffré sous 999 laisse déjà des couches (et leurs experts) au
		// CPU : l'utilisateur a tranché.
		if n, err := strconv.Atoi(strings.TrimSpace(firstNonEmpty(flagValue(extra, "-ngl", "--n-gpu-layers", "--gpu-layers"), cfg["NGL"]))); err == nil && n < 999 {
			return nil
		}
		return []string{"MoE plus gros que la VRAM (" + sizes + ") : ce moteur ne place pas les experts (pas de --fit) — " +
			"garde --n-cpu-moe"}
	}
	if why := fitBlocker(cfg, extra, si); why != "" {
		if why == "surcharge de tenseurs" {
			return []string{"MoE plus gros que la VRAM (" + sizes + ") et --fit coupé par un -ot qui ne vise pas les " +
				"experts : rien ne les sort du GPU — --n-cpu-moe, ou retire ce -ot"}
		}
		return nil
	}
	if ub > 0 && ub <= 512 {
		return []string{"UBATCH 2048+ recommandé : --fit laisse des experts en RAM (" + sizes + "), recopiés vers le " +
			"GPU à chaque micro-lot du prefill ; BATCH 4096 avec — à mesurer (bench complet)"}
	}
	return nil
}

// firstNonEmpty : la première valeur non vide.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// loadArgEnv : les variables qui règlent le chargement sans drapeau.
// LLAMA_ARG_RPC n'en est pas une, mais dit que des poids partent sur des
// machines distantes (loadModeRisk).
var loadArgEnv = []string{"LLAMA_ARG_LOAD_MODE", "LLAMA_ARG_MLOCK", "LLAMA_ARG_NO_MMAP", "LLAMA_ARG_RPC"}

// probeLoadEnv relève ces variables. Appelée après probeServeGPUs, qui
// remet ArgEnv à zéro.
func probeLoadEnv(si *serveSysInfo) {
	if si.ArgEnv == nil {
		si.ArgEnv = map[string]string{}
	}
	for _, k := range loadArgEnv {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			si.ArgEnv[k] = v
		}
	}
}

// residentLoadMode : le mode de chargement qui garde ENTIÈREMENT en RAM les
// poids laissés au CPU, tel qu'écrit (« --load-mode none », « --no-mmap »…),
// ou vide. mmap (et auto) laisse le système relire les pages au besoin : lent
// quand la RAM manque, jamais fatal. none et dio copient les poids en mémoire
// anonyme, mlock et mmap+mlock les verrouillent. Lu sur la ligne finale
// (normalizeLoadFlags a déjà traduit l'une ou l'autre forme), puis dans les
// LLAMA_ARG_* si elle se tait.
func residentLoadMode(args []string, argEnv map[string]string) string {
	mode := strings.ToLower(strings.TrimSpace(flagValue(args, "--load-mode", "-lm")))
	mlock, nommap := hasAnyFlag(args, "--mlock"), hasAnyFlag(args, "--no-mmap")
	if mode == "" && !mlock && !nommap {
		mode = strings.ToLower(strings.TrimSpace(argEnv["LLAMA_ARG_LOAD_MODE"]))
		if mode == "" {
			mlock, nommap = envTruthy(argEnv["LLAMA_ARG_MLOCK"]), envTruthy(argEnv["LLAMA_ARG_NO_MMAP"])
		}
	}
	switch mode {
	case "none", "mlock", "dio", "mmap+mlock":
		return "--load-mode " + mode
	}
	switch {
	case mlock && nommap:
		return "--mlock --no-mmap"
	case mlock:
		return "--mlock"
	case nommap:
		return "--no-mmap"
	}
	return ""
}

// loadGuardMarker : la fin du message de refus, que l'interface reconnaît
// dans le journal du service (modelLoadError).
const loadGuardMarker = "LOAD_GUARD=off pour lancer quand même"

// loadGuardOff : LOAD_GUARD=off, la clé d'échappement du refus.
func loadGuardOff(cfg map[string]string) bool {
	switch strings.ToLower(strings.TrimSpace(cfg["LOAD_GUARD"])) {
	case "off", "0", "false", "non", "no":
		return true
	}
	return false
}

// loadGuardExitStatus : le code de sortie de « loki serve » quand LOAD_GUARD
// refuse. Le refus est certain et ne dépend que du preset et de la machine :
// relancer ne changerait rien. Sous systemd, Restart=on-failure relançait
// pourtant toutes les trois secondes, sans fin ; l'unité écrite par
// « loki install » porte donc RestartPreventExitStatus pour ce code-là
// (EX_CONFIG de sysexits.h : erreur de configuration). Le conteneur, lui, ne
// relance jamais le moteur de lui-même.
const loadGuardExitStatus = 78

// unitPreventsRefusalRestart : l'unité systemd (son texte) s'abstient-elle de
// relancer sur loadGuardExitStatus ? Une unité écrite par une version
// d'avant ne le fait pas.
func unitPreventsRefusalRestart(unit string) bool {
	for _, line := range strings.Split(unit, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.TrimSpace(k) != "RestartPreventExitStatus" {
			continue
		}
		for _, f := range strings.Fields(v) {
			if f == strconv.Itoa(loadGuardExitStatus) {
				return true
			}
		}
	}
	return false
}

// loadGuardRefusal : l'erreur rendue par cmdServe pour un refus, avec son code
// de sortie. Un superviseur qui relancerait quand même sur ce code (unité
// systemd d'une version d'avant, launchd, qui relance toute sortie non nulle)
// reçoit 0 : une sortie « propre » n'est pas relancée, et la raison reste en
// clair dans le journal, où l'interface la reconnaît (modelLoadError). Le
// moteur d'essai de l'optimiseur garde le vrai code : c'est lui qui dit
// l'échec à « loki tune ».
func loadGuardRefusal(refuse string, trial bool) error {
	code := loadGuardExitStatus
	if !trial {
		if restarts, hint := supervisorRestartsRefusal(); restarts {
			code = 0
			if hint != "" {
				fmt.Fprintln(os.Stderr, "[loki serve] "+hint)
			}
		}
	}
	return &exitStatusError{code: code, err: fmt.Errorf("%s", refuse)}
}

// loadModeRisk : un mode de chargement résident avec trop de poids en RAM.
// Fonction pure ; args est la ligne finale (ou EXTRA_ARGS normalisé).
//
// llama.cpp #26110 : un modèle de 65 Go sur 60 Go de RAM, --load-mode none →
// swap, décodage de 25 à 7,5 t/s ; mlock → processus tué faute de mémoire.
// Seuls comptent les poids qui RESTENT en RAM : ceux qui partent en VRAM n'y
// séjournent pas. On en connaît une borne basse — modèle moins VRAM totale
// (tout le modèle sur macOS, mémoire unifiée) — et une estimation haute quand
// la VRAM est inconnue (le modèle entier).
//
//   - refuse : la borne basse dépasse 90 % de la RAM effective (limite du
//     conteneur comprise) — l'échec est certain. LOAD_GUARD=off lève le refus ;
//   - warn   : l'estimation dépasse 80 % — on prévient, on lance.
//
// Un refus fait sortir « loki serve » en erreur, comme un modèle introuvable :
// mieux vaut la raison en clair qu'un moteur tué en boucle par le noyau. Le code
// de sortie (loadGuardRefusal) empêche le superviseur de relancer en boucle.
func loadModeRisk(cfg map[string]string, args []string, si serveSysInfo) (warn, refuse string) {
	if isExternalConfig(cfg) {
		return "", ""
	}
	// Un moteur qui connaît --load-mode n'a plus LLAMA_ARG_MLOCK ni
	// LLAMA_ARG_NO_MMAP : restées dans l'environnement, elles n'ont aucun effet
	// et ne doivent pas faire refuser un lancement en mmap.
	env := si.ArgEnv
	if helpSupportsLoadMode(si.Help) && (env["LLAMA_ARG_MLOCK"] != "" || env["LLAMA_ARG_NO_MMAP"] != "") {
		env = map[string]string{"LLAMA_ARG_LOAD_MODE": si.ArgEnv["LLAMA_ARG_LOAD_MODE"]}
	}
	mode := residentLoadMode(args, env)
	modelMiB := si.ModelBytes >> 20
	if mode == "" || modelMiB <= 0 || si.RAMMiB <= 0 {
		return "", ""
	}
	// Serveurs RPC : une part des poids part sur d'autres machines, la VRAM
	// locale ne borne plus rien — inconnu, donc un avis au plus, jamais un refus.
	remote := hasAnyFlag(args, "--rpc") || strings.TrimSpace(si.ArgEnv["LLAMA_ARG_RPC"]) != ""
	low, est, upTo, unsure := int64(0), modelMiB, true, false
	switch {
	case remote:
	case si.UnifiedMem:
		low, est, upTo = modelMiB, modelMiB, false
	case si.EngineVRAMMiB > 0:
		// Les cartes que le moteur liste lui-même : rien au-delà ne peut
		// recevoir de poids, la borne basse est sûre.
		low = max(modelMiB-si.EngineVRAMMiB, 0)
		est, upTo = low, false
	case si.VRAMMiB > 0:
		// nvidia-smi seulement : un moteur Vulkan sur cartes mixtes NVIDIA +
		// AMD en utilise davantage, un moteur ROCm d'autres. Une estimation,
		// jamais une borne : un avis au plus (voir probeLoadDevices).
		est, upTo, unsure = max(modelMiB-si.VRAMMiB, 0), false, true
	}
	ram := si.RAMMiB
	what := "environ "
	if upTo {
		what = "jusqu'à "
	}
	if unsure && est*10 > ram*9 {
		return fmt.Sprintf("%s : environ %s de poids en RAM (modèle %s, VRAM NVIDIA %s) pour %s — échec probable ; "+
			"refus seulement si les cartes listées par le moteur le confirment, lancement sinon ; "+
			"--load-mode mmap est plus sûr", mode, gibText(est), gibText(modelMiB), gibText(si.VRAMMiB), gibText(ram)), ""
	}
	if low*10 > ram*9 {
		msg := fmt.Sprintf("%s : au moins %s de poids restent en RAM (modèle %s) pour %s de RAM, limite du conteneur "+
			"comprise — échec certain : swap et décodage effondré, ou processus tué (llama.cpp #26110). "+
			"--load-mode mmap laisse le système relire les pages au besoin", mode, gibText(low), gibText(modelMiB), gibText(ram))
		if loadGuardOff(cfg) {
			return msg + " ; LOAD_GUARD=off : lancé quand même", ""
		}
		return "", msg + " ; " + loadGuardMarker
	}
	if est*10 > ram*8 {
		return fmt.Sprintf("%s : %s%s de poids en RAM pour %s — au-delà, swap (décodage de 25 à 7,5 t/s dans "+
			"llama.cpp #26110) ou processus tué ; --load-mode mmap est plus sûr", mode, what, gibText(est), gibText(ram)), ""
	}
	return "", ""
}

// loadGuardNeedsDevices : un refus se profile sur la seule VRAM NVIDIA
// (nvidia-smi) ? Il ne sera prononcé que sur les cartes du moteur lui-même —
// à lire d'abord (probeLoadDevices). Fonction pure.
func loadGuardNeedsDevices(cfg map[string]string, args []string, si serveSysInfo) bool {
	if si.EngineVRAMMiB > 0 || si.VRAMMiB <= 0 {
		return false
	}
	si.EngineVRAMMiB = si.VRAMMiB
	_, refuse := loadModeRisk(cfg, args, si)
	return refuse != ""
}

// engineDevsVRAM : la VRAM des cartes listées par le moteur, toutes
// additionnées — sans --device, llama.cpp ne place rien ailleurs, et une carte
// qu'il listerait sans s'en servir (iGPU) ne fait que rendre le refus plus
// rare. 0 (inconnu) si la liste est vide ou si une carte annonce 0 Mio (déjà
// pleine : lecture transitoire, voir handleBackendDevices).
func engineDevsVRAM(devs []splitDev) int64 {
	var total int64
	for _, d := range devs {
		if d.TotalMiB <= 0 {
			return 0
		}
		total += d.TotalMiB
	}
	return total
}

// probeLoadDevices : seulement quand loadGuardNeedsDevices — un mode de
// chargement résident qu'on s'apprête à refuser. Les cartes de SPLIT_MODE si
// elles sont déjà lues, sinon --list-devices dans l'environnement du
// lancement (sélection GPU comprise). Échec de lecture : EngineVRAMMiB reste
// à 0, loadModeRisk avertit sans refuser. args : EXTRA_ARGS normalisé.
func probeLoadDevices(cfg map[string]string, args []string, bin string, si *serveSysInfo) {
	if !loadGuardNeedsDevices(cfg, args, *si) {
		return
	}
	devs := si.SplitDevs
	if len(devs) == 0 {
		devs = listEngineDevices(bin)
	}
	si.EngineVRAMMiB = engineDevsVRAM(devs)
}

// previewServeHelp : l'aide supposée d'un moteur récent, pour les aperçus de
// l'éditeur (le process web ne lance pas le moteur pour lire son aide). Le
// lancement, lui, décide sur la vraie aide.
const previewServeHelp = "--load-mode  --fit [on|off]  --fit-target"

// moePreview : les avis de moeNotes et loadModeRisk pour l'éditeur, calculés
// comme au lancement, sur un moteur supposé récent.
func moePreview(content string) (notes []string, load string) {
	cfg := parseEnv(content)
	if isExternalConfig(cfg) {
		return nil, ""
	}
	si := serveSysInfo{Help: previewServeHelp, ArgEnv: map[string]string{}}
	for _, k := range append(append(append([]string{}, serveArgEnv...), fitArgEnv...), loadArgEnv...) {
		if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
			si.ArgEnv[k] = v
		}
	}
	if m := strings.TrimSpace(cfg["MODEL"]); m != "" {
		if p, err := resolveServeModelPath(m); err == nil {
			si.Model = p
		}
	}
	if si.Model == "" {
		return nil, ""
	}
	extra := splitArgs(cfg["EXTRA_ARGS"])
	probeCacheRAM(cfg, extra, &si)
	norm, _ := normalizeLoadFlags(extra, true)
	// Les cartes du moteur, sans le lancer : la dernière liste complète qu'il a
	// donnée à l'éditeur (devices.json). Absente : un avis, pas un refus.
	if loadGuardNeedsDevices(cfg, norm, si) {
		bin := prebuiltResolveBin(firstNonEmpty(cfg["BIN"], ReadConfig()["BIN"]))
		if list, ok := devPersistGet(bin + "\x00" + cfg["CUDA_VISIBLE_DEVICES"]); ok {
			si.EngineVRAMMiB = engineDevsVRAM(splitDevsFrom(list))
		}
	}
	warn, refuse := loadModeRisk(cfg, norm, si)
	if refuse != "" {
		warn = "refusé au lancement — " + refuse
	}
	return moeNotes(cfg, extra, si), warn
}

// autoPlacementPreset prépare la COPIE d'un preset MoE placé à la main, en
// placement automatique. Fonction pure : elle rend le nouveau contenu et la
// liste de ce qui change ; l'appelant l'enregistre sous un nouveau nom, et le
// preset d'origine reste tel quel — le repli si --fit échoue.
//
// Dans la copie :
//
//   - retirés d'EXTRA_ARGS : les -ot qui visent les experts, un -ot qui
//     n'envoie au CPU que per_layer_token_embd (couche d'entrée : llama.cpp la
//     laisse toujours au CPU, le motif est redondant mais coupe --fit),
//     --n-cpu-moe, --cpu-moe, -ngl, --tensor-split, --fit off, -b/-ub et les
//     drapeaux de chargement ;
//   - NGL retiré (Loki écrit -ngl auto sur un moteur qui sait placer),
//     UBATCH=2048, BATCH=4096, « --load-mode mmap » (rien sur un moteur
//     ancien, qui charge déjà ainsi) ;
//   - CTX, cache KV, échantillonnage, raisonnement : intouchés.
//
// Refus (err) quand la copie ne tournerait pas en --fit — un autre -ot, -sm
// row/tensor, SPLIT_MODE=tensor, --n-cpu-ffn — ou quand CTX vaut 0 (--fit
// pourrait réduire le contexte), et quand il n'y a rien à migrer. Jamais de
// --tensor-read-lazy ni --lazy-mode : leur nom change d'un moteur à l'autre.
func autoPlacementPreset(content string) (out string, changes []string, err error) {
	cfg := parseEnv(content)
	if isExternalConfig(cfg) {
		return "", nil, fmt.Errorf("preset externe : le modèle tourne ailleurs")
	}
	extra := splitArgs(cfg["EXTRA_ARGS"])
	var kept []string
	migrated := false
	drop := func(s string) { changes = append(changes, "retiré : "+s) }
	for i := 0; i < len(extra); i++ {
		a := extra[i]
		name, val, hasEq := strings.Cut(a, "=")
		value := func() string {
			if hasEq {
				return val
			}
			if i+1 < len(extra) {
				i++
				return extra[i]
			}
			return ""
		}
		switch name {
		case "-ot", "--override-tensor":
			v := value()
			var rest []string
			for _, el := range strings.Split(v, ",") {
				el = strings.TrimSpace(el)
				pat, _, _ := strings.Cut(el, "=")
				switch {
				case el == "":
				case otTargetsExperts(pat):
					drop(name + " " + el + " (experts : --fit les place)")
					migrated = true
				case strings.Contains(strings.ToLower(pat), "per_layer_token_embd") && otOnCPU(el):
					drop(name + " " + el + " (couche d'entrée, toujours au CPU)")
				default:
					rest = append(rest, el)
				}
			}
			if len(rest) > 0 {
				kept = append(kept, name, strings.Join(rest, ","))
			}
		case "-ncmoe", "--n-cpu-moe":
			v := value()
			drop(name + " " + v)
			if strings.TrimSpace(v) != "0" {
				migrated = true
			}
		case "-cmoe", "--cpu-moe":
			drop(name)
			migrated = true
		case "-ngl", "--n-gpu-layers", "--gpu-layers", "-ts", "--tensor-split",
			"-b", "--batch-size", "-ub", "--ubatch-size", "--load-mode", "-lm":
			drop(name + " " + value())
		case "--mlock", "--no-mmap", "--mmap":
			drop(name)
		case "-fit", "--fit":
			v := value()
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "off", "disabled", "false", "0":
				drop(name + " " + v)
			default:
				kept = append(kept, name, v)
			}
		default:
			kept = append(kept, a)
		}
	}
	if !migrated {
		return "", nil, fmt.Errorf("rien à migrer : ce preset ne place pas d'experts à la main (-ot …exps…, --n-cpu-moe, --cpu-moe)")
	}
	kept = append(kept, "--load-mode", "mmap")
	nc := map[string]string{}
	for k, v := range cfg {
		nc[k] = v
	}
	nc["EXTRA_ARGS"] = joinArgs(kept)
	nc["UBATCH"], nc["BATCH"] = "2048", "4096"
	delete(nc, "NGL")
	if ctx, ok := ctxFixed(nc, kept); !ok {
		return "", nil, fmt.Errorf("contexte « %s » non chiffré : --fit pourrait le réduire pour tenir — donne un CTX chiffré d'abord", ctx)
	}
	if splitModeKey(nc) == "tensor" {
		return "", nil, fmt.Errorf("SPLIT_MODE=tensor coupe --fit : retire-le d'abord")
	}
	if why := fitBlocker(nc, kept, serveSysInfo{Help: previewServeHelp, ArgEnv: map[string]string{}}); why != "" {
		return "", nil, fmt.Errorf("--fit resterait inactif (%s) : retire-le à la main d'abord", why)
	}
	if ngl := strings.TrimSpace(cfg["NGL"]); ngl != "" {
		changes = append(changes, "NGL="+ngl+" retiré : le moteur place les couches (-ngl auto)")
	}
	changes = append(changes, "UBATCH=2048, BATCH=4096 (avant : "+firstNonEmpty(cfg["UBATCH"], "512")+" / "+
		firstNonEmpty(cfg["BATCH"], "2048")+")", "--load-mode mmap")
	ctx, _ := ctxFixed(nc, kept)
	changes = append(changes, "CTX conservé : "+ctx)
	if k, v := serveKVTypes(nc); k != "" || v != "" ||
		flagValue(kept, "-ctk", "--cache-type-k") != "" || flagValue(kept, "-ctv", "--cache-type-v") != "" {
		changes = append(changes, "cache KV conservé tel quel — garde-le identique pour comparer les deux presets")
	}
	out = content
	out = presetSetKey(out, "EXTRA_ARGS", nc["EXTRA_ARGS"])
	out = presetSetKey(out, "UBATCH", "2048")
	out = presetSetKey(out, "BATCH", "4096")
	out = presetSetKey(out, "NGL", "")
	return out, changes, nil
}

// joinArgs : l'inverse de splitArgs — un argument qui contient un blanc est
// entouré d'apostrophes (de guillemets s'il en contient une), pour être relu
// d'un seul tenant.
func joinArgs(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		switch {
		case a == "":
			parts[i] = `""`
		case strings.ContainsAny(a, " \t"):
			if strings.Contains(a, "'") {
				parts[i] = `"` + a + `"`
			} else {
				parts[i] = "'" + a + "'"
			}
		default:
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
}

// presetSetKey remplace les lignes KEY=… d'un preset (toutes : parseEnv
// retient la dernière, une ancienne restée plus bas l'emporterait), l'ajoute à
// la fin si elle manque, les retire si val est vide. Le reste du fichier —
// commentaires, ordre, NAME — ne bouge pas : c'est l'équivalent de cfgWriteKey
// côté éditeur.
func presetSetKey(content, key, val string) string {
	re := regexp.MustCompile(`(?m)^[ \t]*(?:export[ \t]+)?` + regexp.QuoteMeta(key) + `[ \t]*=.*$`)
	if val == "" {
		return regexp.MustCompile(`(?m)^[ \t]*(?:export[ \t]+)?`+regexp.QuoteMeta(key)+`[ \t]*=.*(?:\n|$)`).
			ReplaceAllLiteralString(content, "")
	}
	line := key + "=" + quoteValue(val)
	if re.MatchString(content) {
		return re.ReplaceAllLiteralString(content, line)
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return content + line + "\n"
}
