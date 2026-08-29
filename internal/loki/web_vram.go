// web_vram.go — rendre la mémoire vidéo à la machine, à la demande.
//
// Tant que le moteur tourne, le modèle occupe la VRAM : une autre application
// (jeu, encodage, entraînement, un second serveur d'inférence…) qui réclame la
// carte ne trouve plus rien à prendre. Le geste existait déjà — arrêter le
// service — mais il vivait dans les réglages sous le nom « arrêter », qui ne
// dit pas qu'il libère la carte, et il laissait tourner le serveur de dictée
// qui, lui aussi, garde de la VRAM quand il est allumé sur GPU.
//
//	POST /api/vram/unload  → arrête le moteur ET la dictée, puis rapporte la
//	                          mémoire réellement rendue (before/after/freed)
//	POST /api/vram/reload  → relance le moteur (le modèle se recharge)
package loki

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// gpuStat : une carte vue par nvidia-smi. Mémoire en Mo (unité de nvidia-smi,
// et celle que /api/vram expose déjà à l'interface).
type gpuStat struct {
	Name  string
	Used  int
	Total int
	Util  int
	Temp  int
}

// gpuStats interroge nvidia-smi. Renvoie nil quand il est absent (machine sans
// GPU NVIDIA, Mac, CPU seul) : l'absence de mesure n'est pas une erreur, elle
// prive juste le déchargement de son bilan chiffré.
func gpuStats() []gpuStat {
	out, err := hideCmd(exec.Command("nvidia-smi",
		"--query-gpu=name,memory.used,memory.total,utilization.gpu,temperature.gpu",
		"--format=csv,noheader,nounits")).Output()
	if err != nil {
		return nil
	}
	var gpus []gpuStat
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, ",")
		if len(parts) != 5 {
			continue
		}
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		g := gpuStat{Name: parts[0]}
		g.Used, _ = strconv.Atoi(parts[1])
		g.Total, _ = strconv.Atoi(parts[2])
		g.Util, _ = strconv.Atoi(parts[3])
		g.Temp, _ = strconv.Atoi(parts[4])
		gpus = append(gpus, g)
	}
	return gpus
}

// gpuUsedMB : VRAM occupée, toutes cartes confondues.
func gpuUsedMB(gpus []gpuStat) int {
	used := 0
	for _, g := range gpus {
		used += g.Used
	}
	return used
}

// gpuSettle attend que la VRAM ait fini de redescendre avant de la relire. Le
// pilote ne rend pas la mémoire à l'instant où le processus meurt — taskkill et
// Process.Kill reviennent avant même que CUDA ait commencé son ménage — et une
// lecture prise trop tôt rapporterait « 0 Mo libérés » après un déchargement
// qui a pourtant marché. Trois règles, chacune née d'un faux bilan :
//
//   - une mesure ratée (nvidia-smi absent, ou en pleine réinitialisation juste
//     après l'arrêt) n'est PAS « 0 Mo » : on la saute et on garde la dernière
//     lecture valide — la compter à zéro annoncerait tout le modèle comme
//     libéré alors que rien n'a bougé ;
//   - un palier n'est concluant qu'une fois la baisse commencée : avant, c'est
//     le pilote qui n'a pas encore réagi, pas la mémoire qui a fini de partir ;
//   - deux lectures égales de suite après la baisse, et on rend la valeur.
//     Sans baisse du tout, on rend la dernière lecture après ~4 s.
//
// sample renvoie (Mo occupés, lecture réussie). before : la lecture prise
// avant l'arrêt, qui sert de repère à « la baisse a commencé ».
func gpuSettle(before int, sample func() (int, bool), pause time.Duration) int {
	last, lu := before, false
	baisse, palier := false, 0
	for i := 0; i < 8; i++ {
		time.Sleep(pause)
		cur, ok := sample()
		if !ok {
			continue
		}
		if cur < before {
			baisse = true
		}
		if lu && cur == last {
			palier++
		} else {
			palier = 0
		}
		last, lu = cur, true
		if baisse && palier >= 1 {
			return cur
		}
	}
	return last
}

func gpuUsedSettled(before int) int {
	return gpuSettle(before, func() (int, bool) {
		g := gpuStats()
		return gpuUsedMB(g), g != nil
	}, 500*time.Millisecond)
}

