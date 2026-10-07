//go:build !linux

package loki

import "os/exec"

// Hors Linux, pas d'équivalent simple à Pdeathsig : le navigateur est fermé à
// l'arrêt propre de Loki. (AJEAN passe par un job object sous Windows.)
func prepareBoundChild(cmd *exec.Cmd) {}
