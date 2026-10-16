# Lot 21 — mise à jour de Loki tenue par la façade

## Contexte

`/api/update` (version disponible) et `/api/update/apply` (téléchargement et
remplacement du binaire) vivaient dans `sys_update.go`. En conteneur
(`LOKI_CONTAINER=1`), le Go n'y fait rien. L'image se met à jour par Docker,
et les deux routes le disent (`containerUpdateNote`) :
- `/api/update` : `{current, available: false, note}` ;
- `/api/update/apply` : 409 `{ok: false, error: note}`.

## Le lot

La façade ne tourne qu'en conteneur. Elle tient ces deux routes quand
`LOKI_UPDATE=rust` (`sysupdate.rs`, expérimental) et `LOKI_CONTAINER=1`.
Sinon `loki-rs` ne passe pas la variable au Go, qui garde le téléchargement
et le remplacement du binaire hors conteneur.

La version annoncée est celle du Go (`/internal/v1/auth`). Les binaires de
la façade suivent toujours `loki-rs self-update`.

## Parité

`internal/loki/parity_update_test.go` tire du Go les quatre réponses en
conteneur (GET et POST sur chaque route). `tests/sysupdate.rs` les rejoue par
la façade, version du Go comprise.
