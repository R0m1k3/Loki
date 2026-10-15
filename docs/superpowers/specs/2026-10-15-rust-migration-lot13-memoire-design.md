# Lot 13 — mémoire tenue par la façade

## Contexte

La mémoire de Loki vivait dans le Go :
- `chat_memory.go`, `mem_index.go`, `mem_io.go` et `mem_fsutil.go` pour les pages ;
- `moveMemPage` (`projects.go`) pour le déplacement entre projets ;
- `tracker.go` et `web_tracker.go` pour les trackers ;
- les routes du mode agent.

Lot 13 : la façade tient ces routes quand `LOKI_MEM=rust` (`memory.rs`,
expérimental). Sans la variable, rien ne change.

| Route | Rôle |
|---|---|
| `GET /api/mem?name=` | contenu d'une page (modèle de page neuve sans nom) |
| `POST /api/mem/save` | crée, écrase ou renomme une page ; l'index suit |
| `POST /api/mem/delete` | supprime une page ; sa ligne d'index part avec elle |
| `POST /api/mem/move` | déplace une page vers un autre projet ; les deux index suivent |
| `GET /api/agent` (alias `/api/tools`, `/api/skills`) | mode agent, compactage, mode mémoire, liste des pages |
| `POST /api/agent/toggle` (et alias) | mode agent (`state/agent`) |
| `POST /api/agent/compact` | compactage automatique (`COMPACT`) |
| `GET|POST /api/memory` | mode mémoire du projet actif, repli sur `MEM_MODE` |
| `GET|POST /api/tracker` | panneau des trackers : liste, détail, ajout, modification, suppression, renommage, déplacement |

### Disque et base

Les pages restent des fichiers `memory/<projet>/<nom>.md`, et l'index
`MEMORY.md` reste en clair. Le Go continue de les lire et de les écrire pour
sa propre boucle : outils `mem_*`, index injecté au modèle. Les écritures
restent sûres, comme dans le Go :
- un `.bak` de la version précédente est gardé ;
- le fichier est écrit dans un temporaire, synchronisé, puis renommé ;
- le contenu est relu et comparé octet par octet.

Les trackers restent dans le bucket `tracker`, sous la clé
`<projet>/<slug>`. Le registre des projets (`state/projects`) est lu tel
quel. Le mode mémoire y est écrit avec l'échappement HTML de
`encoding/json`, pour garder les mêmes octets en base.

### Chiffrement

La clé de la mémoire ne quitte pas le Go :
- une page qui porte le magic `LOKIMEMv1` est ouverte par
  `/internal/v1/crypt/open` ;
- toute écriture de page ou de tracker passe par `/internal/v1/crypt/seal`,
  qui rend le clair tel quel quand le chiffrement est coupé.

Quand la mémoire est verrouillée :
- les pages restent listées avec le titre « 🔒 (chiffré — mémoire
  verrouillée) » ;
- les trackers illisibles sont tus ;
- aucune écriture ne retombe en clair.

### Messages d'erreur JSON

Ces routes renvoient au client le message d'erreur de décodage du Go.
`gojson.rs` reproduit `json.NewDecoder(r.Body).Decode(&req)` :
- seule la première valeur compte ;
- `EOF` et `unexpected EOF` ;
- les erreurs du scanner (`invalid character …`) ;
- les erreurs de type, champs anonymes compris ;
- la casse des noms de champs, et l'allocation des pointeurs.

`gojson.rs` est mutualisé pour les lots suivants.

## Restent au Go

- Le coffre et le chiffrement (`/api/mem/encrypt|decrypt|unlock|lock`).
- La santé, les instantanés, la sauvegarde.
- Le hub des projets (`/api/projects`) : sa bascule recharge la discussion.
- La bascule temporaire de projet d'une tâche planifiée. Elle n'existe qu'en
  RAM du Go : la façade sert toujours le projet actif rangé.

## Écarts assumés

- Un corps JSON syntaxiquement valide mais qui fait échouer serde (UTF-8
  invalide, nombre hors limites) donne le message de serde.
- Une date d'un tracker qui tombe dans un trou d'heure d'été est résolue par
  `mktime`.

## Parité

### Décodage JSON

`internal/loki/parity_gojson_test.go` tire du Go 81 corps décodés : champs
et message d'erreur. `tests/gojson.rs` les rejoue.

### Mémoire

`internal/loki/parity_mem_test.go` tire du Go :
- les noms de pages, les titres, les slugs et les modes mémoire ;
- 36 dates de trackers, lues dans un fuseau fixe (UTC+2) ;
- 102 appels de route, avec après chacun le dossier `memory/` complet
  (`.bak` compris) et les clés touchées en base (mode agent, compactage,
  mode mémoire, projets, trackers).

Ces appels couvrent :
- les pages : création, renommage, réenregistrement qui perd l'accroche,
  noms refusés, fichiers cachés et dossiers ignorés ;
- le déplacement, avec ses six refus ;
- la bascule de projet ;
- le mode agent et le compactage ;
- le mode mémoire, y compris un projet actif inconnu ;
- les trackers : ajout, modification, suppression, renommage, déplacement,
  valeurs illisibles et événements nuls.

`tests/memory.rs` rejoue le tout par la façade, avec le même masquage des
identifiants et des instants « maintenant ».

Une mutation de chaque harnais fait échouer son rejeu.

Un essai de bout en bout simule le chiffrement par le faux Go :
- les pages sont écrites scellées et relues en clair ;
- l'index reste en clair ;
- les trackers sont scellés ;
- une fois la mémoire verrouillée, rien n'est réécrit en clair.
