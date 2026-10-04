package loki

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Points de reprise des modèles hybrides (--ctx-checkpoints,
// --checkpoint-min-step) et conservation du raisonnement (--reasoning-preserve).
//
// Un modèle hybride (couches récurrentes : Qwen3.5/3.6, Qwen3-Next…) ne sait pas
// rembobiner son état récurrent comme un cache KV : llama-server en garde des
// copies exactes, les points de reprise, et repart du plus proche quand la
// conversation diverge ou revient du cache de prompts. Avant b10864 (PR #28302),
// le moteur effaçait un point trop proche du précédent (moins de 8192 jetons)
// MÊME quand la liste avait de la place : la reprise retombait jusqu'à ~8 k
// jetons en arrière, recalculés à chaque tour d'outil. Rien de ce que voit le
// modèle ne change ici — un point de reprise est une copie octet pour octet de
// l'état — seul le temps de prefill, et la RAM hôte que ces copies occupent.
//
// Loki ne touche à rien sans savoir : le build n'est cru que pour un moteur
// officiel (voir engineBuildTrusted), le drapeau n'est posé que si l'aide le
// connaît, et l'utilisateur garde le dernier mot (clés du preset, EXTRA_ARGS,
// LLAMA_ARG_*). Tout ce qui décide est pur ; cmdServe fait les lectures.

const (
	// engineMinRecommended : premier build où l'espacement minimal n'évince un
	// point de reprise que lorsque la liste est pleine (PR #28302, 5d806aa).
	engineMinRecommended = 10864
	// engineBuildFloor : en dessous, le numéro ne prouve rien. Un llama.cpp
	// compilé depuis un clone superficiel (--depth=1) annonce « build 1 », un
	// fork numérote à sa façon : traiter ça comme un moteur ancien poserait un
	// avis faux et un drapeau inutile sur un moteur récent.
	engineBuildFloor = 5000
	// engineCkptDefault : n_ctx_checkpoints par défaut de llama.cpp (common.h).
	engineCkptDefault = 32
	// ckptAutoMinStep : espacement posé d'office sur un moteur ancien. Plus serré
	// que les 8192 du moteur, il garde des points qu'il aurait jetés ; jamais 0,
	// qui laisserait l'éviction FIFO jeter le point du début de la conversation
	// dans une longue boucle d'outils.
	ckptAutoMinStep = 2048
	// ckptFallbackMiB : poids d'un point de reprise quand le GGUF ne permet pas de
	// le calculer (70 à 200 Mio mesurés sur les hybrides Qwen) — on prend le haut.
	ckptFallbackMiB = 200
)

// ckptArgEnv : les variables qui règlent la même chose sans drapeau. llama.cpp
// les lit avant la ligne de commande : un drapeau de Loki les écraserait.
var ckptArgEnv = []string{"LLAMA_ARG_CTX_CHECKPOINTS", "LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT",
	"LLAMA_ARG_REASONING_PRESERVE", "LLAMA_ARG_CHAT_TEMPLATE_KWARGS"}

// probeCkptEnv complète ArgEnv avec ckptArgEnv.
func probeCkptEnv(si *serveSysInfo) {
	if si.ArgEnv == nil {
		si.ArgEnv = map[string]string{}
	}
	for _, k := range ckptArgEnv {
		if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
			si.ArgEnv[k] = v
		}
	}
}

// hybridArchs : architectures hybrides connues, au cas où le GGUF ne porterait
// pas de clé ssm.* lisible.
var hybridArchs = map[string]bool{"qwen35": true, "qwen35moe": true, "qwen3next": true, "qwen4exp": true}

// ggufHybrid : le modèle a-t-il des couches récurrentes ? Jamais deviné d'après
// le nom du fichier : sans métadonnées, on répond non.
func ggufHybrid(g *GGUFInfo) bool {
	return g != nil && (g.Hybrid || hybridArchs[strings.ToLower(g.Arch)])
}

// engineSource classe un moteur comme le panneau Moteur : celui de l'image,
// un moteur téléchargé par Loki depuis l'image officielle, le précompilé des
// releases officielles, ou tout le reste (« custom » : compilé, fork, chemin
// à la main).
func engineSource(bin string) string {
	imageBin := providedEngineBin()
	switch {
	case imageBin != "" && samePath(bin, imageBin):
		return "image"
	case engineOwns(bin):
		return "downloaded"
	case prebuiltOwns(bin):
		return "prebuilt"
	}
	return "custom"
}

