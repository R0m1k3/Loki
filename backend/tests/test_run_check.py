import os
import tempfile

os.environ.setdefault("DATA_DIR", tempfile.mkdtemp())
os.environ.setdefault("WORKSPACE_DIR", tempfile.mkdtemp())

from app import tools  # noqa: E402
from app.config import settings  # noqa: E402

# Le workspace effectif peut avoir été fixé par un autre fichier de test
# importé avant celui-ci : on écrit là où les outils lisent réellement.
_WS = os.path.abspath(settings.workspace_dir)
os.makedirs(_WS, exist_ok=True)


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
