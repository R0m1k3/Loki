"""Routes de configuration de l'agent (lecture / mise à jour)."""
from __future__ import annotations

from fastapi import APIRouter
from pydantic import BaseModel

from .. import agent_config

router = APIRouter(prefix="/api/config", tags=["config"])


class ConfigPatch(BaseModel):
    system_prompt: str | None = None
    temperature: float | None = None
    top_p: float | None = None
    top_k: int | None = None
    max_tokens: int | None = None
    tools: dict[str, bool] | None = None


@router.get("")
async def get_config() -> dict:
    return {
        "config": agent_config.get_config(),
        "available_tools": agent_config.AVAILABLE_TOOLS,
    }


@router.put("")
async def put_config(patch: ConfigPatch) -> dict:
    cfg = agent_config.save_config(patch.model_dump(exclude_none=True))
    return {"config": cfg}
