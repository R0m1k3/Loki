//go:build linux

package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// parity_mem_tools_test.go — journal de référence des outils mémoire de
// l'agent (lot 40 de la migration Rust) : mem_search, mem_read, mem_add,
// mem_edit et tracker. Chaque appel (arguments tels que le modèle les
// envoie), son résultat et son diff, puis les fichiers de la mémoire du projet
// et les trackers après coup. Heure fixée, fuseau UTC. Rangé dans
// rust/loki-rs/tests/parity_mem/outils.json ; src/memory/tools/tests.rs le
// rejoue.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteOutilsMemoire

const parityMemToolsFile = "../../rust/loki-rs/tests/parity_mem/outils.json"

type parityMemToolStep struct {
	Name     string            `json:"name"`
	Now      string            `json:"now,omitempty"`
	Tool     string            `json:"tool"`
	Args     json.RawMessage   `json:"args"`
	Label    string            `json:"label"`
	Out      string            `json:"out"`
	Diff     any               `json:"diff"`
	Added    int               `json:"added"`
	Removed  int               `json:"removed"`
	Files    map[string]string `json:"files"`
	Trackers map[string]any    `json:"trackers"`
}

var parityTrackerIDRe = regexp.MustCompile(`\b[0-9]{18,20}\b`)

func TestPariteOutilsMemoire(t *testing.T) {
	home := testHome(t)
	oldLocal, oldNow := time.Local, trackerNow
	time.Local = time.UTC
	var now time.Time
	trackerNow = func() time.Time { return now }
	t.Cleanup(func() { time.Local, trackerNow = oldLocal, oldNow })
	activeProjMu.Lock()
	activeProjCache = ""
	activeProjMu.Unlock()

	ids := map[string]string{}
	var order []string
	mask := func(s string) string {
		return parityTrackerIDRe.ReplaceAllStringFunc(s, func(id string) string {
			if _, ok := ids[id]; !ok {
				order = append(order, id)
				ids[id] = "{{T" + strconv.Itoa(len(order)) + "}}"
			}
			return ids[id]
		})
	}
	unmask := func(s string) string {
		for id, m := range ids {
			s = strings.ReplaceAll(s, m, id)
		}
		return s
	}
	dir := projectMemoryDir(defaultProjectSlug)
	files := func() map[string]string {
		out := map[string]string{}
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(dir, p)
			out[rel] = string(b)
			return nil
		})
		return out
	}
	trackers := func() map[string]any {
		out := map[string]any{}
		kv := allKV(bkTracker)
		keys := make([]string, 0, len(kv))
		for k := range kv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			var v any
			_ = json.Unmarshal([]byte(mask(string(kv[k]))), &v)
			out[k] = v
		}
		return out
	}
	var steps []parityMemToolStep
	run := func(st parityMemToolStep) {
		if st.Now != "" {
			n, err := time.Parse(time.RFC3339, st.Now)
			if err != nil {
				t.Fatal(err)
			}
			now = n
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(unmask(string(st.Args))), &args); err != nil {
			t.Fatalf("%s : %v", st.Name, err)
		}
		if args == nil {
			args = map[string]any{}
		}
		st.Label = toolCallLabel(st.Tool, args)
		res := execTool(context.Background(), st.Tool, args, st.Label, Caps{Agent: true})
		b, _ := json.Marshal(res.Result)
		var out string
		_ = json.Unmarshal(b, &out)
		st.Out = mask(out)
		db, _ := json.Marshal(res.Diff)
		_ = json.Unmarshal(db, &st.Diff)
		st.Added, st.Removed = res.Added, res.Removed
		st.Files = files()
		st.Trackers = trackers()
		steps = append(steps, st)
	}
	a := func(s string) json.RawMessage { return json.RawMessage(s) }

	// Pages.
	run(parityMemToolStep{Name: "recherche dans une mémoire vide", Now: "2026-07-20T10:00:00Z", Tool: "mem_search", Args: a(`{"query":"docker"}`)})
	run(parityMemToolStep{Name: "ajout", Tool: "mem_add", Args: a(`{"file":"docker-notes","content":"# Notes Docker\n\nUtiliser compose v2.\nLe port 8080 est pris.\n\n\n"}`)})
	run(parityMemToolStep{Name: "ajout d'une page existante", Tool: "mem_add", Args: a(`{"file":"docker-notes.md","content":"x"}`)})
	run(parityMemToolStep{Name: "ajout d'un nom invalide", Tool: "mem_add", Args: a(`{"file":"../x","content":"x"}`)})
	run(parityMemToolStep{Name: "ajout sans nom", Tool: "mem_add", Args: a(`{"content":"x"}`)})
	run(parityMemToolStep{Name: "ajout en majuscules", Tool: "mem_add", Args: a(`{"file":"Recettes.MD","content":"# Recettes\nTarte aux pommes\nLa copine de Nathan aime la tarte"}`)})
	run(parityMemToolStep{Name: "ajout d'une page bavarde", Tool: "mem_add", Args: a(`{"file":"nathan","content":"## Nathan\nNathan Nathan Nathan Nathan Nathan Nathan Nathan Nathan.\nNathan est né en 2019."}`)})
	long := strings.Repeat("Un texte de remplissage pour éloigner le terme. ", 4) + "Le mot RARE est là, " + strings.Repeat("puis encore du texte après lui, assez pour couper. ", 4)
	lb, _ := json.Marshal(map[string]string{"file": "longue", "content": "# Longue\n" + long})
	run(parityMemToolStep{Name: "ajout d'une longue page", Tool: "mem_add", Args: lb})
	run(parityMemToolStep{Name: "couverture d'abord", Tool: "mem_search", Args: a(`{"query":"copine Nathan"}`)})
	run(parityMemToolStep{Name: "rareté et nom", Tool: "mem_search", Args: a(`{"query":"Nathan nathan TARTE"}`)})
	run(parityMemToolStep{Name: "requête vide : tout", Tool: "mem_search", Args: a(`{"query":"  "}`)})
	run(parityMemToolStep{Name: "limite", Tool: "mem_search", Args: a(`{"query":"e","limit":2}`)})
	run(parityMemToolStep{Name: "limite hors bornes", Tool: "mem_search", Args: a(`{"query":"e","limit":99}`)})
	run(parityMemToolStep{Name: "extrait coupé des deux côtés", Tool: "mem_search", Args: a(`{"query":"rare"}`)})
	run(parityMemToolStep{Name: "rien", Tool: "mem_search", Args: a(`{"query":"zzz"}`)})
	run(parityMemToolStep{Name: "lecture", Tool: "mem_read", Args: a(`{"file":"docker-notes"}`)})
	run(parityMemToolStep{Name: "lecture d'une plage", Tool: "mem_read", Args: a(`{"file":"docker-notes.md","offset":3,"limit":1}`)})
	run(parityMemToolStep{Name: "lecture au-delà", Tool: "mem_read", Args: a(`{"file":"docker-notes","offset":40}`)})
	run(parityMemToolStep{Name: "lecture d'une page absente", Tool: "mem_read", Args: a(`{"file":"absente"}`)})
	run(parityMemToolStep{Name: "lecture d'un nom invalide", Tool: "mem_read", Args: a(`{"file":"a/b"}`)})
	run(parityMemToolStep{Name: "modification", Tool: "mem_edit", Args: a(`{"file":"docker-notes","old":"Le port 8080 est pris.","new":"Le port 8080 est pris.\nLe port 9090 aussi."}`)})
	run(parityMemToolStep{Name: "modification rejouée", Tool: "mem_edit", Args: a(`{"file":"docker-notes","old":"Le port 8080 est pris.\nLe port 9090 aussi.","new":"Le port 8080 est pris.\nLe port 9090 aussi."}`)})
	run(parityMemToolStep{Name: "modification déjà en place", Tool: "mem_edit", Args: a(`{"file":"docker-notes","old":"compose v1","new":"Utiliser compose v2."}`)})
	run(parityMemToolStep{Name: "ancien texte absent", Tool: "mem_edit", Args: a(`{"file":"docker-notes","old":"introuvable","new":"x"}`)})
	run(parityMemToolStep{Name: "ancien texte en double", Tool: "mem_edit", Args: a(`{"file":"nathan","old":"Nathan Nathan","new":"N"}`)})
	run(parityMemToolStep{Name: "ancien texte vide", Tool: "mem_edit", Args: a(`{"file":"nathan","old":"","new":"N"}`)})
	run(parityMemToolStep{Name: "modification d'une page absente", Tool: "mem_edit", Args: a(`{"file":"absente","old":"a","new":"b"}`)})
	run(parityMemToolStep{Name: "suppression d'un passage", Tool: "mem_edit", Args: a(`{"file":"recettes","old":"\nTarte aux pommes","new":""}`)})

	// Trackers.
	run(parityMemToolStep{Name: "aucun tracker", Tool: "tracker", Args: a(`{}`)})
	run(parityMemToolStep{Name: "ajout sans nom", Tool: "tracker", Args: a(`{"action":"add","text":"1"}`)})
	run(parityMemToolStep{Name: "ajout sans texte", Tool: "tracker", Args: a(`{"action":"add","name":"Poids","text":"  "}`)})
	run(parityMemToolStep{Name: "date illisible", Tool: "tracker", Args: a(`{"action":"add","name":"Poids","when":"hier","text":"72"}`)})
	run(parityMemToolStep{Name: "ajout daté", Tool: "tracker", Args: a(`{"action":"ADD","name":"Poids","when":"2026-07-15","text":" 72.5 "}`)})
	run(parityMemToolStep{Name: "ajout à l'heure", Tool: "tracker", Args: a(`{"action":"add","name":"poids","when":"2026-07-16T08:30","text":"72.1"}`)})
	run(parityMemToolStep{Name: "ajout l'an passé", Tool: "tracker", Args: a(`{"action":"add","name":"Poids","when":"2025-12","text":"73"}`)})
	run(parityMemToolStep{Name: "ajout maintenant", Now: "2026-07-20T10:15:00Z", Tool: "tracker", Args: a(`{"action":"add","name":"Poids","text":"71.9"}`)})
	run(parityMemToolStep{Name: "un autre tracker", Tool: "tracker", Args: a(`{"action":"add","name":"Séances de sport","when":"2024","text":"course"}`)})
	run(parityMemToolStep{Name: "un nom en nombre", Tool: "tracker", Args: a(`{"action":"add","name":42,"when":"2026-01-02 03:04:05","text":7}`)})
	run(parityMemToolStep{Name: "vue d'ensemble", Tool: "tracker", Args: a(`{"action":"view"}`)})
	run(parityMemToolStep{Name: "squelette", Tool: "tracker", Args: a(`{"name":"Poids"}`)})
	run(parityMemToolStep{Name: "une année", Tool: "tracker", Args: a(`{"name":"Poids","when":"2026"}`)})
	run(parityMemToolStep{Name: "une année vide", Tool: "tracker", Args: a(`{"name":"Poids","when":"2027"}`)})
	run(parityMemToolStep{Name: "un mois", Tool: "tracker", Args: a(`{"name":"Poids","when":"2026-07"}`)})
	run(parityMemToolStep{Name: "un jour à l'heure", Tool: "tracker", Args: a(`{"name":"Poids","when":"2026-07-16T08"}`)})
	run(parityMemToolStep{Name: "tracker inconnu", Tool: "tracker", Args: a(`{"name":"Tension"}`)})
	run(parityMemToolStep{Name: "modifier sans id", Tool: "tracker", Args: a(`{"action":"edit","name":"Poids","text":"1"}`)})
	run(parityMemToolStep{Name: "modifier", Tool: "tracker", Args: a(`{"action":"edit","name":"Poids","id":"{{T1}}","text":"72.4","when":"2026-07-14"}`)})
	run(parityMemToolStep{Name: "modifier une date illisible", Tool: "tracker", Args: a(`{"action":"edit","name":"Poids","id":"{{T1}}","when":"demain"}`)})
	run(parityMemToolStep{Name: "modifier un point inconnu", Tool: "tracker", Args: a(`{"action":"edit","name":"Poids","id":"1"}`)})
	run(parityMemToolStep{Name: "modifier un tracker inconnu", Tool: "tracker", Args: a(`{"action":"edit","name":"Tension","id":"1"}`)})
	run(parityMemToolStep{Name: "supprimer sans id", Tool: "tracker", Args: a(`{"action":"delete","name":"Poids"}`)})
	run(parityMemToolStep{Name: "supprimer sans nom", Tool: "tracker", Args: a(`{"action":"delete"}`)})
	run(parityMemToolStep{Name: "supprimer", Tool: "tracker", Args: a(`{"action":"delete","name":"Poids","id":"{{T2}}"}`)})
	run(parityMemToolStep{Name: "supprimer deux fois", Tool: "tracker", Args: a(`{"action":"delete","name":"Poids","id":"{{T2}}"}`)})
	run(parityMemToolStep{Name: "action inconnue", Tool: "tracker", Args: a(`{"action":"purge","name":"Poids"}`)})
	run(parityMemToolStep{Name: "après modifications", Tool: "tracker", Args: a(`{"name":"Poids","when":"2026"}`)})

	got, err := json.MarshalIndent(steps, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if strings.Contains(string(got), home) {
		t.Fatal("le journal contient le dossier du test")
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityMemToolsFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityMemToolsFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityMemToolsFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("les outils mémoire ne produisent plus le journal rangé dans %s.\n"+
			"LOKI_PARITY_UPDATE=1 pour le régénérer si le changement est voulu.", parityMemToolsFile)
	}
}
