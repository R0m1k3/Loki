package loki

import (
	"crypto/subtle"
	"io"
	"net/http"
	"os"
	"strings"

	bolt "go.etcd.io/bbolt"
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
		case "/internal/v1/store":
			handleInternalStore(w, r)
		case "/internal/v1/store/all":
			handleInternalStoreAll(w, r)
		case "/internal/v1/oai/route":
			handleInternalOAIRoute(w, r)
		case "/internal/v1/oai/begin":
			handleInternalOAIBegin(w, r)
		case "/internal/v1/oai/end":
			handleInternalOAIEnd(w, r)
		case "/internal/v1/oai/served":
			handleInternalOAIServed(w, r)
		case "/internal/v1/turn/perf":
			handleInternalTurnPerf(w, r)
		case "/internal/v1/turn/slot":
			handleInternalTurnSlot(w, r)
		case "/internal/v1/prewarm/check":
			handleInternalPrewarmCheck(w, r)
		case "/internal/v1/prewarm/begin":
			handleInternalPrewarmBegin(w, r)
		case "/internal/v1/prewarm/end":
			handleInternalPrewarmEnd(w, r)
		case "/internal/v1/turn/keepimg":
			handleInternalTurnKeepImg(w, r)
		case "/internal/v1/turn/keepimg/store":
			handleInternalTurnKeepImgStore(w, r)
		case "/internal/v1/tool/exec":
			handleInternalToolExec(w, r)
		case "/internal/v1/tool/context":
			handleInternalToolContext(w, r)
		case "/internal/v1/tool/tracker":
			handleInternalToolTracker(w, r)
		case "/internal/v1/tool/wrote":
			handleInternalToolWrote(w, r)
		case "/internal/v1/crypt/seal":
			handleInternalCrypt(w, r, true)
		case "/internal/v1/crypt/open":
			handleInternalCrypt(w, r, false)
		case "/internal/v1/engine/lease":
			handleInternalLease(w, r)
		case "/internal/v1/engine/lease/renew":
			handleInternalLeaseRenew(w, r)
		case "/internal/v1/engine/lease/release":
			handleInternalLeaseRelease(w, r)
		case "/internal/v1/engine/stop":
			handleInternalEngineStop(w, r)
		case "/internal/v1/recall/archive":
			handleInternalRecallArchive(w, r)
		case "/internal/v1/turn/start":
			handleInternalTurnStart(w, r)
		case "/internal/v1/turn/view":
			handleInternalTurnView(w, r)
		case "/internal/v1/turn/done":
			handleInternalTurnDone(w, r)
		case "/internal/v1/turn/verifier":
			handleInternalTurnVerifier(w, r)
		case "/internal/v1/project/activate":
			handleInternalProjectActivate(w, r)
		case "/internal/v1/chat/holder":
			handleInternalChatHolder(w, r)
		case "/internal/v1/conv/active":
			handleInternalConvActive(w, r)
		case "/internal/v1/conv/dropped":
			handleInternalConvDropped(w, r)
		case "/internal/v1/conv/workspace":
			handleInternalConvWorkspace(w, r)
		case "/internal/v1/preset/apply":
			handleInternalPresetApply(w, r)
		case "/internal/v1/tune/guard":
			handleInternalTuneGuard(w, r)
		case "/internal/v1/service/prestart":
			handleInternalServicePrestart(w, r)
		case "/internal/v1/chat/generating":
			handleInternalChatGenerating(w, r)
		case "/internal/v1/dictate/stop":
			handleInternalDictateStop(w, r)
		case "/internal/v1/web/reach/reset":
			handleInternalReachReset(w, r)
		case "/internal/v1/cdp/shutdown":
			handleInternalCDPShutdown(w, r)
		case "/internal/v1/service/action":
			handleInternalServiceAction(w, r)
		case "/internal/v1/project/switch":
			handleInternalProjectSwitch(w, r)
		case "/internal/v1/project/drop-convs":
			handleInternalProjectDropConvs(w, r)
		case "/internal/v1/engine/sidejob":
			handleInternalSideJob(w, r)
		case "/internal/v1/bench/perf":
			handleInternalBenchPerf(w, r)
		case "/internal/v1/tune/context":
			handleInternalTuneContext(w, r)
		case "/internal/v1/tasks/run":
			handleInternalTaskRun(w, r)
		case "/internal/v1/push/send":
			handleInternalPushSend(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

// --- Store ----------------------------------------------------------------
//
// Accès générique à la base pour les routes déjà portées en Rust (lot 4) : le
// Go reste l'unique propriétaire du fichier bbolt jusqu'au lot qui le
// migrera ; Rust lit et écrit À TRAVERS lui. Les écritures passent par
// putBytes/update, donc invalident le cache de ce process comme une écriture
// locale.
//
//	GET    /internal/v1/store?bucket=B&key=K   200 + valeur brute | 404 absente
//	PUT    /internal/v1/store?bucket=B&key=K   corps = valeur      → 204
//	DELETE /internal/v1/store?bucket=B&key=K                       → 204
//	GET    /internal/v1/store/all?bucket=B     200 + {"clé":"valeur",…}
//
// Base illisible : 503 — l'appelant distingue « rien » de « pas pu lire »,
// comme getBytesErr.

// internalStoreMaxPut borne une écriture : la plus grosse valeur légitime (le
// journal d'une discussion) reste très en dessous.
const internalStoreMaxPut = 64 << 20

func handleInternalStore(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bucket, key := q.Get("bucket"), q.Get("key")
	if bucket == "" || key == "" {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "bucket et key requis"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		var val []byte
		var found bool
		err := view(bucket, func(b *bolt.Bucket) error {
			if v := b.Get([]byte(key)); v != nil {
				val, found = append([]byte(nil), v...), true
			}
			return nil
		})
		if err != nil {
			sendJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
			return
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(val)
	case http.MethodPut:
		val, err := io.ReadAll(io.LimitReader(r.Body, internalStoreMaxPut+1))
		if err != nil || len(val) > internalStoreMaxPut {
			sendJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "valeur trop grande ou illisible"})
			return
		}
		if err := putBytes(bucket, key, val); err != nil {
			sendJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		err := update(bucket, func(b *bolt.Bucket) error { return b.Delete([]byte(key)) })
		if err != nil {
			sendJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "méthode non permise"})
	}
}

func handleInternalStoreAll(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	if r.Method != http.MethodGet || bucket == "" {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "GET ?bucket= attendu"})
		return
	}
	m := map[string]string{}
	err := view(bucket, func(b *bolt.Bucket) error {
		return b.ForEach(func(k, v []byte) error {
			m[string(k)] = string(v)
			return nil
		})
	})
	if err != nil {
		sendJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	sendJSON(w, http.StatusOK, m)
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
