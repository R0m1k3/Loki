package loki

// backend_strata.go : deuxième moteur de Loki, Strata (github.com/Niko1221/Strata,
// MIT), spécialisé dans la famille Qwen3.8-Flash-Next (MoE de 125 B) : il
// répartit les experts entre VRAM, RAM et disque pour faire tenir le modèle sur
// une carte de joueur.
//
// Repris d'AJEAN (backend_moe.go, « AJEAN MoE 1.0 ») : le moteur est le paquet
// FIGÉ publié par AJEAN sur sa release GitHub moe-v1.0 — sources de Strata et
// binaire Linux CUDA 12 compilé et validé par l'auteur d'AJEAN (avec ses
// modifications : experts de la carte d'appoint gardés hors RAM). Les deux
// fichiers sont vérifiés par SHA-256 avant usage.
//
// Le choix du moteur se fait par PRESET : ENGINE=strata. Sans la clé (ou
// ENGINE=llamacpp), rien ne change, llama-server reste le moteur.
//
// Déroulé :
//  1. installation (tâche « strata », même suivi que llama.cpp) : télécharge le
//     paquet figé, puis lance l'installeur de Strata en mode automatique en lui
//     imposant le binaire du paquet (--prebuilt) ; l'installeur télécharge le
//     modèle depuis Hugging Face à des révisions figées et le prépare ;
//  2. Loki écrit un preset ENGINE=strata et l'active ;
//  3. `loki serve` lance le serveur de Strata au lieu de llama-server
//     (serveStrata), avec les réglages validés par AJEAN.
//
// Le serveur de Strata expose les mêmes points d'entrée que llama-server
// (/health, /props, /slots, /metrics, /v1/*) : le reste de Loki le voit comme un
// moteur local ordinaire.

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"os/exec"
)

// Paquet figé (release moe-v1.0 d'AJEAN). Le moteur est compilé en CUDA 12.8,
// mode portable AVX2, sm_75/80/86/89/120 : RTX 20 à 50. Un moteur installé d'un
// autre paquet est remplacé au lancement (strataEnsureEngine).
const (
	strataVersion     = "1.0"
	strataReleaseBase = "https://github.com/nathaninline/ajean/releases/download/moe-v" + strataVersion + "/"
	strataSrcAsset    = "ajean-moe-src-" + strataVersion + ".tar.gz"
	strataEngineAsset = "ajean-moe-engine-" + strataVersion + "-linux-x64-cuda12.zip"
	// strataEngineLocal : le nom sous lequel l'installeur attend le moteur (CUDA12_ASSET).
	strataEngineLocal = "strata-linux-x64-cuda12.zip"
)

// strataAssetSHA : empreintes des fichiers de la release, vérifiées avant usage.
var strataAssetSHA = map[string]string{
	strataSrcAsset:    "5747f69426973d957e4f9894a8e3333c6ef0d6ac8565e26ddb8d36f52211cb3b",
	strataEngineAsset: "b5851bc138ab32ca3ffefc16bf91c8f557e4d503503473a22f2b6c9e9b8fd34c",
}

// Valeurs de la clé ENGINE d'un preset.
const (
	engineLlamacpp = "llamacpp"
	engineStrata   = "strata"
)

// engineOf : le moteur d'une configuration. « moe » est accepté pour un preset
// recopié depuis AJEAN.
func engineOf(cfg map[string]string) string {
	switch strings.ToLower(strings.TrimSpace(cfg["ENGINE"])) {
	case engineStrata, "moe":
		return engineStrata
	}
	return engineLlamacpp
}

// isStrataConfig : la configuration fait tourner Strata.
func isStrataConfig(cfg map[string]string) bool { return engineOf(cfg) == engineStrata }

// strataActive : le preset actif fait tourner Strata.
func strataActive() bool { return isStrataConfig(ReadConfig()) }

// strataFamily / strataQuant : ce que l'installeur de Strata sait installer,
// limité à ce qu'on propose en un clic.
type strataFamily struct {
	ID     string   `json:"id"`    // --family
	Label  string   `json:"label"` // affiché
	About  string   `json:"about"` // une ligne
	Quants []string `json:"quants"`
}

type strataQuant struct {
	ID         string  `json:"id"`          // --model
	About      string  `json:"about"`       // une ligne
	DownloadGB float64 `json:"download_gb"` // modèle + tête MTP + encodeur vision
	ArenaGB    float64 `json:"arena_gb"`    // experts à garder en RAM (ou en mmap)
}

// Valeurs reprises de la table MODELS de l'installeur.
var strataFamilies = []strataFamily{
	{ID: "swift", Label: "Swift 1.5", About: "réfléchit moins, répond plus vite", Quants: []string{"IQ2_XS", "IQ3_XXS"}},
	{ID: "qwen", Label: "Classique", About: "le Qwen3.8-Flash-Next original", Quants: []string{"Q2_0", "IQ2_XS", "IQ3_XXS", "IQ3_S"}},
}

var strataQuants = map[string]strataQuant{
	"Q2_0":    {ID: "Q2_0", About: "le plus rapide", DownloadGB: 66.4, ArenaGB: 34.0},
	"IQ2_XS":  {ID: "IQ2_XS", About: "un peu plus fidèle, presque aussi rapide", DownloadGB: 68.0, ArenaGB: 35.5},
	"IQ3_XXS": {ID: "IQ3_XXS", About: "99 % de la qualité du modèle complet", DownloadGB: 75.8, ArenaGB: 42.9},
	"IQ3_S":   {ID: "IQ3_S", About: "la meilleure qualité, le plus lent", DownloadGB: 83.6, ArenaGB: 50.3},
}

// strataLowRAMHeadroomGB : RAM à laisser à côté des experts (OS, moteur,
// serveur, et ici l'UI de Loki). En dessous, les experts passent en mmap.
const strataLowRAMHeadroomGB = 10

// strataDropHeadroomGB : RAM à laisser à côté des experts verrouillés quand la
// carte d'aide garde les siens hors RAM (mesuré par AJEAN : 46 Go de RAM,
// 34,7 Gio d'experts verrouillés, ~2,3 Go encore libres).
const strataDropHeadroomGB = 7

// strataHelperExpertsGB : ce que la carte d'aide garde d'experts (sa VRAM moins
// le contexte CUDA, l'encodeur d'images et la marge : 5,3 Gio sur une 3070 8 Go).
func strataHelperExpertsGB(vramGB float64) float64 { return max(vramGB-2.5, 0) }

