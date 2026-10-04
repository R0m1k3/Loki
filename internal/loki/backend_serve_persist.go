package loki

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// État du slot gardé à travers une bascule de preset (SLOT_PERSIST, off par
// défaut).
//
// Basculer d'un preset A vers B puis revenir à A relance llama-server deux
// fois : au retour, la conversation de A est recalculée en entier — 30 à 80 s
// sur un 27B dense. Avec SLOT_PERSIST=on, Loki demande au moteur, juste avant
// la bascule, d'écrire l'état du slot 0 sur disque (/slots/0?action=save), et
// le recharge au retour, avant la première requête de la discussion
// (llm_slotpersist.go). llama-server ne reprend un état que sur un préfixe de
// jetons identique : au pire, rien n'est repris.
//
// Mais rien, dans le fichier, ne dit quel moteur ni quels réglages ont calculé
// ce cache : llama.cpp ne vérifie que les types et les tailles. Le recharger
// sous un autre build, une autre fenêtre ou un autre placement donnerait au
// modèle un contexte que ce moteur n'aurait jamais produit. D'où la clé : une
// empreinte de TOUT ce qui touche au calcul — ligne de commande complète
// (modèle, fenêtre, types KV, NGL, lots, gabarit, EXTRA_ARGS…), variables
// LLAMA_ARG_*, GGML_* et CUDA_*, identité (taille, date) du modèle et de ses
// tranches, du projecteur, du brouillon, du binaire et de ses bibliothèques.
// Un fichier ne se recharge que sous la même clé ; une mise à jour du moteur
// change donc tout, et n'est jamais un cas de reprise.
//
// Refusé d'emblée : le décodage spéculatif (la sauvegarde de llama.cpp ne
// garde que le modèle cible, pas le brouillon MTP — l'état rechargé serait
// incohérent pour lui), les poids sur CPU (MoE déporté : états de plusieurs
// Gio pour un recalcul de toute façon lent), et un moteur joignable par
// d'autres sans clé d'API : save et restore écrivent des Gio sur le disque.

// slotPersistOn : la clé SLOT_PERSIST est-elle posée ?
func slotPersistOn(cfg map[string]string) bool {
	switch strings.ToLower(strings.TrimSpace(cfg["SLOT_PERSIST"])) {
	case "on", "1", "true", "yes", "oui":
		return true
	}
	return false
}

// slotPersistArgEnv : variables qui contredisent SLOT_PERSIST sans drapeau.
var slotPersistArgEnv = []string{"LLAMA_ARG_SLOT_SAVE_PATH", "LLAMA_ARG_SPEC_TYPE", "LLAMA_ARG_SPEC_DRAFT_MODEL",
	"LLAMA_ARG_SPEC_DRAFT_HF_REPO"}

// probeSlotPersistEnv complète ArgEnv, seulement si la clé est posée.
func probeSlotPersistEnv(cfg map[string]string, si *serveSysInfo) {
	if !slotPersistOn(cfg) {
		return
	}
	if si.ArgEnv == nil {
		si.ArgEnv = map[string]string{}
	}
	for _, k := range slotPersistArgEnv {
		if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
			si.ArgEnv[k] = v
		}
	}
}

// slotPersistPlan : SLOT_PERSIST vaut-il pour ce lancement ? why dit pourquoi
// non (vide = clé absente).
func slotPersistPlan(cfg map[string]string, extra []string, si serveSysInfo) (ok bool, why string) {
	if !slotPersistOn(cfg) {
		return false, ""
	}
	switch {
	case isExternalConfig(cfg):
		return false, "preset externe"
	case !strings.Contains(si.Help, "--slot-save-path"):
		return false, "ce moteur ne connaît pas --slot-save-path"
	case hasAnyFlag(extra, "--slot-save-path") || si.ArgEnv["LLAMA_ARG_SLOT_SAVE_PATH"] != "":
		return false, "--slot-save-path est déjà réglé à la main"
	case si.SlotDir == "":
		return false, "dossier des slots indisponible"
	case specMode(cfg) != "off" || specUserSet(extra, si.ArgEnv) || si.ArgEnv["LLAMA_ARG_SPEC_TYPE"] != "":
		return false, "décodage spéculatif : la sauvegarde du moteur ne garde pas l'état du brouillon"
	case splitModeKey(cfg) != "":
		return false, "SPLIT_MODE : état réparti entre cartes, sauvegarde non validée"
	}
	if !slotSaveSafe(cfg, extra, si) {
		return false, "moteur joignable par d'autres sans clé d'API (HOST=127.0.0.1 ou une clé d'API)"
	}
	blocks := 0
	if si.GGUF != nil {
		blocks = si.GGUF.BlockCount
	}
	if r := cpuWeights(cfg, extra, si.ArgEnv, blocks); r != "" {
		return false, r
	}
	return true, ""
}

