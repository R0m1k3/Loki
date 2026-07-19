import os
import tempfile

os.environ.setdefault("DATA_DIR", tempfile.mkdtemp())
os.environ.setdefault("WORKSPACE_DIR", tempfile.mkdtemp())

from fastapi.testclient import TestClient  # noqa: E402

from app.main import app  # noqa: E402
from app.config import settings  # noqa: E402

client = TestClient(app)
_ROOT = os.path.abspath(settings.workspace_dir)


def test_creation_et_liste():
    r = client.post("/api/projects", json={"name": "routedemo"})
    assert r.status_code == 201
    assert os.path.isdir(os.path.join(_ROOT, "routedemo", ".git"))
    names = [p["name"] for p in client.get("/api/projects").json()["projects"]]
    assert "routedemo" in names


def test_nom_invalide_400():
    assert client.post("/api/projects", json={"name": "../x"}).status_code == 400
    assert client.post("/api/projects", json={"name": "Demo"}).status_code == 400


def test_existant_400():
    client.post("/api/projects", json={"name": "dup"})
    assert client.post("/api/projects", json={"name": "dup"}).status_code == 400


def test_files_re_racine():
    client.post("/api/projects", json={"name": "scoped"})
    with open(os.path.join(_ROOT, "scoped", "a.txt"), "w") as f:
        f.write("x")
    with open(os.path.join(_ROOT, "racine.txt"), "w") as f:
        f.write("y")
    tree = client.get("/api/files", params={"project": "scoped"}).json()["tree"]
    names = [n["name"] for n in tree]
    assert "a.txt" in names and "racine.txt" not in names


def test_files_projet_invalide_400():
    assert client.get("/api/files", params={"project": "../x"}).status_code == 400
