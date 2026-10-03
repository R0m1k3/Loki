package loki

import (
	"context"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Cache de prompts en RAM (--cache-ram) et isolation des travaux annexes.
//
// llama-server garde en RAM hôte une copie EXACTE des états de slot qu'il quitte
// (cache KV, état récurrent, points de reprise, contexte du brouillon MTP) et la
// recharge octet pour octet quand la conversation revient. Ce n'est pas une
// approximation : la seule différence avec un prefill complet est le bruit de
// virgule flottante dû à un découpage des lots différent, exactement comme le
// cache_prompt par défaut. Le type du cache KV n'est pas touché, il est copié
// tel quel.
//
// Le défaut du moteur (8 Gio) est trop petit pour une conversation longue d'un
// modèle dense : l'état principal (30 à 65 k jetons) ne tient plus à côté de
// celui d'un vérificateur ou d'un sous-agent, et la conversation est alors
// recalculée en entier — 30 à 80 s sur un 27B. Loki agrandit donc ce cache,
// mais seulement quand il sait mesurer le besoin et que la RAM est libre : il ne
// descend JAMAIS sous le défaut du moteur, et se tait dès que l'utilisateur a
// tranché (CACHE_RAM, -cram dans EXTRA_ARGS, LLAMA_ARG_CACHE_RAM).
//
// Tout ce qui décide est pur ; probeCacheRAM fait les lectures (GGUF, RAM,
// nvidia-smi) une fois, avant buildServeArgs.

// engineCacheRAMDefault : cache_ram_mib par défaut de llama.cpp (common.h).
const engineCacheRAMDefault = 8192

// cacheArgEnv : la variable qui règle le cache sans drapeau. llama.cpp donne la
// priorité à la ligne de commande : un --cache-ram de Loki l'écraserait.
var cacheArgEnv = []string{"LLAMA_ARG_CACHE_RAM"}

// probeCacheRAM complète si avec ce dont cacheRAMAuto a besoin. Chaque lecture
// ratée laisse un zéro, et un zéro veut dire « on ne sait pas » : le moteur
// garde alors son défaut.
func probeCacheRAM(cfg map[string]string, extra []string, si *serveSysInfo) {
	if si.ArgEnv == nil {
		si.ArgEnv = map[string]string{}
	}
	for _, k := range cacheArgEnv {
		if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
			si.ArgEnv[k] = v
		}
	}
	if si.Model != "" {
		si.ModelBytes = shardFamilySize(filepath.Dir(si.Model), filepath.Base(si.Model))
		if g, err := ggufMeta(si.Model); err == nil {
			si.GGUF = &g
		}
	}
	used, total := ramUsageMB()
	if total > 0 {
		si.RAMMiB, si.RAMAvailMiB = int64(total), int64(total-used)
	}
	if runtime.GOOS == "linux" {
		if limit, cur := cgroupMemMiB(os.DirFS("/")); limit > 0 {
			if si.RAMMiB == 0 || limit < si.RAMMiB {
				si.RAMMiB = limit
			}
			if free := limit - cur; free >= 0 && (si.RAMAvailMiB == 0 || free < si.RAMAvailMiB) {
				si.RAMAvailMiB = free
			}
		}
	}
	// Mémoire unifiée (macOS), AMD, Vulkan, --device choisi à la main : pas de
	// total VRAM fiable, donc pas d'agrandissement.
	if runtime.GOOS == "darwin" || hasAnyFlag(extra, "-dev", "--device") || si.ArgEnv["LLAMA_ARG_DEVICE"] != "" {
		return
	}
	// La sélection du preset d'abord (cudaDeviceEnv l'accompagne toujours de
	// PCI_BUS_ID), celle de l'environnement ensuite : l'aperçu de l'interface
	// tourne dans un process où le preset n'a rien posé.
	cvd, pci := cfg["CUDA_VISIBLE_DEVICES"], true
	if cvd == "" {
		cvd, pci = os.Getenv("CUDA_VISIBLE_DEVICES"), os.Getenv("CUDA_DEVICE_ORDER") == "PCI_BUS_ID"
	}
	si.VRAMMiB = nvidiaVRAMMiB(cvd, pci, 3*time.Second)
}