// Où vivent les experts que les cartes ne gardent pas (clé STRATA_EXPERTS du
// preset : auto, ram ou disk), et le mode effectivement lancé :
//
//	drop   experts de la carte d'appoint hors RAM, le reste verrouillé en RAM
//	       (le plus rapide mesuré par AJEAN) — demande un experts.bin existant
//	ram    tous les experts chargés et verrouillés en RAM
//	arena  experts.bin existant mappé tel quel (cache système)
//	disk   experts lus dans les fichiers du modèle (--mmap-experts) : rien
//	       n'est chargé au démarrage, le cache système garde ce que la RAM
//	       permet. Un peu plus lent, mais jamais tué faute de RAM.
//
// Vécu (2026-10) : 62 Go de RAM dont 18 déjà pris par le reste du serveur, le
// mode « hors RAM » choisi sur la RAM TOTALE, et un premier lancement qui
// écrivait experts.bin en chargeant ~41 Go en mémoire anonyme : le noyau a tué
// le moteur. D'où : décision sur la RAM DISPONIBLE au lancement, et plus jamais
// de premier lancement qui écrit experts.bin.
const (
	strataExpertsAuto = "auto"
	strataExpertsRAM  = "ram"
	strataExpertsDisk = "disk"
)

// memAvailableGB : la RAM réellement disponible (MemAvailable : libre + cache
// récupérable), en Go. Hors Linux ou illisible : la RAM totale.
func memAvailableGB() float64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return totalRAMGB()
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "MemAvailable:"); ok {
			f := strings.Fields(v)
			if len(f) > 0 {
				if kb, err := strconv.ParseFloat(f[0], 64); err == nil {
					return kb * 1024 / 1e9
				}
			}
		}
	}
	return totalRAMGB()
}

// strataArenaGB : la taille des experts du quant d'un preset (lue dans le nom
// de sa config d'installeur : strata-swift-iq3_xxs.json → IQ3_XXS). 0 = inconnu.
func strataArenaGB(cfg map[string]string) float64 {
	tag := strings.ToUpper(strataConfigTag(strings.TrimSpace(cfg["STRATA_CONFIG"])))
	tag = strings.TrimPrefix(tag, "SWIFT-")
	if q, ok := strataQuants[tag]; ok {
		return q.ArenaGB
	}
	return 0
}

// strataPackHasExpertsBin : experts.bin est déjà dans le pack (écrit par un
// lancement précédent ou par l'installeur en mode peu de RAM).
func strataPackHasExpertsBin(args []string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--pack" {
			if _, err := os.Stat(filepath.Join(args[i+1], "experts.bin")); err == nil {
				return true
			}
		}
	}
	return false
}

// strataResolveExperts : le mode lancé, d'après le choix du preset et la RAM
// disponible (availGB) ; helperGB = VRAM de la carte d'appoint (0 sans).
// Pure : la décision se teste en table.
func strataResolveExperts(cfg map[string]string, args []string, availGB, helperGB float64) string {
	binReady := strataPackHasExpertsBin(args) && strataEngineInstalled() == strataEngineAsset
	dropReady := binReady && strataHelperOn(cfg) && cfg["STRATA_DROP"] != "0"
	arena := strataArenaGB(cfg)
	switch strings.ToLower(strings.TrimSpace(cfg["STRATA_EXPERTS"])) {
	case strataExpertsDisk:
		if binReady {
			return "arena"
		}
		return "disk"
	case strataExpertsRAM:
		if dropReady {
			return "drop"
		}
		return "ram"
	}
	if arena <= 0 { // quant inconnu : on ne parie pas sur la RAM
		return "disk"
	}
	if dropReady && availGB >= arena-strataHelperExpertsGB(helperGB)+strataDropHeadroomGB {
		return "drop"
	}
	if availGB >= arena+strataLowRAMHeadroomGB {
		return "ram"
	}
	if binReady {
		return "arena"
	}
	return "disk"
}

// strataExpertsLabel : le mode en clair, pour l'interface et le journal.
func strataExpertsLabel(mode string) string {
	switch mode {
	case "drop":
		return "verrouillés en RAM, hors ceux de la carte d'appoint"
	case "ram":
		return "chargés et verrouillés en RAM"
	case "arena":
		return "experts.bin mappé (cache système)"
	case "disk":
		return "lus depuis le disque (mmap)"
	}
	return mode
}

// strataMaxCtx : contexte natif maximal de Qwen3.8 Flash Next.
const strataMaxCtx = 262144

func strataHome() string    { return filepath.Join(LokiHome(), "strata") }
func strataSrcDir() string  { return filepath.Join(strataHome(), "ajean-moe-"+strataVersion) }
func strataDataDir() string { return filepath.Join(strataHome(), "data") }
func strataDLDir() string   { return filepath.Join(strataHome(), "dl") }

// ---------------------------------------------------------------------------
// Matériel et recommandation
// ---------------------------------------------------------------------------

// strataGPU : une carte NVIDIA vue par nvidia-smi (index = numérotation
// nvidia-smi, alignée sur CUDA avec CUDA_DEVICE_ORDER=PCI_BUS_ID).
type strataGPU struct {
	Index  int     `json:"index"`
	Name   string  `json:"name"`
	VRAMGB float64 `json:"vram_gb"`
	Arch   int     `json:"arch"` // compute capability ×10 (8.6 → 86)
}

// strataEnv : ce que l'écran d'installation doit savoir de la machine.
type strataEnv struct {
	Supported  bool        `json:"supported"`
	Reason     string      `json:"reason,omitempty"` // pourquoi pas, en clair
	GPUs       []strataGPU `json:"gpus"`
	Main       int         `json:"main"`   // index nvidia-smi de la carte principale
	Helper     int         `json:"helper"` // index de la carte d'aide, -1 sans
	RAMGB      float64     `json:"ram_gb"`
	AvailGB    float64     `json:"avail_gb"` // RAM disponible (+ celle du moteur Strata en marche)
	DiskFreeGB float64     `json:"disk_free_gb"`
	Python     string      `json:"python,omitempty"`
}

// strataMinArch : la plus ancienne carte pour laquelle le moteur est compilé (RTX 20).
const strataMinArch = 75

func strataDetect() strataEnv {
	env := strataEnv{Main: -1, Helper: -1, RAMGB: totalRAMGB()}
	env.AvailGB = min(memAvailableGB()+strataEngineRSSGB(), env.RAMGB)
	// strataHome() et pas LokiHome() : /data/strata peut être un disque à part
	// (SSD dédié monté dans le conteneur), c'est sa place libre qui compte.
	if f := diskFree(strataHome()); f > 0 {
		env.DiskFreeGB = float64(f) / 1e9
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		env.Reason = "Strata n'est proposé que sous Linux (x86_64)"
		return env
	}
	gpus, err := detectGPUs()
	if err != nil || len(gpus) == 0 {
		why := "aucune carte détectée"
		if err != nil {
			why = err.Error()
		}
		env.Reason = "Strata demande une carte NVIDIA (" + why + ")"
		return env
	}
	for _, g := range gpus {
		mib, _ := strconv.ParseFloat(g.MemTotal, 64)
		cc, _ := strconv.ParseFloat(g.Cap, 64)
		env.GPUs = append(env.GPUs, strataGPU{Index: g.Index, Name: g.Name, VRAMGB: mib / 1024, Arch: int(cc*10 + 0.5)})
	}
	// Carte principale = la plus grosse VRAM (à égalité : la plus récente) ; la
	// suivante, si elle est assez récente, calcule une part des experts.
	cards := append([]strataGPU(nil), env.GPUs...)
	sort.SliceStable(cards, func(i, j int) bool {
		if cards[i].VRAMGB != cards[j].VRAMGB {
			return cards[i].VRAMGB > cards[j].VRAMGB
		}
		return cards[i].Arch > cards[j].Arch
	})
	if cards[0].Arch < strataMinArch {
		env.Reason = fmt.Sprintf("la carte %s est trop ancienne pour Strata (RTX 20 ou plus récente)", cards[0].Name)
		return env
	}
	if cards[0].VRAMGB < 7.5 {
		env.Reason = fmt.Sprintf("la carte %s n'a que %.0f Go de VRAM (8 Go minimum)", cards[0].Name, cards[0].VRAMGB)
		return env
	}
	env.Main = cards[0].Index
	if len(cards) > 1 && cards[1].Arch >= strataMinArch && cards[1].VRAMGB >= 5.5 {
		env.Helper = cards[1].Index
	}
	py, perr := strataCheckPython()
	env.Python = py
	if perr != nil {
		env.Reason = perr.Error()
		return env
	}
	env.Supported = true
	return env
}

