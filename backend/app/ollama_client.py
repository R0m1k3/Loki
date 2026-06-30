"""Client HTTP léger pour l'API Ollama.

On utilise httpx directement (plutôt que le SDK) pour garder le contrôle
total sur le streaming et n'embarquer aucune dépendance superflue.
"""
from __future__ import annotations

import json
from typing import AsyncIterator

import httpx

from .config import settings


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

    async def show(self, name: str) -> dict:
        """Métadonnées détaillées d'un modèle (/api/show)."""
        async with httpx.AsyncClient(timeout=15.0, follow_redirects=True) as client:
            resp = await client.post(f"{self.host}/api/show", json={"name": name})
            resp.raise_for_status()
            return resp.json()

    async def pull_model(self, name: str) -> AsyncIterator[dict]:
        """Télécharge un modèle en streamant la progression (/api/pull)."""
        async with httpx.AsyncClient(timeout=None, follow_redirects=True) as client:
            async with client.stream(
                "POST", f"{self.host}/api/pull", json={"name": name}
            ) as resp:
                resp.raise_for_status()
                async for line in resp.aiter_lines():
                    if line.strip():
                        yield json.loads(line)

    async def chat(
        self,
        model: str,
        messages: list[dict],
        *,
        tools: list[dict] | None = None,
        options: dict | None = None,
        stream: bool = True,
    ) -> AsyncIterator[dict]:
        """Conversation avec le modèle, en streaming token par token."""
        payload: dict = {"model": model, "messages": messages, "stream": stream}
        if tools:
            payload["tools"] = tools
        if options:
            payload["options"] = options

        async with httpx.AsyncClient(timeout=None, follow_redirects=True) as client:
            async with client.stream(
                "POST", f"{self.host}/api/chat", json=payload
            ) as resp:
                resp.raise_for_status()
                async for line in resp.aiter_lines():
                    if line.strip():
                        yield json.loads(line)


ollama = OllamaClient()
