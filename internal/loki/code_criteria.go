package loki

// code_criteria.go — critères d'acceptation du mode code (contrat d'exécution,
// repris du session-metadata/criteria d'OpenFox, réécrit en Go — voir
// NOTICE.md). Les critères sont posés au début d'une tâche (par le modèle via
// l'outil `criteria`, ou à la main dans l'UI), puis la passe de vérification
// (code_verify.go) les fait passer à `passed` ou `failed`. Le tour de build ne
// se termine « vraiment » que quand tout est passed — c'est le contrat.
//
// Rangés par discussion dans bbolt (bucket chat, clé crit:<id>) : ils suivent
// la discussion, sa suppression les emporte.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Criterion struct {
	ID     int    `json:"id"`
	Text   string `json:"text"`
	Status string `json:"status"` // pending | completed (le builder dit « fait ») | passed | failed
	Note   string `json:"note,omitempty"`
}

const critMaxCount = 20

func critKey(convID string) string { return "crit:" + convID }

func critList(convID string) []Criterion {
	var list []Criterion
	if b, ok := getStoreBytes(bkChat, critKey(convID)); ok && len(b) > 0 {
		_ = json.Unmarshal(b, &list)
	}
	return list
}

func critSave(convID string, list []Criterion) {
	if len(list) == 0 {
		_ = putBytes(bkChat, critKey(convID), nil) // nil supprime la clé
		return
	}
	b, err := json.Marshal(list)
	if err != nil {
		return
	}
	_ = putStoreBytes(bkChat, critKey(convID), b)
}

func critDrop(convID string) { _ = putBytes(bkChat, critKey(convID), nil) }

// critAllPassed : le contrat est rempli. Une liste vide ne compte pas comme
// remplie — il n'y a simplement pas de contrat.
func critAllPassed(list []Criterion) bool {
	if len(list) == 0 {
		return false
	}
	for _, c := range list {
		if c.Status != "passed" {
			return false
		}
	}
	return true
}

func critPending(list []Criterion) int {
	n := 0
	for _, c := range list {
		if c.Status != "passed" {
			n++
		}
	}
	return n
}

// critOpen : les critères que le builder n'a pas encore déclarés faits —
// pending, ou failed et pas encore repris (la reprise les remet à completed).
func critOpen(list []Criterion) []Criterion {
	var out []Criterion
	for _, c := range list {
		if c.Status == "pending" || c.Status == "failed" || c.Status == "" {
			out = append(out, c)
		}
	}
	return out
}

// critRender : la liste formatée pour un prompt (builder ou verifier).
func critRender(list []Criterion) string {
	var b strings.Builder
	for _, c := range list {
		mark := "[ ]"
		switch c.Status {
		case "passed":
			mark = "[✓]"
		case "failed":
			mark = "[✗]"
		case "completed":
			mark = "[~]" // fait selon le builder, pas encore vérifié
		}
		fmt.Fprintf(&b, "%s #%d %s", mark, c.ID, c.Text)
		if c.Note != "" {
			fmt.Fprintf(&b, " — %s", c.Note)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func criteriaTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name: "criteria",
			Description: "Manage the acceptance criteria of the current task (the contract that defines DONE). " +
				"action=add posits new criteria (texts[]), action=set updates one (id + status, optional note), " +
				"action=list shows them, action=clear removes them all. Set criteria BEFORE building. Builder: mark each criterion completed once you have done AND checked it — verification only starts when nothing is left pending. Only the verification pass may mark passed or failed.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{"type": "string", "enum": []string{"add", "set", "list", "clear"}},
					"texts":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "add: one entry per criterion, short and testable"},
					"id":     map[string]any{"type": "integer", "description": "set: criterion id"},
					"status": map[string]any{"type": "string", "enum": []string{"pending", "completed", "passed", "failed"}},
					"note":   map[string]any{"type": "string", "description": "set: why it failed / how it was verified"},
				},
				"required": []string{"action"},
			},
		},
	}
}

// toolCriteria exécute l'outil. allowPass : seul le passage de VÉRIFICATION a
// le droit de marquer `passed` (sinon le builder s'auto-valide et le contrat ne
// contraint plus rien).
func toolCriteria(args map[string]any, allowPass bool) string {
	convID := convEnsureActive()
	action, _ := args["action"].(string)
	list := critList(convID)
	switch action {
	case "list", "":
		if len(list) == 0 {
			return "[aucun critère]"
		}
		return critRender(list)
	case "add":
		raw, _ := args["texts"].([]any)
		next := 1
		for _, c := range list {
			if c.ID >= next {
				next = c.ID + 1
			}
		}
		added := 0
		for _, r := range raw {
			s, ok := r.(string)
			if !ok || strings.TrimSpace(s) == "" {
				continue
			}
			if len(list) >= critMaxCount {
				break
			}
			list = append(list, Criterion{ID: next, Text: strings.TrimSpace(s), Status: "pending"})
			next++
			added++
		}
		if added == 0 {
			return "[erreur] aucun critère fourni (texts)"
		}
		critSave(convID, list)
		critNotify(list)
		return fmt.Sprintf("[ok] %d critère(s) ajouté(s)\n%s", added, critRender(list))
	case "set":
		// id numérique ou chaîne (« "2" », « "#2" ») : les petits modèles
		// écrivent volontiers l'id entre guillemets, qui tombait sinon à #0.
		id := 0
		switch v := args["id"].(type) {
		case float64:
			id = int(v)
		case string:
			id, _ = strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(v), "#"))
		}
		status, _ := args["status"].(string)
		note, _ := args["note"].(string)
		if status == "passed" && !allowPass {
			return "[refusé] Seule la passe de vérification peut marquer un critère passed. Termine ton implémentation ; la vérification suivra."
		}
		for i := range list {
			if list[i].ID != id {
				continue
			}
			if status != "" {
				list[i].Status = status
			}
			if note != "" {
				list[i].Note = strings.TrimSpace(note)
			}
			critSave(convID, list)
			critNotify(list)
			return "[ok] critère #" + fmt.Sprint(id) + " → " + list[i].Status
		}
		return fmt.Sprintf("[erreur] critère #%d inconnu", id)
	case "clear":
		critDrop(convID)
		critNotify(nil)
		return "[ok] critères effacés"
	}
	return "[erreur] action inconnue : " + action
}

// critNotify pousse la liste à l'UI (panneau critères) via le journal de la
// conversation. Best-effort : hors génération il n'y a pas d'epoch en cours,
// on publie sur l'epoch courant.
func critNotify(list []Criterion) {
	if conv == nil {
		return
	}
	conv.mu.Lock()
	epoch := conv.epoch
	conv.mu.Unlock()
	if list == nil {
		list = []Criterion{}
	}
	conv.appendDelta(epoch, map[string]any{"criteria": list, "ts": time.Now().UnixMilli()})
}
