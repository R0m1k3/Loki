package loki

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Décodage spéculatif géré par Loki : une tête MTP (prédiction de plusieurs
// jetons) intégrée au modèle ou publiée à part, ou un petit modèle brouillon.
//
//	SPEC=off (défaut)  → rien, même avec MODEL_DRAFT
//	SPEC=auto          → seulement si tout est réuni (voir specAutoBlocker)
//	SPEC=mtp           → imposé, sans les garde-fous d'auto, avec un avertissement
//	MODEL_DRAFT=<nom>  → la tête ou le brouillon, résolu comme MMPROJ
//	SPEC_N_MAX=<n>     → --spec-draft-n-max (défaut du moteur : 3)
//	SPEC_SAMPLING=probabilistic → seulement avec SPEC=mtp ; greedy sinon
//
// Rien n'y touche au modèle : chaque jeton émis est tiré par l'échantillonneur
// du modèle cible, un jeton du brouillon n'est gardé que s'il coïncide
// (common_sampler_sample_and_accept_n). Le prix est ailleurs : 1 à 2 Go de VRAM
// en plus, un prefill parfois plus lent, et une fonction très récente qui peut
// empêcher le moteur de démarrer. D'où l'opt-in, les garde-fous d'auto et le
// jeton de tentative (specAutoVerdict) : un lancement automatique qui n'a
// jamais répondu ne se retente pas avec la même configuration.

const (
	// specAutoMinBuild : premier build officiel où Loki ose l'auto (MTP serveur,
	// PR #22673 et suites). Build inconnu = non : un compilé maison ou un fork
	// peut connaître le drapeau sans le tenir.
	specAutoMinBuild = 11009
	specNMaxLimit    = 64
)

// specArgEnv : les variables qui règlent la spéculation sans drapeau. Posées,
// elles valent un réglage d'EXTRA_ARGS : Loki se tait.
var specArgEnv = []string{"LLAMA_ARG_SPEC_TYPE", "LLAMA_ARG_SPEC_DRAFT_MODEL", "LLAMA_ARG_SPEC_DRAFT_HF_REPO",
	"LLAMA_ARG_SPEC_DRAFT_N_MAX", "LLAMA_ARG_SPEC_DRAFT_SAMPLING"}

// specUserFlags : la spéculation réglée à la main. Les ajouter EN PLUS ferait
// cumuler les types (--spec-type s'additionne) ou charger deux brouillons.
var specUserFlags = []string{"--spec-type", "-md", "--model-draft", "--spec-draft-model",
	"-hfd", "-hfrd", "--spec-draft-hf", "--hf-repo-draft", "--spec-default"}

// probeSpec : variables du moteur, puis MODEL_DRAFT résolu comme MMPROJ (nom
// simple cherché dans les dossiers déclarés, ou chemin absolu). Introuvable ne
// bloque PAS le lancement, contrairement au projecteur : le brouillon n'est
// qu'une accélération, le moteur démarre sans et la note dit pourquoi.
func probeSpec(cfg map[string]string, si *serveSysInfo) {
	if si.ArgEnv == nil {
		si.ArgEnv = map[string]string{}
	}
	for _, k := range specArgEnv {
		if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
			si.ArgEnv[k] = v
		}
	}
	d := strings.TrimSpace(cfg["MODEL_DRAFT"])
	if d == "" {
		return
	}
	p, err := resolveServeModelPath(d)
	if err == nil {
		_, err = os.Stat(p)
	}
	if err != nil {
		si.DraftErr = err.Error()
		return
	}
	si.Draft = p
	if g, err := ggufMeta(p); err == nil {
		si.DraftGGUF = &g
	}
}

// specMode lit SPEC : "off", "auto", "mtp", ou "" si illisible.
func specMode(cfg map[string]string) string {
	switch v := strings.ToLower(strings.TrimSpace(cfg["SPEC"])); v {
	case "", "off", "non", "no", "0":
		return "off"
	case "auto", "mtp":
		return v
	}
	return ""
}

// specUserSet : EXTRA_ARGS ou l'environnement règlent déjà la spéculation.
func specUserSet(extra []string, argEnv map[string]string) bool {
	if hasAnyFlag(extra, specUserFlags...) {
		return true
	}
	for _, k := range []string{"LLAMA_ARG_SPEC_TYPE", "LLAMA_ARG_SPEC_DRAFT_MODEL", "LLAMA_ARG_SPEC_DRAFT_HF_REPO"} {
		if argEnv[k] != "" {
			return true
		}
	}
	return false
}

