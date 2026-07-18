"""Boosters de qualité pour petits modèles : plan-puis-exécute et auto-critique.

- make_plan : décompose une demande complexe en 3-5 étapes courtes. Un petit
  modèle qui suit un plan écrit réussit bien mieux qu'en improvisant.
- self_review : une passe de critique éclair sur la réponse, puis une révision
  si des défauts sont trouvés (activable : coûte un peu de latence).
"""
from __future__ import annotations

import logging
import re

import httpx

from .ollama_client import OllamaError, ollama

logger = logging.getLogger(__name__)

_PLAN_PROMPT = (
    "Découpe la demande en 3 à 5 étapes courtes et concrètes, une par ligne, "
    "numérotées « 1. », « 2. »… Pas d'introduction, pas de conclusion, "
    "UNIQUEMENT les étapes, en français."
)

_CRITIQUE_PROMPT = (
    "Tu es un relecteur exigeant. Voici une demande et la réponse d'un "
    "assistant. Si la réponse est correcte et complète, réponds exactement "
    "PARFAIT. Sinon, liste au plus 3 défauts concrets (erreurs, oublis, "
    "incohérences), un par ligne."
)

_REVISE_PROMPT = (
    "Réécris la réponse en corrigeant les défauts listés. Donne UNIQUEMENT la "
    "réponse finale corrigée, sans commentaire sur la révision."
)


def needs_plan(message: str) -> bool:
    """Une demande assez longue/composée mérite un plan explicite."""
    if len(message) < 120:
        return False
    connectors = len(re.findall(
        r"\b(puis|ensuite|après|avec|ainsi que|et aussi|également)\b",
        message, re.I,
    ))
    return len(message) > 240 or connectors >= 2


async def _ask(
    model: str,
    system: str,
    user: str,
    *,
    num_predict: int,
    options: dict | None = None,
    keep_alive: str | None = None,
) -> str:
    """Appel court au modèle.

    ``options`` doit contenir les options runner (num_ctx, num_batch…) du chat
    principal : un appel avec des options divergentes force Ollama à recharger
    le modèle en plein message.
    """
    messages = [{"role": "system", "content": system},
                {"role": "user", "content": user}]
    opts = {**(options or {}), "temperature": 0.2, "num_predict": num_predict}
    # think=False : un appel utilitaire (plan, critique) ne doit jamais
    # « réfléchir » — sur un modèle thinking, la pensée dévore le budget et
    # multiplie la latence. Repli sans le paramètre si le modèle le refuse.
    think: bool | None = False
    while True:
        text = ""
        try:
            async for chunk in ollama.chat(
                model, messages, options=opts, think=think,
                keep_alive=keep_alive, stream=True,
            ):
                text += chunk.get("message", {}).get("content", "")
                if chunk.get("done"):
                    break
            return text.strip()
        except OllamaError as exc:
            if think is False and "think" in str(exc).lower():
                think = None
                continue
            raise


async def make_plan(
    model: str,
    message: str,
    *,
    options: dict | None = None,
    keep_alive: str | None = None,
) -> list[str]:
    """Renvoie la liste des étapes (vide si échec — jamais bloquant)."""
    try:
        raw = await _ask(
            model, _PLAN_PROMPT, message[:1200], num_predict=220,
            options=options, keep_alive=keep_alive,
        )
    except (httpx.HTTPError, OSError) as exc:
        logger.warning("Plan impossible : %s", exc)
        return []
    steps = []
    for line in raw.splitlines():
        line = line.strip()
        m = re.match(r"^\d+[.)]\s*(.+)$", line)
        if m:
            steps.append(m.group(1).strip())
    return steps[:5] if len(steps) >= 2 else []


async def self_review(
    model: str,
    request: str,
    answer: str,
    *,
    options: dict | None = None,
    keep_alive: str | None = None,
) -> str | None:
    """Critique puis révise la réponse. None si rien à corriger / échec."""
    if len(answer) < 80:
        return None
    try:
        critique = await _ask(
            model,
            _CRITIQUE_PROMPT,
            f"Demande :\n{request[:800]}\n\nRéponse :\n{answer[:2500]}",
            num_predict=180,
            options=options, keep_alive=keep_alive,
        )
        if not critique or "PARFAIT" in critique.upper()[:40]:
            return None

        revised = await _ask(
            model,
            _REVISE_PROMPT,
            f"Demande :\n{request[:800]}\n\nRéponse initiale :\n{answer[:2500]}"
            f"\n\nDéfauts :\n{critique[:600]}",
            num_predict=1500,
            options=options, keep_alive=keep_alive,
        )
        # Garde-fou : une révision vide ou minuscule ne remplace rien.
        return revised if len(revised) > len(answer) // 3 else None
    except (httpx.HTTPError, OSError) as exc:
        logger.warning("Auto-critique impossible : %s", exc)
        return None
