"""Route d'exécution d'une commande shell validée par l'utilisateur.

La boucle agentique n'exécute jamais run_shell elle-même quand la confirmation
est active : elle émet un événement `tool_confirm`. Le client affiche la
commande, et c'est seulement après clic explicite de l'utilisateur que cette
route exécute la commande (confinée au workspace).
"""
from __future__ import annotations

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

from ..tools import ToolError, run_shell

router = APIRouter(prefix="/api/shell", tags=["shell"])


class ShellRequest(BaseModel):
    command: str


@router.post("/run")
async def run(req: ShellRequest) -> dict:
    """Exécute la commande validée et renvoie sa sortie."""
    try:
        result = run_shell(req.command)
    except ToolError as exc:
        raise HTTPException(400, str(exc))
    return {
        "command": req.command,
        "exit_code": result["exit_code"],
        "output": result["output"],
    }
