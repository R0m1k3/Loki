package loki

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
)

// llm_oai.go — front TLS de l'accès OpenAI public "VPS aveugle" (SNI passthrough).
//
// Principe : le SaaS parle HTTPS OpenAI standard vers <machine>.oai.ajean.link.
// Le VPS relais ne fait que recopier les octets TLS bruts (routage par SNI, sans
// déchiffrer). C'est ICI, sur l'agent, que le TLS est terminé — avec un cert dont
// la clé privée ne quitte JAMAIS cette machine — puis proxifié vers llama-server
// local (/v1). Un attaquant qui possède le VPS ne voit donc que du chiffré.
//
// Certificat : Let's Encrypt via challenge TLS-ALPN-01 servi À TRAVERS LE TUNNEL
// (l'agent est derrière CGNAT, mais le VPS forwarde la validation jusqu'à lui).
// Aucun secret DNS nulle part : le seul DNS est un wildcard *.oai.ajean.link
// statique posé une fois par l'opérateur.

// oaiSuffix est le domaine sous lequel on autorise l'émission de certificats.
const oaiSuffix = ".oai.ajean.link"

// viaTunnelHeader marque une requête arrivée par le tunnel du relais (posé par
// withLocalAuth, qui l'efface d'abord — un client ne peut donc pas le forger).
// Il sert à garder la promesse du tunnel : hors du front TLS dédié, la surface
// des complétions y reste fermée tant que oaiPublicEnabled() est faux.
const viaTunnelHeader = "X-Loki-Via"

// sendOAIError répond au format d'erreur d'OpenAI. Les SDK officiels lisent
// body.error.message ; un {"error":"…"} plat leur fait afficher un message vide,
// et certains clients (LiteLLM, LangChain) échouent carrément au décodage.
func sendOAIError(w http.ResponseWriter, status int, msg, typ, code string) {
	sendJSON(w, status, map[string]any{"error": map[string]any{
		"message": msg, "type": typ, "param": nil, "code": code,
	}})
}

// mountOAI branche la surface compatible OpenAI sur le mux de Loki, protégée par
// la clé des complétions (web_auth.go).
//
// C'est ce qui rend l'API joignable par le nom de domaine ou l'IP de
// L'INTERFACE, sans publier de second port : le moteur peut rester sur la boucle
// locale, et un reverse proxy devant Loki suffit pour le TLS. L'adresse annoncée
// auparavant (celle du moteur, port 8080) n'était joignable dans aucun
// déploiement en conteneur, où ce port n'est pas publié.
//
// ⚠️ On ne monte QUE /v1/. Surtout pas /health, /props, /metrics ni /slots — que
// oaiHandler autorise pour le tunnel : ils divulgueraient le modèle chargé, la
// taille de contexte et l'état des slots, sans authentification quand aucune clé
// n'est définie. Le filtre interne d'oaiHandler reste en seconde barrière.
func mountOAI(mux *http.ServeMux) {
	mux.Handle("/v1/", requireCompletionKey(oaiHandler()))
}

// oaiHandler construit le reverse-proxy vers llama-server, restreint à la surface
// compatible OpenAI. On NE touche PAS à l'en-tête Authorization : le client
// envoie la vraie clé, que llama-server valide lui-même (--api-key). Elle est
// donc vérifiée deux fois — par Loki puis par le moteur — et c'est voulu : le
// moteur reste protégé même si on l'expose un jour en direct.
func oaiHandler() http.Handler {
	lp := &httputil.ReverseProxy{
		// FlushInterval négatif = on vide le tampon à chaque écriture : c'est ce
		// qui fait arriver les tokens un par un chez le client au lieu d'un bloc
		// en fin de génération.
		// ⚠️ Ne JAMAIS envelopper le ResponseWriter sur ce chemin (compteur,
		// journalisation…) : un wrapper qui n'implémente pas http.Flusher
		// retransforme le flux en réponse bufferisée, sans autre symptôme qu'une
		// attente inexplicable.
		FlushInterval: -1,
		// Rewrite (et non Director) : le port du moteur est relu À CHAQUE
		// REQUÊTE. Capturé à la construction, il figeait l'ancienne valeur dès
		// qu'on changeait PORT depuis l'interface, et le proxy visait dans le
		// vide jusqu'au redémarrage de Loki. Rewrite n'hérite pas non plus des
		// X-Forwarded-* entrants : un client ne peut pas les forger vers le
		// moteur.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", LLMPort())})
			pr.Out.Host = pr.In.Host
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
			sendOAIError(w, http.StatusBadGateway,
				"moteur injoignable : "+e.Error(), "api_error", "upstream_unavailable")
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/v1") || p == "/health" || p == "/props" || p == "/metrics" || strings.HasPrefix(p, "/slots") {
			lp.ServeHTTP(w, r)
			return
		}
		sendOAIError(w, http.StatusNotFound,
			"chemin inconnu — l'endpoint compatible OpenAI est /v1/*", "invalid_request_error", "not_found")
	})
}

// runOAIFront termine le TLS sur rawLn (avec tlsCfg) puis sert oaiHandler dessus.
// rawLn peut être un vrai listener TCP (test local) ou un listener alimenté par
// les streams "raw" du tunnel (prod). Bloquant.
func runOAIFront(rawLn net.Listener, tlsCfg *tls.Config) error {
	srv := &http.Server{
		Handler:     oaiHandler(),
		IdleTimeout: 120 * time.Second,
		// completions longues : pas de Read/Write timeout.
	}
	return srv.Serve(tls.NewListener(rawLn, tlsCfg))
}

