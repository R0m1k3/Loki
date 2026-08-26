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

// gpuUsedSettled attend que la VRAM cesse de baisser avant de la relire : le
// pilote ne rend pas la mémoire à l'instant où le processus meurt, et une
// lecture immédiate rapporterait « 0 Mo libérés » juste après un déchargement
// qui a pourtant marché. Au plus ~3 s, et rien du tout sans GPU.
func gpuUsedSettled() int {
	gpus := gpuStats()
	if gpus == nil {
		return 0
	}
	last := gpuUsedMB(gpus)
	for i := 0; i < 6; i++ {
		time.Sleep(500 * time.Millisecond)
		cur := gpuUsedMB(gpuStats())
		if cur >= last {
			return cur
		}
		last = cur
	}
	return last
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
	wasActive := serviceIsActive()

	var errs []string
	if wasActive {
		if err := serviceAction("stop"); err != nil {
			errs = append(errs, "arrêt du moteur : "+plainErr(err))
		}
	}
	// La dictée tourne dans un processus séparé, allumé à la demande et éteint
	// après dix minutes d'inactivité : sur GPU, elle occupe la carte pendant tout
	// ce temps. Libérer la VRAM sans elle ne libérerait pas tout.
	whisperShutdown()

	after := before
	if gpus != nil {
		after = gpuUsedSettled()
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
