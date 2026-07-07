"""Benchmark intégré : évalue objectivement chaque modèle installé.

Cinq mini-épreuves (~30-60 s au total) qui mesurent ce qui compte pour Loki :
appel d'outil, code exécutable, respect des consignes, extraction JSON,
respect d'un format. Score /100, stocké en base et affiché dans l'UI.
"""
from __future__ import annotations

import json
import re
import subprocess
import sys
import tempfile
import time
from typing import AsyncIterator

import httpx

from . import db
from .ollama_client import OllamaError, ollama

BENCH_KEY = "bench"  # config[bench] = {model: {score, details, at}}


async def _ask(model: str, prompt: str, *, system: str = "",
               tools: list | None = None, num_predict: int = 400) -> dict:
    """Un appel modèle ; renvoie {text, tool_calls}."""
    messages = []
    if system:
        messages.append({"role": "system", "content": system})
    messages.append({"role": "user", "content": prompt})
    text, calls = "", []
    async for chunk in ollama.chat(
        model, messages, tools=tools,
        options={"temperature": 0, "num_predict": num_predict}, stream=True,
    ):
        msg = chunk.get("message", {})
        text += msg.get("content", "")
        if msg.get("tool_calls"):
            calls.extend(msg["tool_calls"])
        if chunk.get("done"):
            break
    return {"text": text.strip(), "tool_calls": calls}


def _extract_code(text: str) -> str:
    m = re.search(r"```(?:python)?\s*(.*?)```", text, re.S)
    return (m.group(1) if m else text).strip()


def _run_python(code: str, test: str) -> bool:
    """Exécute code+test dans un sous-processus isolé (timeout 8 s)."""
    with tempfile.NamedTemporaryFile("w", suffix=".py", delete=False) as f:
        f.write(code + "\n" + test)
        path = f.name
    try:
        proc = subprocess.run(
            [sys.executable, "-I", path],
            capture_output=True, timeout=8,
        )
        return proc.returncode == 0
    except (subprocess.SubprocessError, OSError):
        return False


# ── Les 5 épreuves (score 0-20 chacune) ──────────────────────────────────
async def _task_tool_call(model: str) -> tuple[int, str]:
    tools = [{
        "type": "function",
        "function": {
            "name": "write_file",
            "description": "Écrire un fichier",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string"},
                    "content": {"type": "string"},
                },
                "required": ["path", "content"],
            },
        },
    }]
    try:
        r = await _ask(
            model,
            "Crée le fichier bonjour.txt contenant exactement le texte : salut",
            system="Utilise l'outil write_file pour créer le fichier demandé.",
            tools=tools, num_predict=200,
        )
    except httpx.HTTPStatusError as exc:
        if "does not support tools" in exc.response.text.lower():
            return 0, "outils non supportés par ce modèle"
        raise
    for tc in r["tool_calls"]:
        fn = tc.get("function", {})
        if fn.get("name") == "write_file":
            args = fn.get("arguments") or {}
            if isinstance(args, str):
                try:
                    args = json.loads(args)
                except json.JSONDecodeError:
                    return 8, "appel d'outil aux arguments illisibles"
            ok_path = "bonjour" in str(args.get("path", "")).lower()
            ok_content = "salut" in str(args.get("content", "")).lower()
            score = 10 + 5 * ok_path + 5 * ok_content
            return score, "appel d'outil correct" if score == 20 else "appel partiel"
    return 0, "aucun appel d'outil émis"


async def _task_code(model: str) -> tuple[int, str]:
    r = await _ask(
        model,
        "Écris une fonction Python `somme_pairs(nombres)` qui renvoie la somme "
        "des nombres pairs de la liste. Réponds UNIQUEMENT avec le code.",
        num_predict=300,
    )
    code = _extract_code(r["text"])
    if "def somme_pairs" not in code:
        return 0, "fonction absente"
    test = (
        "assert somme_pairs([1,2,3,4]) == 6\n"
        "assert somme_pairs([]) == 0\n"
        "assert somme_pairs([7,9]) == 0\n"
    )
    return (20, "code correct (3/3 tests)") if _run_python(code, test) \
        else (6, "code présent mais tests échoués")


async def _task_instruction(model: str) -> tuple[int, str]:
    r = await _ask(
        model,
        "Quelle est la capitale de la France ? Réponds en 3 mots maximum.",
        num_predict=30,
    )
    text = r["text"]
    has_answer = "paris" in text.lower()
    short = len(text.split()) <= 6
    score = 12 * has_answer + 8 * short
    return score, f"réponse « {text[:40]} »"


async def _task_json(model: str) -> tuple[int, str]:
    r = await _ask(
        model,
        'Extrait les informations en JSON strict {"nom": ..., "ville": ...} '
        "depuis : « Marie habite à Lyon ». Réponds UNIQUEMENT avec le JSON.",
        num_predict=80,
    )
    m = re.search(r"\{.*\}", r["text"], re.S)
    if not m:
        return 0, "pas de JSON"
    try:
        data = json.loads(m.group(0))
    except json.JSONDecodeError:
        return 5, "JSON invalide"
    ok_nom = "marie" in str(data.get("nom", "")).lower()
    ok_ville = "lyon" in str(data.get("ville", "")).lower()
    score = 10 + 5 * ok_nom + 5 * ok_ville
    detail = "extraction correcte" if ok_nom and ok_ville else "extraction partielle"
    return score, detail


async def _task_format(model: str) -> tuple[int, str]:
    r = await _ask(
        model,
        "Liste exactement 3 fruits, un par ligne, chaque ligne préfixée par « - ».",
        num_predict=60,
    )
    lines = [l for l in r["text"].splitlines() if l.strip().startswith("-")]
    if len(lines) == 3:
        return 20, "format exact"
    if len(lines) >= 2:
        return 10, f"{len(lines)} lignes au lieu de 3"
    return 0, "format non respecté"


TASKS = [
    ("Appel d'outil", _task_tool_call),
    ("Code exécutable", _task_code),
    ("Consigne courte", _task_instruction),
    ("Extraction JSON", _task_json),
    ("Respect du format", _task_format),
]


async def run_bench(model: str) -> AsyncIterator[dict]:
    """Exécute les 5 épreuves en streamant la progression, stocke le score."""
    total = 0
    details = []
    for name, fn in TASKS:
        yield {"type": "task_start", "task": name}
        try:
            score, detail = await fn(model)
        except (OllamaError, httpx.HTTPError, OSError) as exc:
            score, detail = 0, f"erreur : {str(exc)[:80]}"
        except Exception as exc:
            # Une épreuve défaillante ne doit pas couper silencieusement le SSE :
            # elle vaut zéro et les autres épreuves continuent.
            score, detail = 0, f"épreuve interrompue : {str(exc)[:80]}"
        total += score
        details.append({"task": name, "score": score, "detail": detail})
        yield {"type": "task_done", "task": name, "score": score, "detail": detail}

    results = db.get_config_value(BENCH_KEY) or {}
    results[model] = {"score": total, "details": details, "at": time.time()}
    db.set_config_value(BENCH_KEY, results)
    yield {"type": "done", "score": total, "details": details}


def get_scores() -> dict:
    return db.get_config_value(BENCH_KEY) or {}
