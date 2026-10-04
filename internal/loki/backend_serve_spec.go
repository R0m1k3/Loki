package loki

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Décodage spéculatif géré par Loki : une tête MTP (prédiction de plusieurs
// jetons) intégrée au modèle ou publiée à part, ou un petit modèle brouillon.
//
//	SPEC=off (défaut)  → rien, même avec MODEL_DRAFT
//	SPEC=auto          → seulement si tout est réuni (voir specAutoBlocker)
//	SPEC=mtp           → imposé, sans les garde-fous d'auto, avec un avertissement
//	MODEL_DRAFT=<nom>  → la tête ou le brouillon, résolu comme MMPROJ
//	SPEC_N_MAX=<n>     → --spec-draft-n-max (défaut du moteur : 3)
//	SPEC_SAMPLING=probabilistic → seulement avec SPEC=mtp ; greedy sinon
//	SPEC=ngram         → n-grammes tirés du contexte (ngram-mod), sans brouillon
//	SPEC=mtp+ngram     → les deux ; un n-gramme trouvé passe avant la tête MTP
//
// Rien n'y touche au modèle : chaque jeton émis est tiré par l'échantillonneur
// du modèle cible, un jeton du brouillon n'est gardé que s'il coïncide
// (common_sampler_sample_and_accept_n). Le prix est ailleurs : 1 à 2 Go de VRAM
// en plus, un prefill parfois plus lent, et une fonction très récente qui peut
// empêcher le moteur de démarrer. D'où l'opt-in, les garde-fous d'auto et le
// jeton de tentative (specAutoVerdict) : un lancement automatique qui n'a
// jamais répondu ne se retente pas avec la même configuration.
//
// Une vérification par lot n'emprunte pas les mêmes noyaux qu'un décodage jeton
// par jeton : même distribution, mais pas le même texte au bit près — deux
// candidats presque à égalité peuvent s'inverser à graine égale, comme entre
// prefill et décodage. On compare donc des sessions rejouées, pas des diffs.

const (
	// specAutoMinBuild : premier build officiel où Loki ose l'auto (MTP serveur,
	// PR #22673 et suites). Build inconnu = non : un compilé maison ou un fork
	// peut connaître le drapeau sans le tenir.
	specAutoMinBuild = 11009
	specNMaxLimit    = 64

	// Bornes de ngram-mod, toujours écrites en clair : --spec-default pose les
	// mêmes aujourd'hui, mais son code porte un TODO qui annonce d'autres types —
	// une mise à jour du moteur changerait le réglage sans un mot. Un brouillon
	// part dès 24 jetons qui se répètent mot pour mot, et fait 48 à 64 jetons :
	// ce que recopie une boucle d'outils (chemins, diffs, arguments JSON).
	specNgramNMatch = 24
	specNgramNMin   = 48
	specNgramNMax   = 64

	// ggmlOffloadMinBatchDefault : seuil par défaut du moteur (ggml-cuda.cu) à
	// partir duquel un lot recopie vers le GPU les poids restés en RAM.
	ggmlOffloadMinBatchDefault = 32
)

// specArgEnv : les variables qui règlent la spéculation sans drapeau. Posées,
// elles valent un réglage d'EXTRA_ARGS : Loki se tait.
var specArgEnv = []string{"LLAMA_ARG_SPEC_TYPE", "LLAMA_ARG_SPEC_DRAFT_MODEL", "LLAMA_ARG_SPEC_DRAFT_HF_REPO",
	"LLAMA_ARG_SPEC_DRAFT_N_MAX", "LLAMA_ARG_SPEC_DRAFT_SAMPLING"}

// specUserFlags : la spéculation réglée à la main. Les ajouter EN PLUS ferait
// cumuler les types (--spec-type s'additionne, sans l'avertissement « specified
// multiple times » des autres drapeaux) ou charger deux brouillons.
var specUserFlags = []string{"--spec-type", "--spec_type", "-md", "--model-draft", "--spec-draft-model",
	"-hfd", "-hfrd", "--spec-draft-hf", "--hf-repo-draft", "--spec-default"}

// specUserPrefixes : des réglages de n-grammes à la main (--spec-ngram-mod-n-max,
// --spec-ngram-simple-size-n…) disent la même chose qu'un --spec-type.
var specUserPrefixes = []string{"--spec-ngram-", "--spec_ngram_"}

// hasFlagPrefix : un drapeau d'args commence-t-il par l'un de ces préfixes ?
func hasFlagPrefix(args []string, prefixes ...string) string {
	for _, a := range args {
		for _, p := range prefixes {
			if strings.HasPrefix(a, p) {
				name, _, _ := strings.Cut(a, "=")
				return name
			}
		}
	}
	return ""
}

// probeSpec : variables du moteur, puis MODEL_DRAFT résolu comme MMPROJ (nom
// simple cherché dans les dossiers déclarés, ou chemin absolu). Introuvable ne
// bloque PAS le lancement, contrairement au projecteur : le brouillon n'est
// qu'une accélération, le moteur démarre sans et la note dit pourquoi.
func probeSpec(cfg map[string]string, si *serveSysInfo) {
	if si.ArgEnv == nil {
		si.ArgEnv = map[string]string{}
	}
	for _, k := range specArgEnv {
		if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
			si.ArgEnv[k] = v
		}
	}
	d := strings.TrimSpace(cfg["MODEL_DRAFT"])
	if d == "" {
		return
	}
	p, err := resolveServeModelPath(d)
	if err == nil {
		_, err = os.Stat(p)
	}
	if err != nil {
		si.DraftErr = err.Error()
		return
	}
	si.Draft = p
	if g, err := ggufMeta(p); err == nil {
		si.DraftGGUF = &g
	}
}

