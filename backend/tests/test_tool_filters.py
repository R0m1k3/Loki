import os
import tempfile

os.environ.setdefault("DATA_DIR", tempfile.mkdtemp())
os.environ.setdefault("WORKSPACE_DIR", tempfile.mkdtemp())

from app import tools  # noqa: E402
from app.config import settings  # noqa: E402

_WS = os.path.abspath(settings.workspace_dir)
os.makedirs(_WS, exist_ok=True)


# ── read_file fenêtré ────────────────────────────────────────────────────

def _write(name: str, content: str) -> str:
    with open(os.path.join(_WS, name), "w", encoding="utf-8") as f:
        f.write(content)
    return name


def test_read_file_petit_entier():
    rel = _write("petit.txt", "a\nb\nc\n")
    result = tools.read_file(rel)
    assert result["content"] == "a\nb\nc"
    assert "3 lignes" in result["summary"]


def test_read_file_gros_fenetre():
    rel = _write("gros.txt", "\n".join(f"ligne {i}" for i in range(1, 501)))
    result = tools.read_file(rel)
    assert "ligne 200" in result["content"]
    assert "ligne 201" not in result["content"].replace("start_line=201", "")
    assert "start_line=201" in result["content"]  # marche à suivre
    assert "1-200 sur 500" in result["summary"]


def test_read_file_fenetre_suivante():
    rel = _write("gros2.txt", "\n".join(f"ligne {i}" for i in range(1, 501)))
    result = tools.read_file(rel, start_line=201)
    assert "ligne 201" in result["content"]
    assert "201-400 sur 500" in result["summary"]


# ── sortie shell filtrée ─────────────────────────────────────────────────

def test_compact_succes_garde_la_fin():
    out = "\n".join(f"étape {i}" for i in range(1, 101))
    compact = tools._compact_output(out, 0)
    assert "étape 100" in compact
    assert "étape 1\n" not in compact
    assert "lignes omises" in compact


def test_compact_echec_garde_les_erreurs():
    out = "\n".join(
        ["compilation démarrée"]
        + [f"module {i} ok" for i in range(50)]
        + ["ERROR: variable x undefined", "build failed"]
    )
    compact = tools._compact_output(out, 1)
    assert "ERROR: variable x undefined" in compact
    assert "build failed" in compact
    assert "module 3 ok" not in compact


def test_dedupe_repetitions():
    lines = ["warn: deprecated"] * 5 + ["fin"]
    assert tools._dedupe_lines(lines) == ["warn: deprecated ×5", "fin"]
