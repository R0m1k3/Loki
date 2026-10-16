//go:build linux

package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// parity_tasks_tools_test.go — journal de référence des outils task_* de
// l'agent (lot 37 de la migration Rust) : chaque appel (arguments tels que le
// modèle les envoie) et son résultat, avec les tâches en base après coup.
// L'heure des tâches est fixée pas à pas ; le fuseau de l'utilisateur, le
// projet actif, les projets connus et les scripts du dossier protégé sont
// posés avant l'appel. Rangé dans
// rust/loki-rs/tests/parity_tasks/tools.json ; tests/tasks.rs le rejoue par
// le port Rust.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteOutilsTaches

const parityTaskToolsFile = "../../rust/loki-rs/tests/parity_tasks/tools.json"

type parityTaskToolStep struct {
	Name    string            `json:"name"`
	Now     string            `json:"now,omitempty"` // RFC 3339 : l'heure des tâches
	State   map[string]string `json:"state,omitempty"`
	Files   map[string]string `json:"files,omitempty"`
	PutTask string            `json:"put_task,omitempty"` // une tâche écrite telle quelle en base
	Tool    string            `json:"tool"`
	Args    json.RawMessage   `json:"args"`
	Out     string            `json:"out"`
	Tasks   any               `json:"tasks"`
}

