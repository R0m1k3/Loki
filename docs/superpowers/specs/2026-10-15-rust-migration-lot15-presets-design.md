# Lot 15 — presets tenus par la façade

## Contexte

Les presets vivaient dans le Go :
- `backend_presets.go` pour l'écriture ;
- les handlers de `web_api.go` et `web_external.go` ;
- les aperçus de l'éditeur (`cacheRAMPreview`, `sideSlotPreview`,
  `moePreview`, `autoPlacementPreset`).

Lot 15 : la façade tient ces routes quand `LOKI_PRESETS=rust`
(`presets_api.rs`, expérimental). Sans la variable, rien ne change.

| Route | Rôle |
|---|---|
| `GET /api/presets` | liste, avec pastilles (contexte, vision, quantification, raisonnement, Strata, externe) et mesure du bench si elle correspond à la configuration actuelle |
| `POST /api/presets/order` | ordre choisi par glisser-déposer |
| `GET /api/preset?id=` | contenu et prompt système ; sans id, la graine d'un preset neuf (BIN personnalisé, HOST, PORT) |
| `POST /api/preset/save` | crée (nom de fichier unique) ou met à jour ; le preset EN SERVICE est appliqué tout de suite |
| `POST /api/preset/delete` | refusé pour le preset actif ; emporte prompt système et mesure, et le modèle sur demande |
| `POST /api/preset/cacheram`, `/api/preset/moe` | aperçus de l'éditeur, calculés comme au lancement (lot 14) |
| `POST /api/preset/autoplace` | copie en placement automatique d'un preset MoE placé à la main |
| `GET /api/preset/external?id=`, `POST /api/preset/external/save`, `/api/preset/external/test` | presets externes ; la clé n'est jamais rendue, le test fait un vrai appel de complétion |
| `POST /api/switch` | bascule de preset |

Les presets restent des fichiers `presets/*.env`, et leur prompt système une
clé de la base. Les aperçus réutilisent le port de `loki serve` (lot 14),
sondes comprises.

## Restent au Go

Tout ce qui touche au moteur reste au Go, jusqu'au lot qui le portera. Le Go
expose deux routes internes pour cela (`facade_presets.go`).

`POST /internal/v1/preset/apply {path, mode}` applique un preset à la
configuration vivante :
- `switch` : l'état du slot est relevé avant (SLOT_PERSIST), puis le moteur
  est relancé, ou arrêté pour un preset externe ;
- `save` : application, puis relance ;
- `apply` : la configuration seule (preset externe en service modifié).

`GET /internal/v1/tune/guard` dit si une optimisation tient le moteur. La
réponse est alors un 409 `{ok: false, tuning: true}`, comme le Go.

## Écarts assumés

- La suppression d'un preset retire sa mesure sans réécrire les autres avec
  leurs champs à zéro, comme le faisait la struct du Go. Le Go les relit
  pareil.
- Le message d'un échec de réseau du test externe est celui de reqwest.

## Parité

`internal/loki/parity_presets_test.go` tire du Go deux séries.

Les fonctions pures :
- nom d'affichage, guillemets, format `.env`, noms refusés ;
- quantification, raisonnement, modèle ;
- URL de complétions, contenu d'un preset externe, message d'erreur d'API ;
- arguments rejoints, clé réécrite dans un preset ;
- huit copies en placement auto ;
- empreinte de mesure, moteur imposé par un preset.

Puis 70 appels de route, avec après chacun les dossiers `presets/` et
`models/`, et les clés touchées en base. Ils couvrent :
- créations, doublons, noms refusés, erreurs JSON ;
- ordre, pastilles et mesures (empreinte, repli sur le modèle, autre
  configuration) ;
- preset actif et suppressions (modèle compris, partagé, absent) ;
- presets externes et leur test contre un faux service (succès, 401 au
  format OpenAI, 404 brut, 500 vide) ;
- placement auto et aperçus.

`tests/presets_api.rs` rejoue le tout par la façade, et une mutation fait
échouer le rejeu.

La bascule et l'enregistrement du preset en service sont vérifiés de bout en
bout contre le faux Go :
- appels d'application, avec leur mode ;
- refus quand une optimisation tourne ;
- erreur d'application rendue ;
- prompt système.

Côté Go, `TestFacadePresetApply` vérifie `presetApply` : configuration
appliquée, appareils préservés, moteur relancé, chemin hors du dossier
refusé.