// specSidecarArch : têtes de spéculation qui ne sont pas des modèles autonomes
// (llama.cpp : draft-eagle3, draft-dflash/dspark). Un -md vers elles avec
// draft-simple ne démarrerait pas : leur type se règle à la main.
func specSidecarArch(arch string) bool {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "eagle3", "dflash":
		return true
	}
	return false
}

// specMode lit SPEC : "off", "auto", "mtp", "ngram", "mtp+ngram", ou "" si
// illisible.
func specMode(cfg map[string]string) string {
	switch v := strings.ToLower(strings.TrimSpace(cfg["SPEC"])); v {
	case "", "off", "non", "no", "0":
		return "off"
	case "auto", "mtp", "ngram", "mtp+ngram":
		return v
	case "ngram+mtp":
		return "mtp+ngram"
	}
	return ""
}

// specUserSet : EXTRA_ARGS ou l'environnement règlent déjà la spéculation.
func specUserSet(extra []string, argEnv map[string]string) bool {
	if hasAnyFlag(extra, specUserFlags...) || hasFlagPrefix(extra, specUserPrefixes...) != "" {
		return true
	}
	for _, k := range []string{"LLAMA_ARG_SPEC_TYPE", "LLAMA_ARG_SPEC_DRAFT_MODEL", "LLAMA_ARG_SPEC_DRAFT_HF_REPO"} {
		if argEnv[k] != "" {
			return true
		}
	}
	return false
}

// helpSupportsMTP : le moteur connaît le type draft-mtp ET le drapeau actuel
// du nombre de jetons — un moteur qui n'a que l'ancien --draft-max est d'une
// autre génération, où un MTP n'existe pas.
func helpSupportsMTP(help string) bool {
	return strings.Contains(help, "draft-mtp") && strings.Contains(help, "--spec-draft-n-max")
}

// ctxFixed : le contexte que verra le moteur (EXTRA_ARGS, sinon CTX, sinon le
// défaut de buildServeArgs), et s'il est chiffré. Avec 0 ou une valeur
// illisible, --fit a le droit de RÉDUIRE le contexte pour tenir.
func ctxFixed(cfg map[string]string, extra []string) (string, bool) {
	c := flagValue(extra, "-c", "--ctx-size")
	if c == "" {
		c = strings.TrimSpace(cfg["CTX"])
	}
	if c == "" {
		c = "32768"
	}
	n, err := strconv.Atoi(c)
	return c, err == nil && n > 0
}