func TestPariteOutilsTaches(t *testing.T) {
	home := testHome(t)
	oldNow := tasksNow
	var now time.Time
	tasksNow = func() time.Time { return now }
	t.Cleanup(func() { tasksNow = oldNow })
	activeProjMu.Lock()
	activeProjCache = ""
	activeProjMu.Unlock()
	t.Cleanup(func() {
		activeProjMu.Lock()
		activeProjCache = ""
		activeProjMu.Unlock()
	})

	// Les identifiants tirés au hasard deviennent {{ID1}}, {{ID2}}… dans
	// l'ordre où ils apparaissent.
	ids := map[string]string{}
	var order []string
	mask := func(s string) string {
		return parityTaskIDRe.ReplaceAllStringFunc(s, func(id string) string {
			if _, ok := ids[id]; !ok {
				order = append(order, id)
				ids[id] = "{{ID" + strconv.Itoa(len(order)) + "}}"
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
	snapshot := func() any {
		b, _ := json.Marshal(listTasks())
		var v any
		_ = json.Unmarshal([]byte(mask(string(b))), &v)
		return v
	}

	var steps []parityTaskToolStep
	run := func(st parityTaskToolStep) {
		if st.Now != "" {
			n, err := time.Parse(time.RFC3339, st.Now)
			if err != nil {
				t.Fatal(err)
			}
			now = n
		}
		parityApplyFiles(t, home, st.Files)
		for k, v := range st.State {
			if err := putStr(bkState, k, v); err != nil {
				t.Fatal(err)
			}
			if k == stActiveProjectKey {
				activeProjMu.Lock()
				activeProjCache = v
				activeProjMu.Unlock()
			}
		}
		if st.PutTask != "" {
			raw := unmask(st.PutTask)
			var tk Task
			if err := json.Unmarshal([]byte(raw), &tk); err != nil {
				t.Fatal(err)
			}
			if err := putBytes(bkTasks, tk.ID, []byte(raw)); err != nil {
				t.Fatal(err)
			}
			mask(raw)
		}
		var args map[string]any
		dec := json.NewDecoder(bytes.NewReader([]byte(unmask(string(st.Args)))))
		if err := dec.Decode(&args); err != nil {
			t.Fatalf("%s : arguments illisibles : %v", st.Name, err)
		}
		if args == nil {
			args = map[string]any{}
		}
		res := execTool(context.Background(), st.Tool, args, toolCallLabel(st.Tool, args), Caps{Agent: true})
		// Le résultat tel que le modèle le reçoit : encodé en JSON (octets
		// orphelins d'une coupe → U+FFFD).
		b, _ := json.Marshal(res.Result)
		var out string
		_ = json.Unmarshal(b, &out)
		st.Out = mask(out)
		st.Tasks = snapshot()
		steps = append(steps, st)
	}
	args := func(s string) json.RawMessage { return json.RawMessage(s) }
	// Un compte-rendu dont la coupe à 300 octets tombe au milieu d'un « é ».
	longReport := strings.Repeat("a", 299) + "é et la suite\n  sur   plusieurs    lignes"

	run(parityTaskToolStep{Name: "liste vide", Now: "2026-03-28T12:00:00Z",
		State: map[string]string{stUserTZKey: "Europe/Paris"},
		Tool:  "task_list", Args: args(`{}`)})
	run(parityTaskToolStep{Name: "dans 30 minutes", Tool: "task_create",
		Args: args(`{"name":"Rappel","prompt":"Bois de l'eau","in_minutes":30}`)})
	run(parityTaskToolStep{Name: "dans 1,9 minute : une seule", Tool: "task_create",
		Args: args(`{"name":"Bref","prompt":"x","in_minutes":1.9}`)})
	run(parityTaskToolStep{Name: "à une heure passée : demain", Tool: "task_create",
		Args: args(`{"name":"Matin","prompt":"Café","at":"08:15"}`)})
	run(parityTaskToolStep{Name: "à une heure à venir : aujourd'hui", Tool: "task_create",
		Args: args(`{"name":"Soir","prompt":"Bilan","at":" 21:05 "}`)})
	run(parityTaskToolStep{Name: "à une date, dans le trou du changement d'heure", Tool: "task_create",
		Args: args(`{"name":"Nuit","prompt":"x","at":"2026-03-29 02:30"}`)})
	run(parityTaskToolStep{Name: "à une date passée", Tool: "task_create",
		Args: args(`{"name":"Hier","prompt":"x","at":"2026-03-27 10:00"}`)})
	run(parityTaskToolStep{Name: "at illisible", Tool: "task_create",
		Args: args(`{"name":"X","prompt":"x","at":"25:00"}`)})
	run(parityTaskToolStep{Name: "at : date impossible", Tool: "task_create",
		Args: args(`{"name":"X","prompt":"x","at":"2026-02-30 10:00"}`)})
	run(parityTaskToolStep{Name: "intervalle", Tool: "task_create",
		Args: args(`{"name":"Veille","prompt":"Regarde les nouvelles","schedule":"@every 2h"}`)})
	run(parityTaskToolStep{Name: "cron, désactivée", Tool: "task_create",
		Args: args(`{"name":"Semaine","prompt":"Point","schedule":"0 9 * * 1-5","enabled":false}`)})
	run(parityTaskToolStep{Name: "quotidienne ancrée", Tool: "task_create",
		Args: args(`{"name":"Journal","prompt":"Écris","schedule":"@every 1d@09:00","in_minutes":5}`)})
	run(parityTaskToolStep{Name: "fréquence invalide", Tool: "task_create",
		Args: args(`{"name":"X","prompt":"x","schedule":"@every 30s"}`)})
	run(parityTaskToolStep{Name: "sans nom", Tool: "task_create",
		Args: args(`{"prompt":"x","in_minutes":5}`)})
	run(parityTaskToolStep{Name: "nom d'un autre type", Tool: "task_create",
		Args: args(`{"name":42,"prompt":"x","in_minutes":5}`)})
	run(parityTaskToolStep{Name: "moins d'une minute : pas de fréquence", Tool: "task_create",
		Args: args(`{"name":"X","prompt":"x","in_minutes":0.5}`)})
	run(parityTaskToolStep{Name: "ni prompt ni script", Tool: "task_create",
		Args: args(`{"name":"X","in_minutes":5}`)})
	run(parityTaskToolStep{Name: "script absent", Tool: "task_create",
		Args: args(`{"name":"Ménage","script":"menage.sh","schedule":"@every 1d"}`)})
	run(parityTaskToolStep{Name: "script hostile", Tool: "task_create",
		Args: args(`{"name":"Ménage","script":"a;b.sh","schedule":"@every 1d"}`)})
	run(parityTaskToolStep{Name: "script dossier", Files: map[string]string{"scripts/outils/x": "#!/bin/sh\n"},
		Tool: "task_create", Args: args(`{"name":"Ménage","script":"outils","schedule":"@every 1d"}`)})
	run(parityTaskToolStep{Name: "script présent", Files: map[string]string{"scripts/menage.sh": "#!/bin/sh\necho ok\n"},
		Tool: "task_create", Args: args(`{"name":"Ménage","script":" menage.sh ","prompt":"ignoré ?","schedule":"@every 1d","enabled":"false"}`)})

	// Les projets : une tâche d'un autre projet est invisible ; un projet
	// disparu vaut le projet par défaut.
	projects := `[{"slug":"generale","name":"Générale","created_at":1},{"slug":"autre","name":"Autre","created_at":2}]`
	run(parityTaskToolStep{Name: "tâche d'un autre projet", State: map[string]string{stProjectsKey: projects},
		PutTask: `{"id":"aaaaaaaaaaaa","name":"Autre","prompt":"x","schedule":"@every 1h","tz":"Europe/Paris","preset":"","project":"autre","no_mem":false,"no_web":false,"enabled":true,"last_run":0,"next_run":1774699200000,"last_dur_ms":0,"last_ok":false,"last_error":"","last_report":""}`,
		Tool:    "task_list", Args: args(`{"id":"aaaaaaaaaaaa"}`)})
	run(parityTaskToolStep{Name: "tâche d'un projet disparu",
		PutTask: `{"id":"bbbbbbbbbbbb","name":"Antique (fantôme)","prompt":"x","schedule":"@every 1h","tz":"America/New_York","preset":"","project":"fantome","no_mem":false,"no_web":false,"enabled":true,"last_run":1774692000000,"next_run":1774699200000,"last_dur_ms":1200,"last_ok":false,"last_error":"moteur occupé","last_report":""}`,
		Tool:    "task_list", Args: args(`{"id":"bbbbbbbbbbbb"}`)})
	rb, _ := json.Marshal(longReport)
	report := string(rb)
	run(parityTaskToolStep{Name: "compte-rendu long",
		PutTask: `{"id":"cccccccccccc","name":"Rapport","prompt":"x","schedule":"@every 1d","tz":"Europe/Paris","preset":"","no_mem":false,"no_web":false,"enabled":false,"last_run":1774692000000,"next_run":1774699200000,"last_dur_ms":5000,"last_ok":true,"last_error":"","last_report":` + report + `}`,
		Tool:    "task_list", Args: args(`{}`)})
	run(parityTaskToolStep{Name: "compte-rendu complet", Tool: "task_list", Args: args(`{"id":" cccccccccccc "}`)})
	run(parityTaskToolStep{Name: "liste d'un autre projet", State: map[string]string{stActiveProjectKey: "autre"},
		Tool: "task_list", Args: args(`{}`)})
	run(parityTaskToolStep{Name: "création dans le projet actif", Tool: "task_create",
		Args: args(`{"name":"Ici","prompt":"x","schedule":"@every 3h"}`)})
	run(parityTaskToolStep{Name: "retour au projet par défaut", State: map[string]string{stActiveProjectKey: "generale"},
		Tool: "task_update", Args: args(`{"id":"aaaaaaaaaaaa","name":"Volée"}`)})

	// Les modifications.
	run(parityTaskToolStep{Name: "modifier sans id", Now: "2026-03-28T15:00:00Z", Tool: "task_update", Args: args(`{"name":"x"}`)})
	run(parityTaskToolStep{Name: "modifier un id inconnu", Tool: "task_update", Args: args(`{"id":"ffffffffffff"}`)})
	run(parityTaskToolStep{Name: "renommer, nouvelle fréquence", Tool: "task_update",
		Args: args(`{"id":"{{ID1}}","name":"  Eau  ","prompt":" ","schedule":" @every 45m "}`)})
	run(parityTaskToolStep{Name: "mettre en pause", Tool: "task_update", Args: args(`{"id":"{{ID1}}","enabled":false}`)})
	run(parityTaskToolStep{Name: "fréquence refusée", Tool: "task_update", Args: args(`{"id":"{{ID1}}","schedule":"jamais"}`)})
	run(parityTaskToolStep{Name: "reprendre une tâche sans prochaine exécution",
		PutTask: `{"id":"dddddddddddd","name":"Dormante","prompt":"x","schedule":"0 8 * * *","tz":"Europe/Paris","preset":"","no_mem":false,"no_web":false,"enabled":false,"last_run":0,"next_run":0,"last_dur_ms":0,"last_ok":false,"last_error":"","last_report":""}`,
		Tool:    "task_update", Args: args(`{"id":"dddddddddddd","enabled":true,"prompt":"Réveil"}`)})
	run(parityTaskToolStep{Name: "liste après modifications", Tool: "task_list", Args: args(`{}`)})

	// Les suppressions.
	run(parityTaskToolStep{Name: "supprimer sans id", Tool: "task_delete", Args: args(`{"id":"  "}`)})
	run(parityTaskToolStep{Name: "supprimer d'un autre projet", Tool: "task_delete", Args: args(`{"id":"aaaaaaaaaaaa"}`)})
	run(parityTaskToolStep{Name: "supprimer", Tool: "task_delete", Args: args(`{"id":"{{ID1}}"}`)})
	run(parityTaskToolStep{Name: "supprimer deux fois", Tool: "task_delete", Args: args(`{"id":"{{ID1}}"}`)})

	// Sans fuseau du navigateur : celui de la première tâche qui en a un.
	run(parityTaskToolStep{Name: "fuseau d'une tâche existante", State: map[string]string{stUserTZKey: ""},
		Tool: "task_create", Args: args(`{"name":"Ailleurs","prompt":"x","at":"23:30"}`)})

	got, err := json.MarshalIndent(steps, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityTaskToolsFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityTaskToolsFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityTaskToolsFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("les outils task_* ne produisent plus le journal rangé dans %s.\n"+
			"LOKI_PARITY_UPDATE=1 pour le régénérer si le changement est voulu.", parityTaskToolsFile)
	}
}
