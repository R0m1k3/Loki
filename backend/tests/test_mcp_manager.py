import os
import sys
import tempfile
import textwrap

os.environ.setdefault("DATA_DIR", tempfile.mkdtemp())
os.environ.setdefault("WORKSPACE_DIR", tempfile.mkdtemp())

import pytest  # noqa: E402

from app import db, mcp_client  # noqa: E402

db.init_db()

# Serveur MCP minimal : un outil "echo" + un outil au nom À TIRETS
# (comme Context7 « resolve-library-id »).
_FAKE_SERVER = textwrap.dedent("""
    from mcp.server.fastmcp import FastMCP
    mcp = FastMCP("fake")

    @mcp.tool()
    def echo(text: str) -> str:
        \"\"\"Répète le texte fourni.\"\"\"
        return "echo:" + text

    @mcp.tool(name="dash-tool-name")
    def dash_tool(text: str) -> str:
        \"\"\"Outil au nom à tirets.\"\"\"
        return "dash:" + text

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
        # Nom à tirets exposé assaini (compatibilité function-calling).
        assert "mcp_fake_dash_tool_name" in names
        result = await mgr.call_tool("mcp_fake_echo", {"text": "bonjour"})
        assert result["ok"] is True
        assert "echo:bonjour" in result["content"]
        # Appel via le nom assaini -> résolu vers le vrai nom à tirets.
        dash = await mgr.call_tool("mcp_fake_dash_tool_name", {"text": "x"})
        assert dash["ok"] is True and "dash:x" in dash["content"]
        # Tolérance : le modèle répond avec des tirets au lieu d'underscores.
        mixed = await mgr.call_tool("mcp_fake_dash-tool-name", {"text": "y"})
        assert mixed["ok"] is True and "dash:y" in mixed["content"]
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
