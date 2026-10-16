# Lot 36 — Appels d'outils MCP sans détour par le Go

## Contexte

Depuis le lot 20, la façade tient le pool MCP (`LOKI_MCP=rust`) : sessions
stdio et HTTP, registre des outils exposés. Le tour de la façade, lui,
faisait encore exécuter un appel `mcp__*` par le Go
(`/internal/v1/tool/exec`).

Le Go ne l'exécutait pas lui-même : `mcpCall` renvoyait l'appel à la façade
(`/internal/facade/v1/mcp/call`). Chaque appel faisait donc deux
allers-retours locaux pour revenir au même pool.

Pour un outil MCP, le Go n'ajoute rien au résultat (`toolStepExtras`) :

- ni capture ;
- ni aperçu du navigateur ;
- ni image relayée.

Seul le texte revient.

## Le lot

- `ChatShared` porte le pool (`mcp`), le même que celui des routes (créé une
  fois dans `main.rs`).
- `GoTurnHost::exec_tool` : un nom `mcp__*`, pool tenu → `Mcp::call`
  directement. Le résultat est celui de la route interne :
  - texte aplati et coupé ;
  - « [l'outil a renvoyé une erreur] » ;
  - « [ok] (aucune sortie) » ;
  - une reconnexion si la session est morte, puis « [erreur MCP] … ».
- Une reconnexion annonce la version du Go dans `clientInfo`, comme la route
  interne. Elle est lue une fois (`/internal/v1/auth`) et retenue.
- Sans pool tenu (`LOKI_MCP` absent), rien ne change : l'appel part au Go,
  qui l'exécute avec son propre pool.

Le Go garde la route interne pour les tours qu'il exécute encore :

- sous-agents ;
- tâches IA ;
- discussion rendue au Go.

## Tests

`tests/lot6.rs` (`outil_mcp_par_le_pool_de_la_facade`) fait tourner un faux
serveur MCP stdio. Un tour de la façade appelle :

- un outil qui répond ;
- un outil dont le serveur meurt.

Le test vérifie :

- aucun appel au Go ;
- le résultat de chaque outil tel que le modèle le reçoit ;
- l'erreur après une reconnexion ;
- la version du Go annoncée à cette reconnexion.
