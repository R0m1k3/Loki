"""Point d'entrée FastAPI de Loki.

Sert l'API (/api/*) et, en production, le frontend React compilé (static/).
"""
from __future__ import annotations

import os
from contextlib import asynccontextmanager

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import FileResponse
from fastapi.staticfiles import StaticFiles

from . import agent_config, coder, db, rag
from .config import settings
from .ollama_client import ollama
from .routes import (
    benchmark, chat, config, files, git, mcp, models, projects, sessions,
    shell, system,
)


async def _warm_default_model() -> None:
    """Précharge le modèle par défaut en VRAM au démarrage (best-effort)."""
    import asyncio
    import logging

    await asyncio.sleep(2)  # laisse le service démarrer
    try:
        cfg = agent_config.get_config(settings.default_model)
        models.start_model_warm(
            settings.default_model, cfg.get("keep_alive", "30m")
        )
        logging.getLogger(__name__).info(
            "Préchargement du modèle %s lancé", settings.default_model
        )
    except Exception as exc:  # best-effort
        logging.getLogger(__name__).info(
            "Préchargement au démarrage ignoré : %s", exc
        )


@asynccontextmanager
async def lifespan(_: FastAPI):
    import asyncio

    db.init_db()
    rag.init_table()
    # Workspace en dépôt git : requis pour les commits du moteur code (Aider).
    coder.ensure_git(settings.workspace_dir)
    # Préchargement du modèle par défaut, sans bloquer le démarrage.
    asyncio.create_task(_warm_default_model())
    yield
    # Ferme le pool HTTP partagé vers Ollama et les sessions MCP.
    await ollama.aclose()
    from .mcp_client import manager as mcp_manager
    await mcp_manager.aclose()


app = FastAPI(
    title="Loki", description="Agent IA local sur Ollama", lifespan=lifespan
)

# En dev, le front tourne sur Vite (5173). On autorise le CORS large ;
# en prod le front est servi par le même origin, donc sans impact.
app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_methods=["*"],
    allow_headers=["*"],
)

app.include_router(models.router)
app.include_router(sessions.router)
app.include_router(chat.router)
app.include_router(files.router)
app.include_router(config.router)
app.include_router(shell.router)
app.include_router(system.router)
app.include_router(benchmark.router)
app.include_router(git.router)
app.include_router(mcp.router)
app.include_router(projects.router)


@app.get("/api/health")
async def health() -> dict:
    return {"status": "ok", "service": "loki", "version": settings.loki_version}


@app.get("/api/version")
async def version() -> dict:
    """Marqueur de build — pour vérifier que l'image déployée est à jour."""
    return {"version": settings.loki_version}


# ── Service du frontend compilé (présent uniquement en image Docker) ─────
_STATIC_DIR = os.path.join(os.path.dirname(__file__), "..", "static")
if os.path.isdir(_STATIC_DIR):
    app.mount(
        "/assets",
        StaticFiles(directory=os.path.join(_STATIC_DIR, "assets")),
        name="assets",
    )

    @app.get("/{full_path:path}")
    async def spa_fallback(full_path: str):
        """Renvoie index.html pour toutes les routes (SPA React)."""
        index = os.path.join(_STATIC_DIR, "index.html")
        return FileResponse(index)
