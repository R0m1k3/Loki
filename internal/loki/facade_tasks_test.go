package loki

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Les tâches partent à la façade avec LOKI_TASKS=rust (lot 26) ; le reste,
// non. Le Go exécute pour elle une tâche IA (/internal/v1/tasks/run) et
// envoie ses notifications (/internal/v1/push/send).
func TestFacadeTaches(t *testing.T) {
	home := testHome(t)
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(internalTokenHeader) != jetonTest {
			w.WriteHeader(403)
			return
		}
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		io.WriteString(w, `{"ok":true,"de":"facade"}`)
	}))
	defer srv.Close()
	t.Setenv(internalTokenEnv, jetonTest)
	t.Setenv(facadeAddrEnv, strings.TrimPrefix(srv.URL, "http://"))
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }

	t.Setenv(facadeTasksEnv, "")
	if facadeTasks() {
		t.Fatal("tenues sans LOKI_TASKS")
	}
	for p := range facadeTasksRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if w.Body.String() != "go" {
			t.Fatalf("sans LOKI_TASKS, %s : %q", p, w.Body.String())
		}
	}
	t.Setenv(facadeTasksEnv, " RUST")
	if len(facadeTasksRoutes) != 7 {
		t.Fatalf("routes : %v", facadeTasksRoutes)
	}
	for p := range facadeTasksRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if !strings.Contains(w.Body.String(), "facade") {
			t.Fatalf("%s non relayé : %q", p, w.Body.String())
		}
	}
	mu.Lock()
	n := len(calls)
	mu.Unlock()
	if n != len(facadeTasksRoutes) {
		t.Fatalf("relais : %v", calls)
	}
	w := httptest.NewRecorder()
	withFacadeChat("/api/chat/stop", local)(w, httptest.NewRequest("POST", "/api/chat/stop", nil))
	if w.Body.String() != "go" {
		t.Fatalf("l'arrêt du chat reste au Go : %q", w.Body.String())
	}

	// /internal/v1/tasks/run : la tâche IA, exécutée comme par le
	// planificateur. Modèle pas prêt : elle ne tourne pas (busy).
	run := func(body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		handleInternalTaskRun(rec, httptest.NewRequest("POST", "/internal/v1/tasks/run", strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, _ := run(`{"id":"absente"}`); code != 404 {
		t.Fatalf("tâche absente : %d", code)
	}
	rec := httptest.NewRecorder()
	handleInternalTaskRun(rec, httptest.NewRequest("GET", "/internal/v1/tasks/run", nil))
	if rec.Code != 405 {
		t.Fatalf("GET : %d", rec.Code)
	}
	if err := saveTask(Task{ID: "ia", Name: "IA", Prompt: "x", Schedule: "@every 1h", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if code, out := run(`{"id":"ia"}`); code != 200 || out["busy"] != true {
		t.Fatalf("tâche IA sans modèle : %d %v", code, out)
	}
	if tk, _ := getTask("ia"); tk.LastRun != 0 {
		t.Fatalf("tâche IA sans modèle : passage enregistré %+v", tk)
	}
	// /internal/v1/push/send : sans abonné, rien ne part.
	rec = httptest.NewRecorder()
	handleInternalPushSend(rec, httptest.NewRequest("POST", "/internal/v1/push/send", strings.NewReader(`{"title":"t","body":"b"}`)))
	if rec.Code != 204 {
		t.Fatalf("push : %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	handleInternalPushSend(rec, httptest.NewRequest("POST", "/internal/v1/push/send", strings.NewReader(`{`)))
	if rec.Code != 400 {
		t.Fatalf("push illisible : %d", rec.Code)
	}

	t.Setenv(internalTokenEnv, "")
	if facadeTasks() {
		t.Fatal("tenues sans jeton interne")
	}

	// Une tâche script reçue tourne aussi (et n'est pas « occupée »).
	if runtime.GOOS != "windows" {
		if err := os.MkdirAll(filepath.Join(home, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "scripts", "ok.sh"), []byte("#!/bin/sh\necho bonjour\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := saveTask(Task{ID: "sc", Name: "Script", Kind: "script", Script: "ok.sh", Schedule: "@every 1h", Enabled: true}); err != nil {
			t.Fatal(err)
		}
		if code, out := run(`{"id":"sc"}`); code != 200 || out["busy"] != false {
			t.Fatalf("tâche script : %d %v", code, out)
		}
		if tk, _ := getTask("sc"); tk.LastRun == 0 || !tk.LastOK || !strings.Contains(tk.LastReport, "bonjour") {
			t.Fatalf("tâche script : %+v", tk)
		}
	}
}
