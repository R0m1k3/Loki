# Projets + aperçu réductible — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** l'agent travaille dans des projets (sous-dossiers du workspace) choisis par session depuis le composer, et le panneau d'aperçu se replie.

**Architecture:** une contextvar dans `tools.py` re-racine `_safe_path`/`_workspace_root` sur `workspace/<projet>` pour toute la requête (chat, outils, shell, Aider) ; les routes fichiers/git prennent un paramètre `project` optionnel et posent la même contextvar. La session porte son projet (colonne SQLite). Frontend : chip 📁 dans le composer, store re-racine les appels fichiers.

**Tech Stack:** FastAPI, SQLite (migrations douces ALTER TABLE), contextvars, React/zustand.

**Spec:** `docs/superpowers/specs/2026-07-19-projets-apercu-design.md`

## Global Constraints

- Nom de projet : `^[a-z0-9][a-z0-9_-]{0,40}$` — sinon 400.
- Session sans projet (NULL) = racine du workspace, comportement actuel intact.
- Confinement `_safe_path` inchangé dans sa logique (re-raciné seulement).
- Un dépôt git PAR projet (`ensure_git` à la création et au premier usage git).
- Projet disparu du disque → chat retombe sur la racine + notice SSE.
- UI française, styles existants. Tests : `python -m pytest` depuis `backend/`.
- Après chaque tâche frontend : `cd frontend && npx tsc -b` passe.

---

### Task 1: Racine active (contextvar) + colonne project + routes sessions

**Files:**
- Modify: `backend/app/tools.py` (contextvar + validation nom)
- Modify: `backend/app/db.py` (migration + create/set project)
- Modify: `backend/app/routes/sessions.py` (create + PATCH avec project)
- Test: `backend/tests/test_projects.py`

**Interfaces:**
- Produces: `tools.set_project(name: str | None) -> None` (valide le nom, pose la contextvar ; `ToolError` si nom invalide) ; `tools.active_root() -> str` (racine effective, créée si absente) ; `tools.PROJECT_NAME` (regex compilée) ; `db.create_session(title, model, project=None)` ; `db.set_session_project(sid, project: str | None)` ; PATCH `/api/sessions/{sid}` accepte `{title?, project?}` (`project: ""` = retour racine).

- [ ] **Step 1: Écrire les tests qui échouent**

`backend/tests/test_projects.py` :

```python
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
```

- [ ] **Step 2: Vérifier l'échec**

Run: `cd backend && python -m pytest tests/test_projects.py -q`
Expected: FAIL — `AttributeError: module 'app.tools' has no attribute 'set_project'`

- [ ] **Step 3: tools.py — contextvar**

