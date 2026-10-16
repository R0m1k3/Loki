# Lot 20 — serveurs MCP tenus par la façade

## Contexte

Le Go tenait les serveurs MCP (Model Context Protocol) :
- `mcp_config.go` : la configuration, dans `state/mcp` ;
- `mcp_catalog.go` : le catalogue embarqué ;
- `mcp_client.go` : le pool de sessions, avec le SDK MCP Go ;
- les routes `/api/mcp*` de `web_api.go`.

Le pool sert deux usages : l'état affiché dans l'interface, et les outils
`mcp__<serveur>__<outil>` annoncés au modèle et exécutés pour l'agent.

Lot 20 : la façade tient tout cela quand `LOKI_MCP=rust` (`mcp/`,
expérimental). Sans la variable, rien ne change.

| Route | Rôle |
|---|---|
| `GET /api/mcp` | état de chaque serveur, connexion tentée pour les activés (en parallèle) |
| `GET /api/mcp/catalog` | catalogue embarqué (le même fichier que le Go), `{workspace}` résolu, entrées déjà configurées marquées, lanceur (npx, uvx) présent ou non |
| `POST /api/mcp/save` | ajoute ou remplace (actif par défaut) ; nom contrôlé, un seul transport |
| `POST /api/mcp/delete`, `/api/mcp/toggle`, `/api/mcp/tool` | retire, active ou éteint, masque ou démasque un outil |
| `POST /api/mcp/test` | reconnexion forcée |

## Client MCP en Rust

`mcp/client.rs` parle JSON-RPC 2.0 sur deux transports, comme le SDK Go :
- stdio : un processus local, un message par ligne ;
- Streamable HTTP : un POST par message, réponse en JSON ou en flux SSE,
  session suivie par `Mcp-Session-Id`, en-têtes configurés ajoutés.

La session s'ouvre par `initialize` puis `notifications/initialized`. Elle
sert `tools/list` (première page, comme le Go) et `tools/call`. Un `ping` du
serveur reçoit sa réponse.

Le pool (`mcp/mod.rs`) suit `mcp_client.go` :
- une session gardée d'un tour à l'autre ;
- une entrée publiée avant la connexion, pour que deux appelants partagent
  la même tentative ;
- une session invalidée pendant sa connexion n'est jamais republiée ;
- délai de 3 min pour npx et uvx (premier téléchargement), 20 s sinon ;
- un appel borné à 120 s, une reconnexion si la session est morte ;
- la sortie plafonnée à la fenêtre de contexte (12 000 au moins), une erreur
  d'outil signalée, une sortie vide rendue « [ok] (aucune sortie) ».

## Le Go

Un seul pool, celui de la façade. Le Go, qui exécute encore les outils de
l'agent, lui demande les outils à annoncer, leurs appels et le
pré-chauffage (`/internal/facade/v1/mcp/{tools,call,prewarm}`,
`facade_mcp.go`), quelle que soit la conversation qui tourne. Son propre
pool reste vide.

L'image copie `internal/loki/mcp_catalog.json` dans l'étape de compilation
Rust : un seul catalogue pour les deux binaires.

## Écarts assumés

- Les erreurs de transport (connexion refusée, flux coupé) ont les messages
  du client Rust. Une erreur JSON-RPC et un binaire introuvable ont ceux du
  Go.
- Un contenu « autre » (lien de ressource, ressource intégrée) est rendu en
  JSON dans l'ordre des champs du SDK. Les annotations éventuelles sont dans
  l'ordre alphabétique.

## Parité

`internal/loki/parity_mcp_test.go` tire du Go :
- les fonctions pures : noms exposés, schémas, libellés d'appel, serveur
  d'origine d'un outil, ligne du préambule, délais et leurs messages,
  aplatissement de neuf résultats ;
- la lecture de la configuration et seize mutations avec leurs erreurs et la
  base après chacune ;
- ce qu'un vrai serveur du SDK MCP Go met sur le fil en Streamable HTTP :
  onze outils, et le résultat de chacun ;
- ce que le client Go en tire : état de quatre serveurs (joignable,
  binaire introuvable, désactivé, mal configuré), outils annoncés (un
  masqué), appels.

`tests/mcp.rs` rejoue les fonctions et la configuration. Un faux serveur
resert ce que le serveur du SDK a répondu (appels en SSE) : l'état, les
outils et les appels tirés par la façade doivent être ceux du Go, sur une
seule session. Sont aussi vérifiés de bout en bout :
- le transport stdio : environnement, ping du serveur, serveur mort en plein
  appel puis reconnexion ;
- les routes de configuration, en-têtes compris ;
- le catalogue ;
- une session invalidée pendant sa connexion, sans processus laissé
  derrière.

Une mutation de chaque règle fait échouer les tests. Côté Go,
`TestFacadeMCP` vérifie le relais, les outils, les appels, le
pré-chauffage et une façade injoignable.
