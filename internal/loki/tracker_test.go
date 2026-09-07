package loki

import (
	"strings"
	"testing"
	"time"
)

// parseWhen accepte une date à précision variable. Le point important est
// dateOnly : un relevé saisi « le 12 mars » n'a pas d'heure, l'afficher
// « 00:00 » serait une information inventée.
func TestParseWhenPrecisionVariable(t *testing.T) {
	cases := []struct {
		in       string
		dateOnly bool
	}{
		{"2026", true},
		{"2026-07", true},
		{"2026-07-15", true},
		{"2026-07-15 14:30", false},
		{"2026-07-15T14:30", false},
	}
	for _, c := range cases {
		ts, dateOnly, err := parseWhen(c.in)
		if err != nil {
			t.Fatalf("parseWhen(%q) : %v", c.in, err)
		}
		if ts <= 0 {
			t.Fatalf("parseWhen(%q) rend un instant nul", c.in)
		}
		if dateOnly != c.dateOnly {
			t.Fatalf("parseWhen(%q) dateOnly = %v, attendu %v", c.in, dateOnly, c.dateOnly)
		}
	}
	if _, _, err := parseWhen("la semaine dernière"); err == nil {
		t.Fatal("une date incompréhensible doit être refusée, pas rangée à un instant arbitraire")
	}
	// Vide = maintenant, et AVEC heure : c'est une saisie à l'instant t.
	ts, dateOnly, err := parseWhen("")
	if err != nil || dateOnly || ts <= 0 {
		t.Fatalf("parseWhen(\"\") = (%d, %v, %v), attendu maintenant avec heure", ts, dateOnly, err)
	}
}

// Un point sans heure ne doit jamais s'afficher avec un faux minuit.
func TestFmtEventSansHeure(t *testing.T) {
	ts, dateOnly, err := parseWhen("2026-07-15")
	if err != nil {
		t.Fatal(err)
	}
	got := fmtEvent(TrackerEvent{TS: ts, DateOnly: dateOnly})
	if strings.Contains(got, ":") {
		t.Fatalf("un point sans heure affiche une heure : %q", got)
	}
	if got != "2026-07-15" {
		t.Fatalf("fmtEvent = %q, attendu 2026-07-15", got)
	}
}

// Le stockage est cloisonné par projet, comme les pages : un tracker d'un projet
// ne doit pas apparaître dans un autre.
func TestTrackerCloisonneParProjet(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()

	if _, err := trackerAdd("poids", "2026-01-02", "78,2 kg"); err != nil {
		t.Fatal(err)
	}
	p, err := createProject("NAS")
	if err != nil {
		t.Fatal(err)
	}
	if err := setActiveProject(p.Slug); err != nil {
		t.Fatal(err)
	}
	if n := len(trackerList()); n != 0 {
		t.Fatalf("%d tracker(s) visibles depuis un projet neuf", n)
	}
	if _, err := trackerAdd("température", "2026-01-02", "41 °C"); err != nil {
		t.Fatal(err)
	}
	if n := len(trackerList()); n != 1 {
		t.Fatalf("%d tracker(s) dans le projet NAS, attendu 1", n)
	}
	if err := setActiveProject(defaultProjectSlug); err != nil {
		t.Fatal(err)
	}
	list := trackerList()
	if len(list) != 1 || list[0].Name != "poids" {
		t.Fatalf("le projet Générale ne retrouve pas son tracker : %+v", list)
	}
}

