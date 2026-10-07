//go:build linux

package loki

// Lier le Chrome du computer use à la vie de Loki (repris d'AJEAN) : le noyau
// lui envoie SIGKILL quand Loki meurt, même tué brutalement. Sans ça, un Chrome
// orphelin restait dans le conteneur après un redémarrage de l'UI. Ses
// sous-processus suivent la mort du processus principal de Chrome.

import (
	"os/exec"
	"syscall"
)

func prepareBoundChild(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