// statefulSampler : un échantillonneur dont l'état avance à chaque tirage
// (mirostat, adaptive-p). Le rejet probabiliste fait d'abord tirer le modèle
// cible, puis peut émettre un autre jeton : ces échantillonneurs avanceraient
// sur un jeton que la réponse ne contient pas. Le mode greedy, lui, reste exact.
func statefulSampler(extra []string) string {
	for _, v := range flagValues(extra, "--mirostat") {
		if v = strings.TrimSpace(v); v != "" && v != "0" {
			return "--mirostat " + v
		}
	}
	for _, v := range flagValues(extra, "--samplers") {
		if strings.Contains(strings.ToLower(strings.ReplaceAll(v, "-", "_")), "adaptive_p") {
			return "--samplers adaptive-p"
		}
	}
	for _, v := range flagValues(extra, "--sampling-seq", "--sampler-seq") {
		if strings.Contains(v, "a") {
			return "--sampling-seq avec adaptive-p"
		}
	}
	return ""
}

// specAutoBlocker dit pourquoi SPEC=auto ne s'active pas, ou vide. Chaque
// garde répond à une panne précise :
//
//   - qwen4exp : MTP fusionné il y a quelques jours, OOM signalés en multi-GPU ;
//   - placement manuel (couches fixées, -ts, -ot, experts sur CPU, -sm row,
//     --fit off, -dev) : seul --fit compte la VRAM de la tête MTP ;
//   - contexte non chiffré : fit aurait le droit de le RÉDUIRE pour faire de
//     la place — de l'information perdue pour le modèle ;
//   - projecteur vision : une VRAM déjà serrée par un second modèle ;
//   - build officiel inconnu ou trop ancien ;
//   - un essai précédent avec la même configuration n'a jamais répondu.
func specAutoBlocker(cfg map[string]string, extra []string, si serveSysInfo) string {
	if si.GGUF != nil && strings.EqualFold(si.GGUF.Arch, "qwen4exp") {
		return "qwen4exp : MTP trop récent pour ce moteur, SPEC=mtp pour l'imposer"
	}
	if why := fitBlocker(cfg, extra, si); why != "" {
		return "placement manuel (" + why + ") — --fit ne compte pas la VRAM du brouillon"
	}
	if hasAnyFlag(extra, "-dev", "--device") || si.ArgEnv["LLAMA_ARG_DEVICE"] != "" {
		return "placement manuel (--device) — --fit ne compte pas la VRAM du brouillon"
	}
	if _, ok := ctxFixed(cfg, extra); !ok {
		return "contexte non chiffré — fit pourrait le réduire pour loger le brouillon"
	}
	if si.MMProj != "" || hasAnyFlag(extra, "--mmproj", "-mm") {
		return "projecteur vision chargé"
	}
	if si.EngineBuild < specAutoMinBuild {
		if si.EngineBuild <= 0 {
			return "build du moteur inconnu (compilé ou fork)"
		}
		return fmt.Sprintf("moteur b%d, b%d au moins", si.EngineBuild, specAutoMinBuild)
	}
	return si.SpecAutoBlocked
}

