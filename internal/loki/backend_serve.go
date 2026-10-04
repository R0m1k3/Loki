package loki

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// helpSupportsReasoningFlag dit si ce llama-server accepte « --reasoning ».
//
// Le drapeau est récent : les moteurs plus anciens, et certains forks, ne le
// connaissent pas et REFUSENT de démarrer sur un argument inconnu. Comme on ne
// l'ajoute que pour interdire le raisonnement, mieux vaut demander au binaire
// que parier : on lit son aide, une fois, au lancement du moteur.
//
// L'aide se lit avec le même chemin de bibliothèques que le vrai lancement
// (setLibraryPath a déjà été appelé) : sans ça un moteur parfaitement valide
// échoue à s'exécuter (« libllama-common.so introuvable ») et on conclurait à
// tort qu'il ne gère pas le drapeau. Les tests de capacité prennent le TEXTE de
// l'aide, pas le binaire : buildServeArgs reste ainsi une fonction pure,
// testable avec une aide fabriquée.
func helpSupportsReasoningFlag(help string) bool {
	return strings.Contains(help, "--reasoning ")
}

// binHelp lit « <bin> --help » UNE fois par binaire et garde le texte. Plusieurs
// capacités se déduisent de cette aide (--reasoning, --load-mode, « -ngl auto »)
// et relancer le moteur à chaque question coûterait quelques secondes de plus au
// démarrage. Aide illisible = chaîne vide : tous les tests de capacité répondent
// « non », c'est-à-dire « ne prends aucun risque, garde la forme historique ».
var (
	helpMu    sync.Mutex
	helpCache = map[string]string{}
)

