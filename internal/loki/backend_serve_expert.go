package loki

import (
	"os"
	"strconv"
	"strings"
)

// Réglages d'expert du moteur local, tous éteints tant que le preset ne les
// demande pas. Aucun ne touche au modèle : ni poids, ni contexte, ni
// échantillonnage — seulement OÙ et COMMENT le même calcul s'exécute.
//
//	FIT_TARGET=1024,3072      → --fit-target : marge VRAM laissée libre par carte
//	OP_OFFLOAD_MIN_BATCH=N    → GGML_OP_OFFLOAD_MIN_BATCH : seuil d'envoi au GPU
//	                            des poids restés en RAM
//	CUDA_GRAPH_OPT=on         → GGML_CUDA_GRAPH_OPT=1 : branches Q/K/V en parallèle
//
// Une variable déjà présente dans l'environnement (gabarit Docker/Unraid) est
// un choix de l'utilisateur : jamais touchée. Les variables passent par
// l'environnement du seul « loki serve », qui devient llama-server : le reste
// du conteneur ne les voit pas.

// expertEnvKeys : les variables du moteur que ces réglages posent.
var expertEnvKeys = []string{"GGML_CUDA_GRAPH_OPT", "GGML_OP_OFFLOAD_MIN_BATCH"}

// fitArgEnv : les LLAMA_ARG_* qui, sans drapeau, décident de --fit et de sa
// marge. llama.cpp les applique quand la ligne de commande se tait.
var fitArgEnv = []string{"LLAMA_ARG_FIT", "LLAMA_ARG_FIT_TARGET", "LLAMA_ARG_TENSOR_SPLIT"}

// probeExpertEnv relève ce que l'environnement a déjà tranché. Appelée après
// probeServeGPUs, dont elle complète ArgEnv.
func probeExpertEnv(si *serveSysInfo) {
	si.UserEnv = map[string]string{}
	for _, k := range expertEnvKeys {
		if v := os.Getenv(k); v != "" {
			si.UserEnv[k] = v
		}
	}
	if si.ArgEnv == nil {
		si.ArgEnv = map[string]string{}
	}
	for _, k := range fitArgEnv {
		if v := os.Getenv(k); v != "" {
			si.ArgEnv[k] = v
		}
	}
}

// fitTargetMin : la marge par défaut de llama.cpp (common.h), en Mio. En
// dessous, fit range plus serré que ce qu'il sait estimer — le tampon du modèle
// MTP, qu'il ne mesure pas toujours, suffit alors à ramener le « cudaMalloc
// failed: out of memory » au premier long prompt. On remonte, en le disant.
const fitTargetMin = 1024

// fitTargetMaxValues : llama.cpp refuse (et le serveur meurt en boucle) une
// liste aussi longue que son nombre maximal de cartes ; 8 reste loin du bord.
const fitTargetMaxValues = 8

// parseFitTarget valide FIT_TARGET : des entiers en Mio séparés par « , » ou
// « / », une valeur par carte dans l'ordre que voit le moteur (une seule vaut
// pour toutes). Jamais transmis brut : llama.cpp lit chaque valeur avec
// std::stoull, qui lève sur le moindre caractère parasite — moteur mort au
// démarrage, relancé en boucle par systemd ou Docker.
func parseFitTarget(v string) (vals []int, raised bool, ok bool) {
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '/' })
	if len(parts) == 0 || len(parts) > fitTargetMaxValues ||
		strings.Count(v, ",")+strings.Count(v, "/") != len(parts)-1 {
		return nil, false, false
	}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || strings.Trim(p, "0123456789") != "" || len(p) > 7 {
			return nil, false, false
		}
		n, _ := strconv.Atoi(p)
		if n < fitTargetMin {
			n, raised = fitTargetMin, true
		}
		vals = append(vals, n)
	}
	return vals, raised, true
}