// specArgs : drapeaux de décodage spéculatif que Loki ajoute, ce qu'il en dit,
// et s'ils viennent de SPEC=auto (cmdServe pose alors le jeton de tentative).
// Fonction pure ; cmdServe a déjà résolu MODEL_DRAFT et lu les GGUF.
func specArgs(cfg map[string]string, extra []string, si serveSysInfo) (args, notes []string, auto bool) {
	mode := specMode(cfg)
	draftKey := strings.TrimSpace(cfg["MODEL_DRAFT"])
	switch mode {
	case "":
		return nil, []string{"SPEC=" + cfg["SPEC"] + " illisible (off, auto, mtp, ngram ou mtp+ngram) : sans décodage spéculatif"}, false
	case "off":
		if draftKey != "" {
			return nil, []string{"MODEL_DRAFT ignoré : SPEC=off (auto ou mtp pour s'en servir)"}, false
		}
		return nil, nil, false
	}
	if specUserSet(extra, si.ArgEnv) {
		return nil, []string{"décodage spéculatif déjà réglé dans EXTRA_ARGS ou LLAMA_ARG_* : SPEC ignoré"}, false
	}
	skip := func(why string) ([]string, []string, bool) {
		return nil, []string{"SPEC=" + mode + " : " + why + " — sans décodage spéculatif"}, false
	}

	// N-grammes : leurs garde-fous d'abord, ils valent pour les deux formes.
	if mode == "ngram" || mode == "mtp+ngram" {
		if why := ngramBlocker(cfg, extra, si, mode); why != "" {
			return skip(why)
		}
	}
	if mode == "ngram" {
		args, notes = ngramSpecArgs(cfg, extra, si, "")
		return args, notes, false
	}

	src, mtp, why, quiet := specSource(mode, draftKey, si)
	if mode == "mtp+ngram" && !mtp {
		// Pas de tête MTP utilisable : la passer quand même ferait mourir le
		// moteur sur « failed to create MTP context », et boucler. Les n-grammes,
		// eux, n'ont besoin de rien.
		if why == "" {
			why = "MODEL_DRAFT n'est pas une tête MTP"
		}
		args, notes = ngramSpecArgs(cfg, extra, si, "SPEC=mtp+ngram : sans MTP ("+why+") → n-grammes seuls. ")
		return args, notes, false
	}
	if why != "" {
		return skip(why)
	}
	if quiet {
		return nil, nil, false
	}
	args = src
	if mode == "mtp+ngram" {
		// Une seule liste : --spec-type s'additionne, mais une valeur unique se
		// lit d'un coup d'œil dans le journal.
		args[len(args)-1] = "draft-mtp,ngram-mod"
		args = append(args, ngramModParams()...)
	}

	label := "brouillon " + baseName(si.Draft)
	if mtp {
		label = "tête MTP"
	}
	if mode == "auto" {
		if why := specAutoBlocker(cfg, extra, si); why != "" {
			return skip(why)
		}
		auto = true
		notes = append(notes, "SPEC=auto → "+label+" : sortie inchangée (chaque jeton est vérifié par le modèle), "+
			"~1-2 Go de VRAM en plus. Mesure le prefill ; SPEC=off pour couper.")
	} else {
		note := "SPEC=" + mode + " → " + label + " imposé : sortie inchangée, ~1-2 Go de VRAM en plus"
		if mode == "mtp+ngram" {
			note += " ; n-grammes en plus (ngram-mod, prioritaires quand ils trouvent une répétition)"
		}
		if why := fitBlocker(cfg, extra, si); why != "" || hasAnyFlag(extra, "-dev", "--device") {
			if why == "" {
				why = "--device"
			}
			note += " ; placement manuel (" + why + ") : --fit ne compte pas le brouillon, " +
				"place-le avec -devd / -ngld si la VRAM manque"
		}
		if _, ok := ctxFixed(cfg, extra); !ok {
			note += " ; contexte non chiffré : fit pourrait le réduire pour loger le brouillon"
		}
		if si.GGUF != nil && strings.EqualFold(si.GGUF.Arch, "qwen4exp") {
			note += " ; MTP qwen4exp très récent"
		}
		notes = append(notes, note)
		if mode == "mtp+ngram" {
			notes = append(notes, ngramCostNotes(cfg, extra, si)...)
		}
	}

	// Nombre de jetons anticipés : défaut du moteur (3) si la clé est vide.
	if v := strings.TrimSpace(cfg["SPEC_N_MAX"]); v != "" {
		n, err := strconv.Atoi(v)
		switch {
		case err != nil || n < 1 || n > specNMaxLimit:
			notes = append(notes, "SPEC_N_MAX="+v+" illisible (1 à 64 jetons) : défaut du moteur")
		case !strings.Contains(si.Help, "--spec-draft-n-max"):
			notes = append(notes, "ce moteur ne connaît pas --spec-draft-n-max : SPEC_N_MAX ignoré")
		case hasAnyFlag(extra, "--spec-draft-n-max") || si.ArgEnv["LLAMA_ARG_SPEC_DRAFT_N_MAX"] != "":
		default:
			args = append(args, "--spec-draft-n-max", strconv.Itoa(n))
		}
	}

	// Tirage du brouillon. greedy est exact quelle que soit la chaîne
	// d'échantillonnage : on le fixe dès que le moteur connaît le drapeau, pour
	// qu'un changement de défaut en amont ne change rien ici.
	sampling := "greedy"
	switch v := strings.ToLower(strings.TrimSpace(cfg["SPEC_SAMPLING"])); v {
	case "", "greedy":
	case "probabilistic":
		switch {
		case mode == "auto":
			notes = append(notes, "SPEC_SAMPLING=probabilistic ignoré avec SPEC=auto (SPEC=mtp pour le choisir) : greedy")
		case statefulSampler(extra) != "":
			notes = append(notes, "SPEC_SAMPLING=probabilistic refusé avec "+statefulSampler(extra)+
				" (cet échantillonneur avancerait sur des jetons rejetés) : greedy")
		case !strings.Contains(si.Help, "probabilistic"):
			notes = append(notes, "ce moteur ne connaît pas --spec-draft-sampling probabilistic : greedy")
		default:
			sampling = v
		}
	default:
		notes = append(notes, "SPEC_SAMPLING="+v+" illisible (greedy ou probabilistic) : greedy")
	}
	if strings.Contains(si.Help, "--spec-draft-sampling") && !hasAnyFlag(extra, "--spec-draft-sampling") &&
		si.ArgEnv["LLAMA_ARG_SPEC_DRAFT_SAMPLING"] == "" {
		args = append(args, "--spec-draft-sampling", sampling)
	}
	return args, notes, auto
}

