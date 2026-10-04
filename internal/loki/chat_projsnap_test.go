package loki

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// projSnapSetup : base et projet neufs, moteur de comédie qui répond « ok. ».
// on = PROJ_SNAPSHOT posée.
func projSnapSetup(t *testing.T, on bool) *moteurEcho {
	t.Helper()
	withWorkspace(t)
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
	ensureDefaultProject()
	freshTplProbe(t)
	if err := SetConfigKey("MODEL", "/models/"+echoTestModel); err != nil {
		t.Fatal(err)
	}
	if on {
		if err := SetConfigKey("PROJ_SNAPSHOT", "on"); err != nil {
			t.Fatal(err)
		}
	}
	m := &moteurEcho{reply: func(int, string) (int, string) {
		return 200, sseChunk("ok.") + sseFinal("stop", 100, 2)
	}}
	m.start(t)
	return m
}

// playTurn joue un tour utilisateur complet sur c et rend le corps de la PREMIÈRE
// requête de ce tour.
func playTurn(t *testing.T, m *moteurEcho, c *Conversation, caps Caps, text string) string {
	t.Helper()
	before := len(m.all())
	c.mu.Lock()
	c.Messages = append(c.Messages, um(text))
	c.mu.Unlock()
	c.generate(context.Background(), caps, 0.7, c.epoch)
	all := m.all()
	if len(all) <= before {
		t.Fatalf("aucune requête pour %q", text)
	}
	return all[before]
}

// userContents : le texte des messages user d'une requête, dans l'ordre.
func userContents(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, m := range requestMessages(t, body) {
		if m["role"] == "user" {
			s, _ := m["content"].(string)
			out = append(out, s)
		}
	}
	return out
}

func systemContent(t *testing.T, body string) string {
	t.Helper()
	for _, m := range requestMessages(t, body) {
		if m["role"] == "system" {
			s, _ := m["content"].(string)
			return s
		}
	}
	return ""
}

// oldAssembly : l'assemblage de generate d'avant PROJ_SNAPSHOT, recopié tel quel.
func oldAssembly(caps Caps, msgs []Message) []Message {
	final := msgs
	if caps.Agent {
		final = append(projectSystemMessages(), msgs...)
		if m, ok := codeInstructionsMessage(caps); ok {
			final = append([]Message{m}, final...)
		}
		if sp := readSysPrompt(); sp != "" {
			final = append([]Message{{Role: "system", Content: sp}}, final...)
		}
	}
	sent, _ := prepareTurn(final, caps)
	return normalizeSystemMessages(sent)
}

