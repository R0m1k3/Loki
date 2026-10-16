# Lot 43 — Jobs d'arrière-plan du mode Code dans la façade

## Contexte

En mode Code, le modèle lance un serveur de dev, un watcher ou un build long
avec `bash_bg`. Il en suit la sortie avec `bash_tail`, qui sait aussi
l'arrêter. Les deux outils partaient au Go (`/internal/v1/tool/exec`), qui
gardait les processus. C'étaient les derniers outils shell du tour encore au
Go, `bash` étant porté depuis le lot 7.

## Le lot

`src/tools/jobs.rs` porte `code_jobs.go`.

**`bash_bg`** :

- refus avant tout lancement :
  - commande vide ;
  - commande dangereuse (même politique que `bash`) ;
  - optimisation en cours (`/internal/v1/tune/guard`) ;
- journal `.loki/jobs/<id>.log` dans le dossier de la discussion (le
  contexte des outils du Go) ;
- numéros `job1`, `job2`… ;
- shell de la plateforme, sans le jeton interne dans l'environnement ;
- le processus survit au tour ;
- code de sortie du Go : -1 pour un processus tué.

**`bash_tail`** :

- 40 lignes par défaut, 200 au plus ; un nombre est tronqué comme
  `int(float64)` ;
- état « en cours » ou « terminé (exit N) » et durée arrondie à la
  seconde ;
- sortie vide signalée ;
- fin gardée en caractères ;
- octets invalides en U+FFFD un à un ;
- `kill:true` arrête le job et relit aussitôt, comme le Go : l'état peut
  encore dire « en cours ».

Les erreurs d'écriture ont le texte du Go. `paths::go_mkdir_all` reproduit
`os.MkdirAll` : l'erreur nomme l'élément du chemin qui a échoué.

`ChatShared.jobs` (les jobs du processus) sert les tours :
`GoTurnHost::exec_tool` exécute les deux outils. Sans contexte d'outils,
`bash_bg` passe au Go comme avant.

Les jobs d'une discussion supprimée s'arrêtent avec elle
(`GoSessionHooks::dropped`, `stopConvJobs`).

L'optimiseur refuse de mesurer tant qu'un job tourne. Côté Go,
`bgJobsRunning` ajoute ceux de la façade quand elle tient la conversation
(`POST /internal/facade/v1/bgjobs` → `{running}`) ; façade injoignable :
les siens seulement.

## Parité

`internal/loki/parity_jobs_test.go` (Linux) tire du Go un journal de 24
étapes (`tests/parity_jobs/jobs.json`). Chaque étape porte le résultat (pid
et durées masqués) et les journaux présents. Il couvre :

- commande vide, refusée ;
- sortie des deux flux, lignes demandées (nulles, en texte, tronquées) ;
- arrêt d'un job fini ;
- job inconnu, sans id ;
- sortie en erreur sans rien d'écrit ;
- job long, en cours, arrêté (état relu masqué), puis fini à -1 ;
- quarante lignes par défaut, deux cents au plus ;
- sortie démesurée en caractères multi-octets ;
- octets invalides.

`tests/jobs.rs` le rejoue contre le port.

D'autres tests complètent le journal :

- `tests/lot6.rs` (`job_arriere_plan_par_la_facade`) : un tour de la façade
  lance un job dans le dossier de la discussion sans passer par le Go ;
  supprimer une autre discussion n'y touche pas, la sienne l'arrête ;
- `anneau_de_la_facade_pour_le_go` : le compte des jobs par l'API interne ;
- `TestFacadeJobsArrierePlan` : le Go les ajoute aux siens, façade en panne
  comprise ;
- tests unitaires : refus de l'optimiseur, discussion supprimée, dossier
  impossible.
