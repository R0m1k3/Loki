package loki

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// web_auth.go protège l'API de pilotage (loki web) quand elle est exposée sur
// internet — c.-à-d. l'API que tout client (navigateur, app mobile, script…)
// utilise pour switcher de preset, redémarrer le service, lire le status, etc.
//
// La clé de pilotage est volontairement DISTINCTE de .api_key (qui, elle,
// protège llama-server / les complétions). On veut pouvoir donner à un client
// un accès aux complétions sans lui donner le droit de redémarrer la machine,
// et inversement. Elle est relue à chaque requête,
// depuis un cache invalidé à chaque écriture (voir readWebKeyErr), pour qu'un
// changement de clé prenne effet sans redémarrer le serveur web.

// readWebKeyErr renvoie la clé de pilotage en distinguant « aucune clé » d'une
// LECTURE RATÉE. La nuance est tout sauf cosmétique : sans clé, l'API est
// ouverte. Confondre les deux, c'est ouvrir l'API parce que la base était
// momentanément verrouillée par une commande CLI — une panne d'E/S qui désarme
// l'authentification. requireWebAuth refuse donc plutôt que d'ouvrir.
//
// requireWebAuth la consulte à CHAQUE requête : sans cache, chacune payait une
// ouverture complète de la base sous dbMu (vignettes chargées deux par deux,
// SSE, statut…), en concurrence avec l'écrivain des discussions (AJEAN #120).
// On garde la dernière lecture RÉUSSIE, invalidée comme cachedKV : écriture de
// ce process (cacheBust du bucket d'état), date/taille du fichier pour les
// autres process (la CLI), une seconde au plus. Une lecture ratée n'est jamais
// mise en cache : elle remonte, et l'API reste fermée.
func readWebKeyErr() (string, error) {
	path := dbPath()
	mtime, size := dbStamp()
	c := &webKeyCache
	c.Lock()
	if c.ok && c.path == path && c.size == size && c.mtime.Equal(mtime) && time.Since(c.when) < cacheMaxAge {
		k := c.key
		c.Unlock()
		return k, nil
	}
	gen := c.gen
	c.Unlock()
	b, err := getBytesErr(bkState, "web_key")
	if err != nil {
		return "", err
	}
	// Date et taille relevées APRÈS la lecture : ouvrir la base peut à lui seul
	// toucher le fichier (Windows), et un relevé d'avant ne correspondait alors
	// jamais. Une écriture externe glissée entre la lecture et le relevé reste
	// couverte par l'âge maximal.
	mtime, size = dbStamp()
	c.Lock()
	// Une invalidation pendant la lecture : la valeur lue est peut-être déjà
	// l'ancienne — ne pas la garder.
	if c.gen == gen {
		c.ok, c.path, c.key, c.when, c.mtime, c.size = true, path, string(b), time.Now(), mtime, size
	}
	c.Unlock()
	return string(b), nil
}

var webKeyCache struct {
	sync.Mutex
	ok    bool
	gen   uint64 // incrémenté à chaque invalidation
	path  string // la base lue (LOKI_HOME change d'un test à l'autre)
	key   string
	when  time.Time
	mtime time.Time
	size  int64
}

// webKeyCacheBust : appelé par cacheBust pour toute écriture du bucket d'état.
func webKeyCacheBust() {
	webKeyCache.Lock()
	webKeyCache.ok = false
	webKeyCache.gen++
	webKeyCache.Unlock()
}

// readWebKey renvoie la clé de pilotage, ou "" si aucune n'est définie (ou
// illisible). Réservé à l'affichage ; toute décision d'accès passe par
// readWebKeyErr.
func readWebKey() string { k, _ := readWebKeyErr(); return k }

