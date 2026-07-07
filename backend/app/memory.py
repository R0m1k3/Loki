"""Mémoire de conversation compressée — le vrai levier des petits modèles.

Un petit modèle se noie dans un long historique : il oublie la consigne, part
en boucle, et un grand num_ctx le fait déborder du GPU. On garde donc :
  [invite système] + [résumé compact des anciens tours] + [N derniers messages]

Le résumé est régénéré en arrière-plan (après la réponse, sans latence pour
l'utilisateur) dès que l'historique dépasse le seuil.
"""
from __future__ import annotations

import logging

import httpx

from . import db
from .ollama_client import ollama

logger = logging.getLogger(__name__)

# Nombre de messages récents passés tels quels au modèle.
KEEP_RECENT = 10
# Au-delà de ce total, les anciens tours sont compressés dans le résumé.
SUMMARIZE_AFTER = KEEP_RECENT + 6

_SUMMARY_PROMPT = (
    "Résume la conversation ci-dessous en français, en 10 lignes maximum. "
    "Conserve impérativement : l'objectif de l'utilisateur, les décisions "
    "prises, les fichiers créés/modifiés et leur rôle, et les points encore "
    "ouverts. Réponds UNIQUEMENT par le résumé."
)


def build_convo(sid: str, system_prompt: str) -> list[dict]:
    """Construit le contexte : système + résumé éventuel + messages récents."""
    convo: list[dict] = [{"role": "system", "content": system_prompt}]

    session = db.get_session(sid) or {}
    summary = (session.get("summary") or "").strip()
    messages = db.list_messages_for_model(sid)

    if summary and len(messages) > KEEP_RECENT:
        convo.append(
            {
                "role": "system",
                "content": f"Résumé des échanges précédents :\n{summary}",
            }
        )
        convo += messages[-KEEP_RECENT:]
    else:
        convo += messages
    return convo


async def maybe_summarize(sid: str, model: str) -> None:
    """Compresse les anciens tours dans le résumé (tâche d'arrière-plan)."""
    try:
        messages = db.list_messages_for_model(sid)
        if len(messages) <= SUMMARIZE_AFTER:
            return

        session = db.get_session(sid) or {}
        previous = (session.get("summary") or "").strip()
        old = messages[:-KEEP_RECENT]

        transcript = "\n".join(
            f"[{m['role']}] {m['content'][:600]}" for m in old
        )[-8000:]
        if previous:
            transcript = f"[résumé existant] {previous}\n{transcript}"

        text = ""
        async for chunk in ollama.chat(
            model,
            [
                {"role": "system", "content": _SUMMARY_PROMPT},
                {"role": "user", "content": transcript},
            ],
            options={"temperature": 0.2, "num_predict": 350},
            stream=True,
        ):
            text += chunk.get("message", {}).get("content", "")
            if chunk.get("done"):
                break

        if text.strip():
            db.set_session_summary(sid, text.strip())
    except (httpx.HTTPError, OSError) as exc:
        # Best-effort : un échec de résumé ne doit jamais gêner le chat.
        logger.warning("Résumé de session %s impossible : %s", sid, exc)
