# MCP + Skills + Boucle code vérifiée — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** rendre les modèles locaux effectivement plus intelligents en dotant l'agent Loki d'outils MCP préconfigurés (toggleables, zéro coût si inactifs), de skills automatiques (méthodes expertes injectées), et d'une vérification statique automatique du code produit.

**Architecture:** un gestionnaire MCP singleton (pattern `ollama_client`) maintient des sessions lazy vers les serveurs activés et expose leurs outils au format function-calling Ollama, préfixés `mcp_<id>_` ; la boucle agent les dispatche comme les outils natifs. Un routeur lexical (pattern `router.py`) choisit au plus une skill markdown par message et l'injecte en message système. Un outil `run_check` (analyse statique uniquement) est appelé automatiquement après chaque écriture de code.

**Tech Stack:** FastAPI, SDK Python officiel `mcp`, Node.js (serveurs npx), React/zustand.

**Spec:** `docs/superpowers/specs/2026-07-18-mcp-skills-design.md`

## Global Constraints

- Serveur MCP désactivé = process jamais lancé, zéro outil dans le prompt.
- Panne MCP (crash, timeout) ne bloque JAMAIS le chat : serveur marqué en erreur, notice SSE, retry au message suivant.
- Timeout 30 s par appel d'outil MCP ; résultats tronqués à 8 000 caractères.
- Au plus UNE skill par message ; sélection lexicale, aucun appel LLM.
- `run_check` : analyse statique seulement (py_compile, node --check, check_html, json.loads) — JAMAIS d'exécution de code.
- UI en français ; styles existants (border-[3px] border-line, Card/Toggle de SettingsView).
- Un seul worker uvicorn ; état en mémoire process OK.
- Tests backend : pytest (nouveau, `backend/tests/`), lancés avec `python -m pytest` depuis `backend/`.
- Après chaque tâche frontend : `cd frontend && npx tsc -b` doit passer.

---

### Task 1: Infra pytest + catalogue et état MCP

**Files:**
- Create: `backend/tests/__init__.py` (vide)
- Create: `backend/tests/test_mcp_config.py`
- Create: `backend/app/mcp_client.py` (partie catalogue/état seulement)
- Modify: `backend/requirements.txt`

**Interfaces:**
- Produces: `CATALOG: dict[str, dict]` (clés `playwright|context7|fetch|searxng|custom`, valeurs `{label, description, command: list[str] | None, url_param: bool, env_params: list[str], expose: list[str] | None}`) ; `get_mcp_state() -> dict` et `set_mcp_state(sid, enabled, params) -> dict` persistés via `db.get_config_value("mcp")/db.set_config_value("mcp", …)`.

- [ ] **Step 1: Ajouter les dépendances**

Dans `backend/requirements.txt`, ajouter à la fin :

```
mcp>=1.9
pytest>=8.3
pytest-asyncio>=0.25
```

Puis : `cd backend && python -m pip install mcp pytest pytest-asyncio`

- [ ] **Step 2: Écrire le test qui échoue**

`backend/tests/test_mcp_config.py` :

```python
import os
import tempfile

os.environ.setdefault("DATA_DIR", tempfile.mkdtemp())
os.environ.setdefault("WORKSPACE_DIR", tempfile.mkdtemp())

from app import db  # noqa: E402
from app import mcp_client  # noqa: E402

db.init_db()


def test_catalog_has_preconfigured_servers():
    for sid in ("playwright", "context7", "fetch", "searxng", "custom"):
        assert sid in mcp_client.CATALOG
        assert mcp_client.CATALOG[sid]["label"]


def test_state_defaults_disabled():
    state = mcp_client.get_mcp_state()
    assert set(state) == set(mcp_client.CATALOG)
    assert all(not s["enabled"] for s in state.values())


def test_toggle_persists():
    mcp_client.set_mcp_state("fetch", enabled=True, params={})
    assert mcp_client.get_mcp_state()["fetch"]["enabled"] is True
    mcp_client.set_mcp_state("fetch", enabled=False, params={})
    assert mcp_client.get_mcp_state()["fetch"]["enabled"] is False


def test_custom_params_persist():
    mcp_client.set_mcp_state(
        "custom", enabled=False, params={"command": "npx -y some-mcp"}
    )
    assert (
        mcp_client.get_mcp_state()["custom"]["params"]["command"]
        == "npx -y some-mcp"
    )
```

- [ ] **Step 3: Vérifier l'échec**

Run: `cd backend && python -m pytest tests/test_mcp_config.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'app.mcp_client'`

- [ ] **Step 4: Implémenter catalogue + état**

`backend/app/mcp_client.py` :

```python
"""Client MCP : catalogue préconfiguré + sessions vers les serveurs activés.

Un serveur désactivé n'est jamais démarré et n'expose aucun outil au modèle
(chaque outil injecté coûte du contexte). Connexion lazy au premier message,
session réutilisée ensuite. Toute panne est non bloquante pour le chat.
"""
from __future__ import annotations

import logging

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
```

- [ ] **Step 5: Vérifier le passage**

Run: `cd backend && python -m pytest tests/test_mcp_config.py -v`
Expected: 4 PASS

- [ ] **Step 6: Commit**

```bash
git add backend/tests backend/app/mcp_client.py backend/requirements.txt
git commit -m "feat(mcp): catalogue préconfiguré + état persisté + infra pytest"
```

---

### Task 2: Gestionnaire de sessions MCP (lazy, conversion schémas, appels)