// fitTargetArgs traduit FIT_TARGET en --fit-target. Fonction pure.
//
// La marge règle combien de VRAM --fit laisse libre sur chaque carte ; sur deux
// cartes inégales (5060 Ti + 3060), une marge plus large sur la lente pousse
// plus de couches vers la rapide. Placement seulement : mêmes poids, mêmes
// types de cache, même contexte. Rien n'est posé quand :
//
//   - le moteur ne connaît pas --fit-target (un drapeau inconnu le tue) ;
//   - EXTRA_ARGS ou LLAMA_ARG_FIT_TARGET l'ont déjà fixée ;
//   - le contexte n'est pas un nombre positif : avec -c 0, fit a le droit de
//     RÉDUIRE le contexte pour tenir, et une marge plus large l'y pousserait
//     sans un mot — de l'information perdue pour le modèle ;
//   - --fit ne tournera pas (voir fitBlocker) : la clé ne ferait rien, et une
//     comparaison A/B mesurerait deux fois le même placement.
func fitTargetArgs(cfg map[string]string, extra []string, si serveSysInfo) (args, notes []string) {
	raw := strings.TrimSpace(cfg["FIT_TARGET"])
	if raw == "" {
		return nil, nil
	}
	ignore := func(why string) (a, n []string) {
		return nil, []string{"FIT_TARGET=" + raw + " ignoré : " + why}
	}
	vals, raised, ok := parseFitTarget(raw)
	switch {
	case !ok:
		return ignore("des Mio par carte, ex. 1024,3072 (8 valeurs au plus)")
	case !strings.Contains(si.Help, "--fit-target"):
		return ignore("ce moteur ne connaît pas --fit-target")
	case hasAnyFlag(extra, "-fitt", "--fit-target"):
		return ignore("--fit-target déjà dans EXTRA_ARGS")
	case si.ArgEnv["LLAMA_ARG_FIT_TARGET"] != "":
		return ignore("LLAMA_ARG_FIT_TARGET déjà posé dans l'environnement")
	}
	ctx := flagValue(extra, "-c", "--ctx-size")
	if ctx == "" {
		ctx = strings.TrimSpace(cfg["CTX"])
		if ctx == "" {
			ctx = "32768"
		}
	}
	if n, err := strconv.Atoi(ctx); err != nil || n <= 0 {
		return ignore("contexte « " + ctx + " » non fixé — --fit pourrait le réduire pour tenir ; donne un CTX chiffré")
	}
	if why := fitBlocker(cfg, extra, si); why != "" {
		return ignore("--fit inactif (" + why + "), la marge ne servirait à rien")
	}
	s := make([]string, len(vals))
	for i, n := range vals {
		s[i] = strconv.Itoa(n)
	}
	v := strings.Join(s, ",")
	note := "--fit-target " + v + " (FIT_TARGET, Mio libres par carte, dans l'ordre vu par le moteur) — " +
		"vérifie « offloaded N/N layers » dans le journal"
	if raised {
		note += " ; marges sous 1024 Mio remontées à 1024"
	}
	return []string{"--fit-target", v}, []string{note}
}

// fitBlocker dit pourquoi llama.cpp ne lancera PAS --fit (common/fit.cpp :
// « already set by user, abort »), ou vide s'il le lancera. Les couches GPU
// sont lues comme buildServeArgs les écrira : EXTRA_ARGS d'abord, sinon la
// traduction de NGL par nglArgs (999 y devient auto), sinon — NGL=auto, aucun
// drapeau — la variable du moteur.
func fitBlocker(cfg map[string]string, extra []string, si serveSysInfo) string {
	fit := flagValue(extra, "-fit", "--fit")
	if fit == "" {
		fit = si.ArgEnv["LLAMA_ARG_FIT"]
	}
	switch strings.ToLower(strings.TrimSpace(fit)) {
	case "off", "disabled", "false", "0":
		return "--fit off"
	}
	ngl := flagValue(extra, "-ngl", "--n-gpu-layers", "--gpu-layers")
	if ngl == "" {
		if a, _ := nglArgs(cfg["NGL"], helpFitsLayersItself(si.Help)); len(a) == 2 {
			ngl = a[1]
		} else {
			ngl = si.ArgEnv["LLAMA_ARG_N_GPU_LAYERS"]
		}
	}
	if ngl = strings.TrimSpace(ngl); ngl != "" && !strings.EqualFold(ngl, "auto") {
		return "couches GPU fixées à " + ngl
	}
	if hasAnyFlag(extra, "-ts", "--tensor-split") || si.ArgEnv["LLAMA_ARG_TENSOR_SPLIT"] != "" {
		return "--tensor-split"
	}
	if r := tensorOverride(extra, si.ArgEnv); r != "" {
		return r
	}
	sm := flagValue(extra, "-sm", "--split-mode")
	if sm == "" {
		sm = si.ArgEnv["LLAMA_ARG_SPLIT_MODE"]
	}
	if strings.EqualFold(strings.TrimSpace(sm), "row") {
		return "-sm row"
	}
	return ""
}

