//go:build linux

package loki

import (
	"strings"
	"testing"
)

// L'unité du moteur écrite par « loki install » ne relance pas un refus de
// LOAD_GUARD : sinon systemd le relançait toutes les trois secondes, sans fin.
func TestEngineUnitPreventsRefusalRestart(t *testing.T) {
	if !unitPreventsRefusalRestart(engineUnitTemplate) {
		t.Fatal("RestartPreventExitStatus absent de l'unité du moteur")
	}
	if !strings.Contains(engineUnitTemplate, "Restart=on-failure") {
		t.Fatal("les autres échecs doivent toujours être relancés")
	}
}