**Files:**
- Modify: `backend/app/mcp_client.py` (ajout de la classe manager)
- Create: `backend/tests/test_mcp_manager.py`
- Modify: `backend/app/main.py` (aclose au shutdown)

**Interfaces:**
- Consumes: `CATALOG`, `get_mcp_state()` (Task 1).
- Produces: singleton `manager = McpManager()` avec :
  - `async def tool_definitions(self) -> list[dict]` — outils Ollama des serveurs activés (connexion lazy ; serveur en panne ignoré + noté). Format : `{"type": "function", "function": {"name": "mcp_<sid>_<tool>", "description": …, "parameters": …}}`.
  - `async def call_tool(self, prefixed_name: str, args: dict) -> dict` — retourne `{"ok": bool, "content": str, "summary": str}` (content tronqué 8 000 c.).
  - `def statuses(self) -> dict[str, dict]` — `{sid: {"state": "inactive|connected|error", "error": str | None, "tools": int}}`.
  - `def notices(self) -> list[str]` — messages de panne accumulés depuis le dernier appel (vidés à la lecture).
  - `async def aclose(self)` ; `async def test_server(self, sid) -> dict` (connexion d'essai, renvoie `{"ok", "tools": [noms], "error"}`).

- [ ] **Step 1: Test qui échoue (serveur MCP factice en Python pur)**

`backend/tests/test_mcp_manager.py` — utilise un mini serveur MCP stdio écrit avec le SDK (process enfant) :

```python
import os
import sys
import tempfile
import textwrap

os.environ.setdefault("DATA_DIR", tempfile.mkdtemp())
os.environ.setdefault("WORKSPACE_DIR", tempfile.mkdtemp())

import pytest  # noqa: E402

from app import db, mcp_client  # noqa: E402

db.init_db()

# Serveur MCP minimal : un outil "echo" qui renvoie son argument.
_FAKE_SERVER = textwrap.dedent("""
    from mcp.server.fastmcp import FastMCP
    mcp = FastMCP("fake")

    @mcp.tool()
    def echo(text: str) -> str:
        \"\"\"Répète le texte fourni.\"\"\"
        return "echo:" + text

    mcp.run()
""")


@pytest.fixture()
def fake_server_cmd(tmp_path):
    path = tmp_path / "fake_mcp.py"
    path.write_text(_FAKE_SERVER, encoding="utf-8")
    return [sys.executable, str(path)]


@pytest.mark.asyncio
async def test_tools_exposed_and_called(fake_server_cmd, monkeypatch):
    monkeypatch.setitem(
        mcp_client.CATALOG, "fake",
        {"label": "Fake", "description": "", "command": fake_server_cmd,
         "url_param": False, "env_params": [], "expose": None},
    )
    mcp_client.set_mcp_state("fake", enabled=True, params={})
    mgr = mcp_client.McpManager()
    try:
        defs = await mgr.tool_definitions()
        names = [d["function"]["name"] for d in defs]
        assert "mcp_fake_echo" in names
        result = await mgr.call_tool("mcp_fake_echo", {"text": "bonjour"})
        assert result["ok"] is True
        assert "echo:bonjour" in result["content"]
    finally:
        await mgr.aclose()
        mcp_client.set_mcp_state("fake", enabled=False, params={})


@pytest.mark.asyncio
async def test_disabled_server_exposes_nothing():
    mgr = mcp_client.McpManager()
    try:
        assert await mgr.tool_definitions() == []
    finally:
        await mgr.aclose()


@pytest.mark.asyncio
async def test_broken_server_never_raises(monkeypatch):
    monkeypatch.setitem(
        mcp_client.CATALOG, "broken",
        {"label": "Broken", "description": "",
         "command": [sys.executable, "-c", "import sys; sys.exit(3)"],
         "url_param": False, "env_params": [], "expose": None},
    )
    mcp_client.set_mcp_state("broken", enabled=True, params={})
    mgr = mcp_client.McpManager()
    try:
        assert await mgr.tool_definitions() == []
        assert mgr.statuses()["broken"]["state"] == "error"
        assert any("broken" in n.lower() or "Broken" in n for n in mgr.notices())
    finally:
        await mgr.aclose()
        mcp_client.set_mcp_state("broken", enabled=False, params={})
```

Ajouter `backend/pytest.ini` :

```ini
[pytest]
asyncio_mode = auto
```

(Créer le fichier ; `asyncio_mode = auto` évite de décorer chaque test.)

- [ ] **Step 2: Vérifier l'échec**

Run: `cd backend && python -m pytest tests/test_mcp_manager.py -v`
Expected: FAIL — `AttributeError: module 'app.mcp_client' has no attribute 'McpManager'`

- [ ] **Step 3: Implémenter le manager**

Ajouter à `backend/app/mcp_client.py` :

```python
import asyncio
import shlex
from contextlib import AsyncExitStack

from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client

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
            msg = str(exc)[:300]
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
            return {"ok": False, "tools": [], "error": str(exc)[:300]}
        finally:
            await conn.close()

    async def aclose(self) -> None:
        for conn in list(self._conns.values()):
            await conn.close()
        self._conns.clear()


manager = McpManager()
```

- [ ] **Step 4: Fermer au shutdown**

Dans `backend/app/main.py`, après `await ollama.aclose()` :

```python
    from .mcp_client import manager as mcp_manager
    await mcp_manager.aclose()
```

- [ ] **Step 5: Vérifier le passage**

Run: `cd backend && python -m pytest tests/ -v`
Expected: tous PASS (les 3 nouveaux + les 4 de Task 1)

- [ ] **Step 6: Commit**

```bash
git add backend/app/mcp_client.py backend/app/main.py backend/tests/test_mcp_manager.py backend/pytest.ini
git commit -m "feat(mcp): gestionnaire de sessions lazy + conversion schémas + appels"
```

---

### Task 3: Routes API MCP

**Files:**
- Create: `backend/app/routes/mcp.py`
- Modify: `backend/app/main.py` (import + include_router)

**Interfaces:**
- Consumes: `mcp_client.CATALOG`, `get_mcp_state`, `set_mcp_state`, `manager.statuses()`, `manager.test_server(sid)` (Tasks 1-2).
- Produces: `GET /api/mcp` → `{servers: [{id, label, description, enabled, params, url_param, env_params, state, error, tools}]}` ; `PUT /api/mcp/{sid}` body `{enabled: bool, params: dict}` → même forme ; `POST /api/mcp/{sid}/test` → `{ok, tools, error}`.

- [ ] **Step 1: Implémenter les routes**

`backend/app/routes/mcp.py` :

```python
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
```

Dans `backend/app/main.py` : ajouter `mcp` à l'import
`from .routes import benchmark, chat, config, files, git, mcp, models, sessions, shell, system`
et `app.include_router(mcp.router)` à côté des autres.

- [ ] **Step 2: Vérifier à la main**

```bash
cd backend && python -m uvicorn app.main:app --port 8199 &
sleep 4
curl -s http://localhost:8199/api/mcp | python -m json.tool | head -20
curl -s -X PUT http://localhost:8199/api/mcp/fetch -H "Content-Type: application/json" -d '{"enabled": true, "params": {}}' | head -c 200
curl -s -X POST http://localhost:8199/api/mcp/fetch/test
```

Expected: liste des 5 serveurs ; toggle persiste ; test renvoie `{"ok": true, "tools": ["mcp_fetch_fetch"], …}` (mcp_server_fetch installé en Task 8 — avant ça `ok:false` + message d'erreur propre, PAS un crash). Tuer le serveur ensuite.

- [ ] **Step 3: Commit**

```bash
git add backend/app/routes/mcp.py backend/app/main.py
git commit -m "feat(mcp): routes GET/PUT/test du catalogue de serveurs"
```

---

### Task 4: Intégration boucle agent + chat

**Files:**
- Modify: `backend/app/agent.py` (paramètre `mcp_tools`, dispatch `mcp_*`)
- Modify: `backend/app/routes/chat.py` (récupération defs MCP dans le gather + notices)

**Interfaces:**
- Consumes: `manager.tool_definitions()`, `manager.call_tool()`, `manager.notices()` (Task 2).
- Produces: `run_agent(..., mcp_tools: list[dict] | None = None)` — les outils MCP s'ajoutent aux natifs et sont dispatchés en async.

- [ ] **Step 1: agent.py — signature et fusion des outils**

Dans `run_agent` (`backend/app/agent.py`), ajouter le paramètre après `keep_alive` :

```python
    keep_alive: str | None = None,
    mcp_tools: list[dict] | None = None,
) -> AsyncIterator[dict]:
```

Remplacer le bloc de sélection des outils (début de fonction) :

```python
    # enabled_tools=None -> tous les outils ; liste vide -> aucun outil.
    if enabled_tools is None:
        tools = list(TOOL_DEFINITIONS)
    elif enabled_tools:
        tools = [
            t for t in TOOL_DEFINITIONS if t["function"]["name"] in enabled_tools
        ]
    else:
        tools = []
    # Outils MCP des serveurs activés (déjà résolus par l'appelant).
    tools.extend(mcp_tools or [])
    if not tools:
        tools = None
```

- [ ] **Step 2: agent.py — dispatch des appels MCP**

Dans la boucle d'exécution des outils, juste APRÈS le bloc `if name == "code_task":` et AVANT le `try: result = run_tool(...)`, insérer :

```python
                # Outils MCP : dispatch asynchrone vers le serveur concerné.
                if name.startswith("mcp_"):
                    from .mcp_client import manager as mcp_manager
                    mcp_res = await mcp_manager.call_tool(name, args)
                    status = "ok" if mcp_res["ok"] else "error"
                    record = {"name": name, "args": args,
                              "summary": mcp_res["summary"], "status": status}
                    collected.append(record)
                    yield {"type": "tool_result", **record}
                    convo.append({
                        "role": "tool",
                        "tool_name": name,
                        "content": json.dumps(
                            {"ok": mcp_res["ok"], "content": mcp_res["content"]},
                            ensure_ascii=False,
                        ),
                    })
                    continue
```

- [ ] **Step 3: chat.py — résolution des defs MCP en parallèle**

Dans `backend/app/routes/chat.py`, étendre le `asyncio.gather` existant de `event_stream()` (qui résout memories/plan/code_model) avec un 4ᵉ élément :

```python
        from ..mcp_client import manager as mcp_manager

        memories, plan, code_model, mcp_tools = await asyncio.gather(
            rag.recall(req.session_id, req.content, embed_model=cfg.get("embed_model"))
            if want_rag else asyncio.sleep(0, result=[]),
            enhance.make_plan(model, req.content, options=run_opts, keep_alive=keep)
            if want_plan else asyncio.sleep(0, result=[]),
            coder.pick_code_model(model, cfg.get("code_model"))
            if use_code else asyncio.sleep(0, result=model),
            mcp_manager.tool_definitions(),
        )

        # Pannes MCP éventuelles : notice non bloquante dans le fil.
        for notice in mcp_manager.notices():
            yield _sse("notice", {"message": notice})
```

Et passer `mcp_tools=mcp_tools` à l'appel `run_agent(...)` dans `produce_events`.

- [ ] **Step 4: Vérifier**

```bash
cd backend && python -m pytest tests/ -v && python -m compileall -q app && echo OK
```

Expected: tests PASS + OK. Puis test manuel complet (nécessite Ollama) : activer `fetch` via PUT, envoyer un message « lis https://example.com et résume » → ToolCard `mcp_fetch_fetch` dans le fil. Sans Ollama, vérifier au moins que `POST /api/chat` sans serveur MCP actif se comporte comme avant.

- [ ] **Step 5: Commit**

```bash
git add backend/app/agent.py backend/app/routes/chat.py
git commit -m "feat(mcp): outils MCP dans la boucle agent (dispatch async + notices)"
```

---

### Task 5: Frontend — API client + onglet MCP dans Configuration

**Files:**
- Modify: `frontend/src/api/client.ts` (types + 3 fonctions)
- Modify: `frontend/src/panels/SettingsView.tsx` (nouvelle carte MCP)

**Interfaces:**
- Consumes: routes Task 3.
- Produces: `listMcp(): Promise<McpServer[]>`, `updateMcp(id, enabled, params): Promise<McpServer[]>`, `testMcp(id): Promise<{ok: boolean; tools: string[]; error: string | null}>` ; composant `McpCard` rendu dans SettingsView.

- [ ] **Step 1: client.ts**

Ajouter (près des autres types/fonctions) :

```typescript
export interface McpServer {
  id: string;
  label: string;
  description: string;
  url_param: boolean;
  env_params: string[];
  enabled: boolean;
  params: Record<string, string>;
  state: "inactive" | "connected" | "error";
  error: string | null;
  tools: number;
}

export async function listMcp(): Promise<McpServer[]> {
  const res = await fetch("/api/mcp");
  return (await res.json()).servers;
}

export async function updateMcp(
  id: string,
  enabled: boolean,
  params: Record<string, string>
): Promise<McpServer[]> {
  const res = await fetch(`/api/mcp/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ enabled, params }),
  });
  if (!res.ok) throw await apiError(res, "mise à jour MCP impossible");
  return (await res.json()).servers;
}