// specSource : d'où vient le brouillon de SPEC=auto|mtp|mtp+ngram. MODEL_DRAFT
// s'il est posé, sinon la tête MTP du modèle ; dans les deux cas on regarde les
// TENSEURS, comme llama.cpp : une clé nextn_predict_layers seule ne prouve pas
// que la tête est dans le fichier. why non vide = aucun brouillon ; quiet =
// rien à dire (auto sur un modèle sans MTP).
func specSource(mode, draftKey string, si serveSysInfo) (args []string, mtp bool, why string, quiet bool) {
	switch {
	case draftKey != "":
		switch {
		case si.Draft == "":
			why := "MODEL_DRAFT=" + draftKey + " introuvable"
			if si.DraftErr != "" {
				why += " (" + si.DraftErr + ")"
			}
			return nil, false, why, false
		case si.DraftGGUF == nil:
			return nil, false, "MODEL_DRAFT illisible (GGUF incomplet ou en cours de téléchargement ?)", false
		case si.DraftGGUF.HasNextNTensor:
			if !helpSupportsMTP(si.Help) {
				return nil, false, "ce moteur ne connaît pas draft-mtp", false
			}
			// Type explicite : llama.cpp ne le devine que sur la première tranche.
			return []string{"-md", si.Draft, "--spec-type", "draft-mtp"}, true, "", false
		case specSidecarArch(si.DraftGGUF.Arch):
			return nil, false, "MODEL_DRAFT est une tête " + si.DraftGGUF.Arch +
				", pas un modèle brouillon : règle -md et --spec-type dans EXTRA_ARGS", false
		case mode == "mtp+ngram":
			return nil, false, "", false // un modèle brouillon n'est pas une tête MTP
		default:
			if !strings.Contains(si.Help, "--spec-type") || !strings.Contains(si.Help, "draft-simple") {
				return nil, false, "ce moteur ne connaît pas --spec-type draft-simple", false
			}
			// Sans type, un brouillon qui n'est pas une tête MTP serait chargé en
			// VRAM puis jamais utilisé.
			return []string{"-md", si.Draft, "--spec-type", "draft-simple"}, false, "", false
		}
	case si.GGUF == nil:
		return nil, false, "métadonnées du modèle illisibles", false
	case !si.GGUF.HasNextNTensor:
		if mode != "auto" || si.GGUF.NextN > 0 {
			return nil, false, "pas de tête MTP dans ce fichier (publiée à part ? MODEL_DRAFT=mtp-….gguf)", false
		}
		return nil, false, "", true
	case !helpSupportsMTP(si.Help):
		return nil, false, "ce moteur ne connaît pas draft-mtp", false
	}
	return []string{"--spec-type", "draft-mtp"}, true, "", false
}

// ngramModParams : les trois bornes de ngram-mod, toujours explicites.
func ngramModParams() []string {
	return []string{
		"--spec-ngram-mod-n-match", strconv.Itoa(specNgramNMatch),
		"--spec-ngram-mod-n-min", strconv.Itoa(specNgramNMin),
		"--spec-ngram-mod-n-max", strconv.Itoa(specNgramNMax),
	}
}

