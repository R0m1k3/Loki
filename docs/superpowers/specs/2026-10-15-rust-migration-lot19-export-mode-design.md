# Lot 19 — export, mode Code et critères de la discussion

## Contexte

Avec `LOKI_CHAT=rust`, la façade tient la conversation (lot 6), mais trois
routes de la discussion ouverte restaient au Go :
- `/api/chat/export` (`chat_export.go`). Il relisait la copie enregistrée,
  qui peut avoir un temps de retard sur la conversation vivante ;
- `/api/chat/mode` (`code_mode.go`) : mode Chat ou Code de la discussion, et
  ses critères ;
- `/api/chat/criteria` : critères édités à la main.

Lot 19 : ces trois routes rejoignent celles de la conversation. La façade
les sert quand elle la tient (`chatx.rs`, `facadeChatRoutes` côté Go), sans
nouvelle variable. Sans `LOKI_CHAT=rust`, rien ne change.

| Route | Rôle |
|---|---|
| `GET /api/chat/export` | Markdown (fait pour être lu) ou JSON (fait pour être retraité), tiré de la conversation vivante ; options `format`, `reasoning`, `tools`, `results`, `turns` |
| `GET\|POST /api/chat/mode` | mode de la discussion ouverte (`chat/mode:<id>`, absent = chat) et ses critères ; un changement part sur le fil (`{mode}`) |
| `POST /api/chat/criteria` | critères édités : vides écartés, vingt au plus, ids posés, statuts ramenés à `pending`/`passed`/`failed` ; scellés comme le contenu des discussions (`chat/crit:<id>`), sur le fil (`{criteria, ts}`) |

L'export suit `chat_export.go` :
- Markdown : un bloc par tour, le raisonnement replié dans un `<details>`,
  les outils en liste (seul l'état final d'un outil compte, comme au
  rejeu), la compaction signalée, un export partiel annoncé en tête ;
- JSON : indenté et HTML échappé comme `json.MarshalIndent`, vue modèle sans
  mise à jour de contexte (`PROJ_SNAPSHOT`) ni image en base64 (élidée avec
  sa taille), journal filtré par les mêmes cases ;
- les derniers échanges comptent à partir des messages de l'utilisateur, pas
  des rappels de Loki.

## Restent au Go

Le reste du mode Code reste au Go et lit les mêmes clés : l'indice « ça
ressemble à du code », les travaux de fond et l'outil `criteria` de l'agent.

## Écarts assumés

- L'export part de la conversation vivante, pas de sa copie enregistrée :
  le dernier tour y est, même tout juste fini.
- Les images d'un tour en cours restent dans l'export JSON, élidées. Une
  conversation relue par le Go les avait déjà perdues.

## Parité

`internal/loki/parity_chatx_test.go` tire du Go l'export d'une conversation
riche sous 22 jeux d'options lus dans la requête. La conversation contient :
- pièces jointes, raisonnement en deltas ;
- outils finis, en cours, avec et sans sortie, labels piégés pour le
  Markdown ;
- statistiques, compaction ;
- images en base64 et en URL, rappel de Loki ;
- mise à jour de contexte marquée, et une tapée par l'utilisateur.

Plus les exports d'une conversation vide.

Il tire aussi 23 appels aux routes du mode et des critères, avec la base et
les événements du fil après chacun : erreurs JSON, y compris celles d'un
critère mal typé (`Criterion.criteria.id`), et au-delà de vingt critères.

`tests/chatx.rs` rejoue l'export octet pour octet et les routes par la
façade. Il vérifie aussi les en-têtes du téléchargement et le maintien des
routes au Go sans la conversation. Une mutation de chaque règle fait échouer
les tests.
