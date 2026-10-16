package loki

import (
	"fmt"
	"os"
	"strings"
)

// facade_dictate.go — dictée vocale tenue par la façade Rust (LOKI_DICTATE=rust,
// lot 9 de la migration ; expérimental, le Go reste le défaut).
//
// La façade sert elle-même /api/transcribe et /api/dictate/* sur le port
// public, et supervise le serveur de transcription. Deux choses restent à
// régler ici :
//
//   - le tunnel du relais et sa redistribution E2E arrivent dans ce process :
//     ces routes sont remises à la façade (withFacadeChat) ;
//   - libérer la VRAM (web_vram.go) et l'optimiseur (backend_tune_run.go)
//     arrêtent la dictée : c'est alors à la façade qu'on le demande, son
//     serveur n'étant pas un enfant de ce process.

const facadeDictateEnv = "LOKI_DICTATE"

// facadeDictate : la façade tient-elle la dictée ? Il faut aussi son adresse
// et le jeton interne, sans lesquels rien ne peut lui être remis.
func facadeDictate() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeDictateEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// facadeDictateRoutes : les routes que la façade sert alors (dictate.rs).
var facadeDictateRoutes = map[string]bool{
	"/api/transcribe":              true,
	"/api/dictate/config":          true,
	"/api/dictate/models":          true,
	"/api/dictate/models/download": true,
	"/api/dictate/state":           true,
}

// facadeDictateStop demande à la façade d'arrêter son serveur de dictée, sans
// attendre derrière un démarrage (elle tue le processus qui démarre). Rend
// false quand la façade ne tient pas la dictée : l'appelant arrête alors le
// serveur de ce process.
func facadeDictateStop() bool {
	if !facadeDictate() {
		return false
	}
	if err := facadePost("/internal/facade/v1/dictate/stop", map[string]any{}); err != nil {
		fmt.Printf("[dictée] arrêt demandé à la façade : %v\n", err)
	}
	return true
}
