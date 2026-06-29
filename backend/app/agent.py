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

import json
from typing import AsyncIterator

import httpx

from .ollama_client import ollama
from .tools import TOOL_DEFINITIONS, ToolError, run_tool

MAX_ITERATIONS = 6


def _parse_args(raw) -> dict:
    if isinstance(raw, dict):
        return raw
    if isinstance(raw, str):
        try:
            return json.loads(raw)
        except json.JSONDecodeError:
            return {}
    return {}


async def run_agent(
    model: str,
    convo: list[dict],
    *,
    options: dict | None = None,
    enabled_tools: list[str] | None = None,
    confirm_shell: bool = True,
) -> AsyncIterator[dict]:
    # enabled_tools=None -> tous les outils ; liste vide -> aucun outil.
    if enabled_tools is None:
        tools = TOOL_DEFINITIONS
    elif enabled_tools:
        tools = [
            t for t in TOOL_DEFINITIONS if t["function"]["name"] in enabled_tools
        ]
    else:
        tools = None
    collected: list[dict] = []
    text_parts: list[str] = []

    try:
        for _ in range(MAX_ITERATIONS):
            content_buf = ""
            tool_calls: list[dict] = []

            async for chunk in ollama.chat(
                model, convo, tools=tools, options=options, stream=True
            ):
                msg = chunk.get("message", {})
                token = msg.get("content", "")
                if token:
                    content_buf += token
                    yield {"type": "token", "content": token}
                if msg.get("tool_calls"):
                    tool_calls.extend(msg["tool_calls"])
                if chunk.get("done"):
                    break

            assistant_turn: dict = {"role": "assistant", "content": content_buf}
            if tool_calls:
                assistant_turn["tool_calls"] = tool_calls
            convo.append(assistant_turn)
            if content_buf.strip():
                text_parts.append(content_buf.strip())

            if not tool_calls:
                break

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
                            "name": name,
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
                        "name": name,
                        "content": json.dumps(result, ensure_ascii=False),
                    }
                )

            # Une commande shell attend une validation : on interrompt la boucle.
            if awaiting_confirmation:
                # Laisse le modèle conclure son tour (message d'attente).
                final_chunk = ""
                async for chunk in ollama.chat(model, convo, options=options, stream=True):
                    tok = chunk.get("message", {}).get("content", "")
                    if tok:
                        final_chunk += tok
                        yield {"type": "token", "content": tok}
                    if chunk.get("done"):
                        break
                if final_chunk.strip():
                    text_parts.append(final_chunk.strip())
                break
    except (httpx.HTTPError, OSError) as exc:
        yield {"type": "error", "message": str(exc)}
        return

    yield {
        "type": "final",
        "content": "\n\n".join(text_parts).strip(),
        "tools": collected,
    }
