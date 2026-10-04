package loki

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// Second slot pour les travaux annexes (SIDE_SLOT, off par défaut).
//
// En --parallel 1, un vérificateur, un sous-agent, une tâche ou un bench
// prennent LE slot de la conversation : son état part dans le cache RAM et
// revient après — quand il y tient encore. Avec SIDE_SLOT=on, le moteur ouvre
// deux slots, la conversation garde le slot 0 et tout le reste passe par le
// slot 1 (id_slot, voir llm_sideslot.go) : l'état principal ne bouge plus du
// tout.
//
// Le dimensionnement est le cœur de l'affaire. Un cache KV UNIFIÉ partagé entre
// deux slots laisse chacun grandir jusqu'à la taille du pool : quand les deux
// travaillent et que le pool est plein, llama.cpp renvoie « Context size has
// been exceeded. » aux DEUX, et Loki prendrait ça pour un débordement de la
// conversation — compaction, voire réduction, d'un fil qui tenait très bien
// dans sa fenêtre. Et --kv-unified-per-slot plafonne tous les slots à la même
// valeur, sans rien garantir sur la somme. On prend donc le cache NON unifié
// (--no-kv-unified) avec -c 2×CTX : llama.cpp découpe alors le pool en deux
// flux de CTX jetons chacun, séparés. Un débordement concurrent est impossible
// par construction, chaque slot a exactement la fenêtre de la conversation (les
// travaux annexes ne sont pas raccourcis), et --cache-idle-slots ne vide plus
// les slots au repos (il ne le fait que sous cache unifié) : rien d'autre à
// régler.
//
// Le prix : le cache KV (et l'état récurrent d'un hybride) en double, en VRAM.
// On refuse donc l'option quand le compte ne tient pas, quand des poids vivent
// en RAM (le KV en plus pousserait le placement automatique à sortir des
// couches du GPU), et dès que l'utilisateur a réglé lui-même l'un des leviers
// en jeu : on ne mélange pas ses réglages et les nôtres.

// sideSlotOn : la clé SIDE_SLOT est-elle posée ?
func sideSlotOn(cfg map[string]string) bool {
	switch strings.ToLower(strings.TrimSpace(cfg["SIDE_SLOT"])) {
	case "on", "1", "true", "yes", "oui":
		return true
	}
	return false
}

// sideSlotConflictFlags : les drapeaux d'EXTRA_ARGS qui décident déjà de ce que
// SIDE_SLOT règle. Un seul suffit à refuser.
var sideSlotConflictFlags = []string{
	"-c", "--ctx-size",
	"-np", "--parallel",
	"-kvu", "--kv-unified", "-no-kvu", "--no-kv-unified",
	"--kv-unified-per-slot",
	"--cache-idle-slots", "--no-cache-idle-slots",
}

// sideSlotArgEnv : les mêmes leviers posés par variable. La ligne de commande
// les écraserait sans rien dire : un choix de l'utilisateur, on s'abstient.
var sideSlotArgEnv = []string{"LLAMA_ARG_N_PARALLEL", "LLAMA_ARG_KV_UNIFIED", "LLAMA_ARG_KV_UNIFIED_PER_SLOT",
	"LLAMA_ARG_CACHE_IDLE_SLOTS", "LLAMA_ARG_CTX_SIZE"}

// probeSideSlotEnv complète ArgEnv avec sideSlotArgEnv, seulement si la clé est
// posée : sans elle, rien n'est lu.
func probeSideSlotEnv(cfg map[string]string, si *serveSysInfo) {
	if !sideSlotOn(cfg) {
		return
	}
	if si.ArgEnv == nil {
		si.ArgEnv = map[string]string{}
	}
	for _, k := range sideSlotArgEnv {
		if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
			si.ArgEnv[k] = v
		}
	}
}

// sideSlotPlan décide de SIDE_SLOT pour ce lancement. ctx > 0 : accepté, chaque
// slot aura ctx jetons (le pool en fait le double) ; extraMiB est la VRAM de
// plus, estimée. ctx == 0 : refusé (why dit pourquoi), ou clé absente (why vide)
// — la ligne de commande est alors celle d'avant, à l'octet près.
func sideSlotPlan(cfg map[string]string, extra []string, si serveSysInfo) (ctx int, extraMiB int64, why string) {
	if !sideSlotOn(cfg) {
		return 0, 0, ""
	}
	if isExternalConfig(cfg) {
		return 0, 0, "preset externe"
	}
	if p := strings.TrimSpace(cfg["PARALLEL"]); p != "" && p != "2" {
		return 0, 0, "PARALLEL=" + p + " (SIDE_SLOT ouvre exactement 2 slots)"
	}
	for _, f := range sideSlotConflictFlags {
		if hasAnyFlag(extra, f) {
			return 0, 0, f + " est réglé dans EXTRA_ARGS"
		}
	}
	for _, k := range sideSlotArgEnv {
		if si.ArgEnv[k] != "" {
			return 0, 0, k + " est posée"
		}
	}
	if !strings.Contains(si.Help, "--no-kv-unified") {
		return 0, 0, "ce moteur ne connaît pas --no-kv-unified"
	}
	n, err := strconv.Atoi(strings.TrimSpace(cfg["CTX"]))
	if strings.TrimSpace(cfg["CTX"]) == "" {
		n, err = 32768, nil
	}
	if err != nil || n <= 0 {
		return 0, 0, "CTX doit être un nombre de jetons explicite"
	}
	extraMiB, why = sideSlotVRAM(cfg, extra, si, n)
	if why != "" {
		return 0, 0, why
	}
	return n, extraMiB, ""
}