func binHelp(bin string) string {
	helpMu.Lock()
	defer helpMu.Unlock()
	if h, ok := helpCache[bin]; ok {
		return h
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--help")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	h := ""
	if err := cmd.Run(); err == nil || out.Len() > 0 {
		h = out.String()
	}
	helpCache[bin] = h
	return h
}

// helpSupportsLoadMode : les moteurs récents ont fusionné --mlock, --mmap et
// --no-mmap dans un seul --load-mode ; les anciens ne connaissent que les trois
// drapeaux d'origine. On traduit donc à l'exécution, sans réécrire le preset :
// le même EXTRA_ARGS reste lançable sur les deux générations de moteur.
func helpSupportsLoadMode(help string) bool {
	return strings.Contains(help, "--load-mode")
}

// helpSupportsNGLAuto : « -ngl auto » laisse llama.cpp mesurer la VRAM libre et
// choisir le nombre de couches. Imposer un nombre (999 compris) désarme ce
// calcul — « n_gpu_layers already set by user to 999, abort » — et le moteur
// tente alors de tout mettre sur le GPU, quitte à échouer en cudaMalloc.
func helpSupportsNGLAuto(h string) bool {
	i := strings.Index(h, "--n-gpu-layers")
	if i < 0 {
		i = strings.Index(h, "-ngl")
	}
	if i < 0 {
		return false
	}
	end := i + 400
	if end > len(h) {
		end = len(h)
	}
	return strings.Contains(h[i:end], "'auto'")
}

// helpFitsLayersItself : ce moteur sait-il répartir les couches tout seul ?
//
// La lecture fine de l'aide (helpSupportsNGLAuto) reste la source de vérité, mais
// elle dépend de la mise en page d'un texte d'aide — une description reformulée
// ou une colonne plus large, et on conclut « non » sur un moteur parfaitement
// capable, donc on lui réimpose 999 et l'abandon revient. --load-mode est arrivé
// dans la même vague que « -ngl auto » : sa présence sert de second témoin, plus
// grossier mais insensible à la mise en page.
func helpFitsLayersItself(help string) bool {
	return helpSupportsNGLAuto(help) || helpSupportsLoadMode(help)
}

// nglArgs traduit la valeur NGL du preset en arguments, et renvoie au passage la
// note à afficher quand Loki a substitué quelque chose. Fonction pure : la seule
// question qui demande le moteur (« sait-il répartir les couches tout seul ? »)
// arrive déjà tranchée, ce qui la rend testable sans lancer llama-server.
func nglArgs(ngl string, fitsItself bool) ([]string, string) {
	ngl = strings.TrimSpace(ngl)
	switch {
	case strings.EqualFold(ngl, "auto"):
		return nil, "" // rien du tout : le moteur décide seul
	case ngl == "999" && fitsItself:
		return []string{"-ngl", "auto"},
			"NGL=999 (sentinelle « toutes les couches ») → -ngl auto : ce moteur mesure lui-même la " +
				"VRAM libre. NGL=all pour forcer l'ancien comportement."
	case ngl != "":
		return []string{"-ngl", ngl}, ""
	case fitsItself:
		return []string{"-ngl", "auto"}, ""
	default:
		// Moteur ancien : omettre -ngl le ferait tourner 100 % CPU.
		return []string{"-ngl", "999"}, ""
	}
}

// hasAnyFlag dit si l'utilisateur a déjà posé l'un de ces drapeaux dans
// EXTRA_ARGS. Loki ajoute plusieurs valeurs par défaut (--parallel, -ngl) : les
// ajouter EN PLUS de celles de l'utilisateur fait râler le moteur (« argument
// specified multiple times ») et, pire, c'est la DERNIÈRE occurrence qui gagne
// — le réglage explicite du preset serait donc silencieusement écrasé par le
// défaut de Loki. Quand l'utilisateur a tranché, on se tait.
func hasAnyFlag(args []string, flags ...string) bool {
	for _, a := range args {
		name := a
		if i := strings.IndexByte(name, '='); i > 0 {
			name = name[:i]
		}
		for _, f := range flags {
			if name == f {
				return true
			}
		}
	}
	return false
}

// normalizeLoadFlags choisit, au lancement, la forme des drapeaux de chargement
// que CE binaire comprend. llama.cpp récent a remplacé --mlock / --no-mmap par
// --load-mode (et refuse les anciens) ; un moteur ancien ou un fork ne connaît
// que les anciens. L'éditeur écrit --load-mode, un preset d'avant peut porter
// les anciens : on traduit dans le sens qu'il faut (repris d'AJEAN 0.16.0).
//
// Vers --load-mode (moteur récent). Deux interrupteurs → une seule valeur, au
// sens où llama.cpp l'entend (« mlock » = PAS de mmap + résident) :
//
//	--mlock seul          → mmap+mlock  (l'ancien sens de --mlock : mmap conservé)
//	--no-mmap seul        → none
//	--mlock + --no-mmap   → mlock
//	--mmap                → mmap
//
// ⚠️ Jusqu'ici --mlock seul devenait « mlock », qui COUPE le mmap : le modèle
// entier montait en RAM au lieu d'être mappé — un OOM assuré sur un modèle plus
// gros que la mémoire. Un --load-mode déjà écrit gagne sur tout : on retire
// alors simplement les vieux drapeaux.
//
// Vers les anciens drapeaux (moteur qui ne connaît pas --load-mode) :
//
//	none → --no-mmap · mlock → --mlock --no-mmap · mmap+mlock → --mlock
//	auto, mmap, dio → rien (défaut de l'ancien moteur)
//
// La note éventuelle (dio abandonné) est RENDUE, pas affichée : buildServeArgs
// reste pure et c'est cmdServe qui l'écrit, à sa place parmi les autres notes.
func normalizeLoadFlags(args []string, supportsLoadMode bool) ([]string, string) {
	if !supportsLoadMode {
		return downgradeLoadMode(args)
	}
	explicit := hasAnyFlag(args, "--load-mode", "-lm")
	mlock, nommap, mmap := false, false, false
	out := make([]string, 0, len(args)+2)
	for _, a := range args {
		switch a {
		case "--mlock":
			mlock = true
		case "--no-mmap":
			nommap = true
		case "--mmap":
			mmap = true
		default:
			out = append(out, a)
		}
	}
	if explicit || !(mlock || nommap || mmap) {
		return out, ""
	}
	mode := "mmap"
	switch {
	case mlock && nommap:
		mode = "mlock"
	case mlock:
		mode = "mmap+mlock"
	case nommap:
		mode = "none"
	}
	return append(out, "--load-mode", mode), ""
}

// downgradeLoadMode retraduit un --load-mode pour un moteur qui ne le connaît
// pas : le lui passer tel quel le ferait sortir en erreur au démarrage.
func downgradeLoadMode(args []string) ([]string, string) {
	mode, found := "", false
	kept := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--load-mode" || a == "-lm" {
			found = true
			if i+1 < len(args) {
				mode = strings.ToLower(args[i+1])
				i++
			}
			continue
		}
		if v, ok := strings.CutPrefix(a, "--load-mode="); ok {
			found, mode = true, strings.ToLower(v)
			continue
		}
		kept = append(kept, a)
	}
	if !found {
		return args, ""
	}
	add := func(flag string) {
		for _, a := range kept {
			if a == flag {
				return
			}
		}
		kept = append(kept, flag)
	}
	switch mode {
	case "none":
		add("--no-mmap")
	case "mlock":
		add("--mlock")
		add("--no-mmap")
	case "mmap+mlock":
		add("--mlock")
	case "dio":
		return kept, "ce moteur ne connaît pas --load-mode dio (DirectIO) : chargement par défaut"
	}
	return kept, ""
}

