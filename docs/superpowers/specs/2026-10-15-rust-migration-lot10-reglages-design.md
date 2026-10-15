# Lot 10 — réglages partagés tenus par la façade

## Contexte

Quatre routes ne font que lire et écrire des réglages en base, partagés entre
tous les appareils :

| Route | Port de | Ce qu'elle règle |
|---|---|---|
| `GET\|POST /api/prefs` | `web_prefs.go` | thème, affichage, prénom (24 caractères, sans caractère de contrôle), avatars (liste fermée) |
| `GET\|POST /api/sysprompt` | `web_sysprompt.go` | prompt système du preset actif (`state/sysprompt:<id>`), repli sur le global (`state/sysprompt`) |
| `GET\|POST /api/reasoning-effort` | `handleReasoningEffort` | `REASONING_EFFORT` de la configuration vivante (liste blanche) |
| `GET\|POST /api/oai/public` | `web_public_url.go` | adresse publique normalisée en « schéma://hôte[:port] » |

Lot 10 : la façade les sert quand `LOKI_SETTINGS=rust` (`settings.rs`,
expérimental ; sans la variable rien ne change).

Les valeurs restent dans la base du Go, aux mêmes clés. Le Go les relit là
où il s'en sert (tour de chat, accès OpenAI) : son cache de configuration se
périme sur l'horodatage du fichier de base, qu'une écriture de la façade
modifie. `facade_settings.go` remet les routes du tunnel à la façade.

## Écarts assumés

- Message d'un corps JSON syntaxiquement illisible : celui de serde. Une
  valeur du mauvais type (`{"text":5}`) garde le message d'`encoding/json`.
- Préférences réécrites clé par clé au lieu d'une transaction.
- Port nommé dans l'adresse publique (`:https`) refusé : le Go le résolvait
  par `/etc/services`.

## Parité

`internal/loki/parity_settings_test.go` tire du Go :

- 19 adresses publiques normalisées ou refusées ;
- 11 valeurs d'intensité ;
- 29 appels de route, avec le contenu de la base après chacun :
  - fusion des préférences (valeurs refusées, prénom tronqué, vides effacés) ;
  - prompt par preset avec repli, effacement, méthode refusée ;
  - intensité acceptée ou refusée ;
  - adresse publique.

`tests/settings.rs` rejoue le tout par la façade. Une mutation (prénom
tronqué à 25 caractères) fait échouer le rejeu.
