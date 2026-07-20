"""Profil de configuration de l'agent : invite système, génération, outils.

Persisté en base sous la clé `agent`. Fournit les valeurs par défaut et la
fusion avec ce qui est stocké, pour rester robuste aux montées de version.
"""
from __future__ import annotations

from . import db

CONFIG_KEY = "agent"
MODEL_PROFILES_KEY = "model_profiles"
PROFILE_STATE_KEY = "model_profiles_state"
PROFILE_VERSION = 7

DEFAULT_SYSTEM_PROMPT = (
    "Tu es Loki, un assistant de développement local agentique. Tu disposes "
    "d'outils pour lire, écrire et lister des fichiers dans le workspace, et "
    "d'un moteur code (code_task) pour toute création ou modification de code "
    "multi-fichiers : privilégie code_task pour les tâches de programmation. "
    "Utilise les outils pour accomplir les tâches concrètement, puis réponds "
    "de façon concise en français. Pour MODIFIER un fichier existant, utilise "
    "edit_file (search/replace) plutôt que de tout réécrire : lis d'abord le "
    "fichier avec read_file, puis copie dans `search` l'extrait EXACT à changer "
    "(quelques lignes suffisent, l'indentation est tolérée). N'emploie "
    "write_file en overwrite QUE pour créer un nouveau fichier ou en cas de "
    "refonte complète. Quand tu appelles write_file, fournis toujours `path` et "
    "`content` ; pour un fichier long, appelle write_file en plusieurs morceaux "
    "(overwrite puis append) afin de toujours produire un JSON valide. Tu ne "
    "peux écrire QUE dans le workspace : jamais de chemin absolu ni de `../` "
    "qui en sortent. Après avoir écrit un fichier, propose un aperçu. "
    "Formate TOUJOURS tes réponses en Markdown : titres, listes, gras pour les "
    "points clés, tableaux si pertinent, et surtout des blocs de code avec le "
    "langage indiqué (```python, ```html…) pour tout extrait de code ou commande."
)

