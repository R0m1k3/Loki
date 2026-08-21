package loki

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// binSupportsReasoningFlag dit si ce llama-server accepte « --reasoning ».
//
// Le drapeau est récent : les moteurs plus anciens, et certains forks, ne le
// connaissent pas et REFUSENT de démarrer sur un argument inconnu. Comme on ne
// l'ajoute que pour interdire le raisonnement, mieux vaut demander au binaire
// que parier : on lit son aide, une fois, au lancement du moteur.
//
// L'aide se lit avec le même chemin de bibliothèques que le vrai lancement
// (setLibraryPath a déjà été appelé) : sans ça un moteur parfaitement valide
// échoue à s'exécuter (« libllama-common.so introuvable ») et on conclurait à
// tort qu'il ne gère pas le drapeau.
func binSupportsReasoningFlag(bin string) bool {
	return strings.Contains(binHelp(bin), "--reasoning ")
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

// binSupportsLoadMode : les moteurs récents ont fusionné --mlock, --mmap et
// --no-mmap dans un seul --load-mode ; les anciens ne connaissent que les trois
// drapeaux d'origine. On traduit donc à l'exécution, sans réécrire le preset :
// le même EXTRA_ARGS reste lançable sur les deux générations de moteur.
func binSupportsLoadMode(bin string) bool {
	return strings.Contains(binHelp(bin), "--load-mode")
}

// binSupportsNGLAuto : « -ngl auto » laisse llama.cpp mesurer la VRAM libre et
// choisir le nombre de couches. Imposer un nombre (999 compris) désarme ce
// calcul — « n_gpu_layers already set by user to 999, abort » — et le moteur
// tente alors de tout mettre sur le GPU, quitte à échouer en cudaMalloc.
func binSupportsNGLAuto(bin string) bool {
	h := binHelp(bin)
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

// binFitsLayersItself : ce moteur sait-il répartir les couches tout seul ?
//
// La lecture fine de l'aide (binSupportsNGLAuto) reste la source de vérité, mais
// elle dépend de la mise en page d'un texte d'aide — une description reformulée
// ou une colonne plus large, et on conclut « non » sur un moteur parfaitement
// capable, donc on lui réimpose 999 et l'abandon revient. --load-mode est arrivé
// dans la même vague que « -ngl auto » : sa présence sert de second témoin, plus
// grossier mais insensible à la mise en page.
func binFitsLayersItself(bin string) bool {
	return binSupportsNGLAuto(bin) || binSupportsLoadMode(bin)
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

// normalizeLoadFlags traduit les drapeaux de chargement dépréciés vers
// --load-mode, sur les moteurs qui le connaissent :
//
//	--mlock    → --load-mode mlock   (garder le modèle en RAM)
//	--no-mmap  → --load-mode none    (tout charger, pas de mmap)
//	--mmap     → --load-mode mmap
//
// Les deux à la fois (--mlock --no-mmap) : mlock l'emporte, c'est l'intention la
// plus forte (résident en RAM, jamais rendu au système). Un --load-mode déjà
// écrit à la main dans EXTRA_ARGS gagne sur tout : on retire alors simplement
// les vieux drapeaux. Sur un moteur ancien, rien n'est touché.
func normalizeLoadFlags(args []string, supportsLoadMode bool) []string {
	if !supportsLoadMode {
		return args
	}
	explicit := hasAnyFlag(args, "--load-mode")
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
		return out
	}
	mode := "mmap"
	switch {
	case mlock:
		mode = "mlock"
	case nommap:
		mode = "none"
	}
	return append(out, "--load-mode", mode)
}

// cmdServe replaces the historic start.sh: read config.env, build the
// llama-server invocation, and exec it (replacing this process so systemd
// supervises llama-server directly).
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

	// Sélection GPU (loki gpu) : on filtre les devices visibles par llama-server.
	// CUDA_DEVICE_ORDER=PCI_BUS_ID garantit que les index correspondent à ceux
	// affichés par nvidia-smi (sinon CUDA réordonne par "device le plus rapide").
	if v := cfg["CUDA_VISIBLE_DEVICES"]; v != "" {
		_ = os.Setenv("CUDA_VISIBLE_DEVICES", v)
		_ = os.Setenv("CUDA_DEVICE_ORDER", "PCI_BUS_ID")
	}

	get := func(key, fallback string) string {
		if v, ok := cfg[key]; ok && v != "" {
			return v
		}
		return fallback
	}
	kv := get("KV_TYPE", "")
	ktv := get("KV_TYPE_K", kv)
	vtv := get("KV_TYPE_V", kv)

	// EXTRA_ARGS est lu ICI, avant de composer la ligne de commande : ce que
	// l'utilisateur a écrit à la main décide des défauts que Loki a le droit
	// d'ajouter (voir hasAnyFlag). Il est aussi traduit vers les drapeaux de
	// chargement actuels quand le moteur les attend (voir normalizeLoadFlags).
	extra := normalizeLoadFlags(splitArgs(cfg["EXTRA_ARGS"]), binSupportsLoadMode(bin))

	llmArgs := []string{bin,
		"-m", model,
		"-c", get("CTX", "32768"),
		"-t", get("THREADS", "0"),
		"-tb", get("THREADS_BATCH", "0"),
		"-b", get("BATCH", "2048"),
		"-ub", get("UBATCH", "512"),
		"--host", get("HOST", "0.0.0.0"),
		"--port", get("PORT", "8080"),
	}
	// --parallel 1 EXPLICITE : les llama-server récents ouvrent plusieurs slots
	// par défaut, soit des tampons de calcul GPU multipliés d'autant — pour un
	// serveur mono-utilisateur comme Loki, c'est de la VRAM brûlée pour rien.
	// Vécu : une config 27B/60k ctx qui tournait très bien est morte en
	// « cudaMalloc failed: out of memory » après une mise à jour du moteur,
	// uniquement à cause de ce nouveau défaut. PARALLEL=n dans le preset pour qui
	// veut vraiment servir plusieurs requêtes à la fois — ou --parallel dans
	// EXTRA_ARGS, auquel cas on ne redouble pas le drapeau.
	if !hasAnyFlag(extra, "--parallel", "-np") {
		llmArgs = append(llmArgs, "--parallel", get("PARALLEL", "1"))
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
	if !hasAnyFlag(extra, "-ngl", "--n-gpu-layers", "--gpu-layers") {
		args, note := nglArgs(get("NGL", ""), binFitsLayersItself(bin))
		if note != "" {
			fmt.Fprintln(os.Stderr, "[loki serve] "+note)
		}
		llmArgs = append(llmArgs, args...)
	}
	if ktv != "" {
		llmArgs = append(llmArgs, "-ctk", ktv)
	}
	if vtv != "" {
		llmArgs = append(llmArgs, "-ctv", vtv)
	}
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
		llmArgs = append(llmArgs, "--mmproj", mmPath)
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
			llmArgs = append(llmArgs, "--reasoning", r, "--reasoning-budget", get("REASONING_BUDGET", "-1"))
		} else if binSupportsReasoningFlag(bin) {
			// Pas de budget ici : « off » suffit, et un budget sur un moteur qui
			// n'attend rien d'autre ne ferait qu'ajouter une occasion d'échouer.
			llmArgs = append(llmArgs, "--reasoning", "off")
		} else {
			// Vieux moteur (ou fork) qui ne connaît pas le drapeau : le lui passer
			// le ferait sortir en erreur au démarrage, donc boucler. On le dit et on
			// continue sans — mieux vaut un modèle qui réfléchit qu'un moteur mort.
			fmt.Fprintf(os.Stderr, "[loki serve] ce moteur ne connaît pas --reasoning : impossible de désactiver le raisonnement\n")
		}
	}
	// API_KEY protège le serveur quand il est exposé sur internet : llama-server
	// exige alors l'en-tête "Authorization: Bearer <clé>". La clé est lue depuis
	// $LOKI_HOME/.api_key en priorité (elle survit ainsi aux changements de preset
	// qui réécrivent config.env), avec config.env comme repli rétro-compatible.
	if k, _ := effectiveAPIKeyErr(); k != "" {
		llmArgs = append(llmArgs, "--api-key", k)
	}
	// EXTRA_ARGS (déjà découpé plus haut comme le ferait le shell — les guillemets
	// gardent ensemble un chemin qui contient des espaces) ferme la marche.
	llmArgs = append(llmArgs, extra...)

	// Working dir = LOKI_HOME so relative paths in EXTRA_ARGS (e.g. --mmproj
	// mmproj-F16.gguf) still resolve.
	_ = os.Chdir(LokiHome())

	fmt.Fprintf(os.Stderr, "[loki serve] %s  model=%s  port=%s\n",
		bin, filepath.Base(model), get("PORT", "8080"))

	// Hand off to the llama-server process. On Unix this replaces the current
	// process (exec); on Windows it runs as a child and waits. See sys_platform_*.go.
	return execServer(bin, llmArgs)
}
