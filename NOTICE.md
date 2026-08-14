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

## Services externes

Le tunnel d'accès distant continue de pointer vers **ajean.link**, le relais
opéré par l'auteur d'AJEAN ; le catalogue de modèles intégré est également
servi par ajean.link. Ces services appartiennent au projet amont.