# Outils disponibles. Les sensibles (web_search, run_shell) sont désactivés
# par défaut, conformément à la maquette.
AVAILABLE_TOOLS = [
    "read_file", "write_file", "edit_file", "list_dir", "grep_search",
    "run_check", "code_task", "web_search", "run_shell",
]
SENSITIVE_TOOLS = {"run_shell"}
DEFAULT_TOOL_STATE = {
    "read_file": True,
    "write_file": True,
    "edit_file": True,
    "list_dir": True,
    "grep_search": True,
    "run_check": True,
    "code_task": True,
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
PROFILE_FIELDS = {
    "system_prompt",
    "tools",
    "confirm_shell",
    "think",
    "code_model",
    "plan_mode",
    "self_review",
    "rag_enabled",
    "embed_model",
    "skills_enabled",
    "keep_alive",
    *GENERATION_FIELDS,
}

DEFAULT_GENERATION: dict = {
    "temperature": 0.7,
    "top_p": 0.9,
    "top_k": 40,
    "max_tokens": 2048,
    # 16k : les tâches longues (recherche, multi-fichiers) saturaient 4-8k en
    # un seul tour et Ollama tronquait la consigne. Le cache KV d'un 16k reste
    # raisonnable (~1-3 Go selon modèle) ; réduis num_ctx dans Configuration
    # si la VRAM déborde, ou active OLLAMA_KV_CACHE_TYPE=q8_0 côté Ollama.
    "num_ctx": 16384,
    "num_gpu": -1,
    "num_batch": 256,
}

RTX_3060_GEMMA4_PROFILE: dict = {
    **DEFAULT_GENERATION,
    "max_tokens": 4096,
    "num_ctx": 16384,
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
    # Modèle utilisé par le moteur code : "auto" = meilleur modèle code installé
    # (qwen-coder, deepseek-coder…), sinon le modèle de chat courant.
    "code_model": "auto",
    # Plan-puis-exécute : décompose les demandes complexes en étapes.
    "plan_mode": True,
    # Auto-critique : une passe de relecture/révision avant la réponse finale.
    "self_review": False,
    # Mémoire long-terme (RAG) ENTRE sessions, via un modèle d'embedding.
    # Désactivée par défaut : chaque discussion ne se souvient que d'elle-même
    # (résumé + messages récents). Sinon une ancienne demande sans rapport (ex.
    # « appli sport ») ressurgit dans une nouvelle discussion (ex. « jeu
    # d'échecs ») et embrouille les petits modèles. Réactivable dans Réglages.
    "rag_enabled": False,
    "embed_model": "auto",
    # Skills : méthodes expertes injectées automatiquement selon la tâche.
    "skills_enabled": True,
    # Durée de maintien du modèle en VRAM (préchargement). "0" = décharge
    # aussitôt, "-1" = jamais, "30m" = 30 minutes.
    "keep_alive": "30m",
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
    # v6 : contexte 16k par défaut. On ne touche qu'aux profils restés sur un
    # ancien défaut (4096/8192) — une valeur personnalisée est respectée.
    for prof in profiles.values():
        if prof.get("num_ctx") in (4096, 8192):
            prof["num_ctx"] = 16384
    db.set_config_value(MODEL_PROFILES_KEY, profiles)

    # v7 : la mémoire inter-sessions (RAG) faisait ressurgir d'anciennes
    # demandes sans rapport dans une nouvelle discussion. On la désactive une
    # fois sur les installs existantes ; réactivable manuellement dans Réglages.
    stored = db.get_config_value(CONFIG_KEY)
    if stored and stored.get("rag_enabled"):
        stored["rag_enabled"] = False
        db.set_config_value(CONFIG_KEY, stored)

    db.set_config_value(PROFILE_STATE_KEY, {"version": PROFILE_VERSION})


def _clean_tools(value: dict | None, fallback: dict | None = None) -> dict:
    fallback = fallback or DEFAULT_TOOL_STATE
    return {
        name: bool((value or {}).get(name, fallback.get(name, DEFAULT_TOOL_STATE[name])))
        for name in AVAILABLE_TOOLS
    }


def get_config(model: str | None = None) -> dict:
    """Configuration complète, avec surcharge sauvegardée par modèle."""
    _migrate_profiles()
    stored = db.get_config_value(CONFIG_KEY) or {}
    cfg = {**DEFAULT_CONFIG, **stored}
    cfg["tools"] = _clean_tools(stored.get("tools"))
    if model:
        profiles = db.get_config_value(MODEL_PROFILES_KEY) or {}
        profile = profiles.get(model, {})
        cfg.update({**_default_generation(model), **profile})
        cfg["tools"] = _clean_tools(profile.get("tools"), cfg["tools"])
    return cfg


def save_config(patch: dict, model: str | None = None) -> dict:
    """Sauvegarde tous les réglages, globalement ou pour un modèle."""
    clean = {k: v for k, v in patch.items() if v is not None}
    cfg = {**get_config(model), **clean}
    if "tools" in patch and patch["tools"]:
        cfg["tools"] = _clean_tools(patch["tools"], cfg["tools"])

    if model:
        profiles = db.get_config_value(MODEL_PROFILES_KEY) or {}
        profiles[model] = {field: cfg[field] for field in PROFILE_FIELDS}
        db.set_config_value(MODEL_PROFILES_KEY, profiles)
    else:
        db.set_config_value(CONFIG_KEY, {field: cfg[field] for field in PROFILE_FIELDS})
    return get_config(model)


def runner_options(cfg: dict) -> dict:
    """Sous-ensemble d'options qui détermine l'identité du runner Ollama.

    Ollama choisit son runner (processus de chargement du modèle) d'après
    num_ctx / num_batch / num_gpu. TOUT appel au même modèle (plan, résumé,
    agent…) doit envoyer ces mêmes valeurs, sinon Ollama recharge le modèle en
    plein milieu d'un message — la cause principale des lenteurs observées.
    """
    opts: dict = {"num_batch": cfg["num_batch"]}
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


def ollama_options(cfg: dict) -> dict:
    """Traduit la config en options de génération Ollama."""
    return {
        **runner_options(cfg),
        "temperature": cfg["temperature"],
        "top_p": cfg["top_p"],
        "top_k": cfg["top_k"],
        "num_predict": cfg["max_tokens"],
    }


def enabled_tool_names(cfg: dict) -> list[str]:
    return [name for name, on in cfg["tools"].items() if on]
