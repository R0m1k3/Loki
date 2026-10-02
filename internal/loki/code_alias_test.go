package loki

import (
	"encoding/json"
	"testing"
)

func outils(noms ...string) []Tool {
	var ts []Tool
	for _, n := range noms {
		ts = append(ts, Tool{Type: "function", Function: ToolFunction{Name: n}})
	}
	return ts
}

// Noms et arguments appris d'autres agents : traduits vers les outils de Loki.
func TestReparationDesAppelsHallucines(t *testing.T) {
	tools := outils("read", "edit", "bash", "subagent")
	cas := []struct {
		name, args, wantName string
		wantArgs             map[string]any
	}{
		{"read_file", `{"path":"a.go"}`, "read", map[string]any{"file": "a.go"}},
		{"str_replace", `{"file_path":"a.go","old_string":"x","new_string":"y"}`, "edit", map[string]any{"file": "a.go", "old": "x", "new": "y"}},
		{"run_command", `{"cmd":"ls"}`, "bash", map[string]any{"command": "ls"}},
		{"edit", `{"file":"a.go","old_str":"x","new":"y"}`, "edit", map[string]any{"file": "a.go", "old": "x", "new": "y"}},
		{"code_reviewer", `{"prompt":"relis"}`, "subagent", map[string]any{"role": "code-reviewer", "task": "relis"}},
	}
	for _, c := range cas {
		tc := ToolCall{Function: ToolCallFunc{Name: c.name, Arguments: c.args}}
		if !repairToolCall(&tc, tools) {
			t.Errorf("%s : non réparé", c.name)
			continue
		}
		var got map[string]any
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &got)
		if tc.Function.Name != c.wantName || len(got) != len(c.wantArgs) {
			t.Errorf("%s → %s %v", c.name, tc.Function.Name, got)
			continue
		}
		for k, v := range c.wantArgs {
			if got[k] != v {
				t.Errorf("%s : %s=%v, attendu %v", c.name, k, got[k], v)
			}
		}
	}
	// Outil cible absent de ce tour, ou appel déjà correct : intact.
	for _, tc := range []ToolCall{
		{Function: ToolCallFunc{Name: "write_file", Arguments: `{"path":"a"}`}},
		{Function: ToolCallFunc{Name: "read", Arguments: `{"file":"a"}`}},
	} {
		if repairToolCall(&tc, tools) {
			t.Errorf("modifié à tort : %+v", tc)
		}
	}
}