// Les points sont triés par date à l'enregistrement : on peut donc saisir un
// relevé oublié après coup sans que l'historique parte en désordre.
func TestTrackerTriParDate(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()

	for _, when := range []string{"2026-03-01", "2026-01-01", "2026-02-01"} {
		if _, err := trackerAdd("poids", when, "valeur "+when); err != nil {
			t.Fatal(err)
		}
	}
	s, ok := trackerLoad("poids")
	if !ok {
		t.Fatal("tracker introuvable")
	}
	if len(s.Events) != 3 {
		t.Fatalf("%d points, attendu 3", len(s.Events))
	}
	for i := 1; i < len(s.Events); i++ {
		if s.Events[i-1].TS > s.Events[i].TS {
			t.Fatalf("points non triés : %d après %d", s.Events[i].TS, s.Events[i-1].TS)
		}
	}
	// La vue d'ensemble doit annoncer la DERNIÈRE valeur, pas la dernière saisie.
	m := trackerList()[0]
	if !strings.Contains(m.LastText, "2026-03-01") {
		t.Fatalf("dernière valeur = %q, attendu celle de mars", m.LastText)
	}
}

// Renommer re-clé le tracker quand le slug change : sans ça, un point ajouté
// après renommage (trackerAdd re-slugifie le nom) créerait un tracker parallèle.
func TestTrackerRenameRecleLeTracker(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()
	if _, err := trackerAdd("poids", "2026-01-01", "78 kg"); err != nil {
		t.Fatal(err)
	}
	if err := trackerRename("poids", "Masse corporelle"); err != nil {
		t.Fatal(err)
	}
	if _, ok := trackerLoad("poids"); ok {
		t.Fatal("l'ancienne clé existe encore : deux trackers pour la même donnée")
	}
	s, ok := trackerLoad("masse-corporelle")
	if !ok {
		t.Fatal("tracker introuvable sous son nouveau slug")
	}
	if len(s.Events) != 1 {
		t.Fatalf("%d point(s) après renommage, attendu 1", len(s.Events))
	}
	// Un point ajouté sous le NOUVEAU nom doit rejoindre le même tracker.
	if _, err := trackerAdd("Masse corporelle", "2026-02-01", "77 kg"); err != nil {
		t.Fatal(err)
	}
	if n := len(trackerList()); n != 1 {
		t.Fatalf("%d trackers après ajout post-renommage, attendu 1", n)
	}
}

// Déplacer vers un autre projet emporte les points, et ne laisse rien derrière.
func TestTrackerMoveToProject(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()
	if _, err := trackerAdd("conso", "2026-01-01", "412 kWh"); err != nil {
		t.Fatal(err)
	}
	p, err := createProject("Maison")
	if err != nil {
		t.Fatal(err)
	}
	if err := trackerMoveToProject("conso", p.Slug); err != nil {
		t.Fatal(err)
	}
	if n := len(trackerList()); n != 0 {
		t.Fatalf("%d tracker(s) restants dans le projet d'origine", n)
	}
	if err := setActiveProject(p.Slug); err != nil {
		t.Fatal(err)
	}
	s, ok := trackerLoad("conso")
	if !ok || len(s.Events) != 1 {
		t.Fatalf("tracker mal arrivé dans le projet cible : ok=%v", ok)
	}
}

// La vue par niveaux ne doit JAMAIS tout charger : c'est la raison d'être des
// trackers. Sur un historique long, la vue d'entrée résume par année.
func TestTrackerViewNeChargeJamaisTout(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()

	// 400 points étalés sur trois ans : bien au-delà du plafond d'affichage.
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.Local)
	for i := 0; i < 400; i++ {
		d := base.AddDate(0, 0, i*3).Format("2006-01-02")
		if _, err := trackerAdd("conso", d, "valeur "+d); err != nil {
			t.Fatal(err)
		}
	}
	skeleton := trackerView("conso", "")
	if strings.Count(skeleton, "\n") > 10 {
		t.Fatalf("la vue d'entrée déverse l'historique :\n%s", skeleton)
	}
	if !strings.Contains(skeleton, "Années") {
		t.Fatalf("la vue d'entrée doit résumer par année :\n%s", skeleton)
	}
	// Une descente au mois liste les points, mais reste plafonnée.
	events := trackerView("conso", "2024-01")
	if n := strings.Count(events, "\n- "); n > trackerEventsCap {
		t.Fatalf("%d points listés, plafond %d", n, trackerEventsCap)
	}
}

