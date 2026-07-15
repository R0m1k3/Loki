"""Routes Git du workspace : historique, diff, retour arrière.

Le workspace est un dépôt git (Aider commite chaque modification). Ces routes
donnent à l'UI un panneau Git : voir les commits, leur diff, et annuler une
modification. Toutes les commandes sont exécutées DANS le workspace.
"""
from __future__ import annotations

import subprocess

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

from ..coder import ensure_git
from ..config import settings

router = APIRouter(prefix="/api/git", tags=["git"])


def _git(*args: str, timeout: int = 15) -> subprocess.CompletedProcess:
    """Exécute une commande git dans le workspace."""
    ensure_git(settings.workspace_dir)
    return subprocess.run(
        ["git", *args],
        cwd=settings.workspace_dir,
        capture_output=True,
        text=True,
        timeout=timeout,
    )


@router.get("/log")
async def git_log(limit: int = 40) -> dict:
    """Historique des commits (hash court, message, auteur, date, nb fichiers)."""
    fmt = "%h%x1f%s%x1f%an%x1f%ar%x1f%H"
    proc = _git("log", f"-{max(1, min(limit, 200))}", f"--pretty=format:{fmt}")
    if proc.returncode != 0:
        # Dépôt sans commit encore.
        return {"commits": []}

    commits = []
    for line in proc.stdout.splitlines():
        parts = line.split("\x1f")
        if len(parts) != 5:
            continue
        short, subject, author, when, full = parts
        # Nombre de fichiers touchés par ce commit.
        stat = _git("show", "--stat", "--oneline", "--format=", full)
        files = [l for l in stat.stdout.splitlines() if "|" in l]
        commits.append({
            "hash": short,
            "full_hash": full,
            "subject": subject,
            "author": author,
            "when": when,
            "files_changed": len(files),
        })
    return {"commits": commits}


@router.get("/diff")
async def git_diff(hash: str | None = None) -> dict:
    """Diff d'un commit (hash) ou des modifications non commitées si absent."""
    if hash:
        if not hash.replace("-", "").isalnum():
            raise HTTPException(400, "hash invalide")
        proc = _git("show", "--no-color", hash)
    else:
        proc = _git("diff", "--no-color", "HEAD")
    if proc.returncode != 0:
        raise HTTPException(404, "diff introuvable")
    # Borne la taille pour ne pas noyer l'UI.
    return {"diff": proc.stdout[:200_000]}


class RevertRequest(BaseModel):
    hash: str


@router.post("/revert")
async def git_revert(req: RevertRequest) -> dict:
    """Annule un commit en créant un commit inverse (git revert)."""
    h = req.hash.strip()
    if not h.replace("-", "").isalnum():
        raise HTTPException(400, "hash invalide")
    proc = _git("revert", "--no-edit", h, timeout=30)
    if proc.returncode != 0:
        # Conflit ou commit introuvable : on nettoie un éventuel revert en cours.
        _git("revert", "--abort")
        detail = (proc.stderr or proc.stdout).strip()[:300]
        raise HTTPException(409, f"revert impossible : {detail}")
    return {"ok": True, "message": f"commit {h[:7]} annulé"}
