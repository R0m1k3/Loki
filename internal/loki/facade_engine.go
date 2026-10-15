package loki

import (
	"net/http"
	"os"
	"strings"
)

// facade_engine.go — pilotage du moteur tenu par la façade Rust
// (LOKI_ENGINE=rust, lot 16 de la migration ; expérimental, le Go reste le
// défaut) : démarrage, arrêt, redémarrage (/api/start|stop|restart) et
// déchargement / rechargement de la VRAM (/api/vram/unload|reload).
//
// La façade lance elle-même « loki-rs engine <action> » — le superviseur que
// le Go appelait déjà en conteneur (sys_service_rust_linux.go) — et contrôle
// le moteur avant de le relancer (preflightEngine). Elle ne tient ce lot qu'en
// conteneur avec ce superviseur : sous systemd, ou avec
// LOKI_ENGINE_SUPERVISOR=go, le Go le garde (décidé par loki-rs au démarrage).
//
// Ce qui vit ici reste ici jusqu'au lot qui le portera, derrière l'API
// interne :
//
//   - le verrou de l'optimiseur, consulté puis débarrassé d'un essai orphelin
//     avant tout démarrage (/internal/v1/service/prestart) ;
//   - la génération en cours quand le Go tient la conversation
//     (/internal/v1/chat/generating) ;
//   - l'arrêt de la dictée quand le Go la tient (/internal/v1/dictate/stop).

const facadeEngineEnv = "LOKI_ENGINE"

// facadeEngine : la façade pilote-t-elle le moteur ?
func facadeEngine() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeEngineEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// facadeEngineRoutes : les routes que la façade sert alors (enginectl.rs).
var facadeEngineRoutes = map[string]bool{
	"/api/start":       true,
	"/api/stop":        true,
	"/api/restart":     true,
	"/api/vram/unload": true,
	"/api/vram/reload": true,
}

// enginePrestart : ce que serviceAction fait avant un démarrage — refus
// pendant une optimisation, puis ménage d'un verrou périmé (essai orphelin
// arrêté, application interrompue défaite). Variable pour les tests.
var enginePrestart = func() error {
	if err := tuneGuard(); err != nil {
		return err
	}
	tuneReapStale()
	return nil
}

// handleInternalServicePrestart : POST → 200 {error} ("" : la voie est
// libre).
func handleInternalServicePrestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST attendu"})
		return
	}
	msg := ""
	if err := enginePrestart(); err != nil {
		msg = err.Error()
	}
	sendJSON(w, http.StatusOK, map[string]any{"error": msg})
}

// handleInternalChatGenerating : GET → 200 {generating} — la conversation
// tenue ici écrit-elle une réponse ?
func handleInternalChatGenerating(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET attendu"})
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{"generating": conv.isGenerating()})
}

// handleInternalDictateStop : POST → 204 — arrête la dictée tenue ici, sans
// attendre derrière un démarrage (asrShutdownVite).
func handleInternalDictateStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST attendu"})
		return
	}
	asrShutdownVite()
	w.WriteHeader(http.StatusNoContent)
}