// L'index injecté porte la DERNIÈRE valeur de chaque tracker : c'est ce qui
// permet à l'IA de répondre « tu pesais 78 kg » sans appeler l'outil.
func TestTrackerIndexMessagePorteLaDerniereValeur(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()
	if err := setMemMode(MemAlways); err != nil {
		t.Fatal(err)
	}
	if _, ok := trackerIndexMessage(); ok {
		t.Fatal("un index est produit alors qu'aucun tracker n'existe")
	}
	if _, err := trackerAdd("poids", "2026-01-01", "78 kg"); err != nil {
		t.Fatal(err)
	}
	if _, err := trackerAdd("poids", "2026-02-01", "77 kg"); err != nil {
		t.Fatal(err)
	}
	m, ok := trackerIndexMessage()
	if !ok {
		t.Fatal("aucun index malgré un tracker")
	}
	s, _ := m.Content.(string)
	if !strings.HasPrefix(s, trackerIndexPrefix) {
		t.Fatalf("préfixe manquant : %q", s)
	}
	if !strings.Contains(s, "77 kg") {
		t.Fatalf("la dernière valeur n'est pas dans l'index : %q", s)
	}
	if strings.Contains(s, "78 kg") {
		t.Fatalf("l'index déverse l'historique au lieu du dernier point : %q", s)
	}
}

// L'outil du modèle : les quatre actions passent par un seul schéma, et
// supprimer un tracker ENTIER lui est refusé — une erreur d'id ne doit pas
// effacer des années de relevés.
func TestToolTrackerActions(t *testing.T) {
	newProjectHome(t)
	ensureDefaultProject()

	if out := toolTracker(map[string]any{"action": "add", "name": "poids", "when": "2026-01-01", "text": "78 kg"}); !strings.HasPrefix(out, "[ok]") {
		t.Fatalf("ajout refusé : %q", out)
	}
	s, ok := trackerLoad("poids")
	if !ok || len(s.Events) != 1 {
		t.Fatal("le point n'a pas été enregistré")
	}
	id := s.Events[0].ID

	if out := toolTracker(map[string]any{"action": "edit", "name": "poids", "id": id, "text": "77 kg"}); !strings.HasPrefix(out, "[ok]") {
		t.Fatalf("modification refusée : %q", out)
	}
	if s, _ := trackerLoad("poids"); s.Events[0].Text != "77 kg" {
		t.Fatalf("texte non modifié : %q", s.Events[0].Text)
	}

	if out := toolTracker(map[string]any{"action": "delete", "name": "poids"}); !strings.HasPrefix(out, "[erreur]") {
		t.Fatalf("suppression du tracker entier acceptée par l'outil : %q", out)
	}
	if _, ok := trackerLoad("poids"); !ok {
		t.Fatal("le tracker a été supprimé malgré le refus annoncé")
	}

	if out := toolTracker(map[string]any{"action": "delete", "name": "poids", "id": id}); !strings.HasPrefix(out, "[ok]") {
		t.Fatalf("suppression du point refusée : %q", out)
	}
	if out := toolTracker(map[string]any{"action": "danser"}); !strings.HasPrefix(out, "[erreur]") {
		t.Fatalf("action inconnue acceptée : %q", out)
	}
}

// str tolère qu'un modèle envoie un nombre là où le schéma annonce une chaîne :
// c'est fréquent, et refuser l'appel pour ça n'apprend rien au modèle.
func TestStrTolerantAuType(t *testing.T) {
	if got := str(float64(42)); got != "42" {
		t.Fatalf("str(42.0) = %q, attendu \"42\"", got)
	}
	if got := str(nil); got != "" {
		t.Fatalf("str(nil) = %q", got)
	}
	if got := str("texte"); got != "texte" {
		t.Fatalf("str(\"texte\") = %q", got)
	}
}
