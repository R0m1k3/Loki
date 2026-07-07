"""Routes liées à Ollama : statut de connexion, modèles, téléchargement."""
from __future__ import annotations

import asyncio
import json

import httpx
from fastapi import APIRouter, HTTPException, Query
from fastapi.responses import StreamingResponse
from pydantic import BaseModel

from ..config import settings
from ..ollama_client import ollama

router = APIRouter(prefix="/api", tags=["ollama"])

# État en mémoire des préchargements. Loki est lancé avec un worker unique ; ce
# suivi permet au navigateur d'interroger une route courte pendant que le long
# chargement Ollama continue sans maintenir la requête HTTP initiale ouverte.
_warm_states: dict[str, dict[str, str]] = {}
_warm_tasks: dict[str, asyncio.Task[None]] = {}


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


class WarmRequest(BaseModel):
    name: str
    keep_alive: str = "30m"


async def _warm_in_background(name: str, keep_alive: str) -> None:
    try:
        await ollama.warm(name, keep_alive)
        _warm_states[name] = {"state": "loaded"}
    except Exception as exc:
        _warm_states[name] = {
            "state": "error",
            "error": f"préchargement impossible : {str(exc)[:500]}",
        }


def start_model_warm(name: str, keep_alive: str) -> None:
    """Démarre au plus une tâche de préchargement par modèle."""
    current = _warm_tasks.get(name)
    if current and not current.done():
        return
    _warm_states[name] = {"state": "loading"}
    task = asyncio.create_task(_warm_in_background(name, keep_alive))
    _warm_tasks[name] = task

    def forget(done: asyncio.Task[None]) -> None:
        if _warm_tasks.get(name) is done:
            _warm_tasks.pop(name, None)

    task.add_done_callback(forget)


@router.post("/models/warm", status_code=202)
async def warm_model(req: WarmRequest) -> dict:
    """Démarre le préchargement sans exposer sa durée au reverse proxy."""
    name = req.name.strip()
    if not name:
        raise HTTPException(400, "nom de modèle vide")
    start_model_warm(name, req.keep_alive)
    return {"warming": name, "state": _warm_states[name]["state"]}


@router.get("/models/warm/status")
async def warm_status(name: str = Query(min_length=1)) -> dict:
    """État court du préchargement : idle, loading, loaded ou error."""
    return _warm_states.get(name.strip(), {"state": "idle"})


@router.get("/models/loaded")
async def loaded_models() -> dict:
    """Modèles actuellement chargés en mémoire + placement GPU/CPU (/api/ps)."""
    try:
        loaded = await ollama.ps()
    except (httpx.HTTPError, OSError):
        return {"loaded": []}
    result = []
    for m in loaded:
        size = m.get("size", 0) or 0
        vram = m.get("size_vram", 0) or 0
        result.append({
            "name": m.get("name") or m.get("model"),
            "on_gpu": bool(size and vram >= size * 0.99),
            "gpu_percent": int(vram / size * 100) if size else 0,
        })
    return {"loaded": result}


class PullRequest(BaseModel):
    name: str


class DeleteModelRequest(BaseModel):
    name: str


@router.delete("/models")
async def delete_model(req: DeleteModelRequest) -> dict:
    """Supprime explicitement un modèle de l'instance Ollama."""
    if not req.name.strip():
        raise HTTPException(400, "nom de modèle vide")
    try:
        await ollama.delete_model(req.name.strip())
    except httpx.HTTPStatusError as exc:
        detail = exc.response.text[:500] or str(exc)
        raise HTTPException(502, f"Ollama : {detail}") from exc
    except (httpx.HTTPError, OSError) as exc:
        raise HTTPException(502, f"Ollama injoignable : {exc}") from exc
    return {"deleted": req.name.strip()}


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