// cmdServe replaces the historic start.sh: read config.env, build the
// llama-server invocation, and exec it (replacing this process so systemd
// supervises llama-server directly).
//
// Tout ce qui touche au monde (fichiers, aide du moteur, variables
// d'environnement, port, exec) reste ICI ; la composition de la ligne de
// commande vit dans buildServeArgs, pure, pour qu'un changement de drapeau se
// vérifie en table de tests plutôt qu'en relançant un moteur.
func cmdServe(args []string) error {
	cfg := ReadConfig()
	bin := cfg["BIN"]
	if bin == "" {
		return fmt.Errorf("BIN non défini — lance « loki edit »")
	}
	model := cfg["MODEL"]
	if model == "" {
		return fmt.Errorf("MODEL non défini — lance « loki edit »")
	}
	// MODEL vaut soit un simple nom de fichier (le .gguf vit dans LOKI_HOME ou
	// dans un dossier déclaré — disque externe…), soit un chemin absolu. Sous
	// systemd/launchd le WorkingDirectory vaut LOKI_HOME, donc le relatif tombait
	// juste ; lancé depuis une app de bureau, le répertoire courant est « / » et
	// llama-server ne trouvait rien. On résout donc explicitement, quel que soit
	// le contexte de lancement.
	resolved, err := resolveServeModelPath(model)
	if err != nil {
		return err
	}
	model = resolved
	if _, err := os.Stat(model); err != nil {
		return fmt.Errorf("modèle introuvable : %s", model)
	}
	// Modèle découpé en tranches : llama-server ouvre les suivantes tout seul, mais
	// s'il en manque une il démarre puis meurt sur un tenseur introuvable — message
	// incompréhensible, et systemd relance en boucle. On le dit ici, en clair.
	if missing := shardFamilyMissing(filepath.Dir(model), filepath.Base(model)); len(missing) > 0 {
		return fmt.Errorf("modèle incomplet : il manque %s dans %s — ce modèle tient en %d fichiers, télécharge-les tous",
			strings.Join(missing, ", "), filepath.Dir(model), len(shardFamily(filepath.Base(model))))
	}
	if !filepath.IsAbs(bin) {
		bin = filepath.Join(LokiHome(), bin)
	}
	// Le moteur précompilé s'installe dans un dossier versionné : un preset écrit
	// avant une mise à jour pointe sur une release qui n'existe plus. On le fait
	// suivre au moteur courant plutôt que d'échouer en 127.
	bin = prebuiltResolveBin(bin)

	// Make sure llama-server can find its bundled shared libraries (the .so/.dll
	// neighbours of the binary). This is platform-specific: LD_LIBRARY_PATH on
	// Linux, PATH on Windows — handled inside execServer.
	setLibraryPath(filepath.Dir(bin))

	// La sélection GPU est posée AVANT de lire l'aide du moteur, comme elle l'a
	// toujours été : le moteur interrogé voit déjà les mêmes devices que celui
	// qu'on lancera. buildServeArgs la renvoie aussi dans env (même valeur).
	applyServeEnv(cudaDeviceEnv(cfg))

	si := serveSysInfo{Help: binHelp(bin), Model: model}
	// Vision : le projecteur multimodal (mmproj-*.gguf) donne des yeux au modèle.
	// C'est un fichier .gguf À PART du modèle, chargé via --mmproj. On le résout
	// comme le modèle (nom simple cherché dans les dossiers déclarés, ou chemin
	// absolu), pour qu'un preset écrit sous Windows reste lançable ailleurs et que
	// le champ « Vision » de l'interface n'ait qu'à écrire le nom du fichier.
	// Introuvable = on préfère le dire clairement plutôt que laisser llama-server
	// mourir en boucle sur un « failed to load mmproj » cryptique.
	if mm := strings.TrimSpace(cfg["MMPROJ"]); mm != "" {
		mmPath, err := resolveServeModelPath(mm)
		if err != nil {
			return fmt.Errorf("projecteur vision introuvable : %s (%v)", mm, err)
		}
		if _, err := os.Stat(mmPath); err != nil {
			return fmt.Errorf("projecteur vision introuvable : %s", mmPath)
		}
		si.MMProj = mmPath
	}
	// API_KEY protège le serveur quand il est exposé sur internet : llama-server
	// exige alors l'en-tête "Authorization: Bearer <clé>". La clé est lue depuis
	// $LOKI_HOME/.api_key en priorité (elle survit ainsi aux changements de preset
	// qui réécrivent config.env), avec config.env comme repli rétro-compatible.
	si.APIKey, _ = effectiveAPIKeyErr()
	// Conteneur à l'étroit (cpuset, quota) : seul Linux l'expose, dans /proc et
	// /sys. Un LLAMA_ARG_THREADS déjà posé est un choix de l'utilisateur — un -t
	// de Loki l'écraserait, donc on ne sonde même pas.
	if runtime.GOOS == "linux" && os.Getenv("LLAMA_ARG_THREADS") == "" {
		si.CPU = cpusetThreads(os.DirFS("/"))
	}

	extra := splitArgs(cfg["EXTRA_ARGS"])
	// Deux GPU ou plus : de quoi décider des files de lancement CUDA (voir
	// launchQueuesEnv). Après la sélection GPU, qui fait partie de la réponse.
	probeServeGPUs(cfg, extra, &si)
	probeExpertEnv(&si)
	probeFidelityEnv(&si)
	probeCacheRAM(cfg, extra, &si)
	probeCkptEnv(&si)
	probeSideSlotEnv(cfg, &si)
	// SPLIT_MODE=tensor seulement : cartes listées par CE moteur, dans son ordre.
	probeSplit(cfg, bin, &si)
	spec := specMode(cfg)
	if spec != "off" && spec != "" {
		probeSpec(cfg, &si)
	}
	// Build du moteur : seulement pour un hybride ou SPEC=auto, les seuls cas où
	// il décide de quelque chose (voir ckptArgs, specAutoBlocker) — inutile de
	// lancer « --version » sinon.
	if ggufHybrid(si.GGUF) || spec == "auto" {
		si.EngineBuild = engineBuildTrusted(bin)
	}
	// Lu seulement ici ; rien n'est consommé avant que le port soit libre.
	specMark := specAutoMark{FP: configFingerprint(cfg), Bin: bin, Build: si.EngineBuild}
	specRecord := false
	if spec == "auto" {
		si.SpecAutoBlocked, specRecord = specAutoPeek(specMark)
	}
	probeSlotPersistEnv(cfg, &si)
	if strings.Contains(si.Help, "--slot-save-path") && !hasAnyFlag(extra, "--slot-save-path") {
		si.SlotDir = prepareSlotDir(LokiHome(), slotPersistOn(cfg))
	}

	llmArgs, env, notes := buildServeArgs(cfg, extra, bin, si)
	applyServeEnv(env)
	// SLOT_PERSIST (backend_serve_persist.go) : la clé de CE lancement, et les
	// états gardés au-delà de deux retirés. Refusé pour ce preset (brouillon,
	// poids sur CPU…) : ceux des autres presets restent, pour leur retour.
	// Clé retirée : plus aucun état gardé sur le disque.
	persistKey := ""
	if ok, _ := slotPersistPlan(cfg, extra, si); ok {
		persistKey = slotPersistKey(llmArgs, env, si, bin)
		slotPersistPrune(si.SlotDir)
	} else if !slotPersistOn(cfg) {
		slotPersistPurge(filepath.Join(LokiHome(), "slots"))
	}
	for _, n := range notes {
		fmt.Fprintln(os.Stderr, "[loki serve] "+n)
	}

	// Working dir = LOKI_HOME so relative paths in EXTRA_ARGS (e.g. --mmproj
	// mmproj-F16.gguf) still resolve.
	_ = os.Chdir(LokiHome())

	// Port déjà pris (souvent un llama-server orphelin qu'un stop n'a pas pu
	// tuer) : deux moteurs sur un même port se partagent les requêtes au hasard,
	// VRAM saturée et réponses du mauvais modèle. On refuse en clair.
	host, port := argValue(llmArgs, "--host"), argValue(llmArgs, "--port")
	if err := waitPortFree(host, port, 5*time.Second); err != nil {
		return err
	}
	// Le type EFFECTIF : un -ctk/-ctv d'EXTRA_ARGS l'emporte sur KV_TYPE, et c'est
	// lui que le moteur accélère ou non.
	kt, vt, _ := effectiveKVTypes(cfg, extra, si.ArgEnv)
	warnSlowKV(kt, vt)
	// Jeton de tentative : l'ancien consommé (et l'échec inscrit), le nouveau
	// posé, au dernier moment — port libre, l'ancien moteur est donc bien parti
	// et le process web ne peut pas le confondre avec lui.
	if spec == "auto" {
		_, _, auto := specArgs(cfg, extra, si)
		specAutoSettle(specMark, si.SpecAutoBlocked, specRecord, auto)
	}

	// Posée au dernier moment, port libre : l'ancien moteur est parti, la clé
	// lue par le process web est celle du moteur qui répondra.
	writeSlotPersistMarker(LokiHome(), persistKey)

	fmt.Fprintf(os.Stderr, "[loki serve] %s  model=%s  port=%s\n",
		bin, filepath.Base(model), port)

	// Hand off to the llama-server process. On Unix this replaces the current
	// process (exec); on Windows it runs as a child and waits. See sys_platform_*.go.
	return execServer(bin, llmArgs)
}

