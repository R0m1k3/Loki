"""Outils de l'agent, exécutés côté serveur et confinés au workspace.

Chaque outil expose :
  - une définition JSON (format function-calling Ollama/OpenAI) ;
  - une implémentation Python qui renvoie un dict {ok, summary, ...}.

Toutes les opérations fichier sont strictement confinées à WORKSPACE_DIR :
toute tentative de sortie (../, chemin absolu hors workspace) est rejetée.
"""
from __future__ import annotations

import html
import os
import re
import subprocess

import httpx

from .config import settings


class ToolError(Exception):
    """Erreur d'exécution d'un outil (message destiné au modèle)."""


def _workspace_root() -> str:
    root = os.path.abspath(settings.workspace_dir)
    os.makedirs(root, exist_ok=True)
    return root


def _safe_path(rel: str) -> str:
    """Résout un chemin relatif en restant confiné au workspace."""
    root = _workspace_root()
    target = os.path.abspath(os.path.join(root, rel or "."))
    if target != root and not target.startswith(root + os.sep):
        raise ToolError(f"chemin hors du workspace refusé : {rel}")
    return target


# ── Implémentations ──────────────────────────────────────────────────────
def read_file(path: str) -> dict:
    target = _safe_path(path)
    if not os.path.isfile(target):
        raise ToolError(f"fichier introuvable : {path}")
    with open(target, "r", encoding="utf-8", errors="replace") as f:
        content = f.read()
    size = os.path.getsize(target)
    summary = "fichier vide (0 octet)" if size == 0 else f"{len(content.splitlines())} lignes lues"
    return {"ok": True, "content": content, "summary": summary}


def write_file(path: str, content: str, mode: str = "overwrite") -> dict:
    target = _safe_path(path)
    if mode not in {"overwrite", "append"}:
        raise ToolError("mode write_file invalide : utilise overwrite ou append")
    os.makedirs(os.path.dirname(target) or _workspace_root(), exist_ok=True)
    existed = os.path.isfile(target)
    with open(target, "a" if mode == "append" else "w", encoding="utf-8") as f:
        f.write(content)
    lines = len(content.splitlines())
    verb = "complété" if mode == "append" else "modifié" if existed else "écrit"
    return {"ok": True, "summary": f"{verb} · {lines} lignes", "lines": lines}


def list_dir(path: str = ".") -> dict:
    target = _safe_path(path)
    if not os.path.isdir(target):
        raise ToolError(f"répertoire introuvable : {path}")
    entries = []
    for name in sorted(os.listdir(target)):
        full = os.path.join(target, name)
        entries.append({"name": name, "type": "dir" if os.path.isdir(full) else "file"})
    return {
        "ok": True,
        "entries": entries,
        "summary": f"{len(entries)} élément(s)",
    }


def web_search(query: str, max_results: int = 5) -> dict:
    """Recherche web (DuckDuckGo HTML, sans clé d'API).

    Optionnellement, si SEARX_URL est défini, interroge une instance SearxNG.
    Renvoie une liste de résultats {title, url, snippet}.
    """
    query = (query or "").strip()
    if not query:
        raise ToolError("requête de recherche vide")

    searx = os.environ.get("SEARX_URL")
    try:
        if searx:
            results = _search_searx(searx, query, max_results)
        else:
            results = _search_duckduckgo(query, max_results)
    except httpx.HTTPError as exc:
        raise ToolError(f"recherche web indisponible : {exc}") from exc

    summary = f"{len(results)} résultat(s)" if results else "aucun résultat"
    return {"ok": True, "results": results, "summary": summary}


def _search_searx(base: str, query: str, n: int) -> list[dict]:
    with httpx.Client(timeout=10.0) as client:
        r = client.get(
            base.rstrip("/") + "/search",
            params={"q": query, "format": "json"},
        )
        r.raise_for_status()
        data = r.json().get("results", [])[:n]
    return [
        {"title": d.get("title", ""), "url": d.get("url", ""),
         "snippet": d.get("content", "")}
        for d in data
    ]


