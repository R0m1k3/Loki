"""Routes de gestion des sessions de conversation."""
from __future__ import annotations

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

from .. import db, tools

router = APIRouter(prefix="/api/sessions", tags=["sessions"])


class CreateSession(BaseModel):
    title: str = "Nouvelle session"
    model: str | None = None
    project: str | None = None


class UpdateSession(BaseModel):
    title: str | None = None
    project: str | None = None  # "" = retour à la racine du workspace


@router.get("")
async def get_sessions() -> dict:
    return {"sessions": db.list_sessions()}


@router.post("")
async def post_session(req: CreateSession) -> dict:
    if req.project and not tools.PROJECT_NAME.match(req.project):
        raise HTTPException(400, "nom de projet invalide")
    return db.create_session(req.title, req.model, req.project or None)


@router.get("/{sid}")
async def get_one(sid: str) -> dict:
    session = db.get_session(sid)
    if not session:
        raise HTTPException(404, "session introuvable")
    return {"session": session, "messages": db.list_messages(sid)}


@router.patch("/{sid}")
async def patch_session(sid: str, req: UpdateSession) -> dict:
    if not db.get_session(sid):
        raise HTTPException(404, "session introuvable")
    if req.title is not None:
        db.rename_session(sid, req.title)
    if req.project is not None:
        project = req.project or None
        if project and not tools.PROJECT_NAME.match(project):
            raise HTTPException(400, "nom de projet invalide")
        db.set_session_project(sid, project)
    return {"ok": True}


@router.delete("/{sid}")
async def remove_session(sid: str) -> dict:
    db.delete_session(sid)
    return {"ok": True}