// serveSysInfo rassemble ce que cmdServe a dû demander au monde avant de
// composer la ligne de commande : l'aide du moteur (d'où se déduisent ses
// capacités), les chemins déjà résolus et vérifiés, la clé d'API. Avec elle,
// buildServeArgs n'a plus besoin de toucher ni au disque ni au binaire.
type serveSysInfo struct {
	Help   string    // sortie de « <bin> --help » ; vide = moteur inconnu, aucun risque pris
	Model  string    // chemin du .gguf principal, résolu et vérifié
	MMProj string    // chemin du projecteur vision, résolu et vérifié ; vide = pas de vision
	APIKey string    // clé effective (.api_key, sinon config.env) ; vide = serveur ouvert
	CPU    cpuBudget // sonde de conteneur (Linux) ; zéro = llama.cpp choisit ses threads seul

	LaunchQueues string            // CUDA_SCALE_LAUNCH_QUEUES déjà dans l'environnement : choix de l'utilisateur, intouché
	GPUs         int               // GPU visibles (CUDA_VISIBLE_DEVICES, sinon nvidia-smi) ; 0 = inconnu ou non sondé
	ArgEnv       map[string]string // LLAMA_ARG_* de l'environnement qui décident du pipeline, de --fit et du cache (serveArgEnv, fitArgEnv, fidelityArgEnv)
	UserEnv      map[string]string // GGML_* des réglages d'expert déjà posés (voir expertEnvKeys) : intouchés

	// Cache de prompts (voir backend_serve_cache.go). Zéro = inconnu : le moteur
	// garde son défaut.
	GGUF        *GGUFInfo // métadonnées du modèle ; nil = illisibles
	ModelBytes  int64     // taille du modèle, toutes tranches
	RAMMiB      int64     // RAM de la machine, limite du conteneur comprise
	RAMAvailMiB int64     // RAM libre au lancement
	VRAMMiB     int64     // VRAM NVIDIA des cartes visibles
	SlotDir     string    // dossier de --slot-save-path, créé et vérifié ; vide = pas de drapeau

	// Build d'un moteur officiel (engineBuildTrusted), lu seulement pour un
	// modèle hybride ou SPEC=auto ; 0 = inconnu ou non lu : ni avis ni drapeau
	// automatique.
	EngineBuild int

	// Décodage spéculatif (voir backend_serve_spec.go), lu seulement si SPEC le
	// demande.
	Draft           string    // MODEL_DRAFT résolu et vérifié ; vide = absent ou introuvable
	DraftErr        string    // pourquoi MODEL_DRAFT n'a pas été trouvé
	DraftGGUF       *GGUFInfo // métadonnées du brouillon ; nil = illisibles
	SpecAutoBlocked string    // un essai automatique précédent a échoué : la raison

	// Cartes listées par le moteur (--list-devices), lues seulement pour
	// SPLIT_MODE=tensor (voir backend_serve_split.go) ; vide = inconnues.
	SplitDevs []splitDev
}

