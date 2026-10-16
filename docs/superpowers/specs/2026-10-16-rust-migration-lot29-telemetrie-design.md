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

Les complétions de la façade rejoignent l'anneau du Go :

- `chat/perf.rs` porte la partie « relevé » :
  - `perfRecFromStats` (complétion streamée) ;
  - `perfRecFromWire` (réponse non streamée, lue sans échec possible, avec
    les mêmes tolérances que `perfNum`) ;
  - `perfPrefix` (empreintes FNV-1a cumulées des messages envoyés).
- `TurnHost::perf` range une complétion. La boucle du tour l'appelle :
  - après chaque complétion lue, complète ou non, numérotée dans le tour ;
  - avec les empreintes seulement si elle est complète ;
  - pour une complétion coupée par un stop, rangée incomplète.

  La dernière statistique d'une complétion complète porte la nature, la
  perte de cache, ce qui s'est intercalé et l'alerte, comme celle du Go.
- La nature : `main` pour un tour, `verify` pour la vérification du mode
  Code, `compact` pour un résumé de compaction. Le résumé est rangé dès que
  sa réponse se lit, sans empreintes.
- Le Go (`/internal/v1/perf/record`) range le relevé comme les siens :
  numéro, instant, perte de cache, ce qui s'est intercalé. Il calcule
  l'alerte au seuil du preset actif (`perfLostAlertAt`), jamais pour un
  preset externe.

`/api/perf/summary` reste au Go, qui tient l'anneau.

## Tests

- `chat/perf.rs` :
  - relevé d'une réponse entière : nombres en chaîne, valeurs hors bornes,
    octet parasite, cache d'OpenAI, timings d'un autre type ;
  - empreintes cumulées, FNV-1a de référence ;
  - relevé au format du Go.
- La boucle du tour :
  - deux complétions numérotées, nature et totaux, empreintes qui se
    prolongent ;
  - dernière statistique avec la perte vue par le Go ;
  - complétion coupée rangée incomplète, sans statistique finale.
- Le vrai hôte (`tests/lot6.rs`) :
  - complétions d'un tour rangées par le Go, rattachées à la discussion ;
  - résumés de compaction (rangés si la réponse se lit, pas sinon).
- Le Go (`TestFacadePerf`) :
  - première complétion, perte et ce qui s'est intercalé, alerte ;
  - preset externe sans alerte, complétion incomplète ;
  - champs envoyés en trop ignorés, corps illisible, GET refusé.

## Écarts assumés

- Les empreintes sont calculées sur le JSON des messages tel que Rust
  l'écrit (sans l'échappement HTML du Go). Elles ne se comparent qu'entre
  complétions de la même discussion et de la même nature, toutes produites
  par la façade.
- Une complétion coupée par un stop est rangée sans ses compteurs partiels.
