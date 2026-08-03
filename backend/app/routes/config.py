"""Routes de lecture et mise à jour de la configuration de l'agent."""
from __future__ import annotations

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

from .. import agent_config

router = APIRouter(prefix="/api/config", tags=["config"])


class ConfigPatch(BaseModel):
    """Champs modifiables. DOIT couvrir agent_config.PROFILE_FIELDS.

    Pydantic ignore silencieusement un champ non déclaré : un oubli ici fait
    « sauter » le réglage sans la moindre erreur (c'était le cas de
    keep_alive, skills_enabled et ponytail).
    """

    system_prompt: str | None = None
    temperature: float | None = None
    top_p: float | None = None
    top_k: int | None = None
    max_tokens: int | None = None
    num_ctx: int | None = None
    num_gpu: int | None = None
    num_batch: int | None = None
    tools: dict[str, bool] | None = None
    confirm_shell: bool | None = None
    think: bool | None = None
    code_model: str | None = None
    plan_mode: bool | None = None
    self_review: bool | None = None
    rag_enabled: bool | None = None
    embed_model: str | None = None
    memory_mode: str | None = None
    skills_enabled: bool | None = None
    ponytail: bool | None = None
    keep_alive: str | None = None


@router.get("")
async def get_config(model: str | None = None) -> dict:
    return {
        "config": agent_config.get_config(model),
        "available_tools": agent_config.AVAILABLE_TOOLS,
    }


@router.put("")
async def put_config(patch: ConfigPatch, model: str | None = None) -> dict:
    cfg = agent_config.save_config(patch.model_dump(exclude_none=True), model)
    return {"config": cfg}


# ── Presets : jeux de réglages nommés, commutables (idée reprise d'AJEAN) ──
class PresetBody(BaseModel):
    name: str


@router.get("/presets")
async def get_presets() -> dict:
    return {"presets": agent_config.list_presets()}


@router.post("/presets")
async def post_preset(body: PresetBody, model: str | None = None) -> dict:
    """Enregistre la configuration courante sous un nom."""
    try:
        return {"presets": agent_config.save_preset(body.name, model)}
    except ValueError as exc:
        raise HTTPException(400, str(exc)) from exc


@router.post("/presets/apply")
async def apply_preset(body: PresetBody, model: str | None = None) -> dict:
    """Applique un preset à la configuration courante."""
    cfg = agent_config.apply_preset(body.name, model)
    if cfg is None:
        raise HTTPException(404, "preset introuvable")
    return {"config": cfg}


@router.delete("/presets")
async def remove_preset(name: str) -> dict:
    return {"presets": agent_config.delete_preset(name)}
