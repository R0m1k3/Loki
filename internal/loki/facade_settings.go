package loki

import (
	"os"
	"strings"
)

// facade_settings.go — réglages partagés tenus par la façade Rust
// (LOKI_SETTINGS=rust, lot 10 de la migration ; expérimental, le Go reste le
// défaut) : préférences d'affichage, prompt système, intensité du
// raisonnement, adresse publique.
//
// Les valeurs restent dans la base, aux mêmes clés : ce process les relit là
// où il s'en sert (tour de chat, accès OpenAI). Seules les routes changent de
// main ; celles qui arrivent ici par le tunnel du relais sont remises à la
// façade (withFacadeChat).

const facadeSettingsEnv = "LOKI_SETTINGS"

// facadeSettings : la façade tient-elle les réglages partagés ?
func facadeSettings() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeSettingsEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// facadeSettingsRoutes : les routes que la façade sert alors (settings.rs).
var facadeSettingsRoutes = map[string]bool{
	"/api/prefs":            true,
	"/api/sysprompt":        true,
	"/api/reasoning-effort": true,
	"/api/oai/public":       true,
}
