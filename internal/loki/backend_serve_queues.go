package loki

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Files de lancement CUDA (CUDA_SCALE_LAUNCH_QUEUES) sur deux GPU ou plus.
//
// Quand llama.cpp répartit les couches entre plusieurs cartes et que le modèle
// y tient en entier, il fait travailler les cartes en pipeline : pendant que la
// seconde calcule un micro-lot, la première attaque le suivant. Encore faut-il
// que le CPU puisse empiler assez de lancements CUDA d'avance ; avec la file par
// défaut du pilote, il bloque et le pipeline se vide. « 4x » quadruple cette
// file (PR llama.cpp #19042) : c'est une affaire de pilote, pas de calcul — mêmes
// noyaux, mêmes entrées, même ordre, donc une sortie identique au jeton près. Le
// prix : ~120 Mo de RAM système par GPU.
//
// llama.cpp l'a posé par défaut puis retiré (#19227) après des blocages sur
// Jetson (mémoire unifiée, MoE décodé en partie sur CPU) ; ses docs le
// recommandent désormais à la main en multi-GPU. Loki le pose donc lui-même,
// mais seulement là où le pipeline peut réellement exister (voir
// pipelineBlocker) : ailleurs le gain est nul et l'on ne s'exposerait qu'au
// seul cas de panne jamais rapporté.
//
// Gain non mesuré sur la machine de référence (5060 Ti + 3060, 27B) : en amont,
// +10 à 25 % de prompt seulement sur un 70B réparti sur deux grosses cartes,
// rien de mesurable sur un 8B. Le décodage ne bouge pas. Pour vérifier qu'il
// s'applique, le journal du moteur doit dire « pipeline parallelism enabled ».
//
// Choix de l'utilisateur, par ordre de priorité :
//
//	CUDA_SCALE_LAUNCH_QUEUES déjà dans l'environnement → intouché (conteneur Unraid…)
//	CUDA_LAUNCH_QUEUES=off                             → rien
//	CUDA_LAUNCH_QUEUES=0.25x|0.5x|2x|4x                → cette valeur, sans condition
//	vide ou auto                                       → 4x si ≥ 2 GPU et pipeline possible
//
// Une autre valeur serait lue comme 1x par le pilote, sans un mot : on l'ignore
// en le disant.

const launchQueuesAuto = "4x"

// launchQueueValues : les seules valeurs que le pilote CUDA comprend.
var launchQueueValues = map[string]bool{"0.25x": true, "0.5x": true, "2x": true, "4x": true}

// serveArgEnv : les variables LLAMA_ARG_* qui font, sans drapeau, ce que lit
// pipelineBlocker. llama.cpp les applique quand le drapeau manque — les ignorer
// ferait conclure à un pipeline que le moteur n'ouvrira pas.
var serveArgEnv = []string{"LLAMA_ARG_DEVICE", "LLAMA_ARG_SPLIT_MODE", "LLAMA_ARG_N_GPU_LAYERS",
	"LLAMA_ARG_CPU_MOE", "LLAMA_ARG_N_CPU_MOE", "LLAMA_ARG_NO_KV_OFFLOAD"}

// launchQueuesEnv décide de CUDA_SCALE_LAUNCH_QUEUES. Fonction pure : le nombre
// de GPU et l'environnement arrivent déjà sondés dans si. Valeur vide = ne rien
// poser.
func launchQueuesEnv(cfg map[string]string, extra []string, si serveSysInfo) (val string, notes []string) {
	if si.LaunchQueues != "" {
		return "", nil
	}
	want := strings.ToLower(strings.TrimSpace(cfg["CUDA_LAUNCH_QUEUES"]))
	switch {
	case want == "off":
		return "", nil
	case launchQueueValues[want]:
		return want, []string{"CUDA_SCALE_LAUNCH_QUEUES=" + want + " (CUDA_LAUNCH_QUEUES)"}
	case want != "" && want != "auto":
		notes = append(notes, "CUDA_LAUNCH_QUEUES="+cfg["CUDA_LAUNCH_QUEUES"]+
			" ignoré (0.25x, 0.5x, 2x, 4x ou off) → automatique")
	}
	n := servedGPUCount(extra, si)
	if n < 2 || pipelineBlocker(cfg, extra, si.ArgEnv) != "" {
		return "", notes
	}
	return launchQueuesAuto, append(notes, fmt.Sprintf("CUDA_SCALE_LAUNCH_QUEUES=%s (%d GPU, parallélisme pipeline "+
		"possible) — CUDA_LAUNCH_QUEUES=off pour s'en passer", launchQueuesAuto, n))
}

