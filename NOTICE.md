# Avis d'attribution

**Loki est un fork de [AJEAN](https://github.com/nathaninline/ajean)**, créé
par [nathaninline](https://github.com/nathaninline) et publié sous licence MIT.
L'essentiel du code de ce dépôt — moteur d'assistant IA en Go, interface web
embarquée, mémoire persistante, accès internet, outils MCP, accès distant
chiffré — est l'œuvre du projet AJEAN. Merci à son auteur.

La licence d'origine est conservée à l'identique dans [`LICENSE`](LICENSE).

## Modifications apportées par le fork

- **Renommage** AJEAN → Loki (binaire `loki`, `LOKI_HOME`, `/etc/loki`,
  units `loki-engine` / `loki-ui`).
- **Conteneurisation** : image Docker autonome (llama.cpp compilé CUDA +
  binaire Go), `docker-compose` et variante Unraid, publication GHCR.
- **Supervision sans systemd** (`sys_service_container.go`) : en conteneur,
  le moteur est piloté par fichier PID au lieu de systemctl — indispensable
  pour que l'UI puisse redémarrer le moteur (changement de modèle).
- **`loki config get/set`** : lecture/écriture non interactive de la
  configuration, utilisée par l'entrypoint Docker.
- **Mode code** (`internal/loki/code_*.go`, `lsp.go`, `agents/`) : agent de
  code avec critères d'acceptation, passe de vérification indépendante,
  outils read/grep/glob, politique d'exécution, diagnostics LSP, outils git,
  jobs d'arrière-plan et relance sur appel d'outil textuel. La conception
  (contrat builder/verifier, tracker « lu avant d'écrire », auto-retry
  patterns, table languages.json) est reprise
  d'**[OpenFox](https://github.com/co-l/openfox)** (MIT) et réécrite en Go
  pour ce fork ; les prompts de rôles sont des réécritures originales. S'y
  ajoutent depuis deux idées reprises d'OpenFox et réimplémentées ici : les
  **sous-agents** (`code_subagent.go` — un rôle délégué qui travaille dans son
  propre contexte) et le **fenêtrage du fil** au chargement d'une longue
  discussion.

## Reprises depuis l'amont après le fork

Le fork continue de suivre AJEAN et y reprend des fonctionnalités, adaptées au
conteneur : le **contrôle du navigateur** (`computer_use.go`, `computer_cdp.go`,
`browser_grid.go` — pilotage d'un Chromium en CDP, ici celui de Playwright déjà
présent dans l'image) et la **préparation des images** envoyées au modèle
(`web_upload_orient.go` — orientation EXIF et redimensionnement), les
**notifications Web Push** (`push.go`, `web_push.go`), les **tâches script** et
le dossier de scripts (`chat_scripts.go`, `tasks_script.go`, `tasks_tools.go`),
et le **chiffrement de la mémoire** (`mem_crypto.go`, `mem_vault.go`,
`mem_store.go`, `mem_io.go`, `mem_migrate.go`, `mem_snapshots.go`,
`backup_bundle.go`). La sauvegarde vers le relais (`relay_backup.go`) n'est
**pas** reprise : elle vise ajean.link, le service de l'auteur amont. Loki
expose à la place un export/import de paquet chiffré en fichier local.

## Services externes

Le tunnel d'accès distant continue de pointer vers **ajean.link**, le relais
opéré par l'auteur d'AJEAN ; le catalogue de modèles intégré est également
servi par ajean.link. Ces services appartiennent au projet amont.
