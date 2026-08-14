# Loki — assistant IA local en conteneur (fork d'AJEAN)

> **Loki est un fork de [AJEAN](https://github.com/nathaninline/ajean)** de
> [nathaninline](https://github.com/nathaninline), sous licence MIT — voir
> [`NOTICE.md`](NOTICE.md) et [`LICENSE`](LICENSE). L'essentiel du code et des
> fonctionnalités vient d'AJEAN ; ce fork le rebaptise et le fait tourner dans
> **un conteneur Docker GPU autonome**, là où l'amont s'installe en binaire +
> systemd sur la machine hôte.

Loki fait tourner un modèle de langage **100 % en local** : tchat, mémoire
persistante, accès internet, outils MCP, agent (shell, fichiers), accès distant
chiffré — serveur d'inférence llama.cpp compris, dans une seule image.

## Architecture

```
┌────────────────── conteneur loki ──────────────────┐
│  loki web  (UI + API, port 8090, premier plan)     │
│     │ pilote (fichier PID — pas de systemd)        │
│  loki serve ──exec──► llama-server (CUDA, :8080)   │
│                        ▲ modèles .gguf             │
│  /data (config, bbolt, mémoire, workspace)         │
│  /models (GGUF déposés à la main)                  │
└────────────────────────────────────────────────────┘
```

- **Un seul conteneur** : l'UI joint le moteur sur `localhost` (contrainte
  héritée de l'amont), les deux partagent donc le même conteneur.
- **Sans systemd** : l'amont pilote le moteur via systemctl ; en conteneur,
  Loki bascule automatiquement sur une supervision par fichier PID
  (`internal/loki/sys_service_container.go`). Changer de modèle depuis l'UI
  redémarre le moteur normalement.
- Le moteur (port 8080, non authentifié par défaut) **n'est pas exposé** ;
  seule l'UI (8090) l'est.

## Démarrage rapide (Docker, GPU NVIDIA)

Pré-requis : pilote NVIDIA + [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html).

```bash
cp .env.example .env    # CUDA_ARCHS=86 pour une RTX 30xx, etc.
docker compose up --build   # 30-45 min : compilation CUDA de llama.cpp
```

Interface : http://localhost:8090 — télécharge un modèle depuis le catalogue
intégré (onglet modèles), il démarre tout seul.

## Installation sur Unraid

L'image est construite et publiée par GitHub Actions sur GHCR
(`ghcr.io/r0m1k3/loki:latest`) à chaque push sur `main` — aucun build sur
Unraid. Compose prêt à l'emploi : [`docker-compose.unraid.yml`](docker-compose.unraid.yml).

1. Installe le plugin **Nvidia Driver** (Apps) et vérifie `nvidia-smi`.
2. Crée les dossiers :
   ```bash
   mkdir -p /mnt/user/appdata/loki/data /mnt/user/appdata/loki/models
   ```
3. Plugin **Compose Manager** → nouvelle stack → colle
   `docker-compose.unraid.yml` → **Compose Up**.
4. Interface : `http://<ip-unraid>:8090`.

## Configuration

Tout se règle **dans l'UI** (modèle, contexte, presets…) et survit aux
redémarrages (volume `/data`). Variables d'environnement du conteneur :

| Variable | Rôle | Défaut |
|---|---|---|
| `LOKI_WEB_PORT` | port de l'UI | `8090` |
| `LOKI_MODEL` | modèle initial (semé au 1er boot seulement) | — |
| `LOKI_CTX` | taille de contexte initiale | `32768` |
| `LOKI_NGL` | couches GPU initiales | `999` (tout) |
| `LOKI_HOME` | données (volume) | `/data` |
| `LOKI_MODEL_DIRS` | dossiers .gguf additionnels | `/models` |

En CLI dans le conteneur : `docker exec -it loki loki status` (aussi :
`logs`, `restart`, `config`, `bench`, `test`…).

## Fonctionnalités (héritées d'AJEAN)

- **Tchat** avec streaming, raisonnement visible, pièces jointes, vision
  (selon modèle), export de conversations.
- **Mémoire persistante** (`memory off|ondemand|always`).
- **Accès internet** : recherche + lecture de pages, moteur Go intégré ou
  [Crawl4AI](https://github.com/unclecode/crawl4ai) pour les pages JS.
- **Agent** : shell, fichiers, workspace (`agent on`).
- **Serveurs MCP** : Node.js est inclus dans l'image pour les serveurs `npx`.
- **Presets** de configuration par modèle, bench, auto-détection GPU.
- **Accès distant chiffré** via le relais [ajean.link](https://ajean.link)
  (service opéré par l'auteur de l'amont).
- **API OpenAI-compatible** exposable (`network on`, protégée par clé).

## Différences avec l'amont

| | AJEAN (amont) | Loki (ce fork) |
|---|---|---|
| Installation | binaire + `sudo ajean install` (systemd) | `docker compose up` |
| Moteur llama.cpp | compilé sur la machine (`ajean llamacpp install`) | précompilé CUDA dans l'image |
| Supervision moteur | systemd / launchd / PID (Windows) | fichier PID (`LOKI_CONTAINER=1`) |
| Configuration initiale | `ajean edit` ($EDITOR) | entrypoint + `loki config set` |
| Mise à jour | `ajean update` (binaire GitHub) | `docker compose pull` |

Le reste — UI, mémoire, outils, protocole — est celui d'AJEAN. Pour récupérer
les évolutions de l'amont :

```bash
git fetch upstream && git merge upstream/main   # conflits de renommage à arbitrer
```

## Build sans GPU / autres accélérateurs

L'image par défaut cible CUDA. Pour un essai CPU, remplace dans le
`Dockerfile` les bases `nvidia/cuda:*` par `ubuntu:24.04` et retire les flags
`-DGGML_CUDA=*` (llama.cpp bascule en CPU). Vulkan/ROCm : adapter les flags
comme le fait l'amont (`internal/loki/backend_build.go`).

## Licence

MIT — © les contributeurs d'AJEAN (« Jean contributors ») pour le code amont,
voir [`LICENSE`](LICENSE) et [`NOTICE.md`](NOTICE.md).
