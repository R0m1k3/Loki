//go:build linux

package loki

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// parity_projects_test.go — journal de référence du hub des projets (lot 18
// de la migration Rust) : liste et comptes (discussions, pages, trackers),
// création (slug translittéré, unique, tronqué), renommage, description,
// bascule et suppression (discussions, trackers et mémoire du projet, actif
// réassigné, dernier projet indestructible), refus pendant une génération,
// erreurs JSON. Après chaque appel : le registre, le projet actif, l'arbre
// memory/, les trackers et l'index des discussions. La conversation est tenue
// par le Go (le Rust le rejoue contre le faux Go, qui fait de même).
// Rangé dans rust/loki-rs/tests/parity_projects/projects.json ;
// tests/projects_api.rs le rejoue par la façade.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteProjets

const parityProjectsFile = "../../rust/loki-rs/tests/parity_projects/projects.json"

type parityProjectStep struct {
	Name       string         `json:"name"`
	Generating bool           `json:"generating,omitempty"`
	Method     string         `json:"method"`
	Body       string         `json:"body,omitempty"`
	Status     int            `json:"status"`
	Out        any            `json:"out"`
	After      map[string]any `json:"after"`
}

// parityZeroTimes : les dates de création, propres à chaque exécution.
func parityZeroTimes(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if k == "created" || k == "created_at" || k == "updated" {
				t[k] = 0
			} else {
				t[k] = parityZeroTimes(x)
			}
		}
	case []any:
		for i := range t {
			t[i] = parityZeroTimes(t[i])
		}
	}
	return v
}

