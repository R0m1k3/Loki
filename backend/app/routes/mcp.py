"""Routes de configuration des serveurs MCP (catalogue + toggles)."""
from __future__ import annotations

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

from ..mcp_client import CATALOG, get_mcp_state, manager, set_mcp_state

router = APIRouter(prefix="/api/mcp", tags=["mcp"])


def _payload() -> dict:
    state = get_mcp_state()
    statuses = manager.statuses()
    return {
        "servers": [
            {
                "id": sid,
                "label": entry["label"],
                "description": entry["description"],
                "url_param": entry["url_param"],
                "env_params": entry["env_params"],
                "enabled": state[sid]["enabled"],
                "params": state[sid]["params"],
                **statuses[sid],
            }
            for sid, entry in CATALOG.items()
        ]
    }


@router.get("")
async def list_servers() -> dict:
    return _payload()


class McpUpdate(BaseModel):
    enabled: bool
    params: dict = {}


@router.put("/{sid}")
async def update_server(sid: str, req: McpUpdate) -> dict:
    if sid not in CATALOG:
        raise HTTPException(404, "serveur inconnu")
    set_mcp_state(sid, enabled=req.enabled, params=req.params)
    return _payload()


@router.post("/{sid}/test")
async def test_server(sid: str) -> dict:
    if sid not in CATALOG:
        raise HTTPException(404, "serveur inconnu")
    return await manager.test_server(sid)
