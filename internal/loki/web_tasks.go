// web_tasks.go — endpoints de pilotage des tâches planifiées (voir tasks.go).
// Tous derrière la clé de pilotage (enregistrés via api(...) dans newWebMux).
package loki

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// handleTasks : GET → liste des tâches + état de l'interrupteur maître + si le
// mode agent est actif (sans lui, une tâche n'a aucun outil pour agir).
//
// Les horodatages (next_run, last_run) partent en epoch ms BRUTS : c'est le
// navigateur qui les met en forme, dans le fuseau de l'utilisateur. Les formater
// ici les figerait dans le fuseau du SERVEUR (souvent UTC), d'où un décalage
// horaire à l'écran.
func handleTasks(w http.ResponseWriter, r *http.Request) {
	tasks := listTasks()
	conv.mu.Lock()
	runningID := conv.runningTaskID
	conv.mu.Unlock()
	// Une tâche script tourne HORS du verrou de génération (aucune inférence) :
	// conv ne la voit pas, on complète donc avec le registre des scripts en cours
	// (tasks_script.go), sinon l'UI l'affiche à l'arrêt pendant qu'elle tourne.
	if runningID == "" {
		if sid, _ := scriptRunningAny(); sid != "" {
			runningID = sid
		}
	}
	// Presets (id + nom + actif) pour peupler le sélecteur du formulaire de tâche.
	presets := []map[string]any{}
	if list, err := ListPresets(); err == nil {
		for _, p := range list {
			presets = append(presets, map[string]any{"id": p.ID, "name": p.Name, "active": p.Active})
		}
	}
	// Scripts durables disponibles, pour le sélecteur d'une tâche « script seul ».
	scripts, _ := listScripts()
	if scripts == nil {
		scripts = []scriptInfo{}
	}
	sendJSON(w, 200, map[string]any{
		"ok":          true,
		"tasks":       tasks,
		"scripts":     scripts,
		"scripts_dir": scriptsDir(),
		"paused":      tasksPaused(),
		"agent":       agentEnabled(),
		"running_id":  runningID,
		"presets":     presets,
		// Projets, pour le sélecteur « projet visé » du formulaire de tâche.
		"projects": projectDTOs(),
		// État global mémoire/web, pour proposer des défauts cohérents à la création.
		"mem_on": memMode() != MemOff,
		"web_on": internetEnabled() && crawlReachable(),
	})
}

