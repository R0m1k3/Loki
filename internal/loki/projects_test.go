package loki

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newProjectHome monte un LOKI_HOME neuf et repose les caches de projet : le slug
// actif est mémorisé en RAM (activeProjCache), donc sans ça un test hériterait du
// projet du précédent alors qu'il croit partir d'une base vierge.
func newProjectHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("LOKI_HOME", home)
	activeProjMu.Lock()
	activeProjCache = ""
	activeProjMu.Unlock()
	setProjectOverride("")
	t.Cleanup(func() {
		activeProjMu.Lock()
		activeProjCache = ""
		activeProjMu.Unlock()
		setProjectOverride("")
	})
	return home
}

// La migration doit être SILENCIEUSE et sans perte : une installation d'avant les
// projets a ses pages à plat dans memory/, elles doivent se retrouver dans
// « Générale » — et y être INDEXÉES, ce que rien ne fait autrement (l'index n'est
// alimenté que par la création d'une page).
func TestEnsureDefaultProjectMigreLaMemoirePlate(t *testing.T) {
	home := newProjectHome(t)
	flat := filepath.Join(home, "memory")
	if err := os.MkdirAll(flat, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flat, "docker.md"), []byte("# Notes Docker\n\ncontenu"), 0o644); err != nil {
		t.Fatal(err)
	}

	ensureDefaultProject()

	if activeProjectSlug() != defaultProjectSlug {
		t.Fatalf("projet actif %q après migration, attendu %q", activeProjectSlug(), defaultProjectSlug)
	}
	moved := filepath.Join(flat, defaultProjectSlug, "docker.md")
	if _, err := os.Stat(moved); err != nil {
		t.Fatalf("la page plate n'a pas été déplacée dans le projet : %v", err)
	}
	if _, err := os.Stat(filepath.Join(flat, "docker.md")); err == nil {
		t.Fatal("la page est restée à la racine : elle serait invisible ET dupliquée")
	}
	idx := MemContent(memIndexFile)
	if !strings.Contains(idx, "](docker.md)") {
		t.Fatalf("page migrée absente de l'index — elle ne serait jamais proposée à l'IA :\n%s", idx)
	}
	if !strings.Contains(idx, "Notes Docker") {
		t.Fatalf("l'index doit porter le TITRE de la page, pas seulement son nom :\n%s", idx)
	}
}

// Idempotence : le démarrage passe par ensureDefaultProject à chaque fois. Un
// second passage ne doit ni redupliquer une ligne d'index, ni créer un doublon
// de projet.
func TestEnsureDefaultProjectEstIdempotent(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()
	if err := MemAdd("notes.md", "# Notes\n\nx"); err != nil {
		t.Fatal(err)
	}
	before := MemContent(memIndexFile)

	ensureDefaultProject()

	if after := MemContent(memIndexFile); after != before {
		t.Fatalf("l'index a changé au 2e passage :\navant %q\naprès %q", before, after)
	}
	if n := len(listProjects()); n != 1 {
		t.Fatalf("%d projets après deux amorçages, attendu 1", n)
	}
}

// Le cloisonnement est la promesse centrale : une page écrite dans un projet ne
// doit pas exister dans un autre.
func TestMemoireCloisonneeParProjet(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()

	if err := MemAdd("generale.md", "# Page de Générale\n"); err != nil {
		t.Fatal(err)
	}
	p, err := createProject("Serveur NAS")
	if err != nil {
		t.Fatal(err)
	}
	if err := setActiveProject(p.Slug); err != nil {
		t.Fatal(err)
	}
	if err := MemAdd("nas.md", "# Page du NAS\n"); err != nil {
		t.Fatal(err)
	}

	names := func() []string {
		var out []string
		for _, m := range MemList() {
			out = append(out, m.Name)
		}
		return out
	}
	got := strings.Join(names(), " ")
	if strings.Contains(got, "generale.md") {
		t.Fatalf("la page de Générale est visible depuis le projet NAS : %s", got)
	}
	if !strings.Contains(got, "nas.md") {
		t.Fatalf("la page du projet NAS est absente de son propre projet : %s", got)
	}
	if err := setActiveProject(defaultProjectSlug); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(), " "); strings.Contains(got, "nas.md") {
		t.Fatalf("la page du NAS fuit dans Générale : %s", got)
	}
}

