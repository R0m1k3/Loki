"""Routes de configuration de l'agent (lecture / mise à jour / auto-réglage)."""
from __future__ import annotations

from fastapi import APIRouter
from pydantic import BaseModel

from .. import agent_config, autotune
from ..config import settings

router = APIRouter(prefix="/api/config", tags=["config"])


class ConfigPatch(BaseModel):
    system_prompt: str | None = None
    temperature: float | None = None
    top_p: float | None = None
    top_k: int | None = None
    max_tokens: int | None = None
    num_ctx: int | None = None
    tools: dict[str, bool] | None = None
    confirm_shell: bool | None = None


class AutoTuneRequest(BaseModel):
    model: str
    apply: bool = True


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


@router.post("/auto")
async def auto_tune(req: AutoTuneRequest) -> dict:
    """Détecte le GPU + le modèle, calcule les réglages optimaux, et (par
    défaut) les applique. Renvoie le détail de la détection pour l'UI."""
    reco = await autotune.recommend(req.model)
    placement = await autotune.placement(req.model)

    applied = None
    if req.apply:
        applied = agent_config.save_config(
            {
                "num_ctx": reco["recommended"]["num_ctx"],
                "max_tokens": reco["recommended"]["max_tokens"],
            }
        )

    return {
        "detection": reco,
        "placement": placement,
        "config": applied or agent_config.get_config(),
        "vram_override": settings.gpu_vram_mb,
    }
