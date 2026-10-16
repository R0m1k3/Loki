package loki

import (
	"fmt"
	"net/http"
	"os"
	"strings"
)

// facade_tune.go — l'optimiseur tenu par la façade Rust (LOKI_TUNE=rust, lot
// 28 de la migration ; expérimental, le Go reste le défaut) : les routes
// /api/tune*, le job, les essais et l'application (rust/loki-rs/src/tune).
//
// Le verrou de l'optimiseur reste le FICHIER de LOKI_HOME, au même format
// (backend_tune_lock.go) : le Go le lit (tuneGuard), et sa reprise
// (tuneRecoverWatch) écarte celui d'une façade morte — essai orphelin arrêté,
// application défaite, moteur relancé. Le verrou de génération : le bail du
// moteur, motif « tune » (errTuneBusy pour les autres, le stop du chat
// annule). Les essais restent des « loki serve » du Go (LOKI_TUNE_TRIAL).
//
// Le Go prête ce qu'il est seul à voir (handleInternalTuneContext) et le
// démarrage et l'arrêt du moteur SANS le refus de l'optimiseur
// (handleInternalServiceAction, « os ») : c'est la façade qui tient le verrou.

const facadeTuneEnv = "LOKI_TUNE"

// facadeTune : la façade tient-elle l'optimiseur ?
func facadeTune() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeTuneEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// facadeTuneRoutes : les routes que la façade sert alors.
var facadeTuneRoutes = map[string]bool{
	"/api/tune":        true,
	"/api/tune/status": true,
	"/api/tune/cancel": true,
	"/api/tune/apply":  true,
	"/api/tune/last":   true,
}

// handleInternalTuneContext : GET → {busy, main_active, perf}.
//
//   - busy : ce qui interdit d'optimiser dans le processus web
//     (tunePreflight) — un benchmark, une génération, des jobs du mode code ;
//     "" sinon ;
//   - main_active : le vrai moteur tourne (engineNeedsStop) ;
//   - perf : la télémétrie, d'où l'optimiseur tire les poids d'un tour type.
func handleInternalTuneContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET attendu"})
		return
	}
	busy := ""
	switch {
	case benchRunning():
		busy = errBenchBusy.Error()
	case conv.isGenerating():
		busy = "génération en cours"
		if err := conv.busyReason(); err != nil {
			busy = err.Error()
		}
	case bgJobsRunning() > 0:
		busy = fmt.Sprintf("%d job(s) d'arrière-plan du mode code tournent encore (bash_bg) : ils fausseraient les mesures — arrête-les d'abord",
			bgJobsRunning())
	}
	sendJSON(w, http.StatusOK, map[string]any{"busy": busy, "main_active": engineNeedsStop(),
		"perf": perfLog.snapshot()})
}
