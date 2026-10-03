package loki

import (
	"strings"
	"testing"
)

func mcpTestTool(server, tool string) Tool {
	return Tool{Type: "function", Function: ToolFunction{
		Name:        mcpExposedName(server, tool),
		Description: "[MCP: " + server + "] décrit " + tool,
	}}
}

// La ligne MCP du préambule se déduit des outils envoyés sur le MÊME tour : elle
// ne peut ni manquer au premier tour après un démarrage (pool encore vide quand
// le préambule était bâti), ni compter un outil masqué, ni apparaître pour un
// rôle qui ne reçoit aucun outil MCP.
func TestMCPPromptLineDeriveDesOutils(t *testing.T) {
	cases := []struct {
		name  string
		tools []Tool
		want  string
	}{
		{"aucun outil", nil, ""},
		{"aucun outil MCP", []Tool{bashTool(), readTool()}, ""},
		{"un serveur", []Tool{bashTool(), mcpTestTool("github", "issues"), mcpTestTool("github", "prs")},
			"github (2)"},
		{"plusieurs serveurs, triés", []Tool{mcpTestTool("zeta", "a"), mcpTestTool("alpha.io", "b"), mcpTestTool("zeta", "c")},
			"alpha.io (1), zeta (2)"},
		{"description sans le préfixe [MCP: …] ignorée", []Tool{{Type: "function", Function: ToolFunction{Name: "mcp__x__y", Description: "rien"}}},
			""},
		{"description vide après le préfixe", []Tool{{Type: "function", Function: ToolFunction{Name: "mcp__x__y", Description: "[MCP: x] "}}},
			"x (1)"},
	}
	for _, c := range cases {
		got := mcpPromptLine(c.tools)
		if c.want == "" {
			if got != "" {
				t.Errorf("%s : ligne %q, attendu aucune", c.name, got)
			}
			continue
		}
		if !strings.HasSuffix(got, ": "+c.want+".") {
			t.Errorf("%s : ligne %q, attendu la liste %q", c.name, got, c.want)
		}
	}
}

// Le préambule et les outils du tour viennent de la même tranche : le rôle
// planner (aucun outil MCP) n'a pas de ligne MCP, le fil principal l'a dès le
// premier tour.
func TestBaseSystemPromptLigneMCPSuitLesOutils(t *testing.T) {
	caps := Caps{Agent: true, Mem: MemOff}
	with := append(EnabledTools(caps), mcpTestTool("github", "issues"))
	if sp := baseSystemPrompt(caps, with); !strings.Contains(sp, "MCP servers connected") || !strings.Contains(sp, "github (1)") {
		t.Fatalf("ligne MCP absente alors qu'un outil MCP part sur ce tour :\n%s", sp)
	}
	if sp := baseSystemPrompt(caps, EnabledTools(caps)); strings.Contains(sp, "MCP servers") {
		t.Fatalf("ligne MCP sans aucun outil MCP envoyé :\n%s", sp)
	}
	planner := Caps{Agent: true, Code: true, Role: "planner", Mem: MemOff}
	if sp := baseSystemPrompt(planner, EnabledTools(planner)); strings.Contains(sp, "MCP servers") {
		t.Fatalf("le planner ne reçoit aucun outil MCP, sa ligne ne doit pas l'annoncer :\n%s", sp)
	}
}

// prepareTurn rend la séquence ET les outils qu'elle décrit : le préambule
// injecté est exactement celui de baseSystemPrompt sur ces outils.
func TestPrepareTurnPreambuleCoherentAvecOutils(t *testing.T) {
	testHome(t)
	caps := Caps{Agent: true, Mem: MemOff}
	sent, tools := prepareTurn([]Message{{Role: "user", Content: "salut"}}, caps)
	if len(sent) != 2 || sent[0].Role != "system" {
		t.Fatalf("séquence inattendue : %#v", sent)
	}
	sys, _ := sent[0].Content.(string)
	if !strings.HasPrefix(sys, baseSystemPrompt(caps, tools)) {
		t.Fatalf("le préambule ne correspond pas aux outils renvoyés :\n%s", sys)
	}
}