// sideSlotVRAM estime la VRAM du second slot (cache KV de ctx jetons et état
// récurrent) et vérifie que modèle + deux états tiennent dans 90 % de la VRAM,
// la même marge que cacheRAMAuto. Inconnu = refus : on ne double pas le cache
// KV à l'aveugle, c'est exactement la panne de mémoire qu'on a déjà vécue.
func sideSlotVRAM(cfg map[string]string, extra []string, si serveSysInfo, ctx int) (extraMiB int64, why string) {
	switch {
	case si.VRAMMiB <= 0:
		return 0, "VRAM NVIDIA inconnue (macOS, AMD, --device…)"
	case si.GGUF == nil:
		return 0, "métadonnées GGUF illisibles"
	case si.ModelBytes <= 0:
		return 0, "taille du modèle inconnue"
	}
	if r := cpuWeights(cfg, extra, si.ArgEnv, si.GGUF.BlockCount); r != "" {
		return 0, r
	}
	kt, vt, _ := effectiveKVTypes(cfg, extra, si.ArgEnv)
	perTok, rec := ggufStateBytes(*si.GGUF, kt, vt)
	if perTok <= 0 {
		return 0, "taille d'état inconnue"
	}
	state := perTok*float64(ctx) + float64(rec)
	if float64(si.ModelBytes)+2*state > 0.9*float64(si.VRAMMiB)*(1<<20) {
		return 0, fmt.Sprintf("VRAM insuffisante : modèle + 2 × %d Mio d'état dépassent 90 %% de %d Mio",
			int64(math.Ceil(state/(1<<20))), si.VRAMMiB)
	}
	return int64(math.Ceil(state / (1 << 20))), ""
}

// sideSlotNote : ce que le lancement dit de SIDE_SLOT, accepté ou refusé.
func sideSlotNote(ctx int, extraMiB int64, why string, nglForced bool) string {
	if ctx <= 0 {
		if why == "" {
			return ""
		}
		return "SIDE_SLOT refusé (" + why + ") : un seul slot, comme sans la clé"
	}
	n := fmt.Sprintf("SIDE_SLOT : 2 slots de %d jetons (-c %d, --no-kv-unified), ~%d Mio de VRAM en plus "+
		"pour le second (cache KV et état récurrent ; le brouillon MTP éventuel et les points de reprise en RAM hôte "+
		"doublent aussi)", ctx, 2*ctx, extraMiB)
	if nglForced {
		return n + " ; NGL est imposé : surveille la VRAM au premier long prompt"
	}
	return n + " ; le placement automatique sortira des couches du GPU s'il manque de place"
}

// sideSlotPreview : l'avis de SIDE_SLOT pour l'éditeur de preset, calculé comme
// au lancement, à l'aide du moteur près (supposée connaître --no-kv-unified :
// un moteur trop ancien sera refusé au lancement, et le journal le dira).
func sideSlotPreview(content string) (on bool, extraMiB int64, why string) {
	cfg := parseEnv(content)
	if !sideSlotOn(cfg) {
		return false, 0, ""
	}
	if isExternalConfig(cfg) {
		return false, 0, "preset externe"
	}
	extra := splitArgs(cfg["EXTRA_ARGS"])
	si := serveSysInfo{Help: "--no-kv-unified", ArgEnv: map[string]string{}}
	for _, k := range append(append(append(append([]string{}, serveArgEnv...), fidelityArgEnv...), cacheArgEnv...), sideSlotArgEnv...) {
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
		return false, 0, "modèle introuvable"
	}
	probeCacheRAM(cfg, extra, &si)
	ctx, mib, why := sideSlotPlan(cfg, extra, si)
	return ctx > 0, mib, why
}

// nglForced : le preset impose-t-il un nombre de couches GPU ? Alors le
// placement automatique ne tourne pas, et un second état de trop finit en
// panne de mémoire plutôt qu'en couches sorties du GPU. 999 est la sentinelle
// « toutes », traduite en auto sur un moteur récent (voir nglArgs).
func nglForced(cfg map[string]string, extra []string) bool {
	v := flagValue(extra, "-ngl", "--n-gpu-layers", "--gpu-layers")
	if v == "" {
		v = strings.TrimSpace(cfg["NGL"])
		if v == "999" {
			return false
		}
	}
	return v != "" && !strings.EqualFold(v, "auto")
}
