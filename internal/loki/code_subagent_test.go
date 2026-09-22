package loki

import (
	"context"
	"strings"
	"testing"
)

// Les garde-fous de l'outil subagent doivent répondre SANS appeler le modèle :
// un rôle inconnu, une tâche vide ou un appel depuis un sous-agent sont refusés
// à l'entrée (sinon chaque faute de frappe coûte une génération complète).
func TestSubagentRefusesBeforeAnyInference(t *testing.T) {
	cases := []struct {
		name   string
		args   map[string]any
		parent Caps
		want   string
	}{
		{"rôle inconnu", map[string]any{"role": "architecte", "task": "x"}, Caps{Code: true}, "rôle inconnu"},
		{"rôle verifier refusé", map[string]any{"role": "verifier", "task": "x"}, Caps{Code: true}, "rôle inconnu"},
		{"tâche vide", map[string]any{"role": "explorer", "task": "  "}, Caps{Code: true}, "tâche vide"},
		{"pas de récursion", map[string]any{"role": "explorer", "task": "x"}, Caps{Code: true, Role: "explorer"}, "ne peut pas en appeler un autre"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolSubagent(context.Background(), tc.args, tc.parent)
			if !strings.HasPrefix(got, "[erreur]") || !strings.Contains(got, tc.want) {
				t.Fatalf("attendu un refus contenant %q, obtenu %q", tc.want, got)
			}
		})
	}
}

// Un rôle délégué travaille en LECTURE SEULE : ni write/edit (les modifications
// restent dans le fil principal), ni subagent (pas de récursion), et seul le
// planner pose des critères.
func TestSubagentRolesAreReadOnly(t *testing.T) {
	forbidden := map[string]bool{"write": true, "edit": true, "subagent": true, "mem_add": true, "mem_edit": true, "web_search": true}
	for _, role := range subagentRoles {
		names := map[string]bool{}
		for _, tool := range EnabledTools(Caps{Agent: true, Code: true, Role: role}) {
			names[tool.Function.Name] = true
		}
		for f := range forbidden {
			if names[f] {
				t.Fatalf("le rôle %s ne devrait pas avoir l'outil %s", role, f)
			}
		}
		if !names["read"] || !names["grep"] {
			t.Fatalf("le rôle %s devrait pouvoir lire le code", role)
		}
		if got := names["criteria"]; got != (role == "planner") {
			t.Fatalf("critères pour %s : %v (attendu %v)", role, got, role == "planner")
		}
	}
}
