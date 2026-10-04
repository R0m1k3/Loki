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
//
// L'instant est relatif au boot : après une coupure de courant, un service
// lancé au même moment du démarrage peut retrouver le même PID ET le même top,
// et un verrou laissé par l'ancien passerait pour vivant — tout serait refusé
// jusqu'à son retrait à la main. L'identifiant du boot le préfixe donc.
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
	// Binaire remplacé par une mise à jour pendant que le processus tourne : le
	// lien porte « (deleted) », c'est pourtant bien le même processus.
	exe = strings.TrimSuffix(exe, " (deleted)")
	start = f[19]
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			start = id + "/" + start
		}
	}
	return start, exe, true
}
