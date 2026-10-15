# Lot 11 — fichiers de la discussion tenus par la façade

## Contexte

Les fichiers rangés par discussion vivaient dans le Go :

- `web_upload.go` : dépôt, téléchargement, image ;
- `web_files.go` : panneau Fichiers ;
- `web_files_zip.go` : archive ;
- `web_thumb.go` et `web_upload_orient.go` : vignettes ;
- `chat_convfiles.go` : chemins.

Lot 11 : la façade les sert quand `LOKI_FILES=rust` (`convfiles.rs`,
expérimental ; sans la variable rien ne change).

| Route | Rôle |
|---|---|
| `POST /api/chat/upload` | dépôt en morceaux base64 (8 Mio), fichier de 1 Gio au plus, espace disque vérifié, `.part` renommé à la fin sous un nom sûr et unique |
| `GET /api/chat/files` | panneau : dossiers d'abord puis du plus récent au plus ancien, tailles récursives (liens non suivis), vue « hors discussion » |
| `POST /api/chat/file/delete` | suppression ; refus de la racine et du dossier des discussions |
| `GET /api/chat/file` | fiche, tranche base64 (tunnel E2E) ou brut en pièce jointe (plage d'octets) |
| `GET /api/chat/image` | type reniflé sur le contenu, en ligne sous une CSP fermée ; vignette JPEG `thumb=<px>` redressée (EXIF) et réduite par moyennage |
| `GET /api/chat/files/zip` | archive construite à la fiche, puis servie en tranches ou en brut |

Le dossier de la discussion active reste décidé par le Go : la façade le lui
demande à chaque requête par la nouvelle route `/internal/v1/conv/workspace`
(`{dir, root, id, title}`). Il connaît la discussion active, qu'il tienne la
conversation ou que ce soit la façade. Les liens des anciens messages
(`captures/<id>/…`, `uploads/…` à la racine) restent résolus comme avant.

`facade_files.go` remet les routes du tunnel à la façade. Il ne vide plus les
`.part` d'envois au démarrage : la façade le fait au sien.

## Écarts assumés

- Octets des vignettes JPEG : l'encodeur diffère. Les dimensions, le
  redressement et la réduction (algorithme porté pixel pour pixel) sont les
  mêmes.
- Horodatage des entrées `.zip` en UTC.
- Une seule plage d'octets par requête de téléchargement : le Go en acceptait
  plusieurs, en multipart.

## Parité

`internal/loki/parity_files_test.go` tire du Go :

- 10 noms de dépôt et 6 slugs ;
- 6 en-têtes EXIF ;
- les 8 orientations d'une image de 5×3 et 4 réductions, pixel pour pixel ;
- 10 reniflages ;
- 42 appels de route sur une arborescence fixe, dates comprises :
  - panneau, téléchargement, image et vignette ;
  - dépôt en un morceau, en deux morceaux, envoi expiré, fichier vide,
    base64 illisible ;
  - suppressions ;
  - archive de la discussion et « hors discussion », contenu comparé
    entrée par entrée ;
  - les fichiers restants après chaque POST.

`tests/convfiles.rs` rejoue le tout par la façade, plus une plage d'octets,
un lien vers l'extérieur (refusé, non compté) et le nom d'archive tiré du
titre. Deux mutations (ordre du panneau, suffixe des doublons) font échouer
le rejeu.
