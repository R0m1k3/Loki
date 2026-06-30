"""Client HTTP léger pour l'API Ollama.

On utilise httpx directement (plutôt que le SDK) pour garder le contrôle
total sur le streaming et n'embarquer aucune dépendance superflue.
"""
from __future__ import annotations

import json
from typing import AsyncIterator

import httpx

from .config import settings


class OllamaError(RuntimeError):
    """Erreur renvoyée par Ollama (statut HTTP ≥ 400 ou champ ``error`` dans le flux).

    Ollama signale certains échecs *au milieu* d'un flux streaming (HTTP 200)
    via une ligne JSON ``{"error": "..."}`` — typiquement un débordement mémoire
    ou un contexte trop grand. On lève alors cette exception pour que l'appelant
    la remonte à l'utilisateur au lieu de l'avaler silencieusement.
    """


# Connexion rapide à échouer si Ollama est injoignable, mais lecture sans limite :
# une génération longue (ou un chargement de modèle sur CPU) ne doit pas couper.
_STREAM_TIMEOUT = httpx.Timeout(connect=10.0, read=None, write=30.0, pool=10.0)


async def _raise_for_stream_status(resp: httpx.Response) -> None:
    """Lève une ``OllamaError`` détaillée si la réponse streaming est en erreur.

    Sur une réponse en flux, ``raise_for_status`` n'inclut pas le corps ; on le
    lit explicitement pour exposer le message d'Ollama (modèle absent, etc.).
    """
    if resp.status_code < 400:
        return
    body = await resp.aread()
    detail = body.decode(errors="replace").strip()
    try:
        detail = json.loads(detail).get("error", detail)
    except (json.JSONDecodeError, AttributeError):
        pass
    raise OllamaError(f"Ollama a renvoyé {resp.status_code} : {detail[:500]}")


class OllamaClient:
    """Enveloppe asynchrone autour de l'API REST d'Ollama."""

    def __init__(self, host: str | None = None) -> None:
        self.host = (host or settings.ollama_host).rstrip("/")

    async def ping(self) -> dict:
        """Vérifie la connexion et renvoie la version d'Ollama."""
        async with httpx.AsyncClient(timeout=5.0, follow_redirects=True) as client:
            resp = await client.get(f"{self.host}/api/version")
            resp.raise_for_status()
            return resp.json()

    async def list_models(self) -> list[dict]:
        """Liste les modèles installés localement (/api/tags)."""
        async with httpx.AsyncClient(timeout=10.0, follow_redirects=True) as client:
            resp = await client.get(f"{self.host}/api/tags")
            resp.raise_for_status()
            return resp.json().get("models", [])

    async def ps(self) -> list[dict]:
        """Modèles actuellement chargés et leur répartition VRAM/CPU (/api/ps)."""
        async with httpx.AsyncClient(timeout=5.0, follow_redirects=True) as client:
            resp = await client.get(f"{self.host}/api/ps")
            resp.raise_for_status()
            return resp.json().get("models", [])

    async def show(self, name: str) -> dict:
        """Métadonnées détaillées d'un modèle (/api/show)."""
        async with httpx.AsyncClient(timeout=15.0, follow_redirects=True) as client:
            resp = await client.post(f"{self.host}/api/show", json={"name": name})
            resp.raise_for_status()
            return resp.json()

    async def pull_model(self, name: str) -> AsyncIterator[dict]:
        """Télécharge un modèle en streamant la progression (/api/pull)."""
        async with httpx.AsyncClient(
            timeout=_STREAM_TIMEOUT, follow_redirects=True
        ) as client:
            async with client.stream(
                "POST", f"{self.host}/api/pull", json={"name": name}
            ) as resp:
                await _raise_for_stream_status(resp)
                async for line in resp.aiter_lines():
                    if not line.strip():
                        continue
                    try:
                        chunk = json.loads(line)
                    except json.JSONDecodeError:
                        continue
                    if isinstance(chunk, dict) and chunk.get("error"):
                        raise OllamaError(str(chunk["error"]))
                    yield chunk

    async def chat(
        self,
        model: str,
        messages: list[dict],
        *,
        tools: list[dict] | None = None,
        options: dict | None = None,
        think: bool | None = None,
        stream: bool = True,
    ) -> AsyncIterator[dict]:
        """Conversation avec le modèle, en streaming token par token."""
        payload: dict = {"model": model, "messages": messages, "stream": stream}
        if tools:
            payload["tools"] = tools
        if options:
            payload["options"] = options
        # think=False désactive le raisonnement des modèles « thinking ».
        if think is not None:
            payload["think"] = think

        async with httpx.AsyncClient(
            timeout=_STREAM_TIMEOUT, follow_redirects=True
        ) as client:
            async with client.stream(
                "POST", f"{self.host}/api/chat", json=payload
            ) as resp:
                await _raise_for_stream_status(resp)
                async for line in resp.aiter_lines():
                    if not line.strip():
                        continue
                    try:
                        chunk = json.loads(line)
                    except json.JSONDecodeError:
                        # Ligne partielle / non-JSON : on l'ignore plutôt que de
                        # faire planter tout le flux.
                        continue
                    # Échec en cours de génération (OOM, contexte trop grand…) :
                    # Ollama l'émet dans le flux avec HTTP 200. On le remonte.
                    if isinstance(chunk, dict) and chunk.get("error"):
                        raise OllamaError(str(chunk["error"]))
                    yield chunk


ollama = OllamaClient()
