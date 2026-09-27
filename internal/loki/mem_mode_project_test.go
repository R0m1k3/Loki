package loki

import "testing"

// Chaque projet garde son propre mode mémoire ; un projet sans réglage retombe
// sur l'ancien global MEM_MODE.
func TestMemModePerProject(t *testing.T) {
	testHome(t)
	ensureDefaultProject()
	if err := SetConfigKey("MEM_MODE", "ondemand"); err != nil {
		t.Fatal(err)
	}
	if memMode() != MemOnDemand {
		t.Fatalf("sans réglage propre, repli global attendu, got %q", memMode())
	}
	b, err := createProject("Chantier")
	if err != nil {
		t.Fatal(err)
	}
	if err := setActiveProject(b.Slug); err != nil {
		t.Fatal(err)
	}
	if err := setMemMode(MemSearchFirst); err != nil {
		t.Fatal(err)
	}
	if memMode() != MemSearchFirst {
		t.Fatalf("mode du projet actif attendu, got %q", memMode())
	}
	if err := setActiveProject(defaultProjectSlug); err != nil {
		t.Fatal(err)
	}
	if memMode() != MemOnDemand {
		t.Fatalf("l'autre projet ne doit pas hériter du réglage, got %q", memMode())
	}
	if setProjectMemMode(b.Slug, "n'importe quoi") == nil {
		t.Fatal("un mode inconnu doit être refusé")
	}
}