En tête de `backend/app/tools.py` (après l'import `settings`) :

```python
from contextvars import ContextVar

# Projet actif pour la requête en cours : re-racine tous les outils sur
# workspace/<projet>. None = racine du workspace (comportement historique).
_ACTIVE_PROJECT: ContextVar[str | None] = ContextVar("loki_project", default=None)

PROJECT_NAME = re.compile(r"^[a-z0-9][a-z0-9_-]{0,40}$")


def set_project(name: str | None) -> None:
    """Fixe le projet actif de la requête (None = racine)."""
    if name is not None and not PROJECT_NAME.match(name):
        raise ToolError(f"nom de projet invalide : {name!r}")
    _ACTIVE_PROJECT.set(name)


def active_root() -> str:
    """Racine effective (workspace ou projet), créée si nécessaire."""
    return _workspace_root()
```

Remplacer `_workspace_root` :

```python
def _workspace_root() -> str:
    root = os.path.abspath(settings.workspace_dir)
    project = _ACTIVE_PROJECT.get()
    if project:
        root = os.path.join(root, project)
    os.makedirs(root, exist_ok=True)
    return root
```

(`_safe_path` ne change pas : il s'appuie sur `_workspace_root()`.)

- [ ] **Step 4: db.py — colonne + accesseurs**

Dans `init_db`, à côté de la migration `summary` :

```python
        if "project" not in scols:
            conn.execute("ALTER TABLE sessions ADD COLUMN project TEXT")
```

`create_session` — ajouter le paramètre et la colonne (adapter l'INSERT
existant) :

```python
def create_session(title: str, model: str | None, project: str | None = None) -> dict:
```

et inclure `project` dans l'INSERT et le dict renvoyé. Ajouter :

```python
def set_session_project(sid: str, project: str | None) -> None:
    with _LOCK, _connect() as conn:
        conn.execute(
            "UPDATE sessions SET project = ? WHERE id = ?", (project, sid)
        )
```

Vérifier que `list_sessions`/`get_session` renvoient la colonne (elles font
`dict(row)` — la colonne suit automatiquement).

- [ ] **Step 5: sessions.py — create + PATCH**

```python
class CreateSession(BaseModel):
    title: str = "Nouvelle session"
    model: str | None = None
    project: str | None = None


class UpdateSession(BaseModel):
    title: str | None = None
    project: str | None = None  # "" = retour à la racine du workspace
```

```python
@router.post("")
async def post_session(req: CreateSession) -> dict:
    if req.project and not tools.PROJECT_NAME.match(req.project):
        raise HTTPException(400, "nom de projet invalide")
    return db.create_session(req.title, req.model, req.project or None)


@router.patch("/{sid}")
async def patch_session(sid: str, req: UpdateSession) -> dict:
    if not db.get_session(sid):
        raise HTTPException(404, "session introuvable")
    if req.title is not None:
        db.rename_session(sid, req.title)
    if req.project is not None:
        project = req.project or None
        if project and not tools.PROJECT_NAME.match(project):
            raise HTTPException(400, "nom de projet invalide")
        db.set_session_project(sid, project)
    return {"ok": True}
```

(import : `from .. import db, tools`)

- [ ] **Step 6: Vérifier + commit**

Run: `cd backend && python -m pytest tests/ -q` — tout PASS.

```bash
git add backend/app/tools.py backend/app/db.py backend/app/routes/sessions.py backend/tests/test_projects.py
git commit -m "feat(projets): racine active par contextvar + projet par session"
```

---

### Task 2: Routes /api/projects + files/git re-racinés

**Files:**
- Create: `backend/app/routes/projects.py`
- Modify: `backend/app/routes/files.py` (param `project` sur les 4 routes)
- Modify: `backend/app/routes/git.py` (param `project` sur les 3 routes)
- Modify: `backend/app/main.py` (include projects.router)
- Test: `backend/tests/test_projects_routes.py`

**Interfaces:**
- Consumes: `tools.set_project`, `tools.active_root`, `tools.PROJECT_NAME`, `coder.ensure_git` (existant).
- Produces: `GET /api/projects` → `{projects: [{name, files}], root_files}` ; `POST /api/projects {name}` → `{name}` (201) ; toutes les routes files/git acceptent `?project=`.

- [ ] **Step 1: Tests qui échouent**

`backend/tests/test_projects_routes.py` :

```python
import os
import tempfile

os.environ.setdefault("DATA_DIR", tempfile.mkdtemp())
os.environ.setdefault("WORKSPACE_DIR", tempfile.mkdtemp())

from fastapi.testclient import TestClient  # noqa: E402

from app import tools  # noqa: E402
from app.main import app  # noqa: E402
from app.config import settings  # noqa: E402

client = TestClient(app)
_ROOT = os.path.abspath(settings.workspace_dir)


def test_creation_et_liste():
    r = client.post("/api/projects", json={"name": "demo"})
    assert r.status_code == 201
    assert os.path.isdir(os.path.join(_ROOT, "demo", ".git"))
    names = [p["name"] for p in client.get("/api/projects").json()["projects"]]
    assert "demo" in names


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
```

Run: `cd backend && python -m pytest tests/test_projects_routes.py -q`
Expected: FAIL (404 sur /api/projects).

Note : `TestClient` requiert `httpx` (déjà présent). Si `starlette` réclame
un extra, `python -m pip install "httpx<1"` est déjà satisfait.

- [ ] **Step 2: routes/projects.py**

```python
"""Routes des projets : sous-dossiers de premier niveau du workspace."""
from __future__ import annotations

import os

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

from .. import coder, tools
from ..config import settings

router = APIRouter(prefix="/api/projects", tags=["projects"])


def _root() -> str:
    root = os.path.abspath(settings.workspace_dir)
    os.makedirs(root, exist_ok=True)
    return root


def _count_files(path: str) -> int:
    total = 0
    for dirpath, dirnames, filenames in os.walk(path):
        dirnames[:] = [d for d in dirnames if not d.startswith(".")]
        total += sum(1 for f in filenames if not f.startswith("."))
    return total


@router.get("")
async def list_projects() -> dict:
    root = _root()
    projects = []
    root_files = 0
    for name in sorted(os.listdir(root)):
        full = os.path.join(root, name)
        if name.startswith("."):
            continue
        if os.path.isdir(full):
            projects.append({"name": name, "files": _count_files(full)})
        else:
            root_files += 1
    return {"projects": projects, "root_files": root_files}


class CreateProject(BaseModel):
    name: str


@router.post("", status_code=201)
async def create_project(req: CreateProject) -> dict:
    name = req.name.strip()
    if not tools.PROJECT_NAME.match(name):
        raise HTTPException(400, "nom de projet invalide (a-z, 0-9, - et _)")
    target = os.path.join(_root(), name)
    if os.path.exists(target):
        raise HTTPException(400, "ce projet existe déjà")
    os.makedirs(target)
    # Dépôt git par projet : commits Aider + onglet Git propres au projet.
    coder.ensure_git(target)
    return {"name": name}
```

`main.py` : ajouter `projects` à l'import des routes et
`app.include_router(projects.router)`.

- [ ] **Step 3: files.py — param project**

Chaque route gagne `project: str | None = None` et commence par :

```python
    try:
        tools_mod.set_project(project or None)
    except ToolError as exc:
        raise HTTPException(400, str(exc)) from exc
```

avec en tête du fichier `from .. import tools as tools_mod` (l'import existant
`from ..tools import ToolError, _safe_path` reste). Puis remplacer chaque
usage de `os.path.abspath(settings.workspace_dir)` par `tools_mod.active_root()` :
- `list_files` : `root = tools_mod.active_root()` (le `os.makedirs` est déjà
  fait par `active_root`).
- `_tree` : signature `_tree(path: str, root: str)` — le `rel` se calcule
  contre `root` passé en argument (adapter l'appel récursif et l'appel
  depuis `list_files`).
- `delete_file` : le refus de racine devient
  `if os.path.abspath(target) == tools_mod.active_root():`.
- `file_content` / `download_file` : seulement `set_project` en tête
  (`_safe_path` suit la contextvar).

IMPORTANT : remettre `tools_mod.set_project(None)` n'est pas nécessaire —
la contextvar est par-tâche asyncio, chaque requête FastAPI a son contexte.

- [ ] **Step 4: git.py — param project**

`_git` gagne `project: str | None = None` :

```python
def _git(*args: str, timeout: int = 15, project: str | None = None) -> subprocess.CompletedProcess:
    root = os.path.abspath(settings.workspace_dir)
    if project:
        if not tools.PROJECT_NAME.match(project):
            raise HTTPException(400, "nom de projet invalide")
        root = os.path.join(root, project)
    ensure_git(root)
    return subprocess.run(
        ["git", *args], cwd=root, ...
    )
```

(reprendre les kwargs existants de l'appel `subprocess.run` du fichier ;
imports : `from .. import tools`, `from fastapi import HTTPException` déjà
présent ou à ajouter, `import os`). Les trois routes (`git_log`, `git_diff`,
`git_revert`) gagnent `project: str | None = None` et le propagent à chaque
appel `_git(..., project=project)`.

- [ ] **Step 5: Vérifier + commit**

Run: `cd backend && python -m pytest tests/ -q` — tout PASS.

```bash
git add backend/app/routes/projects.py backend/app/routes/files.py backend/app/routes/git.py backend/app/main.py backend/tests/test_projects_routes.py
git commit -m "feat(projets): routes /api/projects + files et git re-racinés"
```

---

### Task 3: Chat re-raciné (contexte, Aider, notice projet disparu)

**Files:**
- Modify: `backend/app/routes/chat.py`
- Modify: `backend/app/coder.py` (`run_code_task` paramètre `root`)
- Modify: `backend/app/agent.py` (dispatch `code_task` passe la racine)
- Test: `backend/tests/test_projects.py` (ajout)

**Interfaces:**
- Consumes: `tools.set_project`, `tools.active_root` (Task 1).
- Produces: `coder.run_code_task(instruction, model, files=None, root: str | None = None)`.

- [ ] **Step 1: Test qui échoue (aides de contexte re-racinées)**

Ajouter à `backend/tests/test_projects.py` :

```python
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
```

Run: `cd backend && python -m pytest tests/test_projects.py -q`
Expected: FAIL — `_workspace_listing`/`_mentioned_files` lisent encore
`settings.workspace_dir`.

- [ ] **Step 2: chat.py — racine active partout**

Dans `chat()` juste après la récupération de la session :

```python
    # Projet de la session : re-racine outils, shell, Aider et aides de
    # contexte pour tout le tour. Projet disparu -> retour racine + notice.
    project = session.get("project") or None
    project_missing = False
    if project:
        proj_dir = os.path.join(os.path.abspath(settings.workspace_dir), project)
        if not os.path.isdir(proj_dir):
            project_missing, project = True, None
    tools.set_project(project)
```

avec `from .. import tools` dans les imports du fichier. Dans
`event_stream()`, après le `yield _sse("start", …)` :

```python
        if project_missing:
            yield _sse("notice", {"message":
                "Projet de la session introuvable sur le disque — retour au workspace."})
```

Remplacer dans `_session_code_context`, `_workspace_listing` et
`_mentioned_files` chaque `root = os.path.abspath(settings.workspace_dir)`
par `root = tools.active_root()`.

Chemin code : passer la racine à Aider —

```python
            async for chunk in _code_stream(
                req, code_model, extra=extra, plan=plan, files=code_files,
            ):
```

`_code_stream` → `_run_aider_keepalive(instruction, model, files)` →

```python
    task = asyncio.create_task(
        asyncio.to_thread(
            coder.run_code_task, instruction, model, files, tools.active_root()
        )
    )
```

(`active_root()` évalué AVANT le to_thread : la contextvar ne suit pas dans
le thread.)

- [ ] **Step 3: coder.py — paramètre root**

```python
def run_code_task(
    instruction: str,
    model: str,
    files: list[str] | None = None,
    root: str | None = None,
) -> dict:
```

et remplacer `root = os.path.abspath(settings.workspace_dir)` par
`root = os.path.abspath(root or settings.workspace_dir)` (le reste — ensure_git,
chdir, fnames — utilise déjà `root`).

- [ ] **Step 4: agent.py — code_task scoped**

Dans le dispatch `code_task` de `run_agent`, remplacer l'appel :

```python
                if name == "code_task":
                    from .tools import active_root
                    code_model = await coder.pick_code_model(model)
                    result = await asyncio.to_thread(
                        coder.run_code_task,
                        args.get("instruction", ""),
                        code_model,
                        args.get("files") or [],
                        active_root(),
                    )
```

- [ ] **Step 5: Vérifier + commit**

Run: `cd backend && python -m pytest tests/ -q` — tout PASS ;
`python -m compileall -q app`.

```bash
git add backend/app/routes/chat.py backend/app/coder.py backend/app/agent.py backend/tests/test_projects.py
git commit -m "feat(projets): chat, aides de contexte et Aider suivent le projet de la session"
```

---

### Task 4: Frontend — API client + store projets

**Files:**
- Modify: `frontend/src/api/client.ts`
- Modify: `frontend/src/store/useStore.ts`

**Interfaces:**
- Consumes: routes Tasks 1-2.
- Produces: `listProjects(): Promise<{projects: {name: string; files: number}[]; root_files: number}>` ; `createProject(name): Promise<void>` ; `setSessionProject(id, project: string | null): Promise<void>` ; store : `currentProject(): string | null` (getter), `setProject(name: string | null)`, `projects: {name: string; files: number}[]`, `refreshProjects()`.

- [ ] **Step 1: client.ts**

Type `Session` : ajouter `project?: string | null;`. Ajouter :

```typescript
export async function listProjects(): Promise<{
  projects: { name: string; files: number }[];
  root_files: number;
}> {
  const res = await fetch("/api/projects");
  return res.json();
}

export async function createProject(name: string): Promise<void> {
  const res = await fetch("/api/projects", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name }),
  });
  if (!res.ok) throw await apiError(res, "création du projet impossible");
}

export async function setSessionProject(
  id: string,
  project: string | null
): Promise<void> {
  const res = await fetch(`/api/sessions/${id}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ project: project ?? "" }),
  });
  if (!res.ok) throw await apiError(res, "changement de projet impossible");
}
```

Re-raciner les appels fichiers — signatures :

```typescript
const projQuery = (project?: string | null) =>
  project ? `&project=${encodeURIComponent(project)}` : "";

