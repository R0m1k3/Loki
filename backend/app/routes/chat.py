"""Route de conversation : streaming token par token depuis Ollama (SSE).

Flux :
  1. on enregistre le message utilisateur ;
  2. on rejoue l'historique de la session vers Ollama en streaming ;
  3. on relaie chaque token au client (SSE) ;
  4. on enregistre la réponse complète de l'assistant.
"""
from __future__ import annotations

import json

import httpx
from fastapi import APIRouter, HTTPException
from fastapi.responses import StreamingResponse
from pydantic import BaseModel

from .. import db
from ..config import settings
from ..ollama_client import ollama

router = APIRouter(prefix="/api", tags=["chat"])

SYSTEM_PROMPT = (
    "Tu es Loki, un assistant de développement local. Tu écris du code clair, "
    "commenté en français, et tu réponds de façon concise et utile."
)


class ChatRequest(BaseModel):
    session_id: str
    content: str
    model: str | None = None
    options: dict | None = None


def _sse(event: str, data: dict) -> str:
    return f"event: {event}\ndata: {json.dumps(data, ensure_ascii=False)}\n\n"


@router.post("/chat")
async def chat(req: ChatRequest) -> StreamingResponse:
    session = db.get_session(req.session_id)
    if not session:
        raise HTTPException(404, "session introuvable")

    model = req.model or session.get("model") or settings.default_model

    # Premier message : on titre la session avec un extrait.
    history = db.list_messages(req.session_id)
    if not history:
        title = req.content.strip().split("\n")[0][:60] or "Nouvelle session"
        db.rename_session(req.session_id, title)

    db.add_message(req.session_id, "user", req.content, None)

    # Contexte envoyé à Ollama : invite système + historique complet.
    convo = [{"role": "system", "content": SYSTEM_PROMPT}]
    convo += [
        {"role": m["role"], "content": m["content"]}
        for m in db.list_messages(req.session_id)
    ]

    async def event_stream():
        yield _sse("start", {"model": model})
        full = ""
        try:
            async for chunk in ollama.chat(
                model, convo, options=req.options, stream=True
            ):
                token = chunk.get("message", {}).get("content", "")
                if token:
                    full += token
                    yield _sse("token", {"content": token})
                if chunk.get("done"):
                    break
        except (httpx.HTTPError, OSError) as exc:
            yield _sse("error", {"message": str(exc)})

        if full:
            db.add_message(req.session_id, "assistant", full, model)
        yield _sse("done", {"content": full, "model": model})

    return StreamingResponse(event_stream(), media_type="text/event-stream")
