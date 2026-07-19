import os
import tempfile

os.environ.setdefault("DATA_DIR", tempfile.mkdtemp())
os.environ.setdefault("WORKSPACE_DIR", tempfile.mkdtemp())

import pytest  # noqa: E402

from app import db, tools  # noqa: E402
from app.config import settings  # noqa: E402

db.init_db()
_ROOT = os.path.abspath(settings.workspace_dir)


@pytest.fixture(autouse=True)
def _reset_project():
    tools.set_project(None)
    yield
    tools.set_project(None)


def test_racine_par_defaut():
    assert tools.active_root() == _ROOT


def test_set_project_reracine():
    tools.set_project("demo")
    root = tools.active_root()
    assert root == os.path.join(_ROOT, "demo")
    assert os.path.isdir(root)  # créée à la volée


def test_confinement_conserve():
    tools.set_project("demo")
    with pytest.raises(tools.ToolError):
        tools._safe_path("../hors-projet")


def test_nom_projet_invalide():
    for bad in ("../x", "UPPER", "a b", "", "x" * 50):
        with pytest.raises(tools.ToolError):
            tools.set_project(bad)


def test_session_porte_son_projet():
    s = db.create_session("t", None, project="demo")
    assert db.get_session(s["id"])["project"] == "demo"
    db.set_session_project(s["id"], None)
    assert db.get_session(s["id"])["project"] is None


def test_aides_contexte_suivent_le_projet():
    from app.routes.chat import _mentioned_files, _workspace_listing
    tools.set_project("ctxdemo")
    root = tools.active_root()
    with open(os.path.join(root, "app.py"), "w", encoding="utf-8") as f:
        f.write("x = 1")
    assert "app.py" in _workspace_listing()
    assert _mentioned_files("corrige app.py") == ["app.py"]
    tools.set_project(None)
    assert _mentioned_files("corrige app.py") == []  # absent de la racine