// cudaDeviceEnv : sélection GPU (loki gpu), on filtre les devices visibles par
// llama-server. CUDA_DEVICE_ORDER=PCI_BUS_ID garantit que les index
// correspondent à ceux affichés par nvidia-smi (sinon CUDA réordonne par
// « device le plus rapide »).
func cudaDeviceEnv(cfg map[string]string) map[string]string {
	env := map[string]string{}
	if v := cfg["CUDA_VISIBLE_DEVICES"]; v != "" {
		env["CUDA_VISIBLE_DEVICES"] = v
		env["CUDA_DEVICE_ORDER"] = "PCI_BUS_ID"
	}
	return env
}

func applyServeEnv(env map[string]string) {
	for k, v := range env {
		_ = os.Setenv(k, v)
	}
}

// serveKVTypes donne les types de cache K et V du preset : KV_TYPE pour les
// deux, KV_TYPE_K / KV_TYPE_V pour les régler séparément. Vide = défaut moteur.
func serveKVTypes(cfg map[string]string) (k, v string) {
	kv := cfg["KV_TYPE"]
	k, v = kv, kv
	if s := cfg["KV_TYPE_K"]; s != "" {
		k = s
	}
	if s := cfg["KV_TYPE_V"]; s != "" {
		v = s
	}
	return k, v
}

