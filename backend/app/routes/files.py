"""Routes du workspace : arborescence, contenu, téléchargement, suppression."""
from __future__ import annotations

import os
import shutil

from fastapi import APIRouter, HTTPException
from fastapi.responses import FileResponse

from ..config import settings
from ..tools import ToolError, _safe_path

router = APIRouter(prefix="/api/files", tags=["files"])


def _tree(path: str) -> list[dict]:
    """Arborescence triée (dossiers d'abord) du workspace."""
    items = []
    for name in sorted(os.listdir(path)):
        if name.startswith("."):
            continue
        full = os.path.join(path, name)
        rel = os.path.relpath(full, os.path.abspath(settings.workspace_dir))
        if os.path.isdir(full):
            items.append({"name": name, "path": rel, "type": "dir",
                          "children": _tree(full)})
        else:
            items.append({"name": name, "path": rel, "type": "file",
                          "size": os.path.getsize(full)})
    # Dossiers avant fichiers
    items.sort(key=lambda x: (x["type"] != "dir", x["name"]))
    return items


@router.get("")
async def list_files() -> dict:
    root = os.path.abspath(settings.workspace_dir)
    os.makedirs(root, exist_ok=True)
    return {"tree": _tree(root)}


@router.get("/content")
async def file_content(path: str) -> dict:
    try:
        target = _safe_path(path)
    except ToolError as exc:
        raise HTTPException(400, str(exc))
    if not os.path.isfile(target):
        raise HTTPException(404, "fichier introuvable")
    with open(target, "r", encoding="utf-8", errors="replace") as f:
        return {"path": path, "content": f.read()}


@router.get("/download")
async def download_file(path: str) -> FileResponse:
    """Télécharge un fichier en conservant le confinement au workspace."""
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
async def delete_file(path: str) -> dict:
    """Supprime un fichier ou un dossier (récursif) du workspace.

    Même confinement que le téléchargement (`_safe_path`) ; la racine du
    workspace est refusée. Les dotfiles (.git…) ne sont jamais listés par
    l'arborescence, donc inaccessibles depuis l'UI.
    """
    try:
        target = _safe_path(path)
    except ToolError as exc:
        raise HTTPException(400, str(exc)) from exc
    root = os.path.abspath(settings.workspace_dir)
    if os.path.abspath(target) == root:
        raise HTTPException(400, "suppression de la racine du workspace refusée")
    if os.path.isdir(target):
        shutil.rmtree(target)
    elif os.path.isfile(target):
        os.remove(target)
    else:
        raise HTTPException(404, "fichier introuvable")
    return {"deleted": path}
