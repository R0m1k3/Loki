# Lot 35 — Routes de base par le tunnel du relais

## Contexte

La façade sert toujours neuf routes, sans variable (`api.rs`) :

- `/api/ping` ;
- `/api/status` ;
- `/api/config` ;
- `/api/engine/cmdline` ;
- `/api/model/layers` ;
- `/api/paths` ;
- `/api/ram` ;
- `/api/vram` ;
- `/api/service/log`.

Mais le tunnel du relais (accès distant, E2E) sert le mux du Go tel quel.
`withFacadeChat` ne remet à la façade que les routes des lots. Un utilisateur
distant tombait donc encore sur les handlers Go de ces neuf routes : deux
implémentations à garder d'accord.

## Le lot

- Go (`facade_core.go`) : une table des routes de base, remises à la façade
  par `withFacadeChat` dès qu'elle tourne (adresse et jeton interne
  présents), sans variable. Façade muette : 502 « façade indisponible ».
  Sans façade (`LOKI_FACADE=0`), le Go les sert lui-même.
- Rust (`api.rs`) : une requête relayée par le Go (jeton interne) a déjà été
  authentifiée par lui. Ni clé ni hôte local ne sont exigés de nouveau,
  comme pour les routes des lots ; la version du Go est lue quand même pour
  `/api/ping`.

## Tests

- Go (`facade_core_test.go`) :
  - les neuf routes relayées avec le jeton ;
  - une route du Go gardée ;
  - façade muette ;
  - sans façade, servies ici.
- Rust (`tests/lot2.rs`) :
  - relayée par le Go avec un hôte distant et une clé posée : servie par la
    façade, pas renvoyée au Go ;
  - sans jeton, ou avec un faux : refusée.

## Reste à faire

L'interface statique et `/v1` passent encore par le Go sur le tunnel. Ses
handlers sont gardés pour `LOKI_FACADE=0`.