// opOffloadMinBatchEnv traduit OP_OFFLOAD_MIN_BATCH en GGML_OP_OFFLOAD_MIN_BATCH.
// Fonction pure.
//
// Quand des poids restent en RAM (experts MoE sur CPU), llama.cpp recopie vers
// le GPU ceux dont un lot a besoin dès qu'il compte au moins 32 jetons ; en
// dessous, il calcule sur CPU. Monter le seuil garde sur CPU les lots plus
// courts : utile seulement si une spéculation ngram propose des brouillons de
// 32 jetons ou plus (les lots de vérification MTP, quelques jetons, restent
// déjà sous 32). Le prix : un tour d'outil de 32 à N-1 jetons se calcule lui
// aussi sur CPU. N se trouve à la mesure, pas d'office. Même distribution ;
// les logits ne sont pas identiques au bit près, comme déjà entre décodage CPU
// et prefill GPU.
func opOffloadMinBatchEnv(cfg map[string]string, si serveSysInfo) (val string, notes []string) {
	raw := strings.TrimSpace(cfg["OP_OFFLOAD_MIN_BATCH"])
	if raw == "" || si.UserEnv["GGML_OP_OFFLOAD_MIN_BATCH"] != "" {
		return "", nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 1<<20 {
		return "", []string{"OP_OFFLOAD_MIN_BATCH=" + raw + " ignoré : un nombre de jetons, ex. 128"}
	}
	v := strconv.Itoa(n)
	return v, []string{"GGML_OP_OFFLOAD_MIN_BATCH=" + v + " (OP_OFFLOAD_MIN_BATCH) : lots de moins de " + v +
		" jetons calculés sur CPU pour les poids restés en RAM"}
}

// graphOptEnv traduit CUDA_GRAPH_OPT=on en GGML_CUDA_GRAPH_OPT=1. Fonction pure.
//
// Au décodage, llama.cpp lance alors les trois branches Q, K et V de chaque
// couche sur des flux CUDA concurrents. Chaque opération garde son noyau et ses
// entrées : sortie identique au jeton près en amont (PR #28198, perplexité
// inchangée). Gain modeste — quelques % sur un MoE tout GPU, rien sur un dense,
// dilué quand les experts tournent sur CPU ou que MTP vérifie par lots. Sans
// effet avant #28198 en multi-GPU ou si les graphes CUDA sont coupés.
//
// Expérimental : sur une architecture qu'il n'a pas vue, le repérage des
// branches peut s'arrêter sur un GGML_ASSERT, et le moteur redémarre en boucle.
// Une variable ne se détecte pas dans l'aide du moteur : on le dit au
// lancement, pour que « CUDA_GRAPH_OPT=off puis redémarrer » vienne à l'esprit.
//
// Refusé quand Q, K et V d'une même couche pourraient finir sur deux cartes
// (-sm row/tensor, -ot visant l'attention) : #28198 en fait une condition
// d'exactitude.
func graphOptEnv(cfg map[string]string, extra []string, si serveSysInfo) (val string, notes []string) {
	raw := strings.TrimSpace(cfg["CUDA_GRAPH_OPT"])
	switch strings.ToLower(raw) {
	case "", "off":
		return "", nil
	case "on":
	default:
		return "", []string{"CUDA_GRAPH_OPT=" + raw + " ignoré (on ou off)"}
	}
	if si.UserEnv["GGML_CUDA_GRAPH_OPT"] != "" {
		return "", nil
	}
	if why := qkvSplitRisk(extra, si.ArgEnv); why != "" {
		return "", []string{"CUDA_GRAPH_OPT ignoré : " + why + " peut séparer Q, K et V d'une couche entre cartes"}
	}
	return "1", []string{"GGML_CUDA_GRAPH_OPT=1 (CUDA_GRAPH_OPT, expérimental) — si le moteur s'arrête sur un " +
		"GGML_ASSERT, CUDA_GRAPH_OPT=off puis redémarre"}
}

// qkvSplitRisk : un placement qui pourrait répartir les tenseurs d'attention
// d'une même couche sur plusieurs appareils. La découpe par couches (défaut,
// --tensor-split compris) garde chaque couche entière : rien à craindre.
func qkvSplitRisk(extra []string, argEnv map[string]string) string {
	sm := flagValue(extra, "-sm", "--split-mode")
	if sm == "" {
		sm = argEnv["LLAMA_ARG_SPLIT_MODE"]
	}
	if sm = strings.ToLower(strings.TrimSpace(sm)); sm == "row" || sm == "tensor" {
		return "-sm " + sm
	}
	for _, v := range append(flagValues(extra, "-ot", "--override-tensor"), argEnv["LLAMA_ARG_OVERRIDE_TENSOR"]) {
		if strings.Contains(strings.ToLower(v), "attn") {
			return "-ot sur l'attention"
		}
	}
	return ""
}