// slotSaveSafe : le moteur n'est joignable que par la machine, ou exige une
// clé d'API. Sans ça, --slot-save-path donnerait à n'importe qui le droit
// d'écrire des Gio sur le disque.
func slotSaveSafe(cfg map[string]string, extra []string, si serveSysInfo) bool {
	host := flagValue(extra, "--host")
	if host == "" {
		host = cfg["HOST"]
	}
	if host == "" {
		host = "0.0.0.0"
	}
	return loopbackHost(host) || si.APIKey != ""
}

// slotPersistNote : ce que le lancement dit de SLOT_PERSIST.
func slotPersistNote(ok bool, why string) string {
	if ok || why == "" {
		return ""
	}
	return "SLOT_PERSIST refusé (" + why + ") : l'état du slot ne sera pas gardé à la bascule de preset"
}

// slotPersistFileRe : les seuls fichiers que Loki écrit dans le dossier des
// slots — clé du moteur, empreinte de la discussion.
var slotPersistFileRe = regexp.MustCompile(`^persist-[0-9a-f]{16}-[0-9a-f]{12}\.bin$`)

// slotPersistMaxFiles : au plus deux états gardés (A→B→A, puis B→A→B).
const slotPersistMaxFiles = 2

// slotPersistKey : l'empreinte de tout ce qui décide du calcul du cache.
// args : la ligne de commande finale (la clé d'API n'y entre pas) ; env : ce
// que buildServeArgs pose. Les fichiers sont pris par taille et date : un
// modèle retéléchargé, un moteur mis à jour changent la clé.
func slotPersistKey(args []string, env map[string]string, si serveSysInfo, bin string) string {
	h := sha256.New()
	w := func(s string) { h.Write([]byte(s)); h.Write([]byte{0}) }
	for i := 0; i < len(args); i++ {
		if args[i] == "--api-key" {
			i++
			continue
		}
		w(args[i])
	}
	vars := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "LLAMA_ARG_") || strings.HasPrefix(k, "GGML_") || strings.HasPrefix(k, "CUDA_") {
			vars[k] = v
		}
	}
	for k, v := range env {
		vars[k] = v
	}
	delete(vars, "LLAMA_ARG_API_KEY")
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w(k + "=" + vars[k])
	}
	stat := func(p string) {
		if p == "" {
			return
		}
		if st, err := os.Stat(p); err == nil {
			w(fmt.Sprintf("%s|%d|%d", p, st.Size(), st.ModTime().UnixNano()))
		} else {
			w(p + "|?")
		}
	}
	if si.Model != "" {
		for _, n := range shardFamily(filepath.Base(si.Model)) {
			stat(filepath.Join(filepath.Dir(si.Model), n))
		}
		stat(si.Model)
	}
	stat(si.MMProj)
	stat(si.Draft)
	stat(bin)
	// Bibliothèques du moteur (libllama, ggml-cuda…) : une mise à jour peut
	// les changer sans toucher au binaire.
	if entries, err := os.ReadDir(filepath.Dir(bin)); err == nil {
		for _, e := range entries {
			n := strings.ToLower(e.Name())
			if strings.HasSuffix(n, ".so") || strings.Contains(n, ".so.") || strings.HasSuffix(n, ".dll") || strings.HasSuffix(n, ".dylib") {
				stat(filepath.Join(filepath.Dir(bin), e.Name()))
			}
		}
	}
	w(fmt.Sprintf("build=%d", si.EngineBuild))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// slotPersistMarker : la clé du moteur lancé, écrite par « loki serve » juste
// avant de lui céder la place, lue par le process web. Hors du dossier des
// slots : le moteur ne peut pas l'écraser par un save.
func slotPersistMarker(home string) string { return filepath.Join(home, "slots-engine.key") }

// writeSlotPersistMarker pose (key non vide) ou retire la clé du moteur.
func writeSlotPersistMarker(home, key string) {
	p := slotPersistMarker(home)
	if key == "" {
		_ = os.Remove(p)
		return
	}
	_ = os.WriteFile(p, []byte(key+"\n"), 0o600)
}

// readSlotPersistMarker : la clé du moteur en service ; vide = SLOT_PERSIST
// refusé ou absent à son lancement.
func readSlotPersistMarker(home string) string {
	b, err := os.ReadFile(slotPersistMarker(home))
	if err != nil {
		return ""
	}
	k := strings.TrimSpace(string(b))
	if len(k) != 16 {
		return ""
	}
	if _, err := hex.DecodeString(k); err != nil {
		return ""
	}
	return k
}

// slotPersistPrune garde les slotPersistMaxFiles états les plus récents.
func slotPersistPrune(dir string) {
	type f struct {
		name string
		at   int64
	}
	var fs []f
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !slotPersistFileRe.MatchString(e.Name()) {
			continue
		}
		if info, err := e.Info(); err == nil {
			fs = append(fs, f{e.Name(), info.ModTime().UnixNano()})
		}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].at > fs[j].at })
	for i := slotPersistMaxFiles; i < len(fs); i++ {
		_ = os.Remove(filepath.Join(dir, fs[i].name))
	}
}

// slotPersistPurge retire tous les états gardés : la clé n'est plus là, ils
// n'ont plus rien à faire sur le disque.
func slotPersistPurge(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Type().IsRegular() && slotPersistFileRe.MatchString(e.Name()) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
