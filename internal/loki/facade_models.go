package loki

import (
	"os"
	"strings"
)

// facade_models.go — routes des modèles tenues par la façade Rust
// (LOKI_MODELS=rust, lot 8 de la migration ; expérimental, le Go reste le
// défaut) : liste du sélecteur, dossiers, suppression, téléchargements et
// catalogue Hugging Face.
//
// La façade sert elle-même ces routes sur le port public. Le tunnel du relais
// et sa redistribution E2E arrivent pourtant ICI : elles lui sont remises, sans
// quoi un téléchargement lancé depuis le téléphone vivrait dans ce process, que
// le poste — servi par la façade — ne verrait pas.
//
// Les .part orphelins sont vidés par la façade à son démarrage : les vider ici
// (un Go relancé seul après un plantage) effacerait sous ses pieds un
// téléchargement qu'elle mène.

const facadeModelsEnv = "LOKI_MODELS"

// facadeModels : la façade tient-elle les routes des modèles ? Il faut aussi
// son adresse et le jeton interne, sans lesquels rien ne peut lui être remis.
func facadeModels() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeModelsEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// cleanStartupPartFiles vide les .part orphelins au démarrage — sauf quand la
// façade tient les téléchargements : elle le fait à SON démarrage.
func cleanStartupPartFiles() {
	if !facadeModels() {
		cleanStalePartFiles()
	}
}

// facadeModelRoutes : les routes que la façade sert alors (models.rs,
// downloads.rs, hf.rs).
var facadeModelRoutes = map[string]bool{
	"/api/models":                 true,
	"/api/models/delete":          true,
	"/api/models/dirs":            true,
	"/api/hf/search":              true,
	"/api/hf/files":               true,
	"/api/hf/token":               true,
	"/api/models/download":        true,
	"/api/models/download/probe":  true,
	"/api/models/download/status": true,
	"/api/models/download/cancel": true,
}
