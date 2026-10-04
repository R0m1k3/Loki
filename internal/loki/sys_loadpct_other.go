//go:build !linux

package loki

// Hors Linux, pas de /proc/<pid>/io : le pourcentage de chargement n'est pas
// mesurable, l'UI garde « chargement… » sans chiffre.
func engineLoadPct() int { return -1 }

// engineIOSample : pas de /proc/<pid>/io, pas d'indication « possible thrash ».
func engineIOSample() (read, rss int64, ok bool) { return 0, 0, false }
