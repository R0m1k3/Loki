"""Statistiques système temps réel : CPU, RAM, GPU/VRAM (barre supérieure)."""
from __future__ import annotations

import asyncio
import shutil

import httpx
import psutil
from fastapi import APIRouter

from ..config import settings
from ..ollama_client import ollama

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


async def _all_local_gpus() -> list[dict]:
    """Tous les GPU NVIDIA visibles depuis le CONTENEUR Loki (peut être vide)."""
    if not _NVIDIA_SMI:
        return []
    try:
        proc = await asyncio.create_subprocess_exec(
            _NVIDIA_SMI,
            "--query-gpu=index,name,memory.total,memory.used,utilization.gpu",
            "--format=csv,noheader,nounits",
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        out, _ = await asyncio.wait_for(proc.communicate(), timeout=3)
        gpus = []
        for line in out.decode().strip().splitlines():
            idx, name, total, used, util = (p.strip() for p in line.split(","))
            gpus.append({
                "index": int(idx), "name": name,
                "vram_total_mb": float(total), "vram_used_mb": float(used),
                "util_pct": float(util),
            })
        return gpus
    except Exception:
        return []


@router.get("/hardware")
async def hardware() -> dict:
    """Vue matériel : GPU vu par Loki vs GPU réellement utilisé par Ollama."""
    # 1) Ce que voit le conteneur Loki (nvidia-smi) + override éventuel.
    local_gpus = await _all_local_gpus()
    override = None
    if settings.gpu_vram_mb > 0:
        override = {
            "name": settings.gpu_name or "GPU déclaré (GPU_VRAM_MB)",
            "vram_total_mb": settings.gpu_vram_mb,
        }

    # 2) Ce qu'Ollama utilise réellement (modèles chargés + placement VRAM/CPU).
    ollama_info: dict = {"host": ollama.host, "connected": False, "running": []}
    try:
        version = await ollama.ping()
        ollama_info["connected"] = True
        ollama_info["version"] = version.get("version")
    except (httpx.HTTPError, OSError) as exc:
        ollama_info["error"] = str(exc)[:200]

    if ollama_info["connected"]:
        try:
            for m in await ollama.ps():
                size = m.get("size", 0) or 0
                vram = m.get("size_vram", 0) or 0
                if size <= 0:
                    where = "inconnu"
                elif vram >= size * 0.99:
                    where = "GPU"
                elif vram <= size * 0.01:
                    where = "CPU"
                else:
                    where = "mixte"
                ollama_info["running"].append({
                    "name": m.get("name") or m.get("model"),
                    "processor": where,
                    "gpu_percent": int(vram / size * 100) if size else 0,
                    "size_mb": round(size / 1_000_000),
                    "vram_mb": round(vram / 1_000_000),
                })
        except (httpx.HTTPError, OSError):
            pass

    # Ollama est-il sur la même machine que Loki ? (heuristique sur l'hôte)
    host = ollama.host.lower()
    is_local = any(h in host for h in ("localhost", "127.0.0.1", "host.docker.internal"))

    return {
        "loki_gpus": local_gpus,
        "gpu_override": override,
        "ollama": ollama_info,
        "ollama_is_local": is_local,
        "note": (
            "Loki ne voit pas directement le GPU d'un Ollama distant : il déduit "
            "le placement (GPU/CPU) depuis les modèles chargés. Déclare GPU_VRAM_MB "
            "pour l'auto-réglage si Ollama tourne sur une autre machine."
        ),
    }