// requireWebAuth wraps an HTTP handler, rejecting requests that don't present
// the configured Bearer token. When no key is configured the handler is left
// open — ce qui n'arrive plus qu'après un retrait explicite (loki set-web-key
// off) : cmdWeb génère sinon une clé au démarrage (ensureWebKey) et avertit
// bruyamment si l'API reste ouverte.
func requireWebAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, err := readWebKeyErr()
		if err != nil {
			// On ne sait pas si une clé protège cette API : on ferme.
			sendJSON(w, http.StatusServiceUnavailable,
				map[string]any{"error": "configuration illisible — réessaie dans un instant"})
			return
		}
		if msg := crossSiteReject(r, key != ""); msg != "" {
			sendJSON(w, http.StatusForbidden, map[string]any{"error": msg})
			return
		}
		if key == "" {
			next(w, r)
			return
		}
		if !checkBearer(r, key) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="loki"`)
			sendJSON(w, http.StatusUnauthorized, map[string]any{"error": "non autorisé"})
			return
		}
		next(w, r)
	}
}

// requireCompletionKey protège la surface compatible OpenAI (/v1) avec la clé
// des COMPLÉTIONS, et non avec la clé de pilotage. Trois raisons, dans l'ordre
// d'importance :
//
//  1. Sémantique : c'est exactement la séparation décrite en tête de ce fichier
//     — on veut pouvoir donner à une app tierce l'accès aux complétions sans lui
//     donner le droit de redémarrer la machine.
//  2. Technique : un client OpenAI n'a qu'UN en-tête Authorization. S'il y met la
//     clé des complétions, une garde de pilotage le refuse ; s'il y met celle de
//     pilotage, llama-server (qui reçoit l'en-tête tel quel) la refuse. Les deux
//     gardes ne peuvent pas être satisfaites en même temps.
//  3. Format : les SDK OpenAI lisent body.error.message ; un {"error":"…"} plat
//     leur fait afficher un message vide.
func requireCompletionKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Préflight CORS : un OPTIONS ne porte JAMAIS d'Authorization. Le refuser
		// casse tout client tiers qui tourne dans un navigateur (playground web,
		// interface de chat hébergée ailleurs). llama-server répond lui-même le
		// préflight, ses en-têtes CORS traversent le proxy tels quels.
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		// Trafic venu du tunnel du relais : la surface OpenAI n'y est ouverte que
		// si le drapeau historique l'autorise. Sans lui, le tunnel ne servait
		// cette surface que par son front TLS dédié ; la monter sur le mux ne
		// doit pas l'ouvrir en douce par le canal HTTP.
		if r.Header.Get(viaTunnelHeader) == "tunnel" && !oaiPublicEnabled() {
			sendOAIError(w, http.StatusNotFound,
				"chemin inconnu — l'endpoint compatible OpenAI est /v1/*", "invalid_request_error", "not_found")
			return
		}
		key, err := effectiveAPIKeyErr()
		if err != nil {
			// On ne sait pas si une clé protège cet endpoint : on ferme.
			sendOAIError(w, http.StatusServiceUnavailable,
				"configuration illisible — réessaie dans un instant", "api_error", "service_unavailable")
			return
		}
		if key == "" {
			// Aucune clé : endpoint ouvert, comme llama-server sans --api-key. Le
			// panneau Accès OpenAI l'affiche en rouge, et cmdWeb le crie au
			// démarrage — c'est le prix d'un réglage local sans friction.
			next.ServeHTTP(w, r)
			return
		}
		if !checkBearer(r, key) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="loki"`)
			sendOAIError(w, http.StatusUnauthorized,
				"Clé API invalide ou absente — envoie l'en-tête Authorization: Bearer <clé>.",
				"invalid_request_error", "invalid_api_key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkBearer reports whether the request carries the expected key as an
// "Authorization: Bearer <clé>" header. La comparaison est à temps constant.
// PAS de repli ?key=<clé> en query string : une clé dans l'URL finit dans les
// logs des proxys (Caddy, Cloudflare), l'historique navigateur et les Referer.
func checkBearer(r *http.Request, key string) bool {
	want := []byte(key)
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		got := []byte(strings.TrimSpace(h[len("Bearer "):]))
		if subtle.ConstantTimeCompare(got, want) == 1 {
			return true
		}
	}
	return false
}

// cmdSetWebKey sets (or clears) the control-API key in $LOKI_HOME/.web_key.
//
//	loki set-web-key <clé>     définit la clé
//	loki set-web-key           génère une clé aléatoire
//	loki set-web-key show      affiche la clé actuelle
//	loki set-web-key off       supprime la protection (API ouverte), et
//	                           empêche sa régénération au démarrage
//
// Contrairement à set-api-key, aucun redémarrage n'est nécessaire : le serveur
// web relit la clé à chaque requête.
func cmdSetWebKey(args []string) error {
	var key string
	switch {
	case len(args) == 0:
		k, err := newWebKey()
		if err != nil {
			return err
		}
		key = k
		fmt.Printf("%s clé générée : %s\n", green("[ok]"), bold(key))
	case args[0] == "show":
		k, err := readWebKeyErr()
		if err != nil {
			return err
		}
		if k == "" {
			fmt.Printf("%s aucune clé de pilotage — l'API web n'est pas protégée\n", yellow("[!]"))
		} else {
			fmt.Println(k)
		}
		return nil
	case args[0] == "" || args[0] == "off" || args[0] == "none":
		key = ""
	default:
		key = strings.TrimSpace(args[0])
	}
	if err := putStr(bkState, "web_key", key); err != nil {
		return err
	}
	// Retrait EXPLICITE : on le retient, pour que le démarrage suivant ne
	// régénère pas une clé (voir ensureWebKey). Toute autre valeur l'annule.
	open := ""
	if key == "" {
		open = "1"
	}
	if err := putStr(bkState, webKeyOpenFlag, open); err != nil {
		return err
	}
	if key == "" {
		fmt.Printf("%s clé de pilotage supprimée — l'API web n'est plus protégée\n", yellow("[info]"))
		return nil
	}
	fmt.Printf("%s clé de pilotage enregistrée\n", green("[ok]"))
	fmt.Printf("       les clients doivent envoyer : %s\n", dim("Authorization: Bearer "+key))
	fmt.Printf("       (relance 'loki web' si le serveur web tourne déjà — non requis, lu à chaud)\n")
	return nil
}

