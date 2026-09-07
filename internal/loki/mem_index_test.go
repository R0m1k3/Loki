package loki

import (
	"strings"
	"testing"
)

// L'index est tenu par le CODE : créer une page l'y inscrit, la supprimer l'en
// retire. C'est tout l'intérêt — un modèle qui oublie de tenir son index ne peut
// plus le désynchroniser.
func TestMemIndexSuitLesPages(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()

	if err := MemAdd("docker.md", "# Notes Docker\n\ncontenu"); err != nil {
		t.Fatal(err)
	}
	idx := MemContent(memIndexFile)
	if !strings.Contains(idx, "](docker.md)") {
		t.Fatalf("page créée absente de l'index :\n%s", idx)
	}
	if !strings.Contains(idx, "Notes Docker") {
		t.Fatalf("l'index doit porter le titre de la page :\n%s", idx)
	}

	if err := MemDelete("docker.md"); err != nil {
		t.Fatal(err)
	}
	if idx := MemContent(memIndexFile); strings.Contains(idx, "](docker.md)") {
		t.Fatalf("page supprimée toujours indexée — l'IA la chercherait en vain :\n%s", idx)
	}
}

// L'accroche écrite après le titre appartient au MODÈLE : réenregistrer la page
// ne doit pas l'effacer, sinon tout ce qu'il ajoute disparaît à la prochaine
// modification.
func TestMemIndexPreserveLAccroche(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()
	if err := MemAdd("nas.md", "# NAS\n"); err != nil {
		t.Fatal(err)
	}
	// Le modèle enrichit la ligne d'index.
	enriched := strings.Replace(MemContent(memIndexFile), "- [NAS](nas.md)", "- [NAS](nas.md) — adresses IP et partages", 1)
	if err := MemSave(memIndexFile, "", enriched); err != nil {
		t.Fatal(err)
	}
	// Une réconciliation ne doit pas repasser dessus.
	reconcileMemIndex()
	memIndexAdd("nas.md")

	idx := MemContent(memIndexFile)
	if !strings.Contains(idx, "adresses IP et partages") {
		t.Fatalf("l'accroche du modèle a été écrasée :\n%s", idx)
	}
	if strings.Count(idx, "](nas.md)") != 1 {
		t.Fatalf("ligne dupliquée pour la même page :\n%s", idx)
	}
}

// Un nom qui est le SUFFIXE d'un autre ne doit pas emporter sa ligne : c'est ce
// que garantit le motif `](fichier.md)` plutôt qu'une simple recherche du nom.
func TestMemIndexNeConfondPasLesNomsProches(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()
	if err := MemAdd("notes.md", "# Notes\n"); err != nil {
		t.Fatal(err)
	}
	if err := MemAdd("mes-notes.md", "# Mes notes\n"); err != nil {
		t.Fatal(err)
	}
	if err := MemDelete("notes.md"); err != nil {
		t.Fatal(err)
	}
	idx := MemContent(memIndexFile)
	if !strings.Contains(idx, "](mes-notes.md)") {
		t.Fatalf("la suppression de notes.md a emporté mes-notes.md :\n%s", idx)
	}
	if strings.Contains(idx, "](notes.md)") {
		t.Fatalf("notes.md est toujours indexée :\n%s", idx)
	}
}

// L'index n'est injecté qu'en mode mémoire proactif : en « sur demande » ou
// « désactivée », le lister à chaque tour contredirait le réglage (et coûterait
// du contexte pour rien).
func TestMemIndexMessageSuitLeModeMemoire(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()
	if err := MemAdd("x.md", "# X\n"); err != nil {
		t.Fatal(err)
	}

	if err := setMemMode(MemAlways); err != nil {
		t.Fatal(err)
	}
	m, ok := memIndexMessage()
	if !ok {
		t.Fatal("aucun index injecté en mode auto")
	}
	if s, _ := m.Content.(string); !strings.HasPrefix(s, memIndexPrefix) || !strings.Contains(s, "](x.md)") {
		t.Fatalf("message d'index mal formé : %q", s)
	}

	for _, mode := range []MemMode{MemOnDemand, MemOff} {
		if err := setMemMode(mode); err != nil {
			t.Fatal(err)
		}
		if _, ok := memIndexMessage(); ok {
			t.Fatalf("index injecté en mode %q", mode)
		}
	}
}

// projectSystemMessages est le point d'entrée unique : il doit poser le contexte
// projet AVANT l'index mémoire (on situe le chantier, puis on liste), et ne rien
// produire quand il n'y a rien à dire.
func TestProjectSystemMessagesOrdreEtVide(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()
	if err := setMemMode(MemAlways); err != nil {
		t.Fatal(err)
	}

	if msgs := projectSystemMessages(); len(msgs) != 0 {
		t.Fatalf("%d message(s) alors qu'il n'y a ni description, ni page, ni tracker", len(msgs))
	}

	if err := setProjectDesc(defaultProjectSlug, "le NAS"); err != nil {
		t.Fatal(err)
	}
	if err := MemAdd("x.md", "# X\n"); err != nil {
		t.Fatal(err)
	}
	msgs := projectSystemMessages()
	if len(msgs) != 2 {
		t.Fatalf("%d message(s), attendu 2 (contexte + index)", len(msgs))
	}
	first, _ := msgs[0].Content.(string)
	second, _ := msgs[1].Content.(string)
	if !strings.HasPrefix(first, projectContextPrefix) {
		t.Fatalf("le contexte projet doit venir en premier : %q", first)
	}
	if !strings.HasPrefix(second, memIndexPrefix) {
		t.Fatalf("l'index mémoire doit suivre le contexte : %q", second)
	}
	for _, m := range msgs {
		if m.Role != "system" {
			t.Fatalf("rôle %q : ces messages doivent être fusionnables en tête par normalizeSystemMessages", m.Role)
		}
	}
}