var rePyVersion = regexp.MustCompile(`Python (\d+)\.(\d+)`)

// strataCheckPython : l'installeur de Strata est en Python (3.10 ou plus
// récent) et crée son propre environnement (module venv). L'image Docker de
// Loki les embarque ; hors image, il faut python3 et python3-venv.
func strataCheckPython() (string, error) {
	out, err := hideCmd(exec.Command("python3", "--version")).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("Python 3 est introuvable : installe-le (apt install python3 python3-venv)")
	}
	m := rePyVersion.FindStringSubmatch(string(out))
	if m == nil {
		return "", fmt.Errorf("version de Python illisible : %s", strings.TrimSpace(string(out)))
	}
	maj, _ := strconv.Atoi(m[1])
	mnr, _ := strconv.Atoi(m[2])
	ver := m[1] + "." + m[2]
	if maj < 3 || (maj == 3 && mnr < 10) {
		return ver, fmt.Errorf("Python %s est trop ancien : il faut Python 3.10 ou plus récent", ver)
	}
	if e := hideCmd(exec.Command("python3", "-c", "import venv, ensurepip")).Run(); e != nil {
		return ver, fmt.Errorf("le module venv de Python manque : apt install python%s-venv", ver)
	}
	return ver, nil
}

// strataTag : le nom que l'installeur donne aux dossiers d'un choix
// (models/<tag>, packs/<tag en minuscules>) : « swift-IQ3_XXS », « IQ3_XXS ».
func strataTag(family, quant string) string {
	if family == "swift" {
		return "swift-" + quant
	}
	return quant
}

func strataDirGB(dir string) float64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, e := d.Info(); e == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return float64(total) / 1e9
}

// strataHaveGB : ce qui est déjà sur le disque pour CE choix (installation
// reprise ou réinstallation) : ses fichiers de modèle, son experts.bin, et la
// tête MTP commune à tous les choix. Autant de téléchargement en moins.
func strataHaveGB(family, quant string) float64 {
	tag := strataTag(family, quant)
	return strataDirGB(filepath.Join(strataDataDir(), "models", tag)) +
		strataDirGB(filepath.Join(strataDataDir(), "packs", strings.ToLower(tag))) +
		strataDirGB(filepath.Join(strataDataDir(), "mtp"))
}

// strataEngineRSSGB : la RAM tenue par le moteur Strata qui tourne (serveur et
// moteur, arbre du service), en Go. Ajoutée à la RAM disponible quand on prédit
// pour CETTE machine : sinon le moteur en marche ferait croire qu'il ne tient pas.
func strataEngineRSSGB() float64 {
	if !strataActive() {
		return 0
	}
	root := readServicePID()
	if root <= 0 {
		return 0
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	parent := map[int]int{}
	rss := map[int]float64{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "PPid:"); ok {
				parent[pid], _ = strconv.Atoi(strings.TrimSpace(v))
			} else if v, ok := strings.CutPrefix(line, "VmRSS:"); ok {
				if f := strings.Fields(v); len(f) > 0 {
					kb, _ := strconv.ParseFloat(f[0], 64)
					rss[pid] = kb * 1024 / 1e9
				}
			}
		}
	}
	var total float64
	for pid, r := range rss {
		for p, n := pid, 0; p > 0 && n < 64; p, n = parent[p], n+1 {
			if p == root {
				total += r
				break
			}
		}
	}
	return total
}

// strataFit : ce qu'un quant demande sur cette machine, et s'il y tient.
type strataFit struct {
	Quant  strataQuant `json:"quant"`
	DiskGB float64     `json:"disk_gb"`
	Mmap   bool        `json:"mmap"` // en mode auto, les experts seraient lus depuis le disque
	Drop   bool        `json:"drop"` // experts de la carte d'aide hors RAM, le reste verrouillé
	OK     bool        `json:"ok"`
	Why    string      `json:"why,omitempty"`
	Preset string      `json:"preset,omitempty"` // id du preset si ce choix est déjà installé
	Active bool        `json:"active,omitempty"` // ... et si c'est le preset actif
	// Details : la config de lancement de ce preset, calculée par la MÊME
	// fonction que le lancement (strataBuildConfig) : ce qui est affiché tourne.
	Details *strataDetails `json:"details,omitempty"`
}

// strataDetails : la config d'un preset installé, en clair pour l'interface.
type strataDetails struct {
	Version    string `json:"version"`
	Ctx        string `json:"ctx"`
	KV         string `json:"kv"`
	KVResident string `json:"kv_resident,omitempty"`
	Spec       string `json:"spec"`
	Prefill    string `json:"prefill"`
	ShortRead  string `json:"short_read"`
	CacheRoot  string `json:"cache_root"`
	Mmap       bool   `json:"mmap"`
	Drop       bool   `json:"drop"`
	// Experts : le choix du preset (auto, ram, disk) ; Mode : le mode lancé
	// (drop, ram, arena, disk) — celui du dernier lancement pour le preset
	// actif, sinon celui que la RAM disponible donnerait maintenant.
	Experts   string `json:"experts"`
	Mode      string `json:"mode"`
	ModeLabel string `json:"mode_label"`
	MainGPU   string `json:"main_gpu"`
	HelperGPU string `json:"helper_gpu,omitempty"`
	VisionGPU string `json:"vision_gpu,omitempty"`
	// pour les réglages : ce que la machine permet et l'état actuel
	HasHelper bool `json:"has_helper"` // une carte d'aide est disponible
	HelperOn  bool `json:"helper_on"`
	Vision    bool `json:"vision"`
}

