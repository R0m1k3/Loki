# Lot 16 — pilotage du moteur tenu par la façade

## Contexte

Le Go pilotait le moteur depuis l'interface :
- `svcHandler` (`web_api.go`) pour `/api/start`, `/api/stop` et
  `/api/restart` ;
- `handleVramUnload` et `handleVramReload` (`web_vram.go`) pour rendre la
  carte graphique à la machine, puis la reprendre ;
- `serviceAction` (`sys_service.go`) : le verrou de l'optimiseur, puis, en
  conteneur, le superviseur Rust `loki-rs engine <action>` (lot 3,
  `sys_service_rust_linux.go`) ;
- `preflightEngine` : ce sans quoi le moteur ne peut pas démarrer, dit avant
  de le lancer.

Lot 16 : la façade tient ces routes quand `LOKI_ENGINE=rust`
(`enginectl.rs`, expérimental). Sans la variable, rien ne change.

| Route | Rôle |
|---|---|
| `/api/start`, `/api/stop`, `/api/restart` | pilotage du moteur ; réponse `{ok, out}` comme le Go |
| `POST /api/vram/unload` | arrête le moteur et la dictée, rapporte la VRAM rendue ; refusé pendant une réponse en cours sauf `{force: true}` |
| `POST /api/vram/reload` | relance le moteur après le contrôle d'avant démarrage ; `{already: true}` s'il tourne |

La façade lance elle-même `loki-rs engine <action>`, avec l'environnement que
le Go lui passait (`LOKI_GO_BIN`, `LOKI_HOME`, `LOKI_SERVICE`) et de quoi
joindre l'API interne du Go pour `loki-rs engine serve` (`LOKI_GO_ADDR`,
`LOKI_INTERNAL_TOKEN`). Sa dernière ligne d'erreur devient l'erreur rendue,
sans le préfixe du binaire. Un preset externe ne lance rien ; un redémarrage
arrête alors le moteur local.

Le bilan de VRAM suit `gpuSettle` :
- une lecture ratée est sautée, jamais comptée « 0 Mo » ;
- un palier ne conclut qu'une fois la baisse commencée ;
- les jauges du moniteur sont invalidées après coup.

## Quand la façade ne tient pas ce lot

La façade ne prend ce lot qu'en conteneur, avec son superviseur. Sous
systemd, ou avec `LOKI_ENGINE_SUPERVISOR=go`, `loki-rs` ne passe pas
`LOKI_ENGINE` au Go, qui garde les routes (décidé au démarrage, raison dans
le journal).

## Restent au Go

Le Go expose trois routes internes (`facade_engine.go`) :
- `POST /internal/v1/service/prestart` : avant tout démarrage, refus pendant
  une optimisation, puis ménage d'un verrou périmé (essai orphelin arrêté,
  application interrompue défaite). Le verrou de l'optimiseur suit le
  processus qui le tient, il reste au Go jusqu'au lot de l'optimiseur ;
- `GET /internal/v1/chat/generating` : la réponse en cours, quand le Go tient
  la conversation (sinon la façade lit la sienne) ;
- `POST /internal/v1/dictate/stop` : l'arrêt de la dictée, quand le Go la
  tient (sinon la façade arrête la sienne).

`GET /internal/v1/tune/guard` (lot 15) sert le refus 409 du rechargement.

Les autres appelants de `serviceAction` côté Go (bascule de preset,
optimiseur, mises à jour du moteur) passent toujours par le Go. Les deux voies
lancent le même superviseur, sur les mêmes fichiers PID et journal.

## Écarts assumés

- `engineNeedsStop` regarde aussi les états transitoires de systemd. En
  conteneur, seul cas où la façade tient ce lot, il se réduit au PID vivant.
- Un superviseur tué par un signal est décrit comme par le Go (« signal:
  killed ») pour les signaux courants, par son numéro sinon.

## Parité

`internal/loki/parity_engine_test.go` tire du Go :
- dix bilans de VRAM (palier avant la baisse, lectures ratées, VRAM en
  hausse…) ;
- la dernière ligne d'erreur du superviseur et le retrait des couleurs ;
- vingt-six installations passées au contrôle d'avant démarrage (preset
  externe, Strata, BIN absent ou relatif, MODEL absent, hors des dossiers,
  remontant, en tranches incomplètes…) ;
- dix réponses de route qui ne lancent rien (GET refusé avec `Allow: POST`,
  optimisation ou réponse en cours, moteur déjà arrêté, contrôle en échec).

`tests/enginectl.rs` rejoue le tout par la façade. Il vérifie aussi de bout en
bout, avec un faux superviseur et un faux nvidia-smi :
- démarrage, arrêt, redémarrage, refus pendant une optimisation ;
- échec du démarrage, preset externe ;
- déchargement chiffré, arrêt en échec, VRAM en hausse ;
- rechargement.

Une mutation de chaque règle fait échouer les tests.

Côté Go, `TestFacadeMoteur` vérifie le relais et les trois routes internes.
