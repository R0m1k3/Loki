package loki

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// sys_service.go holds the platform-neutral pieces of service management. The
// actual start/stop/restart/status/logs implementation is platform-specific:
//   - sys_service_linux.go   → systemd (systemctl/journalctl)
//   - sys_service_darwin.go  → launchd (launchctl)
//   - sys_service_windows.go → PID-file background process supervisor
//
// editConfig and showVram live here because they work the same everywhere.

// serviceAction : le seul passage vers serviceActionOS pour le reste de Loki.
// Pendant une optimisation (backend_tune_lock.go), le moteur d'essai tient la
// VRAM : démarrer le vrai moteur à côté — bascule de preset, GPU, mise à jour,
// rechargement… — chargerait deux modèles. Refus, quel que soit l'appelant et la
// plateforme ; l'optimiseur, lui, appelle serviceActionOS directement. Un verrou
// périmé (optimisation tuée) est écarté AVANT tout démarrage, son essai
// orphelin arrêté : sinon le moteur relancé partagerait la carte avec lui.
func serviceAction(action string) error {
	if action == "start" || action == "restart" {
		if err := tuneGuard(); err != nil {
			return err
		}
		tuneReapStale()
	}
	return serviceActionOS(action)
}

// preflightEngine vérifie ce sans quoi le moteur ne PEUT pas démarrer, avant de
// lancer le service. Sinon llama-server sortait en erreur, systemd le relançait
// toutes les 3 s, et `loki start` affichait un « activating » rassurant pendant
// que `loki test` répondait « /health ne répond pas ». Le diagnostic n'était
// visible que dans le journal.
func preflightEngine() error {
	cfg := ReadConfig()
	// Strata n'a ni BIN ni MODEL : son installeur a tout rangé (backend_strata.go).
	if isStrataConfig(cfg) {
		return strataPreflight(cfg)
	}
	bin := strings.TrimSpace(cfg["BIN"])
	if bin == "" {
		return fmt.Errorf("BIN non défini — installe un moteur : %s (ou renseigne BIN avec %s)",
			bold("loki llamacpp install"), bold("loki edit"))
	}
	if !filepath.IsAbs(bin) {
		bin = filepath.Join(LokiHome(), bin)
	}
	if _, err := os.Stat(prebuiltResolveBin(bin)); err != nil {
		return fmt.Errorf("moteur introuvable : %s — relance %s", bin, bold("loki llamacpp install"))
	}
	model := strings.TrimSpace(cfg["MODEL"])
	if model == "" {
		return fmt.Errorf("MODEL non défini — indique un .gguf avec %s (dossier des modèles : %s)",
			bold("loki edit"), modelsDir())
	}
	p, err := resolveServeModelPath(model)
	if err != nil {
		return fmt.Errorf("MODEL=%s : %w", model, err)
	}
	if _, err := os.Stat(p); err != nil {
		return fmt.Errorf("modèle introuvable : %s — corrige MODEL avec %s", p, bold("loki edit"))
	}
	// Modèle en plusieurs fichiers : une tranche manquante ne se voit qu'au moment
	// où llama-server réclame un tenseur absent, dans le journal du service.
	if missing := shardFamilyMissing(filepath.Dir(p), filepath.Base(p)); len(missing) > 0 {
		return fmt.Errorf("modèle incomplet : il manque %s dans %s — ce modèle tient en %d fichiers, télécharge-les tous",
			strings.Join(missing, ", "), filepath.Dir(p), len(shardFamily(filepath.Base(p))))
	}
	return nil
}

