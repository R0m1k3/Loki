"""Routes de gestion des sessions de conversation."""
from __future__ import annotations

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

from .. import db

router = APIRouter(prefix="/api/sessions", tags=["sessions"])


class CreateSession(BaseModel):
    title: str = "Nouvelle session"
    model: str | None = None


class RenameSession(BaseModel):
    title: str


@router.get("")
async def get_sessions() -> dict:
    return {"sessions": db.list_sessions()}


@router.post("")
async def post_session(req: CreateSession) -> dict:
    return db.create_session(req.title, req.model)


@router.get("/{sid}")
async def get_one(sid: str) -> dict:
    session = db.get_session(sid)
    if not session:
        raise HTTPException(404, "session introuvable")
    return {"session": session, "messages": db.list_messages(sid)}


@router.patch("/{sid}")
async def patch_session(sid: str, req: RenameSession) -> dict:
    if not db.get_session(sid):
        raise HTTPException(404, "session introuvable")
    db.rename_session(sid, req.title)
    return {"ok": True}


@router.delete("/{sid}")
async def remove_session(sid: str) -> dict:
    db.delete_session(sid)
    return {"ok": True}
