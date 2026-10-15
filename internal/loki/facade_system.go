package loki

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

// facade_system.go — interrupteurs système tenus par la façade Rust
// (LOKI_SYSTEM=rust, lot 17 de la migration ; expérimental, le Go reste le
// défaut) : accès internet (/api/internet), pilotage du navigateur
// (/api/computer), clé de complétion (/api/apikey) et écoute réseau du moteur
// (/api/network).
//
// Les réglages restent des clés de la base. Ce qui vit dans ce processus reste
// ici, derrière l'API interne :
//
//   - le cache de joignabilité de Crawl4AI des outils web, vidé quand l'URL, la
//     clé ou le moteur changent (/internal/v1/web/reach/reset) ;
//   - le Chromium piloté par les outils browser_*, fermé quand l'interrupteur
//     se coupe (/internal/v1/cdp/shutdown) ;
//   - le redémarrage du moteur qui applique une nouvelle clé, quand la façade
//     ne pilote pas le moteur (/internal/v1/service/action).

const facadeSystemEnv = "LOKI_SYSTEM"

// facadeSystem : la façade tient-elle les interrupteurs système ?
func facadeSystem() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeSystemEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// facadeSystemRoutes : les routes que la façade sert alors (system.rs).
var facadeSystemRoutes = map[string]bool{
	"/api/internet": true,
	"/api/computer": true,
	"/api/apikey":   true,
	"/api/network":  true,
}

// handleInternalReachReset : POST → 204 — le prochain tour reteste Crawl4AI.
func handleInternalReachReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST attendu"})
		return
	}
	reachMu.Lock()
	reachURL = ""
	reachMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// handleInternalCDPShutdown : POST → 204 — ferme le navigateur piloté.
func handleInternalCDPShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST attendu"})
		return
	}
	cdpShutdown()
	w.WriteHeader(http.StatusNoContent)
}

// internalServiceAction : serviceAction, variable pour les tests.
var internalServiceAction = serviceAction

// handleInternalServiceAction : POST {action, if_active} → 200 {ran, error}.
// if_active : seulement si le moteur tourne (une clé neuve ne s'applique qu'au
// redémarrage, inutile de démarrer un moteur arrêté pour elle).
func handleInternalServiceAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST attendu"})
		return
	}
	var in struct {
		Action   string `json:"action"`
		IfActive bool   `json:"if_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	switch in.Action {
	case "start", "stop", "restart":
	default:
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "action inconnue : " + in.Action})
		return
	}
	if in.IfActive && !serviceIsActive() {
		sendJSON(w, http.StatusOK, map[string]any{"ran": false, "error": ""})
		return
	}
	msg := ""
	if err := internalServiceAction(in.Action); err != nil {
		msg = err.Error()
	}
	sendJSON(w, http.StatusOK, map[string]any{"ran": true, "error": msg})
}
