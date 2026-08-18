package loki

// code_mode.go — le mode Chat/Code d'une discussion. Bascule EXPLICITE
// (sélecteur dans le pied du composeur), persistée par discussion ; en mode
// chat, une détection côté serveur peut SUGGÉRER la bascule (puce ignorable
// dans l'UI) mais ne l'impose jamais — un faux positif coûterait des tours de
// planification/vérification sur un simple bonjour.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func modeKey(convID string) string { return "mode:" + convID }
func hintKey(convID string) string { return "hinted:" + convID }

// convCodeMode : la discussion active est-elle en mode code ?
func convCodeMode() bool { return getStr(bkChat, modeKey(convEnsureActive())) == "code" }

func setConvMode(convID, mode string) error {
	if mode != "code" {
		mode = "" // chat = défaut = clé absente
	}
	return putStr(bkChat, modeKey(convID), mode)
}

// dropConvMode : appelé à la suppression d'une discussion.
func dropConvMode(convID string) {
	_ = putStr(bkChat, modeKey(convID), "")
	_ = putStr(bkChat, hintKey(convID), "")
}

// --- Détection « ça ressemble à du code » ------------------------------------
// Côté serveur, AUCUN token : ni consigne dans le prompt, ni appel modèle.
// Un indice suffit ; la puce de l'UI est gratuite à ignorer.

var codeHintRes = []*regexp.Regexp{
	// Un bloc de code clôturé.
	regexp.MustCompile("```"),
	// Un chemin de fichier source.
	regexp.MustCompile(`(?i)\b[\w./\\-]+\.(go|py|js|jsx|ts|tsx|rs|java|kt|c|h|cpp|hpp|cs|rb|php|swift|sh|ps1|sql|html|css|scss|vue|svelte|toml|yaml|yml|json|dockerfile|makefile)\b`),
	// Verbes / vocabulaire de tâche de code.
	regexp.MustCompile(`(?i)\b(corrige|implémente|implemente|refactor|refactorise|débogue|debug|compile|stack ?trace|segfault|exception|traceback|unit ?test|pull ?request|merge|commit|clone)\b`),
	regexp.MustCompile(`(?i)\b(bug|erreur de compilation|tests? (échoue|cassé)|fonction|classe|module|api rest|endpoint)\b`),
}

// looksLikeCode dit si un message utilisateur ressemble à une tâche de code.
// Le dépôt git déjà cloné dans la discussion est aussi un indice fort.
func looksLikeCode(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	for _, re := range codeHintRes {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// convHasRepo : un dépôt git vit déjà dans le dossier de la discussion.
func convHasRepo() bool {
	ws := convWorkspace()
	if _, err := os.Stat(filepath.Join(ws, ".git")); err == nil {
		return true
	}
	entries, err := os.ReadDir(ws)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(ws, e.Name(), ".git")); err == nil {
			return true
		}
	}
	return false
}

// maybeCodeHint émet la puce « passer en mode Code ? » si le message s'y
// prête. Une seule fois par discussion : une suggestion répétée est du bruit.
func maybeCodeHint(c *Conversation, epoch int, text string) {
	if convCodeMode() || !agentEnabled() {
		return
	}
	id := convEnsureActive()
	if getBool(bkChat, hintKey(id)) {
		return
	}
	if !looksLikeCode(text) && !convHasRepo() {
		return
	}
	_ = putBool(bkChat, hintKey(id), true)
	c.appendDelta(epoch, map[string]any{"code_hint": true})
}

// --- API ---------------------------------------------------------------------

// handleChatMode — GET : {mode, criteria} de la discussion active ;
// POST {"mode":"code"|"chat"} : bascule (et pousse le nouvel état aux abonnés).
func handleChatMode(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			sendJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if body.Mode != "code" && body.Mode != "chat" {
			sendJSON(w, 400, map[string]any{"ok": false, "error": "mode inconnu (chat|code)"})
			return
		}
		id := convEnsureActive()
		if err := setConvMode(id, body.Mode); err != nil {
			sendJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		// Pousse la bascule à tous les appareils abonnés (le sélecteur doit
		// suivre, comme le fil lui-même).
		conv.mu.Lock()
		epoch := conv.epoch
		conv.mu.Unlock()
		conv.appendDelta(epoch, map[string]any{"mode": body.Mode})
		sendJSON(w, 200, map[string]any{"ok": true, "mode": body.Mode})
		return
	}
	mode := "chat"
	if convCodeMode() {
		mode = "code"
	}
	sendJSON(w, 200, map[string]any{
		"ok":       true,
		"mode":     mode,
		"criteria": critList(convEnsureActive()),
	})
}

// handleChatCriteria — POST : édition manuelle des critères depuis l'UI.
// {"criteria":[{"id":1,"text":"…","status":"pending"},…]} remplace la liste.
func handleChatCriteria(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, 405, map[string]any{"ok": false, "error": "POST attendu"})
		return
	}
	var body struct {
		Criteria []Criterion `json:"criteria"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		sendJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var clean []Criterion
	next := 1
	for _, c := range body.Criteria {
		if strings.TrimSpace(c.Text) == "" || len(clean) >= critMaxCount {
			continue
		}
		if c.ID <= 0 {
			c.ID = next
		}
		if c.ID >= next {
			next = c.ID + 1
		}
		switch c.Status {
		case "pending", "passed", "failed":
		default:
			c.Status = "pending"
		}
		clean = append(clean, c)
	}
	critSave(convEnsureActive(), clean)
	critNotify(clean)
	sendJSON(w, 200, map[string]any{"ok": true, "criteria": clean})
}