// configTemplate est le squelette commenté proposé par `loki edit` : la
// configuration vit en base, donc sur une installation neuve le fichier
// temporaire était QUASI VIDE — impossible de deviner quoi écrire. On y déroule
// donc les clés utiles avec leur rôle, les valeurs déjà définies telles quelles,
// les autres commentées.
var configTemplate = []struct{ key, help string }{
	{"BIN", "chemin de llama-server (posé par « loki llamacpp install »)"},
	{"MODEL", "nom de fichier .gguf ou chemin complet"},
	{"HOST", "adresse d'écoute du moteur (défaut 0.0.0.0)"},
	{"PORT", "port du moteur (défaut 8080)"},
	{"CTX", "taille du contexte (défaut 32768)"},
	{"NGL", "couches déportées sur le GPU (999 ou auto = ce qui tient en VRAM ; all = tout, quitte à saturer)"},
	{"BATCH", "batch (défaut 2048)"},
	{"UBATCH", "micro-batch (défaut 512)"},
	{"THREADS", "threads CPU ; vide ou 0 = auto (cœurs physiques ; cœurs P seulement sur Intel hybride Linux)"},
	{"THREADS_BATCH", "threads CPU du prefill et de la vérification spéculative (MTP) ; vide ou 0 = comme THREADS"},
	{"FIT_TARGET", "Mio laissés libres par carte par le placement auto (--fit-target), ex. 1024,3072 dans l'ordre des cartes ; " +
		"vide = 1024, minimum 1024 ; sans effet si NGL chiffré, --tensor-split, -ot ou --n-cpu-moe"},
	{"LOAD_GUARD", "off = lancer quand même un --load-mode none/mlock/dio/mmap+mlock (ou --no-mmap, --mlock) dont les poids " +
		"restés en RAM dépassent à coup sûr 90 % de la RAM (limite du conteneur comprise) ; vide (défaut) = refus clair au " +
		"lancement dans ce cas, simple avertissement au-delà de 80 % estimés"},
	{"OP_OFFLOAD_MIN_BATCH", "experts MoE sur CPU : taille de lot à partir de laquelle ils sont recopiés vers le GPU ; " +
		"vide = défaut du moteur (32) ; plus haut = lots courts sur CPU, à mesurer"},
	{"SPLIT_MODE", "tensor = parallélisme de tenseurs entre cartes CUDA (-sm tensor, expérimental) : décodage plus rapide, " +
		"prefill plus lent, à mesurer par tour. Loki pose -ngl all, -ts au prorata de la VRAM, -fa on (sauf réglés à la main) " +
		"et GGML_CUDA_ALLREDUCE=internal (NCCL compresse en BF16, avec perte). Refusé sans la liste {none,layer,row,tensor} " +
		"dans l'aide du moteur, avec un cache KV autre que f16/bf16/f32, des poids ou le KV sur CPU, --backend-sampling, " +
		"-fa off, -sm à la main, SPEC, SIDE_SLOT, CTX non chiffré, une seule carte ou une VRAM estimée insuffisante. " +
		"Vide/off (défaut) = découpe par couches"},
	{"CUDA_GRAPH_OPT", "on = GGML_CUDA_GRAPH_OPT=1, branches Q/K/V en parallèle au décodage (expérimental, " +
		"sortie identique, gain de 0 à quelques %) ; off puis redémarrage s'il plante"},
	{"KV_TYPE", "quantization du cache KV ; vide = f16, sorties de référence ; q8_0 les modifie légèrement, " +
		"q4_0 avec perte mesurable ; KV_TYPE_K / KV_TYPE_V pour les séparer"},
	{"CACHE_RAM", "cache de prompts en RAM hôte (--cache-ram, Mio), copie exacte des conversations quittées, jamais la VRAM ; " +
		"vide = auto (agrandi seulement pour un modèle tout-GPU), -1 = sans limite, 0 = coupé"},
	{"CACHE_ISOLATE", "off = ne plus effacer le slot après un sous-agent, une vérification, une tâche ou un bench"},
	{"SIDE_SLOT", "on = second slot pour les travaux annexes : le moteur ouvre 2 slots de CTX jetons chacun (-c 2×CTX, " +
		"--parallel 2, cache KV non unifié : aucun slot ne déborde sur l'autre), la discussion garde le slot 0, vérification, " +
		"sous-agents, tâches, bench, résumé sur transcription et clients /v1 passent par le 1 (id_slot) — l'état de la " +
		"discussion ne bouge plus. Cache KV et état récurrent en double en VRAM : refusé si le compte ne tient pas, si des " +
		"poids sont sur CPU, ou si PARALLEL≠2, -c, -np, -kvu, --cache-idle-slots… sont réglés à la main. Vide/off (défaut) = un slot"},
	{"SLOT_PERSIST", "on = garder sur disque (LOKI_HOME/slots, 2 fichiers au plus, 8 Gio chacun au plus) l'état du slot de " +
		"la discussion à la bascule de preset, et le recharger au retour avant son premier message : moteur, modèle et réglages " +
		"identiques à l'octet près (empreinte), slot encore vierge, sinon calcul normal. Refusé avec le décodage spéculatif, " +
		"des poids sur CPU, ou un moteur joignable sans clé d'API hors 127.0.0.1. Jamais à travers une mise à jour du moteur. " +
		"Réglage de machine, gardé à la bascule. Vide/off (défaut) = rien n'est écrit"},
	{"CTX_CHECKPOINTS", "points de reprise par slot d'un modèle hybride (--ctx-checkpoints) ; vide = défaut du moteur (32) ; " +
		"chacun pèse 70 à 200 Mio de RAM hôte : 8 à 16 si le modèle remplit déjà la RAM"},
	{"CKPT_MIN_STEP", "espacement minimal en jetons entre deux points de reprise (--checkpoint-min-step), > 0 ; " +
		"vide = défaut du moteur, ou 2048 d'office sur un hybride avec un moteur officiel antérieur à b10864"},
	{"SPEC", "décodage spéculatif, sortie inchangée : off (défaut) / auto = tête MTP du modèle ou MODEL_DRAFT si --fit place " +
		"tout et qu'aucun essai n'a échoué / mtp = imposé ; ~1-2 Go de VRAM en plus / ngram = n-grammes du contexte " +
		"(ngram-mod 24/48/64, sans brouillon, utile en mode code, à mesurer ; refusé avec des poids sur CPU sauf " +
		"OP_OFFLOAD_MIN_BATCH ≥ 66) / mtp+ngram = les deux, n-grammes seuls si pas de tête MTP"},
	{"MODEL_DRAFT", "tête MTP publiée à part (mtp-*.gguf) ou petit modèle brouillon, nom ou chemin comme MODEL ; utilisé avec SPEC=auto, mtp ou mtp+ngram"},
	{"SPEC_N_MAX", "jetons anticipés par étape (--spec-draft-n-max) ; vide = défaut du moteur (3)"},
	{"SPEC_SAMPLING", "tirage du brouillon : greedy (défaut, exact) ; probabilistic seulement avec SPEC=mtp ou mtp+ngram"},
	{"REASONING", "passthrough du mode raisonnement (on/auto/deepseek)"},
	{"REASONING_PRESERVE", "on/off = garder ou non la réflexion des tours passés dans le gabarit (--reasoning-preserve) ; " +
		"vide = défaut du moteur (on depuis b10763). Sans REASONING_ECHO, Loki ne renvoie pas cette réflexion : off rend " +
		"l'ancien historique aux gabarits type Qwen3.6, mais change celui des gabarits qui la gardent d'eux-mêmes (Qwen3.8). " +
		"Avec REASONING_ECHO=on, on = préfixe stable d'un message à l'autre (mode entraîné mais optionnel selon la fiche " +
		"Qwen3.6), au prix de plus de contexte par tour, donc d'une compaction plus tôt"},
	{"REASONING_ECHO", "on = renvoyer au moteur local le raisonnement du modèle (reasoning_content), au format entraîné : " +
		"les étapes d'une boucle d'outils se relisent avec leur réflexion au lieu de blocs vides, et le moteur ne recalcule " +
		"plus le dernier message ; plus de contexte par tour, compaction plus tôt. Vide/off (défaut) = rien n'est renvoyé. " +
		"Jamais vers une API externe ni vers un autre modèle ; suspendu de lui-même si le gabarit le refuse"},
	{"PROJ_SNAPSHOT", "on = figer le bloc projet (description, index mémoire, trackers, AGENTS.md) par discussion et livrer " +
		"ses changements en <context_update> en tête du message suivant : une page créée ou une valeur de tracker ne fait plus " +
		"recalculer toute la conversation. Repris tout neuf à chaque compaction, redémarrage ou changement de modèle/projet. " +
		"Vide/off (défaut) = bloc reconstruit à chaque tour ; sans effet sur un preset externe"},
	{"PREWARM", "on = préparer le prochain tour pendant que tu lis : après un tour (compaction comprise) ou une tâche, " +
		"Loki envoie au moteur local la requête suivante avec un message « . » et 1 jeton de réponse, jetés ; le vrai message " +
		"ne calcule plus que lui-même. Annulé dès qu'une autre requête part, sauf si elle prolonge exactement ce préfixe ; " +
		"jamais avec plus d'un slot (sauf les deux de SIDE_SLOT : slot de la discussion). full = aussi au changement de discussion " +
		"(après 3 s). Vide/off (défaut) = aucune requête de plus"},
	{"KEEP_TURN_IMAGES", "zone grise — on = garder dans l'historique les images montrées par les outils (captures, see_image), " +
		"rangées par référence, au lieu de les oublier en fin de tour : le tour suivant ne recalcule plus la boucle d'outils " +
		"depuis la première capture. Coût mesuré par le moteur, total borné à 10 % de la fenêtre, retirées d'abord à toute " +
		"compaction (qui arrive donc plus tôt) ; vision active seulement, preset externe s'il déclare la vision (images " +
		"refacturées à chaque tour). Vide/off (défaut) = images éphémères, requête inchangée"},
	{"NUDGE_IN_TOOL", "zone grise — on = le rappel de budget d'outils part au bout du dernier résultat d'outil au lieu d'un " +
		"message à part, sur le moteur local et seulement si la sonde de gabarit dit que le rendu bouge quand un message " +
		"s'ajoute (Qwen3.5) : la boucle d'outils du tour n'est plus recalculée. Un modèle peut moins bien suivre une consigne " +
		"lue dans une sortie d'outil : à comparer avant de l'adopter. Vide/off (défaut) = message à part"},
	{"REASONING_BUDGET", "plafond de tokens de réflexion ; -1 = illimité"},
	{"REASONING_EFFORT", "intensité du raisonnement : vide (auto) / none / low / medium / high / xhigh"},
	{"TEMP", "température d'échantillonnage ; vide = défaut du moteur"},
	{"TOP_P", "noyau de probabilité (top_p) ; vide = défaut du moteur"},
	{"TOP_K", "top_k ; 0 = désactivé, vide = défaut du moteur"},
	{"MIN_P", "seuil de probabilité (min_p) ; vide = défaut du moteur"},
	{"PRESENCE_PENALTY", "pénalité de présence ; vide = défaut du moteur"},
	{"REPEAT_PENALTY", "pénalité de répétition (1 = neutre) ; vide = défaut du moteur"},
	{"COMPACT", "compactage automatique du contexte (off pour couper)"},
	{"COMPACT_CONTINUATION", "on = le résumé d'une compaction prolonge la requête du tour que le moteur local a en cache " +
		"(mêmes messages, outils et réglages du gabarit, plus une demande de résumé) au lieu d'une transcription calculée à froid ; " +
		"repli sur la transcription au moindre écart (autre requête passée par le slot, marge, refus, appel d'outil, résumé vide). " +
		"Ne redemande pas un résumé voué au refus. Vide/off (défaut) = compaction d'avant ; sans effet sur un preset externe"},
	{"MEM_MODE", "mémoire de l'IA : off / ondemand / always"},
	{"EXTRA_ARGS", "ajouté tel quel à la ligne de commande de llama-server"},
}

