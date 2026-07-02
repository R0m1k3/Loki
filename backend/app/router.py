"""Routeur automatique : moteur code (Aider) ou boucle agent classique ?

Invisible pour l'utilisateur : chaque message est classé.
  1. Heuristique lexicale rapide (gratuite) — tranche les cas évidents.
  2. Cas ambigus : micro-classification par le modèle lui-même (3 jetons).
"""
from __future__ import annotations

import re

import httpx

from .ollama_client import ollama

# Vocabulaire fortement lié au code / au développement.
_STRONG = re.compile(
    r"\b(code|coder?|script|fonction|classe|refactor|bug|d[ée]bug|html|css|"
    r"javascript|js|python|typescript|react|api|composant|page|site|landing|"
    r"formulaire|d[ée]veloppe|impl[ée]mente|programme|widget|frontend|backend)\b",
    re.I,
)
# Extensions de fichiers mentionnées explicitement.
_FILE_EXT = re.compile(
    r"\.(py|js|ts|tsx|jsx|html?|css|json|md|sh|sql|ya?ml|go|rs|java|php|vue)\b", re.I
)
# Verbes d'action de création/modification.
_ACTION = re.compile(
    r"\b(cr[ée]e[rs]?|modifie[rs]?|corrige[rs]?|ajoute[rs]?|[ée]cri[st]|refai[st]|"
    r"am[ée]liore[rs]?|fixe?|update|change[rs]?|construis|g[ée]n[èe]re[rs]?)\b",
    re.I,
)


def score_code_task(message: str) -> int:
    """Score heuristique : >= 3 -> code, <= 1 -> agent, 2 -> ambigu."""
    score = 0
    if _STRONG.search(message):
        score += 2
    if _FILE_EXT.search(message):
        score += 2
    if _ACTION.search(message):
        score += 1
    if "```" in message:
        score += 2
    return score


async def is_code_task(message: str, model: str) -> bool:
    """Décision finale : heuristique, puis LLM sur les cas ambigus."""
    score = score_code_task(message)
    if score >= 3:
        return True
    if score <= 1:
        return False

    # Ambigu : on demande au modèle (réponse d'un mot, quasi instantané).
    try:
        answer = ""
        async for chunk in ollama.chat(
            model,
            [
                {
                    "role": "system",
                    "content": (
                        "Tu classes des demandes. Réponds UNIQUEMENT par CODE "
                        "si la demande nécessite d'écrire ou modifier du code "
                        "ou des fichiers de projet, sinon CHAT."
                    ),
                },
                {"role": "user", "content": message[:500]},
            ],
            options={"num_predict": 4, "temperature": 0},
            stream=True,
        ):
            answer += chunk.get("message", {}).get("content", "")
            if chunk.get("done"):
                break
        return "CODE" in answer.upper()
    except (httpx.HTTPError, OSError):
        # Ollama indisponible : la boucle agent gérera l'erreur proprement.
        return False