// engineBuildTrust : le build n'est cru que pour un moteur publié par ggml-org,
// dont la CI numérote sur l'historique complet, et s'il est vraisemblable. 0 =
// inconnu : ni avis, ni drapeau automatique.
func engineBuildTrust(source string, build int) int {
	if source == "custom" || build < engineBuildFloor {
		return 0
	}
	return build
}

// engineBuildTrusted : build du moteur, ou 0 s'il n'est pas digne de confiance.
// Le « --version » est gardé par binaire, comme l'aide (binHelp), et la clé
// porte la taille et la date du fichier : un moteur recompilé au même chemin
// est relu.
func engineBuildTrusted(bin string) int {
	src := engineSource(bin)
	if src == "custom" {
		return 0 // pas la peine de lancer le binaire
	}
	return engineBuildTrust(src, engineBuildCached(bin))
}

var (
	buildMu    sync.Mutex
	buildCache = map[string]int{}
)

func engineBuildCached(bin string) int {
	st, err := os.Stat(bin)
	if err != nil {
		return 0
	}
	key := fmt.Sprintf("%s|%d|%d", bin, st.Size(), st.ModTime().UnixNano())
	buildMu.Lock()
	defer buildMu.Unlock()
	if b, ok := buildCache[key]; ok {
		return b
	}
	b, _ := engineBuildOf(bin)
	buildCache[key] = b
	return b
}

// engineBuildNotice : l'avis du panneau Moteur et du journal de lancement pour
// un build trop ancien. Vide = rien à dire (à jour, ou build inconnu).
func engineBuildNotice(build int) string {
	if build <= 0 || build >= engineMinRecommended {
		return ""
	}
	return fmt.Sprintf("moteur b%d : les points de reprise des modèles hybrides (Qwen3.5/3.6, Qwen3-Next…) y sont "+
		"évincés trop tôt — jusqu'à ~8 k jetons recalculés à chaque reprise. Corrigé à partir de b%d.",
		build, engineMinRecommended)
}

