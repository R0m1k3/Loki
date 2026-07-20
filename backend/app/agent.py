"""Boucle agentique : tool-calling itératif au-dessus d'Ollama.

L'agent dialogue avec le modèle ; quand celui-ci demande un outil, on l'exécute,
on réinjecte le résultat, et on reboucle jusqu'à une réponse finale (ou la
limite d'itérations). La fonction est un générateur asynchrone d'événements
relayés tels quels au client via SSE.

Événements émis :
  token        {content}           — fragment de texte de l'agent
  tool_call    {name, args}        — début d'exécution d'un outil
  tool_result  {name, args, summary, status}
  final        {content, tools}    — réponse complète + récap des outils
"""
from __future__ import annotations

import asyncio
import json
import re
from typing import AsyncIterator

import httpx

from . import coder
from .ollama_client import OllamaError, ollama
from .tools import TOOL_DEFINITIONS, ToolError, run_tool

MAX_ITERATIONS = 6
MAX_TOOL_REPAIR_ATTEMPTS = 2

# Coupe-circuit de réflexion : au-delà de cette taille de pensée SANS aucun
# contenu ni appel d'outil, on interrompt la génération en cours au lieu
# d'attendre l'épuisement du budget num_predict (minutes sur un gros modèle).
_MAX_THINKING_CHARS = 12_000

# Élagage : au-delà de cette taille, un résultat d'outil des itérations
# passées est tronqué. Les gros payloads (MCP 8 Ko, shell 4 Ko) saturaient le
# contexte en un seul tour long — Ollama tronquait alors silencieusement le
# DÉBUT de la conversation, faisant « oublier » la consigne au modèle.
_PRUNE_KEEP_CHARS = 350


def _prune_old_tool_results(convo: list[dict], before_index: int) -> None:
    """Compacte les résultats d'outils déjà consommés par le modèle.

    Seuls les messages ``tool`` antérieurs à ``before_index`` (donc traités
    lors d'une itération précédente) sont tronqués ; le dernier lot reste
    intact, c'est celui auquel le modèle répond.
    """
    for msg in convo[:before_index]:
        content = msg.get("content", "")
        if msg.get("role") == "tool" and len(content) > _PRUNE_KEEP_CHARS:
            msg["content"] = (
                content[:_PRUNE_KEEP_CHARS]
                + "… [résultat archivé — déjà traité, ne pas redemander]"
            )


def _tools_not_supported(exc: OllamaError) -> bool:
    """Détecte un modèle incapable de function calling.

    Deux cas :
      - Ollama refuse explicitement (« does not support tools ») ;
      - Ollama n'arrive pas à dériver un parseur d'appels d'outils du template
        du modèle (« Unable to generate parser for this template ») — fréquent
        sur des modèles exotiques dont le template Jinja lève une exception.
    Dans les deux cas, on retombe sur une conversation simple, sans outils.
    """
    message = str(exc).lower()
    return (
        "does not support tools" in message
        or "does not support tool" in message
        or "unable to generate parser for this template" in message
        or "automatic parser generation failed" in message
    )


def _thinking_not_supported(exc: OllamaError) -> bool:
    """Le modèle (ou son template) refuse le paramètre ``think``."""
    message = str(exc).lower()
    return "does not support thinking" in message or "thinking is not supported" in message


def _invalid_tool_arguments(exc: OllamaError) -> bool:
    message = str(exc).lower()
    return (
        "invalid tool call arguments" in message
        or "unexpected end of json" in message
        or "failed to parse tool" in message
    )


def _parse_args(raw) -> dict:
    if isinstance(raw, dict):
        return raw
    if isinstance(raw, str):
        try:
            return json.loads(raw)
        except json.JSONDecodeError:
            return {}
    return {}


# Marqueur d'avancement du plan émis par le modèle (« ✅ Étape 2 terminée »).
# Tolérant : coche/croix optionnelle, mot « étape » optionnel, numéro requis.
_STEP_DONE = re.compile(
    r"(?:✅|✔|☑|\[x\])\s*(?:étape|etape|step)?\s*(\d{1,2})"
    r"|(?:étape|etape|step)\s*(\d{1,2})\s*(?:terminée|terminee|faite|ok|✅|✔)",
    re.I,
)


def _scan_plan_done(text: str, plan_len: int, already: set[int]) -> list[int]:
    """Indices (0-based) de nouvelles étapes annoncées terminées dans ``text``."""
    fresh: list[int] = []
    for m in _STEP_DONE.finditer(text):
        num = m.group(1) or m.group(2)
        idx = int(num) - 1
        if 0 <= idx < plan_len and idx not in already:
            already.add(idx)
            fresh.append(idx)
    return fresh


