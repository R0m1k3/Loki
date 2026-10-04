//go:build unix

package loki

import (
	"os/exec"
	"syscall"
	"time"
)

// Moteur d'essai de l'optimiseur (backend_tune_run.go), côté Unix.
//
// L'essai est un « loki serve » qui devient llama-server par exec : même PID,
// même groupe. Setpgid en fait le chef d'un groupe à lui — un signal au groupe
// (-pid) l'atteint, quoi qu'il ait lancé, et rien d'autre : ni le processus web
// qui l'a démarré, ni le vrai moteur.
func tuneTrialAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// tuneKillTree arrête le groupe de l'essai : SIGTERM, puis SIGKILL s'il traîne.
// done se ferme quand l'appelant a récolté le processus (cmd.Wait) ; nil pour un
// orphelin d'un ancien Loki, dont on ne peut que sonder l'existence — un enfant
// non récolté resterait « vivant » (zombie) aux yeux du signal 0.
func tuneKillTree(pid int, done <-chan struct{}) {
	if pid <= 0 {
		return
	}
	gone := func(wait time.Duration) bool {
		deadline := time.Now().Add(wait)
		for time.Now().Before(deadline) {
			if done != nil {
				select {
				case <-done:
					return true
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}
			if !processAlive(pid) {
				return true
			}
			time.Sleep(100 * time.Millisecond)
		}
		return false
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	if gone(10 * time.Second) {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	gone(5 * time.Second)
}
