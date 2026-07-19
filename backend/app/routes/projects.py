"""Routes des projets : sous-dossiers de premier niveau du workspace."""
from __future__ import annotations

import os

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

from .. import coder, tools
from ..config import settings

router = APIRouter(prefix="/api/projects", tags=["projects"])


def _root() -> str:
    root = os.path.abspath(settings.workspace_dir)
    os.makedirs(root, exist_ok=True)
    return root


def _count_files(path: str) -> int:
    total = 0
    for dirpath, dirnames, filenames in os.walk(path):
        dirnames[:] = [d for d in dirnames if not d.startswith(".")]
        total += sum(1 for f in filenames if not f.startswith("."))
    return total


@router.get("")
async def list_projects() -> dict:
    root = _root()
    projects = []
    root_files = 0
    for name in sorted(os.listdir(root)):
        full = os.path.join(root, name)
        if name.startswith("."):
            continue
        if os.path.isdir(full):
            projects.append({"name": name, "files": _count_files(full)})
        else:
            root_files += 1
    return {"projects": projects, "root_files": root_files}


class CreateProject(BaseModel):
    name: str


@router.post("", status_code=201)
async def create_project(req: CreateProject) -> dict:
    name = req.name.strip()
    if not tools.PROJECT_NAME.match(name):
        raise HTTPException(400, "nom de projet invalide (a-z, 0-9, - et _)")
    target = os.path.join(_root(), name)
    if os.path.exists(target):
        raise HTTPException(400, "ce projet existe déjà")
    os.makedirs(target)
    # Dépôt git par projet : commits Aider + onglet Git propres au projet.
    coder.ensure_git(target)
    return {"name": name}
