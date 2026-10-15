# Lot 7 — outils de l'agent exécutés par la façade

## Contexte

Lot 6 : quand la façade tient la conversation (`LOKI_CHAT=rust`), la boucle du
tour est en Rust mais chaque appel d'outil repart au Go
(`/internal/v1/tool/exec`). Lot 7 : porter les outils eux-mêmes, en retirant
les appels au Go un par un. Rien ne change sans `LOKI_CHAT=rust` : la boucle
Go exécute ses outils comme avant.

## Étape 1 — fichiers et shell (fait)

Exécutés par la façade (`rust/loki-rs/src/tools/`) : `read`, `grep`, `glob`,
`write`, `edit`, `bash`, `ask`, avec les mêmes garde-fous dans le même ordre
(commande catastrophique, `&` final en mode Code, bac à sable du mode Code,
dossier mémoire interdit, « lu avant d'écrire »), le même diff pour l'UI et le
même nettoyage de la sortie du shell (`| tail -N`, ANSI, fin bornée).

L'état partagé reste au Go, en un seul exemplaire, derrière trois routes
internes (`web_internal_toolstate.go`) :

| Route | Rôle |
|---|---|
| `POST /internal/v1/tool/context` | dossier de travail (`agentCwd`), dossier mémoire, cible distante |
| `POST /internal/v1/tool/tracker` | `read` : lecture notée ; `check` : refus éventuel avant écriture |
| `POST /internal/v1/tool/wrote` | après une écriture : note du suivi, bloc des diagnostics LSP |

Le suivi reste au Go parce que les sous-agents et les tâches planifiées, qui y
tournent, le lisent et l'écrivent aussi ; les serveurs LSP y vivent.

Repli sur le Go : outil non porté ; `write`/`edit`/`bash` quand l'agent vise
un poste distant (une commande catastrophique est refusée avant, ici) ;
contexte indisponible.

Le jeton de l'API interne est retiré de l'environnement du shell de l'agent.

### Parité

`internal/loki/parity_tools_test.go` passe 58 appels à `execTool` sur une
arborescence de test et range le journal (libellés, résultats, diffs,
questions, arborescence finale) dans `rust/loki-rs/tests/parity_tools/` ;
`rust/loki-rs/tests/tools.rs` rejoue les mêmes appels dans `exec_local` et
retrouve le même journal. Le test Go échoue si le comportement Go change
(`LOKI_PARITY_UPDATE=1` pour régénérer, puis le test Rust dit si le port suit).

### Écarts assumés

- Motif `grep` invalide : le message d'erreur est celui du moteur d'expressions
  de Rust (crate `regex`), pas celui de Go.
- Classes `\w`, `\b`, `\s`, `\d` d'un motif `grep` : Unicode en Rust, ASCII en
  Go (un mot accentué y est un mot entier).
- Octets hors UTF-8 dans un fichier lu ou cherché : remplacés par U+FFFD par
  séquence invalide (Go : par octet, à l'encodage JSON). `edit` travaille sur
  les octets et laisse intact ce qu'il ne remplace pas.
- `mkdir` en échec : le chemin cité est le dossier parent visé, pas le
  composant qui a échoué.

## Étapes suivantes

1. Web (`web_search`, `web_open`, `web_read`, `web_grep`, `web_images`) :
   configuration seule, pas d'état partagé.
2. Shell d'arrière-plan (`bash_bg`, `bash_tail`) et git (`git_status`,
   `git_diff`, `git_clone`) : jobs tenus par le Go aujourd'hui.
3. Mémoire (`mem_*`, `recall*`) : avec le portage du chiffrement.
4. Tâches, navigateur (CDP), MCP, sous-agents, critères et mode Code.
