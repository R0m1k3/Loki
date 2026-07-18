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