export async function listFiles(project?: string | null): Promise<FileNode[]> {
  const query = project ? `?project=${encodeURIComponent(project)}` : "";
  const res = await fetch(`/api/files${query}`);
  return (await res.json()).tree;
}

export async function fileContent(
  path: string,
  project?: string | null
): Promise<string> {
  const res = await fetch(
    `/api/files/content?path=${encodeURIComponent(path)}${projQuery(project)}`
  );
  if (!res.ok) return "";
  return (await res.json()).content;
}

export function downloadFile(path: string, project?: string | null): void {
  const link = document.createElement("a");
  link.href = `/api/files/download?path=${encodeURIComponent(path)}${projQuery(project)}`;
  link.download = path.split(/[\\/]/).pop() ?? "fichier";
  document.body.appendChild(link);
  link.click();
  link.remove();
}

export async function deleteFile(
  path: string,
  project?: string | null
): Promise<void> {
  const res = await fetch(
    `/api/files?path=${encodeURIComponent(path)}${projQuery(project)}`,
    { method: "DELETE" }
  );
  if (!res.ok) {
    throw await apiError(res, `suppression impossible (${res.status})`);
  }
}
```

Idem routes git utilisées par PreviewPanel (`getGitLog`, `getGitDiff`,
`revertCommit`) : paramètre optionnel `project` ajouté en query string de la
même façon.

`createSession` : paramètre `project?: string | null` transmis dans le body.

- [ ] **Step 2: useStore.ts**

État : `projects: { name: string; files: number }[]` (init `[]`). Actions :

```typescript
  refreshProjects: async () => {
    try {
      const { projects } = await listProjects();
      set({ projects });
    } catch { /* backend indisponible */ }
  },

  currentProject: () => {
    const s = get().sessions.find((x) => x.id === get().currentSessionId);
    return s?.project ?? null;
  },

  setProject: async (name) => {
    let sid = get().currentSessionId;
    if (!sid) {
      const s = await createSession(get().selectedModel || undefined, name);
      set({ currentSessionId: s.id, messages: [] });
      sid = s.id;
    } else {
      await setSessionProject(sid, name);
    }
    await get().refreshSessions();
    await get().refreshFiles();
    set({ previewPath: null, previewContent: "" });
  },
