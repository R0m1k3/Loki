package loki

import (
	"os"
	"strings"
)

// facade_mem.go — mémoire tenue par la façade Rust (LOKI_MEM=rust, lot 13 de
// la migration ; expérimental, le Go reste le défaut) : éditeur des pages,
// index MEMORY.md, déplacement d'une page vers un autre projet, panneau des
// trackers, mode mémoire et interrupteurs du mode agent.
//
// Les pages restent des fichiers memory/<projet>/*.md et les trackers des
// valeurs du bucket tracker : ce process continue de les lire et de les
// écrire pour sa propre boucle (outils mem_* et tracker, index injecté au
// modèle). Le chiffrement reste ici (la clé ne quitte pas ce process) : la
// façade fait sceller et ouvrir ses contenus par /internal/v1/crypt/*.
//
// Seules les routes changent de main ; celles qui arrivent ici par le tunnel
// du relais sont remises à la façade (withFacadeChat).

const facadeMemEnv = "LOKI_MEM"

// facadeMem : la façade tient-elle la mémoire ?
func facadeMem() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeMemEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// facadeMemRoutes : les routes que la façade sert alors (memory.rs).
var facadeMemRoutes = map[string]bool{
	"/api/mem":           true,
	"/api/mem/save":      true,
	"/api/mem/delete":    true,
	"/api/mem/move":      true,
	"/api/agent":         true,
	"/api/tools":         true,
	"/api/skills":        true,
	"/api/agent/toggle":  true,
	"/api/tools/toggle":  true,
	"/api/skills/toggle": true,
	"/api/agent/compact": true,
	"/api/memory":        true,
	"/api/tracker":       true,
}
