//go:build linux

package loki

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// parity_jobs_test.go — journal de référence des jobs d'arrière-plan du mode
// Code (lot 43 de la migration Rust) : bash_bg et bash_tail, de la commande
// refusée au job tué. Pid et durées sont masqués ; l'état relu juste après
// un arrêt aussi (le Go relit sans attendre la fin). Rangé dans
// rust/loki-rs/tests/parity_jobs/jobs.json ; tests/jobs.rs le rejoue contre
// le port.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteJobs

const parityJobsFile = "../../rust/loki-rs/tests/parity_jobs/jobs.json"

type parityJobStep struct {
	Name   string         `json:"name"`
	Tool   string         `json:"tool"`
	Args   map[string]any `json:"args"`
	Wait   string         `json:"wait,omitempty"` // job dont attendre la fin avant l'appel
	After  string         `json:"after,omitempty"`
	Result string         `json:"result"`
	Logs   []string       `json:"logs"` // journaux présents dans .loki/jobs
}

var (
	parityJobsPid   = regexp.MustCompile(`\(pid [0-9]+\)`)
	parityJobsSince = regexp.MustCompile(`depuis [0-9hms.]+`)
	parityJobsState = regexp.MustCompile(`— (en cours|terminé \(exit -1\)) —`)
)

func TestPariteJobs(t *testing.T) {
	root := testHome(t)
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	setTaskWorkspace(ws)
	t.Cleanup(func() { setTaskWorkspace("") })
	jobsMu.Lock()
	jobs, jobSeq = map[string]*bgJob{}, 0
	jobsMu.Unlock()
	t.Cleanup(func() {
		for _, j := range jobs {
			if j.Cmd.Process != nil {
				_ = j.Cmd.Process.Kill()
			}
		}
	})

	wait := func(id string) {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			jobsMu.Lock()
			j := jobs[id]
			jobsMu.Unlock()
			if j != nil {
				j.mu.Lock()
				done := j.done
				j.mu.Unlock()
				if done {
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("job %s : pas fini", id)
	}
	var steps []parityJobStep
	run := func(st parityJobStep) {
		if st.Wait != "" {
			wait(st.Wait)
		}
		args := map[string]any{}
		b, _ := json.Marshal(st.Args)
		_ = json.Unmarshal(b, &args)
		res := execTool(t.Context(), st.Tool, args, toolCallLabel(st.Tool, args), Caps{Agent: true, Code: true})
		r := parityJobsPid.ReplaceAllString(res.Result, "(pid {{PID}})")
		r = parityJobsSince.ReplaceAllString(r, "depuis {{D}}")
		if st.After == "kill" {
			r = parityJobsState.ReplaceAllString(r, "— {{ETAT}} —")
		}
		st.Result = r
		st.Logs = []string{}
		entries, _ := os.ReadDir(filepath.Join(ws, ".loki", "jobs"))
		for _, e := range entries {
			st.Logs = append(st.Logs, e.Name())
		}
		if st.Args == nil {
			st.Args = map[string]any{}
		}
		steps = append(steps, st)
	}
	a := func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}

	run(parityJobStep{Name: "commande vide", Tool: "bash_bg", Args: a("command", "   ")})
	run(parityJobStep{Name: "commande refusée", Tool: "bash_bg", Args: a("command", "rm -rf /")})
	run(parityJobStep{Name: "lancement", Tool: "bash_bg", Args: a("command", "echo un; echo deux >&2; printf 'trois\\n\\n'")})
	run(parityJobStep{Name: "sortie", Wait: "job1", Tool: "bash_tail", Args: a("id", "job1")})
	run(parityJobStep{Name: "deux lignes", Tool: "bash_tail", Args: a("id", "job1", "lines", 2.0)})
	run(parityJobStep{Name: "lignes nulles", Tool: "bash_tail", Args: a("id", "job1", "lines", 0.0)})
	run(parityJobStep{Name: "lignes en texte", Tool: "bash_tail", Args: a("id", "job1", "lines", "1")})
	run(parityJobStep{Name: "lignes tronquées", Tool: "bash_tail", Args: a("id", "job1", "lines", 1.9)})
	run(parityJobStep{Name: "arrêt d'un job fini", Tool: "bash_tail", Args: a("id", "job1", "kill", true)})
	run(parityJobStep{Name: "job inconnu", Tool: "bash_tail", Args: a("id", "job9")})
	run(parityJobStep{Name: "sans id", Tool: "bash_tail", Args: a()})
	run(parityJobStep{Name: "sortie en erreur", Tool: "bash_bg", Args: a("command", "exit 3")})
	run(parityJobStep{Name: "rien d'écrit", Wait: "job2", Tool: "bash_tail", Args: a("id", "job2")})
	run(parityJobStep{Name: "job long", Tool: "bash_bg", Args: a("command", "sleep 30")})
	run(parityJobStep{Name: "en cours", Tool: "bash_tail", Args: a("id", "job3")})
	run(parityJobStep{Name: "arrêt", Tool: "bash_tail", Args: a("id", "job3", "kill", true), After: "kill"})
	run(parityJobStep{Name: "après l'arrêt", Wait: "job3", Tool: "bash_tail", Args: a("id", "job3")})
	run(parityJobStep{Name: "beaucoup de lignes", Tool: "bash_bg", Args: a("command", "for i in $(seq 1 300); do echo ligne $i; done")})
	run(parityJobStep{Name: "quarante par défaut", Wait: "job4", Tool: "bash_tail", Args: a("id", "job4")})
	run(parityJobStep{Name: "deux cents au plus", Tool: "bash_tail", Args: a("id", "job4", "lines", 500.0)})
	run(parityJobStep{Name: "sortie démesurée", Tool: "bash_bg", Args: a("command", "for i in $(seq 1 9000); do printf 'é'; done; echo")})
	run(parityJobStep{Name: "fin gardée", Wait: "job5", Tool: "bash_tail", Args: a("id", "job5")})
	run(parityJobStep{Name: "octets invalides", Tool: "bash_bg", Args: a("command", "printf 'a\\377b\\n'")})
	run(parityJobStep{Name: "octets relus", Wait: "job6", Tool: "bash_tail", Args: a("id", "job6")})

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{"steps": steps}); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityJobsFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityJobsFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityJobsFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("les jobs d'arrière-plan ne produisent plus le journal rangé dans %s.\n"+
			"LOKI_PARITY_UPDATE=1 pour le régénérer si le changement est voulu.", parityJobsFile)
	}
}
