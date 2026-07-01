"""Statistiques système temps réel : CPU, RAM, GPU/VRAM (barre supérieure)."""
from __future__ import annotations

import asyncio
import shutil

import psutil
from fastapi import APIRouter

router = APIRouter(prefix="/api/system", tags=["system"])

_NVIDIA_SMI = shutil.which("nvidia-smi")


async def _gpu_stats() -> dict | None:
    """Utilisation GPU/VRAM via nvidia-smi ; None si absent (pas de GPU NVIDIA)."""
    if not _NVIDIA_SMI:
        return None
    try:
        proc = await asyncio.create_subprocess_exec(
            _NVIDIA_SMI,
            "--query-gpu=utilization.gpu,memory.used,memory.total,name",
            "--format=csv,noheader,nounits",
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        out, _ = await asyncio.wait_for(proc.communicate(), timeout=3)
        line = out.decode().strip().splitlines()[0]
        util, used, total, name = (p.strip() for p in line.split(","))
        return {
            "name": name,
            "util_pct": float(util),
            "vram_used_mb": float(used),
            "vram_total_mb": float(total),
        }
    except Exception:
        return None


@router.get("/stats")
async def stats() -> dict:
    """CPU %, RAM et GPU/VRAM courants."""
    cpu_pct = psutil.cpu_percent(interval=None)
    mem = psutil.virtual_memory()
    gpu = await _gpu_stats()
    return {
        "cpu_pct": cpu_pct,
        "ram_used_go": round(mem.used / 1_000_000_000, 1),
        "ram_total_go": round(mem.total / 1_000_000_000, 1),
        "ram_pct": mem.percent,
        "gpu": gpu,
    }
