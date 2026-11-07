package loki

import (
	"bufio"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	"golang.org/x/term"
)

// web_password.go : connexion à l'UI par MOT DE PASSE.
//
// Le mot de passe ne remplace pas la clé de pilotage (web_key) : il sert à
// l'OBTENIR. Le serveur n'en garde qu'un hachage argon2id ; un mot de passe juste
// renvoie la clé, que le navigateur range et envoie en Bearer comme avant. Tout
// le reste (requireWebAuth, façade Rust, tunnel, chiffrement de la mémoire,
// scripts) est donc inchangé.
//
//   - Pas encore de mot de passe : l'UI propose d'en choisir un (première visite
//     après l'installation ou la mise à jour). Le premier venu le choisit :
//     ouvrir l'UI juste après, ou passer par « loki password ».
//   - Mot de passe défini : l'UI le demande, une fois par navigateur.
//   - Perdu : « loki password » dans le conteneur en définit un nouveau.

const (
	webPasswordKey    = "web_password"
	webPasswordMinLen = 8
	// Au-delà de webLoginFreeFails échecs d'affilée, chaque nouvel essai attend
	// un délai qui double, plafonné : de quoi rendre un essai en série inutile
	// sans jamais enfermer le propriétaire plus d'une minute.
	webLoginFreeFails = 5
	webLoginMaxDelay  = 60 * time.Second
)

var errWeakPassword = fmt.Errorf("mot de passe trop court (%d caractères minimum)", webPasswordMinLen)

// webPasswordHash : ce qui est stocké (JSON) sous state/web_password.
type webPasswordHash struct {
	Salt string    `json:"salt"`
	Hash string    `json:"hash"`
	KDF  kekParams `json:"kdf"`
}

func hashWebPassword(pw string) ([]byte, error) {
	salt, err := randBytes(16)
	if err != nil {
		return nil, err
	}
	kp := kekParams{T: argonTime, M: argonMemory, P: argonThreads}
	h := webPasswordHash{
		Salt: base64.StdEncoding.EncodeToString(salt),
		Hash: base64.StdEncoding.EncodeToString(deriveKEK(pw, salt, kp.T, kp.M, kp.P)),
		KDF:  kp,
	}
	return json.Marshal(h)
}

// readWebPasswordErr renvoie le hachage stocké (nil = pas de mot de passe), en
// remontant une lecture ratée : elle ne doit jamais passer pour « aucun mot de
// passe », ce qui rouvrirait le choix du mot de passe à n'importe qui.
func readWebPasswordErr() ([]byte, error) { return getBytesErr(bkState, webPasswordKey) }

// argonMu : un seul calcul argon2 (64 Mio) à la fois pour tout le serveur. Sans
// lui, une rafale d'essais de connexion épuise la RAM de la machine.
var argonMu sync.Mutex

func checkWebPassword(stored []byte, pw string) bool {
	var h webPasswordHash
	if json.Unmarshal(stored, &h) != nil {
		return false
	}
	salt, err1 := base64.StdEncoding.DecodeString(h.Salt)
	want, err2 := base64.StdEncoding.DecodeString(h.Hash)
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	argonMu.Lock()
	got := deriveKEK(pw, salt, h.KDF.T, h.KDF.M, h.KDF.P)
	argonMu.Unlock()
	return subtle.ConstantTimeCompare(got, want) == 1
}

// setWebPassword enregistre le mot de passe et garantit qu'une clé de pilotage
// existe (c'est elle que la connexion renvoie). Un retrait explicite de la clé
// (« loki set-web-key off ») est levé : choisir un mot de passe, c'est vouloir
// une UI protégée. onlyIfUnset : refuse (errPasswordSet) si un mot de passe
// existe déjà — vérifié dans la MÊME transaction, pour que deux visiteurs
// simultanés ne puissent pas choisir chacun le leur. Renvoie la clé.
func setWebPassword(pw string, onlyIfUnset bool) (string, error) {
	if len([]rune(pw)) < webPasswordMinLen {
		return "", errWeakPassword
	}
	argonMu.Lock()
	enc, err := hashWebPassword(pw)
	argonMu.Unlock()
	if err != nil {
		return "", err
	}
	fresh, err := newWebKey()
	if err != nil {
		return "", err
	}
	var key string
	err = update(bkState, func(b *bolt.Bucket) error {
		if onlyIfUnset && len(b.Get([]byte(webPasswordKey))) > 0 {
			return errPasswordSet
		}
		key = string(b.Get([]byte("web_key")))
		if key == "" {
			key = fresh
			if err := b.Put([]byte("web_key"), []byte(key)); err != nil {
				return err
			}
		}
		if err := b.Delete([]byte(webKeyOpenFlag)); err != nil {
			return err
		}
		return b.Put([]byte(webPasswordKey), enc)
	})
	if err != nil {
		return "", err
	}
	return key, nil
}

var errPasswordSet = errors.New("un mot de passe est déjà défini")

// --- Anti-essais en série -----------------------------------------------------

// Compteur GLOBAL : derrière la façade Rust, toutes les requêtes arrivent de
// 127.0.0.1, un compteur par IP ne distinguerait rien.
var webLogin struct {
	sync.Mutex
	fails int
	until time.Time
}

// webLoginWait : temps à attendre avant d'accepter un nouvel essai (0 = libre).
func webLoginWait(now time.Time) time.Duration {
	webLogin.Lock()
	defer webLogin.Unlock()
	if now.Before(webLogin.until) {
		return webLogin.until.Sub(now)
	}
	return 0
}

