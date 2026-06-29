"""Routes liées à Ollama : statut de connexion, modèles, téléchargement."""
from __future__ import annotations

import json

import httpx
from fastapi import APIRouter
from fastapi.responses import StreamingResponse
from pydantic import BaseModel

from ..config import settings
from ..ollama_client import ollama

router = APIRouter(prefix="/api", tags=["ollama"])


@router.get("/status")
async def status() -> dict:
    """État de la connexion Ollama (point vert/rouge de la barre supérieure)."""
    try:
        version = await ollama.ping()
        return {
            "connected": True,
            "host": ollama.host,
            "version": version.get("version"),
            "default_model": settings.default_model,
        }
    except (httpx.HTTPError, OSError) as exc:
        return {
            "connected": False,
            "host": ollama.host,
            "error": str(exc),
            "default_model": settings.default_model,
        }


@router.get("/models")
async def list_models() -> dict:
    """Modèles installés localement, formatés pour le sélecteur de l'UI."""
    try:
        raw = await ollama.list_models()
    except (httpx.HTTPError, OSError):
        return {"models": []}

    models = []
    for m in raw:
        details = m.get("details", {})
        size_go = round(m.get("size", 0) / 1_000_000_000, 1)
        models.append(
            {
                "name": m.get("name"),
                "size_go": size_go,
                "parameter_size": details.get("parameter_size"),
                "quantization": details.get("quantization_level"),
                "family": details.get("family"),
            }
        )
    return {"models": models, "default": settings.default_model}


class PullRequest(BaseModel):
    name: str


@router.post("/models/pull")
async def pull_model(req: PullRequest) -> StreamingResponse:
    """Télécharge un modèle en streamant la progression (SSE)."""

    async def event_stream():
        try:
            async for chunk in ollama.pull_model(req.name):
                yield f"data: {json.dumps(chunk)}\n\n"
        except (httpx.HTTPError, OSError) as exc:
            yield f"data: {json.dumps({'error': str(exc)})}\n\n"
        yield "data: [DONE]\n\n"

    return StreamingResponse(event_stream(), media_type="text/event-stream")
