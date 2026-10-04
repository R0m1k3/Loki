package loki

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Parallélisme de tenseurs (SPLIT_MODE=tensor, off par défaut) : expérimental.
//
// La découpe par couches (défaut de llama.cpp) donne à chaque carte des
// couches entières, traversées l'une après l'autre. En mode tensor, chaque
// couche est coupée entre les cartes, qui calculent en même temps puis
// additionnent leurs morceaux (all-reduce). Sur deux RTX 4090, l'amont mesure
// +21-30 % au décodage, mais -37 % au prefill — et la boucle d'agent de Loki
// relit beaucoup. Sur deux GeForce en PCIe sans P2P, la somme passe par
// l'hôte : un gain net par tour est loin d'être acquis. D'où l'opt-in, et une
// longue liste de refus propres plutôt qu'un moteur qui meurt en boucle.
//
// Fidélité : l'all-reduce change l'ordre des additions (bruit de virgule
// flottante), mais le chemin NCCL va plus loin — au-delà de 32 768 éléments il
// COMPRESSE les sommes partielles en BF16 (ggml-cuda.cu), soit à chaque couche
// de chaque prefill : une perte de précision du calcul, de la classe d'une
// quantification. Loki pose donc GGML_CUDA_ALLREDUCE=internal (réduction dans
// le type natif), sauf si l'utilisateur a choisi lui-même. Le cache KV doit
// rester f16, bf16 ou f32 : le mode tensor ne gère pas les caches quantifiés,
// et Loki ne réécrit jamais un type de cache d'office (ça change le budget
// VRAM). Pas de --fit en mode tensor (common/fit.cpp) : Loki estime la VRAM
// lui-même et refuse si le compte ne tient pas, sans jamais réduire CTX.

// splitDev : une carte telle que la voit CE moteur (--list-devices), dans son
// ordre — celui que suivent -ts et --device.
type splitDev struct {
	ID       string // CUDA0, Vulkan1…
	TotalMiB int64
}

// splitTensorDenyArch : les architectures que llama.cpp refuse en mode tensor
// (llm_arch_supports_sm_tensor) — il mourrait au chargement, et systemd
// relancerait en boucle. qwen4exp n'y est pas, mais Loki l'écarte : son preset
// place les experts sur CPU avec un cache q8_0, l'inverse de ce qu'exige le
// mode tensor.
var splitTensorDenyArch = map[string]bool{
	"grok": true, "mpt": true, "plamo2": true, "minicpm3": true, "gemma3n": true, "mamba": true, "mamba2": true,
	"jamba": true, "falcon-h1": true, "olmo2": true, "olmoe": true, "deepseek2": true, "deepseek32": true,
	"hy_v4": true, "dots3note": true, "glm-dsa": true, "bitnet": true, "t5": true, "nemotron_h": true,
	"nemotron_h_moe": true, "granitehybrid": true, "minimax-01": true, "minimax-m2": true, "minimax-m3": true,
	"mistral4": true, "kimi-linear": true, "bailingmoe3": true, "kimi-k3": true, "glm5-next": true,
	"qwen3tts": true, "qwen4exp": true,
}

// splitArgEnv : les variables qui décident de ce que SPLIT_MODE règlerait.
var splitArgEnv = []string{"LLAMA_ARG_FLASH_ATTN", "LLAMA_ARG_BACKEND_SAMPLING", "LLAMA_ARG_TENSOR_SPLIT"}

// splitModeKey lit SPLIT_MODE : "" (clé absente, off ou layer — le défaut du
// moteur, rien à poser), "tensor", ou "?" si illisible.
func splitModeKey(cfg map[string]string) string {
	switch v := strings.ToLower(strings.TrimSpace(cfg["SPLIT_MODE"])); v {
	case "", "off", "layer", "non", "no", "0":
		return ""
	case "tensor":
		return v
	}
	return "?"
}

// helpSupportsSplitTensor : la liste LITTÉRALE des valeurs de -sm. Le mot
// « tensor » seul est partout dans l'aide (--tensor-split, --override-tensor) :
// s'y fier ferait passer -sm tensor à un moteur qui le refuse au démarrage.
func helpSupportsSplitTensor(help string) bool {
	return strings.Contains(help, "{none,layer,row,tensor}")
}

