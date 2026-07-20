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
import os
import re
from contextlib import suppress

from fastapi import APIRouter, HTTPException
from fastapi.responses import StreamingResponse
from pydantic import BaseModel

from .. import agent_config, coder, db, enhance, memory, rag, skills, tools
from .. import router as msg_router
from ..tools import check_html, _safe_path
from ..agent import run_agent
from ..config import settings

router = APIRouter(prefix="/api", tags=["chat"])
logger = logging.getLogger(__name__)


class ChatRequest(BaseModel):
    session_id: str
    content: str
    model: str | None = None
    # Mode d'exécution : "plan" (lecture seule), "build" (normal), "yolo" (auto).
    mode: str = "build"


# Outils autorisés en mode Plan : lecture/analyse uniquement.
_READONLY_TOOLS = {"read_file", "list_dir", "grep_search", "run_check"}


def _apply_mode(cfg: dict, mode: str) -> dict:
    """Adapte la config au mode d'exécution choisi dans le composer."""
    cfg = dict(cfg)
    if mode == "plan":
        # Lecture seule : aucune écriture, aucun shell, aucun moteur code.
        cfg["tools"] = {
            name: (on and name in _READONLY_TOOLS)
            for name, on in cfg["tools"].items()
        }
        cfg["plan_mode"] = True
    elif mode == "yolo":
        # Autonomie maximale : plus de validation shell.
        cfg["confirm_shell"] = False
    # "build" : comportement par défaut (inchangé).
    return cfg


def _sse(event: str, data: dict) -> str:
    return f"event: {event}\ndata: {json.dumps(data, ensure_ascii=False)}\n\n"


def _prev_was_code(history: list[dict]) -> bool:
    """Le dernier tour assistant de la session était-il un travail de code ?"""
    for m in reversed(history):
        if m["role"] != "assistant":
            continue
        meta = m.get("meta") or {}
        if meta.get("engine") == "code":
            return True
        tools = meta.get("tools") or []
        return any(
            t.get("name") in ("code_task", "write_file", "edit_file")
            for t in tools
        )
    return False


def _session_code_context(history: list[dict]) -> tuple[str, list[str]]:
    """Récap compact du travail en cours + fichiers touchés dans la session.

    Le moteur code ne reçoit que le message courant : sur une reprise
    (« corrige les bugs »), sans ce récap il ignore quel fichier, quel projet
    et quelle demande d'origine. Les fichiers touchés servent aussi de cible
    par défaut pour Aider.
    """
    root = tools.active_root()
    files: list[str] = []
    for m in history:
        if m["role"] != "assistant":
            continue
        for t in (m.get("meta") or {}).get("tools") or []:
            candidates: list[str] = []
            path = (t.get("args") or {}).get("path")
            if t.get("name") in ("write_file", "edit_file") and path:
                candidates.append(str(path))
            for f in t.get("files") or []:
                candidates.append(str(f))
            for c in candidates:
                rel = c.replace("\\", "/").lstrip("./")
                if rel not in files and os.path.isfile(os.path.join(root, rel)):
                    files.append(rel)

    user_msgs = [m["content"].strip() for m in history if m["role"] == "user"]
    lines: list[str] = []
    if user_msgs:
        lines.append(f"- Demande initiale : {user_msgs[0][:200]}")
        for prev in user_msgs[-2:]:
            if prev != user_msgs[0]:
                lines.append(f"- Puis : {prev[:200]}")
    if files:
        lines.append(f"- Fichiers déjà créés/modifiés : {', '.join(files[:8])}")
    recap = (
        "Contexte de la session (travail en cours) :\n" + "\n".join(lines)
        if lines else ""
    )
    return recap, files


def _workspace_listing(limit: int = 40) -> list[str]:
    """Chemins relatifs des fichiers de la racine active (aperçu compact)."""
    root = tools.active_root()
    out: list[str] = []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if not d.startswith(".")]
        for name in sorted(filenames):
            if name.startswith("."):
                continue
            rel = os.path.relpath(os.path.join(dirpath, name), root)
            out.append(rel.replace("\\", "/"))
            if len(out) >= limit:
                return out
    return out


_FILE_MENTION = re.compile(r"[\w][\w./\\-]*\.[a-z0-9]{1,5}\b", re.I)


