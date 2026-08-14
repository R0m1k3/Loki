//go:build !windows && !darwin

package loki

// setupConsole : sous Linux, le process a toujours une vraie console/tty
// standard (stdout hérité). Rien à faire → on signale simplement « on a une
// console ». Les cas « lancé par un clic » sont traités par
// sys_console_windows.go (double-clic sur loki.exe) et sys_console_darwin.go
// (ouverture de Loki.app depuis le Finder).
func setupConsole() bool { return true }

// appWarning : rien à signaler hors macOS (l'App Translocation est spécifique à
// Gatekeeper). Voir sys_console_darwin.go.
func appWarning() string { return "" }
