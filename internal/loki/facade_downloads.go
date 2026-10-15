package loki

import (
	"os"
	"strings"
)

// facade_downloads.go — téléchargements de modèles tenus par la façade Rust
// (LOKI_DOWNLOADS=rust, lot « modèles » de la migration ; expérimental, le Go
// reste le défaut).
//
// La façade sert elle-même /api/models/download* sur le port public. Le tunnel
// du relais et sa redistribution E2E arrivent pourtant ICI : ces routes lui
// sont remises, sans quoi un téléchargement lancé depuis le téléphone vivrait
// dans ce process, que le poste — servi par la façade — ne verrait pas.
//
// Les .part orphelins sont vidés par la façade à son démarrage : les vider ici
// (un Go relancé seul après un plantage) effacerait sous ses pieds un
// téléchargement qu'elle mène.

const facadeDownloadsEnv = "LOKI_DOWNLOADS"

// facadeDownloads : la façade tient-elle les téléchargements de modèles ? Il
// faut aussi son adresse et le jeton interne, sans lesquels rien ne peut lui
// être remis.
func facadeDownloads() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeDownloadsEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// cleanStartupPartFiles vide les .part orphelins au démarrage — sauf quand la
// façade tient les téléchargements : elle le fait à SON démarrage.
func cleanStartupPartFiles() {
	if !facadeDownloads() {
		cleanStalePartFiles()
	}
}

// facadeDownloadRoutes : les routes que la façade sert alors (downloads.rs).
var facadeDownloadRoutes = map[string]bool{
	"/api/models/download":        true,
	"/api/models/download/probe":  true,
	"/api/models/download/status": true,
	"/api/models/download/cancel": true,
}