// handleTaskSave crée ou met à jour une tâche. Valide le schedule (400 sinon) et
// recalcule NextRun. Un ID vide = création.
func handleTaskSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Prompt   string `json:"prompt"`
		Schedule string `json:"schedule"`
		TZ       string `json:"tz"`
		Preset   string `json:"preset"`
		Project  string `json:"project"`
		Kind     string `json:"kind"`
		Script   string `json:"script"`
		NoMem    bool   `json:"no_mem"`
		NoWeb    bool   `json:"no_web"`
		Enabled  bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Prompt = strings.TrimSpace(req.Prompt)
	req.Schedule = strings.TrimSpace(req.Schedule)
	req.Kind = strings.TrimSpace(req.Kind)
	req.Script = strings.TrimSpace(req.Script)
	if req.Name == "" {
		sendJSON(w, 400, map[string]any{"ok": false, "error": "nom obligatoire"})
		return
	}
	// Une tâche script exige un script EXISTANT (le vérifier ici évite une tâche
	// planifiée qui échouera silencieusement au premier tic) ; une tâche IA exige
	// une consigne.
	if req.Kind == "script" {
		if req.Script == "" {
			sendJSON(w, 400, map[string]any{"ok": false, "error": "script obligatoire pour une tâche script"})
			return
		}
		if err := scriptExists(req.Script); err != nil {
			sendJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	} else if req.Prompt == "" {
		sendJSON(w, 400, map[string]any{"ok": false, "error": "consigne obligatoire"})
		return
	}
	if err := validateSchedule(req.Schedule, req.TZ); err != nil {
		sendJSON(w, 400, map[string]any{"ok": false, "error": "fréquence invalide : " + err.Error()})
		return
	}

	var t Task
	if req.ID != "" {
		if cur, ok := getTask(req.ID); ok {
			t = cur // conserve l'historique d'exécution (LastRun/LastReport…)
		} else {
			t.ID = req.ID
		}
	}
	if t.ID == "" {
		t.ID = newTaskID()
	}
	t.Name, t.Prompt, t.Enabled = req.Name, req.Prompt, req.Enabled
	t.TZ, t.Preset = req.TZ, req.Preset
	t.Kind, t.Script = req.Kind, req.Script
	// Projet visé : vide = le projet actif au moment de l'enregistrement. Une tâche
	// créée depuis un projet appartient à ce projet — c'est là que sa mémoire et
	// ses trackers l'attendent quand elle tournera (voir runTask).
	if p := strings.TrimSpace(req.Project); p != "" && projectExists(p) {
		t.Project = p
	} else if t.Project == "" {
		t.Project = activeProjectSlug()
	}
	t.NoMem, t.NoWeb = req.NoMem, req.NoWeb
	// Recalcule NextRun si la fréquence a changé (ou à la création).
	if t.Schedule != req.Schedule || t.NextRun == 0 {
		t.NextRun = computeNextRun(req.Schedule, req.TZ, time.Now())
	}
	t.Schedule = req.Schedule
	if err := saveTask(t); err != nil {
		sendJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sendJSON(w, 200, map[string]any{"ok": true, "id": t.ID})
}

// handleTaskDelete supprime une tâche.
func handleTaskDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.ID == "" {
		sendJSON(w, 400, map[string]any{"ok": false, "error": "id manquant"})
		return
	}
	if err := deleteTask(req.ID); err != nil {
		sendJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sendJSON(w, 200, map[string]any{"ok": true})
}

// handleTaskToggle active/désactive UNE tâche. En l'activant, on (re)pose son
// NextRun pour qu'elle reparte proprement.
func handleTaskToggle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
		On bool   `json:"on"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	t, ok := getTask(req.ID)
	if !ok {
		sendJSON(w, 404, map[string]any{"ok": false, "error": "tâche introuvable"})
		return
	}
	t.Enabled = req.On
	if req.On {
		t.NextRun = computeNextRun(t.Schedule, t.TZ, time.Now())
	}
	if err := saveTask(t); err != nil {
		sendJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sendJSON(w, 200, map[string]any{"ok": true})
}

// handleTasksPause pilote l'interrupteur maître ({on:true} = suspendre tout).
func handleTasksPause(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On bool `json:"on"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := setTasksPaused(req.On); err != nil {
		sendJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sendJSON(w, 200, map[string]any{"ok": true, "paused": tasksPaused()})
}

// handleTaskStop arrête la tâche en cours. Une tâche SCRIPT ne passe pas par le
// verrou de génération : on l'annule via son registre (tasks_script.go). Sinon
// (tâche IA), on retombe sur conv.Stop, qui annule la génération autonome.
func handleTaskStop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.ID != "" && scriptRunStop(req.ID) {
		sendJSON(w, 200, map[string]any{"ok": true})
		return
	}
	conv.Stop()
	sendJSON(w, 200, map[string]any{"ok": true})
}

// handleTaskRun lance une tâche MAINTENANT (bouton « tester »). Exécution
// détachée : on rend la main tout de suite et on rafraîchit l'état à la fin.
// 409 si une génération est déjà en cours.
func handleTaskRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	t, ok := getTask(req.ID)
	if !ok {
		sendJSON(w, 404, map[string]any{"ok": false, "error": "tâche introuvable"})
		return
	}
	// Tâche script : aucune inférence, donc ni verrou de génération ni moteur
	// chargé à attendre — la refuser parce que le modèle répond ailleurs n'aurait
	// aucun sens.
	if t.Kind == "script" {
		go runTask(t)
		sendJSON(w, 200, map[string]any{"ok": true})
		return
	}
	// Vérifie la disponibilité AVANT de détacher, pour pouvoir répondre 409/503.
	conv.mu.Lock()
	busy := conv.Generating
	conv.mu.Unlock()
	if busy {
		sendJSON(w, 409, map[string]any{"ok": false, "error": ErrBusy.Error()})
		return
	}
	if !healthCheck() {
		sendJSON(w, 503, map[string]any{"ok": false, "error": errModelLoading.Error()})
		return
	}
	go runTask(t)
	sendJSON(w, 200, map[string]any{"ok": true})
}
