import os
import tempfile

os.environ.setdefault("DATA_DIR", tempfile.mkdtemp())
os.environ.setdefault("WORKSPACE_DIR", tempfile.mkdtemp())

import pytest  # noqa: E402

from app import agent  # noqa: E402


def _convo():
    return [
        {"role": "system", "content": "consigne"},
        {"role": "user", "content": "fais la tâche"},
    ]


@pytest.mark.asyncio
async def test_relance_apres_reflexion_seule(monkeypatch):
    """1er appel : réflexion seule -> relance ; 2e appel : réponse finale."""
    calls: list[list[dict]] = []

    async def fake_chat(model, convo, **kwargs):
        calls.append([dict(m) for m in convo])
        if len(calls) == 1:
            yield {"message": {"thinking": "hmm, je réfléchis longuement…"},
                   "done": False}
            yield {"message": {}, "done": True}
        else:
            yield {"message": {"content": "réponse finale"}, "done": True}

    monkeypatch.setattr(agent.ollama, "chat", fake_chat)
    events = [
        e async for e in agent.run_agent("test", _convo(), enabled_tools=[])
    ]

    final = [e for e in events if e["type"] == "final"]
    assert final and "réponse finale" in final[0]["content"]
    # La relance a bien injecté la consigne de reprise.
    assert any(
        "Continue la tâche" in m["content"]
        for m in calls[1] if m["role"] == "user"
    )


@pytest.mark.asyncio
async def test_reflexion_pas_renvoyee_au_modele(monkeypatch):
    """La pensée est affichée mais jamais réinjectée dans l'historique."""
    calls: list[list[dict]] = []

    async def fake_chat(model, convo, **kwargs):
        calls.append([dict(m) for m in convo])
        if len(calls) == 1:
            yield {"message": {"thinking": "je planifie",
                               "content": "étape 1",
                               "tool_calls": [{"function": {
                                   "name": "list_dir",
                                   "arguments": {"path": "."}}}]},
                   "done": True}
        else:
            yield {"message": {"content": "terminé"}, "done": True}

    monkeypatch.setattr(agent.ollama, "chat", fake_chat)
    events = [
        e async for e in agent.run_agent("test", _convo(), enabled_tools=None)
    ]

    final = [e for e in events if e["type"] == "final"]
    assert final and "terminé" in final[0]["content"]
    assert "je planifie" in final[0]["thinking"]  # gardée pour l'UI
    # Aucun message assistant réinjecté ne contient la clé thinking.
    assistant_turns = [m for m in calls[1] if m["role"] == "assistant"]
    assert assistant_turns
    assert all("thinking" not in m for m in assistant_turns)


@pytest.mark.asyncio
async def test_coupe_circuit_pensee_interminable(monkeypatch):
    """Pensée sans fin -> génération coupée en vol, puis relance qui aboutit."""
    calls: list[int] = []

    async def fake_chat(model, convo, **kwargs):
        calls.append(1)
        if len(calls) == 1:
            # Flux de pensée « infini » : jamais de done, jamais de contenu.
            for _ in range(10_000):
                yield {"message": {"thinking": "x" * 200}, "done": False}
        else:
            yield {"message": {"content": "réponse après coupe"}, "done": True}

    monkeypatch.setattr(agent.ollama, "chat", fake_chat)
    events = [
        e async for e in agent.run_agent("test", _convo(), enabled_tools=[])
    ]

    final = [e for e in events if e["type"] == "final"]
    assert final and "réponse après coupe" in final[0]["content"]
    assert len(calls) == 2  # coupé puis relancé, pas d'épuisement du flux


@pytest.mark.asyncio
async def test_reflexion_coupee_apres_deux_impasses(monkeypatch):
    """Deux itérations de pensée pure -> think désactivé, tâche finie."""
    seen_think: list = []

    async def fake_chat(model, convo, think=None, **kwargs):
        seen_think.append(think)
        if len(seen_think) <= 2:
            yield {"message": {"thinking": "boucle de pensée"}, "done": True}
        else:
            yield {"message": {"content": "fini sans réfléchir"}, "done": True}

    monkeypatch.setattr(agent.ollama, "chat", fake_chat)
    events = [
        e async for e in agent.run_agent(
            "test", _convo(), enabled_tools=[], think=True
        )
    ]

    final = [e for e in events if e["type"] == "final"]
    assert final and "fini sans réfléchir" in final[0]["content"]
    assert seen_think[-1] is False  # think coupé pour l'appel final
    assert any(e["type"] == "notice" for e in events)