// cgroupMemMiB : limite mémoire du conteneur et sa consommation, en Mio (v2,
// sinon v1). 0 = pas de limite. Le MemAvailable de /proc/meminfo voit l'hôte
// entier : sans ça, un conteneur limité à 16 Go sur une machine de 64 Go se
// croirait riche.
func cgroupMemMiB(fsys fs.FS) (limit, current int64) {
	read := func(p string) int64 {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return 0
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil || n <= 0 || n >= 1<<60 { // « max » ou la valeur géante du v1 sans limite
			return 0
		}
		return n >> 20
	}
	if limit = read("sys/fs/cgroup/memory.max"); limit > 0 {
		return limit, read("sys/fs/cgroup/memory.current")
	}
	if limit = read("sys/fs/cgroup/memory/memory.limit_in_bytes"); limit > 0 {
		return limit, read("sys/fs/cgroup/memory/memory.usage_in_bytes")
	}
	return 0, 0
}

// nvidiaVRAMMiB : VRAM totale des cartes que llama-server verra. Borné dans le
// temps comme nvidiaGPUCount ; au moindre doute, 0.
func nvidiaVRAMMiB(cvd string, pciOrder bool, timeout time.Duration) int64 {
	if !hasTool("nvidia-smi") {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := hideCmd(exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=index,memory.total",
		"--format=csv,noheader,nounits")).Output()
	if err != nil {
		return 0
	}
	return vramFromSMI(string(out), cvd, pciOrder)
}

// vramFromSMI additionne la VRAM des cartes sélectionnées par
// CUDA_VISIBLE_DEVICES. Les index de nvidia-smi sont dans l'ordre du bus PCI ;
// ceux de CUDA aussi, mais seulement avec CUDA_DEVICE_ORDER=PCI_BUS_ID. Sans
// lui, on ne sait pas quelle carte est laquelle : on prend la plus petite autant
// de fois qu'il y a de cartes — un minorant, qui ne fait que rendre Loki plus
// prudent. Un UUID ou une tranche MIG : on ne sait pas, 0.
func vramFromSMI(out, cvd string, pciOrder bool) int64 {
	byIdx := map[int]int64{}
	var minMiB int64
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 2 {
			continue
		}
		i, err1 := strconv.Atoi(strings.TrimSpace(f[0]))
		m, err2 := strconv.ParseInt(strings.TrimSpace(f[1]), 10, 64)
		if err1 != nil || err2 != nil || m <= 0 {
			return 0
		}
		byIdx[i] = m
		if minMiB == 0 || m < minMiB {
			minMiB = m
		}
	}
	if len(byIdx) == 0 {
		return 0
	}
	cvd = strings.TrimSpace(cvd)
	if cvd == "" {
		var sum int64
		for _, m := range byIdx {
			sum += m
		}
		return sum
	}
	var sum int64
	n := 0
	for _, d := range strings.Split(cvd, ",") {
		if d = strings.TrimSpace(d); d == "" {
			continue
		}
		i, err := strconv.Atoi(d)
		if err != nil {
			return 0
		}
		m, ok := byIdx[i]
		if !ok {
			return 0
		}
		sum += m
		n++
	}
	if !pciOrder {
		return minMiB * int64(n)
	}
	return sum
}

// kvTypeBytes : octets par élément d'un type de cache (blocs de 32 pour les
// types quantifiés de ggml). Inconnu : compté comme f16, l'estimation reste
// bornée par le plafond de RAM.
func kvTypeBytes(t string) float64 {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "f32":
		return 4
	case "q8_0":
		return 34.0 / 32
	case "q5_1":
		return 24.0 / 32
	case "q5_0":
		return 22.0 / 32
	case "q4_1":
		return 20.0 / 32
	case "q4_0", "iq4_nl":
		return 18.0 / 32
	}
	return 2
}

// ggufStateBytes estime ce que pèse l'état d'une séquence : octets de cache KV
// par jeton (couches d'attention), et octets d'état récurrent (couches SSM d'un
// hybride, en f32 comme dans llama.cpp). 0 par jeton = on ne sait pas.
func ggufStateBytes(g GGUFInfo, kt, vt string) (perToken float64, recurrent int64) {
	kl, vl := g.KeyLen, g.ValLen
	if g.EmbeddingLength > 0 && g.HeadCount > 0 {
		if kl == 0 {
			kl = g.EmbeddingLength / g.HeadCount
		}
		if vl == 0 {
			vl = g.EmbeddingLength / g.HeadCount
		}
	}
	if kl == 0 || vl == 0 || g.BlockCount == 0 {
		return 0, 0
	}
	heads, recLayers := 0, 0
	switch {
	case len(g.HeadCountKVLayers) > 0:
		for _, h := range g.HeadCountKVLayers {
			heads += h
			if h == 0 && g.Hybrid {
				recLayers++
			}
		}
	case g.Hybrid && g.FullAttnInterval > 0:
		attn := g.BlockCount / g.FullAttnInterval
		heads, recLayers = attn*g.HeadCountKV, g.BlockCount-attn
	default:
		heads = g.BlockCount * g.HeadCountKV
		if heads == 0 && g.Hybrid {
			recLayers = g.BlockCount // récurrent pur
		}
	}
	perToken = float64(heads) * (float64(kl)*kvTypeBytes(kt) + float64(vl)*kvTypeBytes(vt))
	if recLayers > 0 {
		conv := int64(0)
		if g.SSMConv > 1 {
			conv = int64(g.SSMConv-1) * int64(g.SSMInner+2*g.SSMGroups*g.SSMState)
		}
		recurrent = int64(recLayers) * (conv + int64(g.SSMState)*int64(g.SSMInner)) * 4
	}
	return perToken, recurrent
}

