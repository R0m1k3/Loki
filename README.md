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
| `DEFAULT_MODEL` | `gemma4:12b`                         | Modèle sélectionné au démarrage |
| `WORKSPACE_DIR` | `/workspace`                        | Dossier de travail de l'agent |
| `DATA_DIR`      | `/data`                             | Base SQLite (sessions + config) |
| `PORT`          | `8717`                              | Port de l'application (dedans = dehors) |
| `SEARX_URL`     | *(vide)*                            | Instance SearxNG pour `web_search` (sinon DuckDuckGo) |

## Utilisation

1. Vérifie la pastille **Ollama** (verte = connecté) en haut à droite, et choisis
   un modèle qui supporte le *function calling* (profil fourni : `gemma4:12b`).
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

## Préchargement des modèles (réponses instantanées)

Ollama décharge un modèle de la VRAM après quelques minutes d'inactivité : le
message suivant paie alors un rechargement complet (lent). Loki évite ça :

- **Préchargement à la sélection** : choisir un modèle le charge immédiatement
  en VRAM (`/api/models/warm`).
- **Maintien au chaud** : chaque requête envoie un `keep_alive` (défaut 30 min,
  réglable dans Configuration → Intelligence : de « décharger aussitôt » à
  « toujours »).
- **Préchargement au démarrage** du modèle par défaut (en arrière-plan).
- **Indicateur d'état** : la pastille du sélecteur de modèle est verte quand le
  modèle est chargé sur GPU, orange sur CPU, blanche s'il reste à charger.

## Modes d'exécution (Plan / Build / Yolo)

Un sélecteur dans le composer contrôle le niveau d'autonomie de l'agent :
- **Plan** 🔍 — lecture seule (`read_file`, `list_dir`, `grep_search`) : l'agent
  analyse et propose sans jamais modifier de fichier ni exécuter de commande.
- **Build** 🔨 — normal : écrit les fichiers, `run_shell` demande confirmation.
- **Yolo** ⚡ — autonomie maximale : approuve tout, y compris le shell.

## Panneau Git & Diff

Le workspace étant un dépôt git (chaque action de l'agent = un commit), l'onglet
**Git** du panneau de droite montre l'historique des commits, le **diff coloré**
de chacun, et un bouton **↶ Annuler** (revert) pour défaire une modification en
un clic.

## Intelligence augmentée

- **Plan-puis-exécute** : les demandes complexes sont décomposées en 3-5 étapes
  affichées dans le fil ; l'agent (ou le moteur code) suit le plan.
- **Auto-critique « Qualité + »** (Configuration → Intelligence) : la réponse
  est relue et révisée avant d'être finalisée.
- **Mémoire long-terme (RAG)** : chaque échange est vectorisé (`/api/embed`)
  et les souvenirs pertinents des anciennes sessions sont réinjectés en
  contexte. Nécessite un modèle d'embedding installé (ex.
  `ollama pull nomic-embed-text`) — sinon désactivé silencieusement.
- **Vérification HTML** : liens locaux cassés et balises déséquilibrées sont
  détectés après chaque génération ; le moteur code fait une passe
  d'auto-correction.
- **Benchmark intégré** (Configuration → Benchmark) : 5 mini-épreuves notées
  /100 (appel d'outil, code exécutable, consignes, extraction JSON, format)
  pour comparer objectivement tes modèles installés.

## Tirer le meilleur des petits modèles

Loki est conçu pour qu'un modèle local modeste se comporte comme un bon agent :

- **Mémoire compressée** : au-delà d'un seuil, les anciens tours sont résumés
  en arrière-plan et le modèle ne reçoit que « invite + résumé + 10 derniers
  messages ». Contexte court = modèle concentré, et qui reste sur le GPU.
- **Outils chirurgicaux** : `edit_file` (recherche/remplacement exact — pas de
  réécriture intégrale ratée), `grep_search` (trouver avant de modifier), et
  `write_file` par morceaux (overwrite/append).
- **Auto-vérification** : après chaque écriture, la syntaxe (`.py`, `.json`)
  est contrôlée ; l'erreur est renvoyée immédiatement au modèle, qui se
  corrige dans le même tour.
- **Bon modèle au bon poste** : les tâches de code sont confiées au meilleur
  modèle code installé (`qwen-coder`, `deepseek-coder`…), automatiquement
  (config `code_model: auto`), même si tu discutes avec un généraliste.
- **Récupération d'appels d'outils malformés** : arguments réparés ou
  redemandés, modèles sans function-calling détectés et gérés.

## Moteur code (façon Claude Code) — routage automatique

Loki embarque [Aider](https://aider.chat) (Apache-2.0, version figée) comme
**moteur code** : édition multi-fichiers fiable (formats diff/search-replace,
efficaces même avec de petits modèles), repo map, et **commits git
automatiques** dans le workspace.

**C'est invisible** : un routeur classe chaque message.
- Tâche de code détectée (heuristique + micro-classification LLM sur les cas
  ambigus) → le message part au **moteur code** ; le fil affiche la carte
  `code_task`, les fichiers modifiés et le commit.
- Sinon → **boucle agent** classique ; et l'agent peut lui-même déléguer au
  moteur via l'outil `code_task` quand il juge qu'il faut coder.

Désactivable dans Configuration → Outils (`code_task`). Le workspace est
auto-initialisé en dépôt git : chaque modification de code = un commit
(historique et rollback via `git log` / `git revert` dans le workspace).

## Profil GPU fourni

Loki est préconfiguré pour une **RTX 3060 12 Go** avec `gemma4:12b` en Q4 :
contexte 8192, sortie 4096 jetons, batch 256, GPU principal 0 et les 49 couches
du modèle sur le GPU. Les paramètres de génération et de cache/contexte sont
enregistrés séparément pour chaque modèle.

La quantification du cache KV reste globale dans Ollama. Pour économiser environ
la moitié de sa VRAM, démarre Ollama avec `OLLAMA_FLASH_ATTENTION=1` et
`OLLAMA_KV_CACHE_TYPE=q8_0`. Un redémarrage d'Ollama est requis. Le conteneur
doit également exposer le GPU (`--gpus=all`) ; Loki ne peut pas contourner une
configuration Docker sans accès CUDA.

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
