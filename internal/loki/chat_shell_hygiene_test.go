package loki

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

func TestSplitTailPipe(t *testing.T) {
	cas := map[string]struct {
		cmd string
		n   int
	}{
		"go build ./... 2>&1 | tail -20": {"go build ./... 2>&1", 20},
		"make | tail -n 5":               {"make", 5},
		"make|tail -n40":                 {"make", 40},
		"go test | tail":                 {"go test", 10},
		"tail -f log.txt":                {"tail -f log.txt", 0},
		"cat a | tail -5 | grep x":       {"cat a | tail -5 | grep x", 0},
		"echo ok":                        {"echo ok", 0},
	}
	for in, want := range cas {
		if cmd, n := splitTailPipe(in); cmd != want.cmd || n != want.n {
			t.Errorf("%q → (%q, %d), attendu (%q, %d)", in, cmd, n, want.cmd, want.n)
		}
	}
	if !trailingBackground("./serveur &") || trailingBackground("make && make test") || trailingBackground("ls 2>&1") {
		t.Error("trailingBackground se trompe")
	}
	if got := stripANSI("\x1b[1;31mFAIL\x1b[0m ok"); got != "FAIL ok" {
		t.Errorf("stripANSI = %q", got)
	}
}

// « cmd | tail -N » rend le VRAI code de sortie, et seulement N lignes.
func TestShellTailGardeLeCodeDeSortie(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell POSIX")
	}
	withWorkspace(t)
	out := runShell(context.Background(), "sh -c 'printf \"l1\nl2\nl3\n\"; exit 2' | tail -n 2", 10)
	if !strings.HasPrefix(out, "exit: 2") || strings.Contains(out, "l1") || !strings.Contains(out, "l3") {
		t.Fatalf("sortie = %q", out)
	}
}

// Au délai dépassé, la sortie déjà produite est rendue.
func TestShellTimeoutGardeLaSortie(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell POSIX")
	}
	withWorkspace(t)
	out := runShell(context.Background(), "echo avant; sleep 5", 1)
	if !strings.Contains(out, "timeout") || !strings.Contains(out, "avant") {
		t.Fatalf("sortie = %q", out)
	}
}