export async function testMcp(
  id: string
): Promise<{ ok: boolean; tools: string[]; error: string | null }> {
  const res = await fetch(`/api/mcp/${id}/test`, { method: "POST" });
  return res.json();
}
```

- [ ] **Step 2: SettingsView — carte MCP**

Dans `frontend/src/panels/SettingsView.tsx` : ajouter l'import
`import { listMcp, updateMcp, testMcp, type McpServer } from "../api/client";`
puis, à côté de `HardwareCard`/`BenchCard` (composants de même niveau), ajouter :

```tsx
function McpCard() {
  const [servers, setServers] = useState<McpServer[]>([]);
  const [testing, setTesting] = useState<string | null>(null);
  const [testResult, setTestResult] = useState<Record<string, string>>({});

  useEffect(() => {
    listMcp().then(setServers).catch(() => {});
  }, []);

  const toggle = async (s: McpServer) => {
    setServers(await updateMcp(s.id, !s.enabled, s.params));
  };

  const setParam = async (s: McpServer, key: string, value: string) => {
    setServers(await updateMcp(s.id, s.enabled, { ...s.params, [key]: value }));
  };

  const doTest = async (s: McpServer) => {
    setTesting(s.id);
    try {
      const r = await testMcp(s.id);
      setTestResult((m) => ({
        ...m,
        [s.id]: r.ok
          ? `✓ ${r.tools.length} outil(s) : ${r.tools.join(", ").slice(0, 120)}`
          : `✗ ${r.error ?? "échec"}`,
      }));
    } finally {
      setTesting(null);
    }
  };

  return (
    <Card
      title="Serveurs MCP"
      subtitle="Outils professionnels pour l'agent (navigateur, docs, web). Désactivé = zéro coût."
    >
      <div className="flex flex-col gap-3">
        {servers.map((s) => (
          <div key={s.id} className="border-[3px] border-line bg-base p-3">
            <div className="flex items-center gap-3">
              <span
                className={`h-[11px] w-[11px] flex-none border-2 border-line ${
                  s.state === "connected"
                    ? "bg-ok"
                    : s.state === "error"
                      ? "bg-warn"
                      : "bg-muted-2"
                }`}
                title={s.error ?? s.state}
              />
              <div className="min-w-0 flex-1">
                <div className="text-[14px] font-semibold text-ink">
                  {s.label}
                  {s.tools > 0 && (
                    <span className="ml-2 text-[11px] text-muted-2">
                      {s.tools} outil(s)
                    </span>
                  )}
                </div>
                <div className="truncate text-[12px] text-muted-2">
                  {s.description}
                </div>
              </div>
              <button
                onClick={() => doTest(s)}
                disabled={testing === s.id}
                className="border-2 border-line bg-card px-2 py-1 text-[11px] text-ink-2"
              >
                {testing === s.id ? "test…" : "Tester"}
              </button>
              <Toggle on={s.enabled} onClick={() => toggle(s)} />
            </div>
            {(s.url_param || s.env_params.length > 0) && (
              <div className="mt-2 flex flex-col gap-1.5">
                {s.url_param && (
                  <input
                    defaultValue={s.params.command ?? ""}
                    onBlur={(e) => setParam(s, "command", e.target.value)}
                    placeholder="commande stdio (ex. npx -y mon-mcp) ou URL"
                    className="border-2 border-line bg-card px-2 py-1 text-[12px] text-ink"
                  />
                )}
                {s.env_params.map((k) => (
                  <input
                    key={k}
                    defaultValue={s.params[k] ?? ""}
                    onBlur={(e) => setParam(s, k, e.target.value)}
                    placeholder={k}
                    className="border-2 border-line bg-card px-2 py-1 text-[12px] text-ink"
                  />
                ))}
              </div>
            )}
            {s.error && (
              <div className="mt-1.5 text-[12px] text-warn">{s.error}</div>
            )}
            {testResult[s.id] && (
              <div className="mt-1.5 text-[12px] text-muted-2">
                {testResult[s.id]}
              </div>
            )}
          </div>
        ))}
      </div>
    </Card>
  );
}
```

Rendre `<McpCard />` dans le JSX principal de `SettingsView`, juste avant `<HardwareCard />` (chercher `<HardwareCard` dans le fichier). Vérifier la signature du composant `Card` local (ligne ~721) : s'il n'a pas de prop `subtitle`, adapter l'appel au format réel du composant.

- [ ] **Step 3: Vérifier**

Run: `cd frontend && npx tsc -b`
Expected: aucune erreur. Puis `npm run dev` + backend : la carte MCP liste 5 serveurs, toggle persiste au rechargement, « Tester » sur fetch affiche outils ou erreur propre.

- [ ] **Step 4: Commit**

```bash
git add frontend/src/api/client.ts frontend/src/panels/SettingsView.tsx
git commit -m "feat(mcp): onglet Configuration — catalogue, toggles, test de connexion"
```

---

### Task 6: Skills — bibliothèque + routeur lexical + injection

**Files:**
- Create: `backend/skills/debogage-systematique.md`, `creation-web.md`, `refactor-sur.md`, `analyse-donnees.md`, `redaction-structuree.md`
- Create: `backend/app/skills.py`
- Create: `backend/tests/test_skills.py`
- Modify: `backend/app/agent_config.py` (champ `skills_enabled`)
- Modify: `backend/app/routes/chat.py` (injection + notice)

**Interfaces:**
- Produces: `skills.pick_skill(message: str) -> dict | None` avec clés `{name, title, body}` ; config `skills_enabled: bool` (défaut True) dans `DEFAULT_CONFIG`/`PROFILE_FIELDS`.

- [ ] **Step 1: Les 5 fichiers de skills**

Format commun — frontmatter simple `clé: valeur` puis corps. Exemple complet, `backend/skills/debogage-systematique.md` :

```markdown
---
name: debogage-systematique
title: Débogage systématique
keywords: bug, plante, erreur, exception, traceback, crash, échoue, marche pas, fonctionne pas, cassé, debug, débogue, corrige le bug, ne s'affiche pas, undefined, null, NaN
---
Méthode de débogage à suivre STRICTEMENT, étape par étape :
1. REPRODUIRE : identifie l'entrée exacte et le comportement observé vs attendu. Si le message d'erreur est fourni, cite-le et pars de là.
2. LOCALISER : lis le code concerné (read_file / grep_search) AVANT toute modification. Trouve la ligne qui produit le symptôme.
3. HYPOTHÈSE : formule UNE cause précise. Vérifie-la en lisant le code, pas en devinant.
4. CORRIGER : modification minimale et ciblée (edit_file). Ne réécris pas tout le fichier. Ne corrige qu'une cause à la fois.
5. VÉRIFIER : relis le code modifié (run_check) et explique pourquoi le symptôme disparaît. Signale tout autre problème repéré sans le corriger.
Interdit : proposer une correction sans avoir lu le code ; corriger plusieurs choses à la fois ; conclure « ça devrait marcher » sans vérification.
```

`creation-web.md` (keywords : `site, page, html, css, landing, formulaire, portfolio, interface, responsive, boutique, vitrine, menu, header, footer, animation`) — corps : 1. STRUCTURE (HTML sémantique complet d'abord) ; 2. STYLE (CSS cohérent, palette limitée, responsive mobile-first) ; 3. INTERACTIVITÉ (JS minimal, sans dépendance externe) ; 4. VÉRIFIER (run_check + liens internes ; si le navigateur MCP est disponible, ouvrir la page et lire la console). Interdit : livrer sans vérification ; images externes non demandées.

`refactor-sur.md` (keywords : `refactor, refactorise, réorganise, nettoie, simplifie, renomme, découpe, extrait, duplication, dette`) — corps : 1. COMPRENDRE (lire tout le code concerné + ses usages via grep_search) ; 2. PETITS PAS (une transformation à la fois, comportement identique) ; 3. VÉRIFIER après chaque pas (run_check) ; 4. RÉCAPITULER les changements. Interdit : changer le comportement ; renommer sans vérifier tous les usages.

`analyse-donnees.md` (keywords : `csv, json, données, tableau, statistique, moyenne, analyse, colonnes, tri, filtre, graphique, export`) — corps : 1. EXAMINER le format réel (read_file sur un échantillon) ; 2. VALIDER (types, valeurs manquantes, incohérences — les signaler) ; 3. TRANSFORMER (script clair, borné) ; 4. PRÉSENTER (résumé chiffré + limites de l'analyse). Interdit : supposer le format sans l'avoir lu.

`redaction-structuree.md` (keywords : `rédige, écris un texte, article, documentation, readme, rapport, résumé, lettre, mail, présentation, plan`) — corps : 1. PLAN (annonce la structure avant de rédiger) ; 2. RÉDACTION (paragraphes courts, un point par paragraphe, français précis) ; 3. RELECTURE (cohérence, répétitions, longueur adaptée à la demande). Interdit : remplissage, généralités.

Chaque fichier suit EXACTEMENT le même format frontmatter que l'exemple.

- [ ] **Step 2: Test qui échoue**

`backend/tests/test_skills.py` :

```python
import os
import tempfile