// ngramBlocker dit pourquoi SPEC=ngram|mtp+ngram ne s'active pas, ou vide.
//
//   - Le moteur doit connaître --spec-ngram-mod-n-match, pas seulement le mot
//     « ngram-mod » : les moteurs d'avant le renommage ont encore le type dans
//     leur liste mais refusent les drapeaux actuels, et meurent au démarrage.
//   - SPEC=ngram n'a pas de brouillon : un --spec-draft-* d'EXTRA_ARGS veut dire
//     qu'on règle un brouillon à la main, on ne mélange pas.
//   - Poids en RAM (experts MoE sur CPU…) : llama.cpp recopie vers le GPU les
//     poids d'un lot dès GGML_OP_OFFLOAD_MIN_BATCH jetons (32 par défaut). Un
//     lot de vérification de n_max+1 jetons au-dessus du seuil ferait passer
//     les experts de chaque couche par le PCIe — voire par le disque si le
//     modèle mappé dépasse la RAM — à chaque brouillon. Refusé tant que le seuil
//     n'est pas relevé au-dessus du lot.
func ngramBlocker(cfg map[string]string, extra []string, si serveSysInfo, mode string) string {
	if !strings.Contains(si.Help, "--spec-ngram-mod-n-match") {
		return "ce moteur ne connaît pas --spec-ngram-mod-n-match"
	}
	if mode == "ngram" {
		if f := hasFlagPrefix(extra, "--spec-draft-", "--spec_draft_"); f != "" {
			return f + " dans EXTRA_ARGS (brouillon réglé à la main)"
		}
	}
	blocks := 0
	if si.GGUF != nil {
		blocks = si.GGUF.BlockCount
	}
	if r := cpuWeights(cfg, extra, si.ArgEnv, blocks); r != "" {
		if thr := opOffloadThreshold(cfg, si); specNgramNMax+1 >= thr {
			return fmt.Sprintf("%s : un lot de vérification de %d jetons atteint GGML_OP_OFFLOAD_MIN_BATCH (%d) et "+
				"recopierait ces poids vers le GPU à chaque brouillon — OP_OFFLOAD_MIN_BATCH=128 pour l'essayer, à mesurer",
				r, specNgramNMax+1, thr)
		}
	}
	return ""
}