```

(`createSession(model?, project?)` : étendre la fonction client existante.)
`refreshFiles` passe le projet : `listFiles(get().currentProject())`.
`openPreview` : `fileContent(path, get().currentProject())`.
`removeFile` : `deleteFile(path, get().currentProject())`.
`newSession` : `createSession(model, get().currentProject())` — héritage.
`openSession` : après chargement, `await get().refreshFiles()` (l'arbre suit
la session ouverte).
Les composants qui appellent `downloadFile(path)` passent aussi
`useStore.getState().currentProject()` (LeftPanel, ActivityViews,
PreviewPanel).
Types : ajouter `projects`, `refreshProjects`, `currentProject`, `setProject`
à `LokiState`.

- [ ] **Step 3: Vérifier + commit**

Run: `cd frontend && npx tsc -b` — OK (les composants utilisant downloadFile
sont ajustés dans cette tâche pour compiler).

```bash
git add frontend/src/api/client.ts frontend/src/store/useStore.ts frontend/src/panels/LeftPanel.tsx frontend/src/panels/ActivityViews.tsx frontend/src/panels/PreviewPanel.tsx
git commit -m "feat(projets): client API + store re-racinés par projet"
```

---

### Task 5: Chip projet dans le composer + aperçu réductible

**Files:**
- Create: `frontend/src/components/ProjectChip.tsx`
- Modify: `frontend/src/panels/ChatPanel.tsx` (rendu du chip à côté du ModeSelector)
- Modify: `frontend/src/panels/PreviewPanel.tsx` (repli)

**Interfaces:**
- Consumes: store Task 4 (`projects`, `refreshProjects`, `currentProject`, `setProject`).

- [ ] **Step 1: ProjectChip.tsx**

```tsx
import { useEffect, useRef, useState } from "react";
import { useStore } from "../store/useStore";
import { createProject } from "../api/client";

