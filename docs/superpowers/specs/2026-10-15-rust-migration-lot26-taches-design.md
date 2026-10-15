# Lot 26 — Tâches planifiées tenues par la façade

## Contexte

Une tâche planifiée tourne seule, sans utilisateur en face. Il en existe
deux sortes :

- **une tâche IA** : une consigne, exécutée par l'agent dans un fil
  éphémère ;
- **une tâche « script seul »** : un script du dossier protégé
  (`$LOKI_HOME/scripts`), lancé sans modèle. Sa sortie devient le
  compte-rendu.

Le Go les tient dans plusieurs fichiers :

- `tasks.go` : la base (`bkTasks`) et l'interrupteur maître ;
- `tasks_cron.go` : les fréquences ;
- `tasks_sched.go` : le planificateur, un tic par minute et une tâche due
  par tic ;
- `tasks_run.go` et `tasks_script.go` : l'exécution ;
- `web_tasks.go` : sept routes.

Les fréquences prennent quatre formes :

- `@every 2h` : intervalle glissant d'au moins une minute ;
- `@every 3d@9:30` : tous les N jours, à une heure murale ;
- `@once 2026-10-01 16:54` : une seule fois ;
- une expression cron à cinq champs.

L'heure murale est celle du fuseau de la tâche. Sans fuseau, ou avec un
fuseau inconnu, c'est l'heure locale du serveur.

## Le lot

Avec `LOKI_TASKS=rust` (expérimental), la façade (`rust/loki-rs/src/tasks`)
sert les routes `/api/tasks*` et fait tourner le planificateur, que le Go ne
démarre plus. Elle exécute elle-même les tâches script :

- le shell de l'agent (`run_shell`), dans le dossier de la discussion
  ouverte ;
- une limite de 10 min ;
- un arrêt par `/api/tasks/stop`.

Une tâche IA passe par le verrou de génération et les outils de l'agent, qui
restent au Go. La façade la lui fait exécuter par `/internal/v1/tasks/run`,
que le Go traite comme son propre planificateur l'aurait fait :

- preset épinglé ;
- projet forcé ;
- `RunAutonomous` ;
- état persisté ;
- notification.

Moteur occupé ou modèle pas prêt : la tâche ne tourne pas, son état ne bouge
pas, et le tic suivant la réessaie. Le Go rend la main à la fin du passage.

Les notifications des tâches script partent par la façade si elle tient les
notifications, sinon par le Go (`/internal/v1/push/send`).

Les fréquences demandent l'heure murale d'un fuseau IANA. `src/tz.rs` porte
ce que le Go fait de la zoneinfo du système :

- lecture des TZif v1 et v2, et de la règle POSIX qui prolonge les
  transitions ;
- `time.Local` (`$TZ`, `/etc/localtime`) ;
- `time.Date`, avec la normalisation des débordements ;
- les heures qui sautent ou qui se répètent, résolues comme le Go.

`tasks/cron.rs` porte `time.ParseDuration` du Go, messages d'erreur compris.

Côté Go :

- `facade_tasks.go` : le relais des routes reçues par le tunnel et les deux
  routes internes ;
- `web_server.go` : le planificateur n'est pas démarré ;
- `tasksNow` : l'heure des tâches, remplaçable en test.

## Correction de la façade

Les appels de la façade à l'API interne du Go étaient tous bornés à 10 s. Un
outil exécuté par le Go (`/internal/v1/tool/exec`) qui dure plus longtemps
était coupé :

- une page web lente ;
- le navigateur ;
- un serveur MCP tenu par le Go.

Le résultat partait en erreur alors que l'outil continuait. Ces appels sont
désormais bornés à 30 min. Une tâche IA exécutée par le Go est attendue sans
autre limite que 24 h.

## Ce que les mutations ont corrigé dans le port

Le premier passage des mutations a laissé survivre des cas que le journal ne
voyait pas. Confrontés au Go, plusieurs étaient de vrais écarts :

- **dates lointaines** : un instant en nanosecondes sur 64 bits s'arrête en
  2262. Le Go accepte `@once 2400-02-29 10:00`, ou un intervalle en jours
  qui mène au-delà. Les instants des fréquences passent sur 128 bits ;
- **intervalle en jours démesuré** : `@every 200000d` déborde dans le Go
  (`time.Duration`), qui rend une date passée. Le port déborde de même ;
- **l'heure qui saute** : `@every 1d@02:30` à Paris, le jour du passage à
  l'heure d'été, tombe à 3 h 30. Le Go avance ensuite de jour en jour depuis
  ce candidat (`AddDate`) et reste à 3 h 30. Le port repartait de 2 h 30 ;
