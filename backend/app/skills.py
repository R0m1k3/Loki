"""Skills : méthodes expertes injectées automatiquement selon la tâche.

Sélection lexicale instantanée (aucun appel LLM) : au plus UNE skill par
message, injectée en message système pour ce tour uniquement.
"""
from __future__ import annotations

import os
import re

_SKILLS_DIR = os.path.join(os.path.dirname(__file__), "..", "skills")

# Seuil : nombre minimal de mots-clés distincts trouvés dans le message.
_MIN_HITS = 2


def _load() -> dict[str, dict]:
    out: dict[str, dict] = {}
    if not os.path.isdir(_SKILLS_DIR):
        return out
    for fname in sorted(os.listdir(_SKILLS_DIR)):
        if not fname.endswith(".md"):
            continue
        with open(os.path.join(_SKILLS_DIR, fname), encoding="utf-8") as f:
            raw = f.read()
        m = re.match(r"^---\n(.*?)\n---\n(.*)$", raw, re.S)
        if not m:
            continue
        meta: dict[str, str] = {}
        for line in m.group(1).splitlines():
            if ":" in line:
                key, _, value = line.partition(":")
                meta[key.strip()] = value.strip()
        keywords = [
            k.strip().lower()
            for k in meta.get("keywords", "").split(",")
            if k.strip()
        ]
        out[meta.get("name", fname[:-3])] = {
            "name": meta.get("name", fname[:-3]),
            "title": meta.get("title", fname[:-3]),
            "keywords": keywords,
            "body": m.group(2).strip(),
        }
    return out


ALL_SKILLS: dict[str, dict] = _load()


def pick_skill(message: str) -> dict | None:
    """Meilleure skill pour ce message, ou None si rien d'assez net."""
    low = message.lower()
    best, best_hits = None, 0
    for skill in ALL_SKILLS.values():
        hits = sum(1 for kw in skill["keywords"] if kw in low)
        if hits > best_hits:
            best, best_hits = skill, hits
    if best is None or best_hits < _MIN_HITS:
        return None
    return {"name": best["name"], "title": best["title"], "body": best["body"]}