// helpSupportsMTP : le moteur connaît le type draft-mtp ET le drapeau actuel
// du nombre de jetons — un moteur qui n'a que l'ancien --draft-max est d'une
// autre génération, où un MTP n'existe pas.
func helpSupportsMTP(help string) bool {
	return strings.Contains(help, "draft-mtp") && strings.Contains(help, "--spec-draft-n-max")
}

// ctxFixed : le contexte que verra le moteur (EXTRA_ARGS, sinon CTX, sinon le
// défaut de buildServeArgs), et s'il est chiffré. Avec 0 ou une valeur
// illisible, --fit a le droit de RÉDUIRE le contexte pour tenir.
func ctxFixed(cfg map[string]string, extra []string) (string, bool) {
	c := flagValue(extra, "-c", "--ctx-size")
	if c == "" {
		c = strings.TrimSpace(cfg["CTX"])
	}
	if c == "" {
		c = "32768"
	}
	n, err := strconv.Atoi(c)
	return c, err == nil && n > 0
}

// statefulSampler : un échantillonneur dont l'état avance à chaque tirage
// (mirostat, adaptive-p). Le rejet probabiliste fait d'abord tirer le modèle
// cible, puis peut émettre un autre jeton : ces échantillonneurs avanceraient
// sur un jeton que la réponse ne contient pas. Le mode greedy, lui, reste exact.
func statefulSampler(extra []string) string {
	for _, v := range flagValues(extra, "--mirostat") {
		if v = strings.TrimSpace(v); v != "" && v != "0" {
			return "--mirostat " + v
		}
	}
	for _, v := range flagValues(extra, "--samplers") {
		if strings.Contains(strings.ToLower(strings.ReplaceAll(v, "-", "_")), "adaptive_p") {
			return "--samplers adaptive-p"
		}
	}
	for _, v := range flagValues(extra, "--sampling-seq", "--sampler-seq") {
		if strings.Contains(v, "a") {
			return "--sampling-seq avec adaptive-p"
		}
	}
	return ""
}

// specAutoBlocker dit pourquoi SPEC=auto ne s'active pas, ou vide. Chaque
// garde répond à une panne précise :
//
//   - qwen4exp : MTP fusionné il y a quelques jours, OOM signalés en multi-GPU ;
//   - placement manuel (couches fixées, -ts, -ot, experts sur CPU, -sm row,
//     --fit off, -dev) : seul --fit compte la VRAM de la tête MTP ;
//   - contexte non chiffré : fit aurait le droit de le RÉDUIRE pour faire de
//     la place — de l'information perdue pour le modèle ;
//   - projecteur vision : une VRAM déjà serrée par un second modèle ;
//   - build officiel inconnu ou trop ancien ;
//   - un essai précédent avec la même configuration n'a jamais répondu.
func specAutoBlocker(cfg map[string]string, extra []string, si serveSysInfo) string {
	if si.GGUF != nil && strings.EqualFold(si.GGUF.Arch, "qwen4exp") {
		return "qwen4exp : MTP trop récent pour ce moteur, SPEC=mtp pour l'imposer"
	}
	if why := fitBlocker(cfg, extra, si); why != "" {
		return "placement manuel (" + why + ") — --fit ne compte pas la VRAM du brouillon"
	}
	if hasAnyFlag(extra, "-dev", "--device") || si.ArgEnv["LLAMA_ARG_DEVICE"] != "" {
		return "placement manuel (--device) — --fit ne compte pas la VRAM du brouillon"
	}
	if _, ok := ctxFixed(cfg, extra); !ok {
		return "contexte non chiffré — fit pourrait le réduire pour loger le brouillon"
	}
	if si.MMProj != "" || hasAnyFlag(extra, "--mmproj", "-mm") {
		return "projecteur vision chargé"
	}
	if si.EngineBuild < specAutoMinBuild {
		if si.EngineBuild <= 0 {
			return "build du moteur inconnu (compilé ou fork)"
		}
		return fmt.Sprintf("moteur b%d, b%d au moins", si.EngineBuild, specAutoMinBuild)
	}
	return si.SpecAutoBlocked
}