def _mentioned_files(text: str) -> list[str]:
    """Fichiers du workspace explicitement cités dans le message.

    Transmis au moteur code pour qu'Aider travaille directement sur les bons
    fichiers au lieu de deviner via la repo map.
    """
    root = tools.active_root()
    out: list[str] = []
    for raw in _FILE_MENTION.findall(text):
        rel = raw.replace("\\", "/").lstrip("./")
        if os.path.isfile(os.path.join(root, rel)) and rel not in out:
            out.append(rel)
    return out[:8]


async def _run_aider_keepalive(
    instruction: str, model: str, files: list[str] | None = None
):
    """Lance Aider dans un thread en gardant le flux SSE vivant."""
    # Racine résolue AVANT le thread : la contextvar projet ne suit pas
    # dans asyncio.to_thread.
    root = tools.active_root()
    task = asyncio.create_task(
        asyncio.to_thread(coder.run_code_task, instruction, model, files, root)
    )
    while not task.done():
        await asyncio.sleep(10)
        if not task.done():
            yield None  # signal keepalive
    yield await task


async def _code_stream(
    req: ChatRequest,
    model: str,
    *,
    extra: str = "",
    plan: list[str] | None = None,
    files: list[str] | None = None,
):
    """Chemin « moteur code » : Aider + vérification HTML avec auto-correction."""
    instruction = req.content + (extra or "")
    yield _sse("tool_call", {"name": "code_task", "args": {"instruction": req.content}})

    result = None
    async for item in _run_aider_keepalive(instruction, model, files):
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
    cfg = _apply_mode(agent_config.get_config(model), req.mode)

    # Projet de la session : re-racine outils, shell, Aider et aides de
    # contexte pour tout le tour. Projet disparu -> retour racine + notice.
    project = session.get("project") or None
    project_missing = False
    if project:
        proj_dir = os.path.join(os.path.abspath(settings.workspace_dir), project)
        if not os.path.isdir(proj_dir):
            project_missing, project = True, None
    tools.set_project(project)

    history = db.list_messages(req.session_id)

    # Premier message : titre la session avec un extrait.
    if not history:
        title = req.content.strip().split("\n")[0][:60] or "Nouvelle session"
        db.rename_session(req.session_id, title)

    db.add_message(req.session_id, "user", req.content, None)

    # Routage automatique : moteur code si la demande est une tâche de code —
    # ou la SUITE d'un travail de code (« ajoute un bouton », « continue »…),
    # que l'heuristique seule classerait à tort en discussion.
    prev_code = _prev_was_code(history)
    use_code = (
        cfg["tools"].get("code_task", True)
        and coder.available()
        and (
            msg_router.is_code_task(req.content)
            or (prev_code and msg_router.is_code_followup(req.content))
        )
    )

    # Mémoire compressée : système + résumé des anciens tours + messages récents.
    convo = memory.build_convo(req.session_id, cfg["system_prompt"])

    # Options runner partagées par TOUS les appels au modèle de chat (plan,
    # résumé, agent) : indispensables pour qu'Ollama garde le même runner.
    run_opts = agent_config.runner_options(cfg)
    keep = cfg.get("keep_alive", "30m")

    async def event_stream():
        yield _sse("start", {"model": model, "engine": "code" if use_code else "agent"})

        if project_missing:
            yield _sse("notice", {"message": (
                "Projet de la session introuvable sur le disque — retour au "
                "workspace."
            )})

        # Préparation du contexte APRÈS le start SSE et en PARALLÈLE : rappel
        # RAG, plan et choix du modèle code partent ensemble au lieu de
        # s'enchaîner en bloquant le premier token.
        want_rag = cfg.get("rag_enabled", False)
        want_plan = cfg.get("plan_mode", True) and (
            use_code or enhance.needs_plan(req.content)
        )
        if want_rag or want_plan or use_code:
            yield _sse("status", {"message": "Préparation du contexte…"})

        from ..mcp_client import manager as mcp_manager

        # La préparation peut déclencher un CHARGEMENT de modèle (plan, embed)
        # qui dure plusieurs minutes sur un gros modèle. Sans battement de
        # cœur, le silence SSE fait couper la connexion par le reverse proxy.
        prep = asyncio.ensure_future(asyncio.gather(
            rag.recall(req.session_id, req.content, embed_model=cfg.get("embed_model"))
            if want_rag else asyncio.sleep(0, result=[]),
            enhance.make_plan(model, req.content, options=run_opts, keep_alive=keep)
            if want_plan else asyncio.sleep(0, result=[]),
            coder.pick_code_model(model, cfg.get("code_model"))
            if use_code else asyncio.sleep(0, result=model),
            mcp_manager.tool_definitions(),
        ))
        while True:
            try:
                memories, plan, code_model, mcp_tools = await asyncio.wait_for(
                    asyncio.shield(prep), timeout=10.0
                )
                break
            except TimeoutError:
                yield _sse("ping", {"status": "waiting"})

        # Pannes MCP éventuelles : notice non bloquante dans le fil.
        for mcp_notice in mcp_manager.notices():
            yield _sse("notice", {"message": mcp_notice})

        if memories:
            convo.insert(1, {
                "role": "system",
                "content": "Souvenirs pertinents d'anciennes sessions :\n"
                + "\n---\n".join(memories),
            })

        # État du workspace injecté chaque tour : sans ça le modèle ignore
        # quels fichiers existent et régurgite du code en chat au lieu de
        # modifier le bon fichier.
        listing = _workspace_listing()
        if listing:
            recap, session_files = _session_code_context(history)
            parts = ["Fichiers du workspace : " + ", ".join(listing)]
            if session_files:
                parts.append(
                    "Fichiers de la tâche en cours : " + ", ".join(session_files[:8])
                )
            convo.insert(1, {"role": "system", "content": "\n".join(parts)})

        # Session code restée en chemin agent : pousse le modèle à AGIR sur
        # les fichiers au lieu de décrire les changements — cause fréquente de
        # « l'agent s'arrête sans rien modifier » sur une reprise de code.
        if prev_code and not use_code:
            convo.insert(1, {
                "role": "system",
                "content": (
                    "Cette session travaille sur du code existant du workspace. "
                    "Pour toute demande de modification ou d'ajout : AGIS avec "
                    "les outils — code_task pour un changement multi-fichiers, "
                    "edit_file pour un changement ciblé, write_file pour un "
                    "nouveau fichier. Lis le fichier concerné avant de le "
                    "modifier, puis modifie-le RÉELLEMENT. Ne colle JAMAIS le "
                    "code corrigé dans ta réponse sans l'avoir écrit dans le "
                    "fichier."
                ),
            })

        # Skill : méthode experte injectée pour ce tour (jamais persistée).
        if cfg.get("skills_enabled", True):
            skill = skills.pick_skill(req.content)
            if skill:
                convo.insert(1, {
                    "role": "system",
                    "content": (
                        "Méthode à suivre pour cette tâche :\n" + skill["body"]
                    ),
                })
                yield _sse("notice", {"message": f"📘 Méthode : {skill['title']}"})

        if plan:
            yield _sse("plan", {"steps": plan})

        # Chemin « moteur code » : Aider gère la tâche de bout en bout.
        if use_code:
            instruction_plan = (
                "\n\nPlan à suivre :\n"
                + "\n".join(f"{i+1}. {s}" for i, s in enumerate(plan))
                if plan else ""
            )
            # Reprise : Aider ne voit que le message courant — on lui donne le
            # récap de session et, à défaut de fichiers cités, ceux déjà
            # touchés (« corrige les bugs » => il ouvre le bon fichier).
            recap, session_files = _session_code_context(history)
            extra = instruction_plan
            if recap:
                extra = f"\n\n{recap}" + extra
            code_files = _mentioned_files(req.content) or session_files[:8]
            async for chunk in _code_stream(
                req, code_model, extra=extra, plan=plan, files=code_files,
            ):
                yield chunk
            asyncio.create_task(memory.maybe_summarize(
                req.session_id, model, options=run_opts, keep_alive=keep,
            ))
            if cfg.get("rag_enabled", False):
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
                    keep_alive=cfg.get("keep_alive", "30m"),
                    mcp_tools=mcp_tools,
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
            revised = await enhance.self_review(
                model, req.content, final_content,
                options=run_opts, keep_alive=keep,
            )
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
        asyncio.create_task(memory.maybe_summarize(
            req.session_id, model, options=run_opts, keep_alive=keep,
        ))
        if cfg.get("rag_enabled", False) and final_content:
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
