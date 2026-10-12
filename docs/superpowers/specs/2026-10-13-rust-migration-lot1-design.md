# Migration de Loki vers Rust — lot 1 : façade Rust + mise à jour au démarrage

Date : 2026-10-13
Statut : validé (design), à planifier

## Contexte

Loki est aujourd'hui un monobinaire Go (`internal/loki`, ~65 000 lignes hors
tests) qui sert une UI embarquée (~26 000 lignes JS/CSS/HTML) et pilote
llama.cpp, Strata, MCP, le mode Code, etc. L'objectif à terme est de le
réécrire en Rust. Une réécriture d'un bloc ne livrerait rien pendant des
semaines : la migration est **progressive**, par lots, avec Rust en production
dès le premier lot.

Second besoin : aujourd'hui, un `docker restart` relance la même image. Le
conteneur doit prendre la dernière version de Loki **au prochain démarrage**,
sans `docker compose pull` manuel.

## Architecture de cohabitation (« strangler fig »)

```
:8090 ──► loki-rs (Rust, axum/tokio)
            ├─ routes portées → traitées en Rust
            └─ tout le reste  → proxy HTTP / SSE / WebSocket
                                 → loki web (Go) sur 127.0.0.1:8091
```

- Nouveau workspace Cargo dans `rust/`, binaire `loki-rs`, compilé en statique
  (`x86_64-unknown-linux-musl`) : indépendant de la glibc de l'image.
- Go écoute uniquement en local sur 8091 (nouvelle option d'adresse d'écoute
  de `loki web`). Le port public reste 8090 : compose et reverse proxy
  inchangés.
- Au lot 1, **aucune route n'est portée** : la façade proxifie tout. Elle sert
  de socle aux lots suivants.
- Authentification (clé de pilotage, `LOKI_TRUSTED_HOSTS`) : reste à Go tant
  qu'il sert la route. Go ne fonde aucune décision d'accès sur l'IP source
  (vérifié : ni `RemoteAddr` ni `X-Forwarded-For` dans `internal/loki`), donc
  passer par 127.0.0.1 n'élargit rien. La façade transmet `Host`,
  `Authorization`, `Cookie`, `Origin`, `Sec-Fetch-*`, `X-Forwarded-Proto`
  **sans les altérer**, et retire seulement les en-têtes hop-by-hop.
- **Sécurité** : la façade **efface `X-Loki-Via`** de toute requête entrante.
  Cet en-tête, posé par le tunnel du relais, dispense du contrôle d'hôte
  anti-DNS-rebinding (`crossSiteReject`). Aujourd'hui l'écoute HTTP directe de
  Go ne l'efface pas : sans clé de pilotage, une page en DNS rebinding peut le
  forger. La façade ferme ce trou dans le conteneur ; un correctif Go séparé
  le ferme pour `loki web` hors conteneur. Test dédié.
- Flux : pas de timeout d'écriture, pas de bufferisation des réponses
  `text/event-stream` (chat SSE, `/v1` en streaming), upgrade WebSocket relayé
  (`/api/node/ws` et autres). Pas de recompression : Go gère déjà gzip.
- Le relais `loki link` reste entièrement côté Go jusqu'à son propre lot.

### Démarrage et arrêt

`loki-rs serve` lance `loki web 8091` (Go) comme processus enfant, attend
qu'il réponde, puis ouvre 8090. Si l'enfant meurt, la façade le relance
(backoff borné) et répond 502 avec un message clair entre-temps. Sur SIGTERM,
la façade transmet le signal à l'enfant et attend sa fin (moteur llama-server
arrêté proprement par Go, comme aujourd'hui).

## Mise à jour au démarrage

### Publication (CI)

À chaque push sur `main`, en plus de l'image GHCR, un workflow publie la
release GitHub **`rolling`** (pré-release, assets écrasés) :

- `loki-linux` — binaire Go (déjà `CGO_ENABLED=0`)
- `loki-rs-linux` — binaire Rust musl
- `SHA256SUMS` — format `sha256sum` (celui que `loki update` sait déjà lire)
- `manifest.json` — `{"commit": "<sha>", "image_abi": <entier>}`

