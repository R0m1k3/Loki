package loki

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// API interne d'un tour de discussion tenu par la façade Rust (lot 6).
//
// La boucle, le journal et la persistance sont en Rust ; ce qui décrit le
// tour reste ici, le temps d'être porté (lot 7) : pièces jointes et vision,
// rôle du mode Code, prompt du preset, consignes du dépôt, contexte du
// projet, préambule d'agent, outils (MCP compris), puce « mode Code »,
// notification Web Push, et l'état par discussion que les outils lisent.
//
//	POST /internal/v1/turn/start   corps de /api/chat/send
//	  → {files, caps, turn_caps, content, model, code_hint}
//	POST /internal/v1/turn/view    {"caps":{…}}  → {head, prefix, tools}
//	  409 : PROJ_SNAPSHOT actif (le bloc projet figé reste au Go)
//	POST /internal/v1/turn/done    {"ms":N}      → 204 (Web Push si abonnés)
//	POST /internal/v1/project/activate {"slug":…} → 204 (projet actif changé
//	  par la façade, qui a enregistré la discussion du projet quitté)
//	POST /internal/v1/turn/verifier {"caps":{…}} → {system, tools, caps}
//	  (passe de vérification du mode Code : contexte ISOLÉ, rôle verifier)
//	POST /internal/v1/conv/active  {"id":…}      → 204 (discussion active des outils)
//	POST /internal/v1/conv/dropped {"id":…}      → 204 (fichiers, jobs, critères,
//	                                                   mode, résultats d'outils ;
//	                                                   "files_only" : les fichiers)

const internalTurnMaxBody = 8 << 20

func readInternalJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST attendu"})
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, internalTurnMaxBody+1))
	if err != nil || len(raw) > internalTurnMaxBody || json.Unmarshal(raw, v) != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "corps illisible"})
		return false
	}
	return true
}

// handleInternalTurnStart : ce que StartTurn et EnqueueOrStart tirent de la
// requête avant de lancer (ou de mettre en file) le tour. caps : capacités
// demandées (file d'attente, préchauffage) ; turn_caps : celles du tour, rôle
// du mode Code figé ; content : le message utilisateur vu par le modèle.
func handleInternalTurnStart(w http.ResponseWriter, r *http.Request) {
	var body chatReq
	if !readInternalJSON(w, r, &body) {
		return
	}
	files := attachFiles(body.Files)
	if strings.TrimSpace(body.Message) == "" && len(files) == 0 {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "message vide"})
		return
	}
	rememberUserTZ(body.TZ)
	caps := capsFromBody(body)
	turnCaps := withTurnRole(caps, body.Message)
	prompt := body.Message
	if strings.TrimSpace(prompt) == "" {
		prompt = "Prends-en connaissance."
	}
	if files == nil {
		files = []attachInfo{}
	}
	sendJSON(w, http.StatusOK, map[string]any{
		"files":     files,
		"caps":      caps,
		"turn_caps": turnCaps,
		"content":   userMessageContent(files, prompt),
		"model":     chatModelName(),
		"code_hint": !turnCaps.Code && codeHintDue(body.Message),
	})
}

// handleInternalTurnView : les pièces de turnView (assembleTurn sans
// PROJ_SNAPSHOT) que la façade assemble autour de l'historique : head part
// devant les messages, puis prefix (InjectSkills) fusionné dans le premier
// système qui n'est pas un message projet, ou posé en tête.
func handleInternalTurnView(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Caps Caps `json:"caps"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	caps := in.Caps
	if caps.Agent && projSnapEnabled(ReadConfig()) {
		sendJSON(w, http.StatusConflict, map[string]any{"error": "PROJ_SNAPSHOT actif : le bloc projet figé reste tenu par le Go"})
		return
	}
	head := []Message{}
	if caps.Agent {
		if sp := readSysPrompt(); sp != "" {
			head = append(head, Message{Role: "system", Content: sp})
		}
		if m, ok := codeInstructionsMessage(caps); ok {
			head = append(head, m)
		}
		head = append(head, projectSystemMessages()...)
	}
	tools := EnabledTools(caps)
	if tools == nil {
		tools = []Tool{}
	}
	sendJSON(w, http.StatusOK, map[string]any{"head": head, "prefix": injectPrefix(caps, tools), "tools": tools})
}

// handleInternalTurnVerifier : ce que runVerifyPass envoie au moteur hors de
// l'historique — le prompt du rôle verifier suivi des faits de la machine, et
// le jeu d'outils fermé de la vérification. caps : celles du tour, rôle
// remplacé (le Go s'y fie pour laisser l'outil criteria marquer « passed »).
func handleInternalTurnVerifier(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Caps Caps `json:"caps"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	vcaps := in.Caps
	vcaps.Role = "verifier"
	sendJSON(w, http.StatusOK, map[string]any{
		"system": rolePrompt("verifier") + "\n\n" + machineSystemPrompt(vcaps),
		"tools":  EnabledTools(vcaps),
		"caps":   vcaps,
	})
}

// handleInternalProjectActivate : setActiveProject pour la façade (projet
// rangé en base, cache de ceux qui le lisent ici — mémoire, trackers, outils).
func handleInternalProjectActivate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Slug string `json:"slug"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	if !projectExists(in.Slug) {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "projet introuvable"})
		return
	}
	if err := setActiveProject(in.Slug); err != nil {
		sendJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleInternalTurnDone(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Ms int64 `json:"ms"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	if hasPushSubs() {
		d := time.Duration(in.Ms) * time.Millisecond
		go sendPushToAll("Loki", "Réponse prête · "+fmtDurFR(d))
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleInternalConvActive : la façade a changé de discussion active. Les
// outils exécutés ici (dossier de la discussion, mode Code, critères) lisent
// l'identifiant en cache : il suit.
func handleInternalConvActive(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.ID) == "" {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "id manquant"})
		return
	}
	convActiveMu.Lock()
	convActiveRemember(in.ID)
	convActiveMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// handleInternalConvDropped : ce qui part avec une discussion supprimée par
// la façade (mêmes effets que la fin de convDelete).
func handleInternalConvDropped(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
		// FilesOnly : seulement les fichiers (Reset vide la discussion, il ne
		// la supprime pas).
		FilesOnly bool `json:"files_only"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.ID) == "" {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "id manquant"})
		return
	}
	if in.FilesOnly {
		dropConvFiles(in.ID)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	stopConvJobs(in.ID)
	critDrop(in.ID)
	dropConvMode(in.ID)
	dropConvFiles(in.ID)
	deleteToolResultsFor(in.ID)
	w.WriteHeader(http.StatusNoContent)
}