func jsonValue(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Clé absente : la requête est celle d'avant, à l'octet près — bloc vivant qui
// suit la page créée, aucune ligne système de plus, rien de persisté en plus.
func TestProjSnapDefautRienNeChange(t *testing.T) {
	m := projSnapSetup(t, false)
	if err := MemAdd("nas.md", "# NAS\n"); err != nil {
		t.Fatal(err)
	}
	c := newTestConv()
	caps := Caps{Agent: true, Mem: MemAlways}
	for i, text := range []string{"bonjour", "et ensuite ?"} {
		if i == 1 {
			if err := MemAdd("docker.md", "# Docker\n"); err != nil {
				t.Fatal(err)
			}
		}
		c.mu.Lock()
		want := oldAssembly(caps, append(append([]Message(nil), c.Messages...), um(text)))
		c.mu.Unlock()
		body := playTurn(t, m, c, caps, text)
		var got struct {
			Messages any `json:"messages"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Messages, jsonValue(t, want)) {
			t.Fatalf("tour %d : requête différente de l'assemblage d'avant\nreçu  %v\nattendu %v", i, got.Messages, jsonValue(t, want))
		}
		if strings.Contains(body, "context_update") || strings.Contains(body, "as of") {
			t.Fatalf("tour %d : trace du bloc figé sans la clé :\n%s", i, body)
		}
	}
	if us := userContents(t, m.all()[1]); !strings.Contains(us[0], "](docker.md)") {
		t.Fatalf("sans la clé, le bloc vivant doit montrer la nouvelle page : %q", us[0])
	}
	b, _ := json.Marshal(c)
	if c.ProjSnap != nil || strings.Contains(string(b), "proj_snap") {
		t.Fatalf("instantané persisté sans la clé : %s", b)
	}
}

// La première page d'un projet vide arrive en mise à jour : le premier message
// reste identique, la mise à jour est rangée dans l'historique et renvoyée telle
// quelle ensuite, la suivante n'annonce que la page ajoutée.
func TestProjSnapPageAjouteeEnMiseAJour(t *testing.T) {
	m := projSnapSetup(t, true)
	c := newTestConv()
	caps := Caps{Agent: true, Mem: MemAlways}
	b1 := playTurn(t, m, c, caps, "bonjour")
	if !strings.Contains(systemContent(t, b1), projSnapSystemLine) {
		t.Fatal("ligne fixe du système absente avec la clé")
	}
	if err := MemAdd("nas.md", "# NAS\n"); err != nil {
		t.Fatal(err)
	}
	b2 := playTurn(t, m, c, caps, "et le NAS ?")
	u1, u2 := userContents(t, b1), userContents(t, b2)
	if u2[0] != u1[0] {
		t.Fatalf("premier message changé :\navant %q\naprès %q", u1[0], u2[0])
	}
	last := u2[len(u2)-1]
	if !strings.HasPrefix(last, ctxUpdOpen) || !strings.Contains(last, "](nas.md)") || !strings.HasSuffix(last, "et le NAS ?") {
		t.Fatalf("mise à jour attendue en tête du nouveau message : %q", last)
	}
	if err := MemAdd("docker.md", "# Docker\n"); err != nil {
		t.Fatal(err)
	}
	b3 := playTurn(t, m, c, caps, "et docker ?")
	u3 := userContents(t, b3)
	if u3[0] != u1[0] || u3[1] != u2[1] {
		t.Fatalf("messages déjà envoyés modifiés :\n%q\n%q", u3[0], u3[1])
	}
	last = u3[len(u3)-1]
	if !strings.Contains(last, "+ [Docker](docker.md)") || strings.Contains(last, "nas.md") {
		t.Fatalf("seule la page ajoutée doit être annoncée : %q", last)
	}
	// Sans changement : rien de posé.
	b4 := playTurn(t, m, c, caps, "merci")
	if u4 := userContents(t, b4); u4[len(u4)-1] != "merci" {
		t.Fatalf("mise à jour vide posée : %q", u4[len(u4)-1])
	}
	// Persisté et relu : l'état annoncé survit.
	raw, _ := json.Marshal(c)
	c2 := newTestConv()
	c2.loadFrom("x", raw)
	if c2.ProjSnap == nil || c2.ProjSnap.State.Mem["docker.md"] == "" {
		t.Fatalf("instantané non relu : %+v", c2.ProjSnap)
	}
}

// Une nouvelle valeur de tracker remplace sa ligne (« ~ ») ; un tracker supprimé
// est annoncé comme tel.
func TestProjSnapTrackerLigneRemplacee(t *testing.T) {
	m := projSnapSetup(t, true)
	if _, err := trackerAdd("poids", "2026-10-01", "72.4 kg"); err != nil {
		t.Fatal(err)
	}
	c := newTestConv()
	caps := Caps{Agent: true, Mem: MemAlways}
	b1 := playTurn(t, m, c, caps, "salut")
	if u := userContents(t, b1); !strings.Contains(u[0], "Trackers (tracker tool) as of ") || strings.Contains(u[0], "answer straight from this") {
		t.Fatalf("en-tête figé attendu : %q", u[0])
	}
	if _, err := trackerAdd("poids", "2026-10-03", "73 kg"); err != nil {
		t.Fatal(err)
	}
	b2 := playTurn(t, m, c, caps, "mon poids ?")
	u := userContents(t, b2)
	if u[0] != userContents(t, b1)[0] {
		t.Fatal("premier message changé")
	}
	if !strings.Contains(u[len(u)-1], "~ poids — 73 kg (2026-10-03)") {
		t.Fatalf("ligne de tracker attendue : %q", u[len(u)-1])
	}
}

// Une compaction (début, fin, manuelle) reprend le bloc tout neuf au tour suivant
// et retire les mises à jour de la queue gardée : une vieille valeur ne doit pas
// suivre la fraîche.
func TestProjSnapCompactionRafraichit(t *testing.T) {
	m := projSnapSetup(t, true)
	if _, err := trackerAdd("poids", "2026-10-01", "72.4 kg"); err != nil {
		t.Fatal(err)
	}
	c := newTestConv()
	caps := Caps{Agent: true, Mem: MemAlways}
	playTurn(t, m, c, caps, "salut")
	if _, err := trackerAdd("poids", "2026-10-02", "72.8 kg"); err != nil {
		t.Fatal(err)
	}
	playTurn(t, m, c, caps, "note-le")
	if !hasContextUpdates(c.Messages) {
		t.Fatal("mise à jour attendue dans l'historique")
	}
	for i := 0; i < 10; i++ {
		c.Messages = append(c.Messages, am("étape "+strings.Repeat("détail ", 300)), um("continue "+strings.Repeat("x", 600)))
	}
	// Le message avec la mise à jour en dernier : il tombe dans la queue gardée.
	c.Messages = append(c.Messages, am("vu"), um(ctxUpdOpen+` at="x">`+"\n~ poids — 72.4 kg\n"+ctxUpdClose+"\n\nencore"))
	if _, changed := c.compactAndPublish(context.Background(), c.epoch, "test", append([]Message(nil), c.Messages...), 50000, caps); !changed {
		t.Fatal("compaction sans effet")
	}
	if c.ProjSnap != nil || hasContextUpdates(c.Messages) {
		t.Fatalf("après compaction : instantané %v, mises à jour restantes %v", c.ProjSnap != nil, hasContextUpdates(c.Messages))
	}
	b := playTurn(t, m, c, caps, "et maintenant ?")
	if strings.Contains(strings.Join(userContents(t, b), " "), ctxUpdOpen) {
		t.Fatalf("mise à jour après le bloc neuf : %s", b)
	}
	if u := userContents(t, b); !strings.Contains(u[0], "72.8 kg") {
		t.Fatalf("bloc neuf attendu avec la dernière valeur : %q", u[0])
	}
}

// Compaction EN COURS de tour : le bloc vivant remplace le figé dans la requête
// rejouée (la mise à jour avalée par le résumé n'est pas perdue), et c'est lui
// que le tour suivant renvoie.
func TestProjSnapCompactionEnTour(t *testing.T) {
	m := projSnapSetup(t, true)
	c := newTestConv()
	caps := Caps{Agent: true, Mem: MemAlways}
	playTurn(t, m, c, caps, "salut")
	if err := MemAdd("nas.md", "# NAS\n"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		c.Messages = append(c.Messages, am("étape "+strings.Repeat("détail ", 300)), um("continue "+strings.Repeat("x", 600)))
	}
	c.Messages = append(c.Messages, am("vu"))
	n := len(m.all())
	m.mu.Lock()
	m.reply = func(k int, _ string) (int, string) {
		if k == n {
			return 400, `{"error":{"code":400,"message":"request (99999 tokens) exceeds the available context size"}}`
		}
		return 200, sseChunk("ok.") + sseFinal("stop", 100, 2)
	}
	m.mu.Unlock()
	playTurn(t, m, c, caps, "et le NAS ?")
	all := m.all()
	first, retry := all[n], all[n+1]
	if uf := userContents(t, first); !strings.HasPrefix(uf[len(uf)-1], ctxUpdOpen) {
		t.Fatal("mise à jour attendue dans la requête refusée")
	}
	u := userContents(t, retry)
	if strings.Contains(strings.Join(u, " "), ctxUpdOpen) {
		t.Fatalf("mise à jour restée après la compaction en cours de tour : %q", u)
	}
	if !strings.Contains(u[0], "](nas.md)") {
		t.Fatalf("la page doit être dans le bloc rafraîchi : %q", u[0])
	}
	if c.ProjSnap == nil || c.ProjSnap.State.Mem["nas.md"] == "" || hasContextUpdates(c.Messages) {
		t.Fatalf("instantané rafraîchi non rangé : %+v", c.ProjSnap)
	}
	next := playTurn(t, m, c, caps, "merci")
	head := func(s string) string { return s[:strings.Index(s, "</project_context>")] }
	if head(userContents(t, next)[0]) != head(u[0]) {
		t.Fatal("le tour suivant doit renvoyer le bloc rafraîchi à l'identique")
	}
}

// Changer de projet, de discussion ou repartir à zéro ne garde pas l'instantané.
func TestProjSnapProjetEtDiscussion(t *testing.T) {
	m := projSnapSetup(t, true)
	c := newTestConv()
	caps := Caps{Agent: true, Mem: MemAlways}
	playTurn(t, m, c, caps, "salut")
	p, err := createProject("Jardin")
	if err != nil {
		t.Fatal(err)
	}
	if err := setProjectDesc(p.Slug, "Le potager."); err != nil {
		t.Fatal(err)
	}
	if err := setActiveProject(p.Slug); err != nil {
		t.Fatal(err)
	}
	b := playTurn(t, m, c, caps, "et le jardin ?")
	u := userContents(t, b)
	if !strings.Contains(u[0], "Le potager.") || strings.Contains(strings.Join(u, " "), ctxUpdOpen) {
		t.Fatalf("changement de projet : bloc neuf attendu, sans mise à jour : %q", u[0])
	}
	if c.ProjSnap == nil {
		t.Fatal("instantané attendu")
	}
	// loadFrom : fil neuf, ou enregistré sans le champ.
	c.loadFrom("neuf", nil)
	if c.ProjSnap != nil {
		t.Fatal("fil neuf : instantané hérité")
	}
	c.ProjSnap = &projSnapshot{Key: "x"}
	c.loadFrom("ancien", []byte(`{"messages":[{"role":"user","content":"vieux"}]}`))
	if c.ProjSnap != nil {
		t.Fatal("fil enregistré sans instantané : hérité")
	}
	c.ProjSnap = &projSnapshot{Key: "x"}
	c.Reset()
	if c.ProjSnap != nil {
		t.Fatal("Reset garde l'instantané")
	}
}

// Sans agent : ni bloc ni mise à jour, et les blocs restés d'avant ne partent pas.
func TestProjSnapSansAgent(t *testing.T) {
	m := projSnapSetup(t, true)
	c := newTestConv()
	playTurn(t, m, c, Caps{Agent: true, Mem: MemAlways}, "salut")
	if err := MemAdd("nas.md", "# NAS\n"); err != nil {
		t.Fatal(err)
	}
	playTurn(t, m, c, Caps{Agent: true, Mem: MemAlways}, "le NAS ?")
	if err := MemAdd("docker.md", "# Docker\n"); err != nil {
		t.Fatal(err)
	}
	before := c.ProjSnap.State
	b := playTurn(t, m, c, Caps{}, "réponds sans outils")
	if strings.Contains(b, "context_update") || strings.Contains(b, "project_context") {
		t.Fatalf("agent off : contexte projet envoyé : %s", b)
	}
	if last := c.Messages[len(c.Messages)-2]; msgText(last) != "réponds sans outils" {
		t.Fatalf("agent off : message modifié : %q", msgText(last))
	}
	if !reflect.DeepEqual(c.ProjSnap.State, before) {
		t.Fatal("agent off : état annoncé modifié")
	}
}

// Clé retirée après usage : l'instantané et les blocs sont nettoyés, retour au
// bloc vivant sans vieille mise à jour derrière lui.
func TestProjSnapCleRetiree(t *testing.T) {
	m := projSnapSetup(t, true)
	c := newTestConv()
	caps := Caps{Agent: true, Mem: MemAlways}
	playTurn(t, m, c, caps, "salut")
	if err := MemAdd("nas.md", "# NAS\n"); err != nil {
		t.Fatal(err)
	}
	playTurn(t, m, c, caps, "le NAS ?")
	if err := SetConfigKey("PROJ_SNAPSHOT", ""); err != nil {
		t.Fatal(err)
	}
	b := playTurn(t, m, c, caps, "et après ?")
	if strings.Contains(b, "context_update") || strings.Contains(b, "as of") {
		t.Fatalf("restes du bloc figé : %s", b)
	}
	if c.ProjSnap != nil || hasContextUpdates(c.Messages) {
		t.Fatal("historique non nettoyé")
	}
}

// Rafraîchissement : clé de validité (redémarrage, réglage moteur), système ou
// outils changés, seuil d'accumulation.
func TestProjSnapApplyRafraichit(t *testing.T) {
	projSnapSetup(t, true)
	if err := MemAdd("nas.md", "# NAS\n"); err != nil {
		t.Fatal(err)
	}
	caps := Caps{Agent: true, Mem: MemAlways}
	live := projSnapCapture(caps, "t0")
	c := newTestConv()
	c.Messages = []Message{um("a")}
	if r, _ := c.projSnapApplyLocked(live, "k", "s", "t0"); !r {
		t.Fatal("premier tour : instantané attendu")
	}
	if err := MemAdd("docker.md", "# Docker\n"); err != nil {
		t.Fatal(err)
	}
	live2 := projSnapCapture(caps, "t1")
	c.Messages = append(c.Messages, am("b"), um("c"))
	if r, d := c.projSnapApplyLocked(live2, "k", "s", "t1"); r || d == "" {
		t.Fatalf("mise à jour attendue : refresh=%v delta=%q", r, d)
	}
	for _, tc := range []struct{ name, key, sys string }{{"clé", "k2", "s"}, {"système", "k", "s2"}} {
		snap := *c.ProjSnap
		c.Messages = append(c.Messages, am("d"), um("e"))
		if r, _ := c.projSnapApplyLocked(live, tc.key, tc.sys, "t2"); !r {
			t.Fatalf("%s changé : rafraîchissement attendu", tc.name)
		}
		if hasContextUpdates(c.Messages) {
			t.Fatalf("%s : mises à jour restées après rafraîchissement", tc.name)
		}
		c.ProjSnap = &snap
	}
	// Seuil : les mises à jour accumulées dépassent la limite.
	c.ProjSnap.Key, c.ProjSnap.SysHash = "k", "s"
	c.ProjSnap.DeltaTok = projSnapDeltaLimit(c.ProjSnap.SnapTok)
	c.ProjSnap.State = live.state
	if r, _ := c.projSnapApplyLocked(live2, "k", "s", "t3"); !r {
		t.Fatal("seuil dépassé : rafraîchissement attendu")
	}
	// La clé suit le lancement de Loki.
	k1 := projSnapKey(caps, ReadConfig())
	old := projSnapBoot
	projSnapBoot = "autre"
	defer func() { projSnapBoot = old }()
	if projSnapKey(caps, ReadConfig()) == k1 {
		t.Fatal("redémarrage : clé inchangée")
	}
	projSnapBoot = old
	if err := SetConfigKey("REASONING_EFFORT", "high"); err != nil {
		t.Fatal(err)
	}
	if projSnapKey(caps, ReadConfig()) != k1 {
		t.Fatal("un réglage de conversation ne doit pas rafraîchir")
	}
	if err := SetConfigKey("MODEL", "/models/autre.gguf"); err != nil {
		t.Fatal(err)
	}
	if projSnapKey(caps, ReadConfig()) == k1 {
		t.Fatal("changement de modèle : clé inchangée")
	}
}

// La prose repart en entier ; les sections faites de lignes, ligne à ligne.
func TestProjDeltaParType(t *testing.T) {
	projSnapSetup(t, true)
	old := projState{
		Code: "Repository instructions (AGENTS.md) — follow them:\n\nmake test", Desc: "Ancienne description.",
		MemOn: true, MemProse: "# Index", Mem: map[string]string{"a.md": "- [A](a.md)", "b.md": "- [B](b.md)"},
		Trk: map[string]string{"poids": "poids — 72 kg (2026-10-01)", "pas": "pas — 8000 (2026-10-01)"},
	}
	lv := projLive{
		state: projState{
			Code: "Repository instructions (AGENTS.md) — follow them:\n\ngo test ./...", Desc: "Nouvelle description,\nsur deux lignes.",
			MemOn: true, MemProse: "# Index", Mem: map[string]string{"a.md": "- [A](a.md) — accroche", "c.md": "- [C](c.md)"},
			Trk: map[string]string{"poids": "poids — 73 kg (2026-10-03)", "eau": "eau — 2 L (2026-10-03)"},
		},
		memOrder: []string{"a.md", "c.md"},
		trkOrder: []string{"poids", "eau"},
	}
	d := projDelta(old, lv, "2026-10-04 09:00")
	for _, want := range []string{
		ctxUpdOpen + ` at="2026-10-04 09:00">`,
		"go test ./...",
		"Nouvelle description,\nsur deux lignes.",
		"~ [A](a.md) — accroche", "+ [C](c.md)", "- b.md (deleted)",
		"~ poids — 73 kg (2026-10-03)", "+ eau — 2 L (2026-10-03)", "- pas — 8000 (2026-10-01) (deleted)",
	} {
		if !strings.Contains(d, want) {
			t.Fatalf("manque %q dans :\n%s", want, d)
		}
	}
	if strings.Contains(d, "make test") || strings.Contains(d, "Ancienne") {
		t.Fatalf("l'ancien texte ne doit pas repartir :\n%s", d)
	}
	// En-tête de l'index modifié : l'index repart en entier.
	lv.state.MemProse = "# Index (revu)"
	lv.memMsg = Message{Content: "Project memory index … # Index (revu)"}
	if d := projDelta(old, lv, "t"); !strings.Contains(d, "Current version, replaces any earlier one — Project memory index") {
		t.Fatalf("prose de l'index modifiée : index complet attendu :\n%s", d)
	}
	if projDelta(old, projLive{state: old}, "t") != "" {
		t.Fatal("rien de changé : mise à jour vide attendue")
	}
}

// Les blocs sont retirés partout où le texte de l'utilisateur est lu : tâche du
// vérificateur, titre, export, entrée du résumeur ; en texte et en multimodal.
func TestProjSnapRetireDesLecteurs(t *testing.T) {
	testHome(t)
	blk := ctxUpdOpen + ` at="t">` + "\n+ [A](a.md)\n" + ctxUpdClose + "\n\n"
	c := newTestConv()
	c.Messages = []Message{um(blk + "corrige le build"), am("ok")}
	if got := c.lastUserText(); got != "corrige le build" {
		t.Fatalf("tâche du vérificateur : %q", got)
	}
	if got := convSummary(c.Messages); got != "corrige le build" {
		t.Fatalf("titre : %q", got)
	}
	out, err := c.ExportJSON(exportOpts{})
	if err != nil || strings.Contains(string(out), "context_update") {
		t.Fatalf("export : %v %s", err, out)
	}
	multi := []Message{
		{Role: "user", Content: []any{map[string]any{"type": "text", "text": blk}, map[string]any{"type": "text", "text": "vois l'image"}}},
		{Role: "user", Content: []map[string]any{{"type": "text", "text": blk + "suite"}}},
	}
	got := stripContextUpdates(multi)
	if hasContextUpdates(got) || len(got[0].Content.([]any)) != 1 || got[1].Content.([]map[string]any)[0]["text"] != "suite" {
		t.Fatalf("multimodal : %+v", got)
	}
	if hasContextUpdates(multi) == false {
		t.Fatal("l'original ne doit pas être modifié")
	}
	// Un texte qui en parle plus loin n'est pas touché.
	plain := []Message{um("explique " + blk)}
	if s := stripContextUpdates(plain); msgText(s[0]) != msgText(plain[0]) {
		t.Fatal("bloc hors tête retiré")
	}
	// Mise à jour posée puis retirée : retour exact au texte.
	for _, m0 := range []Message{um("x"), {Role: "user", Content: []any{map[string]any{"type": "text", "text": "vois"}}}} {
		m1, ok := prependCtxUpdate(m0, blk)
		if !ok {
			t.Fatal("pose refusée")
		}
		m2, _ := withoutCtxUpdate(m1)
		if !reflect.DeepEqual(jsonValue(t, m2), jsonValue(t, m0)) {
			t.Fatalf("aller-retour : %+v", m2)
		}
	}
}

// Valeurs de la clé ; un preset externe n'est jamais concerné.
func TestProjSnapEnabled(t *testing.T) {
	for v, want := range map[string]bool{"": false, "off": false, "0": false, "on": true, "1": true, "oui": true, " ON ": true} {
		if got := projSnapEnabled(map[string]string{"PROJ_SNAPSHOT": v}); got != want {
			t.Fatalf("%q : %v", v, got)
		}
	}
	if projSnapEnabled(map[string]string{"PROJ_SNAPSHOT": "on", extKeyFlag: "1"}) {
		t.Fatal("preset externe : jamais de bloc figé")
	}
}

// replaceProjectHead garde le rappel des pages lues, après le bloc neuf.
func TestReplaceProjectHead(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "base"},
		{Role: "system", Content: projectContextPrefix + " — vieux"},
		{Role: "system", Content: memIndexPrefix + " vieux"},
		{Role: "system", Content: memReminderPrefix + " a.md"},
		um("q"),
	}
	block := []Message{{Role: "system", Content: memIndexPrefix + " neuf"}}
	out := replaceProjectHead(msgs, block)
	var got []string
	for _, m := range out {
		got = append(got, msgText(m))
	}
	want := []string{"base", memIndexPrefix + " neuf", memReminderPrefix + " a.md", "q"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
	// Bloc vide au départ : inséré après le système commun.
	out = replaceProjectHead([]Message{{Role: "system", Content: "base"}, um("q")}, block)
	if msgText(out[1]) != memIndexPrefix+" neuf" || msgText(out[2]) != "q" {
		t.Fatalf("%+v", out)
	}
}
