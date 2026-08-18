package loki

// code_jobs.go — commandes shell d'ARRIÈRE-PLAN pour le mode code : bash_bg
// lance un process détaché (serveur de dev, watcher, build long) et rend la
// main tout de suite ; bash_tail relit sa sortie et son état. Sans ça, le
// modèle lance `./serveur &` dans bash et le tour reste suspendu aux tubes
// (voir le commentaire WaitDelay de runShell).
//
// La sortie va dans un fichier du dossier de la discussion
// (.loki/jobs/<id>.log) : elle survit au tour, et le panneau Fichiers la montre.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type bgJob struct {
	ID      string
	Command string
	LogPath string
	Cmd     *exec.Cmd
	Started time.Time

	mu   sync.Mutex
	done bool
	exit int
}

var (
	jobsMu sync.Mutex
	jobs   = map[string]*bgJob{}
	jobSeq int
)

func bashBgTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "bash_bg",
			Description: "Start a shell command in the BACKGROUND (dev server, watcher, long build) and return immediately with a job id. Output goes to a log file; check it with bash_tail. Use this instead of appending '&' in bash.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string", "description": "The command"},
				},
				"required": []string{"command"},
			},
		},
	}
}

func bashTailTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "bash_tail",
			Description: "Read the last lines of a background job's output and its status (running/exited). kill:true stops the job.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":    map[string]any{"type": "string", "description": "Job id from bash_bg"},
					"lines": map[string]any{"type": "integer", "description": "Lines to show (default 40, max 200)"},
					"kill":  map[string]any{"type": "boolean", "description": "Stop the job"},
				},
				"required": []string{"id"},
			},
		},
	}
}

// toolBashBg lance la commande détachée. Même politique que bash (commandes
// dangereuses refusées), même dossier de départ (discussion courante).
func toolBashBg(args map[string]any) string {
	command, _ := args["command"].(string)
	if strings.TrimSpace(command) == "" {
		return "[erreur] commande vide"
	}
	if reason := dangerousCommand(command); reason != "" {
		return refusedCommandResult(reason)
	}
	ws := convWorkspace()
	logDir := filepath.Join(ws, ".loki", "jobs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "[erreur] " + err.Error()
	}
	jobsMu.Lock()
	jobSeq++
	id := fmt.Sprintf("job%d", jobSeq)
	jobsMu.Unlock()
	logPath := filepath.Join(logDir, id+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return "[erreur] " + err.Error()
	}
	// Contexte de FOND, pas celui du tour : le job doit survivre à la fin du
	// tour — c'est sa raison d'être. Il s'arrête par bash_tail kill:true, ou
	// avec le conteneur.
	cmd := newShellCmdDetached(command)
	cmd.Dir = ws
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return "[erreur] " + err.Error()
	}
	j := &bgJob{ID: id, Command: command, LogPath: logPath, Cmd: cmd, Started: time.Now()}
	jobsMu.Lock()
	jobs[id] = j
	jobsMu.Unlock()
	go func() {
		err := cmd.Wait()
		logFile.Close()
		j.mu.Lock()
		j.done = true
		if ee, ok := err.(*exec.ExitError); ok {
			j.exit = ee.ExitCode()
		}
		j.mu.Unlock()
	}()
	return fmt.Sprintf("[ok] job %s lancé (pid %d) — sortie dans .loki/jobs/%s.log, suis-la avec bash_tail", id, cmd.Process.Pid, id)
}

// toolBashTail relit la fin du journal d'un job et son état.
func toolBashTail(args map[string]any) string {
	id, _ := args["id"].(string)
	jobsMu.Lock()
	j := jobs[id]
	jobsMu.Unlock()
	if j == nil {
		return "[erreur] job inconnu : " + id
	}
	if kill, _ := args["kill"].(bool); kill {
		j.mu.Lock()
		running := !j.done
		j.mu.Unlock()
		if running && j.Cmd.Process != nil {
			_ = j.Cmd.Process.Kill()
		}
	}
	lines := 40
	if v, ok := args["lines"].(float64); ok && int(v) > 0 {
		lines = int(v)
	}
	if lines > 200 {
		lines = 200
	}
	b, err := os.ReadFile(j.LogPath)
	if err != nil {
		return "[erreur] " + err.Error()
	}
	all := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	j.mu.Lock()
	status := "en cours"
	if j.done {
		status = fmt.Sprintf("terminé (exit %d)", j.exit)
	}
	j.mu.Unlock()
	out := fmt.Sprintf("job %s — %s — depuis %s\ncommande : %s\n", j.ID, status, time.Since(j.Started).Round(time.Second), j.Command)
	tail := strings.Join(all, "\n")
	if strings.TrimSpace(tail) == "" {
		tail = "(aucune sortie pour l'instant)"
	}
	return out + "\n" + tailRunes(tail, toolMaxOutput)
}

// stopConvJobs tue les jobs d'une discussion (suppression / vidage de la
// discussion : ses fichiers partent, ses process aussi).
func stopConvJobs(convID string) {
	prefix := filepath.Join(agentWorkspace(), convFilesRoot, safeConvID(convID))
	jobsMu.Lock()
	defer jobsMu.Unlock()
	for id, j := range jobs {
		if strings.HasPrefix(j.LogPath, prefix) {
			j.mu.Lock()
			running := !j.done
			j.mu.Unlock()
			if running && j.Cmd.Process != nil {
				_ = j.Cmd.Process.Kill()
			}
			delete(jobs, id)
		}
	}
}