// Renommer ne doit RIEN casser : le slug est l'identité, il ne bouge pas, donc
// les pages restent là où elles sont.
func TestRenameProjectGardeLeSlugEtLesPages(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()
	p, err := createProject("Chantier")
	if err != nil {
		t.Fatal(err)
	}
	if err := setActiveProject(p.Slug); err != nil {
		t.Fatal(err)
	}
	if err := MemAdd("plan.md", "# Plan\n"); err != nil {
		t.Fatal(err)
	}

	if err := renameProject(p.Slug, "Chantier rénové"); err != nil {
		t.Fatal(err)
	}
	if projectName(p.Slug) != "Chantier rénové" {
		t.Fatalf("libellé non mis à jour : %q", projectName(p.Slug))
	}
	if activeProjectSlug() != p.Slug {
		t.Fatalf("le slug a bougé au renommage : %q", activeProjectSlug())
	}
	if len(MemList()) != 2 { // la page + l'index
		t.Fatalf("pages perdues au renommage : %d", len(MemList()))
	}
}

// Le dernier projet n'est pas supprimable : sans lui, il n'y a plus de mémoire du
// tout et aucun endroit où retomber.
func TestDeleteProjectRefuseLeDernier(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()
	if err := deleteProject(defaultProjectSlug); err == nil {
		t.Fatal("la suppression du dernier projet a été acceptée")
	}
}

// Supprimer le projet ACTIF doit rebasculer sur un autre, sinon l'application
// pointe sur un dossier qui n'existe plus.
func TestDeleteProjectActifRebascule(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()
	p, err := createProject("Temporaire")
	if err != nil {
		t.Fatal(err)
	}
	if err := setActiveProject(p.Slug); err != nil {
		t.Fatal(err)
	}
	if err := deleteProject(p.Slug); err != nil {
		t.Fatal(err)
	}
	if activeProjectSlug() == p.Slug {
		t.Fatal("le projet actif est resté sur le projet supprimé")
	}
	if projectExists(p.Slug) {
		t.Fatal("le projet supprimé figure encore au registre")
	}
	if _, err := os.Stat(projectMemoryDir(p.Slug)); err == nil {
		t.Fatal("le dossier mémoire du projet supprimé existe encore")
	}
}

// L'override sert aux tâches planifiées : il doit dévier memoryDir SANS toucher
// au projet persisté, sinon lancer une tâche changerait le projet à l'écran.
func TestProjectOverrideNeChangePasLActif(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()
	p, err := createProject("Veille")
	if err != nil {
		t.Fatal(err)
	}
	seen := ""
	withProject(p.Slug, func() { seen = memoryDir() })
	if seen != projectMemoryDir(p.Slug) {
		t.Fatalf("memoryDir n'a pas suivi l'override : %q", seen)
	}
	if activeProjectSlug() != defaultProjectSlug {
		t.Fatalf("l'override a changé le projet actif : %q", activeProjectSlug())
	}
	if memoryDir() != projectMemoryDir(defaultProjectSlug) {
		t.Fatal("memoryDir est resté sur le projet de l'override après sa libération")
	}
}

// slugifyIdent porte l'identité d'un projet ET d'un tracker : les accents doivent
// être translittérés, pas jetés — « Réseau » qui devient « r-seau » est illisible
// dans un chemin de fichier.
func TestSlugifyIdent(t *testing.T) {
	cases := map[string]string{
		"Serveur NAS":   "serveur-nas",
		"Réseau & Wifi": "reseau-wifi",
		"  Déjà vu  ":   "deja-vu",
		"Poids (kg)":    "poids-kg",
		"":              "",
	}
	for in, want := range cases {
		if got := slugifyIdent(in); got != want {
			t.Errorf("slugifyIdent(%q) = %q, attendu %q", in, got, want)
		}
	}
}

// Une description non vide doit atteindre l'IA ; une description vide ne doit pas
// produire un message de contexte creux.
func TestProjectContextMessage(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()

	if _, ok := projectContextMessage(); ok {
		t.Fatal("un message de contexte est produit alors qu'aucune description n'existe")
	}
	if err := setProjectDesc(defaultProjectSlug, "administration du NAS Unraid"); err != nil {
		t.Fatal(err)
	}
	m, ok := projectContextMessage()
	if !ok {
		t.Fatal("aucun message de contexte malgré une description")
	}
	s, _ := m.Content.(string)
	if !strings.HasPrefix(s, projectContextPrefix) {
		t.Fatalf("préfixe manquant, le message ne serait pas reconnaissable : %q", s)
	}
	if !strings.Contains(s, "NAS Unraid") {
		t.Fatalf("la description n'est pas dans le message : %q", s)
	}
}
