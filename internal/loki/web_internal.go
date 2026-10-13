package loki

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"
)

// API interne entre la façade Rust (rust/loki-rs) et ce serveur.
//
// La façade reprend des routes une à une ; pour authentifier celles qu'elle
// sert elle-même, elle doit connaître la clé de pilotage, qui vit dans la base
// bbolt — verrouillée en exclusif par ce process. Elle la demande donc ici, à
// chaque requête (même règle que requireWebAuth : un changement de clé prend
// effet immédiatement).
//
// Trois verrous, parce que la réponse contient un secret :
//   - la route n'existe que sur l'écoute TCP directe (withInternalAPI enveloppe
//     le mux dans cmdWeb) : le tunnel du relais sert le mux nu et ne la voit
//     jamais ;
//   - elle exige le jeton que la façade tire au hasard à chaque démarrage et
//     transmet par l'environnement (LOKI_INTERNAL_TOKEN) ;
//   - sans jeton, jeton trop court ou faux : 404, rien ne trahit la route.

const (
	internalTokenEnv    = "LOKI_INTERNAL_TOKEN"
	internalTokenHeader = "X-Loki-Internal"
	internalPrefix      = "/internal/"
	// Un jeton plus court qu'un secret de 128 bits désactive l'API interne :
	// mieux vaut une façade qui ferme l'accès qu'une route devinable.
	internalTokenMinLen = 32
)

func internalToken() string {
	t := strings.TrimSpace(os.Getenv(internalTokenEnv))
	if len(t) < internalTokenMinLen {
		return ""
	}
	return t
}

// withInternalAPI sert /internal/* et passe tout le reste à next.
func withInternalAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, internalPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		tok := internalToken()
		got := r.Header.Get(internalTokenHeader)
		if tok == "" || subtle.ConstantTimeCompare([]byte(got), []byte(tok)) != 1 {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/internal/v1/auth":
			handleInternalAuth(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

// handleInternalAuth : de quoi authentifier une requête de pilotage côté Rust.
// Base illisible → 503 : la façade ferme alors l'accès, elle n'ouvre jamais
// sur une panne d'E/S (cf. readWebKeyErr).
func handleInternalAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET attendu"})
		return
	}
	key, err := readWebKeyErr()
	if err != nil {
		sendJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "configuration illisible"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	sendJSON(w, http.StatusOK, map[string]any{"web_key": key, "version": Version})
}
