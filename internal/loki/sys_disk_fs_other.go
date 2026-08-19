//go:build !linux

package loki

// Hors Linux, on n'a pas de table de magies de systèmes de fichiers à
// consulter : l'espace libre rapporté est considéré comme fiable (c'est le cas
// des volumes locaux Windows et macOS, les seules cibles concernées ici).
func diskFreeReliable(string) bool { return true }