async def run_agent(
    model: str,
    convo: list[dict],
    *,
    options: dict | None = None,
    enabled_tools: list[str] | None = None,
    confirm_shell: bool = True,
    think: bool = True,
    keep_alive: str | None = None,
    mcp_tools: list[dict] | None = None,
    plan: list[str] | None = None,
) -> AsyncIterator[dict]:
    # enabled_tools=None -> tous les outils ; liste vide -> aucun outil.
    if enabled_tools is None:
        tools = list(TOOL_DEFINITIONS)
    elif enabled_tools:
        tools = [
            t for t in TOOL_DEFINITIONS if t["function"]["name"] in enabled_tools
        ]
    else:
        tools = []
    # Outils MCP des serveurs activés (déjà résolus par l'appelant).
    tools.extend(mcp_tools or [])
    if not tools:
        tools = None
    collected: list[dict] = []
    text_parts: list[str] = []
    thinking_parts: list[str] = []
    active_tools = tools
    tool_fallback_used = False
    tool_repair_attempts = 0
    request_options = dict(options or {})
    # On n'envoie ``think`` que pour le DÉSACTIVER (False) ; laissé à None, le
    # modèle garde son comportement par défaut. Repli si le modèle le refuse.
    request_think: bool | None = None if think else False
    # Mode réflexion : la pensée consomme le budget num_predict. Sans marge,
    # le modèle « pense » tout son quota et s'arrête sans agir ni répondre.
    if request_think is None and think:
        request_options["num_predict"] = max(
            int(request_options.get("num_predict") or 0), 6144
        )
    # Compteur d'itérations où le modèle n'a produit QUE du raisonnement.
    thinking_only_strikes = 0

    # Suivi de l'avancement du plan : étapes déjà annoncées terminées.
    plan_len = len(plan or [])
    plan_done: set[int] = set()

    # Métriques cumulées sur tous les appels Ollama du tour agentique : Ollama
    # les renvoie dans le chunk final (done=true) de chaque génération.
    stats = {"eval_count": 0, "eval_duration": 0, "prompt_eval_count": 0}

    def _accumulate(chunk: dict) -> None:
        stats["eval_count"] += chunk.get("eval_count") or 0
        stats["eval_duration"] += chunk.get("eval_duration") or 0
        stats["prompt_eval_count"] += chunk.get("prompt_eval_count") or 0

    # Index du début du dernier lot de résultats d'outils (à préserver).
    last_batch_start = 0

    try:
        for _ in range(MAX_ITERATIONS):
            content_buf = ""
            thinking_buf = ""
            thinking_status_sent = False
            tool_calls: list[dict] = []

            # Compacte les résultats d'outils des itérations antérieures :
            # garde le contexte court, la consigne système jamais tronquée.
            if last_batch_start:
                _prune_old_tool_results(convo, last_batch_start)

            # Un modèle peut savoir discuter sans supporter les outils. Ollama
            # refuse alors la requête entière : on retente une fois en chat simple.
            while True:
                try:
                    async for chunk in ollama.chat(
                        model,
                        convo,
                        tools=active_tools,
                        options=request_options,
                        think=request_think,
                        keep_alive=keep_alive,
                        stream=True,
                    ):
                        msg = chunk.get("message", {})
                        token = msg.get("content", "")
                        thinking = msg.get("thinking", "")
                        if thinking:
                            thinking_buf += thinking
                            # Diffuse le raisonnement en direct pour l'afficher
                            # dans le panneau repliable du chat.
                            yield {"type": "thinking", "content": thinking}
                            if not thinking_status_sent:
                                thinking_status_sent = True
                                yield {"type": "status", "message": "Réflexion…"}
                            # Pensée interminable sans production : on coupe la
                            # génération maintenant — la relance (plus bas)
                            # remet le modèle au travail immédiatement.
                            if (
                                len(thinking_buf) > _MAX_THINKING_CHARS
                                and not content_buf
                                and not tool_calls
                            ):
                                break
                        if token:
                            content_buf += token
                            yield {"type": "token", "content": token}
                            # Coche les étapes du plan annoncées terminées, en
                            # direct. Scan borné : uniquement quand le token
                            # porte un marqueur plausible.
                            if plan_len and any(
                                c in token for c in ("✅", "✔", "☑", "tape", "step", "]")
                            ):
                                for idx in _scan_plan_done(content_buf, plan_len, plan_done):
                                    yield {"type": "plan_step", "index": idx, "status": "done"}
                        if msg.get("tool_calls"):
                            tool_calls.extend(msg["tool_calls"])
                        if chunk.get("done"):
                            _accumulate(chunk)
                            break
                    break
                except OllamaError as exc:
                    if (
                        active_tools
                        and tool_repair_attempts < MAX_TOOL_REPAIR_ATTEMPTS
                        and not content_buf
                        and not tool_calls
                        and _invalid_tool_arguments(exc)
                    ):
                        tool_repair_attempts += 1
                        request_options["num_predict"] = max(
                            int(request_options.get("num_predict", 0)), 4096
                        )
                        thinking_buf = ""
                        # NB : on réinjecte ce rappel en `user`, pas en `system`.
                        # Beaucoup de templates (Gemma, Mistral…) lèvent
                        # « System message must be at the beginning » dès qu'un
                        # message system apparaît ailleurs qu'en tête, ce qui
                        # ferait échouer toute la requête en 400.
                        convo.append(
                            {
                                "role": "user",
                                "content": (
                                    "L'appel d'outil précédent contenait un JSON "
                                    "tronqué. Réessaie immédiatement avec des arguments "
                                    "JSON valides. Pour un fichier long, utilise write_file "
                                    "en plusieurs appels : overwrite puis append, avec des "
                                    "morceaux courts et complets."
                                ),
                            }
                        )
                        yield {
                            "type": "notice",
                            "message": (
                                "Appel d'outil tronqué : nouvelle tentative "
                                f"{tool_repair_attempts}/{MAX_TOOL_REPAIR_ATTEMPTS}."
                            ),
                        }
                        continue
                    if (
                        active_tools
                        and not tool_fallback_used
                        and not content_buf
                        and not tool_calls
                        and _tools_not_supported(exc)
                    ):
                        active_tools = None
                        tool_fallback_used = True
                        yield {
                            "type": "notice",
                            "message": (
                                "Ce modèle ne supporte pas les outils ; "
                                "réponse en mode conversation simple."
                            ),
                        }
                        continue
                    if request_think is not None and _thinking_not_supported(exc):
                        # Le modèle n'accepte pas qu'on désactive sa réflexion :
                        # on retire le paramètre et on relance.
                        request_think = None
                        continue
                    raise

            # Itération « réflexion seule » : ni contenu, ni appel d'outil —
            # le modèle a brûlé sa génération à penser. Sans relance, la
            # boucle s'arrêtait là et la tâche restait inachevée.
            if thinking_buf and not content_buf.strip() and not tool_calls:
                thinking_parts.append(thinking_buf)
                thinking_only_strikes += 1
                if thinking_only_strikes == 1:
                    convo.append({
                        "role": "user",
                        "content": (
                            "Tu n'as produit que du raisonnement, sans réponse "
                            "ni action. Continue la tâche MAINTENANT : appelle "
                            "l'outil suivant ou donne ta réponse finale, sans "
                            "réfléchir davantage."
                        ),
                    })
                    yield {"type": "status", "message": "Relance après réflexion…"}
                    continue
                # Deuxième fois : la réflexion est coupée pour finir la tâche.
                request_think = False
                yield {
                    "type": "notice",
                    "message": (
                        "Réflexion désactivée pour ce tour : le modèle "
                        "n'avançait plus."
                    ),
                }
                continue

            assistant_turn: dict = {"role": "assistant", "content": content_buf}
            if thinking_buf:
                # Gardée pour l'affichage (panneau repliable), mais JAMAIS
                # renvoyée au modèle : la re-soumettre gonflait le contexte à
                # chaque itération des longues tâches.
                thinking_parts.append(thinking_buf)
            if tool_calls:
                assistant_turn["tool_calls"] = tool_calls
            convo.append(assistant_turn)
            if content_buf.strip():
                text_parts.append(content_buf.strip())

            if not tool_calls:
                break

            # Les résultats du lot qui suit commencent ici : ils restent
            # intacts au prochain tour, les précédents seront compactés.
            last_batch_start = len(convo)

            # Exécution des outils demandés, puis réinjection des résultats.
            awaiting_confirmation = False
            for tc in tool_calls:
                fn = tc.get("function", {})
                name = fn.get("name", "")
                args = _parse_args(fn.get("arguments"))

                yield {"type": "tool_call", "name": name, "args": args}

                # run_shell est sensible : on demande validation au lieu d'exécuter.
                if name == "run_shell" and confirm_shell:
                    command = args.get("command", "")
                    record = {
                        "name": name,
                        "args": args,
                        "summary": "validation requise",
                        "status": "pending",
                    }
                    collected.append(record)
                    yield {"type": "tool_confirm", "name": name, "command": command}
                    convo.append(
                        {
                            "role": "tool",
                            "tool_name": name,
                            "content": json.dumps(
                                {
                                    "ok": False,
                                    "status": "pending",
                                    "message": "Commande en attente de validation "
                                    "de l'utilisateur. N'exécute rien d'autre.",
                                },
                                ensure_ascii=False,
                            ),
                        }
                    )
                    awaiting_confirmation = True
                    continue

                # code_task : délégué au moteur code (Aider), long -> thread.
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
                    summary = result.get("summary", "terminé")
                    status = "ok" if result.get("ok") else "error"
                    record = {"name": name, "args": {"instruction": args.get("instruction", "")},
                              "summary": summary, "status": status}
                    collected.append(record)
                    yield {"type": "tool_result", **record}
                    convo.append({
                        "role": "tool",
                        "tool_name": name,
                        "content": json.dumps(
                            {k: result.get(k) for k in ("ok", "summary", "files", "text")},
                            ensure_ascii=False,
                        ),
                    })
                    continue

                # Outils MCP : dispatch asynchrone vers le serveur concerné.
                if name.startswith("mcp_"):
                    from .mcp_client import manager as mcp_manager
                    mcp_res = await mcp_manager.call_tool(name, args)
                    status = "ok" if mcp_res["ok"] else "error"
                    record = {"name": name, "args": args,
                              "summary": mcp_res["summary"], "status": status}
                    collected.append(record)
                    yield {"type": "tool_result", **record}
                    convo.append({
                        "role": "tool",
                        "tool_name": name,
                        "content": json.dumps(
                            {"ok": mcp_res["ok"], "content": mcp_res["content"]},
                            ensure_ascii=False,
                        ),
                    })
                    continue

                try:
                    result = run_tool(name, args)
                    summary = result.get("summary", "terminé")
                    status = result.get("_status", "ok")
                except ToolError as exc:
                    result = {"ok": False, "error": str(exc)}
                    summary = str(exc)
                    status = "error"

                record = {"name": name, "args": args, "summary": summary, "status": status}
                collected.append(record)
                yield {"type": "tool_result", **record}

                convo.append(
                    {
                        "role": "tool",
                        "tool_name": name,
                        "content": json.dumps(result, ensure_ascii=False),
                    }
                )

            # Une commande shell attend une validation : on interrompt la boucle.
            if awaiting_confirmation:
                # Laisse le modèle conclure son tour (message d'attente).
                final_chunk = ""
                async for chunk in ollama.chat(
                    model, convo, options=request_options,
                    think=request_think, keep_alive=keep_alive, stream=True
                ):
                    tok = chunk.get("message", {}).get("content", "")
                    if tok:
                        final_chunk += tok
                        yield {"type": "token", "content": tok}
                    if chunk.get("done"):
                        _accumulate(chunk)
                        break
                if final_chunk.strip():
                    text_parts.append(final_chunk.strip())
                break
    except OllamaError as exc:
        # Échec signalé par Ollama (HTTP ou ligne d'erreur dans le flux) :
        # souvent un débordement mémoire / contexte trop grand. On l'expose.
        yield {"type": "error", "message": f"Ollama : {exc}"}
        return
    except (httpx.HTTPError, OSError) as exc:
        yield {
            "type": "error",
            "message": f"Impossible de joindre Ollama ({ollama.host}) : {exc}",
        }
        return

    final_content = "\n\n".join(text_parts).strip()
    if not final_content and not collected:
        yield {
            "type": "error",
            "message": (
                "Le modèle a terminé sans renvoyer de texte (il n'a produit que "
                "du raisonnement). Désactive « Mode réflexion » dans les Réglages, "
                "ou essaie un modèle de chat plus récent."
            ),
        }
        return

    eval_secs = stats["eval_duration"] / 1e9
    final_stats = {
        "eval_count": stats["eval_count"],
        "prompt_eval_count": stats["prompt_eval_count"],
        "tokens_per_sec": (
            round(stats["eval_count"] / eval_secs, 1) if eval_secs > 0 else None
        ),
    }
    yield {
        "type": "final",
        "content": final_content,
        "tools": collected,
        "stats": final_stats,
        "thinking": "\n\n".join(thinking_parts).strip(),
    }
