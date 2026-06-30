# Loki — Agent IA local sur Ollama

Loki est un atelier d'agent IA **100 % local**, conçu pour se connecter à
[Ollama](https://ollama.com) et travailler en mode agentique : un tchat, des
outils (lecture/écriture de fichiers, aperçu HTML en direct…) et un workspace
de fichiers. Pensé pour un déploiement **Docker** simple.

Le thème visuel (« atelier café », sombre et chaleureux, accent ambre) est
décliné fidèlement depuis la maquette d'origine.

## Architecture

```
Frontend (React + Vite + TS + Tailwind)
        │ HTTP + SSE
Backend (FastAPI, Python)
   ├── /api/status, /api/models, /api/models/pull   (Ollama)
   ├── /api/sessions  (CRUD sessions)
   ├── /api/chat      (boucle agentique + outils, streaming SSE)
   ├── /api/files     (arborescence + contenu du workspace)
   │   Outils agent : read_file · write_file · list_dir (confinés au workspace)
        │ httpx                     │ volume
     Ollama (:11434)            /workspace + /data (SQLite)
```

Un seul conteneur : le backend FastAPI sert l'API **et** le frontend compilé.
Loki se connecte à un **Ollama existant** via `OLLAMA_HOST`.

## Démarrage rapide (Docker)

```bash
cp .env.example .env          # ajuste OLLAMA_HOST si besoin
docker compose up --build
```

Application disponible sur http://localhost:8717

> **Ollama** : par défaut Loki vise `http://host.docker.internal:11434`
> (un Ollama installé sur la machine hôte). Pour embarquer Ollama dans la
> stack : `docker compose --profile ollama up` puis règle
> `OLLAMA_HOST=http://ollama:11434`.

## Installation sur Unraid

L'image est **construite et publiée automatiquement par GitHub Actions** sur GHCR
(`ghcr.io/r0m1k3/loki:latest`) à chaque push sur `main`. Aucun build ni `git`
n'est nécessaire sur Unraid — un simple `pull`. Compose prêt à l'emploi :
[`docker-compose.unraid.yml`](docker-compose.unraid.yml).

1. Dans le terminal Unraid, crée les dossiers de données :
   ```bash
   mkdir -p /mnt/user/appdata/loki/workspace /mnt/user/appdata/loki/data
   ```
2. Installe le plugin **Compose Manager** (Apps), crée une nouvelle stack, et
   colle le contenu de `docker-compose.unraid.yml`.
3. **Adapte `OLLAMA_HOST`** : mets l'IP de ton serveur Unraid où tourne le
   conteneur Ollama, p. ex. `http://192.168.1.10:11434`.
4. **Compose Up**. Loki est accessible sur `http://<ip-unraid>:8717`
   (change le port à gauche du mapping `8717:8080` s'il est déjà pris).

> La première publication de l'image prend quelques minutes (le temps que le
> workflow GitHub se termine). Si l'image est privée, rends le package **public**
> une fois (GitHub → Packages → `loki` → Package settings → Change visibility).
>
> Les modèles déjà présents dans ton Ollama sont **détectés automatiquement** ;
> tu peux en télécharger d'autres depuis l'onglet Configuration.
> Mise à jour : Compose Down/Up (avec pull), ou `docker compose pull`.

## Développement (sans Docker)

**Backend**
```bash
cd backend
pip install -r requirements.txt
OLLAMA_HOST=http://localhost:11434 uvicorn app.main:app --reload --port 8080
```

**Frontend** (proxy `/api` → `:8080`)
```bash
cd frontend
npm install
npm run dev        # http://localhost:5173
```

## Configuration (variables d'environnement)

| Variable        | Défaut                              | Rôle                          |
| --------------- | ----------------------------------- | ----------------------------- |
| `OLLAMA_HOST`   | `http://host.docker.internal:11434` | URL de l'instance Ollama      |
| `DEFAULT_MODEL` | `llama3.1:8b`                        | Modèle sélectionné au démarrage |
| `WORKSPACE_DIR` | `/workspace`                        | Dossier de travail de l'agent |
| `DATA_DIR`      | `/data`                             | Base SQLite (sessions + config) |
| `PORT`          | `8717`                              | Port de l'application (dedans = dehors) |
| `SEARX_URL`     | *(vide)*                            | Instance SearxNG pour `web_search` (sinon DuckDuckGo) |

## Utilisation

1. Vérifie la pastille **Ollama** (verte = connecté) en haut à droite, et choisis
   un modèle qui supporte le *function calling* (ex. `llama3.1:8b`,
   `qwen2.5-coder`).
2. Décris une tâche dans le tchat, p. ex. *« Crée une landing page pour un café
   nommé Café Lumière, avec menu et horaires »*.
3. L'agent lit/écrit des fichiers dans le **workspace** ; chaque appel d'outil
   s'affiche dans le fil, et l'aperçu HTML apparaît à droite (onglets **Aperçu /
   Code / Logs**).
4. Règle le comportement dans **Configuration** (modèle, température/top-p/top-k,
   jetons max, outils actifs, invite système).

## Outils de l'agent

| Outil         | Rôle                                   | Par défaut |
| ------------- | -------------------------------------- | ---------- |
| `read_file`   | Lire un fichier du workspace           | activé     |
| `write_file`  | Créer / modifier un fichier            | activé     |
| `list_dir`    | Lister un répertoire                   | activé     |
| `web_search`  | Recherche web (DuckDuckGo / SearxNG)   | désactivé  |
| `run_shell`   | Exécuter une commande **(sensible)**   | désactivé  |

## Sécurité

- **Confinement** : toutes les opérations fichier (`read_file`, `write_file`,
  `list_dir`) et `run_shell` sont strictement confinées au `WORKSPACE_DIR`. Toute
  tentative de sortie (`../`, chemin absolu) est rejetée.
- **`run_shell`** est désactivé par défaut. Une fois activé, chaque commande
  proposée par l'agent demande une **validation explicite** dans l'interface
  (option *confirm_shell*, activée par défaut) avant exécution.
- Le conteneur tourne en **utilisateur non-root** et expose un **HEALTHCHECK**.
- Loki est conçu pour un usage **local** : n'expose pas le port publiquement sans
  ajouter ta propre couche d'authentification.

## Feuille de route

- [x] **Phase 1** — Socle + design system fidèle au thème, layout 3 panneaux
- [x] **Phase 2** — Connexion Ollama : statut, liste des modèles, pull avec progression, sélecteur
- [x] **Phase 3** — Chat streaming (SSE) + persistance des sessions (SQLite)
- [x] **Phase 4** — Boucle agentique & outils fichiers (read/write/list), confinés au workspace, rendu des appels d'outils dans le fil
- [x] **Phase 5** — Aperçu HTML live + onglets Code/Logs + arborescence du workspace
- [x] **Phase 6** — Configuration complète (génération, toggles d'outils, invite système)
- [x] **Phase 7** — Outils avancés : `web_search` (DuckDuckGo/SearxNG) et `run_shell` avec validation utilisateur
- [x] **Phase 8** — Durcissement & documentation Docker

## Structure

```
backend/   FastAPI : routes Ollama, client httpx, config
frontend/  React : design system (tailwind.config), panneaux, store Zustand
workspace/ fichiers créés par l'agent (monté en volume)
data/      base SQLite (monté en volume)
```