// serveCtx : la taille de contexte que le moteur ouvrira (-c d'EXTRA_ARGS, sinon
// CTX, sinon le défaut de Loki ; 0 = celle d'entraînement du modèle).
func serveCtx(cfg map[string]string, extra []string, g *GGUFInfo) int {
	s := flagValue(extra, "-c", "--ctx-size")
	if s == "" {
		s = strings.TrimSpace(cfg["CTX"])
	}
	if s == "" {
		s = "32768"
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	if n == 0 && g != nil {
		n = g.ContextLength
	}
	return n
}

// cpuWeights dit pourquoi des poids (ou le cache KV) vivent en RAM hôte. Le
// cache de prompts y ferait alors concurrence au cache de pages des experts
// mappés : on n'y touche pas. Vide = tout est sur GPU.
func cpuWeights(cfg map[string]string, extra []string, argEnv map[string]string, blocks int) string {
	ot := flagValues(extra, "-ot", "--override-tensor")
	if v := argEnv["LLAMA_ARG_OVERRIDE_TENSOR"]; v != "" {
		ot = append(ot, v)
	}
	for _, v := range ot {
		for _, el := range strings.Split(v, ",") {
			if _, buft, ok := strings.Cut(el, "="); ok && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(buft)), "CPU") {
				return "tenseurs sur CPU (-ot)"
			}
		}
	}
	if hasAnyFlag(extra, "--cpu-moe", "-cmoe") || envTruthy(argEnv["LLAMA_ARG_CPU_MOE"]) {
		return "experts sur CPU"
	}
	for _, n := range []struct{ env, short, long string }{
		{"LLAMA_ARG_N_CPU_MOE", "-ncmoe", "--n-cpu-moe"},
		{"LLAMA_ARG_N_CPU_FFN", "-ncffn", "--n-cpu-ffn"},
	} {
		for _, v := range append(flagValues(extra, n.short, n.long), argEnv[n.env]) {
			if v = strings.TrimSpace(v); v != "" && v != "0" {
				return "couches " + n.long + " sur CPU"
			}
		}
	}
	if !kvOffloaded(extra, argEnv) {
		return "cache KV sur CPU"
	}
	ngl := flagValue(extra, "-ngl", "--n-gpu-layers", "--gpu-layers")
	if ngl == "" {
		ngl = strings.TrimSpace(cfg["NGL"])
		if strings.EqualFold(ngl, "auto") {
			ngl = argEnv["LLAMA_ARG_N_GPU_LAYERS"]
		}
	}
	// 999 est la sentinelle « toutes » ; un nombre plus petit que le modèle (sa
	// couche de sortie comprise) laisse des couches au CPU.
	if n, err := strconv.Atoi(strings.TrimSpace(ngl)); err == nil && n != 999 && (blocks == 0 || n < blocks+1) {
		return "couches GPU limitées à " + strconv.Itoa(n)
	}
	return ""
}