// probeSplit : seulement si SPLIT_MODE=tensor et que le moteur le propose —
// sans la clé, rien n'est lu ni lancé. --list-devices tourne dans
// l'environnement du vrai lancement (bibliothèques, CUDA_VISIBLE_DEVICES et
// PCI_BUS_ID déjà posés par cmdServe) : même ordre de cartes que le moteur.
func probeSplit(cfg map[string]string, bin string, si *serveSysInfo) {
	if splitModeKey(cfg) != "tensor" || !helpSupportsSplitTensor(si.Help) {
		return
	}
	if si.ArgEnv == nil {
		si.ArgEnv = map[string]string{}
	}
	for _, k := range splitArgEnv {
		if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
			si.ArgEnv[k] = v
		}
	}
	if si.UserEnv == nil {
		si.UserEnv = map[string]string{}
	}
	if v := os.Getenv("GGML_CUDA_ALLREDUCE"); v != "" {
		si.UserEnv["GGML_CUDA_ALLREDUCE"] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := hideCmd(exec.CommandContext(ctx, bin, "--list-devices")).CombinedOutput()
	if err != nil {
		return // sortie tronquée (carte pleine, moteur planté) : on ne s'y fie pas
	}
	for _, d := range parseListDevices(string(out)) {
		id, _ := d["id"].(string)
		total, _ := d["total_mib"].(int)
		si.SplitDevs = append(si.SplitDevs, splitDev{ID: id, TotalMiB: int64(total)})
	}
}

// splitPlan décide de SPLIT_MODE=tensor pour ce lancement. Fonction pure.
//
//	on       : accepté ; la ligne gagne -sm tensor et ses compagnons
//	why      : pourquoi refusé (vide = clé absente)
//	devs     : les cartes du découpage, dans l'ordre du moteur
//	needMiB  : la VRAM estimée (poids + cache de CTX jetons)
func splitPlan(cfg map[string]string, extra []string, si serveSysInfo) (on bool, why string, devs []splitDev, needMiB int64) {
	switch splitModeKey(cfg) {
	case "":
		return false, "", nil, 0
	case "?":
		return false, "SPLIT_MODE=" + strings.TrimSpace(cfg["SPLIT_MODE"]) + " illisible (tensor ou off)", nil, 0
	}
	refuse := func(why string) (bool, string, []splitDev, int64) { return false, why, nil, 0 }
	if isExternalConfig(cfg) {
		return refuse("preset externe")
	}
	if !helpSupportsSplitTensor(si.Help) {
		return refuse("ce moteur ne propose pas -sm tensor")
	}
	if sm := flagValue(extra, "-sm", "--split-mode"); sm != "" {
		return refuse("-sm " + sm + " déjà dans EXTRA_ARGS")
	}
	if si.ArgEnv["LLAMA_ARG_SPLIT_MODE"] != "" {
		return refuse("LLAMA_ARG_SPLIT_MODE déjà posée")
	}
	fa := flagValue(extra, "-fa", "--flash-attn")
	if fa == "" {
		fa = si.ArgEnv["LLAMA_ARG_FLASH_ATTN"]
	}
	switch strings.ToLower(strings.TrimSpace(fa)) {
	case "off", "disabled", "false", "0":
		return refuse("Flash Attention coupée (-fa " + fa + ") : le mode tensor l'exige")
	}
	if hasAnyFlag(extra, "-bs", "--backend-sampling") || envTruthy(si.ArgEnv["LLAMA_ARG_BACKEND_SAMPLING"]) {
		return refuse("échantillonnage sur GPU (--backend-sampling) non pris en charge en mode tensor")
	}
	k, v, _ := effectiveKVTypes(cfg, extra, si.ArgEnv)
	if !splitKVOK(k) || !splitKVOK(v) {
		return refuse(fmt.Sprintf("cache KV %s/%s : le mode tensor n'accepte que f16, bf16 ou f32 "+
			"(jamais réécrit d'office : la VRAM changerait)", kvName(k), kvName(v)))
	}
	if r := tensorOverride(extra, si.ArgEnv); r != "" {
		return refuse(r + " : le mode tensor met tout sur les cartes")
	}
	if !kvOffloaded(extra, si.ArgEnv) {
		return refuse("cache KV sur CPU")
	}
	switch {
	case si.GGUF == nil || si.ModelBytes <= 0:
		return refuse("métadonnées ou taille du modèle illisibles")
	case splitTensorDenyArch[strings.ToLower(si.GGUF.Arch)]:
		return refuse("architecture " + si.GGUF.Arch + " exclue du mode tensor")
	}
	if _, why := splitNGL(cfg, extra, si.GGUF.BlockCount); why != "" {
		return refuse(why)
	}
	ctxS, ok := ctxFixed(cfg, extra)
	if !ok {
		return refuse("contexte « " + ctxS + " » non chiffré : pas de --fit en mode tensor, donne un CTX explicite")
	}
	if sideSlotOn(cfg) {
		return refuse("SIDE_SLOT : cache doublé, sans --fit pour rattraper")
	}
	if specMode(cfg) != "off" || specUserSet(extra, si.ArgEnv) {
		return refuse("décodage spéculatif : brouillon et vérification par lots non validés en mode tensor")
	}
	devs, why = splitDevices(extra, si)
	if why != "" {
		return refuse(why)
	}
	// VRAM : poids + cache de CTX jetons (et état récurrent) contre 90 % des
	// cartes, la marge de SIDE_SLOT. Les tampons de calcul n'y sont pas : c'est
	// un minimum, pas une promesse — le journal dira le reste.
	ctx, _ := strconv.Atoi(ctxS)
	perTok, rec := ggufStateBytes(*si.GGUF, k, v)
	if perTok <= 0 {
		return refuse("taille du cache KV inconnue pour ce modèle")
	}
	var total int64
	for _, d := range devs {
		total += d.TotalMiB
	}
	mm := int64(0)
	if si.MMProj != "" {
		if st, err := os.Stat(si.MMProj); err == nil {
			mm = st.Size()
		}
	}
	need := float64(si.ModelBytes+mm) + perTok*float64(ctx) + float64(rec)
	needMiB = int64(math.Ceil(need / (1 << 20)))
	if need > 0.9*float64(total)*(1<<20) {
		return refuse(fmt.Sprintf("VRAM insuffisante sans --fit : ~%d Mio (poids + cache de %d jetons) pour %d Mio "+
			"sur %d cartes — CTX n'est jamais réduit d'office", needMiB, ctx, total, len(devs)))
	}
	return true, "", devs, needMiB
}

