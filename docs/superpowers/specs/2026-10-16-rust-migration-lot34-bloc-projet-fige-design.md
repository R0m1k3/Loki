# Lot 34 — Bloc projet figé (PROJ_SNAPSHOT) dans le tour de la façade

## Contexte

Le contexte du projet part en tête du premier message utilisateur :

- description ;
- index mémoire ;
- trackers ;
- consignes du dépôt ;
- avec la clé, la date et le dossier de travail.

Reconstruit à chaque tour, il change dès qu'une page est créée ou qu'un
tracker reçoit un point. Tout ce qui le suit dans le prompt est alors
recalculé.

`PROJ_SNAPSHOT` (`chat_projsnap.go`) garde une copie figée du bloc par
discussion. Les changements arrivent en `<context_update>` en tête du
nouveau message utilisateur, rangés tels quels dans l'historique. Le bloc
n'est rafraîchi que quand le début du prompt change de toute façon :
compaction, système ou outils, clé du moteur, projet, mode, trop de mises à
jour accumulées.

C'était le dernier réglage qui gardait la conversation au Go.

## Le lot

L'instantané est déjà persisté avec la discussion (`ConvState::proj_snap`),
et la compaction le remet à zéro comme le Go. Il manquait la décision, qui
lit le contexte du projet : elle reste au Go.

### Go (`facade_projsnap.go`)

- `/internal/v1/turn/view`, avec la clé et l'agent :
  - reçoit l'instantané de la discussion (`snap`) ;
  - rend la tête du tour (prompt du preset, puis le bloc figé, ou le vivant
    s'il faut rafraîchir) et le préambule sans date ni dossier
    (`envInCtx`) ;
  - rend aussi `projsnap` : rafraîchir ou non, la mise à jour à poser,
    l'instantané à ranger, la clé et l'empreinte du système du tour.
- `dry` (préchauffage) : la même décision, rien à ranger.
- `/internal/v1/turn/projsnap/refresh` : le bloc vivant et l'instantané qu'il
  devient (`projSnapTurn.refresh`).
- Plus aucun réglage ne garde la conversation : `facadeChatBlocker` et la
  décision prise une fois par processus disparaissent. Le Go la remet dès
  que `LOKI_CHAT=rust`, l'adresse de la façade et le jeton sont là.

### Rust

- `ChatService::assemble_turn`, partagé par le tour et la relance du builder
  du mode Code :
  - rafraîchi : instantané neuf, blocs `<context_update>` retirés de
    l'historique ;
  - mise à jour : posée en tête du dernier message utilisateur
    (`prependCtxUpdate`), dans la discussion et dans la requête ; l'état
    annoncé ne bouge que si elle a trouvé où se poser ;
  - sans la clé : la discussion qui porte encore des blocs en est nettoyée,
    comme avant.
- `TurnHost::project_refresh` : après une compaction ou une réduction en
  plein tour, le bloc vivant remplace le figé (`replaceProjectHead`) et les
  mises à jour sont retirées. L'instantané posé devient celui de la
  discussion à la fin du tour ou de la passe. Le retrait d'images gardées ne
  touche pas au début du prompt : pas de rafraîchissement.
- Le préchauffage demande la vue en `dry`. Il garde les mises à jour de
  l'historique tant que le bloc n'est pas rafraîchi, comme le vrai tour.

## Tests

- Go (`facade_projsnap_test.go`) :
  - la séquence assemblée par la façade égale celle de `turnView`, outils et
    instantané compris ;
  - une page ajoutée : bloc gardé, mise à jour égale à celle de `turnView`,
    état annoncé suivi ;
  - sans changement : rien à poser ;
  - `dry` sans rien à ranger ;
  - clé changée : rafraîchi, bloc vivant en préchauffage ;
  - rafraîchissement en plein tour : bloc vivant, instantané à la clé du
    tour.
- Go (`facade_chat_test.go`) : avec toutes les clés opt-in posées, la façade
  tient la conversation ; sans adresse ou sans `LOKI_CHAT`, le Go et
  pourquoi.
- Rust, `chat/compact` : mise à jour en tête (texte, parties, refus) ; bloc
  projet remplacé à sa place (cas du Go).
- Rust, boucle du tour : rafraîchi après une compaction et une réduction ;
  pas après un retrait d'images gardées.
- Rust, vrai hôte (`tests/lot6.rs`) :
  - instantané rangé ;
  - mise à jour posée dans la requête et l'historique (marque retirée à
    l'envoi) ;
  - rafraîchissement qui retire les mises à jour ;
  - compaction en plein tour : bloc vivant envoyé, instantané rangé ;
  - préchauffage en `dry`.
