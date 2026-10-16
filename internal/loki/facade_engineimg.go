package loki

import (
	"os"
	"strings"
)

// facade_engineimg.go — panneau « moteur de l'image » tenu par la façade Rust
// (LOKI_ENGINE_IMAGE=rust, lot 24 de la migration ; expérimental, le Go reste
// le défaut) : version qui tourne et versions installées, dernier build publié
// sur le registre, mise à jour (job des moteurs, tenu par la façade avec
// LOKI_BACKENDS=rust), bascules, suppression, version recommandée, retour à la
// version précédente et vérification du rendu du gabarit
// (rust/loki-rs/src/engineimg). loki-rs ne passe la variable qu'avec les
// backends. /api/engine/cmdline reste ici.

const facadeEngineImageEnv = "LOKI_ENGINE_IMAGE"

// facadeEngineImage : la façade tient-elle le panneau ?
func facadeEngineImage() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeEngineImageEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// facadeEngineImageRoutes : les routes que la façade sert alors.
var facadeEngineImageRoutes = map[string]bool{
	"/api/engine":          true,
	"/api/engine/check":    true,
	"/api/engine/update":   true,
	"/api/engine/use":      true,
	"/api/engine/remove":   true,
	"/api/engine/plan":     true,
	"/api/engine/rollback": true,
}