// strataDetailsFor lit un preset installé et calcule sa config de lancement.
func strataDetailsFor(presetID string, env strataEnv) *strataDetails {
	content, err := ReadPreset(presetID)
	if err != nil {
		return nil
	}
	pc := parseEnv(content)
	raw, err := os.ReadFile(pc["STRATA_CONFIG"])
	if err != nil {
		return nil
	}
	var base map[string]any
	if json.Unmarshal(raw, &base) != nil {
		return nil
	}
	mode := strataResolveExperts(pc, strataArgList(base), env.AvailGB, strataHelperVRAM(pc, env.GPUs))
	if active, _ := strataPresetActive(presetID); active {
		if m := getStr(bkState, strataLaunchModeKey); m != "" {
			mode = m
		}
	}
	out, err := strataBuildConfig(base, pc, "", mode)
	if err != nil {
		return nil
	}
	args := strataArgList(out)
	val := func(flag string) string {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flag {
				return args[i+1]
			}
		}
		return ""
	}
	gpuName := func(idx string) string {
		for _, g := range env.GPUs {
			if strconv.Itoa(g.Index) == strings.TrimSpace(idx) {
				return fmt.Sprintf("%s (%.0f Go)", strings.TrimPrefix(g.Name, "NVIDIA GeForce "), g.VRAMGB)
			}
		}
		return ""
	}
	d := &strataDetails{
		Version: strataVersion, Ctx: val("--max-context"), KV: val("--kv"), KVResident: val("--kv-resident"),
		Spec: val("--spec"), Prefill: val("--prefill"), ShortRead: val("--short-read"),
		CacheRoot: val("--prompt-cache-root"), MainGPU: gpuName(pc["STRATA_MAIN_GPU"]),
	}
	d.Mode, d.ModeLabel = mode, strataExpertsLabel(mode)
	d.Mmap, d.Drop = mode == "disk" || mode == "arena", mode == "drop"
	d.Experts = strings.ToLower(strings.TrimSpace(pc["STRATA_EXPERTS"]))
	if d.Experts != strataExpertsRAM && d.Experts != strataExpertsDisk {
		d.Experts = strataExpertsAuto
	}
	if h := strings.TrimSpace(pc["STRATA_HELPER_GPU"]); h != "" && h != "-1" {
		d.HasHelper = true
		if strataHelperOn(pc) {
			d.HelperOn = true
			d.HelperGPU = gpuName(h)
		}
	}
	d.Vision = pc["STRATA_VISION"] != "0"
	if v, ok := out["vision"].(map[string]any); ok && d.Vision {
		if dev, has := v["cuda_device"]; has {
			d.VisionGPU = gpuName(fmt.Sprint(dev))
		} else {
			d.VisionGPU = d.MainGPU
		}
	}
	return d
}

func strataFitFor(q strataQuant, env strataEnv, haveGB float64) strataFit {
	// Prédiction du mode auto sur la RAM DISPONIBLE (pas la RAM totale) : sous
	// la marge, les experts seront lus depuis le disque. Le mode « hors RAM » de
	// la carte d'appoint demande un experts.bin, qu'une installation neuve n'a pas.
	f := strataFit{Quant: q, Mmap: env.AvailGB < q.ArenaGB+strataLowRAMHeadroomGB}
	f.DiskGB = q.DownloadGB
	// ce qui est déjà là ne se retélécharge pas ; il reste toujours les
	// environnements (Python, bibliothèques CUDA, moteur) : ~10 Go
	f.DiskGB = max(f.DiskGB-haveGB, 10)
	// En mmap, le système garde les experts en cache tant que la RAM le permet ;
	// au-delà, ils sont relus depuis le disque à chaque jeton : trop lent. On
	// demande que la RAM + la VRAM des cartes couvrent au moins les experts.
	vram := 0.0
	for _, g := range env.GPUs {
		if g.Index == env.Main || g.Index == env.Helper {
			vram += g.VRAMGB
		}
	}
	switch {
	case env.RAMGB+vram-12 < q.ArenaGB:
		f.Why = fmt.Sprintf("demande plus de mémoire : %.0f Go de RAM + VRAM, il en faut ~%.0f", env.RAMGB+vram, q.ArenaGB+12)
	case env.DiskFreeGB > 0 && env.DiskFreeGB < f.DiskGB+5:
		f.Why = fmt.Sprintf("pas assez d'espace disque : %.0f Go libres, il en faut ~%.0f", env.DiskFreeGB, f.DiskGB+5)
	default:
		f.OK = true
	}
	return f
}

// strataRecommend : le quant le plus fidèle qui tient, IQ3_XXS de préférence
// (meilleur compromis mesuré) ; IQ3_S seulement si la RAM le loge sans mmap.
func strataRecommend(family string, env strataEnv) string {
	var fam *strataFamily
	for i := range strataFamilies {
		if strataFamilies[i].ID == family {
			fam = &strataFamilies[i]
		}
	}
	if fam == nil {
		return ""
	}
	best := ""
	for _, id := range []string{"IQ3_XXS", "IQ2_XS", "Q2_0"} {
		if !strataHas(fam.Quants, id) {
			continue
		}
		if strataFitFor(strataQuants[id], env, strataHaveGB(family, id)).OK {
			best = id
			break
		}
	}
	if strataHas(fam.Quants, "IQ3_S") {
		if f := strataFitFor(strataQuants["IQ3_S"], env, strataHaveGB(family, "IQ3_S")); f.OK && !f.Mmap {
			best = "IQ3_S"
		}
	}
	return best
}

func strataHas(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Installation
// ---------------------------------------------------------------------------

// strataInstallReq : le choix de l'utilisateur. Le reste est déduit de la machine.
type strataInstallReq struct {
	Family string `json:"family"`
	Quant  string `json:"quant"`
}

// strataPresetName : nom du fichier preset (et donc de son id) pour un choix donné.
func strataPresetName(family, quant string) string {
	label := "Classique"
	if family == "swift" {
		label = "Swift 1.5"
	}
	return "STRATA FLASH NEXT " + strings.ToUpper(label) + " " + quant
}

func strataFamilyLabel(id string) string {
	for _, f := range strataFamilies {
		if f.ID == id {
			if id == "qwen" {
				return "Qwen3.8 Flash Next"
			}
			return "Qwen3.8 Flash Next " + f.Label
		}
	}
	return id
}

// strataFetch télécharge un fichier de la release figée et vérifie son SHA-256.
func strataFetch(asset string) (string, error) {
	want := strataAssetSHA[asset]
	dst := filepath.Join(strataDLDir(), asset)
	if got, err := strataSHA256File(dst); err == nil && got == want {
		lcAppend(asset + " déjà téléchargé")
		return dst, nil
	}
	if err := os.MkdirAll(strataDLDir(), 0o755); err != nil {
		return "", err
	}
	lcPhase("téléchargement de " + asset + "…")
	tmp := dst + ".part"
	if err := downloadWithProgress(strataReleaseBase+asset, tmp, 0, lcAppend); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("téléchargement de %s : %w", asset, err)
	}
	got, err := strataSHA256File(tmp)
	if err != nil {
		return "", err
	}
	if got != want {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("%s : empreinte SHA-256 inattendue (fichier altéré ou incomplet)", asset)
	}
	return dst, os.Rename(tmp, dst)
}