/** Sélecteur de projet du composer : 📁 <projet> + menu (liste, création). */
export function ProjectChip() {
  const { projects, refreshProjects, currentProject, setProject } = useStore();
  const [open, setOpen] = useState(false);
  const [creating, setCreating] = useState(false);
  const [draft, setDraft] = useState("");
  const [error, setError] = useState<string | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);

  const active = currentProject();

  useEffect(() => {
    if (open) void refreshProjects();
  }, [open, refreshProjects]);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) {
        setOpen(false);
        setCreating(false);
        setError(null);
      }
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open]);

  const choose = async (name: string | null) => {
    setOpen(false);
    await setProject(name);
  };

  const create = async () => {
    const name = draft.trim();
    if (!name) return;
    try {
      await createProject(name);
      setCreating(false);
      setDraft("");
      await choose(name);
    } catch (err) {
      setError(err instanceof Error ? err.message : "création impossible");
    }
  };

  return (
    <div ref={rootRef} className="relative">
      <button
        onClick={() => setOpen((o) => !o)}
        className="flex h-8 items-center gap-1.5 border-2 border-line bg-base px-2.5 text-[12px] text-ink-2"
        title="Projet de travail de cette session"
      >
        📁 <span className="max-w-[140px] truncate">{active ?? "workspace"}</span>
      </button>

      {open && (
        <div
          className="absolute bottom-[calc(100%+6px)] left-0 z-30 w-[240px] border-[3px] border-line bg-card shadow-hard"
          style={{ borderRadius: 7 }}
        >
          <button
            onClick={() => choose(null)}
            className={`block w-full px-3 py-2 text-left text-[13px] text-ink-2 hover:bg-base ${
              active === null ? "bg-base font-bold" : ""
            }`}
          >
            workspace (racine)
          </button>
          {projects.map((p) => (
            <button
              key={p.name}
              onClick={() => choose(p.name)}
              className={`block w-full border-t-2 border-line-soft px-3 py-2 text-left text-[13px] text-ink-2 hover:bg-base ${
                active === p.name ? "bg-base font-bold" : ""
              }`}
            >
              📁 {p.name}
              <span className="ml-1.5 text-[11px] text-muted-2">
                {p.files} fichier{p.files > 1 ? "s" : ""}
              </span>
            </button>
          ))}
          <div className="border-t-2 border-line-soft p-2">
            {creating ? (
              <input
                autoFocus
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") void create();
                  if (e.key === "Escape") {
                    setCreating(false);
                    setError(null);
                  }
                }}
                placeholder="nom-du-projet"
                className="w-full border-2 border-line bg-base px-2 py-1 text-[12px] text-ink outline-none"
              />
            ) : (
              <button
                onClick={() => setCreating(true)}
                className="w-full text-left text-[13px] font-bold text-accent"
              >
                + Nouveau projet
              </button>
            )}
            {error && <div className="mt-1 text-[11px] text-warn">{error}</div>}
          </div>
        </div>
      )}
    </div>
  );
}
```

- [ ] **Step 2: ChatPanel — rendu**

Localiser le rendu du sélecteur de mode dans le composer (composant local
`ModeSelector`, utilisé vers le bas du composer). Ajouter `<ProjectChip />`
juste à côté (même conteneur flex), avec
`import { ProjectChip } from "../components/ProjectChip";`.

- [ ] **Step 3: PreviewPanel — repli**

Dans `PreviewPanel` :

```tsx
  const [collapsed, setCollapsed] = useState(
    () => window.localStorage.getItem("loki.preview.collapsed") === "1"
  );
  const toggleCollapsed = () => {
    setCollapsed((c) => {
      window.localStorage.setItem("loki.preview.collapsed", c ? "0" : "1");
      return !c;
    });
  };
