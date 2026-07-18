"""Client MCP : catalogue préconfiguré + sessions vers les serveurs activés.

Un serveur désactivé n'est jamais démarré et n'expose aucun outil au modèle
(chaque outil injecté coûte du contexte). Connexion lazy au premier message,
session réutilisée ensuite. Toute panne est non bloquante pour le chat.
"""
from __future__ import annotations

import asyncio
import logging
import shlex
from contextlib import AsyncExitStack

from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client

from . import db

logger = logging.getLogger(__name__)

MCP_KEY = "mcp"

# Catalogue embarqué. command=None => serveur "custom" (commande utilisateur).
CATALOG: dict[str, dict] = {
    "playwright": {
        "label": "Playwright (navigateur)",
        "description": "Pilote un vrai navigateur : naviguer, cliquer, lire "
                       "la console, captures. L'agent teste réellement ses pages.",
        "command": ["npx", "@playwright/mcp@latest", "--headless"],
        "url_param": False,
        "env_params": [],
        # Limite le nombre d'outils injectés dans le prompt.
        "expose": [
            "browser_navigate", "browser_click", "browser_type",
            "browser_snapshot", "browser_console_messages",
            "browser_take_screenshot",
        ],
    },
    "context7": {
        "label": "Context7 (documentation)",
        "description": "Documentation à jour de n'importe quelle librairie ou "
                       "framework (React, FastAPI, Tailwind…).",
        "command": ["npx", "-y", "@upstash/context7-mcp"],
        "url_param": False,
        "env_params": [],
        "expose": None,
    },
    "fetch": {
        "label": "Fetch (lecture web)",
        "description": "Lit proprement n'importe quelle URL (markdown épuré).",
        "command": ["python", "-m", "mcp_server_fetch"],
        "url_param": False,
        "env_params": [],
        "expose": None,
    },
    "searxng": {
        "label": "SearxNG (recherche web)",
        "description": "Vraie recherche web via une instance SearxNG "
                       "(renseigner SEARXNG_URL).",
        "command": ["npx", "-y", "mcp-searxng"],
        "url_param": False,
        "env_params": ["SEARXNG_URL"],
        "expose": None,
    },
    "custom": {
        "label": "Personnalisé",
        "description": "N'importe quel serveur MCP : colle une commande "
                       "(stdio) ou une URL (streamable HTTP).",
        "command": None,
        "url_param": True,
        "env_params": [],
        "expose": None,
    },
}


def get_mcp_state() -> dict:
    """État activé/params de chaque serveur du catalogue (défaut : désactivé)."""
    stored = db.get_config_value(MCP_KEY) or {}
    return {
        sid: {
            "enabled": bool(stored.get(sid, {}).get("enabled", False)),
            "params": dict(stored.get(sid, {}).get("params", {})),
        }
        for sid in CATALOG
    }


def set_mcp_state(sid: str, *, enabled: bool, params: dict) -> dict:
    if sid not in CATALOG:
        raise KeyError(sid)
    state = get_mcp_state()
    state[sid] = {"enabled": enabled, "params": dict(params)}
    db.set_config_value(MCP_KEY, state)
    return state


_CALL_TIMEOUT = 30.0
_CONNECT_TIMEOUT = 20.0
_MAX_RESULT_CHARS = 8000


class _ServerConn:
    """Session vivante vers un serveur MCP (process stdio + handshake)."""

    def __init__(self, sid: str) -> None:
        self.sid = sid
        self.stack = AsyncExitStack()
        self.session: ClientSession | None = None
        self.tools: list[dict] = []  # définitions format Ollama

    async def start(self) -> None:
        entry = CATALOG[self.sid]
        params = get_mcp_state()[self.sid]["params"]
        spec = params.get("command", "").strip()
        if entry["command"] is None and spec.startswith(("http://", "https://")):
            # Serveur "custom" en streamable HTTP (URL collée par l'utilisateur).
            from mcp.client.streamable_http import streamablehttp_client
            read, write, _ = await self.stack.enter_async_context(
                streamablehttp_client(spec)
            )
        else:
            if entry["command"] is None:
                # Serveur "custom" : commande stdio saisie par l'utilisateur.
                command = shlex.split(spec)
                if not command:
                    raise ValueError("commande du serveur personnalisé vide")
            else:
                command = list(entry["command"])
            # Paramètres obligatoires (ex. SEARXNG_URL) : refus clair AVANT le
            # lancement, plutôt qu'un échec cryptique à chaque appel d'outil.
            missing = [k for k in entry["env_params"] if not params.get(k)]
            if missing:
                raise ValueError(
                    f"{', '.join(missing)} requis — renseigne ce champ dans la "
                    "carte du serveur (Configuration → Serveurs MCP)"
                )
            env = {k: params[k] for k in entry["env_params"] if params.get(k)}
            server = StdioServerParameters(
                command=command[0], args=command[1:], env=env or None
            )
            read, write = await self.stack.enter_async_context(
                stdio_client(server)
            )
        self.session = await self.stack.enter_async_context(
            ClientSession(read, write)
        )
        await asyncio.wait_for(self.session.initialize(), _CONNECT_TIMEOUT)
        listed = await asyncio.wait_for(self.session.list_tools(), _CONNECT_TIMEOUT)
        expose = entry.get("expose")
        self.tools = [
            {
                "type": "function",
                "function": {
                    "name": f"mcp_{self.sid}_{t.name}",
                    "description": (t.description or t.name)[:400],
                    "parameters": t.inputSchema
                    or {"type": "object", "properties": {}},
                },
            }
            for t in listed.tools
            if expose is None or t.name in expose
        ]

    async def close(self) -> None:
        try:
            await self.stack.aclose()
        except Exception:  # process déjà mort : sans importance
            pass