// specArgs : drapeaux de décodage spéculatif que Loki ajoute, ce qu'il en dit,
// et s'ils viennent de SPEC=auto (cmdServe pose alors le jeton de tentative).
// Fonction pure ; cmdServe a déjà résolu MODEL_DRAFT et lu les GGUF.
func specArgs(cfg map[string]string, extra []string, si serveSysInfo) (args, notes []string, auto bool) {
	mode := specMode(cfg)
	draftKey := strings.TrimSpace(cfg["MODEL_DRAFT"])
	switch mode {
	case "":
		return nil, []string{"SPEC=" + cfg["SPEC"] + " illisible (off, auto ou mtp) : sans décodage spéculatif"}, false
	case "off":
		if draftKey != "" {
			return nil, []string{"MODEL_DRAFT ignoré : SPEC=off (auto ou mtp pour s'en servir)"}, false
		}
		return nil, nil, false
	}
	if specUserSet(extra, si.ArgEnv) {
		return nil, []string{"décodage spéculatif déjà réglé dans EXTRA_ARGS ou LLAMA_ARG_* : SPEC ignoré"}, false
	}
	skip := func(why string) ([]string, []string, bool) {
		return nil, []string{"SPEC=" + mode + " : " + why + " — sans décodage spéculatif"}, false
	}

	// La source : MODEL_DRAFT s'il est posé, sinon la tête MTP du modèle. Dans
	// les deux cas, on regarde les TENSEURS, comme llama.cpp : une clé
	// nextn_predict_layers seule ne prouve pas que la tête est dans le fichier.
	mtp := false
	switch {
	case draftKey != "":
		switch {
		case si.Draft == "":
			why := "MODEL_DRAFT=" + draftKey + " introuvable"
			if si.DraftErr != "" {
				why += " (" + si.DraftErr + ")"
			}
			return skip(why)
		case si.DraftGGUF == nil:
			return skip("MODEL_DRAFT illisible (GGUF incomplet ou en cours de téléchargement ?)")
		case si.DraftGGUF.HasNextNTensor:
			if !helpSupportsMTP(si.Help) {
				return skip("ce moteur ne connaît pas draft-mtp")
			}
			// Type explicite : llama.cpp ne le devine que sur la première tranche.
			args, mtp = []string{"-md", si.Draft, "--spec-type", "draft-mtp"}, true
		default:
			if !strings.Contains(si.Help, "--spec-type") || !strings.Contains(si.Help, "draft-simple") {
				return skip("ce moteur ne connaît pas --spec-type draft-simple")
			}
			// Sans type, un brouillon qui n'est pas une tête MTP serait chargé en
			// VRAM puis jamais utilisé.
			args = []string{"-md", si.Draft, "--spec-type", "draft-simple"}
		}
	case si.GGUF == nil:
		return skip("métadonnées du modèle illisibles")
	case !si.GGUF.HasNextNTensor:
		if mode == "mtp" || si.GGUF.NextN > 0 {
			return skip("pas de tête MTP dans ce fichier (publiée à part ? MODEL_DRAFT=mtp-….gguf)")
		}
		return nil, nil, false // auto sur un modèle sans MTP : rien à dire
	case !helpSupportsMTP(si.Help):
		return skip("ce moteur ne connaît pas draft-mtp")
	default:
		args, mtp = []string{"--spec-type", "draft-mtp"}, true
	}

	label := "brouillon " + baseName(si.Draft)
	if mtp {
		label = "tête MTP"
	}
	if mode == "auto" {
		if why := specAutoBlocker(cfg, extra, si); why != "" {
			return skip(why)
		}
		auto = true
		notes = append(notes, "SPEC=auto → "+label+" : sortie inchangée (chaque jeton est vérifié par le modèle), "+
			"~1-2 Go de VRAM en plus. Mesure le prefill ; SPEC=off pour couper.")
	} else {
		note := "SPEC=mtp → " + label + " imposé : sortie inchangée, ~1-2 Go de VRAM en plus"
		if why := fitBlocker(cfg, extra, si); why != "" || hasAnyFlag(extra, "-dev", "--device") {
			if why == "" {
				why = "--device"
			}
			note += " ; placement manuel (" + why + ") : --fit ne compte pas le brouillon, " +
				"place-le avec -devd / -ngld si la VRAM manque"
		}
		if _, ok := ctxFixed(cfg, extra); !ok {
			note += " ; contexte non chiffré : fit pourrait le réduire pour loger le brouillon"
		}
		if si.GGUF != nil && strings.EqualFold(si.GGUF.Arch, "qwen4exp") {
			note += " ; MTP qwen4exp très récent"
		}
		notes = append(notes, note)
	}

	// Nombre de jetons anticipés : défaut du moteur (3) si la clé est vide.
	if v := strings.TrimSpace(cfg["SPEC_N_MAX"]); v != "" {
		n, err := strconv.Atoi(v)
		switch {
		case err != nil || n < 1 || n > specNMaxLimit:
			notes = append(notes, "SPEC_N_MAX="+v+" illisible (1 à 64 jetons) : défaut du moteur")
		case !strings.Contains(si.Help, "--spec-draft-n-max"):
			notes = append(notes, "ce moteur ne connaît pas --spec-draft-n-max : SPEC_N_MAX ignoré")
		case hasAnyFlag(extra, "--spec-draft-n-max") || si.ArgEnv["LLAMA_ARG_SPEC_DRAFT_N_MAX"] != "":
		default:
			args = append(args, "--spec-draft-n-max", strconv.Itoa(n))
		}
	}

	// Tirage du brouillon. greedy est exact quelle que soit la chaîne
	// d'échantillonnage : on le fixe dès que le moteur connaît le drapeau, pour
	// qu'un changement de défaut en amont ne change rien ici.
	sampling := "greedy"
	switch v := strings.ToLower(strings.TrimSpace(cfg["SPEC_SAMPLING"])); v {
	case "", "greedy":
	case "probabilistic":
		switch {
		case mode != "mtp":
			notes = append(notes, "SPEC_SAMPLING=probabilistic ignoré avec SPEC=auto (SPEC=mtp pour le choisir) : greedy")
		case statefulSampler(extra) != "":
			notes = append(notes, "SPEC_SAMPLING=probabilistic refusé avec "+statefulSampler(extra)+
				" (cet échantillonneur avancerait sur des jetons rejetés) : greedy")
		case !strings.Contains(si.Help, "probabilistic"):
			notes = append(notes, "ce moteur ne connaît pas --spec-draft-sampling probabilistic : greedy")
		default:
			sampling = v
		}
	default:
		notes = append(notes, "SPEC_SAMPLING="+v+" illisible (greedy ou probabilistic) : greedy")
	}
	if strings.Contains(si.Help, "--spec-draft-sampling") && !hasAnyFlag(extra, "--spec-draft-sampling") &&
		si.ArgEnv["LLAMA_ARG_SPEC_DRAFT_SAMPLING"] == "" {
		args = append(args, "--spec-draft-sampling", sampling)
	}
	return args, notes, auto
}

