"""Route de conversation : routage automatique agent / moteur code + SSE.

Flux :
  1. on enregistre le message utilisateur ;
  2. le routeur classe la demande : tâche de code -> moteur code (Aider),
     sinon -> boucle agentique classique (qui peut elle-même appeler code_task) ;
  3. on relaie tokens et événements d'outils au client (SSE) ;
  4. on enregistre la réponse finale de l'assistant + le récap des outils.
"""
from __future__ import annotations

import asyncio
import json
import logging
from contextlib import suppress

from fastapi import APIRouter, HTTPException
from fastapi.responses import StreamingResponse
from pydantic import BaseModel

from .. import agent_config, coder, db, router as msg_router
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


async def _code_stream(req: ChatRequest, model: str):
    """Chemin « moteur code » : Aider travaille, on garde le flux SSE vivant."""
    instruction = req.content
    yield _sse("tool_call", {"name": "code_task", "args": {"instruction": instruction}})

    task = asyncio.create_task(
        asyncio.to_thread(coder.run_code_task, instruction, model, None)
    )
    # Keepalive SSE pendant le travail (peut durer plusieurs minutes).
    while not task.done():
        await asyncio.sleep(10)
        if not task.done():
            yield ": keepalive\n\n"
    result = await task

    status = "ok" if result.get("ok") else "error"
    record = {
        "name": "code_task",
        "args": {"instruction": instruction},
        "summary": result.get("summary", "terminé"),
        "status": status,
    }
    tools_meta = [record]
    yield _sse("tool_result", record)

    # Cartes par fichier modifié (réutilise le rendu write_file de l'UI).
    for f in result.get("files") or []:
        file_rec = {
            "name": "write_file",
            "args": {"path": f},
            "summary": "modifié par le moteur code",
            "status": "ok",
        }
        tools_meta.append(file_rec)
        yield _sse("tool_call", {"name": "write_file", "args": {"path": f}})
        yield _sse("tool_result", file_rec)

    text = result.get("text") or (
        "" if result.get("ok") else f"⚠️ {result.get('summary', 'échec du moteur code')}"
    )
    if text:
        yield _sse("token", {"content": text})

    db.add_message(
        req.session_id, "assistant", text, model,
        meta={"tools": tools_meta, "engine": "code"},
    )
    yield _sse("done", {"content": text, "tools": tools_meta})


@router.post("/chat")
async def chat(req: ChatRequest) -> StreamingResponse:
    session = db.get_session(req.session_id)
    if not session:
        raise HTTPException(404, "session introuvable")

    model = req.model or session.get("model") or settings.default_model
    cfg = agent_config.get_config(model)

    # Premier message : titre la session avec un extrait.
    if not db.list_messages(req.session_id):
        title = req.content.strip().split("\n")[0][:60] or "Nouvelle session"
        db.rename_session(req.session_id, title)

    db.add_message(req.session_id, "user", req.content, None)

    # Routage automatique : moteur code si la demande est une tâche de code,
    # que l'outil est actif et qu'Aider est disponible.
    use_code = (
        cfg["tools"].get("code_task", True)
        and coder.available()
        and await msg_router.is_code_task(req.content, model)
    )

    convo = [{"role": "system", "content": cfg["system_prompt"]}]
    convo += db.list_messages_for_model(req.session_id)

    async def event_stream():
        yield _sse("start", {"model": model, "engine": "code" if use_code else "agent"})

        # Chemin « moteur code » : Aider gère la tâche de bout en bout.
        if use_code:
            async for chunk in _code_stream(req, model):
                yield chunk
            return

        final_content = ""
        tools_meta: list[dict] = []
        stats_meta: dict | None = None
        thinking_meta = ""
        error_message = ""

        queue: asyncio.Queue[dict | None] = asyncio.Queue()

        async def produce_events() -> None:
            try:
                async for event in run_agent(
                    model,
                    convo,
                    options=agent_config.ollama_options(cfg),
                    enabled_tools=agent_config.enabled_tool_names(cfg),
                    confirm_shell=cfg.get("confirm_shell", True),
                    think=cfg.get("think", True),
                ):
                    await queue.put(event)
            except Exception as exc:
                logger.exception("Échec inattendu du flux de chat")
                await queue.put(
                    {
                        "type": "error",
                        "message": f"Erreur interne du chat : {exc}",
                    }
                )
            finally:
                await queue.put(None)

        producer = asyncio.create_task(produce_events())
        try:
            while True:
                try:
                    ev = await asyncio.wait_for(queue.get(), timeout=15.0)
                except TimeoutError:
                    # Empêche OpenResty/Nginx/Cloudflare de fermer le SSE pendant
                    # le chargement parfois long d'un modèle Ollama.
                    yield _sse("ping", {"status": "waiting"})
                    continue

                if ev is None:
                    break

                etype = ev.pop("type")
                if etype in (
                    "token",
                    "thinking",
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
                    stats_meta = ev.get("stats")
                    thinking_meta = ev.get("thinking", "")
        finally:
            if not producer.done():
                producer.cancel()
            with suppress(asyncio.CancelledError):
                await producer

        if final_content or tools_meta:
            meta: dict = {}
            if tools_meta:
                meta["tools"] = tools_meta
            if stats_meta:
                meta["stats"] = stats_meta
            if thinking_meta:
                meta["thinking"] = thinking_meta
            db.add_message(
                req.session_id,
                "assistant",
                final_content,
                model,
                meta=meta or None,
            )
        yield _sse(
            "done",
            {
                "content": final_content,
                "tools": tools_meta,
                "stats": stats_meta,
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
