"""Profil de configuration de l'agent : invite système, génération, outils.

Persisté en base sous la clé `agent`. Fournit les valeurs par défaut et la
fusion avec ce qui est stocké, pour rester robuste aux montées de version.
"""
from __future__ import annotations

from . import db

CONFIG_KEY = "agent"
MODEL_PROFILES_KEY = "model_profiles"
PROFILE_STATE_KEY = "model_profiles_state"
PROFILE_VERSION = 4

DEFAULT_SYSTEM_PROMPT = (
    "Tu es Loki, un assistant de développement local agentique. Tu disposes "
    "d'outils pour lire, écrire et lister des fichiers dans le workspace. "
    "Utilise-les pour accomplir les tâches concrètement, puis réponds de façon "
    "concise en français. Pour un fichier long, appelle write_file en plusieurs "
    "morceaux (overwrite puis append) afin de toujours produire un JSON valide. "
    "Après avoir écrit un fichier, propose un aperçu. "
    "Formate TOUJOURS tes réponses en Markdown : titres, listes, gras pour les "
    "points clés, tableaux si pertinent, et surtout des blocs de code avec le "
    "langage indiqué (```python, ```html…) pour tout extrait de code ou commande."
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

GENERATION_FIELDS = {
    "temperature",
    "top_p",
    "top_k",
    "max_tokens",
    "num_ctx",
    "num_gpu",
    "num_batch",
}

DEFAULT_GENERATION: dict = {
    "temperature": 0.7,
    "top_p": 0.9,
    "top_k": 40,
    "max_tokens": 2048,
    "num_ctx": 4096,
    "num_gpu": -1,
    "num_batch": 256,
}

RTX_3060_GEMMA4_PROFILE: dict = {
    **DEFAULT_GENERATION,
    "max_tokens": 4096,
    "num_ctx": 8192,
    # num_gpu = -1 : laisse Ollama placer le plus de couches possible sur le GPU
    # (auto-fit, comme `ollama run`). Forcer un nombre de couches qui ne tient pas
    # en VRAM fait basculer toute l'inférence sur le CPU.
    "num_gpu": -1,
}

DEFAULT_CONFIG: dict = {
    "system_prompt": DEFAULT_SYSTEM_PROMPT,
    **DEFAULT_GENERATION,
    "tools": dict(DEFAULT_TOOL_STATE),
    # Demander une validation utilisateur avant toute commande shell.
    "confirm_shell": True,
    # Mode réflexion des modèles « thinking ». Désactiver (False) évite qu'un
    # modèle ne renvoie que du raisonnement sans réponse finale.
    "think": True,
}


def _default_generation(model: str | None) -> dict:
    if model and model.split(":", 1)[0].lower() == "gemma4":
        return dict(RTX_3060_GEMMA4_PROFILE)
    return dict(DEFAULT_GENERATION)


def _migrate_profiles() -> None:
    state = db.get_config_value(PROFILE_STATE_KEY) or {}
    if state.get("version", 0) >= PROFILE_VERSION:
        return
    profiles = db.get_config_value(MODEL_PROFILES_KEY) or {}
    gemma_profile = {
        **RTX_3060_GEMMA4_PROFILE,
        **profiles.get("gemma4:12b", {}),
    }
    if gemma_profile.get("max_tokens", 0) <= 2048:
        gemma_profile["max_tokens"] = 4096
    profiles["gemma4:12b"] = gemma_profile
    # v4 : un ancien profil pouvait forcer num_gpu sur un nombre de couches codé
    # en dur (ex. 49), ce qui basculait l'inférence sur le CPU quand ça ne tenait
    # pas en VRAM. On repasse en auto (-1) pour laisser Ollama placer les couches.
    for prof in profiles.values():
        if prof.get("num_gpu", -1) is not None and prof.get("num_gpu", -1) > 0:
            prof["num_gpu"] = -1
    db.set_config_value(MODEL_PROFILES_KEY, profiles)
    db.set_config_value(PROFILE_STATE_KEY, {"version": PROFILE_VERSION})


def get_config(model: str | None = None) -> dict:
    """Configuration globale complétée par le profil du modèle demandé."""
    _migrate_profiles()
    stored = db.get_config_value(CONFIG_KEY) or {}
    global_stored = {k: v for k, v in stored.items() if k not in GENERATION_FIELDS}
    cfg = {**DEFAULT_CONFIG, **global_stored}
    if model:
        profiles = db.get_config_value(MODEL_PROFILES_KEY) or {}
        cfg.update({**_default_generation(model), **profiles.get(model, {})})
    cfg["tools"] = {
        name: bool(stored.get("tools", {}).get(name, DEFAULT_TOOL_STATE[name]))
        for name in AVAILABLE_TOOLS
    }
    return cfg


def save_config(patch: dict, model: str | None = None) -> dict:
    """Sauvegarde le comportement global et la génération par modèle."""
    clean = {k: v for k, v in patch.items() if v is not None}
    cfg = {**get_config(model), **clean}
    if "tools" in patch and patch["tools"]:
        cfg["tools"] = {
            name: bool(
                patch["tools"].get(
                    name, cfg["tools"].get(name, DEFAULT_TOOL_STATE[name])
                )
            )
            for name in AVAILABLE_TOOLS
        }
    global_cfg = {k: v for k, v in cfg.items() if k not in GENERATION_FIELDS}
    db.set_config_value(CONFIG_KEY, global_cfg)

    if model:
        profiles = db.get_config_value(MODEL_PROFILES_KEY) or {}
        profiles[model] = {field: cfg[field] for field in GENERATION_FIELDS}
        db.set_config_value(MODEL_PROFILES_KEY, profiles)
    return get_config(model)


def ollama_options(cfg: dict) -> dict:
    """Traduit la config en options de génération Ollama."""
    opts = {
        "temperature": cfg["temperature"],
        "top_p": cfg["top_p"],
        "top_k": cfg["top_k"],
        "num_predict": cfg["max_tokens"],
        "num_batch": cfg["num_batch"],
    }
    # num_gpu n'est transmis que si l'utilisateur force explicitement un nombre
    # de couches (≥ 0). En -1 (défaut), on laisse Ollama auto-ajuster l'offload
    # GPU comme `ollama run` ; lui imposer une valeur peut le forcer sur le CPU.
    if cfg.get("num_gpu", -1) >= 0:
        opts["num_gpu"] = cfg["num_gpu"]
        opts["main_gpu"] = 0
    # num_ctx n'est envoyé que s'il est défini (> 0), sinon défaut du modèle.
    if cfg.get("num_ctx"):
        opts["num_ctx"] = cfg["num_ctx"]
    return opts


def enabled_tool_names(cfg: dict) -> list[str]:
    return [name for name, on in cfg["tools"].items() if on]