// buildServeArgs compose la ligne de commande de llama-server (bin en tête) à
// partir du preset, d'EXTRA_ARGS tel que découpé par splitArgs et de ce que
// cmdServe a sondé. Fonction pure : elle renvoie aussi les variables
// d'environnement à poser et les notes à afficher, sans rien faire elle-même.
// L'ordre des arguments est celui qu'a toujours produit cmdServe — le moteur
// retient la DERNIÈRE occurrence d'un drapeau, donc l'ordre fait partie du
// comportement.
func buildServeArgs(cfg map[string]string, extra []string, bin string, si serveSysInfo) (args []string, env map[string]string, notes []string) {
	get := func(key, fallback string) string {
		if v, ok := cfg[key]; ok && v != "" {
			return v
		}
		return fallback
	}
	ktv, vtv := serveKVTypes(cfg)
	env = cudaDeviceEnv(cfg)

	// EXTRA_ARGS est lu ICI, avant de composer la ligne de commande : ce que
	// l'utilisateur a écrit à la main décide des défauts que Loki a le droit
	// d'ajouter (voir hasAnyFlag). Il est aussi traduit vers les drapeaux de
	// chargement actuels quand le moteur les attend (voir normalizeLoadFlags).
	extra, loadNote := normalizeLoadFlags(extra, helpSupportsLoadMode(si.Help))
	if loadNote != "" {
		notes = append(notes, loadNote)
	}

	// Parallélisme de tenseurs (SPLIT_MODE=tensor, voir backend_serve_split.go) :
	// sans la clé, splitOn est faux et rien de ce qui suit ne change. Accepté, il
	// compte pour les réglages qui lisent le placement dans EXTRA_ARGS (files de
	// lancement, graphes CUDA, --fit) comme un -sm tensor écrit à la main.
	splitOn, splitA, splitNGLArgs, splitEnv, splitNotes := splitTensorArgs(cfg, extra, si)
	place := extra
	if splitOn {
		place = append(append([]string{}, extra...), "-sm", "tensor")
	}
	for k, v := range splitEnv {
		env[k] = v
	}
	notes = append(notes, splitNotes...)

	// Files de lancement CUDA : une variable d'environnement, pas un drapeau —
	// la ligne de commande n'en dépend pas (voir launchQueuesEnv).
	q, qNotes := launchQueuesEnv(cfg, place, si)
	if q != "" {
		env["CUDA_SCALE_LAUNCH_QUEUES"] = q
	}
	notes = append(notes, qNotes...)
	// Réglages d'expert passés par variable (voir backend_serve_expert.go) :
	// absents du preset, ils ne posent rien.
	g, gNotes := graphOptEnv(cfg, place, si)
	if g != "" {
		env["GGML_CUDA_GRAPH_OPT"] = g
	}
	o, oNotes := opOffloadMinBatchEnv(cfg, si)
	if o != "" {
		env["GGML_OP_OFFLOAD_MIN_BATCH"] = o
	}
	notes = append(append(notes, gNotes...), oNotes...)

	// Threads : vide ou 0 = AUCUN drapeau, pour que llama.cpp prenne ses cœurs
	// physiques au lieu de tous les threads logiques (voir threadArgs).
	threads, threadNotes := threadArgs(cfg["THREADS"], cfg["THREADS_BATCH"], extra, si.CPU)
	notes = append(notes, threadNotes...)
	// Second slot (SIDE_SLOT, voir backend_serve_side.go) : sans la clé, sideCtx
	// vaut 0 et rien de ce qui suit ne change.
	sideCtx, sideMiB, sideWhy := sideSlotPlan(cfg, extra, si)
	ctxArg := get("CTX", "32768")
	if sideCtx > 0 {
		ctxArg = strconv.Itoa(2 * sideCtx)
	}
	args = []string{bin,
		"-m", si.Model,
		"-c", ctxArg,
	}
	args = append(args, threads...)
	args = append(args,
		"-b", get("BATCH", "2048"),
		"-ub", get("UBATCH", "512"),
		"--host", get("HOST", "0.0.0.0"),
		"--port", get("PORT", "8080"),
	)
	// --parallel 1 EXPLICITE : les llama-server récents ouvrent plusieurs slots
	// par défaut, soit des tampons de calcul GPU multipliés d'autant — pour un
	// serveur mono-utilisateur comme Loki, c'est de la VRAM brûlée pour rien.
	// Vécu : une config 27B/60k ctx qui tournait très bien est morte en
	// « cudaMalloc failed: out of memory » après une mise à jour du moteur,
	// uniquement à cause de ce nouveau défaut. PARALLEL=n dans le preset pour qui
	// veut vraiment servir plusieurs requêtes à la fois — ou --parallel dans
	// EXTRA_ARGS, auquel cas on ne redouble pas le drapeau.
	if sideCtx > 0 {
		// Deux flux KV séparés de sideCtx jetons : aucun slot ne peut manger la
		// place de l'autre (voir backend_serve_side.go).
		args = append(args, "--parallel", "2", "--no-kv-unified")
	} else if !hasAnyFlag(extra, "--parallel", "-np") {
		args = append(args, "--parallel", get("PARALLEL", "1"))
	}
	if n := sideSlotNote(sideCtx, sideMiB, sideWhy, nglForced(cfg, extra)); n != "" {
		notes = append(notes, n)
	}
	// Couches GPU. Quatre cas, dans cet ordre :
	//
	//   NGL=auto      → rien du tout    (le moteur décide seul)
	//   NGL=999       → -ngl auto       sur un moteur récent (voir plus bas)
	//   NGL=<nombre>  → -ngl <nombre>   (l'utilisateur tranche pour de vrai)
	//   clé absente   → -ngl auto si le moteur le propose, sinon -ngl 999
	//
	// Imposer un NOMBRE désarme common_fit_params, qui mesure la VRAM libre et
	// choisit combien de couches y tiennent : « n_gpu_layers already set by user
	// to 999, abort ». Le moteur pousse alors tout sur le GPU, échoue en
	// cudaMalloc ou se replie à moitié sur le CPU — débit effondré, GPU à 100 %.
	//
	// 999 est traité à part parce que ce n'a JAMAIS été un nombre de couches :
	// c'est la sentinelle historique « toutes », semée dans config.env par
	// defaultConfig() sur chaque installation neuve. La quasi-totalité des
	// configurations la portent donc sans que personne ne l'ait choisie — la
	// traiter comme un choix délibéré, c'est condamner tout le monde à l'abandon
	// ci-dessus. Sur un moteur qui connaît « auto », 999 devient donc auto, et on
	// le DIT sur stderr plutôt que de le faire en douce. Qui veut réellement
	// forcer tout sur le GPU écrit NGL=all (ou un nombre qui n'est pas 999).
	if splitOn {
		args = append(args, splitNGLArgs...) // « -ngl all », ou rien si EXTRA_ARGS l'a déjà
	} else if !hasAnyFlag(extra, "-ngl", "--n-gpu-layers", "--gpu-layers") {
		ngl, note := nglArgs(get("NGL", ""), helpFitsLayersItself(si.Help))
		if note != "" {
			notes = append(notes, note)
		}
		args = append(args, ngl...)
	}
	// Marge VRAM par carte du placement automatique (FIT_TARGET, voir
	// fitTargetArgs) : seulement si --fit tournera vraiment.
	fitt, fitNotes := fitTargetArgs(cfg, place, si)
	args = append(append(args, fitt...), splitA...)
	notes = append(notes, fitNotes...)
	if ktv != "" {
		args = append(args, "-ctk", ktv)
	}
	if vtv != "" {
		args = append(args, "-ctv", vtv)
	}
	// Vision : chemin déjà résolu et vérifié par cmdServe (voir là-bas).
	if si.MMProj != "" {
		args = append(args, "--mmproj", si.MMProj)
	}
	// Raisonnement. Trois cas, et la nuance compte :
	//
	//   REASONING=on|auto|deepseek → --reasoning <valeur>
	//   REASONING=off              → --reasoning off  (interdiction EXPLICITE)
	//   clé absente                → aucun drapeau, le moteur fait son défaut
	//
	// L'interface écrivait « off » en EFFAÇANT la ligne, ce qui n'est pas du tout
	// la même chose : sans drapeau, llama-server suit le gabarit du modèle, et un
	// modèle à raisonnement raisonne. L'interrupteur affichait donc « désactivé »
	// pendant que le modèle réfléchissait quand même. Il faut le dire au moteur.
	if r := strings.TrimSpace(cfg["REASONING"]); r != "" {
		if reasoningActive(r) {
			// budget illimité par défaut (-1) : on laisse le modèle réfléchir jusqu'au
			// bout au lieu de le couper à 2048, ce qui tronquait la vraie réponse (la
			// réflexion atteignait le plafond et il ne restait plus de marge pour le
			// contenu). L'anti-boucle côté llm_client.go reste le garde-fou. NE PAS forcer 0 :
			// sur llama.cpp vanilla, 0 = "immediate end" → coupe tout le raisonnement
			// (le fork ik_llama.cpp l'ignore). Configurable via REASONING_BUDGET.
			args = append(args, "--reasoning", r, "--reasoning-budget", get("REASONING_BUDGET", "-1"))
		} else if helpSupportsReasoningFlag(si.Help) {
			// Pas de budget ici : « off » suffit, et un budget sur un moteur qui
			// n'attend rien d'autre ne ferait qu'ajouter une occasion d'échouer.
			args = append(args, "--reasoning", "off")
		} else {
			// Vieux moteur (ou fork) qui ne connaît pas le drapeau : le lui passer
			// le ferait sortir en erreur au démarrage, donc boucler. On le dit et on
			// continue sans — mieux vaut un modèle qui réfléchit qu'un moteur mort.
			notes = append(notes, "ce moteur ne connaît pas --reasoning : impossible de désactiver le raisonnement")
		}
	}
	// Conservation du raisonnement des tours passés : seulement si
	// REASONING_PRESERVE est posée (voir reasoningPreserveArgs pour le compromis).
	rp, rpNotes := reasoningPreserveArgs(cfg, extra, si)
	args = append(args, rp...)
	notes = append(notes, rpNotes...)
	if si.APIKey != "" {
		args = append(args, "--api-key", si.APIKey)
	}
	// Cache de prompts en RAM et dossier des slots (voir backend_serve_cache.go) :
	// avant EXTRA_ARGS, qui garde le dernier mot.
	cram, cramNotes := cacheRAMArgs(cfg, extra, si)
	args = append(append(args, cram...), slotSaveArgs(cfg, extra, si)...)
	notes = append(notes, cramNotes...)
	if n := slotPersistNote(slotPersistPlan(cfg, extra, si)); n != "" {
		notes = append(notes, n)
	}
	// Points de reprise des hybrides (voir backend_serve_ckpt.go).
	ck, ckNotes := ckptArgs(cfg, extra, si)
	args = append(args, ck...)
	notes = append(notes, ckNotes...)
	// Décodage spéculatif (voir backend_serve_spec.go) : rien tant que SPEC
	// n'est pas posé.
	sp, spNotes, _ := specArgs(cfg, extra, si)
	args = append(args, sp...)
	notes = append(notes, spNotes...)
	// EXTRA_ARGS (déjà découpé comme le ferait le shell — les guillemets gardent
	// ensemble un chemin qui contient des espaces) ferme la marche.
	args = append(args, extra...)
	// Garde-fous de fidélité (voir backend_serve_fidelity.go) : ils ne touchent à
	// aucun drapeau, ils DISENT ce que la ligne finale change aux calculs.
	ek, ev, src := effectiveKVTypes(cfg, extra, si.ArgEnv)
	if n := kvFidelityNote(ek, ev, src, extra, si.ArgEnv); n != "" {
		notes = append(notes, n)
	}
	notes = append(notes, lossyCacheNotes(extra, si)...)
	return args, env, notes
}

