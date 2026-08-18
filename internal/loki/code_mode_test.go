package loki

import "testing"

// La bascule est persistée par discussion et retombe sur chat par défaut.
func TestModeParDiscussion(t *testing.T) {
	withWorkspace(t)
	id := convEnsureActive()
	if convCodeMode() {
		t.Fatal("mode code par défaut")
	}
	if err := setConvMode(id, "code"); err != nil {
		t.Fatal(err)
	}
	if !convCodeMode() {
		t.Fatal("bascule sans effet")
	}
	dropConvMode(id)
	if convCodeMode() {
		t.Fatal("mode code après nettoyage")
	}
}

// La détection reconnaît une tâche de code, pas la conversation ordinaire.
func TestDetectionCode(t *testing.T) {
	for _, s := range []string{
		"corrige le bug dans main.go",
		"j'ai une stack trace : Exception in thread",
		"```python\nprint('x')\n```",
		"implémente la fonction de tri",
		"le build casse avec une erreur de compilation",
	} {
		if !looksLikeCode(s) {
			t.Errorf("non détecté : %q", s)
		}
	}
	for _, s := range []string{
		"quelle est la capitale de l'Australie ?",
		"raconte-moi une histoire",
		"quel temps fait-il demain ?",
		"",
	} {
		if looksLikeCode(s) {
			t.Errorf("détecté à tort : %q", s)
		}
	}
}

// Les rôles embarqués existent tous, et le prompt du builder est court —
// la règle « ne pas regonfler » de baseSystemPrompt s'applique aussi ici.
func TestRolesEmbarques(t *testing.T) {
	for _, r := range roleNames {
		p := rolePrompt(r)
		if p == "" {
			t.Errorf("rôle %s : prompt vide", r)
		}
		if len(p) > 2500 {
			t.Errorf("rôle %s : prompt trop long (%d octets)", r, len(p))
		}
	}
	if rolePrompt("inexistant") != "" {
		t.Error("rôle inconnu devrait rendre vide")
	}
}

// wantsPlan : demande de plan reconnue, implémentation directe non.
func TestWantsPlan(t *testing.T) {
	if !wantsPlan("Fais un plan avant de coder") {
		t.Error("plan non reconnu")
	}
	if wantsPlan("corrige le bug maintenant") {
		t.Error("plan détecté à tort")
	}
}
