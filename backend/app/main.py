"""Point d'entrée FastAPI de Loki.

Sert l'API (/api/*) et, en production, le frontend React compilé (static/).
"""
from __future__ import annotations

import os

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import FileResponse
from fastapi.staticfiles import StaticFiles

from .config import settings
from .routes import models

app = FastAPI(title="Loki", description="Agent IA local sur Ollama")

# En dev, le front tourne sur Vite (5173). On autorise le CORS large ;
# en prod le front est servi par le même origin, donc sans impact.
app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_methods=["*"],
    allow_headers=["*"],
)

app.include_router(models.router)


@app.get("/api/health")
async def health() -> dict:
    return {"status": "ok", "service": "loki"}


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