// engineNeedsStop : faut-il envoyer « stop » ? Actif, évidemment. Mais sous
// systemd, « is-active » ne répond « active » qu'une fois le service établi :
// une unité en train de (re)démarrer — Restart=on-failure sur un modèle qui
// meurt au chargement — répond « activating », et la sauter laisserait systemd
// relancer llama-server toutes les trois secondes, GPU repris à chaque tour,
// pendant que l'interface affirme « le moteur était déjà arrêté ». On garde la
// condition pour le bilan, mais élargie aux états transitoires.
func engineNeedsStop() bool {
	if serviceIsActive() {
		return true
	}
	if !systemdAvailable() {
		return false
	}
	out, _ := exec.Command("systemctl", "is-active", serviceName()).Output()
	switch strings.TrimSpace(string(out)) {
	case "activating", "reloading", "deactivating":
		return true
	}
	return false
}

// postOnly refuse tout sauf POST. Ces routes changent l'état de la machine —
// elles arrêtent des processus — et un simple GET ne doit pas y suffire : sans
// clé de pilotage configurée, une balise <img> sur une page tierce visitée par
// hasard suffirait sinon à couper le moteur.
func postOnly(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodPost {
		return true
	}
	w.Header().Set("Allow", http.MethodPost)
	sendJSON(w, 405, map[string]any{"ok": false, "error": "méthode " + r.Method + " refusée : POST attendu"})
	return false
}

// ansiRe / plainErr : les messages de preflightEngine sont écrits pour le
// terminal (bold() y glisse des séquences ANSI). Envoyés tels quels à
// l'interface, ils y affichent des « [1m » au milieu de la phrase.
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

func plainErr(err error) string {
	if err == nil {
		return ""
	}
	return ansiRe.ReplaceAllString(err.Error(), "")
}

// handleVramUnload arrête le moteur (et la dictée) pour rendre la mémoire
// vidéo. Refuse pendant une génération — la couper perdrait la réponse en
// cours — sauf si l'appelant insiste avec {force:true}.
func handleVramUnload(w http.ResponseWriter, r *http.Request) {
	if !postOnly(w, r) {
		return
	}
	var req struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if !req.Force && conv.isGenerating() {
		sendJSON(w, 409, map[string]any{"ok": false, "generating": true,
			"error": "une réponse est en cours d'écriture — décharger maintenant l'interromprait"})
		return
	}
	gpus := gpuStats()
	before := gpuUsedMB(gpus)
	wasActive := engineNeedsStop()

	var errs []string
	if wasActive {
		if err := serviceAction("stop"); err != nil {
			errs = append(errs, "arrêt du moteur : "+plainErr(err))
		}
	}
	// La dictée tourne dans un processus séparé, allumé à la demande et éteint
	// après dix minutes d'inactivité : sur GPU, elle occupe la carte pendant tout
	// ce temps. Libérer la VRAM sans elle ne libérerait pas tout. La variante
	// « vite » : ne jamais attendre derrière un démarrage de la dictée, qui
	// peut tenir le verrou deux minutes — le navigateur aurait abandonné bien
	// avant, moteur pourtant déjà arrêté.
	whisperShutdownVite()

	after := before
	if gpus != nil {
		after = gpuUsedSettled(before)
	}
	freed := before - after
	if freed < 0 {
		freed = 0
	}
	sendJSON(w, 200, map[string]any{
		"ok":         len(errs) == 0,
		"error":      strings.Join(errs, " ; "),
		"was_active": wasActive,
		"active":     serviceIsActive(),
		"gpu":        gpus != nil,
		"before_mb":  before,
		"used_mb":    after,
		"freed_mb":   freed,
	})
}

// handleVramReload relance le moteur après un déchargement. Le préflight évite
// de démarrer un service condamné à mourir en boucle (BIN ou MODEL absents) :
// mieux vaut la vraie raison tout de suite qu'un « chargement… » sans fin.
func handleVramReload(w http.ResponseWriter, r *http.Request) {
	if !postOnly(w, r) {
		return
	}
	if serviceIsActive() {
		sendJSON(w, 200, map[string]any{"ok": true, "already": true})
		return
	}
	if err := preflightEngine(); err != nil {
		sendJSON(w, 200, map[string]any{"ok": false, "error": plainErr(err)})
		return
	}
	if err := serviceAction("start"); err != nil {
		sendJSON(w, 200, map[string]any{"ok": false, "error": plainErr(err)})
		return
	}
	sendJSON(w, 200, map[string]any{"ok": true})
}