// opOffloadThreshold : le seuil GGML_OP_OFFLOAD_MIN_BATCH que verra le moteur —
// celui de l'environnement, sinon celui d'OP_OFFLOAD_MIN_BATCH, sinon le défaut.
func opOffloadThreshold(cfg map[string]string, si serveSysInfo) int {
	v := strings.TrimSpace(si.UserEnv["GGML_OP_OFFLOAD_MIN_BATCH"])
	if v == "" {
		v, _ = opOffloadMinBatchEnv(cfg, si)
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return ggmlOffloadMinBatchDefault
}

// ngramSpecArgs : SPEC=ngram, ou le repli de mtp+ngram sans tête MTP (lead dit
// pourquoi). Pas de --spec-draft-* : il n'y a pas de brouillon.
func ngramSpecArgs(cfg map[string]string, extra []string, si serveSysInfo, lead string) (args, notes []string) {
	args = append([]string{"--spec-type", "ngram-mod"}, ngramModParams()...)
	notes = append(notes, lead+fmt.Sprintf("SPEC=ngram → n-grammes du contexte (ngram-mod, brouillons de %d à %d jetons) : "+
		"distribution inchangée (chaque jeton vérifié), ~16 Mo de RAM ; utile surtout en mode code, à mesurer "+
		"(acceptation par nature dans /api/perf/summary)", specNgramNMin, specNgramNMax))
	if lead == "" {
		// --spec-type explicite : llama.cpp ne choisit plus de lui-même la tête MTP.
		switch {
		case strings.TrimSpace(cfg["MODEL_DRAFT"]) != "":
			notes = append(notes, "MODEL_DRAFT ignoré avec SPEC=ngram : SPEC=mtp+ngram pour les deux")
		case si.GGUF != nil && si.GGUF.HasNextNTensor:
			notes = append(notes, "tête MTP du modèle inutilisée avec SPEC=ngram : SPEC=mtp+ngram pour les deux")
		}
	}
	if strings.TrimSpace(cfg["SPEC_N_MAX"]) != "" || strings.TrimSpace(cfg["SPEC_SAMPLING"]) != "" {
		notes = append(notes, "SPEC_N_MAX et SPEC_SAMPLING ignorés : les n-grammes n'ont pas de brouillon à régler")
	}
	return args, append(notes, ngramCostNotes(cfg, extra, si)...)
}

// ngramCostNotes : ce que les longs brouillons coûtent, là où ça se voit.
func ngramCostNotes(cfg map[string]string, extra []string, si serveSysInfo) []string {
	var notes []string
	if si.GGUF != nil && si.GGUF.Hybrid {
		notes = append(notes, "modèle hybride : chaque brouillon n-gramme copie l'état récurrent (point de reprise en RAM "+
			"hôte) et le recharge s'il est rejeté — compare le temps par tour, pas seulement le débit")
	}
	blocks := 0
	if si.GGUF != nil {
		blocks = si.GGUF.BlockCount
	}
	if cpuWeights(cfg, extra, si.ArgEnv, blocks) != "" && si.ModelBytes > 0 && si.RAMMiB > 0 &&
		si.ModelBytes > si.RAMMiB<<20 {
		notes = append(notes, "modèle plus gros que la RAM, poids sur CPU : un lot de vérification lit la plupart des "+
			"experts de chaque couche — surveille les lectures disque, pas seulement les jetons/s")
	}
	if nglForced(cfg, extra) {
		notes = append(notes, fmt.Sprintf("NGL imposé (--fit inactif) : les logits de %d positions par slot prennent "+
			"quelques dizaines de Mio de VRAM en plus", specNgramNMax+1))
	}
	return notes
}

// --- Jeton de tentative ------------------------------------------------------
//
// Un essai automatique qui fait tomber le moteur ne doit pas se rejouer : sous
// systemd il redémarrerait en boucle en rechargeant 15 Go à chaque fois. cmdServe
// ne peut pas constater l'échec lui-même (sous Unix il DEVIENT llama-server).
// Il pose donc un jeton avant de lancer ; le process web l'efface dès que le
// moteur répond. Au lancement suivant, un jeton encore là pour la même
// configuration, le même moteur et le même build veut dire « jamais répondu » :
// l'auto est coupé pour cette combinaison, et on le dit. Une mise à jour du
// moteur ou un preset modifié retente. Sans process web (CLI seule), le jeton
// n'est jamais effacé : on retombe sur off, le côté sûr.

const (
	specAttemptKey = "spec_auto_attempt"
	specFailedKey  = "spec_auto_failed"
	specFailedMax  = 32
)

// specAutoMark identifie une combinaison preset × moteur.
type specAutoMark struct {
	FP    string `json:"fp"`
	Bin   string `json:"bin"`
	Build int    `json:"build"`
}

func (m specAutoMark) key() string { return fmt.Sprintf("%s|%s|%d", m.FP, m.Bin, m.Build) }

// specAutoVerdict : pure. Renvoie la raison de couper l'auto (vide = permis)
// et s'il faut inscrire cur parmi les échecs.
func specAutoVerdict(cur specAutoMark, attempt *specAutoMark, failed map[string]string) (why string, record bool) {
	if w := failed[cur.key()]; w != "" {
		return w, false
	}
	if attempt != nil && attempt.key() == cur.key() {
		return "le dernier lancement avec cette configuration n'a jamais répondu", true
	}
	return "", false
}

// specAutoPeek lit l'état sans rien changer : la raison de couper l'auto (vide
// = permis) et s'il faudra inscrire l'échec. Rien n'est consommé avant que le
// port soit libre (specAutoSettle) : un second « loki serve », refusé parce que
// le premier charge encore, ne doit pas prendre le jeton de celui-ci pour un
// échec.
func specAutoPeek(cur specAutoMark) (why string, record bool) {
	_ = view(bkState, func(b *bolt.Bucket) error {
		attempt, failed := readSpecAuto(b)
		why, record = specAutoVerdict(cur, attempt, failed)
		return nil
	})
	return why, record
}

// specAutoSettle range, port libre, juste avant de lancer le moteur : l'ancien
// jeton est consommé, l'échec inscrit s'il y a lieu (record, why), et un
// nouveau jeton posé si ce lancement ajoute des drapeaux automatiques.
func specAutoSettle(cur specAutoMark, why string, record, attempt bool) {
	_ = update(bkState, func(b *bolt.Bucket) error {
		_, failed := readSpecAuto(b)
		_ = b.Delete([]byte(specAttemptKey))
		if record {
			if err := putFailed(b, failed, cur.key(), why); err != nil {
				return err
			}
		}
		if !attempt {
			return nil
		}
		raw, err := json.Marshal(cur)
		if err != nil {
			return err
		}
		return b.Put([]byte(specAttemptKey), raw)
	})
}

// readSpecAuto : le jeton en cours (nil = aucun) et les échecs inscrits.
func readSpecAuto(b *bolt.Bucket) (attempt *specAutoMark, failed map[string]string) {
	if raw := b.Get([]byte(specAttemptKey)); raw != nil {
		var a specAutoMark
		if json.Unmarshal(raw, &a) == nil {
			attempt = &a
		}
	}
	failed = map[string]string{}
	if raw := b.Get([]byte(specFailedKey)); raw != nil {
		_ = json.Unmarshal(raw, &failed)
	}
	return attempt, failed
}

func putFailed(b *bolt.Bucket, failed map[string]string, key, why string) error {
	if len(failed) >= specFailedMax {
		failed = map[string]string{}
	}
	failed[key] = why
	raw, err := json.Marshal(failed)
	if err != nil {
		return err
	}
	return b.Put([]byte(specFailedKey), raw)
}

// --- Côté process web --------------------------------------------------------

var (
	specWatchOnce sync.Once
	offloadedRe   = regexp.MustCompile(`offloaded (\d+)/(\d+) layers to GPU`)
)

// startSpecAttemptWatch : une goroutine qui efface le jeton dès que le moteur
// répond, sans attendre qu'une page interroge /api/status.
func startSpecAttemptWatch() {
	specWatchOnce.Do(func() {
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for range t.C {
				specAttemptTick()
			}
		}()
	})
}

