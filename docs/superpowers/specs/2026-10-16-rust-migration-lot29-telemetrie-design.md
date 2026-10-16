# Lot 29 — Télémétrie des complétions de la façade

## Contexte

Le Go range chaque complétion dans un anneau en mémoire (`perf_log.go`).
Pour chacune, il note :

- combien du prompt venait du cache, combien a été recalculé ;
- à quelle vitesse ;
- ce que le décodage spéculatif a gardé.

Il expose des agrégats sur `/api/perf/summary`. Il calcule aussi la **perte
de cache** d'une complétion : ce que le cache aurait dû reprendre de la
précédente complétion de la même discussion, et n'a pas repris. Il nomme ce
qui s'est intercalé entre les deux, et signale la perte à l'interface
au-delà d'un seuil tiré du preset.

Quand la façade tient la conversation (`LOKI_CHAT=rust`), ses complétions
n'apparaissaient nulle part. Le résumé des performances ignorait les tours ;
l'optimiseur tirait ses poids d'une télémétrie vide ; l'interface ne
recevait jamais la perte de cache.

## Le lot

Les complétions de la façade rejoignent l'anneau du Go, par un seul point
d'entrée, `/internal/v1/turn/perf`. Il est arrivé sur `main` en même temps que
ce lot (« Conversation tenue par Rust : télémétrie et requêtes en vol comme
le Go »), et la fusion n'en garde qu'un.

- `TurnHost::perf_record` range une complétion. La boucle du tour l'appelle :
  - après chaque complétion lue, complète ou non, numérotée dans le tour
    (`perfIter`) ;
  - avec les empreintes seulement si elle est complète (`perf_prefix`,
    FNV-1a cumulé des messages envoyés) ;
  - pour une complétion coupée par un stop, rangée incomplète.

  La dernière statistique d'une complétion complète porte ce que le Go en
  dit : la nature, la perte de cache, ce qui s'est intercalé et l'alerte.
- La nature, posée par l'hôte :
  - `main` pour un tour et pour les corrections du builder du mode Code ;
  - `verify` pour la passe de vérification isolée ;
  - `compact` pour un résumé de compaction, rangé dès que sa réponse se lit,
    sans empreintes.
- `chat/perf.rs` porte ce que la façade relève elle-même : la réponse entière
  d'un résumé (`perfRecFromWire`, sans échec possible, avec les mêmes
  tolérances que `perfNum`), traduite en statistiques d'une complétion.
- Le Go range le relevé comme les siens (`perfRecFromStats`) : numéro,
  instant, perte de cache, ce qui s'est intercalé. Il calcule l'alerte au
  seuil du preset actif (`perfLostAlertAt`), jamais pour un preset externe.
- La sonde du gabarit n'en part pas : la façade la tient (lot 23) et la lance
  elle-même après une complétion complète de forme normale. Le Go lui demande
  son verdict (`facadeTplCaps`).

`/api/perf/summary` reste au Go, qui tient l'anneau.

## Tests

- `chat/perf.rs` :
  - relevé d'une réponse entière : nombres en chaîne, valeurs hors bornes,
    octet parasite, cache d'OpenAI, timings d'un autre type ;
  - empreintes cumulées, FNV-1a de référence.
- La boucle du tour :
  - deux complétions numérotées, totaux, empreintes qui se prolongent ;
  - dernière statistique avec la nature et la perte vues par le Go ;
  - complétion coupée rangée incomplète, sans statistique finale.
- Le vrai hôte (`tests/lot6.rs`) :
  - complétions d'un tour rangées par le Go, rattachées à la discussion ;
  - résumés de compaction (rangés si la réponse se lit, pas sinon).
- Le Go (`web_internal_turnperf_test.go`) :
  - complétion rangée dans l'anneau ;
  - perte et ce qui s'est intercalé, alerte ;
  - preset externe sans alerte, complétion incomplète sans empreintes ;
  - nature d'un résumé gardée, un tour par défaut ;
  - corps illisible, GET refusé.

## Écarts assumés

- Les empreintes sont calculées sur le JSON des messages tel que Rust
  l'écrit (sans l'échappement HTML du Go). Elles ne se comparent qu'entre
  complétions de la même discussion et de la même nature, toutes produites
  par la façade.
- Une complétion coupée par un stop est rangée sans ses compteurs partiels.
