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
