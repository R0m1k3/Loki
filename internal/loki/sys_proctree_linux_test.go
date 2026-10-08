//go:build linux

package loki

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseProcStat(t *testing.T) {
	st, ok := parseProcStat("4242 (python3 (x) y) S 4200 4100 4000 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 987654 0 0")
	if !ok || st.state != "S" || st.ppid != 4200 || st.pgrp != 4100 || st.sid != 4000 || st.start != "987654" {
		t.Fatalf("lecture fausse : %+v ok=%v", st, ok)
	}
	if _, ok := parseProcStat("4242 (tronqué"); ok {
		t.Error("ligne sans parenthèse fermante acceptée")
	}
}

func TestProcTreeOf(t *testing.T) {
	tab := map[int]procStat{
		100: {ppid: 1, pgrp: 100, sid: 100, start: "a"}, // chef (Setsid)
		101: {ppid: 100, pgrp: 100, sid: 100, start: "b"},
		200: {ppid: 101, pgrp: 200, sid: 200, start: "c"}, // moteur dans sa propre session
		201: {ppid: 200, pgrp: 200, sid: 200, start: "d"},
		300: {ppid: 1, pgrp: 300, sid: 300, start: "e"},   // étranger
		400: {ppid: 1, pgrp: 100, sid: 100, start: "f"},   // orphelin rattaché à PID 1
		500: {ppid: 100, pgrp: 100, sid: 100, start: "g"}, // nous-mêmes
	}
	got := map[int]bool{}
	for _, r := range procTreeOf(tab, 100, 500) {
		got[r.pid] = true
	}
	for _, pid := range []int{100, 101, 200, 201, 400} {
		if !got[pid] {
			t.Errorf("PID %d oublié", pid)
		}
	}
	if got[300] || got[500] {
		t.Errorf("hors de l'arbre pris : %v", got)
	}
}

// Le chef meurt vite, son enfant (dans une autre session, comme le moteur de
// Strata) traîne à s'arrêter : stopProcTree ne revient qu'une fois tout parti.
func TestStopProcTreeWaitsForChildren(t *testing.T) {
	// Le chef lance un enfant qui ignore SIGTERM dans sa propre session, écrit
	// son PID, puis attend.
	pidFile := t.TempDir() + "/child"
	script := `setsid sh -c 'trap "" TERM; echo $$ > ` + pidFile + `; while :; do sleep 0.1; done' & wait`
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Skip("sh indisponible :", err)
	}
	go cmd.Wait() // pas de zombie pour le chef
	var child int
	for i := 0; i < 50 && child == 0; i++ {
		time.Sleep(50 * time.Millisecond)
		if b, err := os.ReadFile(pidFile); err == nil {
			child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}
	if child == 0 {
		t.Fatal("l'enfant n'a pas démarré")
	}
	st, ok := readProcStat(child)
	if !ok || st.sid == cmd.Process.Pid {
		t.Fatalf("l'enfant devait avoir sa propre session : %+v", st)
	}
	ref := procRef{pid: child, start: st.start}
	if left := stopProcTree(cmd.Process.Pid, 300*time.Millisecond, 5*time.Second); len(left) != 0 {
		t.Fatalf("survivants : %v", left)
	}
	if procRefAlive(ref) {
		t.Fatal("l'enfant tourne encore au retour de stopProcTree")
	}
}
