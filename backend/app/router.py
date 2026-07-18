"""Routeur automatique : moteur code (Aider) ou boucle agent classique ?

Invisible pour l'utilisateur : chaque message est classé par une heuristique
lexicale instantanée. Les cas ambigus partent vers la boucle agent, qui garde
l'outil `code_task` en secours — plus d'appel LLM bloquant avant le premier
token (l'ancien micro-classifieur coûtait un aller-retour modèle complet et
chargeait un runner divergent).
"""
from __future__ import annotations

import re

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
    """Score heuristique : >= 3 -> code, <= 2 -> agent."""
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


def is_code_task(message: str) -> bool:
    """Décision : heuristique pure, aucune requête modèle."""
    return score_code_task(message) >= 3