os.environ.setdefault("DATA_DIR", tempfile.mkdtemp())
os.environ.setdefault("WORKSPACE_DIR", tempfile.mkdtemp())

from app import skills  # noqa: E402


def test_five_skills_loaded():
    assert len(skills.ALL_SKILLS) == 5
    assert all(s["title"] and s["body"] for s in skills.ALL_SKILLS.values())


def test_bug_message_picks_debug():
    s = skills.pick_skill("mon script plante avec une erreur TypeError au démarrage")
    assert s and s["name"] == "debogage-systematique"


def test_web_message_picks_web():
    s = skills.pick_skill("crée une page html responsive pour ma boutique")
    assert s and s["name"] == "creation-web"


def test_banal_message_picks_nothing():
    assert skills.pick_skill("bonjour, quelle heure est-il ?") is None
```

Run: `cd backend && python -m pytest tests/test_skills.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'app.skills'`

- [ ] **Step 3: Implémenter skills.py**

```python
"""Skills : méthodes expertes injectées automatiquement selon la tâche.

Sélection lexicale instantanée (aucun appel LLM) : au plus UNE skill par
message, injectée en message système pour ce tour uniquement.
"""
from __future__ import annotations

import os
import re

_SKILLS_DIR = os.path.join(os.path.dirname(__file__), "..", "skills")

