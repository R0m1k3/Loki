# Migration Rust — lot 2 : UI statique, authentification, `/api/ping`

Date : 2026-10-13 · Statut : validé · Prérequis : lot 1 (façade `loki-rs`).

## But

Faire passer en Rust les premières routes réelles, et surtout poser le socle
dont toutes les routes `/api/*` suivantes auront besoin : l'authentification.

## Périmètre

1. **UI statique servie par Rust**, embarquée dans le binaire à la
   compilation depuis `internal/loki/ui/` (même source que le `go:embed`) :

   | Route | Content-Type | Cache-Control | Autre |
   |---|---|---|---|
   | `/`, `/index.html` | `text/html; charset=utf-8` | `no-store, max-age=0` | |
   | `/marked.min.js` | `application/javascript` | `public, max-age=86400` | |
   | `/sw.js` | `application/javascript` | `no-store, max-age=0` | `Service-Worker-Allowed: /` |
   | `/manifest.webmanifest` | `application/manifest+json` | `public, max-age=3600` | |
   | `/fonts/<nom>.woff2` | `font/woff2` | `public, max-age=31536000, immutable` | |

   GET et HEAD seulement (les autres méthodes restent relayées au Go).
   Compression gzip si le client l'accepte, sauf polices (comme
   `web_gzip.go`) ; `Vary: Accept-Encoding` sur les types compressibles.
   Version compressée calculée une fois, à la première demande.

2. **API interne Go** `GET /internal/v1/auth` → `{"web_key": "…", "version": "…"}`.
   - Servie **uniquement sur l'écoute TCP directe** de `loki web`
     (enveloppe autour du mux dans `cmdWeb`) : le tunnel du relais sert le mux
     nu et ne la voit jamais.
   - Exige l'en-tête `X-Loki-Internal` égal à `LOKI_INTERNAL_TOKEN` (≥ 32
     caractères, comparaison en temps constant). Jeton absent, trop court ou
     faux → 404 (rien ne révèle l'existence de la route).
   - Base illisible → 503 : la façade fermera l'accès (jamais ouvert par
     défaut sur une panne d'E/S, même règle que `requireWebAuth`).
   - La façade tire un jeton aléatoire de 32 octets à chaque démarrage et le
     passe au Go par l'environnement.

3. **Authentification de pilotage en Rust** (`auth.rs`), port à l'identique de
   `requireWebAuth` + `crossSiteReject` + `localHostName` + `checkBearer` :
   - clé relue à **chaque requête** (API interne) : un changement de clé prend
     effet immédiatement ;
   - API interne en erreur (Go absent, 503, réponse illisible) → 503
     `{"error":"configuration illisible — réessaie dans un instant"}` ;
   - refus site tiers / hôte non local sans clé → 403 `{"error":"<raison>"}`
     (mêmes messages que le Go) ;
   - Bearer absent ou faux → 401 `{"error":"non autorisé"}` +
     `WWW-Authenticate: Bearer realm="loki"` ;
   - `X-Loki-Via` n'est jamais pris en compte côté Rust (effacé à l'entrée,
     le trafic du tunnel ne traverse pas la façade).

4. **`/api/ping` en Rust** : `{"ok":true,"service":"loki","version":"<version Go>"}`
   après authentification — première route `/api/*` servie par Rust.

5. **Durcissement de la façade** : `/internal/*` venant de l'extérieur → 404 ;
   en-tête `X-Loki-Internal` entrant effacé avant relais.

## Hors périmètre

Toutes les autres routes `/api/*`, `/v1`, le flux chat : lots suivants.

## Tests

- Go : route interne (sans jeton → 404, mauvais jeton → 404, bon jeton → 200
  avec clé et version, autres chemins relayés au mux).
- Rust : cas de `TestCrossSiteReject` repris un pour un ; `local_host_name`
  (IP, IPv6 entre crochets, nom sans point, suffixes locaux, hôte de
  confiance) ; Bearer ; statiques (types, cache, gzip, HEAD, police inconnue
  → relayée) ; intégration façade + faux backend interne : ping 200 sans clé,
  401 avec clé sans Bearer, 200 avec bon Bearer, 503 si l'API interne tombe,
  403 rebinding, `/internal/v1/auth` depuis l'extérieur → 404.
- Bout en bout local : vrai `loki` Go derrière la façade, `loki set-web-key`,
  ping avec et sans clé.