// cacheRAMAuto calcule la taille du cache de prompts quand CACHE_RAM est vide
// ou « auto ». 0 = garder le défaut du moteur, why dit pourquoi.
//
// Besoin : 2,5 fois l'état d'une conversation pleine (cache KV à CTX jetons,
// plus l'état récurrent et ses points de reprise sur un hybride) — de quoi
// garder la conversation principale ET l'état d'un travail annexe. Plafond : 30
// % de la RAM (limite du conteneur comprise) et la moitié de ce qui est libre au
// lancement. Ce plafond est un réglage, pas un filet : sous Linux, un
// dépassement finit en général par l'OOM killer, pas par le repli du moteur.
func cacheRAMAuto(cfg map[string]string, extra []string, si serveSysInfo) (mib int64, why string) {
	switch {
	case si.VRAMMiB <= 0:
		return 0, "VRAM NVIDIA inconnue"
	case si.GGUF == nil:
		return 0, "métadonnées GGUF illisibles"
	case si.RAMMiB <= 0 || si.RAMAvailMiB <= 0:
		return 0, "RAM inconnue"
	case si.ModelBytes <= 0:
		return 0, "taille du modèle inconnue"
	}
	if r := cpuWeights(cfg, extra, si.ArgEnv, si.GGUF.BlockCount); r != "" {
		return 0, r
	}
	kt, vt, _ := effectiveKVTypes(cfg, extra, si.ArgEnv)
	perTok, rec := ggufStateBytes(*si.GGUF, kt, vt)
	ctx := serveCtx(cfg, extra, si.GGUF)
	if perTok <= 0 || ctx <= 0 {
		return 0, "taille d'état inconnue"
	}
	kvBytes := perTok * float64(ctx)
	if float64(si.ModelBytes)+kvBytes > 0.9*float64(si.VRAMMiB)*(1<<20) {
		return 0, "modèle trop gros pour la VRAM : des poids restent en RAM"
	}
	state := kvBytes
	if rec > 0 {
		// Points de reprise : llama.cpp en garde jusqu'à 32 par séquence ; on en
		// compte un par tranche de 8 k jetons, estimation grossière mais bornée.
		state += float64(rec) * float64(min(32, ctx/8192+1)+1)
	}
	need := int64(math.Ceil(2.5 * state / (1 << 20)))
	ceiling := min(si.RAMMiB*30/100, si.RAMAvailMiB/2)
	if ceiling <= engineCacheRAMDefault {
		return 0, fmt.Sprintf("RAM libre trop juste (plafond %d Mio)", ceiling)
	}
	if need <= engineCacheRAMDefault {
		return 0, fmt.Sprintf("le défaut suffit (besoin estimé %d Mio)", need)
	}
	return min(need, ceiling), fmt.Sprintf("besoin estimé %d Mio, plafond %d Mio", need, ceiling)
}

// cacheRAMSetting lit CACHE_RAM : auto (vide ou « auto »), ou un nombre de Mio
// passé tel quel (-1 = sans limite, 0 = cache coupé). ok=false : valeur
// illisible.
func cacheRAMSetting(cfg map[string]string) (manual string, auto, ok bool) {
	v := strings.TrimSpace(cfg["CACHE_RAM"])
	if v == "" || strings.EqualFold(v, "auto") {
		return "", true, true
	}
	if n, err := strconv.Atoi(v); err == nil && n >= -1 {
		return strconv.Itoa(n), false, true
	}
	return "", false, false
}

// cacheRAMArgs : le --cache-ram que Loki ajoute, et ce qu'il en dit.
func cacheRAMArgs(cfg map[string]string, extra []string, si serveSysInfo) (args, notes []string) {
	manual, auto, ok := cacheRAMSetting(cfg)
	if !ok {
		return nil, []string{"CACHE_RAM=" + cfg["CACHE_RAM"] + " illisible (un nombre de Mio, -1, 0 ou auto) : ignoré"}
	}
	if !strings.Contains(si.Help, "--cache-ram") {
		if !auto {
			notes = append(notes, "ce moteur ne connaît pas --cache-ram : CACHE_RAM ignoré")
		}
		return nil, notes
	}
	if hasAnyFlag(extra, "-cram", "--cache-ram") {
		return nil, nil
	}
	if si.ArgEnv["LLAMA_ARG_CACHE_RAM"] != "" {
		if !auto {
			notes = append(notes, "LLAMA_ARG_CACHE_RAM est posée : CACHE_RAM ignoré")
		}
		return nil, notes
	}
	if !auto {
		return []string{"--cache-ram", manual}, nil
	}
	n, why := cacheRAMAuto(cfg, extra, si)
	if n <= 0 {
		return nil, nil
	}
	return []string{"--cache-ram", strconv.FormatInt(n, 10)},
		[]string{fmt.Sprintf("cache de prompts : --cache-ram %d (RAM hôte, copie exacte de l'état, jamais la VRAM ; %s). "+
			"CACHE_RAM=<Mio> pour fixer, 0 pour couper.", n, why)}
}

