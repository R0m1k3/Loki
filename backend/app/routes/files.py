"""Routes du workspace : arborescence, contenu, téléchargement, suppression.

Toutes les routes acceptent un paramètre optionnel ``project`` : la requête
est alors re-racinée sur ``workspace/<projet>`` (même confinement).
"""
from __future__ import annotations

import os
import shutil

from fastapi import APIRouter, HTTPException
from fastapi.responses import FileResponse

from .. import tools as tools_mod
from ..tools import ToolError, _safe_path

router = APIRouter(prefix="/api/files", tags=["files"])


def _activate(project: str | None) -> None:
    """Pose le projet actif pour la requête (400 si nom invalide)."""
    try:
        tools_mod.set_project(project or None)
    except ToolError as exc:
        raise HTTPException(400, str(exc)) from exc


def _tree(path: str, root: str) -> list[dict]:
    """Arborescence triée (dossiers d'abord) de la racine active."""
    items = []
    for name in sorted(os.listdir(path)):
        if name.startswith("."):
            continue
        full = os.path.join(path, name)
        rel = os.path.relpath(full, root)
        if os.path.isdir(full):
            items.append({"name": name, "path": rel, "type": "dir",
                          "children": _tree(full, root)})
        else:
            items.append({"name": name, "path": rel, "type": "file",
                          "size": os.path.getsize(full)})
    # Dossiers avant fichiers
    items.sort(key=lambda x: (x["type"] != "dir", x["name"]))
    return items


@router.get("")
async def list_files(project: str | None = None) -> dict:
    _activate(project)
    root = tools_mod.active_root()
    return {"tree": _tree(root, root)}


@router.get("/content")
async def file_content(path: str, project: str | None = None) -> dict:
    _activate(project)
    try:
        target = _safe_path(path)
    except ToolError as exc:
        raise HTTPException(400, str(exc))
    if not os.path.isfile(target):
        raise HTTPException(404, "fichier introuvable")
    with open(target, "r", encoding="utf-8", errors="replace") as f:
        return {"path": path, "content": f.read()}


@router.get("/download")
async def download_file(path: str, project: str | None = None) -> FileResponse:
    """Télécharge un fichier en conservant le confinement au workspace."""
    _activate(project)
    try:
        target = _safe_path(path)
    except ToolError as exc:
        raise HTTPException(400, str(exc)) from exc
    if not os.path.isfile(target):
        raise HTTPException(404, "fichier introuvable")
    return FileResponse(
        target,
        filename=os.path.basename(target),
        media_type="application/octet-stream",
    )


@router.delete("")
async def delete_file(path: str, project: str | None = None) -> dict:
    """Supprime un fichier ou un dossier (récursif) de la racine active.

    Même confinement que le téléchargement (`_safe_path`) ; la racine active
    est refusée. Les dotfiles (.git…) ne sont jamais listés par
    l'arborescence, donc inaccessibles depuis l'UI.
    """
    _activate(project)
    try:
        target = _safe_path(path)
    except ToolError as exc:
        raise HTTPException(400, str(exc)) from exc
    if os.path.abspath(target) == tools_mod.active_root():
        raise HTTPException(400, "suppression de la racine refusée")
    if os.path.isdir(target):
        shutil.rmtree(target)
    elif os.path.isfile(target):
        os.remove(target)
    else:
        raise HTTPException(404, "fichier introuvable")
    return {"deleted": path}