func TestPariteProjets(t *testing.T) {
	home := testHome(t)
	ensureDefaultProject()
	// Le projet actif est gardé en RAM d'un test à l'autre.
	activeProjMu.Lock()
	activeProjCache = ""
	activeProjMu.Unlock()
	t.Cleanup(func() {
		activeProjMu.Lock()
		activeProjCache = ""
		activeProjMu.Unlock()
	})
	// Date de création fixe : la graine est rangée telle quelle.
	if err := saveProjects([]Project{{Slug: defaultProjectSlug, Name: defaultProjectName, CreatedAt: 1}}); err != nil {
		t.Fatal(err)
	}
	// Pages de Générale : trois comptées, le reste non.
	for _, f := range []string{"a.md", "b.MD", "z.md", ".cache.md", "notes.txt", "sous/page.md"} {
		p := filepath.Join(projectMemoryDir(defaultProjectSlug), f)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("# "+f+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Discussions : deux de Générale (dont une d'avant les projets), trois
	// d'Alpha (créé plus loin), une d'un projet disparu.
	idx := []convMeta{
		{ID: "g1", Title: "g1", Created: 1, Updated: 10, Project: "generale"},
		{ID: "o1", Title: "o1", Created: 2, Updated: 20},
		{ID: "a1", Title: "a1", Created: 3, Updated: 30, Project: "alpha"},
		{ID: "a2", Title: "a2", Created: 4, Updated: 50, Project: "alpha"},
		{ID: "a3", Title: "a3", Created: 5, Updated: 40, Project: "alpha"},
		{ID: "x1", Title: "x1", Created: 6, Updated: 60, Project: "ailleurs"},
	}
	convIndexSave(idx)
	for _, m := range idx {
		_ = putBytes(bkChat, convKey(m.ID), []byte(`{"id":"`+m.ID+`","log":[]}`))
	}
	// La discussion ouverte, comme au démarrage du Go (convEnsureActive).
	_ = putStr(bkChat, ckActive, "g1")
	convActivate("g1", getBytes(bkChat, convKey("g1")))
	seen := map[string]bool{}
	for _, m := range idx {
		seen[m.ID] = true
	}
	for _, k := range []string{"generale/poids", "alpha/sommeil", "alpha/pas", "alphabet/x"} {
		_ = putStr(bkTracker, k, `{"name":"`+k+`","events":[]}`)
	}

	snapshot := func() map[string]any {
		st := map[string]any{}
		var reg any
		_ = json.Unmarshal(getBytes(bkState, stProjectsKey), &reg)
		st["projects"] = parityZeroTimes(reg)
		st["active"] = getStr(bkState, stActiveProjectKey)
		var tree []string
		_ = filepath.Walk(filepath.Join(home, "memory"), func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(home, p)
			if fi.IsDir() {
				rel += "/"
			}
			tree = append(tree, rel)
			return nil
		})
		sort.Strings(tree)
		st["tree"] = tree
		var trk []string
		for k := range allKV(bkTracker) {
			trk = append(trk, k)
		}
		sort.Strings(trk)
		st["trackers"] = trk
		var convs []string
		for _, m := range convIndex() {
			id := m.ID
			if !seen[id] {
				id = "{{NEW}}" // discussion créée par la bascule, identifiant tiré au hasard
			}
			convs = append(convs, id+":"+m.Project)
		}
		sort.Strings(convs)
		st["convs"] = convs
		return st
	}
	seed := snapshot()
	seedMem := map[string]string{}
	_ = filepath.Walk(filepath.Join(home, "memory"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(home, p)
			seedMem[rel] = string(b)
		}
		return nil
	})
	seedStore := map[string]string{
		"state/projects":       string(getBytes(bkState, stProjectsKey)),
		"state/active_project": getStr(bkState, stActiveProjectKey),
		"chat/index":           string(getBytes(bkChat, ckIndex)),
	}
	for _, m := range idx {
		seedStore["chat/"+convKey(m.ID)] = string(getBytes(bkChat, convKey(m.ID)))
	}
	for k, v := range allKV(bkTracker) {
		seedStore["tracker/"+k] = string(v)
	}

	long := strings.Repeat("é", 2100) + "  fin"
	var steps []parityProjectStep
	for _, st := range []parityProjectStep{
		{Name: "liste", Method: "GET"},
		{Name: "création", Method: "POST", Body: `{"action":"create","name":"Alpha"}`},
		{Name: "création, même nom", Method: "POST", Body: `{"action":"create","name":"Alpha"}`},
		{Name: "création translittérée", Method: "POST", Body: `{"action":" CREATE ","name":"  Réseau NAS — ça ñ  "}`},
		{Name: "création sans lettres", Method: "POST", Body: `{"action":"create","name":"!!!"}`},
		{Name: "création sans lettres bis", Method: "POST", Body: `{"action":"create","name":"???"}`},
		{Name: "création longue", Method: "POST",
			Body: `{"action":"create","name":"` + strings.Repeat("Ab-", 30) + `"}`},
		{Name: "création très longue", Method: "POST",
			Body: `{"action":"create","name":"` + strings.Repeat("ü", 70) + `"}`},
		{Name: "création vide", Method: "POST", Body: `{"action":"create","name":"   "}`},
		{Name: "renommage", Method: "POST", Body: `{"action":"rename","slug":" alpha-2 ","name":"  Bêta  "}`},
		{Name: "renommage vide", Method: "POST", Body: `{"action":"rename","slug":"alpha-2","name":""}`},
		{Name: "renommage inconnu", Method: "POST", Body: `{"action":"rename","slug":"nope","name":"X"}`},
		{Name: "description", Method: "POST", Body: `{"action":"desc","slug":"alpha","desc":"  Le projet Alpha.  "}`},
		{Name: "description longue", Method: "POST", Body: `{"action":"desc","slug":"alpha-2","desc":"` + long + `"}`},
		{Name: "description coupée sur une espace", Method: "POST",
			Body: `{"action":"desc","slug":"projet","desc":"` + strings.Repeat("é", 1999) + `  suite"}`},
		{Name: "description effacée", Method: "POST", Body: `{"action":"desc","slug":"alpha-2","desc":"   "}`},
		{Name: "description inconnue", Method: "POST", Body: `{"action":"desc","slug":"nope","desc":"x"}`},
		{Name: "bascule", Method: "POST", Body: `{"action":"switch","slug":"alpha"}`},
		{Name: "bascule sur l'actif", Method: "POST", Body: `{"action":"switch","slug":"alpha"}`},
		{Name: "bascule inconnue", Method: "POST", Body: `{"action":"switch","slug":"nope"}`},
		{Name: "bascule pendant une génération", Generating: true, Method: "POST", Body: `{"action":"switch","slug":"generale"}`},
		{Name: "suppression pendant une génération", Generating: true, Method: "POST", Body: `{"action":"delete","slug":"alpha"}`},
		{Name: "suppression inconnue pendant une génération", Generating: true, Method: "POST", Body: `{"action":"delete","slug":"nope"}`},
		{Name: "bascule inconnue pendant une génération", Generating: true, Method: "POST", Body: `{"action":"switch","slug":"nope"}`},
		{Name: "suppression de l'actif", Method: "POST", Body: `{"action":"delete","slug":"alpha"}`},
		{Name: "suppression inconnue", Method: "POST", Body: `{"action":"delete","slug":"nope"}`},
		{Name: "suppression", Method: "POST", Body: `{"action":"delete","slug":"projet"}`},
		{Name: "bascule vers un projet sans discussion", Method: "POST", Body: `{"action":"switch","slug":"projet-2"}`},
		{Name: "suppression bis", Method: "POST", Body: `{"action":"delete","slug":"alpha-2"}`},
		{Name: "suppression ter", Method: "POST", Body: `{"action":"delete","slug":"reseau-nas-ca-n"}`},
		{Name: "liste après", Method: "GET"},
		{Name: "action inconnue", Method: "POST", Body: `{"action":"Nuke","slug":"generale"}`},
		{Name: "action vide", Method: "POST", Body: `{}`},
		{Name: "corps vide", Method: "POST", Body: ``},
		{Name: "corps illisible", Method: "POST", Body: `{"action":"create"`},
		{Name: "type faux", Method: "POST", Body: `{"action":"create","name":7}`},
		{Name: "méthode refusée", Method: "PUT", Body: `{}`},
	} {
		conv.mu.Lock()
		conv.Generating = st.Generating
		conv.mu.Unlock()
		rec := httptest.NewRecorder()
		handleProjects(rec, httptest.NewRequest(st.Method, "/api/projects", strings.NewReader(st.Body)))
		conv.mu.Lock()
		conv.Generating = false
		conv.mu.Unlock()
		st.Status = rec.Code
		var out any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s : %q", st.Name, rec.Body.String())
		}
		st.Out = parityZeroTimes(out)
		st.After = snapshot()
		steps = append(steps, st)
	}
	// Les dernières suppressions, jusqu'au dernier projet.
	for _, p := range listProjects() {
		if p.Slug == defaultProjectSlug {
			continue
		}
		rec := httptest.NewRecorder()
		handleProjects(rec, httptest.NewRequest("POST", "/api/projects",
			strings.NewReader(`{"action":"delete","slug":"`+p.Slug+`"}`)))
		var out any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		steps = append(steps, parityProjectStep{Name: "suppression de " + p.Slug, Method: "POST",
			Body: `{"action":"delete","slug":"` + p.Slug + `"}`, Status: rec.Code, Out: parityZeroTimes(out), After: snapshot()})
	}
	rec := httptest.NewRecorder()
	handleProjects(rec, httptest.NewRequest("POST", "/api/projects", strings.NewReader(`{"action":"delete","slug":"generale"}`)))
	var out any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	steps = append(steps, parityProjectStep{Name: "dernier projet", Method: "POST",
		Body: `{"action":"delete","slug":"generale"}`, Status: rec.Code, Out: parityZeroTimes(out), After: snapshot()})

	got, err := parityJSON(map[string]any{"seed": seed, "seed_store": seedStore, "seed_mem": seedMem, "steps": steps})
	if err != nil {
		t.Fatal(err)
	}
	got = []byte(strings.ReplaceAll(string(got), home, "{{HOME}}"))
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityProjectsFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityProjectsFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityProjectsFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("le hub des projets ne produit plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteProjets,\n"+
			"puis cargo test --test projects_api dit si le port Rust suit.\n--- obtenu ---\n%s", parityProjectsFile, got)
	}
}
