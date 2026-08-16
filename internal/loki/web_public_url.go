package loki

// web_public_url.go — l'adresse par laquelle on atteint cette instance.
//
// Le panneau « Accès OpenAI » doit afficher une adresse qu'on puisse coller
// telle quelle dans un client tiers. Il annonçait auparavant l'adresse du
// MOTEUR (http://<ip>:8080/v1), qui n'est joignable dans aucun déploiement en
// conteneur — le port n'y est pas publié — et dont l'IP, en conteneur, est
// celle du bridge Docker. Deux façons de faire mieux, dans cet ordre :
//
//  1. l'adresse publique enregistrée, quand l'utilisateur passe par un nom de
//     domaine ou un reverse proxy et l'a saisie une fois pour toutes ;
//  2. sinon, l'origine de la requête elle-même — c'est-à-dire exactement le
//     domaine ou l'IP qu'on a tapés pour ouvrir l'interface.
//
// Le calcul est ici, en Go, et pas dans le navigateur : une seule règle de
// priorité, réutilisable par la CLI et les journaux, et plus de construction
// d'URL à coups de concaténation côté client — c'est ce qui avait produit
// l'adresse fantôme.

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// publicURL renvoie l'adresse publique enregistrée, normalisée, ou "".
func publicURL() string { return getStr(bkState, "public_url") }

// setPublicURL normalise puis enregistre l'adresse publique. Une chaîne vide
// efface le réglage (on repasse alors sur l'origine de la requête). Renvoie la
// valeur réellement stockée.
func setPublicURL(raw string) (string, error) {
	clean, err := normalizePublicURL(raw)
	if err != nil {
		return "", err
	}
	if err := putStr(bkState, "public_url", clean); err != nil {
		return "", err
	}
	return clean, nil
}

// normalizePublicURL ramène une saisie humaine à une origine canonique :
// "schéma://hôte[:port]", sans slash final. Pure, donc testable sans E/S.
//
// Les refus sont volontairement bavards : cette valeur est recopiée dans la
// configuration d'un autre logiciel, une erreur silencieuse s'y paierait en
// « ça ne marche pas » sans indice.
func normalizePublicURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil // effacement : ce n'est pas une erreur
	}
	// Sans schéma, url.Parse lit « exemple.fr:8090 » comme un schéma « exemple.fr ».
	// Quelqu'un qui tape un nom de domaine nu veut https.
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("adresse illisible : %s", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("adresse en %s:// — seuls http:// et https:// sont acceptés", scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("adresse sans nom de machine")
	}
	if u.User != nil {
		return "", fmt.Errorf("l'adresse ne doit pas porter d'identifiants (%s@…)", u.User.Username())
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("l'adresse ne doit porter ni paramètre ni ancre")
	}
	// Chemin : on tolère le « /v1 » que l'utilisateur recopie naturellement
	// depuis l'adresse affichée, et rien d'autre — le reste supposerait que le
	// reverse proxy réécrit les chemins, ce qu'on ne peut pas vérifier d'ici.
	switch strings.TrimSuffix(u.Path, "/") {
	case "", "/v1":
	default:
		return "", fmt.Errorf("l'adresse ne doit pas contenir de chemin (%s) : Loki sert /v1 à la racine de son port", u.Path)
	}
	host := u.Host
	if h, port, err := net.SplitHostPort(host); err == nil {
		if h == "" {
			return "", fmt.Errorf("adresse sans nom de machine")
		}
		if n, err := net.LookupPort("tcp", port); err != nil || n == 0 {
			return "", fmt.Errorf("port invalide : %s", port)
		}
	} else if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		// IPv6 littéral doit être entre crochets, sinon on ne sait pas où
		// s'arrête l'adresse et où commence le port.
		return "", fmt.Errorf("adresse IPv6 à écrire entre crochets : [%s]", host)
	}
	return scheme + "://" + host, nil
}

// requestBase renvoie l'origine par laquelle CETTE requête a atteint Loki :
// "schéma://hôte[:port]", sans slash final.
//
// L'hôte vient de r.Host, c'est-à-dire de l'en-tête Host du navigateur : le
// domaine réellement tapé, correct même derrière un reverse proxy qui ne
// réécrit pas Host. On n'honore PAS X-Forwarded-Host, qui élargirait la surface
// de confiance sans rien apporter ici.
//
// Le schéma, lui, a besoin de X-Forwarded-Proto : derrière un proxy qui termine
// le TLS, Loki ne voit qu'une requête en clair et annoncerait http:// pour une
// interface servie en https://. Ces en-têtes sont forgeables quand il n'y a pas
// de proxy devant — sans conséquence : le résultat n'est qu'une chaîne affichée
// à un propriétaire déjà authentifié, jamais une décision d'accès.
func requestBase(r *http.Request) string {
	scheme := "http"
	switch {
	case r.TLS != nil:
		scheme = "https"
	}
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
		first := strings.ToLower(strings.TrimSpace(strings.Split(fwd, ",")[0]))
		if first == "http" || first == "https" {
			scheme = first
		}
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host
}

// oaiBaseURL est l'adresse à coller dans un client tiers : l'adresse publique
// enregistrée si elle existe, sinon celle par laquelle la requête est arrivée.
func oaiBaseURL(r *http.Request) string {
	if u := publicURL(); u != "" {
		return u
	}
	return requestBase(r)
}