func webLoginResult(ok bool, now time.Time) {
	webLogin.Lock()
	defer webLogin.Unlock()
	if ok {
		webLogin.fails, webLogin.until = 0, time.Time{}
		return
	}
	webLogin.fails++
	if n := webLogin.fails - webLoginFreeFails; n > 0 {
		d := time.Second << min(n-1, 6)
		webLogin.until = now.Add(min(d, webLoginMaxDelay))
	}
}

// --- Routes publiques /api/auth/* ---------------------------------------------

// handleAuth sert /api/auth/state, /api/auth/setup et /api/auth/login. Publiques
// (le navigateur n'a pas encore la clé), mais soumises à la garde anti-site-tiers.
func handleAuth(w http.ResponseWriter, r *http.Request) {
	key, err := readWebKeyErr()
	stored, err2 := readWebPasswordErr()
	if err != nil || err2 != nil {
		sendJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "configuration illisible — réessaie dans un instant"})
		return
	}
	if msg := crossSiteReject(r, key != ""); msg != "" {
		sendJSON(w, http.StatusForbidden, map[string]any{"error": msg})
		return
	}
	if r.URL.Path == "/api/auth/state" {
		sendJSON(w, http.StatusOK, map[string]any{"state": webAuthState(r, key, stored)})
		return
	}
	if r.Method != http.MethodPost {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST attendu"})
		return
	}
	// JSON obligatoire : un formulaire d'un site tiers ne peut pas en envoyer
	// sans pré-vérification CORS.
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		sendJSON(w, http.StatusUnsupportedMediaType, map[string]any{"error": "Content-Type: application/json attendu"})
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "requête invalide"})
		return
	}
	switch r.URL.Path {
	case "/api/auth/setup":
		k, err := setWebPassword(in.Password, true)
		switch {
		case errors.Is(err, errPasswordSet):
			sendJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		case errors.Is(err, errWeakPassword):
			sendJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		case err != nil:
			sendJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		default:
			sendJSON(w, http.StatusOK, map[string]any{"token": k})
		}
	case "/api/auth/login":
		if len(stored) == 0 {
			sendJSON(w, http.StatusConflict, map[string]any{"error": "aucun mot de passe défini — choisis-en un"})
			return
		}
		now := time.Now()
		if wait := webLoginWait(now); wait > 0 {
			w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
			sendJSON(w, http.StatusTooManyRequests, map[string]any{"error": fmt.Sprintf("trop d'essais — réessaie dans %d s", int(wait.Seconds())+1)})
			return
		}
		ok := checkWebPassword(stored, in.Password)
		webLoginResult(ok, time.Now())
		// Pas de WWW-Authenticate : un mauvais mot de passe n'est pas une clé
		// expirée, l'UI ne doit pas rouvrir son écran en boucle.
		if !ok || key == "" {
			sendJSON(w, http.StatusUnauthorized, map[string]any{"error": "mot de passe incorrect"})
			return
		}
		sendJSON(w, http.StatusOK, map[string]any{"token": key})
	default:
		http.NotFound(w, r)
	}
}

// webAuthState : ce que l'UI doit afficher.
//
//	setup : aucun mot de passe — le faire choisir ;
//	login : mot de passe défini, la requête ne porte pas la bonne clé ;
//	ok    : la requête porte la bonne clé (ou l'API est ouverte exprès).
func webAuthState(r *http.Request, key string, stored []byte) string {
	if len(stored) == 0 {
		// Clé retirée exprès et pas de mot de passe : l'API est ouverte (aux
		// hôtes locaux), comme avant — rien à demander.
		if key == "" && getStr(bkState, webKeyOpenFlag) == "1" {
			return "ok"
		}
		return "setup"
	}
	// Sans clé (retirée après coup par « set-web-key off »), l'API est ouverte :
	// demander le mot de passe ne protégerait rien.
	if key == "" || checkBearer(r, key) {
		return "ok"
	}
	return "login"
}

// --- CLI -----------------------------------------------------------------------

// cmdPassword : « loki password » définit (ou remplace) le mot de passe de l'UI.
// C'est la porte de secours quand il est perdu : docker exec -it loki loki password.
// --stdin lit le mot de passe sur l'entrée standard (scripts).
func cmdPassword(args []string) error {
	var pw string
	switch {
	case len(args) > 0 && args[0] == "--stdin":
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("lecture du mot de passe : %w", err)
		}
		pw = strings.TrimRight(line, "\r\n")
	case len(args) > 0:
		return fmt.Errorf("usage : loki password [--stdin]")
	default:
		fd := int(os.Stdin.Fd())
		if !term.IsTerminal(fd) {
			return fmt.Errorf("pas de terminal : lance « docker exec -it … loki password », ou passe le mot de passe par --stdin")
		}
		fmt.Print("Nouveau mot de passe : ")
		a, err := term.ReadPassword(fd)
		fmt.Println()
		if err != nil {
			return err
		}
		fmt.Print("Encore une fois : ")
		b, err := term.ReadPassword(fd)
		fmt.Println()
		if err != nil {
			return err
		}
		if string(a) != string(b) {
			return fmt.Errorf("les deux saisies diffèrent — rien n'a changé")
		}
		pw = string(a)
	}
	if _, err := setWebPassword(pw, false); err != nil {
		return err
	}
	fmt.Printf("%s mot de passe enregistré — il est demandé à la connexion à l'interface\n", green("[ok]"))
	return nil
}