func strataSHA256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func strataCopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// strataRunInstall : le corps de la tâche d'installation.
func strataRunInstall(req strataInstallReq) {
	env := strataDetect()
	if !env.Supported {
		lcFail(fmt.Errorf("%s", env.Reason))
		return
	}
	q, ok := strataQuants[req.Quant]
	if !ok {
		lcFail(fmt.Errorf("quant inconnu : %s", req.Quant))
		return
	}
	fit := strataFitFor(q, env, strataHaveGB(req.Family, q.ID))
	if !fit.OK {
		lcFail(fmt.Errorf("%s ne tient pas sur cette machine : %s", q.ID, fit.Why))
		return
	}

	// 1. le paquet figé : sources (installeur + serveur) et moteur compilé
	src, err := strataFetch(strataSrcAsset)
	if err != nil {
		lcFail(err)
		return
	}
	engine, err := strataFetch(strataEngineAsset)
	if err != nil {
		lcFail(err)
		return
	}
	if _, err := os.Stat(filepath.Join(strataSrcDir(), "setup.py")); err != nil {
		lcPhase("décompression de Strata…")
		if err := extractArchive(src, strataHome()); err != nil {
			lcFail(fmt.Errorf("décompression : %w", err))
			return
		}
	}
	// L'installeur prend le moteur dans un dossier local (--prebuilt) : il le
	// décompresse dans engine-cuda12/ et contrôle qu'il couvre la carte.
	pre := filepath.Join(strataHome(), "prebuilt")
	_ = os.MkdirAll(pre, 0o755)
	if err := strataCopyFile(engine, filepath.Join(pre, strataEngineLocal)); err != nil {
		lcFail(err)
		return
	}

	// 2. l'installeur, sans question : environnement Python, moteur, modèle
	// (Hugging Face, révisions figées), tête MTP, encodeur vision, pack.
	lcPhase(fmt.Sprintf("installation de %s %s (téléchargement de ~%.0f Go)…", strataFamilyLabel(req.Family), q.ID, q.DownloadGB))
	// setup.sh crée l'environnement Python (.venv) puis y relance setup.py :
	// Python 3.10+ et venv sont vérifiés plus haut (strataDetect).
	args := []string{"setup.sh", "--setup", "--yes", "--no-start", "--no-browser",
		"--family", req.Family, "--model", q.ID,
		"--context", "131072", "--vision", "gpu",
		"--cuda", "12", "--prebuilt", pre + string(os.PathSeparator),
		"--data-dir", strataDataDir(),
		"--gpu", strconv.Itoa(env.Main),
		// Le mode « peu de RAM » est décidé et appliqué par Loki (STRATA_MMAP,
		// voir strataBuildConfig) : l'installeur tourne en mode normal, sans
		// réserver sa propre variante (qui ne voit pas un experts.bin présent).
		"--low-ram", "off",
	}
	// XDG_CACHE_HOME : les caches de pip et de Hugging Face (~/.cache, soit
	// /root dans le conteneur, donc l'image Docker — 20 Go sur Unraid) suivent
	// le reste de Strata dans /data/strata.
	extra := "CUDA_DEVICE_ORDER=PCI_BUS_ID\x00PYTHONUNBUFFERED=1\x00XDG_CACHE_HOME=" + filepath.Join(strataHome(), "cache")
	if err := runStepEnv("installation de Strata", strataSrcDir(), extra, "bash", args...); err != nil {
		lcFail(fmt.Errorf("l'installation a échoué : %w (voir le journal)", err))
		return
	}
	// Marque le moteur installé : c'est celui du paquet (strataEnsureEngine).
	_ = os.WriteFile(strataEngineMarker(), []byte(strataEngineAsset+"\n"), 0o644)
	cfgPath, err := strataFindConfig(q.ID)
	if err != nil {
		lcFail(err)
		return
	}

	// 3. le preset Loki, puis bascule dessus
	name := strataPresetName(req.Family, q.ID)
	preset := filepath.Join(presetsDir(), name+".env")
	body := strings.Join([]string{
		"# NAME=" + strataFamilyLabel(req.Family) + " " + q.ID + " (Strata)",
		"ENGINE=" + engineStrata,
		"STRATA_CONFIG=" + cfgPath,
		"STRATA_MAIN_GPU=" + strconv.Itoa(env.Main),
		"STRATA_HELPER_GPU=" + strconv.Itoa(env.Helper),
		"STRATA_VISION=1",
		// experts placés au lancement selon la RAM disponible (strataResolveExperts)
		"STRATA_EXPERTS=" + strataExpertsAuto,
		"STRATA_DROP=1",
		// la meilleure qualité validée par AJEAN : cache KV en fp16 (32K en
		// VRAM, le reste en RAM pour ne pas prendre la place des experts) et 3
		// jetons de brouillon MTP (mesuré meilleur que 4)
		"STRATA_KV=fp16",
		"STRATA_KV_RESIDENT=32768",
		"STRATA_SPEC=3",
		"CTX=131072",
		"REASONING=off",
		"",
	}, "\n")
	if err := os.MkdirAll(presetsDir(), 0o755); err != nil {
		lcFail(err)
		return
	}
	if err := os.WriteFile(preset, []byte(body), 0o644); err != nil {
		lcFail(err)
		return
	}
	lcPhase("activation du modèle (premier chargement : quelques minutes)…")
	if err := SwitchToPreset(preset); err != nil {
		lcFail(fmt.Errorf("modèle installé mais bascule impossible : %w", err))
		return
	}
	lcDone(strataFamilyLabel(req.Family) + " " + q.ID + " installé (Strata)")
}

func strataBool(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// strataFindConfig : la config de lancement que l'installeur a écrite
// (strata-<tag>.json à la racine des sources), celle du quant demandé.
func strataFindConfig(quant string) (string, error) {
	matches, _ := filepath.Glob(filepath.Join(strataSrcDir(), "strata-*.json"))
	q := strings.ToLower(quant)
	for _, m := range matches {
		if strings.Contains(strings.ToLower(filepath.Base(m)), q) {
			return m, nil
		}
	}
	return "", fmt.Errorf("l'installeur n'a pas laissé de configuration pour %s dans %s", quant, strataSrcDir())
}

// ---------------------------------------------------------------------------
// Lancement (loki serve)
// ---------------------------------------------------------------------------

// strataTuning : réglages validés par AJEAN (2026-10-05), ajoutés à la config
// de l'installeur. Chacun a été mesuré en A/B.
//
//   - --prefill 16384     : +15 % de lecture sur les longs prompts (40K) ;
//   - --short-read 150    : les petits ajouts (< 150 jetons) sont lus comme en
//     génération, sans le coût fixe du chemin par blocs : ~40 % plus rapides ;
//   - --prompt-cache-root 256 : point de reprise posé à la fin du prompt système
//     (défaut 2048) : une nouvelle conversation démarre en ~0,3 s au lieu de 4-5 s.
var strataTuning = map[string]string{
	"--prefill":           "16384",
	"--short-read":        "150",
	"--prompt-cache-root": "256",
}

// strataEnvTuning : variables d'environnement du moteur.
//   - STRATA_PLE_BATCH=0 : le PLE batché du prefill plante sur certaines cartes
//     (« native PLE postops launch: illegal memory access ») ;
//   - STRATA_NO_LARGEPAGES=1 : sans, MADV_HUGEPAGE fige le démarrage de longues
//     minutes dans le compactage mémoire du noyau.
var strataEnvTuning = []string{"STRATA_PLE_BATCH=0", "STRATA_NO_LARGEPAGES=1"}

func strataSetArg(args []string, flag, value string) []string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			args[i+1] = value
			return args
		}
	}
	return append(args, flag, value)
}