// --- Jeton de tentative ------------------------------------------------------
//
// Un essai automatique qui fait tomber le moteur ne doit pas se rejouer : sous
// systemd il redémarrerait en boucle en rechargeant 15 Go à chaque fois. cmdServe
// ne peut pas constater l'échec lui-même (sous Unix il DEVIENT llama-server).
// Il pose donc un jeton avant de lancer ; le process web l'efface dès que le
// moteur répond. Au lancement suivant, un jeton encore là pour la même
// configuration, le même moteur et le même build veut dire « jamais répondu » :
// l'auto est coupé pour cette combinaison, et on le dit. Une mise à jour du
// moteur ou un preset modifié retente. Sans process web (CLI seule), le jeton
// n'est jamais effacé : on retombe sur off, le côté sûr.

const (
	specAttemptKey = "spec_auto_attempt"
	specFailedKey  = "spec_auto_failed"
	specFailedMax  = 32
)

// specAutoMark identifie une combinaison preset × moteur.
type specAutoMark struct {
	FP    string `json:"fp"`
	Bin   string `json:"bin"`
	Build int    `json:"build"`
}

func (m specAutoMark) key() string { return fmt.Sprintf("%s|%s|%d", m.FP, m.Bin, m.Build) }

// specAutoVerdict : pure. Renvoie la raison de couper l'auto (vide = permis)
// et s'il faut inscrire cur parmi les échecs.
func specAutoVerdict(cur specAutoMark, attempt *specAutoMark, failed map[string]string) (why string, record bool) {
	if w := failed[cur.key()]; w != "" {
		return w, false
	}
	if attempt != nil && attempt.key() == cur.key() {
		return "le dernier lancement avec cette configuration n'a jamais répondu", true
	}
	return "", false
}