```

Rendu replié — AVANT le rendu normal :

```tsx
  if (collapsed) {
    return (
      <div className="flex w-9 flex-none flex-col items-center border-l-[3px] border-line bg-panel pt-3">
        <button
          onClick={toggleCollapsed}
          title="Déplier l'aperçu"
          className="text-[15px] text-muted-2 hover:text-accent"
        >
          ⇤
        </button>
        <div className="mt-3 rotate-90 whitespace-nowrap text-[10px] font-bold tracking-wide text-muted-3">
          APERÇU
        </div>
      </div>
    );
  }
```

Bouton de repli dans la barre d'onglets existante (à droite des Tabs) :

```tsx
        <div className="flex-1" />
        <button
          onClick={toggleCollapsed}
          title="Replier l'aperçu"
          className="px-2 text-[15px] text-muted-2 hover:text-accent"
        >
          ⇥
        </button>
```

(replié = composant retourne tôt → aucun contenu/iframe monté, conforme spec.)

- [ ] **Step 4: Vérifier + commit**

Run: `cd frontend && npx tsc -b && npm run build` — OK.

```bash
git add frontend/src/components/ProjectChip.tsx frontend/src/panels/ChatPanel.tsx frontend/src/panels/PreviewPanel.tsx
git commit -m "feat(ui): chip projet dans le composer + aperçu réductible"
```

---

### Task 6: Vérification bout-en-bout + docs + push

**Files:**
- Modify: `README.md`

- [ ] **Step 1: README**

Section après « Panneau Git & Diff » :

```markdown
## Projets (répertoires de travail)

