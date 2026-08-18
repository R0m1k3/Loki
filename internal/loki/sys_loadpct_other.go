//go:build !linux

package loki

// Hors Linux, pas de /proc/<pid>/io : le pourcentage de chargement n'est pas
// mesurable, l'UI garde « chargement… » sans chiffre.
func engineLoadPct() int { return -1 }
