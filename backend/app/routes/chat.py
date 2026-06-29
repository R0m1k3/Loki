"""Route de conversation agentique : boucle d'outils + streaming SSE.

Flux :
  1. on enregistre le message utilisateur ;
  2. on lance la boucle agentique (run_agent) sur l'historique de la session ;
  3. on relaie tokens et événements d'outils au client (SSE) ;
  4. on enregistre la réponse finale de l'assistant + le récap des outils.
"""
from __future__ import annotations

import json

from fastapi import APIRouter, HTTPException
from fastapi.responses import StreamingResponse
from pydantic import BaseModel

from .. import db
from ..agent import run_agent
from ..config import settings

router = APIRouter(prefix="/api", tags=["chat"])

SYSTEM_PROMPT = (
    "Tu es Loki, un assistant de développement local agentique. Tu disposes "
    "d'outils pour lire, écrire et lister des fichiers dans le workspace. "
    "Utilise-les pour accomplir les tâches concrètement, puis réponds de façon "
    "concise en français. Après avoir écrit un fichier, propose un aperçu."
)


class ChatRequest(BaseModel):
    session_id: str
    content: str
    model: str | None = None
    options: dict | None = None
    tools_enabled: bool = True


def _sse(event: str, data: dict) -> str:
    return f"event: {event}\ndata: {json.dumps(data, ensure_ascii=False)}\n\n"


@router.post("/chat")
async def chat(req: ChatRequest) -> StreamingResponse:
    session = db.get_session(req.session_id)
    if not session:
        raise HTTPException(404, "session introuvable")

    model = req.model or session.get("model") or settings.default_model

    # Premier message : titre la session avec un extrait.
    if not db.list_messages(req.session_id):
        title = req.content.strip().split("\n")[0][:60] or "Nouvelle session"
        db.rename_session(req.session_id, title)

    db.add_message(req.session_id, "user", req.content, None)

    convo = [{"role": "system", "content": SYSTEM_PROMPT}]
    convo += db.list_messages_for_model(req.session_id)

    async def event_stream():
        yield _sse("start", {"model": model})
        final_content = ""
        tools_meta: list[dict] = []

        async for ev in run_agent(
            model, convo, options=req.options, tools_enabled=req.tools_enabled
        ):
            etype = ev.pop("type")
            if etype == "token":
                yield _sse("token", ev)
            elif etype == "tool_call":
                yield _sse("tool_call", ev)
            elif etype == "tool_result":
                yield _sse("tool_result", ev)
            elif etype == "error":
                yield _sse("error", ev)
            elif etype == "final":
                final_content = ev["content"]
                tools_meta = ev["tools"]

        if final_content or tools_meta:
            db.add_message(
                req.session_id,
                "assistant",
                final_content,
                model,
                meta={"tools": tools_meta} if tools_meta else None,
            )
        yield _sse("done", {"content": final_content, "tools": tools_meta})

    return StreamingResponse(event_stream(), media_type="text/event-stream")
