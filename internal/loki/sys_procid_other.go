//go:build !linux && !windows

package loki

import (
	"os"
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
	// lstart passe par strftime(%c) dans la langue et le fuseau de l'appelant :
	// le processus web (launchd, sans LANG) et un terminal en français ne liraient
	// pas la même chaîne pour le même processus, et un verrou vivant passerait
	// pour périmé. Langue et fuseau fixés.
	cmd := exec.Command("ps", "-o", "lstart=", "-p", p)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C", "TZ=UTC")
	out, err := cmd.Output()
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
