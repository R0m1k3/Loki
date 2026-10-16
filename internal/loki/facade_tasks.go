package loki

import (
	"net/http"
	"os"
	"strings"
)

// facade_tasks.go — tâches planifiées tenues par la façade Rust
// (LOKI_TASKS=rust, lot 26 de la migration ; expérimental, le Go reste le
// défaut) : les routes /api/tasks*, le planificateur et les tâches « script
// seul » (rust/loki-rs/src/tasks). Une tâche IA passe toujours par le verrou
// de génération et les outils de l'agent, qui sont ici : la façade la fait
// exécuter par le Go (/internal/v1/tasks/run). Le planificateur du Go ne
// démarre pas.

const facadeTasksEnv = "LOKI_TASKS"

// facadeTasks : la façade tient-elle les tâches ?
func facadeTasks() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeTasksEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// facadeTasksRoutes : les routes que la façade sert alors.
var facadeTasksRoutes = map[string]bool{
	"/api/tasks":        true,
	"/api/tasks/save":   true,
	"/api/tasks/delete": true,
	"/api/tasks/toggle": true,
	"/api/tasks/pause":  true,
	"/api/tasks/run":    true,
	"/api/tasks/stop":   true,
}

// handleInternalTaskRun (POST {id}) : exécute MAINTENANT la tâche IA d'id
// donné, comme le ferait le planificateur (preset épinglé, projet forcé,
// RunAutonomous, état persisté, notification), et rend la main à la fin.
// {busy:true} : le moteur était occupé ou le modèle pas prêt — la tâche n'a
// pas tourné et sera réessayée.
func handleInternalTaskRun(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if !readLeaseBody(w, r, &in) {
		return
	}
	t, ok := getTask(in.ID)
	if !ok {
		sendJSON(w, http.StatusNotFound, map[string]any{"error": "tâche introuvable"})
		return
	}
	before := t.LastRun
	runTask(t)
	after, _ := getTask(in.ID)
	sendJSON(w, http.StatusOK, map[string]any{"busy": after.LastRun == before})
}

// handleInternalPushSend (POST {title, body}) : une notification à tous les
// abonnés, envoyée en tâche de fond (les tâches script de la façade quand
// elle ne tient pas les notifications).
func handleInternalPushSend(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if !readLeaseBody(w, r, &in) {
		return
	}
	if hasPushSubs() {
		go sendPushToAll(in.Title, in.Body)
	}
	w.WriteHeader(http.StatusNoContent)
}
