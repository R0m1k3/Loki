# Lot 22 — backends llama.cpp et job des moteurs tenus par la façade

## Contexte

Le panneau Moteur s'appuie sur une famille de routes du Go :
- `web_llamacpp.go` : statut, vérification git, bascule fast/opt, job ;
- `backend_prebuilt.go` : binaires précompilés officiels ;
- `web_api.go` : backends custom et leur désinstallation ;
- `web_devices.go` : GPU vus par un moteur.

Elles partagent **un** job (`lcJob`) avec la mise à jour du moteur de l'image
(`web_engine.go`) et l'installation de Strata (`backend_strata.go`). L'entête
du job est en base (`state/build_job`), ses lignes dans
`$LOKI_HOME/loki-build.log`. Au démarrage, un job noté « en cours » est déclaré
interrompu (`lcRestore`).

## Le lot

Avec `LOKI_BACKENDS=rust` (expérimental), la façade (`rust/loki-rs/src/backends`)
sert les routes suivantes :

| Route | Rôle |
| --- | --- |
| `/api/llamacpp` | statut : dépôt git, binaire, moteur de l'image, précompilé, moteur téléchargé, plan de build, conseil, job |
| `/api/llamacpp/check` | `git fetch`, puis le retard sur origin |
| `/api/llamacpp/prebuilt/check` | dernière release officielle comparée à la version installée |
| `/api/llamacpp/prebuilt` | job : téléchargement, extraction (liens compris), VERSION, ménage, BIN, service arrêté puis relancé |
| `/api/llamacpp/use` | bascule fast/opt, refusée pendant une optimisation |
| `/api/llamacpp/job` | progression et lignes à partir d'un décalage |
| `/api/llamacpp/uninstall-custom` | désinstallation d'un backend custom |
| `/api/backends`, `/api/backends/custom` | les backends présents |
| `/api/backends/devices` | GPU vus par un moteur, avec un cache mémoire et un cache disque |

**Le job vit dans la façade** (`backends/job.rs`) : même entête en base, même
journal, même reprise au démarrage. Les jobs que le Go fait encore tourner lui
rapportent chaque étape par l'API interne `/internal/facade/v1/build-job/*`
(`start`, `line`, `phase`, `fail`, `done`, `commits`, `end`, `snapshot`).
Ces jobs sont :
- la compilation depuis les sources (`install`, `update`, `install-custom`,
  relayées au Go qui garde son refus 409 quand l'image fournit le moteur) ;
- la mise à jour du moteur de l'image ;
- Strata.

Côté Go (`facade_backends.go`), quand la façade tient le lot :
- `startLcJob`, `lcAppend`, `lcPhase`, `lcFail`, `lcDone`, `lcSetCommit` et
  `lcJobSnapshot` passent par la façade ;
- le garde de l'optimiseur reste au Go, avant le démarrage ;
- le Go ne restaure plus de job au démarrage ;
- les routes reçues par le tunnel sont relayées à la façade.

Pour arrêter et relancer le service, la façade pilote le moteur si elle le
tient (lot 16), sinon elle le demande au Go (`/internal/v1/service/action`,
avec `if_active`). Les sondes de la machine passent par un PATH et une racine
injectables (`backends/probe.rs`) :
- `nvidia-smi`, `nvcc`, `hipcc`, `/opt/rocm` ;
- `glslc`, `vulkaninfo`, `ldconfig` et les chemins de `libvulkan`.

## Correction du Go

`prebuiltVersionFormat` lisait le format comme 3ᵉ champ du marqueur VERSION.
Hors CUDA Windows, la version CUDA est vide : le marqueur
« `b300  fmt2` » n'a alors que deux champs. Le format n'était donc jamais
reconnu, et chaque vérification retéléchargeait la release sous Linux et
macOS au lieu de dire « déjà à jour ». Le format est désormais le dernier
champ, et la version CUDA ignore ce champ. Le port Rust suit.

## Parité

`internal/loki/parity_backends_test.go` (Linux) tire du Go un journal rejoué
par `tests/backends.rs`. Le journal est sauté si `/usr/local/cuda*` ou
`/opt/rocm` existent sur la machine.

Le journal contient :
- **le statut** dans tous ses états :
  - dépôt git reproductible (dates et identité fixées), retard connu ou vérifié, tête détachée ;
  - origine perdue, BIN dans un fork ;
  - précompilé versionné, moteur téléchargé, moteur de l'image ;
- **les backends custom** et chaque refus de désinstallation ;
- **la bascule fast/opt** ;
- **la dernière release** :
  - refus HTTP, tag vide, JSON illisible ;
  - aucun asset adapté ;
- **le job précompilé** :
  - installation, « déjà à jour », ancien format réextrait ;
  - optimisation en cours, GitHub qui refuse ;
  - archive absente, archive piégée (`../`), archive sans serveur ;
- **les GPU** :
  - liaison PCIe, mémoire complétée par nvidia-smi ;
  - cache, mémoire nulle non figée, repli sur le disque ;
  - moteur planté, tué ou non exécutable, pilote ancien, cartes de même nom ;
- **après chaque appel** : BIN, entête en base, journal, `devices.json` et l'arborescence de `backends/` et `engine/` ;
- **machine par machine** (dix fausses machines) : le plan de build, le conseil et la variante de release ;
- **le job pas à pas** :
  - phases déduites des lignes de build, troncature à 4000 lignes, vues à partir d'un décalage ;
  - écritures en base limitées à une toutes les 2 s ;
  - refus d'un second job ;
  - reprise au démarrage dans tous les cas : absent, illisible, fini, interrompu, journal vide ou trop long.

Autres tests :
- le Go (`TestFacadeBackends`) vérifie que ses jobs rapportent chaque étape à
  la façade, dans l'ordre, et que la compilation n'est pas relayée ;
- la façade (`api_interne_du_job`, `service_arrete_puis_relance`) vérifie
  l'API interne et le service arrêté puis relancé autour de l'installation.

## Écarts assumés

- Les erreurs réseau (GitHub injoignable) gardent le texte de `reqwest`.
- Quand un asset de la release a un champ de mauvais type, le message n'est
  pas celui du Go.
- `git fetch` lancé par `/api/llamacpp/check` pendant un job du Go n'écrit
  plus ses lignes dans ce job.