// servedGPUCount : combien de GPU CUDA llama-server utilisera. Le --device du
// preset d'abord (c'est là que l'éditeur range le choix des cartes), puis ce
// que cmdServe a compté (CUDA_VISIBLE_DEVICES, sinon nvidia-smi). Un --device
// qui ne nomme que des cartes Vulkan ou Metal compte zéro : la variable ne
// concerne que CUDA.
func servedGPUCount(extra []string, si serveSysInfo) int {
	dev := flagValue(extra, "-dev", "--device")
	if dev == "" {
		dev = si.ArgEnv["LLAMA_ARG_DEVICE"]
	}
	if dev == "" {
		return si.GPUs
	}
	n := 0
	for _, d := range strings.Split(dev, ",") {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(d)), "CUDA") {
			n++
		}
	}
	return n
}

// pipelineBlocker dit pourquoi llama.cpp n'ouvrira PAS le pipeline entre
// cartes (llama-context.cpp : toutes les couches sur GPU, découpe par couches,
// cache KV sur GPU, aucune surcharge de tenseurs). Vide = il peut l'ouvrir.
// --n-cpu-moe et --cpu-moe sont des surcharges de tenseurs déguisées : le
// preset MoE à experts sur CPU n'a donc jamais de pipeline.
func pipelineBlocker(cfg map[string]string, extra []string, argEnv map[string]string) string {
	if hasAnyFlag(extra, "-ot", "--override-tensor", "--cpu-moe", "-cmoe") || envOn(argEnv["LLAMA_ARG_CPU_MOE"]) {
		return "surcharge de tenseurs"
	}
	moe := flagValue(extra, "--n-cpu-moe", "-ncmoe")
	if moe == "" {
		moe = argEnv["LLAMA_ARG_N_CPU_MOE"]
	}
	if moe != "" && moe != "0" {
		return "experts MoE sur CPU"
	}
	if hasAnyFlag(extra, "-nkvo", "--no-kv-offload") || envOn(argEnv["LLAMA_ARG_NO_KV_OFFLOAD"]) {
		return "cache KV sur CPU"
	}
	sm := flagValue(extra, "-sm", "--split-mode")
	if sm == "" {
		sm = argEnv["LLAMA_ARG_SPLIT_MODE"]
	}
	if sm != "" && sm != "layer" {
		return "découpe " + sm
	}
	// Couches GPU : celles d'EXTRA_ARGS, sinon NGL (que buildServeArgs traduit),
	// sinon la variable du moteur. Un nombre choisi — hors la sentinelle 999 —
	// laisse en général des couches au CPU ; auto, all et 999 visent le tout-GPU.
	ngl := flagValue(extra, "-ngl", "--n-gpu-layers", "--gpu-layers")
	if ngl == "" {
		ngl = strings.TrimSpace(cfg["NGL"])
	}
	if ngl == "" || ngl == "auto" {
		ngl = argEnv["LLAMA_ARG_N_GPU_LAYERS"]
	}
	if ngl != "" && ngl != "auto" && ngl != "all" && ngl != "999" {
		return "couches GPU limitées à " + ngl
	}
	return ""
}

// envOn lit une variable booléenne comme llama.cpp : vide, 0 ou false = non.
func envOn(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v != "" && v != "0" && v != "false"
}

// probeServeGPUs remplit ce dont launchQueuesEnv a besoin. On ne sonde
// nvidia-smi que si la réponse peut servir : variable déjà posée, « off », un
// --device explicite ou CUDA_VISIBLE_DEVICES suffisent à trancher sans lui.
func probeServeGPUs(cfg map[string]string, extra []string, si *serveSysInfo) {
	si.LaunchQueues = os.Getenv("CUDA_SCALE_LAUNCH_QUEUES")
	si.ArgEnv = map[string]string{}
	for _, k := range serveArgEnv {
		if v := os.Getenv(k); v != "" {
			si.ArgEnv[k] = v
		}
	}
	if si.LaunchQueues != "" || strings.EqualFold(strings.TrimSpace(cfg["CUDA_LAUNCH_QUEUES"]), "off") {
		return
	}
	if hasAnyFlag(extra, "-dev", "--device") || si.ArgEnv["LLAMA_ARG_DEVICE"] != "" {
		return
	}
	// cmdServe a déjà posé la sélection de config.env dans l'environnement :
	// os.Getenv voit donc le preset d'abord, le conteneur ensuite.
	if cvd := strings.TrimSpace(os.Getenv("CUDA_VISIBLE_DEVICES")); cvd != "" {
		for _, d := range strings.Split(cvd, ",") {
			if strings.TrimSpace(d) != "" {
				si.GPUs++
			}
		}
		return
	}
	si.GPUs = nvidiaGPUCount(3 * time.Second)
}

// nvidiaGPUCount compte les cartes NVIDIA. Borné dans le temps : un pilote
// coincé peut faire pendre nvidia-smi, et le démarrage du moteur ne doit pas
// en dépendre. Au moindre doute, 0 — on ne pose rien.
func nvidiaGPUCount(timeout time.Duration) int {
	if !hasTool("nvidia-smi") {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := hideCmd(exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=index", "--format=csv,noheader")).Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}