# Seuil : nombre minimal de mots-clés distincts trouvés dans le message.
_MIN_HITS = 2


def _load() -> dict[str, dict]:
    out: dict[str, dict] = {}
    if not os.path.isdir(_SKILLS_DIR):
        return out
    for fname in sorted(os.listdir(_SKILLS_DIR)):
        if not fname.endswith(".md"):
            continue
        with open(os.path.join(_SKILLS_DIR, fname), encoding="utf-8") as f:
            raw = f.read()
        m = re.match(r"^---\n(.*?)\n---\n(.*)$", raw, re.S)
        if not m:
            continue
        meta: dict[str, str] = {}
        for line in m.group(1).splitlines():
            if ":" in line:
                key, _, value = line.partition(":")
                meta[key.strip()] = value.strip()
        keywords = [k.strip().lower() for k in meta.get("keywords", "").split(",") if k.strip()]
        out[meta.get("name", fname[:-3])] = {
            "name": meta.get("name", fname[:-3]),
            "title": meta.get("title", fname[:-3]),
            "keywords": keywords,
            "body": m.group(2).strip(),
        }
    return out


ALL_SKILLS: dict[str, dict] = _load()


def pick_skill(message: str) -> dict | None:
    """Meilleure skill pour ce message, ou None si rien d'assez net."""
    low = message.lower()
    best, best_hits = None, 0
    for skill in ALL_SKILLS.values():
        hits = sum(1 for kw in skill["keywords"] if kw in low)
        if hits > best_hits:
            best, best_hits = skill, hits
    if best is None or best_hits < _MIN_HITS:
        return None
    return {"name": best["name"], "title": best["title"], "body": best["body"]}