// configEditorText rend la configuration au format présenté dans $EDITOR.
func configEditorText(cfg map[string]string) string {
	var b strings.Builder
	b.WriteString("# Configuration du moteur Loki (loki-engine).\n")
	b.WriteString("# Une clé par ligne : CLE=valeur. Les lignes commentées (#) sont ignorées :\n")
	b.WriteString("# décommente celles dont tu as besoin. « loki restart » applique.\n\n")
	seen := map[string]bool{}
	for _, f := range configTemplate {
		seen[f.key] = true
		fmt.Fprintf(&b, "# %s\n", f.help)
		if v, ok := cfg[f.key]; ok && v != "" {
			fmt.Fprintf(&b, "%s=%s\n\n", f.key, quoteValue(v))
		} else {
			fmt.Fprintf(&b, "#%s=\n\n", f.key)
		}
	}
	// Tout ce que le squelette ne connaît pas (clés d'une version plus récente,
	// réglages posés par l'UI) : conservé tel quel, en fin de fichier.
	rest := map[string]string{}
	for k, v := range cfg {
		if !seen[k] {
			rest[k] = v
		}
	}
	if len(rest) > 0 {
		b.WriteString("# --- autres clés déjà définies ---\n")
		b.WriteString(formatEnv(rest))
	}
	return b.String()
}