func splitKVOK(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "", "f16", "bf16", "f32":
		return true
	}
	return false
}

func kvName(t string) string {
	if t == "" {
		return "f16"
	}
	return t
}

// splitNGL : les couches GPU en mode tensor, où tout doit être sur les cartes.
// Un -ngl d'EXTRA_ARGS reste celui de l'utilisateur (rien à ajouter) s'il dit
// « tout » ; sinon NGL est traduit en « -ngl all » (999, auto et la clé absente
// visent déjà le tout-GPU, mais auto ne fait rien sans --fit). Une valeur qui
// laisse des couches au CPU est refusée.
func splitNGL(cfg map[string]string, extra []string, blocks int) (args []string, why string) {
	all := func(v string) bool {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "all" || v == "999" {
			return true
		}
		n, err := strconv.Atoi(v)
		return err == nil && blocks > 0 && n >= blocks+1
	}
	if v := flagValue(extra, "-ngl", "--n-gpu-layers", "--gpu-layers"); v != "" {
		if all(v) {
			return nil, ""
		}
		return nil, "-ngl " + v + " dans EXTRA_ARGS : le mode tensor veut toutes les couches sur les cartes (all)"
	}
	switch v := strings.TrimSpace(cfg["NGL"]); {
	case v == "" || strings.EqualFold(v, "auto") || all(v):
		return []string{"-ngl", "all"}, ""
	default:
		return nil, "NGL=" + v + " : le mode tensor veut toutes les couches sur les cartes (all)"
	}
}

// splitDevices : les cartes du découpage. --device (EXTRA_ARGS ou variable)
// choisit lesquelles, dans son ordre ; sinon toutes celles que liste le
// moteur. Toutes CUDA (l'all-reduce n'existe qu'entre cartes CUDA), deux au
// moins.
func splitDevices(extra []string, si serveSysInfo) ([]splitDev, string) {
	if len(si.SplitDevs) == 0 {
		return nil, "cartes inconnues (--list-devices n'a rien répondu)"
	}
	devs := si.SplitDevs
	sel := flagValue(extra, "-dev", "--device")
	if sel == "" {
		sel = si.ArgEnv["LLAMA_ARG_DEVICE"]
	}
	if sel = strings.TrimSpace(sel); sel != "" {
		devs = nil
		for _, name := range strings.Split(sel, ",") {
			name = strings.TrimSpace(name)
			found := false
			for _, d := range si.SplitDevs {
				if strings.EqualFold(d.ID, name) {
					devs, found = append(devs, d), true
					break
				}
			}
			if !found {
				return nil, "--device " + name + " inconnu de ce moteur"
			}
		}
	}
	for _, d := range devs {
		if !strings.HasPrefix(strings.ToUpper(d.ID), "CUDA") {
			return nil, d.ID + " n'est pas une carte CUDA"
		}
	}
	if len(devs) < 2 {
		return nil, "une seule carte"
	}
	return devs, ""
}