La release `rolling` n'est jamais marquée `latest` : `loki update` (machines
non conteneurisées) continue de suivre les tags `vX.Y.Z`.

### ABI d'image

Le Dockerfile déclare `ENV LOKI_IMAGE_ABI=1`. On l'incrémente (Dockerfile et
source du manifeste dans le même commit) quand le code exige un changement de
l'image : nouvelle dépendance système, Python/Node, chemin du moteur, etc.
Un binaire publié avec une ABI différente de celle de l'image n'est **jamais**
installé : il ne pourrait pas tourner correctement.

### Au boot (`loki-rs self-update`)

Appelé par `docker-entrypoint.sh`, avec le `loki-rs` de l'image :

1. `LOKI_AUTO_UPDATE=0` → ne fait rien.
2. Télécharge `manifest.json` (timeout global 20 s). Erreur réseau → étape 6.
3. `image_abi` ≠ `LOKI_IMAGE_ABI` → journal :
   « nouvelle version nécessitant une nouvelle image : docker compose pull »,
   étape 6.
4. `commit` identique à `/data/bin/VERSION` → rien à faire, étape 6.
5. Télécharge les deux binaires en `/data/bin/*.part`, vérifie chacun contre
   `SHA256SUMS`, `chmod +x`, renomme atomiquement, écrit `VERSION` en dernier.
   Toute erreur → `.part` supprimés, rien remplacé.
6. Fin. Code de sortie 0 dans tous les cas : le boot n'est jamais bloqué.

L'entrypoint choisit ensuite les binaires : `/data/bin/loki` et
`/data/bin/loki-rs` s'ils existent, sont exécutables **et** que
`/data/bin/ABI` égale `LOKI_IMAGE_ABI` ; sinon ceux de l'image. Les deux
binaires viennent toujours de la même source (jamais un Go de l'image avec un
Rust de `/data/bin`). Le chemin Go choisi est passé à la façade
(`LOKI_GO_BIN`) et utilisé pour les appels `loki config` de l'entrypoint.

Limite assumée : seuls les binaires Loki se mettent à jour ainsi ; llama.cpp
se met à jour via l'UI (existant), le reste de l'image via un pull signalé par
l'ABI.

## Organisation du code Rust

```
rust/
  Cargo.toml            workspace
  loki-rs/
    src/main.rs         CLI : serve | self-update | version
    src/proxy.rs        façade HTTP/SSE/WebSocket
    src/child.rs        supervision du processus Go
    src/update/         manifeste, téléchargement, SHA256SUMS, installation
```

Dépendances : `tokio`, `axum`, `hyper`/`hyper-util`, `reqwest` (rustls,
sans OpenSSL — musl), `sha2`, `serde`/`serde_json`, `clap`.

## Tests

- Unitaires : parse de `SHA256SUMS` (entrées `*nom`, nom absent), décision
  d'installation (ABI différente, même commit, `LOKI_AUTO_UPDATE=0`),
  installation atomique (échec au milieu → rien remplacé).
- Intégration : façade devant un faux backend axum —
  SSE reçu morceau par morceau (pas en bloc), WebSocket écho,
  en-têtes `Host`/`Authorization`/`Origin` transmis intacts, `X-Loki-Via`
  entrant effacé, 502 si backend absent.
- Mise à jour : faux serveur de release local (URL de base injectable par
  variable d'environnement de test).
- CI : `cargo fmt --check`, `cargo clippy -D warnings`, `cargo test` dans un
  job dédié ; la CI Go (gofmt + staticcheck + tests) reste inchangée.

## Feuille de route des lots suivants (hors périmètre de ce spec)

| Lot | Contenu |
|---|---|
| 2 | Fichiers statiques de l'UI ; lecture de la config via une API interne Go |
| 3 | Supervision de llama-server |
| 4 | Client LLM, `/v1` OpenAI, flux chat SSE |
| 5 | Outils de l'agent, MCP, mode Code/LSP |
| 6 | Modèles et téléchargements, Strata, optimiseur, bench |
| 7 | Mémoire, tâches, dictée, relais, CDP |
| 8 | Store bbolt → redb, retrait de Go |

Chaque lot aura son propre spec, plan et PR.