// editConfig ouvre la configuration dans $EDITOR. La configuration vit en base
// (voir store.go) : on la déroule dans un fichier temporaire au format clé=valeur,
// on laisse l'éditeur faire son travail, puis on relit. Le contenu n'est réécrit
// que si l'éditeur sort proprement — un éditeur avorté ne doit rien effacer.
func editConfig() error {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = defaultEditor()
	}
	tmp, err := os.CreateTemp("", "loki-config-*.env")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.WriteString(configEditorText(ReadConfig())); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	cmd := exec.Command(editor, path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := WriteConfig(parseEnv(string(b))); err != nil {
		return err
	}
	fmt.Println(dim("[info] loki restart pour appliquer"))
	return nil
}

// showVram parses `nvidia-smi --query-gpu=...` and renders a colored bar.
// nvidia-smi is available on both Linux and Windows when an NVIDIA driver is
// installed, so this is platform-neutral.
func showVram() error {
	out, err := hideCmd(exec.Command("nvidia-smi",
		"--query-gpu=name,memory.used,memory.total,utilization.gpu,temperature.gpu",
		"--format=csv,noheader,nounits")).Output()
	if err != nil {
		return fmt.Errorf("nvidia-smi indisponible: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, ",")
		if len(parts) != 5 {
			continue
		}
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		name := parts[0]
		used, _ := strconv.Atoi(parts[1])
		total, _ := strconv.Atoi(parts[2])
		util, _ := strconv.Atoi(parts[3])
		temp, _ := strconv.Atoi(parts[4])
		pct := 0
		if total > 0 {
			pct = used * 100 / total
		}
		full := pct / 5
		bar := strings.Repeat("█", full) + strings.Repeat("░", 20-full)
		fmt.Printf("\n  %s\n", cyan(name))
		fmt.Printf("  VRAM  %s  %3d%%   %.1f / %.1f GiB\n", green(bar), pct, float64(used)/1024, float64(total)/1024)
		fmt.Printf("  GPU   %3d%%      Temp  %d°C\n\n", util, temp)
	}
	return nil
}
