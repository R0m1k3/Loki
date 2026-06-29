"""Outils de l'agent, exécutés côté serveur et confinés au workspace.

Chaque outil expose :
  - une définition JSON (format function-calling Ollama/OpenAI) ;
  - une implémentation Python qui renvoie un dict {ok, summary, ...}.

Toutes les opérations fichier sont strictement confinées à WORKSPACE_DIR :
toute tentative de sortie (../, chemin absolu hors workspace) est rejetée.
"""
from __future__ import annotations

import os

from .config import settings


class ToolError(Exception):
    """Erreur d'exécution d'un outil (message destiné au modèle)."""


def _workspace_root() -> str:
    root = os.path.abspath(settings.workspace_dir)
    os.makedirs(root, exist_ok=True)
    return root


def _safe_path(rel: str) -> str:
    """Résout un chemin relatif en restant confiné au workspace."""
    root = _workspace_root()
    target = os.path.abspath(os.path.join(root, rel or "."))
    if target != root and not target.startswith(root + os.sep):
        raise ToolError(f"chemin hors du workspace refusé : {rel}")
    return target


# ── Implémentations ──────────────────────────────────────────────────────
def read_file(path: str) -> dict:
    target = _safe_path(path)
    if not os.path.isfile(target):
        raise ToolError(f"fichier introuvable : {path}")
    with open(target, "r", encoding="utf-8", errors="replace") as f:
        content = f.read()
    size = os.path.getsize(target)
    summary = "fichier vide (0 octet)" if size == 0 else f"{len(content.splitlines())} lignes lues"
    return {"ok": True, "content": content, "summary": summary}


def write_file(path: str, content: str) -> dict:
    target = _safe_path(path)
    os.makedirs(os.path.dirname(target) or _workspace_root(), exist_ok=True)
    existed = os.path.isfile(target)
    with open(target, "w", encoding="utf-8") as f:
        f.write(content)
    lines = len(content.splitlines())
    verb = "modifié" if existed else "écrit"
    return {"ok": True, "summary": f"{verb} · {lines} lignes", "lines": lines}


def list_dir(path: str = ".") -> dict:
    target = _safe_path(path)
    if not os.path.isdir(target):
        raise ToolError(f"répertoire introuvable : {path}")
    entries = []
    for name in sorted(os.listdir(target)):
        full = os.path.join(target, name)
        entries.append({"name": name, "type": "dir" if os.path.isdir(full) else "file"})
    return {
        "ok": True,
        "entries": entries,
        "summary": f"{len(entries)} élément(s)",
    }


# ── Registre & définitions exposées au modèle ────────────────────────────
TOOL_IMPL = {
    "read_file": read_file,
    "write_file": write_file,
    "list_dir": list_dir,
}

TOOL_DEFINITIONS = [
    {
        "type": "function",
        "function": {
            "name": "read_file",
            "description": "Lire le contenu d'un fichier du workspace.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "Chemin relatif au workspace"}
                },
                "required": ["path"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "write_file",
            "description": "Créer ou modifier un fichier du workspace.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "Chemin relatif au workspace"},
                    "content": {"type": "string", "description": "Contenu complet du fichier"},
                },
                "required": ["path", "content"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "list_dir",
            "description": "Lister le contenu d'un répertoire du workspace.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "Répertoire (défaut : racine)"}
                },
            },
        },
    },
]


def run_tool(name: str, args: dict) -> dict:
    """Exécute un outil par son nom ; lève ToolError si inconnu/invalide."""
    impl = TOOL_IMPL.get(name)
    if impl is None:
        raise ToolError(f"outil inconnu : {name}")
    try:
        return impl(**(args or {}))
    except ToolError:
        raise
    except TypeError as exc:
        raise ToolError(f"arguments invalides pour {name} : {exc}") from exc
    except OSError as exc:
        raise ToolError(f"erreur système ({name}) : {exc}") from exc
