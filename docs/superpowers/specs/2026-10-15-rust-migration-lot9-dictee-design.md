# Lot 9 — dictée vocale tenue par la façade

## Contexte

La dictée vocale vivait dans le Go :

- `dictate_config.go` : réglages ;
- `dictate_models.go` : catalogue Parakeet, téléchargement et extraction ;
- `dictate_server.go` : supervision de `sherpa-onnx-offline-websocket-server`
  et protocole WebSocket ;
- `web_transcribe.go` : routes.

Lot 9 : la façade la tient quand `LOKI_DICTATE=rust` (expérimental ; sans la
variable rien ne change).

## Ce que sert la façade (`rust/loki-rs/src/dictate.rs`)

| Route | Rôle |
|---|---|
| `POST /api/transcribe` | un WAV (≥ 1000 octets, ≤ 32 Mio) entre, le texte sort ; modèle absent : 503 `{downloading, pct}` et téléchargement lancé (relancé après un échec, qui est dit) |
| `GET\|POST /api/dictate/config` | modèle (validé contre le catalogue) et réactivité ; enregistrer arrête le serveur, qui repart au prochain mot avec le nouveau modèle |
| `GET /api/dictate/models` | catalogue trié par taille, présence des quatre fichiers |
| `GET\|POST /api/dictate/models/download` | un téléchargement à la fois, progression sur les octets reçus |
| `GET /api/dictate/state` | serveur allumé (et depuis quand), modèle, présence, téléchargement |
| `POST /internal/facade/v1/dictate/stop` | (le Go, jeton interne) arrêt sans attendre derrière un démarrage |

Même mécanique que le Go :

**Archive `.tar.bz2`**
- Décompressée au fil du téléchargement, dans un dossier `.part` renommé à la fin.
- Entrées absolues ou qui remontent refusées, liens ignorés.

**Serveur de transcription**
- Lancé à la demande sur un port libre de la boucle locale, GPU masqués, sans
  le jeton interne dans son environnement.
- Prêt dès que sa socket répond.
- Une mort au démarrage est rapportée avec sa cause et la dernière ligne du
  journal ; un processus qui ne répond pas fait échouer le démarrage au bout
  de 2 minutes.
- Éteint après dix minutes d'inactivité.
- `shutdown_fast` tue le processus qui démarre au lieu d'attendre le verrou.

**Protocole**
- En-tête de 8 octets (fréquence, nombre d'octets), échantillons float32 en
  trames de 10 Kio, une trame texte en retour.
- Annotations (`[musique]`, `(rires)`) écartées.

## Côté Go (`facade_dictate.go`)

- `withFacadeChat` remet les routes du tunnel et de la redistribution E2E à la
  façade. Si elle ne répond pas : 502 « dictée indisponible : … ».
- `asrShutdownVite` passe par la façade quand elle tient la dictée : son
  serveur n'est pas un enfant de ce process. Cela couvre la libération de la
  VRAM et l'arrêt du moteur principal par l'optimiseur.

## Écarts assumés

- Messages d'erreur de transport (WebSocket, HTTP) et de décompression au
  format des crates Rust.
- Fin de connexion sans réponse : « lecture du résultat : connexion fermée ».

## Parité

`internal/loki/parity_dictate_test.go` tire du Go ce journal :

- treize WAV : mono, bloc `LIST` intercalé, stéréo moyenné, flottant 32 bits,
  données tronquées, impaires, vides, et les refus ;
- les chemins d'archive ;
- les marqueurs ;
- les réglages par défaut ;
- les arguments du serveur ;
- les adresses des modèles ;
- la présence ;
- treize appels de route.

Le tout est rangé dans `tests/parity_dictate/dictate.json` et rejoué par
`tests/dictate.rs`, les routes par la façade. Une mutation (premier canal au
lieu de la moyenne) fait échouer le rejeu.

`tests/dictate.rs` couvre aussi :

- le protocole contre un faux serveur WebSocket (en-tête, découpage
  10240 + 10240 + 3528, marqueurs, réponse illisible, serveur injoignable) ;
- l'extraction (lien ignoré, entrée qui remonte refusée sans rien laisser,
  archive corrompue) ;
- le téléchargement depuis un faux serveur (100 %, HTTP 404, modèle inconnu,
  un seul à la fois) ;
- la supervision d'un faux sherpa-onnx (script Python qui parle le
  protocole) :
  - transcription de bout en bout par la façade ;
  - même serveur réutilisé ;
  - arrêt demandé par le Go, et refusé sans le jeton ;
  - réglage changé ;
- les démarrages ratés : sortie en code 3 avec sa dernière ligne, serveur
  muet, arrêt « vite » qui tue le démarrage, binaire introuvable, modèle
  absent.