```

Run: `cd backend && python -m pytest tests/test_skills.py -v` — 4 PASS.
Si `test_bug_message_picks_debug` échoue à cause du seuil, enrichir les
keywords du fichier concerné (pas le code).

- [ ] **Step 4: Config + injection dans chat.py**

`backend/app/agent_config.py` : dans `DEFAULT_CONFIG`, après `"rag_enabled": True,` ajouter :

```python
    # Skills : méthodes expertes injectées automatiquement selon la tâche.
    "skills_enabled": True,
```

et ajouter `"skills_enabled",` dans `PROFILE_FIELDS`.

`backend/app/routes/chat.py` : import `from .. import skills` (ligne des imports `from .. import agent_config, coder, …` — l'y ajouter). Dans `event_stream()`, juste APRÈS le bloc `if memories:` et AVANT `if plan:` :

```python
        # Skill : méthode experte injectée pour ce tour (jamais persistée).
        if cfg.get("skills_enabled", True):
            skill = skills.pick_skill(req.content)
            if skill:
                convo.insert(1, {
                    "role": "system",
                    "content": f"Méthode à suivre pour cette tâche :\n{skill['body']}",
                })
                yield _sse("notice", {"message": f"📘 Méthode : {skill['title']}"})
```

(Le badge réutilise le canal `notice` existant — zéro changement frontend.)

- [ ] **Step 5: Vérifier + commit**

```bash
cd backend && python -m pytest tests/ -v && python -m compileall -q app
git add backend/skills backend/app/skills.py backend/app/agent_config.py backend/app/routes/chat.py backend/tests/test_skills.py
git commit -m "feat(skills): 5 méthodes expertes auto-injectées (routeur lexical)"
```

---

### Task 7: run_check + vérification auto après écriture

**Files:**
- Modify: `backend/app/tools.py` (fonction `run_check` + registre + définition)
- Modify: `backend/app/agent.py` (auto-check après write_file/edit_file)
- Create: `backend/tests/test_run_check.py`

**Interfaces:**
- Consumes: `_safe_path`, `check_html`, `ToolError` (existants dans tools.py).
- Produces: `run_check(path: str) -> dict` — `{ok, issues: list[str], summary, _status}` ; enregistré dans `TOOL_IMPL` + `TOOL_DEFINITIONS` + `AVAILABLE_TOOLS`/`DEFAULT_TOOL_STATE` (agent_config).

- [ ] **Step 1: Test qui échoue**

`backend/tests/test_run_check.py` :

```python
import os
import tempfile

