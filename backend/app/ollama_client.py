"""Client HTTP léger pour l'API Ollama.

On utilise httpx directement (plutôt que le SDK) pour garder le contrôle
total sur le streaming et n'embarquer aucune dépendance superflue.
"""
from __future__ import annotations

import asyncio
import json
import time
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

# Un chat déclenche plusieurs appels Ollama (routage, embed, plan, agent…) :
# un pool keep-alive partagé évite un handshake TCP à chaque appel.
_LIMITS = httpx.Limits(
    max_connections=20, max_keepalive_connections=10, keepalive_expiry=30.0
)


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
        self._client: httpx.AsyncClient | None = None
        # Cache court de /api/tags : la liste des modèles installés change
        # rarement mais est consultée par plusieurs modules à chaque message.
        self._tags_at = 0.0
        self._tags: list[dict] = []

    def _http(self) -> httpx.AsyncClient:
        """Client partagé (pool keep-alive), créé paresseusement.

        Un seul worker uvicorn / une seule boucle : la création lazy est sûre.
        Le garde ``is_closed`` recrée le client si un arrêt l'a fermé.
        """
        if self._client is None or self._client.is_closed:
            self._client = httpx.AsyncClient(
                timeout=_STREAM_TIMEOUT, follow_redirects=True, limits=_LIMITS
            )
        return self._client

    async def aclose(self) -> None:
        """Ferme le pool partagé (appelé au shutdown de l'app)."""
        if self._client is not None and not self._client.is_closed:
            await self._client.aclose()

    async def ping(self) -> dict:
        """Vérifie la connexion et renvoie la version d'Ollama."""
        resp = await self._http().get(f"{self.host}/api/version", timeout=5.0)
        resp.raise_for_status()
        return resp.json()

    async def list_models(self) -> list[dict]:
        """Liste les modèles installés localement (/api/tags), sans cache."""
        resp = await self._http().get(f"{self.host}/api/tags", timeout=10.0)
        resp.raise_for_status()
        models = resp.json().get("models", [])
        self._tags, self._tags_at = models, time.monotonic()
        return models

    async def list_models_cached(self, ttl: float = 30.0) -> list[dict]:
        """Comme ``list_models`` mais avec un cache court partagé.

        Utilisé par les chemins chauds (routage code, résolution embed, route
        /api/models) pour ne pas marteler /api/tags à chaque message.
        """
        if self._tags and time.monotonic() - self._tags_at < ttl:
            return self._tags
        return await self.list_models()

    def invalidate_tags_cache(self) -> None:
        """Force un rafraîchissement après un pull ou une suppression de modèle."""
        self._tags_at = 0.0
        self._tags = []

    async def embed(
        self, model: str, texts: list[str], keep_alive: str = "30m"
    ) -> list[list[float]]:
        """Vecteurs d'embedding pour une liste de textes (/api/embed).

        ``keep_alive`` long : le modèle d'embedding est minuscule (<0,5 Go) et
        sollicité à chaque message (recall + indexation) — le laisser chargé
        évite un aller-retour VRAM permanent avec le modèle de chat.
        """
        resp = await self._http().post(
            f"{self.host}/api/embed",
            json={"model": model, "input": texts, "keep_alive": keep_alive},
            timeout=30.0,
        )
        resp.raise_for_status()
        return resp.json().get("embeddings", [])

    async def ps(self) -> list[dict]:
        """Modèles actuellement chargés et leur répartition VRAM/CPU (/api/ps)."""
        resp = await self._http().get(f"{self.host}/api/ps", timeout=5.0)
        resp.raise_for_status()
        return resp.json().get("models", [])

    async def show(self, name: str) -> dict:
        """Métadonnées détaillées d'un modèle (/api/show)."""
        resp = await self._http().post(
            f"{self.host}/api/show", json={"name": name}, timeout=15.0
        )
        resp.raise_for_status()
        return resp.json()

    async def delete_model(self, name: str) -> dict:
        """Supprime un modèle installé (/api/delete)."""
        resp = await self._http().request(
            "DELETE", f"{self.host}/api/delete", json={"model": name}, timeout=30.0
        )
        resp.raise_for_status()
        self.invalidate_tags_cache()
        return resp.json() if resp.content else {"status": "success"}

    async def pull_model(self, name: str) -> AsyncIterator[dict]:
        """Télécharge un modèle en streamant la progression (/api/pull)."""
        async with self._http().stream(
            "POST",
            f"{self.host}/api/pull",
            json={"name": name},
            timeout=_STREAM_TIMEOUT,
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
        self.invalidate_tags_cache()

    async def warm(
        self, model: str, keep_alive: str = "30m", options: dict | None = None
    ) -> dict:
        """Précharge un modèle en VRAM sans générer (/api/generate sans prompt).

        Le paramètre keep_alive fixe la durée de rétention en mémoire. Les
        `options` (num_ctx, num_batch, num_gpu…) DOIVENT correspondre à celles du
        chat : sinon Ollama chargerait un runner distinct puis en rechargerait un
        autre au premier message — un double chargement très coûteux pour un gros
        modèle.
        """
        payload: dict = {"model": model, "keep_alive": keep_alive, "stream": False}
        if options:
            payload["options"] = options
        # Le chargement se fait désormais en tâche de fond côté API Loki. On lui
        # laisse jusqu'à dix minutes pour les gros modèles ou un stockage lent.
        timeout = httpx.Timeout(connect=10.0, read=600.0, write=30.0, pool=10.0)
        for attempt in range(2):
            resp = await self._http().post(
                f"{self.host}/api/generate", json=payload, timeout=timeout
            )
            try:
                # Inclut le corps JSON d'Ollama dans l'erreur (OOM, runner…),
                # contrairement à raise_for_status qui ne montrait que « 500 ».
                await _raise_for_stream_status(resp)
            except OllamaError:
                if resp.status_code >= 500 and attempt == 0:
                    await asyncio.sleep(2)
                    continue
                raise
            return resp.json()
        raise OllamaError("préchargement interrompu sans réponse")

    async def chat(
        self,
        model: str,
        messages: list[dict],
        *,
        tools: list[dict] | None = None,
        options: dict | None = None,
        think: bool | None = None,
        keep_alive: str | None = None,
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
        # keep_alive : durée de maintien du modèle en VRAM après la réponse.
        if keep_alive is not None:
            payload["keep_alive"] = keep_alive

        async with self._http().stream(
            "POST", f"{self.host}/api/chat", json=payload, timeout=_STREAM_TIMEOUT
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
