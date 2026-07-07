"""Moteur code : enveloppe Aider pour l'édition multi-fichiers fiable.

Aider apporte ce qui fait la force de Claude Code/Codex : formats d'édition
diff/search-replace robustes (même avec de petits modèles), repo map, et
commits git automatiques dans le workspace.

L'API Python d'Aider n'étant pas officiellement stable, la version est FIGÉE
dans requirements.txt (aider-chat==0.86.2) et tout l'import est local à la
fonction (l'app démarre même si Aider est absent).
"""
from __future__ import annotations

import os
import subprocess
import threading

from .config import settings

# Aider utilise le répertoire courant : un seul run à la fois.
_RUN_LOCK = threading.Lock()


def ensure_git(root: str) -> None:
    """Initialise un dépôt git dans le workspace (requis pour les commits Aider)."""
    os.makedirs(root, exist_ok=True)
    if os.path.isdir(os.path.join(root, ".git")):
        return
    subprocess.run(["git", "init", "-q"], cwd=root, check=False)
    subprocess.run(["git", "config", "user.email", "loki@local"], cwd=root, check=False)
    subprocess.run(["git", "config", "user.name", "Loki"], cwd=root, check=False)


def available() -> bool:
    """Aider est-il installé ?"""
    try:
        import aider  # noqa: F401
        return True
    except ImportError:
        return False


# Familles spécialisées code, par ordre de préférence.
_CODE_MODEL_HINTS = (
    "qwen3-coder", "qwen2.5-coder", "deepseek-coder", "codestral", "devstral",
    "codegemma", "codellama", "starcoder", "coder",
)


async def pick_code_model(current: str, preference: str | None = None) -> str:
    """Choisit le modèle pour les tâches de code.

    - préférence explicite (config code_model != "auto") -> respectée ;
    - sinon, si un modèle spécialisé code est installé, on le prend (le plus
      gros d'abord) : un petit modèle code bat un généraliste sur ce terrain ;
    - sinon, on garde le modèle courant.
    """
    if preference and preference != "auto":
        return preference

    from .ollama_client import ollama
    try:
        installed = await ollama.list_models()
    except Exception:
        return current

    candidates: list[tuple[int, int, str]] = []  # (rang_hint, -taille, nom)
    for m in installed:
        name = (m.get("name") or "").lower()
        for rank, hint in enumerate(_CODE_MODEL_HINTS):
            if hint in name:
                candidates.append((rank, -(m.get("size") or 0), m["name"]))
                break

    if not candidates:
        return current
    candidates.sort()
    return candidates[0][2]


def run_code_task(
    instruction: str,
    model: str,
    files: list[str] | None = None,
) -> dict:
    """Exécute une tâche de code via Aider (synchrone — lancer dans un thread).

    Renvoie {ok, text, files, commit, commit_message, summary}.
    """
    if not available():
        return {
            "ok": False,
            "summary": "moteur code indisponible (aider non installé)",
            "text": "", "files": [], "commit": None,
        }

    from aider.coders import Coder
    from aider.io import InputOutput
    from aider.models import Model

    root = os.path.abspath(settings.workspace_dir)
    ensure_git(root)

    # Aider parle à Ollama via litellm : on pointe vers notre instance.
    os.environ["OLLAMA_API_BASE"] = settings.ollama_host

    with _RUN_LOCK:
        prev_cwd = os.getcwd()
        os.chdir(root)
        try:
            io = InputOutput(yes=True, pretty=False, fancy_input=False)
            coder = Coder.create(
                main_model=Model(f"ollama_chat/{model}"),
                io=io,
                fnames=[os.path.join(root, f) for f in (files or [])],
                auto_commits=True,
                stream=False,
                use_git=True,
                suggest_shell_commands=False,
                detect_urls=False,
            )
            text = coder.run(instruction) or ""
            edited = sorted(coder.aider_edited_files or [])
            commit = getattr(coder, "last_aider_commit_hash", None)
            commit_msg = getattr(coder, "last_aider_commit_message", None)
        except Exception as exc:  # aider peut lever des erreurs variées
            return {
                "ok": False,
                "summary": f"échec moteur code : {exc}",
                "text": "", "files": [], "commit": None,
            }
        finally:
            os.chdir(prev_cwd)

    summary = f"{len(edited)} fichier(s) modifié(s)"
    if commit:
        summary += f" · commit {str(commit)[:7]}"
    return {
        "ok": True,
        "text": text.strip(),
        "files": edited,
        "commit": commit,
        "commit_message": commit_msg,
        "summary": summary,
    }
