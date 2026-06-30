"""Profil de configuration de l'agent : invite système, génération, outils.

Persisté en base sous la clé `agent`. Fournit les valeurs par défaut et la
fusion avec ce qui est stocké, pour rester robuste aux montées de version.
"""
from __future__ import annotations

from . import db

CONFIG_KEY = "agent"

DEFAULT_SYSTEM_PROMPT = (
    "Tu es Loki, un assistant de développement local agentique. Tu disposes "
    "d'outils pour lire, écrire et lister des fichiers dans le workspace. "
    "Utilise-les pour accomplir les tâches concrètement, puis réponds de façon "
    "concise en français. Après avoir écrit un fichier, propose un aperçu."
)

# Outils disponibles. Les sensibles (web_search, run_shell) sont désactivés
# par défaut, conformément à la maquette.
AVAILABLE_TOOLS = ["read_file", "write_file", "list_dir", "web_search", "run_shell"]
SENSITIVE_TOOLS = {"run_shell"}
DEFAULT_TOOL_STATE = {
    "read_file": True,
    "write_file": True,
    "list_dir": True,
    "web_search": False,
    "run_shell": False,
}

DEFAULT_CONFIG: dict = {
    "system_prompt": DEFAULT_SYSTEM_PROMPT,
    "temperature": 0.7,
    "top_p": 0.9,
    "top_k": 40,
    "max_tokens": 2048,
    # Fenêtre de contexte envoyée à Ollama (0 = laisser le défaut du modèle).
    "num_ctx": 0,
    "tools": dict(DEFAULT_TOOL_STATE),
    # Demander une validation utilisateur avant toute commande shell.
    "confirm_shell": True,
}


def get_config() -> dict:
    """Config courante = défauts fusionnés avec le stockage."""
    stored = db.get_config_value(CONFIG_KEY) or {}
    cfg = {**DEFAULT_CONFIG, **stored}
    cfg["tools"] = {
        name: bool(stored.get("tools", {}).get(name, DEFAULT_TOOL_STATE[name]))
        for name in AVAILABLE_TOOLS
    }
    return cfg


def save_config(patch: dict) -> dict:
    """Applique une mise à jour partielle et renvoie la config complète."""
    cfg = {**get_config(), **{k: v for k, v in patch.items() if v is not None}}
    if "tools" in patch and patch["tools"]:
        cfg["tools"] = {
            name: bool(
                patch["tools"].get(
                    name, cfg["tools"].get(name, DEFAULT_TOOL_STATE[name])
                )
            )
            for name in AVAILABLE_TOOLS
        }
    db.set_config_value(CONFIG_KEY, cfg)
    return get_config()


def ollama_options(cfg: dict) -> dict:
    """Traduit la config en options de génération Ollama."""
    opts = {
        "temperature": cfg["temperature"],
        "top_p": cfg["top_p"],
        "top_k": cfg["top_k"],
        "num_predict": cfg["max_tokens"],
    }
    # num_ctx n'est envoyé que s'il est défini (> 0), sinon défaut du modèle.
    if cfg.get("num_ctx"):
        opts["num_ctx"] = cfg["num_ctx"]
    return opts


def enabled_tool_names(cfg: dict) -> list[str]:
    return [name for name, on in cfg["tools"].items() if on]
