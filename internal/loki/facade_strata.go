package loki

import (
	"os"
	"strings"
)

// facade_strata.go — Strata tenu par la façade Rust (LOKI_STRATA=rust, lot 25
// de la migration ; expérimental, le Go reste le défaut) : la machine et ce
// qui y tient, les réglages d'un modèle installé et l'installation (job des
// moteurs, tenu par la façade avec LOKI_BACKENDS=rust) —
// rust/loki-rs/src/strata. loki-rs ne passe la variable qu'avec les backends.
// Le lancement (`loki serve` d'un preset ENGINE=strata) reste ici.

const facadeStrataEnv = "LOKI_STRATA"

// facadeStrata : la façade tient-elle Strata ?
func facadeStrata() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeStrataEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// facadeStrataRoutes : les routes que la façade sert alors.
var facadeStrataRoutes = map[string]bool{
	"/api/strata":          true,
	"/api/strata/install":  true,
	"/api/strata/settings": true,
}
