# Lot 27 — Banc d'essai tenu par la façade

## Contexte

Le banc d'essai mesure le moteur local tel que le chat s'en sert, sur
`/v1/chat/completions`, avec le gabarit, le raisonnement et
l'échantillonnage du preset (`llm_bench.go`, `llm_bench_job.go`). Il a deux
modes :

- **rapide** : un préchauffage jeté, puis la ligne courte (~2000 jetons de
  prompt, ~300 générés) ;
- **complet** : la ligne courte, puis une discussion à profondeur réelle —
  un prefill à froid de D jetons, et trois tours qui y ajoutent chacun ~2048
  jetons neufs. La reprise du cache est lue dans la réponse, jamais
  supposée.

Il tourne en tâche de fond derrière quatre routes :

- `POST /api/bench` lance (202 et un id) ;
- `GET /api/bench/status` dit la phase, puis le résultat ou l'erreur ;
- `POST /api/bench/cancel` annule ;
- `GET /api/bench/last` rend la dernière mesure enregistrée.

Pendant la mesure, le bench tient le verrou de génération de la
conversation. Un message, une tâche planifiée ou un client `/v1` reçoivent
le refus du bench. Seule une mesure complète est enregistrée : la
dernière, et celle du preset actif.

## Le lot

Avec `LOKI_BENCH=rust` (expérimental), la façade (`rust/loki-rs/src/bench`)
sert les quatre routes et déroule le protocole contre le moteur :

- `bench/mod.rs` : le protocole, porté du Go à l'octet près. On y trouve :
  - les mêmes corpus : la prose du Go et l'interface web embarquée, de la
    même source ;
  - les mêmes requêtes ;
  - le même calcul de profondeur, de reprise du cache et de débit ;
  - les mêmes messages d'erreur ;
- `bench/job.rs` : le job, les routes et l'enregistrement (`last_bench`,
  `bench_presets`, avec l'empreinte de configuration du Go).

Le verrou de génération reste celui de la conversation du Go. La façade le
prend par le **bail du moteur**, avec le motif `bench` :

- le Go note alors `benching` et donne aux autres le refus du benchmark ;
- changer de discussion ne rompt pas le bail ;
- `benchRunning()` le voit aussi : les clients `/v1`, le préchauffage et
  l'optimiseur attendent ;
- un stop du chat côté Go (signalé au renouvellement), un Reset ou un bail
  perdu annulent le bench.

Le Go prête aussi à la façade :

- **le travail annexe** (`/internal/v1/engine/sidejob`) : à la fin, le slot
  du bench est effacé si c'est sans risque, comme pour un bench du Go ;
- **la télémétrie** (`/internal/v1/bench/perf`) : la trace du bench dans
  `perf_log`.

Le port, la clé de l'API locale et le second slot (`SIDE_SLOT`) viennent de
`/internal/v1/oai/route`. Les niveaux de raisonnement appris sont ceux de la
conversation de la façade si elle la tient, sinon ceux du bench.

La ligne de commande (`loki bench`) reste au Go.

## Correction des tests du Go

Trois tests du préchauffage échouaient dix minutes par jour, avant minuit :
le préchauffage se tait alors (`prewarmNearMidnight`), et ils lisaient
l'heure réelle. L'heure de ce garde-fou devient remplaçable (`prewarmNow`),
et les tests la fixent à midi.

## Parité

`internal/loki/parity_bench_test.go` (Linux) tire du Go un journal rejoué
par `tests/bench.rs` contre le même faux moteur (`fakeBenchEngine`, porté).
Chaque requête reçue par le moteur est comparée ; les longs textes (corpus)
sont réduits à leur taille et à leur empreinte. Le journal couvre :

- **les routes** : statut et dernière mesure sans bench, GET refusés,
  annulation sans bench ;
- **les refus au lancement** : preset externe, optimisation en cours, modèle
  pas prêt, génération en cours, second lancement pendant un bench ;
- **les échecs dans le job** : moteur occupé par un autre (`/slots`), preset
  Strata ;
- **les mesures** :
  - mode rapide, puis mode complet (profondeur, trois tours, reprise du
    cache, brouillon) ;
  - contexte trop petit (profondeur sautée), contexte inconnu du moteur
    (celui du preset) ;
  - paramètres bornés, petits paramètres, corps illisible ;
- **les échecs d'une mesure** :
  - préchauffage, ligne courte, prefill à froid ou tour en profondeur en
    erreur ;
  - réponse sans timings ;
- **le raisonnement** : niveau refusé par le gabarit puis rejoué, puis le
  niveau appris envoyé d'emblée ;
- **budget et annulation** : budget d'une requête dépassé (profondeur
  partielle, non enregistrée), annulation pendant la profondeur ;
- **après chaque étape** : la dernière mesure et les mesures par preset.

Autres tests :

- ceux du Go (`llm_bench_test.go`), portés (`bench/tests.rs`) : profondeur,
  types de cache, requête, morceaux du corpus, reprise du cache, relectures
  du disque, empreinte de l'enregistrement ;
- la réponse du moteur (timings, cache, brouillon, nombres illisibles),
  `Duration.String` du Go, les flottants écrits à la Go ;
- la façade : le bail « bench », le travail annexe (slot servi ou non), la
  trace, la clé de l'API locale, le second slot, le stop du chat et le bail
  perdu, les routes sans la façade ;
- le Go (`TestFacadeBench`) :
  - les quatre routes partent à la façade, et elles seules ;
  - le bail « bench » : refus, bascule de discussion, libération,
    expiration ;
  - le travail annexe et la trace.

## Mutations

84 mutations du port Rust, dont 81 tuées. Les survivantes du premier passage
l'ont été par les tests ajoutés :

- `bench/tests.rs` : la tranche pile à la fin du corpus, un tour sans
  attendu à côté d'un tour mesuré, les débits nuls rendus par le moteur
  (recalculés), le niveau de raisonnement rejoué une seule fois, le
  préchauffage et la ligne courte d'un `run` rapide (paramètres par défaut,
  64 jetons de préchauffage au moins, code ajouté juste au-delà de la
  prose), la profondeur arrêtée (contexte plein, corpus épuisé, réponse du
  tour précédent reprise, note de reprise selon le modèle hybride) ;
- `tests/bench.rs` : un second lancement pendant la prise du bail, le preset
  relu au départ du job (devenu externe, ou optimiseur lancé entre-temps),
  la mesure enregistrée sous le preset ACTIF ;
- le journal : un prompt juste au-delà de la prose.

Restent trois équivalentes :

- l'annulation vérifiée avant d'envoyer : déjà annulé, le `select!` rend
  l'annulation à son premier tour, avant que la requête ne parte ;
- la progression d'un job remplacé : un seul bench à la fois, un job fini ne
  rapporte plus rien ;
- le PID nul du moteur : `/proc/0` n'existe pas, la lecture échoue de toute
  façon.

## Écarts assumés

- Une erreur réseau vers le moteur garde le texte de `reqwest` après le
  préfixe du Go (`Post "url": …`).
- Un niveau de raisonnement appris par le bench de la façade n'est pas
  transmis au chat du Go, quand le Go tient la conversation (et
  inversement) : la première requête paie le refus, puis la traduction est
  retenue.