func specAttemptTick() {
	var a specAutoMark
	if !getJSON(bkState, specAttemptKey, &a) {
		return
	}
	if externalActive() {
		specAttemptClear(a, "") // plus de moteur local à attendre
		return
	}
	if !healthCheck() {
		return
	}
	// Répondu. Mais avec -c fixé, fit ne réduit pas le contexte : s'il manque
	// la place du brouillon, il descend des couches en RAM — le moteur tourne,
	// plus lentement que sans MTP. Ça compte aussi comme un échec de l'auto.
	why := ""
	if n, m := lastOffload(serviceLogTail(600)); m > 0 && n < m {
		why = fmt.Sprintf("seulement %d/%d couches sur GPU avec le brouillon : fit a déplacé des couches en RAM", n, m)
		fmt.Println("[loki] SPEC=auto : " + why + " — coupé au prochain démarrage (SPEC=mtp pour l'imposer)")
	}
	specAttemptClear(a, why)
}

// specAttemptClear efface le jeton a — et lui seul : relu sous verrou, car un
// « loki serve » a pu entre-temps poser celui d'un AUTRE lancement, qui n'a pas
// encore répondu. why non vide inscrit l'échec de a.
func specAttemptClear(a specAutoMark, why string) {
	_ = update(bkState, func(b *bolt.Bucket) error {
		cur, failed := readSpecAuto(b)
		if cur == nil || cur.key() != a.key() {
			return nil
		}
		_ = b.Delete([]byte(specAttemptKey))
		if why == "" {
			return nil
		}
		return putFailed(b, failed, a.key(), why)
	})
}

// lastOffload lit les lignes « offloaded N/M layers to GPU » du DERNIER
// chargement du journal, repéré à la ligne du serveur « loading model
// '<chemin>' ». Pas à « load_model » (le serveur écrit encore « load_model:
// initializing… » APRÈS le chargement) ni à « loading model tensors » (écrit
// aussi pour le brouillon, qui masquerait le modèle). Un brouillon chargé à
// part a sa propre ligne : la pire des deux compte. 0, 0 = introuvable : on ne
// conclut rien.
func lastOffload(log string) (n, m int) {
	lines := strings.Split(log, "\n")
	start := -1
	for i, l := range lines {
		if strings.Contains(l, "loading model '") {
			start = i
		}
	}
	if start < 0 {
		return 0, 0
	}
	for _, l := range lines[start:] {
		s := offloadedRe.FindStringSubmatch(l)
		if s == nil {
			continue
		}
		a, _ := strconv.Atoi(s[1])
		b, _ := strconv.Atoi(s[2])
		if m == 0 || a < b {
			n, m = a, b
		}
		if a < b {
			break
		}
	}
	return n, m
}
