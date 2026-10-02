package loki

// code_alias.go — réparation des noms d'outils et d'arguments hallucinés.
//
// Les petits modèles ont appris d'autres agents : ils appellent read_file,
// str_replace, run_command, ou passent path / old_string au lieu de file /
// old. L'appel tombait sur « outil inconnu » ou sur un argument manquant, et
// le tour se perdait en allers-retours. On traduit vers l'outil RÉEL quand la
// cible existe dans ce tour (généralisation du transformSubAgentAliases
// d'OpenFox : un outil nommé d'après un sous-agent devient subagent{role}).

import (
	"encoding/json"
	"strings"
)

// toolNameAliases : nom halluciné → outil de Loki.
var toolNameAliases = map[string]string{
	"read_file": "read", "view": "read", "cat": "read", "open_file": "read",
	"write_file": "write", "create_file": "write",
	"edit_file": "edit", "str_replace": "edit", "replace_in_file": "edit", "str_replace_editor": "edit",
	"run_command": "bash", "shell": "bash", "run_shell": "bash", "execute_command": "bash", "terminal": "bash",
	"search": "grep", "grep_search": "grep", "search_files": "grep",
	"find_files": "glob", "list_files": "glob", "file_search": "glob",
}

// toolArgAliases : par outil, argument halluciné → argument attendu.
var toolArgAliases = map[string]map[string]string{
	"read":  {"path": "file", "file_path": "file", "filename": "file", "filepath": "file"},
	"write": {"path": "file", "file_path": "file", "filename": "file", "filepath": "file", "contents": "content", "text": "content"},
	"edit": {"path": "file", "file_path": "file", "filename": "file", "filepath": "file",
		"old_string": "old", "old_str": "old", "old_text": "old", "search": "old",
		"new_string": "new", "new_str": "new", "new_text": "new", "replace": "new"},
	"bash":    {"cmd": "command"},
	"bash_bg": {"cmd": "command"},
	"grep":    {"query": "pattern", "regex": "pattern"},
	"glob":    {"query": "pattern"},
}

// repairToolCall corrige en place le nom et les arguments d'un appel quand ils
// visent, sous un autre nom, un outil disponible dans ce tour. Renvoie true si
// l'appel a été modifié. Les arguments doivent déjà être du JSON valide.
func repairToolCall(tc *ToolCall, tools []Tool) bool {
	have := make(map[string]bool, len(tools))
	for _, t := range tools {
		have[t.Function.Name] = true
	}
	name := tc.Function.Name
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil || args == nil {
		args = map[string]any{}
	}
	changed := false
	if !have[name] {
		lower := strings.ToLower(strings.TrimSpace(name))
		if t := toolNameAliases[lower]; t != "" && have[t] {
			name, changed = t, true
		} else if have["subagent"] {
			// Un sous-agent appelé comme un outil (« explorer », « code_reviewer »).
			role := strings.ReplaceAll(lower, "_", "-")
			for _, r := range subagentRoles {
				if role == r {
					if _, ok := args["role"]; !ok {
						args["role"] = r
					}
					if _, ok := args["task"]; !ok {
						for _, k := range []string{"prompt", "query", "question", "description"} {
							if v, ok := args[k].(string); ok && v != "" {
								args["task"] = v
								delete(args, k)
								break
							}
						}
					}
					name, changed = "subagent", true
					break
				}
			}
		}
	}
	for from, to := range toolArgAliases[name] {
		v, ok := args[from]
		if !ok {
			continue
		}
		if _, exists := args[to]; !exists {
			args[to] = v
			changed = true
		}
		delete(args, from)
	}
	if !changed {
		return false
	}
	b, err := json.Marshal(args)
	if err != nil {
		return false
	}
	tc.Function.Name = name
	tc.Function.Arguments = string(b)
	return true
}
