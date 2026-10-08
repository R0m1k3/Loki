//go:build linux

package loki

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// procRef : un processus repéré par son PID ET son instant de démarrage — un
// PID recyclé pendant l'attente ne passe pas pour le même processus.
type procRef struct {
	pid   int
	start string
}

// procStat : les champs de /proc/<pid>/stat utiles ici.
type procStat struct {
	state           string
	ppid, pgrp, sid int
	start           string
}

// readProcStat lit /proc/<pid>/stat. Le nom (champ 2, entre parenthèses) peut
// contenir des blancs et des parenthèses : on repart de la DERNIÈRE fermante.
func readProcStat(pid int) (procStat, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procStat{}, false
	}
	return parseProcStat(string(b))
}

func parseProcStat(s string) (procStat, bool) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return procStat{}, false
	}
	// f[0] est le champ 3 (état), f[1] ppid, f[2] groupe, f[3] session, f[19]
	// l'instant de démarrage (champ 22).
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return procStat{}, false
	}
	ppid, _ := strconv.Atoi(f[1])
	pgrp, _ := strconv.Atoi(f[2])
	sid, _ := strconv.Atoi(f[3])
	return procStat{state: f[0], ppid: ppid, pgrp: pgrp, sid: sid, start: f[19]}, true
}

// procTable : tous les processus vivants (les zombies ont fini de tourner : leur
// mémoire, VRAM comprise, est déjà rendue).
func procTable() map[int]procStat {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	t := make(map[int]procStat, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if st, ok := readProcStat(pid); ok && st.state != "Z" {
			t[pid] = st
		}
	}
	return t
}

// procTreeOf : le chef root et tout ce qui en descend — sa session et son
// groupe (Setsid au lancement du service), plus les descendants par la
// filiation, pour un enfant qui aurait pris sa propre session (le serveur
// Python de Strata lance son moteur dans un processus à part). À relever AVANT
// d'envoyer le moindre signal : une fois le chef mort, ses orphelins sont
// rattachés à PID 1 et la filiation est perdue.
func procTreeOf(t map[int]procStat, root, self int) []procRef {
	in := map[int]bool{}
	for pid, st := range t {
		if pid != self && (pid == root || st.sid == root || st.pgrp == root) {
			in[pid] = true
		}
	}
	// Fermeture par la filiation, jusqu'à ce que plus rien ne s'ajoute.
	for grew := true; grew; {
		grew = false
		for pid, st := range t {
			if !in[pid] && pid != self && in[st.ppid] {
				in[pid] = true
				grew = true
			}
		}
	}
	refs := make([]procRef, 0, len(in))
	for pid := range in {
		refs = append(refs, procRef{pid: pid, start: t[pid].start})
	}
	return refs
}

// procRefAlive : le processus tourne encore (ni disparu, ni zombie, ni
// remplacé par un autre sous le même PID).
func procRefAlive(r procRef) bool {
	st, ok := readProcStat(r.pid)
	return ok && st.state != "Z" && st.start == r.start
}

func procRefsAlive(refs []procRef) []procRef {
	var alive []procRef
	for _, r := range refs {
		if procRefAlive(r) {
			alive = append(alive, r)
		}
	}
	return alive
}

// stopProcTree arrête le service lancé avec Setsid (pid = chef de session) et
// N'EN REVIENT QU'UNE FOIS TOUT L'ARBRE PARTI.
//
// Attendre le seul chef ne suffit pas : avec Strata, le chef est le serveur
// Python, et le moteur qui tient la VRAM (cache d'experts, plusieurs Go) est un
// autre processus qui finit de s'arrêter après lui. La relance qui suivait
// 500 ms plus tard démarrait alors llama-server sur une carte encore pleine —
// « cudaMalloc failed: out of memory » sur un modèle qui tient pourtant — et
// Strata mesurait la RAM disponible avec l'ancien moteur encore chargé.
//
// SIGTERM à tous, grace pour finir proprement, SIGKILL aux survivants, puis
// settle au plus pour que le noyau les ait vraiment retirés (un processus
// coincé dans une lecture disque ne meurt qu'à la sortie de l'appel système).
// Renvoie ceux qui tournent encore au-delà (rien à faire de plus, mais le dire).
func stopProcTree(pid int, grace, settle time.Duration) []procRef {
	refs := procTreeOf(procTable(), pid, os.Getpid())
	if len(refs) == 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	for _, r := range refs {
		_ = syscall.Kill(r.pid, syscall.SIGTERM)
	}
	if left := waitProcRefs(refs, grace); len(left) == 0 {
		return nil
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	for _, r := range procRefsAlive(refs) {
		_ = syscall.Kill(r.pid, syscall.SIGKILL)
	}
	return waitProcRefs(refs, settle)
}

// waitProcRefs attend que tous aient disparu, au plus wait. Renvoie les survivants.
func waitProcRefs(refs []procRef, wait time.Duration) []procRef {
	deadline := time.Now().Add(wait)
	for {
		left := procRefsAlive(refs)
		if len(left) == 0 || time.Now().After(deadline) {
			return left
		}
		time.Sleep(100 * time.Millisecond)
	}
}