// cacheRAMOff : le cache de prompts est-il coupé (0) par l'une des trois
// sources ? Sans lui, effacer un slot ne protège rien.
func cacheRAMOff(cfg map[string]string, extra []string, argEnv map[string]string) bool {
	v := flagValue(extra, "-cram", "--cache-ram")
	if v == "" {
		v = argEnv["LLAMA_ARG_CACHE_RAM"]
	}
	if v == "" {
		v, _, _ = cacheRAMSetting(cfg)
	}
	return strings.TrimSpace(v) == "0"
}

// cacheIsolationOn : CACHE_ISOLATE=off coupe l'effacement du slot après les
// travaux annexes (et donc --slot-save-path), sans toucher au code.
func cacheIsolationOn(cfg map[string]string) bool {
	return !strings.EqualFold(strings.TrimSpace(cfg["CACHE_ISOLATE"]), "off")
}

// loopbackHost : le moteur n'écoute que sur la machine.
func loopbackHost(h string) bool {
	switch strings.Trim(strings.TrimSpace(h), "[]") {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// slotSaveArgs : --slot-save-path, sans lequel llama-server refuse toute action
// sur /slots — l'effacement compris. Mais le drapeau ouvre aussi save et
// restore, qui écrivent des Gio sur le disque, à quiconque joint le moteur. On
// ne le pose donc que si le moteur est fermé aux autres (boucle locale, ou clé
// d'API exigée), que le dossier existe (le moteur refuse de démarrer sinon), et
// que l'isolation servira : cache actif, CACHE_ISOLATE pas à off. Les proxys de
// Loki refusent de leur côté tout /slots qui n'est pas une lecture.
func slotSaveArgs(cfg map[string]string, extra []string, si serveSysInfo) []string {
	if si.SlotDir == "" || !strings.Contains(si.Help, "--slot-save-path") ||
		hasAnyFlag(extra, "--slot-save-path") || !cacheIsolationOn(cfg) || cacheRAMOff(cfg, extra, si.ArgEnv) {
		return nil
	}
	host := flagValue(extra, "--host")
	if host == "" {
		host = cfg["HOST"]
	}
	if host == "" {
		host = "0.0.0.0"
	}
	if !loopbackHost(host) && si.APIKey == "" {
		return nil
	}
	return []string{"--slot-save-path", si.SlotDir}
}

// prepareSlotDir crée LOKI_HOME/slots (0700) et le vide : Loki n'y sauve jamais
// rien, un fichier présent ne peut venir que d'un appel qui n'aurait pas dû
// passer. Vide = dossier indisponible, le drapeau ne sera pas posé.
func prepareSlotDir(home string) string {
	dir := filepath.Join(home, "slots")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return ""
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.Type().IsRegular() {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	return dir
}

// cacheRAMPreview : la taille que le moteur prendra pour ce preset, pour
// l'éditeur. 0 = défaut du moteur (8192 Mio). Même calcul qu'au lancement, mais
// sans l'aide du moteur : l'aperçu dit ce que Loki demanderait.
func cacheRAMPreview(content string) (mib int64, why string) {
	cfg := parseEnv(content)
	if isExternalConfig(cfg) {
		return 0, "preset externe"
	}
	extra := splitArgs(cfg["EXTRA_ARGS"])
	if v := flagValue(extra, "-cram", "--cache-ram"); v != "" {
		n, _ := strconv.ParseInt(v, 10, 64)
		return n, "fixé par EXTRA_ARGS"
	}
	if v := strings.TrimSpace(os.Getenv("LLAMA_ARG_CACHE_RAM")); v != "" {
		n, _ := strconv.ParseInt(v, 10, 64)
		return n, "fixé par LLAMA_ARG_CACHE_RAM"
	}
	manual, auto, ok := cacheRAMSetting(cfg)
	if !ok {
		return 0, "CACHE_RAM illisible"
	}
	if !auto {
		n, _ := strconv.ParseInt(manual, 10, 64)
		return n, "fixé par CACHE_RAM"
	}
	si := serveSysInfo{ArgEnv: map[string]string{}}
	for _, k := range append(append(append([]string{}, serveArgEnv...), fidelityArgEnv...), cacheArgEnv...) {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			si.ArgEnv[k] = v
		}
	}
	if m := strings.TrimSpace(cfg["MODEL"]); m != "" {
		if p, err := resolveServeModelPath(m); err == nil {
			si.Model = p
		}
	}
	if si.Model == "" {
		return 0, "modèle introuvable"
	}
	probeCacheRAM(cfg, extra, &si)
	return cacheRAMAuto(cfg, extra, si)
}
