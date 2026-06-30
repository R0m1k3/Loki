"""Auto-réglage : exploite au mieux chaque modèle selon le GPU disponible.

Principe :
  1. on détecte la VRAM du GPU (nvidia-smi, sinon rocm-smi, sinon override env) ;
  2. on lit les métadonnées du modèle via Ollama (/api/show + /api/tags) :
     contexte max, architecture (couches, têtes KV, dimension), taille sur disque ;
  3. on calcule la fenêtre de contexte (num_ctx) la plus grande qui tient en VRAM,
     via une estimation du cache KV, puis un nombre de jetons de sortie cohérent.

Tout est best-effort : si une info manque, on retombe sur des paliers prudents.
"""
from __future__ import annotations

import shutil
import subprocess

import httpx

from .config import settings
from .ollama_client import ollama

# Paliers de contexte « ronds » proposés (bornés par le contexte du modèle).
CTX_STEPS = [2048, 4096, 8192, 12288, 16384, 24576, 32768, 49152, 65536, 131072]

# Marges VRAM (Mo) : OS/driver + buffers de calcul d'Ollama.
VRAM_OVERHEAD_MB = 1024
VRAM_COMPUTE_BUFFER_MB = 768


# ── Détection GPU ────────────────────────────────────────────────────────
def detect_gpu() -> dict:
    """Renvoie {available, name, vram_total_mb, source}."""
    # 1) Override explicite (env) — prioritaire, utile en conteneur sans GPU.
    if settings.gpu_vram_mb > 0:
        return {
            "available": True,
            "name": settings.gpu_name or "GPU (configuré)",
            "vram_total_mb": settings.gpu_vram_mb,
            "source": "env",
        }

    # 2) NVIDIA
    if shutil.which("nvidia-smi"):
        try:
            out = subprocess.run(
                ["nvidia-smi",
                 "--query-gpu=name,memory.total",
                 "--format=csv,noheader,nounits"],
                capture_output=True, text=True, timeout=5,
            )
            if out.returncode == 0 and out.stdout.strip():
                line = out.stdout.strip().splitlines()[0]
                name, total = [p.strip() for p in line.split(",")]
                return {
                    "available": True,
                    "name": name,
                    "vram_total_mb": int(float(total)),
                    "source": "nvidia-smi",
                }
        except (OSError, ValueError, subprocess.SubprocessError):
            pass

    # 3) AMD (ROCm) — best-effort
    if shutil.which("rocm-smi"):
        try:
            out = subprocess.run(
                ["rocm-smi", "--showmeminfo", "vram", "--csv"],
                capture_output=True, text=True, timeout=5,
            )
            if out.returncode == 0:
                # Cherche le plus grand entier (octets) -> Mo
                nums = [int(x) for x in out.stdout.replace(",", " ").split()
                        if x.isdigit()]
                if nums:
                    return {
                        "available": True,
                        "name": "GPU AMD",
                        "vram_total_mb": max(nums) // (1024 * 1024),
                        "source": "rocm-smi",
                    }
        except (OSError, ValueError, subprocess.SubprocessError):
            pass

    return {"available": False, "name": "CPU", "vram_total_mb": 0, "source": "none"}


# ── Métadonnées modèle ───────────────────────────────────────────────────
async def model_profile(model: str) -> dict:
    """Extrait contexte max, archi (couches/têtes/dim), taille disque (Mo)."""
    profile = {
        "context_length": None,
        "block_count": None,
        "head_count": None,
        "head_count_kv": None,
        "embedding_length": None,
        "size_mb": None,
        "parameter_size": None,
        "quantization": None,
    }
    try:
        show = await ollama.show(model)
    except (httpx.HTTPError, OSError):
        return profile

    info = show.get("model_info", {}) or {}
    arch = info.get("general.architecture", "")

    def pick(*suffixes):
        for s in suffixes:
            key = f"{arch}.{s}" if arch else s
            if key in info:
                return info[key]
            for k, v in info.items():
                if k.endswith(s):
                    return v
        return None

    profile["context_length"] = pick("context_length")
    profile["block_count"] = pick("block_count")
    profile["head_count"] = pick("attention.head_count")
    profile["head_count_kv"] = pick("attention.head_count_kv")
    profile["embedding_length"] = pick("embedding_length")

    details = show.get("details", {}) or {}
    profile["parameter_size"] = details.get("parameter_size")
    profile["quantization"] = details.get("quantization_level")

    # Taille sur disque via /api/tags
    try:
        for m in await ollama.list_models():
            if m.get("name") == model:
                profile["size_mb"] = int(m.get("size", 0)) // (1024 * 1024)
                break
    except (httpx.HTTPError, OSError):
        pass

    return profile


