//go:build linux

package loki

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// parity_tasks_test.go — journal de référence des tâches planifiées (lot 26
// de la migration Rust) : l'enregistrement (chaque forme de fréquence, ses
// erreurs, les fuseaux et les changements d'heure), la bascule, l'interrupteur
// maître, la suppression, le lancement d'une tâche script (succès, échec,
// arrêt) ou IA (moteur occupé, modèle pas prêt) et les tics du planificateur,
// l'heure des tâches fixée pas à pas. Rangé dans
// rust/loki-rs/tests/parity_tasks/tasks.json ; tests/tasks.rs le rejoue par la
// façade.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteTaches

const parityTasksFile = "../../rust/loki-rs/tests/parity_tasks/tasks.json"

type parityTasksStep struct {
	Name        string            `json:"name"`
	Now         string            `json:"now,omitempty"` // RFC 3339 : l'heure des tâches
	Files       map[string]string `json:"files,omitempty"`
	Config      map[string]string `json:"config,omitempty"`
	State       map[string]string `json:"state,omitempty"`
	Generating  bool              `json:"generating,omitempty"`
	Tick        bool              `json:"tick,omitempty"`     // un tic du planificateur, avant la requête
	Tune        bool              `json:"tune,omitempty"`     // une optimisation tient le moteur pendant l'étape
	PutTask     string            `json:"put_task,omitempty"` // une tâche écrite telle quelle en base
	Method      string            `json:"method,omitempty"`
	Path        string            `json:"path,omitempty"`
	Body        string            `json:"body,omitempty"`
	WaitRunning string            `json:"wait_running,omitempty"` // « {{IDn}} » : attendre qu'elle tourne
	WaitDone    string            `json:"wait_done,omitempty"`    // « {{IDn}} » : attendre la fin du passage
	Status      int               `json:"status,omitempty"`
	Out         any               `json:"out,omitempty"`
	Tasks       any               `json:"tasks"`
}

var (
	parityTaskDurRe     = regexp.MustCompile(`exit: (-?[0-9]+) · [^\n"\\]*`)
	parityTaskIDRe      = regexp.MustCompile(`\b[0-9a-f]{12}\b`)
	parityTaskCreatedRe = regexp.MustCompile(`"created":[0-9]+`)
)