// strataDropFlag retire un drapeau SANS valeur.
func strataDropFlag(args []string, flag string) []string {
	out := args[:0:0]
	for _, a := range args {
		if a != flag {
			out = append(out, a)
		}
	}
	return out
}

// strataDropFlagValue retire un drapeau ET sa valeur.
func strataDropFlagValue(args []string, flag string) []string {
	out := args[:0:0]
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func strataHasArg(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func strataArgList(cfg map[string]any) []string {
	var args []string
	if list, ok := cfg["args"].([]any); ok {
		for _, a := range list {
			args = append(args, fmt.Sprint(a))
		}
	}
	return args
}

// strataBuildConfig lit la config de l'installeur et y applique les réglages
// du preset, avec les experts placés selon `mode` (strataResolveExperts). Pure
// (testable) : ne lance rien.
func strataBuildConfig(base map[string]any, cfg map[string]string, apiKey, mode string) (map[string]any, error) {
	raw, _ := json.Marshal(base)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if _, ok := out["args"].([]any); !ok {
		return nil, fmt.Errorf("configuration de Strata sans « args »")
	}
	args := strataArgList(out)
	for flag, v := range strataTuning {
		args = strataSetArg(args, flag, v)
	}
	helper := strings.TrimSpace(cfg["STRATA_HELPER_GPU"])
	hasHelper := strataHelperOn(cfg)
	// Deuxième carte en aide : elle garde et CALCULE sa part des experts (le
	// chemin « helper » du moteur), la carte principale reste CUDA0. Les cartes
	// sont posées par CUDA_VISIBLE_DEVICES (serveStrata), PAS par la clé « gpu » :
	// avec deux cartes, le serveur y lirait une répartition des couches
	// (--layer-split), un autre mode que celui mesuré.
	if hasHelper {
		args = strataSetArg(args, "--expert-cache-device1", "auto")
		if !strataHasArg(args, "--remote-expert-opt") {
			args = append(args, "--remote-expert-opt")
		}
		delete(out, "gpu")
		// réserve de VRAM posée par l'installeur : absente de la config mesurée,
		// elle retire du cache d'experts à la principale
		args = strataDropFlagValue(args, "--vram-reserve-mib")
		// l'encodeur d'images sur la carte d'aide : la principale garde toute sa
		// VRAM pour les experts (mesuré : -1,5 % au lieu de -3 % sur la principale)
		if v, ok := out["vision"].(map[string]any); ok {
			if n, err := strconv.Atoi(helper); err == nil {
				v["cuda_device"] = n
			}
		}
	}
	// Experts (strataResolveExperts) : les drapeaux de l'installeur
	// (--mmap-experts / --resident-experts) sont remplacés par ceux du mode.
	env := map[string]any{}
	if e, ok := out["env"].(map[string]any); ok {
		env = e
	}
	args = strataDropFlag(strataDropFlag(args, "--mmap-experts"), "--resident-experts")
	delete(env, "STRATA_ARENA_MMAP")
	delete(env, "STRATA_REMOTE_DROP")
	switch mode {
	case "drop":
		if !hasHelper {
			break // sans carte d'appoint, c'est le mode « ram »
		}
		// Les experts de la carte d'aide hors RAM, le reste verrouillé en RAM :
		// la principale relit aussi par PCIe une part de ce qui lui manque (part
		// automatique) ; la réserve de la carte d'aide est figée.
		args = strataDropFlagValue(args, "--pcie-frac")
		env["STRATA_REMOTE_DROP"] = "1"
		// ses experts sont relus de sa VRAM à la lecture d'un prompt, par les fils
		// de copie : 8 fils et 32 tampons (le moteur en prendrait 32 et 128)
		env["STRATA_STAGER_THREADS"] = "8"
		env["STRATA_STAGER_RING"] = "32"
	case "arena":
		// experts.bin mappé tel quel (variante mesurée par AJEAN), sans part PCIe :
		// le mappage n'en donne pas l'adresse GPU.
		args = strataSetArg(args, "--pcie-frac", "0")
		env["STRATA_ARENA_MMAP"] = "1"
	case "disk":
		// Lus dans les fichiers du modèle par le cache système : rien n'est
		// chargé au démarrage. Pas de part PCIe non plus (mémoire non épinglée).
		args = append(args, "--mmap-experts")
		args = strataSetArg(args, "--pcie-frac", "0")
	}
	for _, kv := range strataEnvTuning {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	out["env"] = env
	if ctx := strings.TrimSpace(cfg["CTX"]); ctx != "" {
		args = strataSetArg(args, "--max-context", ctx)
	}
	// réglages imposés par le preset (sinon : ceux de l'installeur)
	if kv := strings.TrimSpace(cfg["STRATA_KV"]); kv != "" {
		args = strataSetArg(args, "--kv", kv)
	}
	if sp := strings.TrimSpace(cfg["STRATA_SPEC"]); sp != "" {
		args = strataSetArg(args, "--spec", sp)
	}
	if kr := strings.TrimSpace(cfg["STRATA_KV_RESIDENT"]); kr != "" {
		args = strataSetArg(args, "--kv-resident", kr)
	}
	// lecture des images coupée dans les réglages : ni encodeur ni --vision
	if cfg["STRATA_VISION"] == "0" {
		args = strataDropFlag(args, "--vision")
		delete(out, "vision")
	}
	list := make([]any, len(args))
	for i, a := range args {
		list[i] = a
	}
	out["args"] = list
	out["port"], _ = strconv.Atoi(firstNonEmpty(cfg["PORT"], "8080"))
	out["host"] = firstNonEmpty(cfg["HOST"], "127.0.0.1")
	if apiKey != "" {
		out["api_key"] = apiKey
	}
	return out, nil
}

// strataLaunchModeKey : le mode des experts du dernier lancement (bkState),
// montré dans la fenêtre de Strata pour le preset actif.
const strataLaunchModeKey = "strata_experts_mode"

// strataHelperVRAM : VRAM (Go) de la carte d'appoint du preset, 0 sans.
func strataHelperVRAM(cfg map[string]string, gpus []strataGPU) float64 {
	if !strataHelperOn(cfg) {
		return 0
	}
	h := strings.TrimSpace(cfg["STRATA_HELPER_GPU"])
	for _, g := range gpus {
		if strconv.Itoa(g.Index) == h {
			return g.VRAMGB
		}
	}
	return 0
}

// strataPresetActive : le preset id est-il celui en service ?
func strataPresetActive(id string) (bool, error) {
	list, err := ListPresets()
	if err != nil {
		return false, err
	}
	for _, p := range list {
		if p.ID == id {
			return p.Active, nil
		}
	}
	return false, nil
}

// strataGPUs : les cartes NVIDIA vues par nvidia-smi (nil si indisponible).
func strataGPUs() []strataGPU {
	gpus, err := detectGPUs()
	if err != nil {
		return nil
	}
	var out []strataGPU
	for _, g := range gpus {
		mib, _ := strconv.ParseFloat(g.MemTotal, 64)
		out = append(out, strataGPU{Index: g.Index, Name: g.Name, VRAMGB: mib / 1024})
	}
	return out
}

// strataEngineMarker : le paquet dont vient le moteur installé.
func strataEngineMarker() string {
	return filepath.Join(strataSrcDir(), "engine-cuda12", ".loki-engine")
}

func strataEngineInstalled() string {
	b, _ := os.ReadFile(strataEngineMarker())
	return strings.TrimSpace(string(b))
}

// strataEnsureEngine : remplace un moteur installé d'une autre révision par
// celui du paquet attendu (téléchargé et vérifié si besoin). En cas d'échec
// l'ancien reste, et le mode qui demande le nouveau n'est pas activé.
func strataEnsureEngine() error {
	if strataEngineInstalled() == strataEngineAsset {
		return nil
	}
	dir := filepath.Join(strataSrcDir(), "engine-cuda12")
	if _, err := os.Stat(dir); err != nil {
		return err
	}
	zp, err := strataFetch(strataEngineAsset)
	if err != nil {
		return err
	}
	z, err := zip.OpenReader(zp)
	if err != nil {
		return err
	}
	defer z.Close()
	for _, f := range z.File {
		name := filepath.Base(f.Name)
		if name != "strata" && name != "strata-vision" && name != "BUILD.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		tmp := filepath.Join(dir, name+".new")
		w, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err == nil {
			_, err = io.Copy(w, rc)
			if cerr := w.Close(); err == nil {
				err = cerr
			}
		}
		rc.Close()
		if err != nil {
			_ = os.Remove(tmp)
			return err
		}
		// rename : remplace le binaire même s'il tourne encore
		if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return os.WriteFile(strataEngineMarker(), []byte(strataEngineAsset+"\n"), 0o644)
}

// strataHelperOn : la carte d'aide existe et n'a pas été coupée dans les
// réglages (STRATA_HELPER=0 la coupe en gardant son index pour la remettre).
func strataHelperOn(cfg map[string]string) bool {
	h := strings.TrimSpace(cfg["STRATA_HELPER_GPU"])
	return h != "" && h != "-1" && cfg["STRATA_HELPER"] != "0"
}

// strataCudaDevices : ordre des cartes vu par le moteur, la principale en CUDA0.
func strataCudaDevices(cfg map[string]string) string {
	main := strings.TrimSpace(cfg["STRATA_MAIN_GPU"])
	if main == "" || main == "-1" {
		return ""
	}
	if strataHelperOn(cfg) {
		return main + "," + strings.TrimSpace(cfg["STRATA_HELPER_GPU"])
	}
	return main
}

// strataPreflight : ce qu'il faut pour lancer Strata, vérifié avant le
// démarrage du service (preflightEngine) pour un message clair.
func strataPreflight(cfg map[string]string) error {
	path := strings.TrimSpace(cfg["STRATA_CONFIG"])
	if path == "" {
		return fmt.Errorf("STRATA_CONFIG non défini : réinstalle le modèle depuis Paramètres → Moteur → Strata")
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("configuration de Strata introuvable : %s — réinstalle le modèle", path)
	}
	py := filepath.Join(filepath.Dir(path), ".venv", "bin", "python")
	if _, err := os.Stat(py); err != nil {
		return fmt.Errorf("environnement Python de Strata absent (%s) : réinstalle le modèle", py)
	}
	return nil
}

// serveStrata : la branche Strata de `loki serve`. Écrit la config finale à
// côté de celle de l'installeur puis remplace le process par le serveur de
// Strata (comme pour llama-server : le superviseur suit directement le moteur).
func serveStrata(cfg map[string]string) error {
	if err := strataPreflight(cfg); err != nil {
		return err
	}
	path := strings.TrimSpace(cfg["STRATA_CONFIG"])
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("configuration de Strata introuvable : %s", path)
	}
	var base map[string]any
	if err := json.Unmarshal(raw, &base); err != nil {
		return fmt.Errorf("configuration de Strata illisible (%s) : %w", path, err)
	}
	if err := strataEnsureEngine(); err != nil {
		fmt.Fprintf(os.Stderr, "[loki serve] moteur %s non installé (%v) : l'ancien reste en place\n", strataEngineAsset, err)
	}
	apiKey, _ := effectiveAPIKeyErr()
	// Experts : décidé ICI, sur la RAM disponible au moment du lancement (l'ancien
	// moteur est déjà arrêté), pas sur la RAM totale.
	avail := memAvailableGB()
	mode := strataResolveExperts(cfg, strataArgList(base), avail, strataHelperVRAM(cfg, strataGPUs()))
	fmt.Fprintf(os.Stderr, "%s experts : %s (choix %s, %.0f Go de RAM disponibles, %.0f Go d'experts)\n",
		strataServeMarker, strataExpertsLabel(mode), firstNonEmpty(cfg["STRATA_EXPERTS"], strataExpertsAuto), avail, strataArenaGB(cfg))
	_ = putStr(bkState, strataLaunchModeKey, mode)
	final, err := strataBuildConfig(base, cfg, apiKey, mode)
	if err != nil {
		return err
	}
	srcDir := filepath.Dir(path)
	py := filepath.Join(srcDir, ".venv", "bin", "python")
	tag := strataConfigTag(path)
	final["log"] = filepath.Join(strataHome(), "strata-"+tag+".log")
	out := filepath.Join(strataHome(), "loki-strata-"+tag+".json")
	data, _ := json.MarshalIndent(final, "", " ")
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return err
	}
	_ = os.Setenv("CUDA_DEVICE_ORDER", "PCI_BUS_ID")
	if dev := strataCudaDevices(cfg); dev != "" {
		_ = os.Setenv("CUDA_VISIBLE_DEVICES", dev)
	}
	_ = os.Setenv("PYTHONUNBUFFERED", "1")
	port := fmt.Sprint(final["port"])
	if err := waitPortFree(fmt.Sprint(final["host"]), port, 5*time.Second); err != nil {
		return err
	}
	server := filepath.Join(srcDir, "serve", "server.py")
	args := []string{py, server, "--engine", "strata", "--config", out, "--port", port}
	_ = os.Chdir(srcDir)
	fmt.Fprintf(os.Stderr, "%s (paquet AJEAN MoE %s)  config=%s  port=%s  gpu=%s\n",
		strataServeMarker, strataVersion, filepath.Base(out), port, os.Getenv("CUDA_VISIBLE_DEVICES"))
	recordEngineCmdline(map[string]string{"CUDA_VISIBLE_DEVICES": os.Getenv("CUDA_VISIBLE_DEVICES")}, args)
	return execServer(py, args)
}

// strataServeMarker : la ligne de journal d'un lancement de Strata, où
// modelLoadError fait commencer la dernière tentative.
const strataServeMarker = "[loki serve] Strata"

// strataConfigTag : le modèle d'une config de l'installeur
// (strata-swift-iq3_xxs.json → swift-iq3_xxs).
func strataConfigTag(path string) string {
	b := strings.TrimSuffix(filepath.Base(path), ".json")
	if i := strings.Index(b, "-"); i >= 0 {
		b = b[i+1:]
	}
	return b
}

// strataVisionActive : le preset Strata actif lit les images (encodeur installé).
func strataVisionActive() bool {
	cfg := ReadConfig()
	return isStrataConfig(cfg) && cfg["STRATA_VISION"] != "0"
}

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

// handleStrata (GET) : la machine, ce qui y tient, la recommandation par
// version, et les modèles déjà installés (presets ENGINE=strata).
func handleStrata(w http.ResponseWriter, r *http.Request) {
	env := strataDetect()
	type famView struct {
		strataFamily
		Fits      []strataFit `json:"fits"`
		Recommend string      `json:"recommend"`
	}
	fams := []famView{}
	for _, f := range strataFamilies {
		v := famView{strataFamily: f, Recommend: strataRecommend(f.ID, env)}
		for _, id := range f.Quants {
			v.Fits = append(v.Fits, strataFitFor(strataQuants[id], env, strataHaveGB(f.ID, id)))
		}
		fams = append(fams, v)
	}
	installed := []string{}
	active := map[string]bool{}
	if list, err := ListPresets(); err == nil {
		for _, p := range list {
			if c, err := ReadPreset(p.ID); err == nil && isStrataConfig(parseEnv(c)) {
				installed = append(installed, p.ID)
				active[p.ID] = p.Active
			}
		}
	}
	// chaque choix déjà installé est marqué : la fenêtre propose alors de
	// l'activer au lieu de le réinstaller
	for i := range fams {
		for j := range fams[i].Fits {
			id := strataPresetName(fams[i].ID, fams[i].Fits[j].Quant.ID)
			if a, ok := active[id]; ok {
				fams[i].Fits[j].Preset, fams[i].Fits[j].Active = id, a
				fams[i].Fits[j].Details = strataDetailsFor(id, env)
			}
		}
	}
	sendJSON(w, 200, map[string]any{
		"version": strataVersion, "env": env, "families": fams, "installed": installed,
		"active": strataActive(),
	})
}

// strataSettingsReq : les réglages modifiables d'un modèle installé.
type strataSettingsReq struct {
	Preset  string `json:"preset"`
	Ctx     int    `json:"ctx"`
	KV      string `json:"kv"`
	Spec    int    `json:"spec"`
	Vision  bool   `json:"vision"`
	Helper  bool   `json:"helper"`
	Experts string `json:"experts"` // auto, ram ou disk ; vide = inchangé
}

// strataApplySettings réécrit les clés du preset (pure, testable).
func strataApplySettings(content string, r strataSettingsReq) (string, error) {
	switch {
	// contexte : de 32K au maximum du modèle (262 144), par pas de 4096
	case r.Ctx < 32768 || r.Ctx > strataMaxCtx || r.Ctx%4096 != 0:
		return "", fmt.Errorf("contexte hors limites : %d", r.Ctx)
	case r.KV != "fp16" && r.KV != "int8":
		return "", fmt.Errorf("cache KV inconnu : %s", r.KV)
	case r.Spec < 2 || r.Spec > 4:
		return "", fmt.Errorf("MTP hors limites : %d", r.Spec)
	case r.Experts != "" && r.Experts != strataExpertsAuto && r.Experts != strataExpertsRAM && r.Experts != strataExpertsDisk:
		return "", fmt.Errorf("placement des experts inconnu : %s", r.Experts)
	}
	set := map[string]string{
		"CTX": strconv.Itoa(r.Ctx), "STRATA_KV": r.KV, "STRATA_SPEC": strconv.Itoa(r.Spec),
		"STRATA_VISION": strataBool(r.Vision), "STRATA_HELPER": strataBool(r.Helper),
	}
	keys := []string{"CTX", "STRATA_KV", "STRATA_SPEC", "STRATA_VISION", "STRATA_HELPER"}
	if r.Experts != "" {
		set["STRATA_EXPERTS"] = r.Experts
		keys = append(keys, "STRATA_EXPERTS")
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		k, _, _ := strings.Cut(strings.TrimSpace(l), "=")
		if _, ok := set[k]; ok {
			continue
		}
		lines = append(lines, l)
	}
	for _, k := range keys {
		lines = append(lines, k+"="+set[k])
	}
	return strings.Join(lines, "\n") + "\n", nil
}

// handleStrataSettings (POST) : enregistre les réglages d'un modèle installé ;
// s'il est actif, la config est réappliquée et le moteur relancé.
func handleStrataSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, 405, map[string]any{"ok": false, "error": "POST attendu"})
		return
	}
	var req strataSettingsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if strings.ContainsAny(req.Preset, "/\\") || req.Preset == "" {
		sendJSON(w, 400, map[string]any{"ok": false, "error": "preset invalide"})
		return
	}
	content, err := ReadPreset(req.Preset)
	if err != nil || !isStrataConfig(parseEnv(content)) {
		sendJSON(w, 404, map[string]any{"ok": false, "error": "modèle introuvable"})
		return
	}
	next, err := strataApplySettings(content, req)
	if err != nil {
		sendJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	path := filepath.Join(presetsDir(), req.Preset+".env")
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		sendJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	restarted := false
	if list, err := ListPresets(); err == nil {
		for _, p := range list {
			if p.ID == req.Preset && p.Active {
				if tuneDenyHTTP(w) {
					return
				}
				if err := SwitchToPreset(path); err != nil {
					sendJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
					return
				}
				restarted = true
			}
		}
	}
	sendJSON(w, 200, map[string]any{"ok": true, "restarted": restarted})
}

// handleStrataInstall (POST {family, quant}) : lance la tâche d'installation.
// Une seule tâche à la fois, partagée avec llama.cpp (même suivi dans l'UI).
func handleStrataInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, 405, map[string]any{"ok": false, "error": "POST attendu"})
		return
	}
	var req strataInstallReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	ok := false
	for _, f := range strataFamilies {
		if f.ID == req.Family && strataHas(f.Quants, req.Quant) {
			ok = true
		}
	}
	if !ok {
		sendJSON(w, 400, map[string]any{"ok": false, "error": "version ou quant inconnu"})
		return
	}
	if err := startLcJob("strata", func() { strataRunInstall(req) }); err != nil {
		sendJSON(w, 409, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sendJSON(w, 200, map[string]any{"ok": true})
}