_DDG_RESULT = re.compile(
    r'<a[^>]*class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>'
    r'.*?class="result__snippet"[^>]*>(.*?)</a>',
    re.DOTALL,
)
_TAGS = re.compile(r"<[^>]+>")


def _clean(text: str) -> str:
    return html.unescape(_TAGS.sub("", text)).strip()


def _search_duckduckgo(query: str, n: int) -> list[dict]:
    with httpx.Client(timeout=10.0, follow_redirects=True) as client:
        r = client.post(
            "https://html.duckduckgo.com/html/",
            data={"q": query},
            headers={"User-Agent": "Mozilla/5.0 (Loki agent)"},
        )
        r.raise_for_status()
    results = []
    for url, title, snippet in _DDG_RESULT.findall(r.text)[:n]:
        results.append({
            "title": _clean(title),
            "url": html.unescape(url),
            "snippet": _clean(snippet),
        })
    return results


def run_shell(command: str, timeout: int = 60) -> dict:
    """Exécute une commande shell dans le workspace (outil sensible).

    L'exécution effective n'a lieu qu'après validation utilisateur (gérée par
    la boucle agentique / la route /api/shell). Confinée au workspace.
    """
    command = (command or "").strip()
    if not command:
        raise ToolError("commande vide")
    try:
        proc = subprocess.run(
            command,
            shell=True,
            cwd=_workspace_root(),
            capture_output=True,
            text=True,
            timeout=timeout,
        )
    except subprocess.TimeoutExpired as exc:
        raise ToolError(f"délai dépassé ({timeout}s)") from exc

    out = (proc.stdout or "") + (proc.stderr or "")
    out = out[:4000]  # borne la taille renvoyée au modèle
    status = "ok" if proc.returncode == 0 else "error"
    return {
        "ok": proc.returncode == 0,
        "exit_code": proc.returncode,
        "output": out,
        "summary": f"code {proc.returncode}",
        "_status": status,
    }


# ── Registre & définitions exposées au modèle ────────────────────────────
TOOL_IMPL = {
    "read_file": read_file,
    "write_file": write_file,
    "list_dir": list_dir,
    "web_search": web_search,
    "run_shell": run_shell,
}

TOOL_DEFINITIONS = [
    {
        "type": "function",
        "function": {
            "name": "read_file",
            "description": "Lire le contenu d'un fichier du workspace.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "Chemin relatif au workspace"}
                },
                "required": ["path"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "write_file",
            "description": "Créer ou modifier un fichier du workspace.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "Chemin relatif au workspace"},
                    "content": {
                        "type": "string",
                        "description": "Contenu complet ou morceau court du fichier",
                    },
                    "mode": {
                        "type": "string",
                        "enum": ["overwrite", "append"],
                        "description": (
                            "overwrite pour le premier morceau, append pour les suivants"
                        ),
                    },
                },
                "required": ["path", "content"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "list_dir",
            "description": "Lister le contenu d'un répertoire du workspace.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "Répertoire (défaut : racine)"}
                },
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "web_search",
            "description": "Rechercher sur le web et renvoyer les meilleurs résultats.",
            "parameters": {
                "type": "object",
                "properties": {
                    "query": {"type": "string", "description": "Termes de recherche"}
                },
                "required": ["query"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "run_shell",
            "description": (
                "Exécuter une commande shell dans le workspace. Outil sensible :"
                " l'utilisateur doit valider la commande avant exécution."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "command": {"type": "string", "description": "Commande à exécuter"}
                },
                "required": ["command"],
            },
        },
    },
]


def run_tool(name: str, args: dict) -> dict:
    """Exécute un outil par son nom ; lève ToolError si inconnu/invalide."""
    impl = TOOL_IMPL.get(name)
    if impl is None:
        raise ToolError(f"outil inconnu : {name}")
    try:
        return impl(**(args or {}))
    except ToolError:
        raise
    except TypeError as exc:
        raise ToolError(f"arguments invalides pour {name} : {exc}") from exc
    except OSError as exc:
        raise ToolError(f"erreur système ({name}) : {exc}") from exc
