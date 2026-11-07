package loki

import (
	"strings"
	"testing"
)

// TestDedupableTool verrouille le fix du test IONOS : les outils à état vivant /
// à image (cu_*, see_image, bash) ne doivent JAMAIS être dédupliqués, sinon un
// second appel identique renvoie « [déjà fait] » sans rejouer l'action ni
// l'image, et l'IA boucle.
func TestDedupableTool(t *testing.T) {
	never := []string{"bash", "see_image", "browser_open", "browser_snapshot", "browser_click", "browser_type", "browser_key", "browser_scroll", "browser_screenshot"}
	for _, n := range never {
		if dedupableTool(n) {
			t.Errorf("%s ne doit PAS être déduplicable", n)
		}
	}
	always := []string{"write", "edit", "mem_add", "web_open"}
	for _, n := range always {
		if !dedupableTool(n) {
			t.Errorf("%s devrait rester déduplicable", n)
		}
	}
}

// TestCUGuard : page de blocage reconnue à son titre, rappel de conclure à la
// 25e action.
func TestCUGuard(t *testing.T) {
	cuBudget.Lock()
	cuBudget.n = 0
	cuBudget.Unlock()
	if out := cuGuard("[ok] cliqué [2]\n\n# Access Denied\nhttps://x.test/"); !strings.Contains(out, "page d'erreur ou de blocage") {
		t.Fatalf("blocage non vu : %q", out)
	}
	if out := cuGuard("# Boutique\nhttps://x.test/"); out != "# Boutique\nhttps://x.test/" {
		t.Fatalf("faux blocage : %q", out)
	}
	for i := 3; i < cuActionBudget; i++ {
		cuGuard("x")
	}
	if out := cuGuard("x"); !strings.Contains(out, "⏱ 25 actions") {
		t.Fatalf("pas de rappel : %q", out)
	}
}
