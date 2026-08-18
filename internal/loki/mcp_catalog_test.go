package loki

import (
	"strings"
	"testing"
)

// Le catalogue est embarqué : s'il ne décode pas, le binaire livré est cassé et
// le panneau MCP part vide. Ce test attrape une virgule en trop au build.
func TestMCPCatalogDecodes(t *testing.T) {
	entries, err := mcpCatalogEntries()
	if err != nil {
		t.Fatalf("catalogue illisible : %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("catalogue vide")
	}

	seen := map[string]bool{}
	for _, e := range entries {
		if e.ID == "" || e.Name == "" || e.Command == "" {
			t.Errorf("entrée incomplète : %+v", e)
		}
		if seen[e.ID] {
			t.Errorf("id dupliqué : %s", e.ID)
		}
		seen[e.ID] = true
		if e.Description == "" {
			t.Errorf("%s : description manquante", e.ID)
		}
		// Un secret annoncé doit exister comme variable d'environnement, sinon
		// l'UI met en avant un champ que l'utilisateur ne peut pas remplir.
		for _, s := range e.Secrets {
			if _, ok := e.Env[s]; !ok {
				t.Errorf("%s : secret %q absent de env", e.ID, s)
			}
		}
	}
}

// Les jetons {workspace} doivent disparaître : un serveur lancé avec un chemin
// littéral « {workspace} » démarre puis échoue à la première lecture.
func TestMCPCatalogExpandsWorkspaceToken(t *testing.T) {
	entries, err := MCPCatalog()
	if err != nil {
		t.Fatalf("MCPCatalog : %v", err)
	}
	for _, e := range entries {
		for _, a := range e.Args {
			if strings.Contains(a, "{workspace}") {
				t.Errorf("%s : jeton non résolu dans args : %s", e.ID, a)
			}
		}
		for k, v := range e.Env {
			if strings.Contains(v, "{workspace}") {
				t.Errorf("%s : jeton non résolu dans env[%s] : %s", e.ID, k, v)
			}
		}
	}
}

func TestReasoningEffortValue(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"  ":      "",
		"auto":    "",
		"default": "",
		"off":     "none",
		"none":    "none",
		"false":   "none",
		"0":       "none",
		"low":     "low",
		"MEDIUM":  "medium",
		" High ":  "high",
	}
	for in, want := range cases {
		if got := reasoningEffortValue(in); got != want {
			t.Errorf("reasoningEffortValue(%q) = %q, attendu %q", in, got, want)
		}
	}
}

func TestReasoningExplicitlyOff(t *testing.T) {
	// Clé absente ou vide = pas de consigne, surtout pas une interdiction.
	for _, v := range []string{"", "   ", "on", "auto", "deepseek"} {
		if reasoningExplicitlyOff(v) {
			t.Errorf("reasoningExplicitlyOff(%q) = true, attendu false", v)
		}
	}
	for _, v := range []string{"off", "none", "0", "false", " Disabled "} {
		if !reasoningExplicitlyOff(v) {
			t.Errorf("reasoningExplicitlyOff(%q) = false, attendu true", v)
		}
	}
}

func TestReasoningTemplateKwargs(t *testing.T) {
	// « aucune » doit parler aux gabarits hybrides, pas seulement à ceux qui
	// lisent reasoning_effort : sinon le modèle réfléchit quand même.
	if got := reasoningTemplateKwargs(false, "none"); got == nil || got["enable_thinking"] != false {
		t.Errorf("effort none : %v, attendu enable_thinking=false", got)
	}
	if got := reasoningTemplateKwargs(true, ""); got == nil || got["enable_thinking"] != false {
		t.Errorf("REASONING=off : %v, attendu enable_thinking=false", got)
	}
	// Un niveau explicite voyage aussi dans les kwargs : les binaires anciens ne
	// relaient pas le champ de haut niveau au gabarit.
	if got := reasoningTemplateKwargs(false, "high"); got == nil || got["reasoning_effort"] != "high" {
		t.Errorf("effort high : %v, attendu reasoning_effort=high", got)
	}
	if _, ok := reasoningTemplateKwargs(false, "high")["enable_thinking"]; ok {
		t.Error("un niveau explicite ne doit pas couper le raisonnement")
	}
	// Auto = aucune consigne : on n'écrit rien dans la requête.
	if got := reasoningTemplateKwargs(false, ""); got != nil {
		t.Errorf("auto : %v, attendu nil", got)
	}
}
