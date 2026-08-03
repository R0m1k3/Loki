"""Mémoire en notes Markdown : l'agent écrit lui-même ce qu'il veut retenir.

Repris d'AJEAN (github.com/nathaninline/jean) : des notes lisibles rangées
dans ``$DATA_DIR/MEMORY/``, que le modèle consulte et enregistre EXPLICITEMENT.

Face au RAG vectoriel, trois avantages décisifs :
  - aucun modèle d'embedding requis (marche dès l'installation) ;
  - le contenu est inspectable et modifiable à la main ;
  - rien n'est mémorisé « à l'insu » : pas de vieille demande sans rapport qui
    ressurgit au milieu d'une nouvelle discussion.

Trois modes (config ``memory_mode``) :
  off       — rien du tout ;
  ondemand  — les outils memory_search / memory_save sont proposés au modèle ;
  always    — idem, plus l'injection automatique des notes pertinentes.
"""
from __future__ import annotations

import os
import re
import time
import unicodedata

from .config import settings

MODES = ("off", "ondemand", "always")

# Bornes : une note reste une note, pas une archive.
_MAX_NOTE_CHARS = 4000
_MAX_NOTES = 200
_EXCERPT_CHARS = 400


def _dir() -> str:
    path = os.path.join(os.path.abspath(settings.data_dir), "MEMORY")
    os.makedirs(path, exist_ok=True)
    return path


def _slug(title: str) -> str:
    """Nom de fichier sûr dérivé du titre (confiné au dossier MEMORY)."""
    text = unicodedata.normalize("NFKD", title or "").encode("ascii", "ignore").decode()
    text = re.sub(r"[^a-zA-Z0-9]+", "-", text).strip("-").lower()
    return (text or "note")[:60]


def _tokens(text: str) -> set[str]:
    """Mots significatifs, sans accents ni casse (>= 3 lettres)."""
    text = unicodedata.normalize("NFKD", text or "").encode("ascii", "ignore").decode()
    return {w for w in re.findall(r"[a-z0-9]{3,}", text.lower())}


def save_note(title: str, content: str) -> dict:
    """Crée ou remplace une note. Renvoie {ok, summary}."""
    title = (title or "").strip()
    content = (content or "").strip()
    if not title:
        return {"ok": False, "summary": "titre vide"}
    if not content:
        return {"ok": False, "summary": "contenu vide"}

    content = content[:_MAX_NOTE_CHARS]
    path = os.path.join(_dir(), f"{_slug(title)}.md")
    existed = os.path.isfile(path)
    with open(path, "w", encoding="utf-8") as f:
        f.write(f"# {title}\n\n{content}\n")

    _prune()
    verb = "mise à jour" if existed else "enregistrée"
    return {"ok": True, "summary": f"note {verb} : {title}"}


def _prune() -> None:
    """Garde les notes les plus récentes (borne dure, jamais de purge totale)."""
    files = [os.path.join(_dir(), n) for n in os.listdir(_dir()) if n.endswith(".md")]
    if len(files) <= _MAX_NOTES:
        return
    files.sort(key=lambda p: os.path.getmtime(p), reverse=True)
    for path in files[_MAX_NOTES:]:
        try:
            os.remove(path)
        except OSError:
            pass


def _read_all() -> list[tuple[str, str]]:
    """[(titre, corps)] de toutes les notes."""
    out = []
    try:
        names = sorted(os.listdir(_dir()))
    except OSError:
        return out
    for name in names:
        if not name.endswith(".md"):
            continue
        try:
            with open(os.path.join(_dir(), name), encoding="utf-8", errors="replace") as f:
                raw = f.read()
        except OSError:
            continue
        first, _, body = raw.partition("\n")
        title = first.lstrip("# ").strip() or name[:-3]
        out.append((title, body.strip()))
    return out


def search_notes(query: str, limit: int = 3) -> list[dict]:
    """Notes les plus proches de la requête (recouvrement de mots-clés)."""
    wanted = _tokens(query)
    if not wanted:
        return []
    scored: list[tuple[int, str, str]] = []
    for title, body in _read_all():
        score = len(wanted & _tokens(f"{title} {body}"))
        if score:
            scored.append((score, title, body))
    scored.sort(key=lambda item: -item[0])
    return [
        {"title": title, "content": body[:_EXCERPT_CHARS]}
        for _, title, body in scored[:limit]
    ]


def list_notes() -> list[dict]:
    """Inventaire des notes (pour l'UI / l'inspection)."""
    notes = []
    for title, body in _read_all():
        path = os.path.join(_dir(), f"{_slug(title)}.md")
        notes.append({
            "title": title,
            "chars": len(body),
            "updated_at": os.path.getmtime(path) if os.path.isfile(path) else time.time(),
        })
    return sorted(notes, key=lambda n: -n["updated_at"])


def delete_note(title: str) -> bool:
    path = os.path.join(_dir(), f"{_slug(title)}.md")
    if not os.path.isfile(path):
        return False
    os.remove(path)
    return True


def recall_block(message: str) -> str | None:
    """Bloc de contexte à injecter en mode ``always`` (None si rien de net)."""
    hits = search_notes(message, limit=2)
    if not hits:
        return None
    body = "\n\n".join(f"## {h['title']}\n{h['content']}" for h in hits)
    return (
        "Notes que tu avais enregistrées et qui semblent liées à cette demande "
        "(ignore-les si elles ne s'appliquent pas) :\n" + body
    )