class McpManager:
    """Sessions MCP lazy + dispatch d'appels d'outils, jamais bloquant."""

    def __init__(self) -> None:
        self._conns: dict[str, _ServerConn] = {}
        self._errors: dict[str, str] = {}
        self._notices: list[str] = []
        self._lock = asyncio.Lock()

    async def _ensure(self, sid: str) -> _ServerConn | None:
        if sid in self._conns:
            return self._conns[sid]
        try:
            conn = _ServerConn(sid)
            await conn.start()
        except Exception as exc:
            msg = str(exc)[:300] or exc.__class__.__name__
            logger.warning("Serveur MCP %s indisponible : %s", sid, msg)
            self._errors[sid] = msg
            self._notices.append(
                f"Serveur MCP « {CATALOG[sid]['label']} » indisponible : {msg}"
            )
            return None
        self._errors.pop(sid, None)
        self._conns[sid] = conn
        return conn

    async def tool_definitions(self) -> list[dict]:
        """Outils des serveurs activés (connexion lazy, pannes ignorées)."""
        defs: list[dict] = []
        async with self._lock:
            state = get_mcp_state()
            # Ferme les serveurs désactivés entre-temps.
            for sid in [s for s in self._conns if not state[s]["enabled"]]:
                await self._conns.pop(sid).close()
            for sid, st in state.items():
                if not st["enabled"]:
                    continue
                conn = await self._ensure(sid)
                if conn:
                    defs.extend(conn.tools)
        return defs

    async def call_tool(self, prefixed_name: str, args: dict) -> dict:
        # mcp_<sid>_<tool> ; sid ne contient pas de "_", le nom d'outil peut.
        try:
            _, sid, tool = prefixed_name.split("_", 2)
        except ValueError:
            return {"ok": False, "content": "", "summary": "nom d'outil invalide"}
        async with self._lock:
            conn = self._conns.get(sid) or await self._ensure(sid)
        if conn is None or conn.session is None:
            return {"ok": False, "content": "",
                    "summary": f"serveur MCP {sid} indisponible"}
        try:
            result = await asyncio.wait_for(
                conn.session.call_tool(tool, args or {}), _CALL_TIMEOUT
            )
        except Exception as exc:
            # Session probablement morte : on la ferme, retry au prochain tour.
            async with self._lock:
                dead = self._conns.pop(sid, None)
            if dead:
                await dead.close()
            return {"ok": False, "content": "",
                    "summary": f"échec MCP : {str(exc)[:200]}"}
        parts = [
            c.text for c in result.content
            if getattr(c, "type", "") == "text" and getattr(c, "text", "")
        ]
        content = "\n".join(parts)[:_MAX_RESULT_CHARS]
        ok = not bool(getattr(result, "isError", False))
        return {
            "ok": ok,
            "content": content,
            "summary": (content.splitlines()[0][:120] if content else "terminé")
            if ok else (content[:120] or "erreur outil MCP"),
        }

    def statuses(self) -> dict[str, dict]:
        state = get_mcp_state()
        out = {}
        for sid, st in state.items():
            if sid in self._conns:
                s = "connected"
            elif sid in self._errors:
                s = "error"
            else:
                s = "inactive"
            out[sid] = {
                "state": s if st["enabled"] else "inactive",
                "error": self._errors.get(sid),
                "tools": len(self._conns[sid].tools) if sid in self._conns else 0,
            }
        return out

    def notices(self) -> list[str]:
        out, self._notices = self._notices, []
        return out

    async def test_server(self, sid: str) -> dict:
        """Connexion d'essai indépendante (n'altère pas les sessions)."""
        conn = _ServerConn(sid)
        try:
            await conn.start()
            return {"ok": True,
                    "tools": [d["function"]["name"] for d in conn.tools],
                    "error": None}
        except Exception as exc:
            return {"ok": False, "tools": [],
                    "error": str(exc)[:300] or exc.__class__.__name__}
        finally:
            await conn.close()

    async def aclose(self) -> None:
        for conn in list(self._conns.values()):
            await conn.close()
        self._conns.clear()


manager = McpManager()