// argValue renvoie la valeur de la DERNIÈRE occurrence de flag (llama-server
// retient la dernière : EXTRA_ARGS peut surcharger --port).
func argValue(args []string, flag string) string {
	v := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			v = args[i+1]
		}
	}
	return v
}

// waitPortFree vérifie que personne n'écoute déjà sur host:port. On tente une
// connexion TCP (fiable même si l'occupant a bindé en SO_REUSEADDR, où un
// Listen de test pourrait réussir). Un moteur qu'on vient d'arrêter peut tenir
// le port quelques instants : on réessaie jusqu'à wait avant de conclure.
// Repris d'AJEAN 0.15.9.
func waitPortFree(host, port string, wait time.Duration) error {
	if port == "" {
		return nil
	}
	h := strings.Trim(host, "[]")
	if h == "" || h == "0.0.0.0" || h == "::" {
		h = "127.0.0.1"
	}
	addr := net.JoinHostPort(h, port)
	deadline := time.Now().Add(wait)
	for {
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			return nil
		}
		c.Close()
		if time.Now().After(deadline) {
			return fmt.Errorf("le port %s est déjà utilisé (sans doute un ancien llama-server encore actif) : "+
				"« loki stop », docker restart loki, ou change PORT", port)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// fastKV : combinaisons K/V que llama.cpp CUDA accélère en Flash-Attention
// avec ses options de compilation par défaut (GGML_CUDA_FA_ALL_QUANTS=OFF) —
// c'est le cas du moteur de l'image server-cuda comme de ceux tirés par OCI.
// Les autres (q5_*, q4_1, combinaisons mixtes) marchent mais sont reconverties
// en f16 à chaque pas : la génération ralentit nettement (AJEAN 0.15.9).
var fastKV = map[string]bool{"f16|f16": true, "q8_0|q8_0": true, "q4_0|q4_0": true, "bf16|bf16": true}

func slowKV(k, v string) bool {
	if k == "" {
		k = "f16"
	}
	if v == "" {
		v = "f16"
	}
	return !fastKV[k+"|"+v]
}

func warnSlowKV(k, v string) {
	if slowKV(k, v) {
		fmt.Fprintf(os.Stderr, "[loki serve] avertissement : cache KV %s/%s non accéléré par llama.cpp CUDA "+
			"(converti en f16 à chaque pas, lent). Préfère q8_0/q8_0, q4_0/q4_0 ou f16.\n", k, v)
	}
}
