# Lot 18 — hub des projets tenu par la façade

## Contexte

Le hub des projets vivait dans le Go :
- `handleProjects` et `projectDTOs` (`web_projects.go`) ;
- les mutations de `projects.go` : `createProject`, `renameProject`,
  `setProjectDesc`, `deleteProject` ;
- `projectSwitch` (`chat_sessions.go`).

Lot 18 : la façade tient `/api/projects` quand `LOKI_PROJECTS=rust`
(`projects_api.rs`, expérimental). Sans la variable, rien ne change.

| Action | Rôle |
|---|---|
| `GET` | les projets, le plus ancien d'abord, avec leurs discussions, pages (`.md` visibles, index exclu) et trackers comptés ; le projet actif |
| `create` | slug translittéré (`Réseau NAS` → `reseau-nas`), unique (`-2`, `-3`…), `projet` à défaut, 40 octets au plus ; nom de 60 runes au plus ; dossier `memory/<slug>` et son index amorcé |
| `rename` | le libellé seul : slug, chemins et discussions ne bougent pas |
| `desc` | 2 000 runes au plus, vide = effacée |
| `switch` | la conversation passe sur le projet (la plus récente de ses discussions, ou une neuve) |
| `delete` | discussions, trackers et mémoire du projet ; le projet actif est réassigné ; le dernier projet est indestructible |

Le registre reste dans la base (`state/projects`, écrit comme `json.Marshal`
avec le HTML échappé), la mémoire de chaque projet dans `memory/<slug>`.

## Restent au Go

Le projet actif passe toujours par le Go (`/internal/v1/project/activate`),
qui en garde une copie en RAM pour la mémoire, les outils et les tâches.

Les discussions sont à qui tient la conversation.

Quand la façade la tient (`LOKI_CHAT=rust`), elle bascule et supprime
elle-même. Avant de supprimer le projet actif, elle enregistre sa discussion
et en ouvre une du premier projet restant.

Sinon le Go garde deux routes internes (`facade_projects.go`) :
- `POST /internal/v1/project/switch` : `projectSwitch` (discussion du projet
  quitté enregistrée, plus récente du projet rejoint ouverte, ou une neuve) ;
- `POST /internal/v1/project/drop-convs` : les discussions du projet
  supprimées (`convDelete`, fichiers et résultats compris).

Une génération en cours refuse la bascule et la suppression (409). La façade
lit la sienne, ou celle du Go (`/internal/v1/chat/generating`, lot 16).

## Écarts assumés

- Pendant une tâche planifiée, le Go voit comme actif le projet de la tâche ;
  la façade lit le projet actif enregistré.
- Pendant un tour de la façade, une bascule vers un projet inconnu répond 409,
  comme quand le Go tient la conversation. Le Go, relayant à la façade,
  répondait 400 (« projet introuvable ») dans ce cas.

## Parité

`internal/loki/parity_projects_test.go` part d'une base amorcée :
- pages visibles, cachées, hors `.md`, dans un sous-dossier ;
- discussions de Générale, d'avant les projets, d'un projet à venir et d'un
  projet disparu ;
- trackers, dont `alphabet/x` qui ne doit pas tomber avec `alpha`.

Il tire du Go 40 appels, avec après chacun le registre, le projet actif,
l'arbre `memory/`, les trackers et l'index des discussions. Ils couvrent :
- créations (doublons, translittération, sans lettres, longues, vides) ;
- renommages et descriptions (longues, coupées sur une espace, effacées) ;
- bascules (sur l'actif, inconnue, vers un projet sans discussion, pendant
  une génération) ;
- suppressions (de l'actif, inconnue, pendant une génération, jusqu'au
  dernier projet) ;
- actions inconnues, corps vide ou illisible, erreur de type, méthode
  refusée.

`tests/projects_api.rs` rejoue le tout par la façade, contre un faux Go qui
bascule et supprime les discussions comme le vrai. La bascule et la
suppression quand la façade tient la conversation sont vérifiées de bout en
bout. Une mutation de chaque règle fait échouer les tests.

Côté Go, `TestFacadeProjets` vérifie le relais et les deux routes internes.
