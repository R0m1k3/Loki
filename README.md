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
   ├── /api/chat      (conversation streaming, SSE)
   │   (outils agentiques : phases suivantes)
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

Application disponible sur http://localhost:8080

> **Ollama** : par défaut Loki vise `http://host.docker.internal:11434`
> (un Ollama installé sur la machine hôte). Pour embarquer Ollama dans la
> stack : `docker compose --profile ollama up` puis règle
> `OLLAMA_HOST=http://ollama:11434`.

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
| `DATA_DIR`      | `/data`                             | Base SQLite (sessions)        |
| `PORT`          | `8080`                              | Port exposé                   |

## Feuille de route

- [x] **Phase 1** — Socle + design system fidèle au thème, layout 3 panneaux
- [x] **Phase 2** — Connexion Ollama : statut, liste des modèles, pull avec progression, sélecteur
- [x] **Phase 3** — Chat streaming (SSE) + persistance des sessions (SQLite)
- [ ] **Phase 4** — Boucle agentique & outils fichiers (read/write/list)
- [ ] **Phase 5** — Aperçu HTML live + onglets Code/Logs + arborescence
- [ ] **Phase 6** — Configuration complète (génération, toggles d'outils, invite système)
- [ ] **Phase 7** — Outils avancés (web_search, run_shell confirmé, html_preview)
- [ ] **Phase 8** — Hardening & documentation Docker

## Structure

```
backend/   FastAPI : routes Ollama, client httpx, config
frontend/  React : design system (tailwind.config), panneaux, store Zustand
workspace/ fichiers créés par l'agent (monté en volume)
data/      base SQLite (monté en volume)
```