func TestPariteTaches(t *testing.T) {
	home := testHome(t)
	oldNow := tasksNow
	var now time.Time
	tasksNow = func() time.Time { return now }
	t.Cleanup(func() { tasksNow = oldNow })

	// Les identifiants tirés au hasard deviennent {{ID1}}, {{ID2}}… dans
	// l'ordre où ils apparaissent.
	ids := map[string]string{}
	var order []string
	mask := func(s string) string {
		s = parityTaskIDRe.ReplaceAllStringFunc(s, func(id string) string {
			if _, ok := ids[id]; !ok {
				order = append(order, id)
				ids[id] = "{{ID" + strconv.Itoa(len(order)) + "}}"
			}
			return ids[id]
		})
		s = strings.ReplaceAll(s, home, "{{HOME}}")
		// La date du projet par défaut, posée à la lecture (heure réelle).
		s = parityTaskCreatedRe.ReplaceAllString(s, `"created":"{{T}}"`)
		return parityTaskDurRe.ReplaceAllString(s, "exit: $1 · {{DUR}}")
	}
	unmask := func(s string) string {
		for id, m := range ids {
			s = strings.ReplaceAll(s, m, id)
		}
		return strings.ReplaceAll(s, "{{HOME}}", home)
	}
	handlers := map[string]http.HandlerFunc{
		"/api/tasks":        handleTasks,
		"/api/tasks/save":   handleTaskSave,
		"/api/tasks/delete": handleTaskDelete,
		"/api/tasks/toggle": handleTaskToggle,
		"/api/tasks/pause":  handleTasksPause,
		"/api/tasks/run":    handleTaskRun,
		"/api/tasks/stop":   handleTaskStop,
	}
	call := func(method, path, body string) (int, any) {
		rec := httptest.NewRecorder()
		handlers[path](rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		var out any
		if err := json.Unmarshal([]byte(mask(rec.Body.String())), &out); err != nil {
			t.Fatalf("%s : corps illisible %q", path, rec.Body.String())
		}
		return rec.Code, out
	}
	snapshot := func() any {
		b, _ := json.Marshal(listTasks())
		var v any
		_ = json.Unmarshal([]byte(mask(string(b))), &v)
		return v
	}
	lastRun := func(id string) int64 {
		tk, _ := getTask(id)
		return tk.LastRun
	}

	var steps []parityTasksStep
	run := func(st parityTasksStep) {
		if st.Now != "" {
			n, err := time.Parse(time.RFC3339, st.Now)
			if err != nil {
				t.Fatal(err)
			}
			now = n
		}
		files := map[string]string{}
		for k, v := range st.Files {
			files[k] = unmask(v)
		}
		parityApplyFiles(t, home, files)
		for k, v := range st.Config {
			if err := SetConfigKey(k, v); err != nil {
				t.Fatal(err)
			}
		}
		for k, v := range st.State {
			if err := putStr(bkState, k, v); err != nil {
				t.Fatal(err)
			}
		}
		if st.PutTask != "" {
			var tk Task
			if err := json.Unmarshal([]byte(st.PutTask), &tk); err != nil {
				t.Fatal(err)
			}
			if err := putBytes(bkTasks, tk.ID, []byte(st.PutTask)); err != nil {
				t.Fatal(err)
			}
		}
		if st.Generating {
			conv.mu.Lock()
			conv.Generating = true
			conv.mu.Unlock()
		}
		var lock *tuneLock
		if st.Tune {
			l, err := tuneLockAcquire("web", false)
			if err != nil {
				t.Fatal(err)
			}
			lock = l
		}
		if st.Tick {
			tickTasks(tasksNow())
		}
		var before int64
		if st.WaitDone != "" {
			before = lastRun(unmask(st.WaitDone))
		}
		if st.Path != "" {
			st.Status, st.Out = call(st.Method, st.Path, unmask(st.Body))
		}
		if lock != nil {
			lock.release()
		}
		if st.Generating {
			conv.mu.Lock()
			conv.Generating = false
			conv.mu.Unlock()
		}
		if st.WaitRunning != "" {
			deadline := time.Now().Add(10 * time.Second)
			for {
				_, out := call("GET", "/api/tasks", "")
				if out.(map[string]any)["running_id"] == st.WaitRunning {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s : la tâche ne tourne pas", st.Name)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		if st.WaitDone != "" {
			id := unmask(st.WaitDone)
			deadline := time.Now().Add(20 * time.Second)
			for lastRun(id) == before {
				if time.Now().After(deadline) {
					t.Fatalf("%s : le passage ne finit pas", st.Name)
				}
				time.Sleep(10 * time.Millisecond)
			}
			// Le registre des scripts est vidé juste après l'écriture.
			for {
				if sid, _ := scriptRunningAny(); sid == "" {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		st.Tasks = snapshot()
		steps = append(steps, st)
	}

	save := func(name, extra string) string {
		return `{"name":"` + name + `","prompt":"Fais le point.","enabled":true` + extra + `}`
	}
	for _, st := range []parityTasksStep{
		{Name: "liste vide", Now: "2026-03-28T12:00:00Z", Files: map[string]string{
			"scripts/ok.sh":        "#!/bin/sh\necho bonjour\n",
			"scripts/echec.sh":     "#!/bin/sh\necho oups >&2\nexit 2\n",
			"scripts/lent.sh":      "#!/bin/sh\nsleep 30\n",
			"scripts/sous/rang.sh": "#!/bin/sh\necho rangé\n",
		}, Method: "GET", Path: "/api/tasks"},
		{Name: "agent actif, interrupteur", State: map[string]string{"agent": "1"}, Method: "GET", Path: "/api/tasks"},
		{Name: "mémoire coupée, internet armé", Config: map[string]string{"MEM_MODE": "off"}, State: map[string]string{"internet": "1"}, Method: "GET", Path: "/api/tasks"},

		// Enregistrement : erreurs.
		{Name: "corps illisible", Method: "POST", Path: "/api/tasks/save", Body: `{"enabled":"oui"}`},
		{Name: "sans nom", Method: "POST", Path: "/api/tasks/save", Body: `{"name":"  ","prompt":"x","schedule":"@every 1h"}`},
		{Name: "sans consigne", Method: "POST", Path: "/api/tasks/save", Body: `{"name":"x","schedule":"@every 1h"}`},
		{Name: "script sans nom de script", Method: "POST", Path: "/api/tasks/save", Body: `{"name":"x","kind":"script","schedule":"@every 1h"}`},
		{Name: "script absent", Method: "POST", Path: "/api/tasks/save", Body: `{"name":"x","kind":"script","script":"absent.sh","schedule":"@every 1h"}`},
		{Name: "script : dossier", Method: "POST", Path: "/api/tasks/save", Body: `{"name":"x","kind":"script","script":"sous","schedule":"@every 1h"}`},
		{Name: "script : remontée", Method: "POST", Path: "/api/tasks/save", Body: `{"name":"x","kind":"script","script":"../x.sh","schedule":"@every 1h"}`},
		{Name: "script : chemin absolu", Method: "POST", Path: "/api/tasks/save", Body: `{"name":"x","kind":"script","script":"/etc/x","schedule":"@every 1h"}`},
		{Name: "script : séparateur Windows", Method: "POST", Path: "/api/tasks/save", Body: `{"name":"x","kind":"script","script":"\\x.sh","schedule":"@every 1h"}`},
		{Name: "script : caractère interdit", Method: "POST", Path: "/api/tasks/save", Body: `{"name":"x","kind":"script","script":"a;b","schedule":"@every 1h"}`},
		{Name: "fréquence vide", Method: "POST", Path: "/api/tasks/save", Body: save("x", ``)},
		{Name: "intervalle trop court", Method: "POST", Path: "/api/tasks/save", Body: save("x", `,"schedule":"@every 30s"`)},
		{Name: "intervalle illisible", Method: "POST", Path: "/api/tasks/save", Body: save("x", `,"schedule":"@every deux heures"`)},
		{Name: "intervalle sans unité", Method: "POST", Path: "/api/tasks/save", Body: save("x", `,"schedule":"@every 5"`)},
		{Name: "jours invalides", Method: "POST", Path: "/api/tasks/save", Body: save("x", `,"schedule":"@every 0d"`)},
		{Name: "heure hors bornes", Method: "POST", Path: "/api/tasks/save", Body: save("x", `,"schedule":"@every 1d@24:00"`)},
		{Name: "heure sans deux-points", Method: "POST", Path: "/api/tasks/save", Body: save("x", `,"schedule":"@every 1d@9h"`)},
		{Name: "cron à 4 champs", Method: "POST", Path: "/api/tasks/save", Body: save("x", `,"schedule":"0 9 * *"`)},
		{Name: "cron : minute hors bornes", Method: "POST", Path: "/api/tasks/save", Body: save("x", `,"schedule":"61 9 * * *"`)},
		{Name: "cron : jour en lettres", Method: "POST", Path: "/api/tasks/save", Body: save("x", `,"schedule":"0 9 * * sun"`)},
		{Name: "cron : plage inversée", Method: "POST", Path: "/api/tasks/save", Body: save("x", `,"schedule":"0 9 5-1 * *"`)},
		{Name: "cron : pas nul", Method: "POST", Path: "/api/tasks/save", Body: save("x", `,"schedule":"*/0 * * * *"`)},
		{Name: "cron jamais satisfait", Method: "POST", Path: "/api/tasks/save", Body: save("x", `,"schedule":"0 0 30 2 *"`)},
		{Name: "une fois, date illisible", Method: "POST", Path: "/api/tasks/save", Body: save("x", `,"schedule":"@once 2026-04-31 10:00"`)},
		{Name: "une fois, déjà passé", Method: "POST", Path: "/api/tasks/save", Body: save("x", `,"schedule":"@once 2026-03-28 13:00","tz":"Europe/Paris"`)},

		// Enregistrement : chaque forme.
		{Name: "toutes les 2 h", Method: "POST", Path: "/api/tasks/save", Body: save("B toutes les 2 h", `,"schedule":"@every 2h"`)},
		{Name: "toutes les 1 h 30", Method: "POST", Path: "/api/tasks/save", Body: save("C une heure et demie", `,"schedule":"@EVERY 1.5h","enabled":false`)},
		{Name: "tous les jours à 23:00, Paris", Method: "POST", Path: "/api/tasks/save", Body: save("D chaque soir", `,"schedule":"@every 1d@23:00","tz":"Europe/Paris"`)},
		{Name: "tous les 3 jours à 9:30, New York", Method: "POST", Path: "/api/tasks/save", Body: save("E tous les trois jours", `,"schedule":"@every 3d@9:30","tz":"America/New_York"`)},
		{Name: "en semaine à 9 h, Calcutta", Method: "POST", Path: "/api/tasks/save", Body: save("F semaine", `,"schedule":"0 9 * * 1-5","tz":"Asia/Kolkata"`)},
		{Name: "chaque nuit à 2:30, Paris (heure qui saute)", Method: "POST", Path: "/api/tasks/save", Body: save("G nuit", `,"schedule":"30 2 * * *","tz":"Europe/Paris"`)},
		{Name: "toutes les 15 min, fuseau inconnu", Method: "POST", Path: "/api/tasks/save", Body: save("H quart d'heure", `,"schedule":"*/15 * * * *","tz":"Nulle/Part"`)},
		{Name: "le dimanche (7)", Method: "POST", Path: "/api/tasks/save", Body: save("I dimanche", `,"schedule":"0 8 * * 7"`)},
		{Name: "une seule fois", Method: "POST", Path: "/api/tasks/save", Body: save("J une fois", `,"schedule":"@once 2026-03-29 03:30","tz":"Europe/Paris"`)},
		{Name: "tâche script", Method: "POST", Path: "/api/tasks/save", Body: `{"name":"A script","kind":"script","script":" ok.sh ","schedule":"@every 1h","enabled":true}`},
		{Name: "tâche script en échec", Method: "POST", Path: "/api/tasks/save", Body: `{"name":"K échec","kind":"script","script":"echec.sh","schedule":"@every 1h","enabled":true}`},
		{Name: "tâche script lente", Method: "POST", Path: "/api/tasks/save", Body: `{"name":"L lente","kind":"script","script":"lent.sh","schedule":"@every 1h"}`},
		{Name: "script une seule fois", Method: "POST", Path: "/api/tasks/save",
			Body: `{"name":"N script une fois","kind":"script","script":"ok.sh","schedule":"@once 2026-03-29 03:30","tz":"Europe/Paris","enabled":true}`},
		{Name: "tâche avec id inconnu", Method: "POST", Path: "/api/tasks/save", Body: `{"id":"zz","name":"M imposée","prompt":"x","schedule":"@every 1h","project":"inconnu","no_mem":true,"no_web":true}`},
		{Name: "liste", Method: "GET", Path: "/api/tasks"},

		// Modification : la fréquence inchangée garde le prochain passage.
		{Name: "renommer sans changer la fréquence", Now: "2026-03-28T12:30:00Z", Method: "POST", Path: "/api/tasks/save",
			Body: `{"id":"{{ID1}}","name":"B renommée","prompt":"Autre.","schedule":"@every 2h","enabled":true}`},
		{Name: "changer la fréquence", Method: "POST", Path: "/api/tasks/save",
			Body: `{"id":"{{ID1}}","name":"B renommée","prompt":"Autre.","schedule":"@every 3h","enabled":true}`},

		// Bascule, interrupteur maître, suppression.
		{Name: "bascule d'une tâche absente", Method: "POST", Path: "/api/tasks/toggle", Body: `{"id":"absente","on":true}`},
		{Name: "réactiver", Now: "2026-03-28T13:00:00Z", Method: "POST", Path: "/api/tasks/toggle", Body: `{"id":"{{ID2}}","on":true}`},
		{Name: "désactiver", Method: "POST", Path: "/api/tasks/toggle", Body: `{"id":"{{ID2}}","on":false}`},
		{Name: "suspendre tout", Method: "POST", Path: "/api/tasks/pause", Body: `{"on":true}`},
		{Name: "liste suspendue", Method: "GET", Path: "/api/tasks"},
		{Name: "tic suspendu", Now: "2026-03-28T23:00:00Z", Tick: true},
		{Name: "reprendre", Method: "POST", Path: "/api/tasks/pause", Body: `{"on":false}`},
		{Name: "interrupteur rangé à 0 : pas suspendu", State: map[string]string{"tasks_paused": "0"}, Method: "GET", Path: "/api/tasks"},
		{Name: "suppression sans id", Method: "POST", Path: "/api/tasks/delete", Body: `{}`},
		{Name: "suppression", Method: "POST", Path: "/api/tasks/delete", Body: `{"id":"zz"}`},

		// Lancements.
		{Name: "lancer une tâche absente", Method: "POST", Path: "/api/tasks/run", Body: `{"id":"absente"}`},
		{Name: "lancer une tâche IA, moteur occupé", Generating: true, Method: "POST", Path: "/api/tasks/run", Body: `{"id":"{{ID1}}"}`},
		{Name: "lancer une tâche IA, modèle pas prêt", Method: "POST", Path: "/api/tasks/run", Body: `{"id":"{{ID1}}"}`},
		{Name: "lancer le script", Method: "POST", Path: "/api/tasks/run", Body: `{"id":"{{ID10}}"}`, WaitDone: "{{ID10}}"},
		{Name: "lancer le script en échec", Method: "POST", Path: "/api/tasks/run", Body: `{"id":"{{ID11}}"}`, WaitDone: "{{ID11}}"},
		{Name: "lancer le script lent", Method: "POST", Path: "/api/tasks/run", Body: `{"id":"{{ID12}}"}`, WaitRunning: "{{ID12}}"},
		{Name: "liste pendant le script", Method: "GET", Path: "/api/tasks"},
		{Name: "arrêter le script", Method: "POST", Path: "/api/tasks/stop", Body: `{"id":"{{ID12}}"}`, WaitDone: "{{ID12}}"},
		{Name: "arrêter sans tâche script", Method: "POST", Path: "/api/tasks/stop", Body: `{}`},
		{Name: "le script qui réussissait échoue", Now: "2026-03-28T23:05:00Z", Files: map[string]string{"scripts/ok.sh": "#!/bin/sh\necho perdu\nexit 3\n"},
			Method: "POST", Path: "/api/tasks/run", Body: `{"id":"{{ID10}}"}`, WaitDone: "{{ID10}}"},
		{Name: "tâche script sans script", Now: "2026-03-28T23:01:00Z", PutTask: `{"id":"vide","name":"S vide","kind":"script","schedule":"@every 1h"}`,
			Method: "POST", Path: "/api/tasks/run", Body: `{"id":"vide"}`, WaitDone: "vide"},
		{Name: "tâche script au nom refusé", PutTask: `{"id":"remonte","name":"S remonte","kind":"script","script":"../x.sh","schedule":"@every 1h"}`,
			Method: "POST", Path: "/api/tasks/run", Body: `{"id":"remonte"}`, WaitDone: "remonte"},
		{Name: "tâche script au script disparu", PutTask: `{"id":"disparu","name":"S disparu","kind":"script","script":"parti.sh","schedule":"@every 1h"}`,
			Method: "POST", Path: "/api/tasks/run", Body: `{"id":"disparu"}`, WaitDone: "disparu"},
		{Name: "puis réussit de nouveau", Now: "2026-03-28T23:10:00Z", Files: map[string]string{"scripts/ok.sh": "#!/bin/sh\necho bonjour\n"},
			Method: "POST", Path: "/api/tasks/run", Body: `{"id":"{{ID10}}"}`, WaitDone: "{{ID10}}"},

		// Planificateur : les tâches IA (jamais exécutées sans modèle, elles
		// bloqueraient la file) d'abord désactivées.
		{Name: "tic : une tâche IA due attend le modèle", Now: "2026-03-28T16:00:00Z", Tick: true},
	} {
		run(st)
	}
	for i := 1; i <= 9; i++ {
		run(parityTasksStep{Name: "désactiver la tâche IA " + strconv.Itoa(i), Method: "POST", Path: "/api/tasks/toggle",
			Body: `{"id":"{{ID` + strconv.Itoa(i) + `}}","on":false}`})
	}
	for _, st := range []parityTasksStep{
		{Name: "tic pendant une optimisation : rien ne part", Now: "2026-03-29T00:30:00Z", Tune: true, Tick: true},
		{Name: "tic : la première tâche due tourne", Now: "2026-03-29T00:30:00Z", Tick: true},
		{Name: "tic : la suivante", Tick: true},
		{Name: "tic : rien de dû", Tick: true},
		{Name: "tic : de nouveau dû", Now: "2026-03-29T01:31:00Z", Tick: true},
		{Name: "tic : la suivante, encore", Tick: true},
		{Name: "tic : la tâche une fois tourne puis s'éteint", Tick: true},
		{Name: "tic : plus rien", Tick: true},
		{Name: "tâche sans prochain passage", Now: "2026-03-29T02:00:00Z",
			PutTask: `{"id":"manuelle","name":"O manuelle","kind":"script","script":"ok.sh","schedule":"0 4 * * *","tz":"UTC","enabled":true}`, Tick: true},
		{Name: "tâche à la fréquence devenue invalide", PutTask: `{"id":"cassee","name":"P cassée","kind":"script","script":"ok.sh","schedule":"@every 10s","enabled":true}`, Tick: true},
		{Name: "même nom, ids dans l'ordre", PutTask: `{"id":"r2","name":"R double","kind":"script","script":"ok.sh","schedule":"@every 1h"}`},
		{Name: "même nom, second", PutTask: `{"id":"r1","name":"R double","kind":"script","script":"ok.sh","schedule":"@every 1h"}`},
		{Name: "liste finale", Method: "GET", Path: "/api/tasks"},
	} {
		run(st)
	}

	got, err := parityJSON(map[string]any{"steps": steps})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll("../../rust/loki-rs/tests/parity_tasks", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityTasksFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityTasksFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("les tâches ne produisent plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteTaches,\n"+
			"puis cargo test --test tasks dit si le port Rust suit.\n--- obtenu ---\n%s", parityTasksFile, got)
	}
}
