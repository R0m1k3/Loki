package loki

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AGENTS.md du dépôt cloné part avec le contexte en mode Code, déplacé dans le
// premier message utilisateur (préfixe système intact).
func TestConsignesDuDepot(t *testing.T) {
	withWorkspace(t)
	if _, ok := codeInstructionsMessage(Caps{Code: true}); ok {
		t.Fatal("consignes trouvées dans un dossier vide")
	}
	repo := filepath.Join(agentCwd(), "projet")
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("Build : make test\n"+strings.Repeat("x", 5000)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := codeInstructionsMessage(Caps{}); ok {
		t.Fatal("consignes injectées hors mode Code")
	}
	m, ok := codeInstructionsMessage(Caps{Code: true})
	s := msgText(m)
	if !ok || !strings.Contains(s, "make test") || !strings.Contains(s, "projet/AGENTS.md") || !strings.Contains(s, "tronqué") {
		t.Fatalf("message = %.200q", s)
	}
	out := normalizeSystemMessages([]Message{{Role: "system", Content: "base"}, m, {Role: "user", Content: "fais-le"}})
	if msgText(out[0]) != "base" || !strings.Contains(msgText(out[1]), "make test") {
		t.Fatalf("consignes restées dans le système : %+v", out)
	}
}
