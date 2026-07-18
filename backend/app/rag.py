"""Mémoire long-terme (RAG) : l'agent se souvient des anciennes sessions.

Chaque échange (question + réponse) est vectorisé via /api/embed d'Ollama et
stocké en SQLite. À chaque nouveau message, on recherche les souvenirs les
plus proches (cosinus) dans les AUTRES sessions et on les injecte en contexte.

Tout est best-effort : sans modèle d'embedding installé, le RAG se désactive
silencieusement (aucun impact sur le chat).
"""
from __future__ import annotations

import asyncio
import json
import logging
import math
import time
import uuid

import httpx

from . import db
from .ollama_client import ollama

logger = logging.getLogger(__name__)

# Modèles d'embedding reconnus, par ordre de préférence.
_EMBED_HINTS = ("nomic-embed", "mxbai-embed", "bge-", "snowflake-arctic-embed",
                "all-minilm", "embed")

_TOP_K = 3
_MIN_SCORE = 0.45
_MAX_MEMORIES = 2000  # au-delà, on élague les plus anciens

_embed_model_cache: dict = {"value": None, "checked_at": 0.0}


def init_table() -> None:
    with db._LOCK, db._connect() as conn:
        conn.execute(
            """
            CREATE TABLE IF NOT EXISTS memories (
                id          TEXT PRIMARY KEY,
                session_id  TEXT NOT NULL,
                content     TEXT NOT NULL,
                embedding   TEXT NOT NULL,
                created_at  REAL NOT NULL
            )
            """
        )


async def resolve_embed_model(preference: str | None = None) -> str | None:
    """Trouve le modèle d'embedding à utiliser (None = RAG indisponible)."""
    if preference and preference != "auto":
        return preference

    # Cache 60 s pour ne pas marteler /api/tags.
    now = time.time()
    if now - _embed_model_cache["checked_at"] < 60:
        return _embed_model_cache["value"]

    value = None
    try:
        for m in await ollama.list_models_cached():
            name = (m.get("name") or "").lower()
            if any(h in name for h in _EMBED_HINTS):
                value = m["name"]
                break
    except (httpx.HTTPError, OSError):
        value = None

    _embed_model_cache.update(value=value, checked_at=now)
    return value


def _cosine(a: list[float], b: list[float]) -> float:
    dot = sum(x * y for x, y in zip(a, b))
    na = math.sqrt(sum(x * x for x in a))
    nb = math.sqrt(sum(x * x for x in b))
    return dot / (na * nb) if na and nb else 0.0


def _score_rows(qvec: list[float], rows: list) -> list[str]:
    """Scoring cosinus sur toutes les mémoires — CPU pur, à lancer via to_thread.

    Jusqu'à _MAX_MEMORIES vecteurs : la boucle Python bloquerait l'event loop
    (et donc tous les SSE en cours) pendant plusieurs dizaines de ms.
    """
    scored: list[tuple[float, str]] = []
    for row in rows:
        try:
            score = _cosine(qvec, json.loads(row["embedding"]))
        except (ValueError, TypeError):
            continue
        if score >= _MIN_SCORE:
            scored.append((score, row["content"]))
    scored.sort(reverse=True)
    return [c for _, c in scored[:_TOP_K]]


async def index_exchange(
    sid: str, user_text: str, assistant_text: str, *, embed_model: str | None
) -> None:
    """Indexe un échange terminé (tâche d'arrière-plan, best-effort)."""
    model = await resolve_embed_model(embed_model)
    if not model:
        return
    content = f"Q: {user_text[:500]}\nR: {assistant_text[:800]}"
    try:
        vectors = await ollama.embed(model, [content])
        if not vectors:
            return
        with db._LOCK, db._connect() as conn:
            conn.execute(
                "INSERT INTO memories (id, session_id, content, embedding, created_at)"
                " VALUES (?, ?, ?, ?, ?)",
                (uuid.uuid4().hex, sid, content,
                 json.dumps(vectors[0]), time.time()),
            )
            # Élagage des souvenirs les plus anciens.
            conn.execute(
                "DELETE FROM memories WHERE id IN ("
                " SELECT id FROM memories ORDER BY created_at DESC"
                f" LIMIT -1 OFFSET {_MAX_MEMORIES})"
            )
    except (httpx.HTTPError, OSError) as exc:
        logger.warning("Indexation RAG impossible : %s", exc)


async def recall(
    sid: str, query: str, *, embed_model: str | None
) -> list[str]:
    """Souvenirs pertinents issus des AUTRES sessions (top-k, score minimal)."""
    model = await resolve_embed_model(embed_model)
    if not model:
        return []
    try:
        vectors = await ollama.embed(model, [query[:800]])
        if not vectors:
            return []
        qvec = vectors[0]

        with db._LOCK, db._connect() as conn:
            rows = conn.execute(
                "SELECT content, embedding FROM memories WHERE session_id != ?",
                (sid,),
            ).fetchall()

        return await asyncio.to_thread(_score_rows, qvec, rows)
    except (httpx.HTTPError, OSError) as exc:
        logger.warning("Rappel RAG impossible : %s", exc)
        return []
