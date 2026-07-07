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

from .. import agent_config, coder, db, enhance, memory, rag, router as msg_router
from ..tools import check_html, _safe_path
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


async def _run_aider_keepalive(instruction: str, model: str):
    """Lance Aider dans un thread en gardant le flux SSE vivant."""
    task = asyncio.create_task(
        asyncio.to_thread(coder.run_code_task, instruction, model, None)
    )
    while not task.done():
        await asyncio.sleep(10)
        if not task.done():
            yield None  # signal keepalive
    yield await task


async def _code_stream(
    req: ChatRequest, model: str, *, extra: str = "", plan: list[str] | None = None
):
    """Chemin « moteur code » : Aider + vérification HTML avec auto-correction."""
    instruction = req.content + (extra or "")
    yield _sse("tool_call", {"name": "code_task", "args": {"instruction": req.content}})

    result = None
    async for item in _run_aider_keepalive(instruction, model):
        if item is None:
            yield ": keepalive\n\n"
        else:
            result = item

    status = "ok" if result.get("ok") else "error"
    record = {
        "name": "code_task",
        "args": {"instruction": req.content},
        "summary": result.get("summary", "terminé"),
        "status": status,
    }
    tools_meta = [record]
    yield _sse("tool_result", record)

    all_files = list(result.get("files") or [])

    # Vérification des pages HTML produites + une passe d'auto-correction.
    html_issues: list[str] = []
    for f in all_files:
        if f.lower().endswith((".html", ".htm")):
            try:
                issues = check_html(_safe_path(f))
            except Exception:
                issues = []
            if issues:
                html_issues.append(f"{f} : " + " ; ".join(issues))

    if html_issues and result.get("ok"):
        yield _sse("tool_call", {"name": "html_check", "args": {"path": "vérification"}})
        yield _sse("tool_result", {
            "name": "html_check", "args": {"path": "vérification"},
            "summary": " | ".join(html_issues)[:200], "status": "error",
        })
        tools_meta.append({
            "name": "html_check", "args": {},
            "summary": " | ".join(html_issues)[:200], "status": "error",
        })
        fix_instruction = (
            "Corrige ces problèmes détectés dans les fichiers HTML, sans rien "
            "casser d'autre :\n" + "\n".join(html_issues)
        )
        fix = None
        async for item in _run_aider_keepalive(fix_instruction, model):
            if item is None:
                yield ": keepalive\n\n"
            else:
                fix = item
        fix_rec = {
            "name": "code_task",
            "args": {"instruction": "auto-correction HTML"},
            "summary": fix.get("summary", "terminé"),
            "status": "ok" if fix.get("ok") else "error",
        }
        tools_meta.append(fix_rec)
        yield _sse("tool_call", {"name": "code_task", "args": fix_rec["args"]})
        yield _sse("tool_result", fix_rec)
        for f in fix.get("files") or []:
            if f not in all_files:
                all_files.append(f)
        if fix.get("text"):
            result["text"] = (result.get("text") or "") + "\n\n" + fix["text"]

    # Cartes par fichier modifié (réutilise le rendu write_file de l'UI).
    for f in all_files:
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

    meta: dict = {"tools": tools_meta, "engine": "code"}
    if plan:
        meta["plan"] = plan
    db.add_message(req.session_id, "assistant", text, model, meta=meta)
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

    # Mémoire compressée : système + résumé des anciens tours + messages récents.
    convo = memory.build_convo(req.session_id, cfg["system_prompt"])

    # Mémoire long-terme (RAG) : souvenirs pertinents des autres sessions.
    if cfg.get("rag_enabled", True):
        memories = await rag.recall(
            req.session_id, req.content, embed_model=cfg.get("embed_model")
        )
        if memories:
            convo.insert(1, {
                "role": "system",
                "content": "Souvenirs pertinents d'anciennes sessions :\n"
                + "\n---\n".join(memories),
            })

    # Moteur code : choisit le meilleur modèle code installé (config "auto").
    code_model = (
        await coder.pick_code_model(model, cfg.get("code_model"))
        if use_code else model
    )

    # Plan-puis-exécute : les demandes complexes sont décomposées d'abord.
    plan: list[str] = []
    if cfg.get("plan_mode", True) and (use_code or enhance.needs_plan(req.content)):
        plan = await enhance.make_plan(model, req.content)

    async def event_stream():
        yield _sse("start", {"model": model, "engine": "code" if use_code else "agent"})

        if plan:
            yield _sse("plan", {"steps": plan})

        # Chemin « moteur code » : Aider gère la tâche de bout en bout.
        if use_code:
            instruction_plan = (
                "\n\nPlan à suivre :\n"
                + "\n".join(f"{i+1}. {s}" for i, s in enumerate(plan))
                if plan else ""
            )
            async for chunk in _code_stream(req, code_model, extra=instruction_plan, plan=plan):
                yield chunk
            asyncio.create_task(memory.maybe_summarize(req.session_id, model))
            if cfg.get("rag_enabled", True):
                last = db.list_messages(req.session_id)
                answer = last[-1]["content"] if last else ""
                asyncio.create_task(rag.index_exchange(
                    req.session_id, req.content, answer,
                    embed_model=cfg.get("embed_model"),
                ))
            return

        if plan:
            convo.append({
                "role": "system",
                "content": "Plan à suivre pour cette demande :\n"
                + "\n".join(f"{i+1}. {s}" for i, s in enumerate(plan)),
            })

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

        # Auto-critique : relecture éclair puis révision (option « Qualité + »).
        if (
            cfg.get("self_review", False)
            and final_content
            and not error_message
            and not tools_meta
        ):
            yield _sse("status", {"message": "Relecture de la réponse…"})
            revised = await enhance.self_review(model, req.content, final_content)
            if revised:
                final_content = revised
                yield _sse("revision", {"content": revised})
                yield _sse("notice", {"message": "Réponse révisée après auto-critique ✓"})

        if final_content or tools_meta:
            meta: dict = {}
            if tools_meta:
                meta["tools"] = tools_meta
            if stats_meta:
                meta["stats"] = stats_meta
            if thinking_meta:
                meta["thinking"] = thinking_meta
            if plan:
                meta["plan"] = plan
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
        # Tâches d'arrière-plan : compression de l'historique + mémoire RAG.
        asyncio.create_task(memory.maybe_summarize(req.session_id, model))
        if cfg.get("rag_enabled", True) and final_content:
            asyncio.create_task(rag.index_exchange(
                req.session_id, req.content, final_content,
                embed_model=cfg.get("embed_model"),
            ))

    return StreamingResponse(
        event_stream(),
        media_type="text/event-stream",
        headers={
            "Cache-Control": "no-cache, no-transform",
            "X-Accel-Buffering": "no",
        },
    )