// ckptCount : nombre de points de reprise par slot que le moteur gardera
// (CTX_CHECKPOINTS valide, sinon EXTRA_ARGS, sinon la variable, sinon le
// défaut). -1 = illisible ; un CTX_CHECKPOINTS illisible est ignoré (voir
// ckptArgs), le moteur garde alors son défaut.
func ckptCount(cfg map[string]string, extra []string, argEnv map[string]string) int {
	v := flagValue(extra, "-ctxcp", "--ctx-checkpoints", "--swa-checkpoints")
	if v == "" {
		v = argEnv["LLAMA_ARG_CTX_CHECKPOINTS"]
	}
	if v == "" {
		if n, err := strconv.Atoi(strings.TrimSpace(cfg["CTX_CHECKPOINTS"])); err == nil && n >= 0 {
			return n
		}
		return engineCkptDefault
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// ckptSlots : nombre de slots que le moteur ouvrira, chacun avec sa propre liste
// de points de reprise. --parallel d'EXTRA_ARGS, sinon PARALLEL, sinon 1 (Loki
// pose toujours --parallel, voir buildServeArgs). Illisible ou « auto » (-1) :
// 4, ce que choisissent les moteurs récents — on compte large.
func ckptSlots(cfg map[string]string, extra []string) int {
	v := flagValue(extra, "-np", "--parallel")
	if v == "" {
		v = strings.TrimSpace(cfg["PARALLEL"])
	}
	if v == "" {
		return 1
	}
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
		return n
	}
	return 4
}

// ckptMiB : poids d'un point de reprise d'un hybride (état récurrent en f32).
func ckptMiB(g *GGUFInfo) int64 {
	if g != nil {
		if _, rec := ggufStateBytes(*g, "", ""); rec > 0 {
			return (rec + (1 << 20) - 1) >> 20
		}
	}
	return ckptFallbackMiB
}

// ramTight : le modèle occupe plus de 70 % de la RAM (mmap d'un gros MoE dont
// les experts vivent dans le cache de pages). Chaque Mio de points de reprise
// y est pris aux poids : plus de lectures disque, décodage plus lent.
func ramTight(si serveSysInfo) bool {
	return si.RAMMiB > 0 && si.ModelBytes > 0 && float64(si.ModelBytes) > 0.7*float64(si.RAMMiB)*(1<<20)
}

// ckptArgs : --ctx-checkpoints et --checkpoint-min-step que Loki ajoute, et ce
// qu'il en dit.
//
//	CTX_CHECKPOINTS=<n>  → --ctx-checkpoints n   (opt-in ; jamais de défaut relevé)
//	CKPT_MIN_STEP=<n>    → --checkpoint-min-step n (n > 0)
//	clé absente          → --checkpoint-min-step 2048 seulement sur un hybride,
//	                       moteur officiel < b10864, RAM non serrée
//
// Chaque drapeau a sa propre garde : un --ctx-checkpoints écrit à la main ne
// coupe pas l'atténuation de l'espacement, qui est une autre question.
func ckptArgs(cfg map[string]string, extra []string, si serveSysInfo) (args, notes []string) {
	hybrid := ggufHybrid(si.GGUF)

	if v := strings.TrimSpace(cfg["CTX_CHECKPOINTS"]); v != "" {
		n, err := strconv.Atoi(v)
		switch {
		case err != nil || n < 0:
			notes = append(notes, "CTX_CHECKPOINTS="+v+" illisible (un nombre de points de reprise par slot, ≥ 0) : ignoré")
		case !strings.Contains(si.Help, "--ctx-checkpoints"):
			notes = append(notes, "ce moteur ne connaît pas --ctx-checkpoints : CTX_CHECKPOINTS ignoré")
		case hasAnyFlag(extra, "-ctxcp", "--ctx-checkpoints", "--swa-checkpoints"):
		case si.ArgEnv["LLAMA_ARG_CTX_CHECKPOINTS"] != "":
			notes = append(notes, "LLAMA_ARG_CTX_CHECKPOINTS est posée : CTX_CHECKPOINTS ignoré")
		default:
			args = append(args, "--ctx-checkpoints", strconv.Itoa(n))
		}
	} else if hybrid && ramTight(si) && strings.Contains(si.Help, "--ctx-checkpoints") &&
		!hasAnyFlag(extra, "-ctxcp", "--ctx-checkpoints", "--swa-checkpoints") && si.ArgEnv["LLAMA_ARG_CTX_CHECKPOINTS"] == "" {
		// Un conseil, pas un réglage : moins de points de reprise, c'est plus de
		// prefill, et seul l'utilisateur peut mesurer lequel des deux coûte le plus.
		notes = append(notes, fmt.Sprintf("modèle hybride de %d Gio pour %d Gio de RAM : jusqu'à %d points de reprise "+
			"(~%d Mio chacun) prennent sur le cache de pages des poids. CTX_CHECKPOINTS=8 à 16 si le débit baisse.",
			si.ModelBytes>>30, si.RAMMiB>>10, engineCkptDefault, ckptMiB(si.GGUF)))
	}

	stepUser := hasAnyFlag(extra, "-cms", "--checkpoint-min-step") || si.ArgEnv["LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT"] != ""
	if v := strings.TrimSpace(cfg["CKPT_MIN_STEP"]); v != "" {
		n, err := strconv.Atoi(v)
		switch {
		case err != nil || n <= 0:
			// 0 = « aucun minimum » pour le moteur : la liste se remplit de points
			// serrés et l'éviction FIFO finit par jeter celui du début de la
			// conversation. On ne le pose pas, sans se taire pour autant.
			notes = append(notes, "CKPT_MIN_STEP="+v+" refusé (un nombre de jetons > 0 ; 0 laisserait le moteur jeter "+
				"le point de reprise du début de la conversation) : défaut du moteur")
		case !strings.Contains(si.Help, "--checkpoint-min-step"):
			notes = append(notes, "ce moteur ne connaît pas --checkpoint-min-step : CKPT_MIN_STEP ignoré")
		case hasAnyFlag(extra, "-cms", "--checkpoint-min-step"):
		case si.ArgEnv["LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT"] != "":
			notes = append(notes, "LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT est posée : CKPT_MIN_STEP ignoré")
		default:
			args = append(args, "--checkpoint-min-step", strconv.Itoa(n))
		}
		return args, notes
	}

	notice := engineBuildNotice(si.EngineBuild)
	if !hybrid || notice == "" {
		return args, notes
	}
	n := ckptCount(cfg, extra, si.ArgEnv)
	switch {
	case stepUser || !strings.Contains(si.Help, "--checkpoint-min-step"):
		return args, append(notes, notice)
	case n <= 0:
		return args, append(notes, notice) // points de reprise coupés : l'espacement ne sert à rien
	case si.RAMMiB <= 0 || si.ModelBytes <= 0 || ramTight(si):
		return args, append(notes, notice+" Pas d'espacement resserré d'office : RAM inconnue ou déjà prise par le modèle "+
			"(CKPT_MIN_STEP=2048 pour l'imposer).")
	}
	// La liste est PAR SLOT : avec PARALLEL=4, quatre listes se remplissent.
	total := n * ckptSlots(cfg, extra)
	worst := int64(total) * ckptMiB(si.GGUF)
	if si.RAMAvailMiB > 0 && worst > si.RAMAvailMiB/4 {
		return args, append(notes, fmt.Sprintf("%s Pas d'espacement resserré d'office : %d points de reprise (~%d Mio) "+
			"dépasseraient le quart de la RAM libre (CKPT_MIN_STEP=2048 pour l'imposer).", notice, total, worst))
	}
	args = append(args, "--checkpoint-min-step", strconv.Itoa(ckptAutoMinStep))
	return args, append(notes, fmt.Sprintf("%s → --checkpoint-min-step %d : points de reprise exacts plus rapprochés, "+
		"~%d Mio de RAM hôte au plus. CKPT_MIN_STEP=<jetons> pour fixer.", notice, ckptAutoMinStep, worst))
}

// reasoningPreserveArgs : REASONING_PRESERVE=on|off → --reasoning-preserve /
// --no-reasoning-preserve, seulement si la clé est posée.
//
// Le compromis, qui explique pourquoi Loki ne choisit pas : depuis b10763 le
// moteur active preserve_reasoning par défaut, et le gabarit garde alors la
// réflexion de TOUS les tours passés. Mais sans REASONING_ECHO, Loki ne renvoie
// pas le raisonnement des tours passés : un gabarit qui ne conserve rien de
// lui-même (Qwen3.6) rend alors des blocs de réflexion vides, là où « off » rend
// l'historique des anciens moteurs. À l'inverse, un gabarit qui conserve déjà de
// lui-même (Qwen3.8) changerait de rendu avec « off ». Aucun choix global n'est
// neutre pour tous les modèles : vide = défaut du moteur, inchangé.
//
// Avec REASONING_ECHO=on (llm_reasoning_echo.go), les deux clés se complètent
// sans se recouvrir : l'écho fournit le raisonnement, cette clé décide si le
// gabarit le garde au-delà du tour en cours. « on » donne alors un préfixe
// stable d'un message utilisateur à l'autre, au prix de plus de contexte ; le
// comptage de Loki suit le verdict de la sonde de gabarit (preservesHistory),
// pas cette clé, car un gabarit sans le réglage (Qwen3.5) l'ignore.
func reasoningPreserveArgs(cfg map[string]string, extra []string, si serveSysInfo) (args, notes []string) {
	v := strings.ToLower(strings.TrimSpace(cfg["REASONING_PRESERVE"]))
	if v == "" {
		return nil, nil
	}
	flag := ""
	switch v {
	case "on":
		flag = "--reasoning-preserve"
	case "off":
		flag = "--no-reasoning-preserve"
	default:
		return nil, []string{"REASONING_PRESERVE=" + cfg["REASONING_PRESERVE"] + " illisible (on ou off) : ignoré"}
	}
	switch {
	case !strings.Contains(si.Help, "--no-reasoning-preserve"):
		return nil, []string{"ce moteur ne connaît pas --reasoning-preserve : REASONING_PRESERVE ignoré"}
	case hasAnyFlag(extra, "--reasoning-preserve", "--no-reasoning-preserve"):
		return nil, nil
	case strings.Contains(strings.Join(flagValues(extra, "--chat-template-kwargs"), " "), "preserve_reasoning"):
		return nil, []string{"--chat-template-kwargs fixe déjà preserve_reasoning : REASONING_PRESERVE ignoré"}
	case si.ArgEnv["LLAMA_ARG_REASONING_PRESERVE"] != "":
		return nil, []string{"LLAMA_ARG_REASONING_PRESERVE est posée : REASONING_PRESERVE ignoré"}
	case strings.Contains(si.ArgEnv["LLAMA_ARG_CHAT_TEMPLATE_KWARGS"], "preserve_reasoning"):
		return nil, []string{"LLAMA_ARG_CHAT_TEMPLATE_KWARGS fixe déjà preserve_reasoning : REASONING_PRESERVE ignoré"}
	}
	return []string{flag}, nil
}