os.environ.setdefault("DATA_DIR", tempfile.mkdtemp())
_WS = tempfile.mkdtemp()
os.environ.setdefault("WORKSPACE_DIR", _WS)

from app import tools  # noqa: E402


def _write(name: str, content: str) -> str:
    path = os.path.join(_WS, name)
    with open(path, "w", encoding="utf-8") as f:
        f.write(content)
    return name


def test_python_valide():
    rel = _write("ok.py", "x = 1\nprint(x)\n")
    assert tools.run_check(rel)["ok"] is True


def test_python_casse():
    rel = _write("ko.py", "def broken(:\n")
    result = tools.run_check(rel)
    assert result["ok"] is False
    assert result["issues"]


def test_json_casse():
    rel = _write("ko.json", "{invalid")
    assert tools.run_check(rel)["ok"] is False


def test_fichier_inconnu_type():
    rel = _write("notes.txt", "bonjour")
    result = tools.run_check(rel)
    assert result["ok"] is True  # type non vérifiable = pas d'erreur
```

Run: `cd backend && python -m pytest tests/test_run_check.py -v`
Expected: FAIL — `AttributeError: module 'app.tools' has no attribute 'run_check'`

- [ ] **Step 2: Implémenter run_check dans tools.py**

Ajouter (avant le registre `TOOL_IMPL`) :

```python
def run_check(path: str) -> dict:
    """Vérification STATIQUE d'un fichier de code — n'exécute jamais rien.

    .py -> py_compile ; .js/.mjs -> node --check (si node présent) ;
    .html -> check_html ; .json -> parse. Autres types : ok sans contrôle.
    """
    target = _safe_path(path)
    if not os.path.isfile(target):
        raise ToolError(f"fichier introuvable : {path}")
    ext = os.path.splitext(target)[1].lower()
    issues: list[str] = []

    if ext == ".py":
        import py_compile
        try:
            py_compile.compile(target, doraise=True)
        except py_compile.PyCompileError as exc:
            issues.append(str(exc.msg)[:500])
    elif ext in (".js", ".mjs"):
        node = shutil.which("node")
        if node:
            proc = subprocess.run(
                [node, "--check", target], capture_output=True, text=True,
                timeout=15,
            )
            if proc.returncode != 0:
                issues.append((proc.stderr or proc.stdout)[:500])
    elif ext in (".html", ".htm"):
        issues.extend(check_html(target))
    elif ext == ".json":
        try:
            with open(target, encoding="utf-8") as f:
                json.load(f)
        except json.JSONDecodeError as exc:
            issues.append(f"JSON invalide : {exc}")

    ok = not issues
    return {
        "ok": ok,
        "issues": issues,
        "summary": "aucun problème" if ok else f"{len(issues)} problème(s)",
        "_status": "ok" if ok else "error",
    }
```

Vérifier que `shutil`, `subprocess`, `json`, `os` sont importés en tête de
tools.py (ajouter les manquants). Enregistrer :
- `TOOL_IMPL["run_check"] = run_check`
- Ajouter à `TOOL_DEFINITIONS` :

```python
    {
        "type": "function",
        "function": {
            "name": "run_check",
            "description": (
                "Vérifier statiquement un fichier de code du workspace "
                "(syntaxe Python/JS/JSON, structure HTML). N'exécute rien."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "Chemin relatif au workspace"}
                },
                "required": ["path"],
            },
        },
    },
```

`backend/app/agent_config.py` : ajouter `"run_check"` dans `AVAILABLE_TOOLS`
(après `grep_search`) et `"run_check": True,` dans `DEFAULT_TOOL_STATE`.
Dans `backend/app/routes/chat.py`, ajouter `"run_check"` à `_READONLY_TOOLS`
(lecture seule : la vérification n'écrit rien).

- [ ] **Step 3: Auto-check après écriture (agent.py)**

Dans la boucle outils de `run_agent`, à la fin du bloc générique (après le
`convo.append({...})` du résultat de `run_tool`), insérer :

```python
                # Vérification statique automatique après toute écriture de
                # code : l'erreur revient au modèle dans le même tour
                # (1 passe de correction max, bornée par MAX_ITERATIONS).
                if (
                    name in ("write_file", "edit_file")
                    and status == "ok"
                    and str(args.get("path", "")).lower().endswith(
                        (".py", ".js", ".mjs", ".html", ".htm", ".json")
                    )
                ):
                    try:
                        from .tools import run_check
                        check = run_check(args["path"])
                    except ToolError:
                        check = None
                    if check and not check["ok"]:
                        record = {
                            "name": "run_check",
                            "args": {"path": args["path"]},
                            "summary": check["summary"],
                            "status": "error",
                        }
                        collected.append(record)
                        yield {"type": "tool_result", **record}
                        convo.append({
                            "role": "tool",
                            "tool_name": "run_check",
                            "content": json.dumps(check, ensure_ascii=False),
                        })
