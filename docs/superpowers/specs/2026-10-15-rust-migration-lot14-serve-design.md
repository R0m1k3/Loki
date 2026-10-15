# Lot 14 — `loki serve` composé en Rust

## Contexte

Depuis le lot 3, la supervision du moteur est en Rust : `loki-rs engine
start|stop|restart`. Mais elle lançait encore `loki serve` (Go), qui compose
la ligne de commande de llama-server avant de lui céder la place.

Cette ligne vit dans `backend_serve*.go`, environ 4 000 lignes. Chaque option
y a été réglée contre un défaut mesuré :
- couches GPU et placement automatique (`--fit`, `--fit-target`) ;
- threads et sonde de conteneur ;
- files de lancement CUDA ;
- second slot, parallélisme de tenseurs ;
- cache de prompts, points de reprise des hybrides ;
- décodage spéculatif (MTP, brouillon, n-grammes) ;
- garde-fous de fidélité, garde-fou de chargement (LOAD_GUARD) ;
- état des slots gardé à la bascule.

Lot 14 : `loki-rs engine serve` fait ce travail quand `LOKI_SERVE=rust`
(expérimental). Le superviseur le lance à la place de `loki serve`. Sans la
variable, rien ne change.

## Découpage

- `serve/` : port fonction par fonction. `mod.rs` contient
  `build_serve_args` et les outils de drapeaux. Les modules suivent les
  fichiers Go :
  - `threads`, `queues`, `expert`, `fidelity` ;
  - `side`, `split`, `cache`, `ckpt` ;
  - `spec`, `moe`, `persist`.

  Tout ce qui décide est pur.
- `serve/run.rs` porte `cmdServe`. Il lit la configuration, la clé d'API, les
  dossiers de modèles et le jeton de tentative de SPEC=auto par l'API interne
  du Go. Il sonde ensuite :
  - l'aide du moteur, `--version`, `--list-devices` (bornés dans le temps,
    avec les bibliothèques du moteur) ;
  - le GGUF ;
  - la RAM, la VRAM (nvidia-smi), le cgroup et le cpuset ;
  - les variables `LLAMA_ARG_*` et `GGML_*`.

  Il vérifie que le port est libre et écrit le marqueur de SLOT_PERSIST. Il
  garde la commande en base pour l'interface (`engine_cmdline`), puis fait
  `exec` du moteur.
- La façade passe au Go l'adresse de son API interne (`LOKI_GO_ADDR`), à côté
  du jeton. Le superviseur, lancé par le Go, en hérite, et `engine serve` aussi.

## Restent au Go

`loki serve` (Go) est lancé à notre place dans ces cas, à l'identique :
- un preset Strata (son propre serveur, lot à venir) ;
- le moteur d'essai de l'optimiseur (`LOKI_TUNE_TRIAL`) ;
- l'absence d'API interne (lancement à la main hors de la façade).

Restent aussi au Go les aperçus de l'éditeur de preset (`/api/preset/cacheram`,
`/api/preset/moe`, « Dupliquer en placement auto »). Ils viendront avec le lot
des presets.

## Écarts assumés

- Le message d'un brouillon (`MODEL_DRAFT`) introuvable cite l'erreur système
  au format Rust.
- Pas de cache de l'aide ni du build par binaire : `engine serve` ne compose
  qu'une ligne par lancement, le cache du Go ne servait qu'au process web.
- Le jeton de tentative de SPEC=auto est lu puis réécrit par l'API interne,
  sans transaction : le Go seul l'efface, une fois le moteur prêt.

## Parité

`internal/loki/parity_serve_test.go` tire du Go 1 872 lancements, chacun
avec sa ligne de commande, son environnement et ses notes. Les lancements
croisent :
- 12 machines (VRAM, RAM, GGUF dense, MoE, hybride, par couches, architecture
  exclue, brouillons, mémoire unifiée, conteneur à l'étroit) ;
- 112 presets ;
- 3 aides de moteur (récent, ancien, inconnu) ;
- 13 jeux de variables `LLAMA_ARG_*`, et des variables `GGML_*`.

Le journal comprend aussi les sondes pures :
- listes de CPU, cpuset et quotas, mémoire du cgroup ;
- sortie de nvidia-smi, `--list-devices`, build du moteur ;
- forme copiable de la commande ;
- verdicts de SPEC=auto ;
- drapeaux de chargement, taille d'état par jeton.

`tests/serve.rs` rejoue le tout contre `serve::build_serve_args`, argument
par argument. Une mutation du calcul du cache, des files CUDA ou des bornes
des n-grammes fait échouer le rejeu.

Deux essais de bout en bout lancent le vrai `loki-rs engine serve` contre un
faux Go et un faux llama-server. Ils vérifient :
- la ligne exécutée et le chemin des bibliothèques ;
- la commande gardée en base ;
- le refus d'un port pris, d'un modèle absent ;
- la sortie propre d'un preset externe ;
- la délégation au Go (sans API interne, essai de l'optimiseur, Strata).
