//go:build linux

package loki

import (
	"os"
	"strconv"
	"strings"
)

// procIdentity : instant de démarrage (en tops d'horloge depuis le boot, champ
// 22 de /proc/<pid>/stat) et binaire d'un processus vivant. Un PID recyclé
// après un redémarrage de Loki a un autre instant de démarrage : l'optimiseur ne
// tue un essai orphelin que si les deux concordent (backend_tune_lock.go).
func procIdentity(pid int) (start, exe string, ok bool) {
	if pid <= 0 {
		return "", "", false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", "", false
	}
	// Le nom du processus (champ 2, entre parenthèses) peut contenir des blancs
	// et des parenthèses : on repart de la DERNIÈRE parenthèse fermante.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return "", "", false
	}
	f := strings.Fields(s[i+1:])
	// f[0] est le champ 3 (état) : le champ 22 est donc f[19]. Un zombie a fini
	// de tourner, il ne compte plus comme vivant.
	if len(f) < 20 || f[0] == "Z" {
		return "", "", false
	}
	exe, _ = os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	return f[19], exe, true
}
