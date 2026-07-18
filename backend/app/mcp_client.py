"""Client MCP : catalogue préconfiguré + sessions vers les serveurs activés.

Un serveur désactivé n'est jamais démarré et n'expose aucun outil au modèle
(chaque outil injecté coûte du contexte). Connexion lazy au premier message,
session réutilisée ensuite. Toute panne est non bloquante pour le chat.
"""
from __future__ import annotations

import logging

from . import db

logger = logging.getLogger(__name__)

MCP_KEY = "mcp"

# Catalogue embarqué. command=None => serveur "custom" (commande utilisateur).
CATALOG: dict[str, dict] = {
    "playwright": {
        "label": "Playwright (navigateur)",
        "description": "Pilote un vrai navigateur : naviguer, cliquer, lire "
                       "la console, captures. L'agent teste réellement ses pages.",
        "command": ["npx", "@playwright/mcp@latest", "--headless"],
        "url_param": False,
        "env_params": [],
        # Limite le nombre d'outils injectés dans le prompt.
        "expose": [
            "browser_navigate", "browser_click", "browser_type",
            "browser_snapshot", "browser_console_messages",
            "browser_take_screenshot",
        ],
    },
    "context7": {
        "label": "Context7 (documentation)",
        "description": "Documentation à jour de n'importe quelle librairie ou "
                       "framework (React, FastAPI, Tailwind…).",
        "command": ["npx", "-y", "@upstash/context7-mcp"],
        "url_param": False,
        "env_params": [],
        "expose": None,
    },
    "fetch": {
        "label": "Fetch (lecture web)",
        "description": "Lit proprement n'importe quelle URL (markdown épuré).",
        "command": ["python", "-m", "mcp_server_fetch"],
        "url_param": False,
        "env_params": [],
        "expose": None,
    },
    "searxng": {
        "label": "SearxNG (recherche web)",
        "description": "Vraie recherche web via une instance SearxNG "
                       "(renseigner SEARXNG_URL).",
        "command": ["npx", "-y", "mcp-searxng"],
        "url_param": False,
        "env_params": ["SEARXNG_URL"],
        "expose": None,
    },
    "custom": {
        "label": "Personnalisé",
        "description": "N'importe quel serveur MCP : colle une commande "
                       "(stdio) ou une URL (streamable HTTP).",
        "command": None,
        "url_param": True,
        "env_params": [],
        "expose": None,
    },
}


def get_mcp_state() -> dict:
    """État activé/params de chaque serveur du catalogue (défaut : désactivé)."""
    stored = db.get_config_value(MCP_KEY) or {}
    return {
        sid: {
            "enabled": bool(stored.get(sid, {}).get("enabled", False)),
            "params": dict(stored.get(sid, {}).get("params", {})),
        }
        for sid in CATALOG
    }


def set_mcp_state(sid: str, *, enabled: bool, params: dict) -> dict:
    if sid not in CATALOG:
        raise KeyError(sid)
    state = get_mcp_state()
    state[sid] = {"enabled": enabled, "params": dict(params)}
    db.set_config_value(MCP_KEY, state)
    return state
