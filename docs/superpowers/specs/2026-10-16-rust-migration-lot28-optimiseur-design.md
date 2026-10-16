# Lot 28 — Optimiseur tenu par la façade

## Contexte

L'optimiseur sans perte (`backend_tune*.go`) cherche des réglages du moteur
plus rapides **sans rien changer à ce que le modèle calcule** : même
contexte, mêmes slots, mêmes types de cache, même échantillonnage. Il
procède ainsi :

1. Il arrête le vrai moteur.
2. Il lance chaque essai sur une COPIE de la configuration :
   - posée dans `LOKI_HOME/tune/run/<essai>/config.env`, jamais
     `config.env` ;
   - composée par « loki serve » à blanc ;
   - puis lancée sur 127.0.0.1 et un port libre.
3. Il mesure chaque essai par le bench complet, à une profondeur fixe.
4. Il garde ce qui gagne au-delà du bruit.
5. Il relance le vrai moteur.

Rien n'est écrit dans un preset sans un geste de l'utilisateur : dans une
copie, ou dans le preset lui-même — moteur redémarré, sondé, et ancienne
version rétablie si la sonde échoue.

Cinq routes :

- `POST /api/tune` lance (202) ;
- `GET /api/tune/status` dit la phase, les essais, l'ETA, puis le résultat ;
- `POST /api/tune/cancel` annule ;
- `POST /api/tune/apply` écrit le résultat, sur un clic (`copy` ou
  `preset`) ;
- `GET /api/tune/last` rend le dernier résultat d'un preset, et si le preset
  ou le moteur ont changé depuis.

Le moteur, pendant une optimisation, appartient aux essais. Le verrou est un
**fichier** de `LOKI_HOME` (`tune.lock`), créé de façon exclusive, que tout
le monde lit :

- le Go, dans tous ses refus (`tuneGuard`) ;
- « loki tune » en ligne de commande ;
- désormais la façade.

## Le lot

Avec `LOKI_TUNE=rust` (expérimental, Unix seulement), la façade
(`rust/loki-rs/src/tune`) sert les cinq routes et déroule l'optimisation :

- `tune/core.rs` : la partie pure, portée à l'octet près :
  - découpe et réécriture d'`EXTRA_ARGS` ;
  - réglages par clé ou par drapeau ;
  - essais de chaque étape selon la machine et les cases cochées ;
  - garde-fous : cœur de la ligne, dénature, clés permises ;
  - diff et réécriture du preset ;
  - journal d'un essai ;
  - score et départage ;
- `tune/lock.rs` : le verrou, **au format du Go** :
  - contenu du fichier, clé de prise, binaires comparés par leur nom ;
  - identité d'un processus comme `procIdentity` l'écrit : champ 22 de
    `/proc/<pid>/stat`, préfixé de l'identifiant du démarrage ; `ps` ailleurs ;
  - écriture d'un bloc ;
  - rendu neutre avant retrait ;
  - ménage par déplacement-relecture ;
- `tune/job.rs` : les routes, le job, les essais et l'application.

**La reprise reste au Go.** Sa reprise (`tuneRecoverWatch`, toutes les
30 s) lit le verrou de la façade et la reconnaît vivante, par la même
identité. Si la façade meurt en plein essai, le Go écarte son verrou comme
celui d'un « loki tune » tué :

- l'essai orphelin est arrêté (identité complète vérifiée) ;
- une application interrompue est défaite ;
- le moteur est relancé s'il tournait.

La façade fait le même ménage quand elle trouve un verrou périmé en prenant
le sien.

**Les essais restent des « loki serve » du Go**, lancés par la façade avec
`LOKI_TUNE_TRIAL` (et `LOKI_TUNE_DRYRUN` à blanc), dans leur propre groupe de
processus. Le groupe est arrêté en sortie, quoi qu'il arrive. L'essai est
noté dans le verrou avant de charger quoi que ce soit.

Le verrou de génération reste celui de la conversation du Go. La façade le
prend par le **bail du moteur**, avec le motif `tune` :

- le Go note alors `benching` et donne aux autres le refus de l'optimiseur ;
- changer de discussion ne rompt pas le bail ;
- un stop du chat, un Reset ou un bail perdu annulent l'optimisation.

Le Go prête à la façade ce qu'il est seul à voir
(`/internal/v1/tune/context`) :

