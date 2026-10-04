//go:build windows

package loki

import (
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

// Moteur d'essai de l'optimiseur (backend_tune_run.go), côté Windows.
//
// L'essai est un « loki serve » qui lance llama-server en ENFANT (execServer) :
// deux processus. taskkill /T emporte l'arbre entier à partir du PID vérifié,
// comme svcStop le fait pour le vrai moteur. Pas de console : le processus web
// tourne souvent en mode application.
func tuneTrialAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNewProcessGroup | createNoWindow,
	}
}

// tuneKillTree arrête l'arbre de l'essai. done : voir la version Unix.
func tuneKillTree(pid int, done <-chan struct{}) {
	if pid <= 0 {
		return
	}
	_ = hideCmd(exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")).Run()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if done != nil {
			select {
			case <-done:
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		if !processAlive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// procIdentity : heure de création et image d'un processus vivant. Le PID seul
// ne suffit pas à désigner l'essai orphelin d'un Loki redémarré : Windows
// recycle vite ses PID, et tuer « le PID noté » pourrait viser n'importe quoi.
func procIdentity(pid int) (start, exe string, ok bool) {
	if pid <= 0 || !processAlive(pid) {
		return "", "", false
	}
	const queryLimitedInfo = 0x1000
	k := syscall.NewLazyDLL("kernel32.dll")
	h, _, _ := k.NewProc("OpenProcess").Call(queryLimitedInfo, 0, uintptr(pid))
	if h == 0 {
		return "", "", false
	}
	defer k.NewProc("CloseHandle").Call(h)
	var creation, exit, kernel, user syscall.Filetime
	r, _, _ := k.NewProc("GetProcessTimes").Call(h,
		uintptr(unsafe.Pointer(&creation)), uintptr(unsafe.Pointer(&exit)),
		uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		return "", "", false
	}
	start = strconv.FormatInt(creation.Nanoseconds(), 10)
	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	if r, _, _ := k.NewProc("QueryFullProcessImageNameW").Call(h, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r != 0 {
		exe = syscall.UTF16ToString(buf[:size])
	}
	return start, exe, true
}