// specAutoCheck lit l'état, tranche et range : jeton consommé, échec inscrit.
func specAutoCheck(cur specAutoMark) string {
	why := ""
	_ = update(bkState, func(b *bolt.Bucket) error {
		var attempt *specAutoMark
		if raw := b.Get([]byte(specAttemptKey)); raw != nil {
			var a specAutoMark
			if json.Unmarshal(raw, &a) == nil {
				attempt = &a
			}
		}
		failed := map[string]string{}
		if raw := b.Get([]byte(specFailedKey)); raw != nil {
			_ = json.Unmarshal(raw, &failed)
		}
		w, record := specAutoVerdict(cur, attempt, failed)
		why = w
		if attempt != nil {
			_ = b.Delete([]byte(specAttemptKey))
		}
		if !record {
			return nil
		}
		return putFailed(b, failed, cur.key(), w)
	})
	return why
}

func putFailed(b *bolt.Bucket, failed map[string]string, key, why string) error {
	if len(failed) >= specFailedMax {
		failed = map[string]string{}
	}
	failed[key] = why
	raw, err := json.Marshal(failed)
	if err != nil {
		return err
	}
	return b.Put([]byte(specFailedKey), raw)
}

// specAutoAttempt pose le jeton juste avant de lancer le moteur.
func specAutoAttempt(cur specAutoMark) {
	_ = putJSON(bkState, specAttemptKey, cur)
}

// --- Côté process web --------------------------------------------------------

var (
	specWatchOnce sync.Once
	offloadedRe   = regexp.MustCompile(`offloaded (\d+)/(\d+) layers to GPU`)
)

// startSpecAttemptWatch : une goroutine qui efface le jeton dès que le moteur
// répond, sans attendre qu'une page interroge /api/status.
func startSpecAttemptWatch() {
	specWatchOnce.Do(func() {
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for range t.C {
				specAttemptTick()
			}
		}()
	})
}

func specAttemptTick() {
	var a specAutoMark
	if !getJSON(bkState, specAttemptKey, &a) {
		return
	}
	if externalActive() {
		_ = putBytes(bkState, specAttemptKey, nil) // plus de moteur local à attendre
		return
	}
	if !healthCheck() {
		return
	}
	// Répondu. Mais avec -c fixé, fit ne réduit pas le contexte : s'il manque
	// la place du brouillon, il descend des couches en RAM — le moteur tourne,
	// plus lentement que sans MTP. Ça compte aussi comme un échec de l'auto.
	why := ""
	if n, m := lastOffload(serviceLogTail(600)); m > 0 && n < m {
		why = fmt.Sprintf("seulement %d/%d couches sur GPU avec le brouillon : fit a déplacé des couches en RAM", n, m)
		fmt.Println("[loki] SPEC=auto : " + why + " — coupé au prochain démarrage (SPEC=mtp pour l'imposer)")
	}
	_ = update(bkState, func(b *bolt.Bucket) error {
		_ = b.Delete([]byte(specAttemptKey))
		if why == "" {
			return nil
		}
		failed := map[string]string{}
		if raw := b.Get([]byte(specFailedKey)); raw != nil {
			_ = json.Unmarshal(raw, &failed)
		}
		return putFailed(b, failed, a.key(), why)
	})
}

// lastOffload lit « offloaded N/M layers to GPU » du DERNIER chargement du
// journal (après la dernière ligne load_model). 0, 0 = introuvable : on ne
// conclut rien.
func lastOffload(log string) (n, m int) {
	lines := strings.Split(log, "\n")
	start := -1
	for i, l := range lines {
		if strings.Contains(l, "loading model") || strings.Contains(l, "load_model") {
			start = i
		}
	}
	if start < 0 {
		return 0, 0
	}
	for _, l := range lines[start:] {
		if s := offloadedRe.FindStringSubmatch(l); s != nil {
			n, _ = strconv.Atoi(s[1])
			m, _ = strconv.Atoi(s[2])
		}
	}
	return n, m
}
