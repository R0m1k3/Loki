import { useEffect, useRef, useState } from "react";
import { useStore } from "../store/useStore";
import { ClipIcon, LokiMark, SendIcon } from "../components/Icon";
import { ToolCard } from "../components/ToolCard";
import type { Message, ToolCall } from "../api/client";

/** Panneau central : barre de contexte, fil de conversation, composer. */
export function ChatPanel() {
  const {
    selectedModel,
    messages,
    streaming,
    streamContent,
    streamTools,
    sendMessage,
    currentSessionId,
  } = useStore();

  const [draft, setDraft] = useState("");
  const scrollRef = useRef<HTMLDivElement>(null);

  // Auto-scroll vers le bas à chaque token / message.
  useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight });
  }, [messages, streamContent, streamTools]);

  const submit = () => {
    if (!draft.trim() || streaming) return;
    sendMessage(draft);
    setDraft("");
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      submit();
    }
  };

  const empty = messages.length === 0 && !streaming;

  return (
    <div className="flex min-w-0 flex-1 flex-col bg-base">
      {/* Barre de contexte */}
      <div className="flex h-[42px] flex-none items-center gap-2 border-b border-line-soft px-[18px]">
        <Chip>Invite système</Chip>
        <Chip>
          <span className="h-1.5 w-1.5 rounded-full bg-ok" />3 outils actifs
        </Chip>
        <Chip>
          Température <b className="font-semibold text-[#e7ddcd]">0.7</b>
        </Chip>
        <div className="flex-1" />
        <span className="font-mono text-[11px] text-muted-4">
          {currentSessionId
            ? `${messages.length} message${messages.length > 1 ? "s" : ""}`
            : "aucune session"}
        </span>
      </div>

      {/* Messages */}
      <div ref={scrollRef} className="scr flex-1 overflow-auto px-7 py-6">
        {empty ? (
          <div className="mx-auto flex max-w-[680px] flex-col items-center justify-center gap-4 pt-24 text-center">
            <LokiMark size={44} />
            <div className="text-lg font-semibold text-ink">
              Prêt à travailler.
            </div>
            <div className="max-w-[360px] text-sm leading-relaxed text-muted">
              Décris une tâche à l'agent. Les outils fichiers et l'aperçu en
              direct arrivent à la prochaine étape.
            </div>
          </div>
        ) : (
          <div className="mx-auto flex max-w-[680px] flex-col gap-[22px]">
            {messages.map((m) => (
              <Bubble key={m.id} msg={m} />
            ))}
            {streaming && (
              <Bubble
                msg={{
                  id: "stream",
                  session_id: "",
                  role: "assistant",
                  content: streamContent,
                  model: selectedModel,
                  meta: { tools: streamTools },
                  created_at: Date.now() / 1000,
                }}
                pending
              />
            )}
          </div>
        )}
      </div>

      {/* Composer */}
      <div className="flex-none border-t border-line-soft px-7 pb-[18px] pt-3.5">
        <div className="mx-auto max-w-[680px]">
          <div className="rounded-[14px] border border-line-strong bg-card-soft p-[13px]">
            <textarea
              rows={1}
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              onKeyDown={onKeyDown}
              placeholder="Envoyer un message à l'agent…"
              className="min-h-[42px] w-full resize-none bg-transparent text-sm leading-relaxed text-ink outline-none placeholder:text-muted-3"
            />
            <div className="mt-1.5 flex items-center gap-2">
              <button className="flex h-[30px] w-[30px] items-center justify-center rounded-lg border border-line-strong text-muted-2">
                <ClipIcon />
              </button>
              <div className="flex h-[30px] items-center gap-1.5 rounded-lg border border-line-strong px-2.5 font-mono text-[11.5px] text-muted">
                <span className="h-1.5 w-1.5 rounded-full bg-accent" />
                {selectedModel || "—"}
              </div>
              <div className="flex-1" />
              <span className="font-mono text-[10.5px] text-muted-4">
                ⏎ envoyer · ⇧⏎ ligne
              </span>
              <button
                onClick={submit}
                disabled={streaming || !draft.trim()}
                className="flex h-[34px] items-center gap-1.5 rounded-[9px] bg-accent px-4 text-[13px] font-bold text-[#1f1b16] disabled:opacity-50"
              >
                {streaming ? "…" : "Envoyer"}
                {!streaming && <SendIcon />}
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

function Bubble({ msg, pending }: { msg: Message; pending?: boolean }) {
  const time = new Date(msg.created_at * 1000).toLocaleTimeString("fr-FR", {
    hour: "2-digit",
    minute: "2-digit",
  });

  if (msg.role === "user") {
    return (
      <div className="flex gap-3">
        <div className="flex h-[30px] w-[30px] flex-none items-center justify-center rounded-[9px] bg-[#3a332b] text-xs font-semibold text-ink-2">
          M
        </div>
        <div className="flex-1">
          <div className="mb-1.5 flex items-baseline gap-2">
            <span className="text-[13px] font-semibold">Vous</span>
            <span className="font-mono text-[10.5px] text-muted-4">{time}</span>
          </div>
          <div className="rounded-xl rounded-tl-[4px] border border-line bg-card-soft px-[15px] py-3 text-sm leading-relaxed text-ink-2 whitespace-pre-wrap">
            {msg.content}
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="flex gap-3">
      <div className="flex h-[30px] w-[30px] flex-none items-center justify-center rounded-[9px] bg-accent-grad">
        <div
          className="bg-base"
          style={{ width: 8, height: 8, borderRadius: 2, transform: "rotate(45deg)" }}
        />
      </div>
      <div className="min-w-0 flex-1">
        <div className="mb-2 flex items-baseline gap-2">
          <span className="text-[13px] font-semibold">Agent</span>
          <span className="font-mono text-[10.5px] text-muted-4">
            {msg.model ? `${msg.model} · ` : ""}
            {time}
          </span>
        </div>
        {(msg.meta?.tools ?? []).map((t: ToolCall, i: number) => (
          <ToolCard key={i} call={t} />
        ))}
        {(msg.content || pending) && (
          <div className="text-sm leading-[1.65] text-ink-2 whitespace-pre-wrap">
            {msg.content}
            {pending && (
              <span className="ml-0.5 inline-block h-3.5 w-[7px] animate-pulse bg-accent align-middle" />
            )}
          </div>
        )}
      </div>
    </div>
  );
}

function Chip({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex h-[25px] items-center gap-1.5 rounded-[7px] border border-line-strong bg-card-soft px-[9px] text-[11.5px] text-ink-3">
      {children}
    </div>
  );
}