// oaiPublicEnabled indique si le front TLS du tunnel doit servir la surface
// OpenAI. Drapeau désormais en LECTURE SEULE : plus aucune interface ne
// l'allume, depuis que l'endpoint est servi par Loki sur son propre port (voir
// mountOAI). Il ne subsiste que pour les installations migrées depuis AJEAN dont
// le tunnel sert encore ce front — et pour son défaut, qui est le refus :
// demuxTunnelStream ferme le flux quand il est absent.
func oaiPublicEnabled() bool { return getBool(bkState, "oai_public") }

// oaiTLSConfig renvoie une config TLS qui, à la demande, obtient/renouvelle via
// Let's Encrypt (TLS-ALPN-01) le certificat de tout nom en *.oai.ajean.link, et
// répond elle-même aux challenges ACME. La clé privée est stockée dans
// $LOKI_HOME/certs et ne quitte jamais la machine. Toujours construite ; c'est le
// démux (oaiPublicEnabled, lu en direct) qui décide de router ou non le trafic.
func oaiTLSConfig() *tls.Config {
	certmagic.Default.Storage = &certmagic.FileStorage{Path: filepath.Join(LokiHome(), "certs")}
	certmagic.DefaultACME.Agreed = true
	certmagic.DefaultACME.Email = os.Getenv("LOKI_ACME_EMAIL")
	certmagic.DefaultACME.DisableHTTPChallenge = true // pas de :80 accessible (CGNAT) → TLS-ALPN uniquement
	magic := certmagic.NewDefault()
	magic.OnDemand = &certmagic.OnDemandConfig{
		DecisionFunc: func(_ context.Context, name string) error {
			if strings.HasSuffix(name, oaiSuffix) {
				return nil
			}
			return fmt.Errorf("nom non autorisé pour l'accès OpenAI: %s", name)
		},
	}
	cfg := magic.TLSConfig() // GetCertificate (on-demand) + gère l'ALPN acme-tls/1
	// certmagic ne met QUE "acme-tls/1" dans NextProtos ; sans "http/1.1" les
	// clients normaux sont rejetés (« unsupported application protocols »). On
	// préfixe http/1.1 tout en gardant acme-tls/1 pour les challenges.
	cfg.NextProtos = append([]string{"http/1.1"}, cfg.NextProtos...)
	cfg.MinVersion = tls.VersionTLS12
	return cfg
}

// --- démultiplexeur du tunnel ------------------------------------------------
// Le relais ouvre soit un stream HTTP normal (UI / E2E), soit un stream "brut"
// qui porte une session TLS de bout en bout (accès OpenAI). On les distingue au
// 1er octet : un enregistrement TLS commence par 0x16 (handshake), une requête
// HTTP par une lettre ASCII (GET/POST/…). Voir demuxTunnelStream dans relay_link.go.

// peekedConn rend un net.Conn dont on a déjà consulté le début, sans perdre ces
// octets (ils restent dans le bufio.Reader).
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *peekedConn) Read(b []byte) (int, error) { return p.r.Read(b) }

// chanListener est un net.Listener alimenté à la main (push), pour injecter dans
// http.Server / tls.NewListener des conns déjà acceptées ailleurs (les streams
// démultiplexés du tunnel).
type chanListener struct {
	ch   chan net.Conn
	done chan struct{}
	addr net.Addr
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{ch: make(chan net.Conn), done: make(chan struct{}), addr: addr}
}

func (l *chanListener) push(c net.Conn) {
	select {
	case l.ch <- c:
	case <-l.done:
		c.Close()
	}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *chanListener) Addr() net.Addr {
	if l.addr != nil {
		return l.addr
	}
	return dummyAddr{}
}

type dummyAddr struct{}

func (dummyAddr) Network() string { return "tunnel" }
func (dummyAddr) String() string  { return "tunnel" }

// selfSignedTLSConfig fabrique un *tls.Config auto-signé pour host. Tests locaux
// uniquement (curl -k) avant de brancher Let's Encrypt.
func selfSignedTLSConfig(host string) (*tls.Config, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// cmdOAI pilote l'accès OpenAI public côté agent.
//
//	loki oai serve [port] [host]   (test local) termine le TLS sur :port avec un
//	                               cert auto-signé et proxifie vers llama /v1.
func cmdOAI(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "serve":
		port := 8443
		if len(args) > 0 && args[0] != "" {
			n, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("port invalide: %s", args[0])
			}
			port = n
		}
		host := "localhost"
		if len(args) > 1 && args[1] != "" {
			host = args[1]
		}
		tlsCfg, err := selfSignedTLSConfig(host)
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
		if err != nil {
			return err
		}
		fmt.Printf("[loki oai] front TLS (test, auto-signé) https://%s:%d/v1 → llama :%d\n", host, port, LLMPort())
		return runOAIFront(ln, tlsCfg)
	default:
		fmt.Println("usage: loki oai serve [port] [host]   (front TLS de test → llama /v1)")
		fmt.Println("  en prod, le front TLS est servi automatiquement dans le tunnel (loki link)")
		fmt.Println("  quand LOKI_LINK_ALLOW_OAI=1 ; cert Let's Encrypt via TLS-ALPN-01.")
		return nil
	}
}