def _kv_bytes_per_token(p: dict) -> int | None:
    """Estimation des octets de cache KV par jeton (KV en f16)."""
    blocks = p.get("block_count")
    kv_heads = p.get("head_count_kv")
    heads = p.get("head_count")
    emb = p.get("embedding_length")
    if not all((blocks, kv_heads, heads, emb)):
        return None
    head_dim = emb / heads
    # 2 (clé+valeur) * couches * têtes_kv * dim_tête * 2 octets (f16)
    return int(2 * blocks * kv_heads * head_dim * 2)


def _round_ctx(candidate: int, ctx_max: int | None) -> int:
    cap = ctx_max or CTX_STEPS[-1]
    best = CTX_STEPS[0]
    for step in CTX_STEPS:
        if step <= candidate and step <= cap:
            best = step
    # Si le modèle plafonne bas, respecte son contexte max.
    return min(best, cap)


# ── Recommandation ───────────────────────────────────────────────────────
async def recommend(model: str) -> dict:
    gpu = detect_gpu()
    prof = await model_profile(model)
    ctx_max = prof.get("context_length")

    rationale: list[str] = []

    if not gpu["available"]:
        # GPU non détecté CÔTÉ LOKI : Ollama tourne probablement sur une autre
        # machine. On ne force surtout PAS num_ctx (0 = défaut du modèle), sinon
        # Ollama chargerait une instance distincte qui déborderait sur le CPU.
        num_ctx = 0
        rationale.append(
            "GPU non détecté côté Loki (Ollama distant ?) — contexte laissé au "
            "défaut du modèle pour rester sur le GPU. Déclare GPU_VRAM_MB pour "
            "un réglage précis."
        )
    else:
        vram = gpu["vram_total_mb"]
        model_mb = prof.get("size_mb") or _fallback_model_mb(prof)
        budget = vram - model_mb - VRAM_OVERHEAD_MB - VRAM_COMPUTE_BUFFER_MB
        kv_per_tok = _kv_bytes_per_token(prof)

        if budget <= 256:
            num_ctx = _round_ctx(2048, ctx_max)
            rationale.append("VRAM insuffisante pour les poids — contexte minimal.")
        elif kv_per_tok:
            tokens_fit = int(budget * 1024 * 1024 / kv_per_tok)
            num_ctx = _round_ctx(tokens_fit, ctx_max)
            rationale.append(
                f"{vram} Mo VRAM − {model_mb} Mo poids → budget KV "
                f"{budget} Mo (~{kv_per_tok // 1024} Ko/jeton)."
            )
        else:
            # Archi inconnue : paliers selon le budget VRAM restant.
            num_ctx = _round_ctx(_tier_ctx(budget), ctx_max)
            rationale.append("Archi modèle incomplète — estimation par paliers.")

    # Jetons de sortie : moitié du contexte, borné. Si num_ctx=0 (auto), on
    # garde une valeur raisonnable sans la réduire.
    max_tokens = 2048 if num_ctx == 0 else max(512, min(num_ctx // 2, 8192))

    return {
        "gpu": gpu,
        "model": model,
        "model_profile": {
            "context_length": ctx_max,
            "parameter_size": prof.get("parameter_size"),
            "quantization": prof.get("quantization"),
            "size_mb": prof.get("size_mb"),
        },
        "recommended": {"num_ctx": num_ctx, "max_tokens": max_tokens},
        "rationale": " ".join(rationale),
    }


async def placement(model: str) -> dict:
    """Lit /api/ps : le modèle est-il chargé sur GPU, CPU, ou un mix ?"""
    try:
        loaded = await ollama.ps()
    except (httpx.HTTPError, OSError):
        return {"loaded": False}

    for m in loaded:
        if m.get("name") == model or m.get("model") == model:
            size = m.get("size", 0) or 0
            size_vram = m.get("size_vram", 0) or 0
            if size <= 0:
                where = "inconnu"
            elif size_vram >= size * 0.99:
                where = "gpu"
            elif size_vram <= size * 0.01:
                where = "cpu"
            else:
                where = "mixte"
            pct = int(size_vram / size * 100) if size else 0
            return {"loaded": True, "where": where, "gpu_percent": pct,
                    "size_mb": size // (1024 * 1024)}
    return {"loaded": False}


def _fallback_model_mb(prof: dict) -> int:
    """Estime la taille des poids si /api/tags n'a rien donné."""
    ps = (prof.get("parameter_size") or "").upper().replace("B", "")
    try:
        billions = float(ps)
    except ValueError:
        billions = 8.0
    # ~0.6 Go/milliard en Q4, approximation prudente.
    return int(billions * 600)


def _tier_ctx(budget_mb: int) -> int:
    if budget_mb >= 12000:
        return 32768
    if budget_mb >= 8000:
        return 16384
    if budget_mb >= 5000:
        return 12288
    if budget_mb >= 3000:
        return 8192
    if budget_mb >= 1500:
        return 4096
    return 2048
