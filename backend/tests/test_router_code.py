import os
import tempfile

os.environ.setdefault("DATA_DIR", tempfile.mkdtemp())
os.environ.setdefault("WORKSPACE_DIR", tempfile.mkdtemp())

from app import router  # noqa: E402
from app.routes.chat import (  # noqa: E402
    _mentioned_files,
    _prev_was_code,
    _session_code_context,
    _workspace_listing,
)
from app.config import settings  # noqa: E402


def test_message_code_explicite():
    assert router.is_code_task("crée une page html avec un formulaire") is True


def test_message_banal_pas_code():
    assert router.is_code_task("quelle heure est-il ?") is False


def test_followup_courts():
    # Suites typiques d'un travail de code, sans vocabulaire code explicite.
    assert router.is_code_followup("ajoute un bouton rouge en haut") is True
    assert router.is_code_followup("continue") is True
    assert router.is_code_followup("enlève le menu et agrandis le titre") is True
    assert router.is_code_followup("merci beaucoup !") is False


def test_prev_was_code_via_engine():
    history = [
        {"role": "user", "content": "crée un site", "meta": None},
        {"role": "assistant", "content": "fait", "meta": {"engine": "code"}},
    ]
    assert _prev_was_code(history) is True


def test_prev_was_code_via_write_file():
    history = [
        {"role": "assistant", "content": "ok",
         "meta": {"tools": [{"name": "write_file", "args": {}}]}},
    ]
    assert _prev_was_code(history) is True


def test_prev_was_code_discussion():
    history = [
        {"role": "assistant", "content": "voici la réponse", "meta": None},
    ]
    assert _prev_was_code(history) is False


def test_mentioned_files_existants_seulement():
    root = os.path.abspath(settings.workspace_dir)
    os.makedirs(root, exist_ok=True)
    with open(os.path.join(root, "index.html"), "w", encoding="utf-8") as f:
        f.write("<html></html>")
    found = _mentioned_files("modifie index.html et style.css")
    assert found == ["index.html"]  # style.css n'existe pas


def test_session_code_context_recap_et_fichiers():
    root = os.path.abspath(settings.workspace_dir)
    os.makedirs(root, exist_ok=True)
    with open(os.path.join(root, "jeu.html"), "w", encoding="utf-8") as f:
        f.write("<html>jeu</html>")
    history = [
        {"role": "user", "content": "crée un jeu snake dans jeu.html",
         "meta": None},
        {"role": "assistant", "content": "fait",
         "meta": {"engine": "code",
                  "tools": [{"name": "write_file",
                             "args": {"path": "jeu.html"}}]}},
        {"role": "user", "content": "il y a des bugs", "meta": None},
    ]
    recap, files = _session_code_context(history)
    assert "crée un jeu snake" in recap
    assert files == ["jeu.html"]


def test_session_code_context_vide_sans_historique():
    recap, files = _session_code_context([])
    assert recap == ""
    assert files == []


def test_workspace_listing_contient_fichiers():
    root = os.path.abspath(settings.workspace_dir)
    os.makedirs(root, exist_ok=True)
    with open(os.path.join(root, "jeu.html"), "w", encoding="utf-8") as f:
        f.write("x")
    listing = _workspace_listing()
    assert "jeu.html" in listing