- **compte-rendu tronqué** : le Go coupe à 4000 octets, au milieu d'un
  caractère s'il le faut, et chaque octet orphelin devient U+FFFD à
  l'encodage JSON. Le port reculait à une frontière de caractère ;
- **nom de fuseau** : le Go refuse tout nom qui contient `..`, pas seulement
  un composant `..`.

## Parité

`internal/loki/parity_tasks_test.go` (Linux) tire du Go un journal rejoué
par `tests/tasks.rs`. L'heure des tâches est fixée pas à pas ; les
identifiants tirés au hasard deviennent `{{ID1}}`, `{{ID2}}`… dans l'ordre où
ils apparaissent. Le journal couvre :

- **l'écran** :
  - scripts du dossier (sous-dossiers compris) ;
  - mode agent, interrupteur maître (rangé à « 0 » : pas suspendu) ;
  - projets, mémoire coupée, internet armé ;
  - la tâche en cours pendant un script ;
- **l'enregistrement** :
  - corps illisible, sans nom, sans consigne ;
  - script absent, dossier, remontée, chemin absolu, séparateur Windows,
    caractère interdit ;
  - chaque erreur de fréquence : vide, intervalle trop court, illisible ou
    sans unité, jours ou heure invalides, cron à 4 champs, hors bornes, en
    lettres, plage inversée, pas nul, jamais satisfait, date illisible ou
    déjà passée ;
  - chaque forme de fréquence, dans plusieurs fuseaux : Paris (dont l'heure
    qui saute), New York, Calcutta, fuseau inconnu ;
  - id inconnu gardé, projet inconnu remplacé par l'actif ;
  - modification avec ou sans changement de fréquence ;
- **la bascule, l'interrupteur maître, la suppression** ;
- **les lancements** :
  - tâche absente ;
  - tâche IA avec moteur occupé (409) ou modèle pas prêt (503) ;
  - script en succès, en échec, arrêté à la main ;
  - script qui échoue après un succès, puis se rétablit ;
  - tâche script rangée sans script, au nom refusé, au script disparu ;
- **le planificateur** :
  - tic suspendu, tic pendant une optimisation ;
  - une tâche IA due qui attend le modèle ;
  - une tâche due par tic, dans l'ordre des noms ;
  - la tâche « une fois » qui s'éteint ;
  - une tâche sans prochain passage (posé sans la lancer) ;
  - une fréquence devenue invalide ;
  - deux tâches de même nom (rangées par id).

Après chaque étape, le journal enregistre les tâches en base.

Autres tests :

- fuseaux et fréquences confrontés au Go :
  - Paris aux deux changements d'heure, New York, Calcutta ;
  - avant la première transition, pile à une transition, la table avant la
    règle prolongée ;
  - `$TZ` (vide, « : », chemin, nom), `/etc/localtime` ;
  - noms refusés, « UTC » sans base ;
  - mois hors bornes, années bissextiles, mois de 30 jours ;
  - `ParseDuration` et ses débordements, cron (plages, pas, bornes, à 78 et
    364 jours) ;
- le compte-rendu tronqué, les noms de script ;
- l'amorçage au démarrage (tâches actives sans prochain passage) ;
- la durée d'un passage, à une horloge qui avance ;
- la tâche IA en cours vue par l'écran ;
- la tâche IA lancée à la main part au Go (`tache_ia_executee_par_le_go`) ;
- le planificateur tourne à son pas sans demande ;
- sans `LOKI_TASKS`, les routes partent au Go ;
- le Go (`TestFacadeTaches`) :
  - les sept routes partent à la façade, et elles seules ;
  - `/internal/v1/tasks/run` sur une tâche IA sans modèle (occupée, rien
    d'enregistré) et sur une tâche script ;
  - `/internal/v1/push/send`.

## Mutations

79 mutations du port Rust, dont 77 tuées. Les survivantes du premier passage
l'ont été par les tests et les étapes ajoutés ci-dessus. Restent deux
équivalentes :

- le refus d'un nom de script absolu **après** nettoyage : sous Unix, un nom
  qui ne commence pas par `/` ne le devient pas (le contrôle du Go vaut pour
  Windows) ;
- `web_on` sans `internet_enabled` : la joignabilité refuse déjà un moteur
  Crawl4AI sans adresse, seul cas où les deux diffèrent.

## Écarts assumés

- Pendant une tâche IA, le Go force le projet de la tâche. Une tâche
  enregistrée sans projet à ce moment-là prend, côté façade, le projet actif
  rangé.