// splitTS : la répartition par défaut, au prorata de la VRAM TOTALE de chaque
// carte (la libre varie avec ce qui tourne encore au lancement), en pour cent.
func splitTS(devs []splitDev) string {
	var total int64
	for _, d := range devs {
		total += d.TotalMiB
	}
	if total <= 0 {
		return ""
	}
	parts := make([]string, len(devs))
	for i, d := range devs {
		parts[i] = strconv.FormatInt(int64(math.Round(100*float64(d.TotalMiB)/float64(total))), 10)
	}
	return strings.Join(parts, ",")
}

// splitTensorArgs : ce que SPLIT_MODE=tensor ajoute à la ligne, à la place du -ngl
// habituel (nglOut), l'environnement et les notes. on=false : rien, et la
// ligne reste celle d'avant à l'octet près.
func splitTensorArgs(cfg map[string]string, extra []string, si serveSysInfo) (on bool, args, ngl []string, env map[string]string, notes []string) {
	ok, why, devs, needMiB := splitPlan(cfg, extra, si)
	if !ok {
		if why != "" {
			notes = append(notes, "SPLIT_MODE=tensor refusé ("+why+") : découpe par couches, comme sans la clé")
		}
		return false, nil, nil, nil, notes
	}
	ngl, _ = splitNGL(cfg, extra, si.GGUF.BlockCount)
	args = []string{"-sm", "tensor"}
	ts := ""
	if !hasAnyFlag(extra, "-ts", "--tensor-split") && si.ArgEnv["LLAMA_ARG_TENSOR_SPLIT"] == "" {
		if ts = splitTS(devs); ts != "" {
			args = append(args, "-ts", ts)
		}
	}
	// llama.cpp active de lui-même Flash Attention en mode tensor ; l'écrire
	// le rend lisible dans le journal. Jamais en double d'un -fa de l'utilisateur.
	if !hasAnyFlag(extra, "-fa", "--flash-attn") && si.ArgEnv["LLAMA_ARG_FLASH_ATTN"] == "" {
		args = append(args, "-fa", "on")
	}
	env = map[string]string{}
	note := fmt.Sprintf("SPLIT_MODE=tensor (expérimental) : -sm tensor sur %d cartes", len(devs))
	if ts != "" {
		note += ", -ts " + ts + " (VRAM totale)"
	}
	note += fmt.Sprintf(", ~%d Mio estimés sans --fit ; décodage plus rapide, prefill plus lent — compare le temps par tour", needMiB)
	switch ar := strings.ToLower(strings.TrimSpace(si.UserEnv["GGML_CUDA_ALLREDUCE"])); ar {
	case "":
		env["GGML_CUDA_ALLREDUCE"] = "internal"
		note += " ; GGML_CUDA_ALLREDUCE=internal : sommes dans le type natif (NCCL les compresserait en BF16, avec perte). " +
			"« NCCL not compiled in » ou « NCCL is unavailable » au journal : sans effet ici. GeForce sans P2P : les " +
			"sommes passent par l'hôte ; GGML_CUDA_P2P=1 à poser soi-même si le pilote le permet"
	case "nccl":
		note += " ; avertissement : GGML_CUDA_ALLREDUCE=nccl (environnement) — NCCL compresse en BF16 les grosses " +
			"réductions du prefill : calcul avec perte. « NCCL not compiled in » au journal = repli sur la réduction interne"
	default:
		note += " ; GGML_CUDA_ALLREDUCE=" + ar + " (environnement) gardé"
	}
	if si.GGUF.Hybrid {
		note += " ; modèle hybride : état récurrent et points de reprise répartis entre cartes, non validés en amont"
	}
	notes = append(notes, note)
	return true, args, ngl, env, notes
}