- **le refus du processus web** : un benchmark (le sien ou celui de la
  façade), une génération, des jobs d'arrière-plan du mode code ;
- **l'état du vrai moteur** (`engineNeedsStop`) ;
- **la télémétrie**, d'où viennent les poids d'un tour type.

Il prête aussi le démarrage et l'arrêt du moteur **sans le refus de
l'optimiseur** (`/internal/v1/service/action`, `os`), puisque c'est la
façade qui tient le verrou. Si la façade pilote elle-même le moteur
(`LOKI_ENGINE=rust`), elle passe par son superviseur.

Le preset est réécrit et appliqué par les fonctions des presets
(`save_applying`, mode `apply` : la configuration, sans redémarrage). Le
redémarrage, la sonde et le retour arrière sont ceux de l'optimiseur.

La ligne de commande (`loki tune`) reste au Go.

Hors Unix, la façade ne tient pas l'optimiseur. Elle passe alors
`LOKI_TUNE` vide au Go, qui le garde et ne relaie rien.

## Parité

`internal/loki/parity_tune_test.go` tire du Go un journal
(`tests/parity_tune/core.json`) rejoué par `tests/tune_core.rs`. Il couvre :

- toute la partie pure ;
- le **verrou** : contenu du fichier octet pour octet, clé de prise,
  comparaison des binaires ;
- le **résultat** tel qu'enregistré et montré (listes vides écrites `null`
  comme une tranche nil du Go) ;
- la vérification **après application** (la configuration est-elle l'essai
  retenu ?).

`tests/tune.rs` déroule de vraies optimisations, essais compris : le binaire
de test joue « loki serve ».

- À blanc, il écrit la ligne de commande de l'essai.
- Pour de vrai, il sert un faux llama-server dont le prefill est d'autant
  plus rapide que le micro-lot est petit.

Les tests couvrent :

- **une optimisation entière** :
  - bail « tune » et verrou au nom de la façade ;
  - vrai moteur arrêté puis relancé sans le refus de l'optimiseur, dictée
    coupée ;
  - quatre essais composés, lancés, mesurés, l'un retenu ;
  - copies de configuration sur une adresse privée ;
  - résultat enregistré par preset et rendu par `/api/tune/last` ;
- **l'application** :
  - dans une copie (rien d'autre ne bouge) ;
  - dans le preset (réécrit, appliqué, moteur redémarré et sondé) ;
  - refusée sur un preset changé depuis la mesure ;
- **une sonde qui échoue** (contexte perdu) : preset et configuration
  rétablis, moteur redémarré dessus, sauvegarde faite avant d'écrire ;
- **les essais refusés** : à la composition, au chargement (mémoire
  insuffisante) ; la référence qui ne se compose pas ; la relance qui
  échoue, dite dans l'erreur ;
- **l'annulation**, par la route et par le stop du chat ;
- **les refus d'entrée**, avec les messages du Go :
  - preset externe, Strata, aucun preset actif ;
  - jobs du mode code, moteur occupé, génération en cours ;
  - optimisation d'un autre processus (et `elsewhere` dans le statut) ;
- **un verrou périmé** :
  - l'essai orphelin arrêté seulement si son identité concorde ;
  - une application interrompue défaite ;
- les routes sans la façade.

Les tests du verrou du Go sont portés (`tune/tests.rs`) :

- prise, refus, essai noté, rendu ;
- propriétaire mort ;
- verrou à notre PID qui n'est pas l'un des nôtres ;
- illisible neuf ou vieux ;
- ménage jamais sur un verrou vivant ;
- binaire remplacé ;
- identité d'un processus ;
- arrêt du groupe.

Côté Go, `TestFacadeTune` vérifie :

- les cinq routes partent à la façade, et elles seules ;
- le bail « tune » : refus, bascule de discussion, libération ;
- le contexte : au repos, pendant l'optimisation, pendant un bench ;
- le démarrage et l'arrêt sans le refus de l'optimiseur.

## Écarts assumés

- Une sortie à blanc qui échoue est lue dans un fichier où vont les deux
  sorties (comme `CombinedOutput`), mais une erreur de lecture de
  `launch.json` ou d'une requête garde le texte de Rust.
- Un niveau de raisonnement appris pendant les essais n'est pas transmis au
  chat du Go quand le Go tient la conversation.
