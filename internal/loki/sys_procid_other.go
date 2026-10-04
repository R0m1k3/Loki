//go:build !linux && !windows

package loki

import (
	"os/exec"
	"strconv"
	"strings"
)

// procIdentity : instant de démarrage et binaire d'un processus vivant, lus par
// ps (macOS, BSD). Voir la version Linux pour le pourquoi.
func procIdentity(pid int) (start, exe string, ok bool) {
	if pid <= 0 || !processAlive(pid) {
		return "", "", false
	}
	p := strconv.Itoa(pid)
	out, err := exec.Command("ps", "-o", "lstart=", "-p", p).Output()
	if err != nil {
		return "", "", false
	}
	start = strings.Join(strings.Fields(string(out)), " ")
	if start == "" {
		return "", "", false
	}
	if out, err := exec.Command("ps", "-o", "comm=", "-p", p).Output(); err == nil {
		exe = strings.TrimSpace(string(out))
	}
	return start, exe, true
}
