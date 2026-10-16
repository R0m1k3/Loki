# Lot 37 — Outils task_* de l'agent en Rust

## Contexte

Depuis le lot 26, la façade tient les tâches planifiées (`LOKI_TASKS=rust`) :

- les routes ;
- le planificateur ;
- les tâches « script seul ».

Les quatre outils par lesquels le modèle gère lui-même ses tâches
(`tasks_tools.go`) restaient au Go, appelés par `/internal/v1/tool/exec` :

- `task_list` ;
- `task_create` ;
- `task_update` ;
- `task_delete`.

## Le lot

`src/tasks/tools.rs` porte les quatre outils, mêmes messages au caractère
près :

- **`task_list`** :
  - liste du projet actif, aperçu d'une ligne par tâche ;
  - compte-rendu coupé à 300 octets, un caractère coupé valant U+FFFD par
    octet comme à l'encodage JSON du Go ;
  - avec un id, la tâche seule et son compte-rendu complet ;
  - une tâche d'un autre projet est « introuvable » ;
- **`task_create`** :
  - tâche unique « dans N minutes » (durée tronquée comme
    `time.Duration(v)`) ou « à HH:MM » (le lendemain si l'heure est passée,
    à la même heure murale) ou « à AAAA-MM-JJ HH:MM » ;
  - fréquence validée ;
  - script vérifié dans le dossier protégé ;
  - tâche rattachée au projet actif ;
  - fuseau : celui du navigateur, sinon celui de la première tâche qui en a
    un ;
- **`task_update`** : modification partielle ; la prochaine exécution est
  recalculée sur une nouvelle fréquence, ou à la reprise d'une tâche qui
  n'en a pas ;
- **`task_delete`**.

Un projet disparu vaut le projet par défaut (`taskProjectOf`).

`GoTurnHost::exec_tool` les exécute quand la façade tient les tâches
(`ChatShared.tasks`, la même base que celle des routes). Sinon l'appel part
au Go comme avant.

Côté Go, les outils prennent l'heure des tâches (`tasksNow`) au lieu de
`time.Now`. Rien ne change en service ; le journal de parité peut ainsi la
fixer.

## Parité

`internal/loki/parity_tasks_tools_test.go` tire du Go un journal de 44
appels (`tests/parity_tasks/tools.json`). Chaque étape porte :

- l'heure, le fuseau, le projet actif, les projets connus ;
- les scripts présents ;
- une tâche posée telle quelle en base ;
- les arguments tels que le modèle les envoie ;
- le résultat tel qu'il le reçoit ;
- les tâches en base après coup.

Le journal couvre :

- l'heure du trou du changement d'heure ;
- une date passée ou impossible ;
- des arguments d'un autre type ;
- un script absent, hostile ou dossier ;
- le compte-rendu coupé au milieu d'un caractère ;
- le fuseau d'une tâche existante.

`tests/tasks.rs` (`parite_des_outils`) le rejoue contre le port Rust.

`tests/lot6.rs` (`outil_task_par_la_facade`) fait tourner un tour de la
façade qui crée une tâche. Il vérifie :

- rien ne passe par le Go ;
- la tâche est rangée au fuseau de l'utilisateur, dans le projet actif ;
- le résultat est celui que le modèle reçoit.
