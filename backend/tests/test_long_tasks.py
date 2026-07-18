import os
import tempfile

os.environ.setdefault("DATA_DIR", tempfile.mkdtemp())
os.environ.setdefault("WORKSPACE_DIR", tempfile.mkdtemp())

import pytest  # noqa: E402

from app import agent, db, mcp_client  # noqa: E402

db.init_db()


def test_prune_compacte_les_anciens_resultats():
    big = "x" * 5000
    convo = [
        {"role": "system", "content": "consigne"},
        {"role": "user", "content": "tâche"},
        {"role": "assistant", "content": "", "tool_calls": [{}]},
        {"role": "tool", "tool_name": "read_file", "content": big},
        {"role": "assistant", "content": "", "tool_calls": [{}]},
        {"role": "tool", "tool_name": "grep_search", "content": big},
    ]
    # Le dernier lot (index 5) commence à 5 : seul l'index 3 est compacté.
    agent._prune_old_tool_results(convo, before_index=5)
    assert len(convo[3]["content"]) < 500
    assert "archivé" in convo[3]["content"]
    assert convo[5]["content"] == big  # lot courant intact
    assert convo[0]["content"] == "consigne"  # système intact


def test_prune_ignore_les_petits_resultats():
    convo = [{"role": "tool", "tool_name": "list_dir", "content": "court"}]
    agent._prune_old_tool_results(convo, before_index=1)
    assert convo[0]["content"] == "court"


@pytest.mark.asyncio
async def test_searxng_sans_url_erreur_claire():
    mgr = mcp_client.McpManager()
    try:
        result = await mgr.test_server("searxng")
        assert result["ok"] is False
        assert "SEARXNG_URL" in (result["error"] or "")
        assert "requis" in (result["error"] or "")
    finally:
        await mgr.aclose()


def test_migration_v6_monte_les_anciens_defauts():
    from app import agent_config
    profiles = db.get_config_value(agent_config.MODEL_PROFILES_KEY) or {}
    profiles["testmodel:7b"] = {"num_ctx": 8192}
    profiles["custom:7b"] = {"num_ctx": 5000}
    db.set_config_value(agent_config.MODEL_PROFILES_KEY, profiles)
    db.set_config_value(agent_config.PROFILE_STATE_KEY, {"version": 5})

    agent_config._migrate_profiles()

    migrated = db.get_config_value(agent_config.MODEL_PROFILES_KEY)
    assert migrated["testmodel:7b"]["num_ctx"] == 16384  # ancien défaut monté
    assert migrated["custom:7b"]["num_ctx"] == 5000  # valeur perso respectée
