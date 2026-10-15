package loki

import (
	"os"
	"strings"
)

// facade_update.go — mise à jour de Loki tenue par la façade Rust
// (LOKI_UPDATE=rust, lot 21 de la migration ; expérimental, le Go reste le
// défaut) : /api/update et /api/update/apply. La façade ne tourne qu'en
// conteneur, où l'image se met à jour par Docker : les deux routes le disent
// (containerUpdateNote). Hors conteneur, loki-rs ne passe pas la variable.

const facadeUpdateEnv = "LOKI_UPDATE"

// facadeUpdate : la façade tient-elle la mise à jour ?
func facadeUpdate() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeUpdateEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// facadeUpdateRoutes : les routes que la façade sert alors (sysupdate.rs).
var facadeUpdateRoutes = map[string]bool{
	"/api/update":       true,
	"/api/update/apply": true,
}
