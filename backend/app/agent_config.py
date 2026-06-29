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

# Outils réellement disponibles (les sensibles arrivent en phase 7).
AVAILABLE_TOOLS = ["read_file", "write_file", "list_dir"]

DEFAULT_CONFIG: dict = {
    "system_prompt": DEFAULT_SYSTEM_PROMPT,
    "temperature": 0.7,
    "top_p": 0.9,
    "top_k": 40,
    "max_tokens": 2048,
    "tools": {name: True for name in AVAILABLE_TOOLS},
}


def get_config() -> dict:
    """Config courante = défauts fusionnés avec le stockage."""
    stored = db.get_config_value(CONFIG_KEY) or {}
    cfg = {**DEFAULT_CONFIG, **stored}
    cfg["tools"] = {
        name: bool(stored.get("tools", {}).get(name, True))
        for name in AVAILABLE_TOOLS
    }
    return cfg


def save_config(patch: dict) -> dict:
    """Applique une mise à jour partielle et renvoie la config complète."""
    cfg = {**get_config(), **{k: v for k, v in patch.items() if v is not None}}
    if "tools" in patch and patch["tools"]:
        cfg["tools"] = {
            name: bool(patch["tools"].get(name, cfg["tools"].get(name, True)))
            for name in AVAILABLE_TOOLS
        }
    db.set_config_value(CONFIG_KEY, cfg)
    return get_config()


def ollama_options(cfg: dict) -> dict:
    """Traduit la config en options de génération Ollama."""
    return {
        "temperature": cfg["temperature"],
        "top_p": cfg["top_p"],
        "top_k": cfg["top_k"],
        "num_predict": cfg["max_tokens"],
    }


def enabled_tool_names(cfg: dict) -> list[str]:
    return [name for name, on in cfg["tools"].items() if on]