// webKeyOpenFlag : posé par « loki set-web-key off » — le propriétaire a retiré
// la clé de pilotage EXPRÈS. Sans lui, une API sans clé est un oubli, et
// ensureWebKey en génère une au démarrage.
const webKeyOpenFlag = "web_key_open"

// newWebKey tire une clé de pilotage aléatoire (192 bits).
func newWebKey() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "loki-web-" + hex.EncodeToString(buf), nil
}

// ensureWebKey : sans clé de pilotage, l'API /api/* est ouverte à quiconque
// joint le port — et elle mène jusqu'à l'exécution de commandes (serveurs MCP,
// llama.cpp personnalisé, agent). Le port étant publié sur toutes les
// interfaces par défaut, on ne démarre plus jamais sans clé : au premier
// démarrage (ou sur une installation qui n'en avait pas), une clé aléatoire est
// générée, enregistrée là où « loki set-web-key » la range, et renvoyée pour
// être affichée UNE fois dans le journal. Une clé déjà définie n'est jamais
// touchée ; un retrait explicite (« loki set-web-key off ») est respecté.
// Renvoie la clé générée, "" si rien n'a été fait.
func ensureWebKey() (string, error) {
	key, err := readWebKeyErr()
	if err != nil || key != "" {
		return "", err
	}
	if getStr(bkState, webKeyOpenFlag) == "1" {
		return "", nil
	}
	key, err = newWebKey()
	if err != nil {
		return "", err
	}
	if err := putStr(bkState, "web_key", key); err != nil {
		return "", err
	}
	return key, nil
}

// crossSiteReject refuse les requêtes qu'un SITE TIERS ouvert dans le navigateur
// (de la machine ou du réseau local) ferait en douce vers l'API. Sans clé (le
// défaut), l'API est ouverte : une page malveillante pouvait envoyer un POST
// « simple » (text/plain, sans pré-vérification CORS) à <serveur>:8090 et
// piloter Loki, jusqu'à faire exécuter des commandes à l'agent. Renvoie "" si
// la requête est acceptable, sinon la raison du refus. Repris d'AJEAN 0.15.5.
//
//   - Sec-Fetch-Site: cross-site → refus (navigateurs récents, couvre aussi les
//     GET déclenchés par une balise <img>/<form>).
//   - Origin présent et différent de l'hôte appelé → refus (tous les navigateurs
//     envoient Origin sur un POST cross-origin). Les clients hors navigateur
//     (scripts, curl, apps) n'envoient pas d'Origin : non concernés.
//   - Sans clé seulement : l'hôte appelé doit être local (IP, localhost, nom de
//     la machine, nom sans point, .local/.lan…) ou listé dans
//     LOKI_TRUSTED_HOSTS. Bloque le « DNS rebinding », où un domaine malveillant
//     se fait résoudre en IP locale pour paraître même-origine. Avec une clé,
//     inutile : le navigateur n'envoie jamais le Bearer tout seul. Le trafic du
//     tunnel (authentifié par le relais) est lui aussi dispensé de ce contrôle.
func crossSiteReject(r *http.Request, keyed bool) string {
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return "requête d'un site tiers refusée"
	}
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		u, err := url.Parse(o)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			return "origine non autorisée : " + o
		}
	} else if o == "null" {
		return "origine non autorisée"
	}
	if !keyed && r.Header.Get(viaTunnelHeader) != "tunnel" && !localHostName(r.Host) {
		return "hôte « " + r.Host + " » non autorisé sans clé de pilotage — définis-en une (loki set-web-key) ou ajoute ce nom à LOKI_TRUSTED_HOSTS"
	}
	return ""
}

// localHostName : l'hôte désigne-t-il la machine ou le réseau local (et non un
// domaine public, seul utilisable pour un DNS rebinding) ?
func localHostName(hostport string) bool {
	h := hostport
	if hh, _, err := net.SplitHostPort(hostport); err == nil {
		h = hh
	}
	h = strings.ToLower(strings.Trim(h, "[]."))
	if h == "" || net.ParseIP(h) != nil || !strings.Contains(h, ".") {
		return true
	}
	for _, suf := range []string{".localhost", ".local", ".lan", ".home", ".internal", ".home.arpa", ".localdomain"} {
		if strings.HasSuffix(h, suf) {
			return true
		}
	}
	if me, err := os.Hostname(); err == nil && me != "" {
		me = strings.ToLower(me)
		if h == me || strings.HasPrefix(h, me+".") {
			return true
		}
	}
	// Échappatoire du conteneur : derrière un reverse proxy (loki.mondomaine.fr),
	// le nom public n'est ni local ni celui du conteneur.
	for _, t := range strings.Split(os.Getenv("LOKI_TRUSTED_HOSTS"), ",") {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" && h == t {
			return true
		}
	}
	return false
}