```

- [ ] **Step 4: Vérifier + commit**

```bash
cd backend && python -m pytest tests/ -v && python -m compileall -q app
git add backend/app/tools.py backend/app/agent.py backend/app/agent_config.py backend/app/routes/chat.py backend/tests/test_run_check.py
git commit -m "feat(agent): run_check statique + vérification auto après écriture"
```

---

### Task 8: Toggle skills dans l'UI + Dockerfile + README

**Files:**
- Modify: `frontend/src/api/client.ts` (champ `skills_enabled` du type `AgentConfig`)
- Modify: `frontend/src/panels/SettingsView.tsx` (toggle dans la carte Intelligence)
- Modify: `Dockerfile` (Node.js + mcp-server-fetch)
- Modify: `README.md`

**Interfaces:**
- Consumes: config `skills_enabled` (Task 6), composant `Toggle` existant.

- [ ] **Step 1: Type + toggle**

`client.ts` : dans l'interface `AgentConfig`, ajouter `skills_enabled: boolean;`
(à côté de `rag_enabled`).

`SettingsView.tsx` : localiser la carte Intelligence (chercher `self_review`
dans le fichier) et ajouter, sur le modèle EXACT de la ligne du toggle
`rag_enabled` qui s'y trouve, une ligne :
libellé **« Skills automatiques »**, sous-titre
*« Méthodes expertes injectées selon la tâche (débogage, web, refactor…) »*,
`<Toggle on={draft.skills_enabled} onClick={() => set("skills_enabled", !draft.skills_enabled)} />`.

- [ ] **Step 2: Dockerfile**

Dans le stage backend du `Dockerfile` (celui qui installe
`requirements.txt`), ajouter Node.js AVANT le pip install (adapter à l'image
de base réelle — vérifier `FROM` en tête de fichier) :

```dockerfile
# Node.js pour les serveurs MCP lancés via npx (Playwright, Context7…).
RUN apt-get update && apt-get install -y --no-install-recommends nodejs npm \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir mcp-server-fetch
```

(Si l'image de base est alpine : `apk add --no-cache nodejs npm` à la place.)

- [ ] **Step 3: README**

Ajouter une section après « Intelligence augmentée » :

```markdown
## Serveurs MCP (outils professionnels)

Configuration → **Serveurs MCP** : catalogue préconfiguré, tout désactivé par
défaut (un serveur inactif ne coûte rien — aucun process, aucun outil dans le
prompt du modèle).

| Serveur | Apport |
| ------- | ------ |
| Playwright | l'agent pilote un vrai navigateur : teste ses pages, lit la console |
| Context7 | documentation à jour de n'importe quelle librairie |
| Fetch | lecture propre d'URL (markdown) |
| SearxNG | vraie recherche web (URL d'instance requise) |
| Personnalisé | n'importe quel serveur MCP (commande stdio ou URL) |

Chaque carte a un bouton **Tester** (connexion d'essai + liste des outils
découverts). Un serveur en panne n'interrompt jamais le chat : notice dans le
fil, nouvelle tentative au message suivant.

## Skills automatiques

Cinq méthodes expertes (débogage systématique, création web, refactor sûr,
analyse de données, rédaction structurée) sont injectées automatiquement selon
la tâche détectée — sélection lexicale instantanée, une seule à la fois, badge
« 📘 Méthode : … » dans le fil. Désactivable dans Configuration → Intelligence.
```

- [ ] **Step 4: Vérifier + commit**

```bash
cd frontend && npx tsc -b && npm run build
cd ../backend && python -m pytest tests/ -q
git add frontend/src/api/client.ts frontend/src/panels/SettingsView.tsx Dockerfile README.md
git commit -m "feat: toggle skills UI + Node.js dans l'image + docs MCP/skills"
```

---

### Task 9: Vérification bout-en-bout

**Files:** aucun (vérification).

- [ ] **Step 1: Suite complète**

```bash
cd backend && python -m pytest tests/ -v && python -m compileall -q app
cd ../frontend && npx tsc -b && npm run build
```

Expected: tout PASS, build OK.

- [ ] **Step 2: Scénarios manuels (backend + frontend + Ollama lancés)**

1. **MCP zéro coût** : aucun serveur actif → envoyer un message → dans les
   logs backend, aucun process MCP lancé ; latence premier token inchangée.
2. **Fetch** : activer Fetch (Tester → ✓) → « lis https://example.com et
   résume » → ToolCard `mcp_fetch_fetch` + résumé.
3. **Panne non bloquante** : serveur Personnalisé avec commande invalide
   (`npx paquet-inexistant`), activer → envoyer un message → notice
   « Serveur MCP … indisponible », réponse normale du modèle.
4. **Skills** : « mon script plante avec TypeError » → notice
   « 📘 Méthode : Débogage systématique » ; « bonjour » → aucune notice.
5. **run_check auto** : « écris hello.py qui affiche bonjour » →
   si le modèle produit une erreur de syntaxe, carte `run_check` en erreur
   puis correction dans le même tour.
6. **Playwright** (nécessite npx) : activer → « crée une page démo puis
   vérifie-la dans le navigateur » → outils browser_* appelés.

- [ ] **Step 3: Commit final + push**

```bash
git push
```

(Le push déclenche le build GHCR — vérifier ensuite l'image sur Unraid.)
