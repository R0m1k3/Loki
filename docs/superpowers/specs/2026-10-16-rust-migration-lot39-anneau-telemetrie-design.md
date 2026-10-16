# Lot 39 — Anneau de télémétrie tenu par la façade

## Contexte

Chaque complétion qui passe par le moteur est rangée dans un anneau
(`perf_log.go`). L'anneau :

- calcule la perte de cache d'une complétion contre la précédente de la même
  discussion et de la même nature ;
- nomme ce qui s'est intercalé (sous-agent, client externe…) ;
- signale une perte au-delà du seuil du preset (stats finales) ;
- nourrit `/api/perf/summary` et les poids de l'optimiseur.

Depuis le lot 29, la façade envoyait chaque complétion de ses tours au Go
(`/internal/v1/turn/perf`). C'était un aller-retour par complétion pour un
anneau tenu ailleurs.

## Le lot

**Rust** (`src/perfring.rs`, port de `perf_log.go`) :

- l'anneau : 5000 entrées, 128 discussions suivies, 500 entrées remontées au
  plus pour nommer ce qui s'est intercalé ;
- la perte : seulement si la complétion prolonge strictement la précédente
  (empreintes cumulées des messages), cache connu, complétion allée au bout ;
- la ligne `[perf]` (LOKI_PERF_LOG) ;
- le seuil d'alerte, dans le même ordre de priorité qu'au lancement :
  - `-cms` d'EXTRA_ARGS ;
  - `LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT` ;
  - `CKPT_MIN_STEP` ;
  - 2048 sur un modèle hybride ;
  - plus le micro-batch ;
- le résumé de `/api/perf/summary` (par nature, par profondeur, brouillon,
  dernier verdict de la sonde de gabarit).

`ChatShared` porte l'anneau. Les tours, les passes de vérification, les
résumés de compaction et le préchauffage de la façade y rangent leurs
complétions directement. Les marques (perte, ce qui s'est intercalé,
alerte) partent sur les stats finales comme avant.

La façade sert :

- `POST /internal/facade/v1/perf/record` : une complétion faite par le Go
  (sous-agent, tâche IA, bench, compaction et préchauffage de ses tours,
  client externe de `/v1`), rendue telle que rangée ;
- `POST /internal/facade/v1/perf/snapshot` : l'anneau, pour les poids de
  l'optimiseur ;
- `GET /api/perf/summary`.

**Go** (`facade_perf.go`) : quand la façade tient la conversation,
`perfRecord` range chez elle, et `perfSnapshot` relit son anneau
(optimiseur, contexte de `/internal/v1/tune/context`). `/api/perf/summary`
lui est remis (`facadeChatRoutes`). Façade injoignable : l'anneau du Go
prend le relais. Sans façade, ou la conversation rendue au Go
(`LOKI_CHAT=go`), rien ne change.

La route `/internal/v1/turn/perf` est retirée : plus personne ne l'appelle.

## Parité

`internal/loki/parity_perf_test.go` tire du Go un journal
(`tests/parity_perf/perf.json`). Quatorze complétions y sont rangées, avec
les empreintes de leurs messages ; elles couvrent :

- prolongement ;
- perte après un sous-agent et un client externe ;
- cache inconnu ;
- divergence ;
- autre discussion ;
- complétion coupée ;
- compaction, bench, vérification.

Pour chaque complétion, le journal garde ce qui est rangé, la ligne
`[perf]` et l'alerte. Il porte aussi le résumé, le résumé vide, et onze
seuils d'alerte. `src/perfring/tests.rs` le rejoue, avec l'anneau borné et
l'heure posée.

`tests/lot6.rs` :

- `cache_perdu_mesure_par_la_facade` : deux tours de la même discussion ;
  la perte est mesurée par la façade et signalée dans les stats finales ;
- `anneau_de_la_facade_pour_le_go` : ce que le Go confie (perte après un
  client externe), ce qu'il relit, le jeton exigé, `/api/perf/summary`
  servi par la façade ;
- les tests du lot 29 lisent désormais l'anneau de la façade.

`internal/loki/facade_perf_test.go` (`TestFacadePerfAnneau`) :

- complétion confiée à la façade ;
- anneau relu ;
- repli sur l'anneau du Go quand la façade est en panne ;
- route remise.