Chaque session peut travailler dans un **projet** : un sous-dossier du
workspace choisi via le chip 📁 du composer (« + Nouveau projet » pour en
créer un). L'agent, le shell, le moteur code et l'arborescence sont confinés
au projet ; chaque projet a son propre dépôt git (historique et revert
indépendants). Session sans projet = racine du workspace.
```

- [ ] **Step 2: Suite complète**

```bash
cd backend && python -m pytest tests/ -q && python -m compileall -q app
cd ../frontend && npx tsc -b && npm run build
```

- [ ] **Step 3: Smoke local (uvicorn, sans Ollama)**

```bash
cd backend && python -m uvicorn app.main:app --port 8199 &   # env WORKSPACE_DIR/DATA_DIR temporaires
curl -s -X POST http://localhost:8199/api/projects -H "Content-Type: application/json" -d '{"name":"demo"}'
curl -s http://localhost:8199/api/projects
curl -s -X POST http://localhost:8199/api/projects -H "Content-Type: application/json" -d '{"name":"../x"}' -o /dev/null -w "%{http_code}"   # 400
SID=$(curl -s -X POST http://localhost:8199/api/sessions -H "Content-Type: application/json" -d '{"title":"t","project":"demo"}' | python -c "import sys,json;print(json.load(sys.stdin)['id'])")
curl -s -X PATCH http://localhost:8199/api/sessions/$SID -H "Content-Type: application/json" -d '{"project":""}'   # retour racine
curl -s "http://localhost:8199/api/files?project=demo"
```

Expected : création 201 + `.git` ; liste contient demo ; 400 sur nom
invalide ; PATCH ok ; arbre du projet vide (pas les fichiers racine).

- [ ] **Step 4: Commit + push**

```bash
git add README.md
git commit -m "docs: projets par session"
git push
```

Vérif UI après déploiement : chip 📁 → créer `demo` → message « crée
hello.txt » → fichier dans workspace/demo/ ; session B sans projet → racine ;
onglet Git montre l'historique du projet actif ; bouton ⇥ replie l'aperçu,
état conservé au rechargement.
