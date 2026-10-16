package loki

import (
	"encoding/json"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// parity_mem_test.go — journal de référence de la mémoire (lot 13 de la
// migration Rust) : noms et titres de pages, slugs, modes mémoire, dates des
// trackers, puis une suite d'appels aux routes (éditeur des pages, index
// MEMORY.md, déplacement entre projets, mode agent, mode mémoire, trackers)
// avec, après chacun, le contenu du dossier memory/ et des clés touchées en
// base. Rangé dans rust/loki-rs/tests/parity_mem/mem.json ; tests/memory.rs le
// rejoue par la façade.
//
// Les dates sont lues dans un fuseau fixe (UTC+2) ; les identifiants de points
// (horodatages en ns) deviennent {{ID1}}, {{ID2}}… dans l'ordre d'apparition,
// et les instants « maintenant » {{NOW}}.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteMemoire

const parityMemFile = "../../rust/loki-rs/tests/parity_mem/mem.json"

type parityMemStep struct {
	// Avant l'appel : fichiers sous memory/ et valeurs en base (« bucket/clé »)
	// posés ; une valeur vide supprime.
	Files  map[string]string `json:"files,omitempty"`
	Store  map[string]string `json:"store,omitempty"`
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Body   string            `json:"body,omitempty"`
	Status int               `json:"status"`
	Out    any               `json:"out"`
	// Après l'appel.
	Disk  map[string]string `json:"disk"`
	State any               `json:"state"`
}

type parityMemWhen struct {
	In       string `json:"in"`
	TS       any    `json:"ts,omitempty"`
	DateOnly bool   `json:"date_only,omitempty"`
	Err      string `json:"err,omitempty"`
}

type parityMemFixture struct {
	Names  []parityNameCase  `json:"names"`
	Titles map[string]string `json:"titles"`
	Slugs  map[string]string `json:"slugs"`
	Modes  map[string]string `json:"modes"`
	When   []parityMemWhen   `json:"when"`
	Seed   map[string]string `json:"seed"`
	Steps  []parityMemStep   `json:"steps"`
}

type parityNameCase struct {
	In  string `json:"in"`
	Out string `json:"out,omitempty"`
	Err string `json:"err,omitempty"`
}

var (
	parityIDRe  = regexp.MustCompile(`"id":"(\d{15,})"`)
	parityNowRe = regexp.MustCompile(`"(ts|first_ts|last_ts)":(\d{12,14})`)
)

// parityMasker remplace identifiants et instants « maintenant » d'un JSON
// canonique (clés triées), dans l'ordre d'apparition.
type parityMasker struct {
	ids map[string]string
}

func (m *parityMasker) mask(s string) string {
	s = parityIDRe.ReplaceAllStringFunc(s, func(x string) string {
		id := parityIDRe.FindStringSubmatch(x)[1]
		ph, ok := m.ids[id]
		if !ok {
			ph = "{{ID" + strconv.Itoa(len(m.ids)+1) + "}}"
			m.ids[id] = ph
		}
		return `"id":"` + ph + `"`
	})
	now := time.Now().UnixMilli()
	return parityNowRe.ReplaceAllStringFunc(s, func(x string) string {
		sm := parityNowRe.FindStringSubmatch(x)
		v, _ := strconv.ParseInt(sm[2], 10, 64)
		if d := v - now; d > -86400000 && d < 86400000 {
			return `"` + sm[1] + `":"{{NOW}}"`
		}
		return x
	})
}

// unmask : les {{IDn}} d'un corps de requête rendus à leur valeur.
func (m *parityMasker) unmask(s string) string {
	for id, ph := range m.ids {
		s = strings.ReplaceAll(s, ph, id)
	}
	return s
}

// canon : JSON aux clés triées, masqué, relu.
func (m *parityMasker) canon(t *testing.T, raw []byte) any {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("JSON illisible : %v (%s)", err, raw)
	}
	b, _ := json.Marshal(v)
	var out any
	if err := json.Unmarshal([]byte(m.mask(string(b))), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPariteMemoire(t *testing.T) {
	home := testHome(t)
	old := time.Local
	time.Local = time.FixedZone("essai", 7200)
	t.Cleanup(func() { time.Local = old })

	var fx parityMemFixture
	for _, in := range []string{"", "  ", "notes", "notes.md", "NOTES.MD", "a b", "../x", "x/y", "é", "a.b-c_d",
		".md", ".", "..", "x.md.md", " pad ", "tab\t", "MEMORY", "memory.md", "x.Md"} {
		c := parityNameCase{In: in}
		if out, err := memFileName(in); err != nil {
			c.Err = err.Error()
		} else {
			c.Out = out
		}
		fx.Names = append(fx.Names, c)
	}
	fx.Titles = map[string]string{}
	for _, in := range []string{"", "\n\n", "# Titre", "## Titre ##", "   ###   ", "\n  \n# A\n# B", "sans dièse\nsuite",
		"#\n#\nVrai titre", "\t# tab", "#  espace insécable ", "#  #  double"} {
		fx.Titles[in] = titleOf(in)
	}
	fx.Slugs = map[string]string{}
	for _, in := range []string{"Réseau NAS", "  Hello World  ", "àâäáã éèêë", "İstanbul", "Ωmega", strings.Repeat("ab", 30),
		"---x---", "", "Ça_va?", "ÉCOLE", "Straße", strings.Repeat("a", 39) + "-bcd", "!!!", "Perso & Cie", "ÎLE ÔÔ ÙÛ Ñ Ç"} {
		fx.Slugs[in] = slugifyIdent(in)
	}
	fx.Modes = map[string]string{}
	for _, in := range []string{"", "off", " OFF ", "ondemand", "on-demand", "search", "always", "auto", "bizarre"} {
		fx.Modes[in] = string(normalizeMemMode(in))
	}
	mk := &parityMasker{ids: map[string]string{}}
	for _, in := range []string{"", "now", "NOW", " Maintenant ", "2024-03-10", "2024-03-10T08:15", "2024-03-10 7:05",
		"2024-03-10 07:5", "2024-03-10 07:05:09", "2024-03-10 07:05:09.5", "2024-03-10 07:05:09,123456789999",
		"2024-03-10 24:00", "2024-03-10 23:60", "2024-13-01", "2024-02-29", "2023-02-29", "2024-04-31", "2024-03",
		"2024", "24", "2024-3", "2024-03-1", " 2024-03-10 ", "2024-03-10  07:05", "demain", "2024-03-10T07:05Z",
		"today", "+024", "0000-01-01", "9999-12-31 23:59:59", "１２３４", "2024-03-10 07", "2024-03-10 7",
		"x\x01y", "2024-03-10T", "2024-03-10 07:05:60"} {
		c := parityMemWhen{In: in}
		ts, dateOnly, err := parseWhen(in)
		if err != nil {
			c.Err = err.Error()
		} else {
			c.TS = ts
			if d := ts - time.Now().UnixMilli(); d > -86400000 && d < 86400000 {
				c.TS = "{{NOW}}"
			}
			c.DateOnly = dateOnly
		}
		fx.When = append(fx.When, c)
	}

	// Base de départ : deux projets, le premier actif, mode mémoire global.
	fx.Seed = map[string]string{
		"state/projects":       `[{"slug":"generale","name":"Générale","created_at":1000},{"slug":"perso","name":"Perso & Cie","created_at":2000,"desc":"à moi"}]`,
		"state/active_project": "generale",
		"config/MEM_MODE":      "ondemand",
	}
	apply := func(store map[string]string) {
		keys := make([]string, 0, len(store))
		for k := range store {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b, key, _ := strings.Cut(k, "/")
			if b == bkState && key == stActiveProjectKey {
				if store[k] == "" {
					putStr(bkState, key, "")
					activeProjMu.Lock()
					activeProjCache = ""
					activeProjMu.Unlock()
				} else if err := setActiveProject(store[k]); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := putStr(b, key, store[k]); err != nil {
				t.Fatal(err)
			}
		}
	}
	apply(fx.Seed)

	memRoot := filepath.Join(home, "memory")
	disk := func() map[string]string {
		out := map[string]string{}
		_ = filepath.WalkDir(memRoot, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(memRoot, p)
			b, _ := os.ReadFile(p)
			out[filepath.ToSlash(rel)] = string(b)
			return nil
		})
		return out
	}
	state := func() any {
		st := map[string]any{
			"agent":       getStr(bkState, "agent"),
			"active":      getStr(bkState, stActiveProjectKey),
			"compact":     ReadConfig()["COMPACT"],
			"mem_mode":    ReadConfig()["MEM_MODE"],
			"projects":    nil,
			"trackers":    map[string]any{},
			"tracker_raw": map[string]string{},
		}
		if b := getBytes(bkState, stProjectsKey); len(b) > 0 {
			var v any
			if json.Unmarshal(b, &v) == nil {
				st["projects"] = v
			}
		}
		for k, v := range allKV(bkTracker) {
			var x any
			if json.Unmarshal([]byte(v), &x) == nil {
				st["trackers"].(map[string]any)[k] = x
			} else {
				st["tracker_raw"].(map[string]string)[k] = v
			}
		}
		b, _ := json.Marshal(st)
		return mk.canon(t, b)
	}

	run := func(st parityMemStep) parityMemStep {
		for rel, content := range st.Files {
			p := filepath.Join(memRoot, filepath.FromSlash(rel))
			if content == "" {
				_ = os.RemoveAll(p)
				continue
			}
			if strings.HasSuffix(rel, "/") {
				_ = os.MkdirAll(p, 0o755)
				continue
			}
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		apply(st.Store)
		w := httptest.NewRecorder()
		r := httptest.NewRequest(st.Method, mk.unmask(st.Path), strings.NewReader(mk.unmask(st.Body)))
		switch r.URL.Path {
		case "/api/mem":
			handleMem(w, r)
		case "/api/mem/save":
			handleMemSave(w, r)
		case "/api/mem/delete":
			handleMemDelete(w, r)
		case "/api/mem/move":
			handleMemMove(w, r)
		case "/api/agent", "/api/tools", "/api/skills":
			handleAgent(w, r)
		case "/api/agent/toggle", "/api/tools/toggle", "/api/skills/toggle":
			handleAgentToggle(w, r)
		case "/api/agent/compact":
			handleCompactToggle(w, r)
		case "/api/memory":
			handleMemoryMode(w, r)
		case "/api/tracker":
			handleTracker(w, r)
		default:
			t.Fatalf("route inattendue %s", st.Path)
		}
		st.Status = w.Code
		st.Out = mk.canon(t, w.Body.Bytes())
		st.Disk = disk()
		st.State = state()
		return st
	}

	for _, st := range []parityMemStep{
		// --- pages ---
		{Method: "GET", Path: "/api/mem"},
		{Method: "GET", Path: "/api/mem?name=absente"},
		{Method: "GET", Path: "/api/agent"},
		{Method: "POST", Path: "/api/mem/save", Body: `{"name":"courses","content":"# Courses\n\nlait\n\n\n"}`},
		{Method: "POST", Path: "/api/mem/save", Body: `{"name":"notes.md","content":"Sans titre"}`},
		{Method: "POST", Path: "/api/mem/save", Body: `{"name":"../evil","content":"x"}`},
		{Method: "POST", Path: "/api/mem/save", Body: `{"name":"   ","content":"x"}`},
		{Method: "POST", Path: "/api/mem/save", Body: `pas du json`},
		{Method: "POST", Path: "/api/mem/save", Body: `{"name":5}`},
		{Method: "POST", Path: "/api/mem/save", Body: `[1]`},
		{Method: "POST", Path: "/api/mem/save", Body: ``},
		{Method: "POST", Path: "/api/mem/save", Body: `{"name":"courses-v2","old":"courses","content":"# Courses v2\nœufs & <pain>"}`},
		{Method: "POST", Path: "/api/mem/save", Body: `{"name":"notes","old":"notes","content":"## Titre\nmaj"}`},
		{Method: "GET", Path: "/api/mem?name=courses-v2"},
		{Method: "GET", Path: "/api/mem?name=+courses-v2.md+"},
		{Method: "GET", Path: "/api/mem?name=a&name=courses-v2"},
		{Method: "GET", Path: "/api/mem?name=../x"},
		{Files: map[string]string{"generale/Zeta.MD": "# Zêta\n", "generale/.cache.md": "caché", "generale/dossier.md/": "-",
			"generale/notes.txt": "pas une page", "generale/vide.md": "\n\n"},
			Method: "GET", Path: "/api/tools"},
		{Method: "POST", Path: "/api/mem/delete", Body: `{"name":"absente"}`},
		{Method: "POST", Path: "/api/mem/delete", Body: `{"name":"bad/name"}`},
		{Method: "POST", Path: "/api/mem/delete", Body: `{"name":"vide"}`},
		{Method: "POST", Path: "/api/mem/delete", Body: `{nope}`},
		// Accroche écrite à la main : un réenregistrement du même nom la perd
		// (ligne retirée puis rajoutée à la fin), comme le Go.
		{Files: map[string]string{"generale/MEMORY.md": "# Index mémoire — Générale\n\n- [Courses v2](courses-v2.md) — la liste\n- [Titre](notes.md)\n"},
			Method: "POST", Path: "/api/mem/save", Body: `{"name":"courses-v2","old":"courses-v2","content":"# Courses v3"}`},
		{Method: "POST", Path: "/api/mem/save", Body: `{"name":"neuve","content":"# Neuve"}`},
		// --- déplacement ---
		{Method: "POST", Path: "/api/mem/move", Body: `{"name":"notes","to":"perso"}`},
		{Method: "POST", Path: "/api/mem/move", Body: `{"name":"notes","to":"perso"}`},
		{Method: "POST", Path: "/api/mem/move", Body: `{"name":"courses-v2","to":"generale"}`},
		{Method: "POST", Path: "/api/mem/move", Body: `{"name":"courses-v2","to":" generale "}`},
		{Method: "POST", Path: "/api/mem/move", Body: `{"name":"courses-v2","to":"inconnu"}`},
		{Method: "POST", Path: "/api/mem/move", Body: `{"name":"MEMORY","to":"perso"}`},
		{Method: "POST", Path: "/api/mem/move", Body: `{"name":"x/y","to":"perso"}`},
		{Method: "POST", Path: "/api/mem/move", Body: `[]`},
		{Method: "GET", Path: "/api/mem/move"},
		{Files: map[string]string{"generale/doublon.md": "# A", "perso/doublon.md": "# B"},
			Method: "POST", Path: "/api/mem/move", Body: `{"name":"doublon","to":"perso"}`},
		// Bascule vers le projet perso : ses pages, son nom.
		{Store: map[string]string{"state/active_project": "perso"}, Method: "GET", Path: "/api/skills"},
		{Method: "POST", Path: "/api/mem/save", Body: `{"name":"perso-1","content":"Première\nligne"}`},
		{Method: "POST", Path: "/api/mem/move", Body: `{"name":"doublon","to":"generale"}`},
		{Store: map[string]string{"state/active_project": "generale"}, Method: "GET", Path: "/api/agent"},
		// --- mode agent, compactage ---
		{Method: "POST", Path: "/api/agent/toggle", Body: `{"on":true}`},
		{Method: "GET", Path: "/api/tools"},
		{Method: "POST", Path: "/api/skills/toggle", Body: `{"on":"oui"}`},
		{Method: "POST", Path: "/api/tools/toggle", Body: `{"on":true}`},
		{Method: "POST", Path: "/api/agent/toggle", Body: `pas du json`},
		{Method: "POST", Path: "/api/agent/compact", Body: `{"on":false}`},
		{Method: "GET", Path: "/api/agent"},
		{Method: "POST", Path: "/api/agent/compact", Body: `{}`},
		{Store: map[string]string{"config/COMPACT": " NON "}, Method: "GET", Path: "/api/agent"},
		{Method: "POST", Path: "/api/agent/compact", Body: `{"on":true}`},
		// --- mode mémoire ---
		{Method: "GET", Path: "/api/memory"},
		{Method: "POST", Path: "/api/memory", Body: `{"mode":" Search "}`},
		{Method: "GET", Path: "/api/agent"},
		{Method: "POST", Path: "/api/memory", Body: `pas du json`},
		{Method: "POST", Path: "/api/memory", Body: `{"mode":"off"}`},
		{Method: "PUT", Path: "/api/memory", Body: `{"mode":"ondemand"}`},
		{Store: map[string]string{"state/active_project": "fantome"}, Method: "POST", Path: "/api/memory", Body: `{"mode":"off"}`},
		{Method: "GET", Path: "/api/memory"},
		{Store: map[string]string{"state/active_project": "perso"}, Method: "POST", Path: "/api/memory", Body: `{"mode":"ondemand"}`},
		{Store: map[string]string{"state/active_project": "generale"}, Method: "GET", Path: "/api/memory"},
		// --- trackers ---
		{Method: "GET", Path: "/api/tracker"},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"add","name":"Poids","when":"2024-03-10","text":" 78,2 kg "}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"add","name":"Poids","when":"2024-03-09 08:15","text":"78,9"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"add","name":"poids","text":"77"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"ADD","name":"Réseau NAS","when":"2023","text":"installé <ok> & fini"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"add","name":"","text":"x"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"add","name":"x","text":"  "}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"add","name":"x","when":"demain","text":"y"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"add","name":"!!!","when":"2025-01","text":"z"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"add","name":"Même jour","when":"2025-01","text":"m"}`},
		{Method: "GET", Path: "/api/tracker?slug=poids"},
		{Method: "GET", Path: "/api/tracker?slug=absent"},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"edit","slug":"poids","id":"{{ID2}}","text":" 78,0 kg "}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"edit","slug":"poids","id":"{{ID2}}","when":"2024-03-11 07:00"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"edit","slug":"poids","id":"{{ID1}}","when":"hier","text":"perdu"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"edit","slug":"poids","id":"999","text":"x"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"edit","slug":"absent","id":"{{ID1}}","text":"x"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"delete","slug":"poids","id":"{{ID1}}"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"delete","slug":"poids","id":"{{ID1}}"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"rename","slug":"reseau-nas","name":" Réseau NAS (bureau) "}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"rename","slug":"poids","name":"POIDS"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"rename","slug":"tracker","name":"Poids"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"rename","slug":"tracker","name":"  "}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"rename","slug":"tracker","name":"???"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"rename","slug":"absent","name":"Neuf"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"move","slug":"tracker","to":"perso"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"move","slug":"poids","to":"inconnu"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"move","slug":"poids","to":"generale"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"move","slug":"absent","to":"perso"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"add","name":"Même jour","when":"2025-02","text":"perso"}`,
			Store: map[string]string{"state/active_project": "perso"}},
		{Store: map[string]string{"state/active_project": "generale"},
			Method: "POST", Path: "/api/tracker", Body: `{"action":"move","slug":"meme-jour","to":"perso"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"delete_tracker","slug":"poids"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"delete_tracker","slug":"absent"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"bidule"}`},
		{Method: "POST", Path: "/api/tracker", Body: `pas du json`},
		{Method: "POST", Path: "/api/tracker", Body: `"texte"`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"add","name":3}`},
		{Method: "PUT", Path: "/api/tracker"},
		{Store: map[string]string{"tracker/generale/casse": "pas du json"}, Method: "GET", Path: "/api/tracker"},
		{Method: "GET", Path: "/api/tracker?slug=casse"},
		{Store: map[string]string{"tracker/generale/vide": `{"name":"Vide","events":null}`}, Method: "GET", Path: "/api/tracker?slug=vide"},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"rename","slug":"vide","name":"VIDE"}`},
		{Method: "POST", Path: "/api/tracker", Body: `{"action":"delete","slug":"vide","id":"1"}`},
		{Method: "GET", Path: "/api/tracker"},
	} {
		fx.Steps = append(fx.Steps, run(st))
	}

	got, err := parityJSON(fx)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityMemFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityMemFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityMemFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("la mémoire ne produit plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteMemoire,\n"+
			"puis cargo test --test memory dit si le port Rust suit.\n--- obtenu ---\n%s", parityMemFile, got)
	}
}
