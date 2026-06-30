"""Route de conversation agentique : boucle d'outils + streaming SSE.

Flux :
  1. on enregistre le message utilisateur ;
  2. on lance la boucle agentique (run_agent) sur l'historique de la session ;
  3. on relaie tokens et événements d'outils au client (SSE) ;
  4. on enregistre la réponse finale de l'assistant + le récap des outils.
"""
from __future__ import annotations

import json
import logging

from fastapi import APIRouter, HTTPException
from fastapi.responses import StreamingResponse
from pydantic import BaseModel

from .. import agent_config, db
from ..agent import run_agent
from ..config import settings

router = APIRouter(prefix="/api", tags=["chat"])
logger = logging.getLogger(__name__)


class ChatRequest(BaseModel):
    session_id: str
    content: str
    model: str | None = None


def _sse(event: str, data: dict) -> str:
    return f"event: {event}\ndata: {json.dumps(data, ensure_ascii=False)}\n\n"


@router.post("/chat")
async def chat(req: ChatRequest) -> StreamingResponse:
    session = db.get_session(req.session_id)
    if not session:
        raise HTTPException(404, "session introuvable")

    model = req.model or session.get("model") or settings.default_model
    cfg = agent_config.get_config()

    # Premier message : titre la session avec un extrait.
    if not db.list_messages(req.session_id):
        title = req.content.strip().split("\n")[0][:60] or "Nouvelle session"
        db.rename_session(req.session_id, title)

    db.add_message(req.session_id, "user", req.content, None)

    convo = [{"role": "system", "content": cfg["system_prompt"]}]
    convo += db.list_messages_for_model(req.session_id)

    async def event_stream():
        yield _sse("start", {"model": model})
        final_content = ""
        tools_meta: list[dict] = []
        error_message = ""

        try:
            async for ev in run_agent(
                model,
                convo,
                options=agent_config.ollama_options(cfg),
                enabled_tools=agent_config.enabled_tool_names(cfg),
                confirm_shell=cfg.get("confirm_shell", True),
            ):
                etype = ev.pop("type")
                if etype in (
                    "token",
                    "status",
                    "notice",
                    "tool_call",
                    "tool_result",
                    "tool_confirm",
                ):
                    yield _sse(etype, ev)
                elif etype == "error":
                    error_message = ev.get("message", "Erreur Ollama inconnue")
                    yield _sse("error", {"message": error_message})
                elif etype == "final":
                    final_content = ev["content"]
                    tools_meta = ev["tools"]
        except Exception as exc:
            # Après l'envoi des en-têtes SSE, une exception non gérée coupe le
            # socket sans explication et laisse l'interface bloquée.
            logger.exception("Échec inattendu du flux de chat")
            error_message = f"Erreur interne du chat : {exc}"
            yield _sse("error", {"message": error_message})

        if final_content or tools_meta:
            db.add_message(
                req.session_id,
                "assistant",
                final_content,
                model,
                meta={"tools": tools_meta} if tools_meta else None,
            )
        yield _sse(
            "done",
            {
                "content": final_content,
                "tools": tools_meta,
                "error": error_message or None,
            },
        )

    return StreamingResponse(
        event_stream(),
        media_type="text/event-stream",
        headers={
            "Cache-Control": "no-cache, no-transform",
            "X-Accel-Buffering": "no",
        },
    )
