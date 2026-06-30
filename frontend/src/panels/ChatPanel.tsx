import { useEffect, useRef, useState } from "react";
import { useStore } from "../store/useStore";
import { ChevronDown, ClipIcon, LokiMark, SendIcon } from "../components/Icon";
import { ToolCard } from "../components/ToolCard";
import { MessageContent } from "../components/MessageContent";
import type { Message, ToolCall } from "../api/client";

/** Panneau central : barre de contexte, fil de conversation, composer. */
export function ChatPanel() {
  const {
    selectedModel,
    messages,
    streaming,
    streamingSessionId,
    streamContent,
    streamThinking,
    streamStatus,
    streamNotice,
    streamTools,
    sendMessage,
    currentSessionId,
    config,
    pendingShell,
    approveShell,
    rejectShell,
    sessions,
  } = useStore();

  const activeTools = config
    ? Object.values(config.tools).filter(Boolean).length
    : 0;

  const [draft, setDraft] = useState("");
  const scrollRef = useRef<HTMLDivElement>(null);

  // Auto-scroll vers le bas à chaque token / message.
  useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight });
  }, [messages, streamContent, streamThinking, streamTools]);

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

  const showingStreaming = streaming && currentSessionId === streamingSessionId;
  const workingSession = sessions.find((s) => s.id === streamingSessionId);
  const empty = messages.length === 0 && !showingStreaming;

  return (
    <div className="flex min-w-0 flex-1 flex-col bg-base">
      {/* Barre de contexte */}
      <div className="flex h-[46px] flex-none items-center gap-2 border-b-[3px] border-line bg-panel px-4">
        <Chip>⚑ Invite système</Chip>
        <Chip>
          <span className="h-2 w-2 border-2 border-line bg-ok" />
          {activeTools} outil{activeTools > 1 ? "s" : ""}
        </Chip>
        <Chip>
          Temp{" "}
          <b className="text-accent">{config ? config.temperature.toFixed(1) : "—"}</b>
        </Chip>
        <div className="flex-1" />
        <span className="text-[13px] text-muted-3">
          {currentSessionId
            ? `${messages.length} message${messages.length > 1 ? "s" : ""}`
            : "aucune session"}
        </span>
      </div>

      {/* Messages */}
      <div ref={scrollRef} className="scr flex-1 overflow-auto px-7 py-6">
        {empty ? (
          <div className="mx-auto flex max-w-[680px] flex-col items-center justify-center gap-4 pt-24 text-center">
            <LokiMark size={48} />
            <div className="font-pixel text-[15px] text-ink">PRÊT À TRAVAILLER</div>
            <div className="max-w-[380px] text-[14px] leading-relaxed text-muted">
              Décris une tâche à l'agent. Outils fichiers et aperçu en direct
              disponibles.
            </div>
          </div>
        ) : (
          <div className="mx-auto flex max-w-[680px] flex-col gap-5">
            {messages.map((m) => (
              <Bubble key={m.id} msg={m} />
            ))}
            {showingStreaming && (
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
                pendingStatus={streamStatus}
                notice={streamNotice}
                thinking={streamThinking}
              />
            )}
            {showingStreaming && pendingShell && (
              <ShellConfirm
                command={pendingShell}
                onApprove={approveShell}
                onReject={rejectShell}
              />
            )}
          </div>
        )}
      </div>

      {/* Composer */}
      <div className="flex-none border-t-[3px] border-line bg-panel px-7 pb-[18px] pt-3.5">
        <div className="mx-auto max-w-[680px]">
          <div className="border-[3px] border-line bg-card p-3 shadow-hard" style={{ borderRadius: 8 }}>
            <textarea
              rows={1}
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              onKeyDown={onKeyDown}
              placeholder="Envoyer un message à l'agent…"
              className="min-h-[40px] w-full resize-none bg-transparent text-[14px] leading-relaxed text-ink outline-none placeholder:text-muted-3"
            />
            <div className="mt-1.5 flex items-center gap-2">
              <button className="flex h-8 w-8 items-center justify-center border-2 border-line text-ink-2">
                <ClipIcon />
              </button>
              <div
                className="flex h-8 min-w-0 max-w-[220px] items-center gap-1.5 border-2 border-line px-2.5 text-[13px] text-ink-2"
                title={selectedModel || undefined}
              >
                <span className="h-2 w-2 border-2 border-line bg-accent" />
                <span className="min-w-0 truncate">{selectedModel || "—"}</span>
              </div>
              {streaming && !showingStreaming ? (
                <span className="min-w-0 flex-1 truncate text-[13px] text-accent">
                  Travail en cours : {workingSession?.title ?? "session ouverte"}
                </span>
              ) : (
                <>
                  <div className="flex-1" />
                  <span className="text-[13px] text-muted-3">⏎ envoyer · ⇧⏎ ligne</span>
                </>
              )}
              <button
                onClick={submit}
                disabled={streaming || !draft.trim()}
                className="flex h-[38px] items-center gap-1.5 border-[3px] border-line bg-card-deep px-4 text-[14px] text-white shadow-hard-accent disabled:opacity-40"
                style={{ borderRadius: 7 }}
              >
                {streaming ? "…" : "ENVOYER"}
                {!streaming && <SendIcon />}
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

function Bubble({
  msg,
  pending,
  pendingStatus,
  notice,
  thinking,
}: {
  msg: Message;
  pending?: boolean;
  pendingStatus?: string;
  notice?: string | null;
  thinking?: string;
}) {
  const time = new Date(msg.created_at * 1000).toLocaleTimeString("fr-FR", {
    hour: "2-digit",
    minute: "2-digit",
  });

  if (msg.role === "user") {
    return (
      <div className="flex gap-3">
        <div className="font-pixel flex h-[34px] w-[34px] flex-none items-center justify-center border-[3px] border-line bg-card-deep text-[11px] text-white">
          M
        </div>
        <div className="flex-1">
          <div className="mb-1.5 flex items-baseline gap-2">
            <span className="text-[14px] text-ink">VOUS</span>
            <span className="text-[13px] text-muted-3">{time}</span>
          </div>
          <div className="border-[3px] border-line bg-card px-[14px] py-3 text-[14px] leading-snug text-ink shadow-hard-sm whitespace-pre-wrap" style={{ borderRadius: 7 }}>
            {msg.content}
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="flex gap-3">
      <div className="flex h-[34px] w-[34px] flex-none items-center justify-center border-[3px] border-line bg-accent">
        <div style={{ width: 10, height: 10, background: "#fff" }} />
      </div>
      <div className="min-w-0 flex-1">
        <div className="mb-2 flex items-baseline gap-2">
          <span className="text-[14px] text-ink">LOKI</span>
          <span className="text-[13px] text-muted-3">
            {msg.model ? `${msg.model} · ` : ""}
            {time}
          </span>
        </div>
        <ReasoningPanel
          text={thinking ?? msg.meta?.thinking ?? ""}
          live={!!pending}
        />
        {(msg.meta?.tools ?? []).map((t: ToolCall, i: number) => (
          <ToolCard key={i} call={t} />
        ))}
        {notice && (
          <div className="mb-2 border-[3px] border-line bg-card px-3 py-2 text-[13px] text-warn">
            {notice}
          </div>
        )}
        {(msg.content || pending) && (
          <div className="text-[14px] leading-[1.5] text-ink-2">
            {msg.content ? (
              <MessageContent text={msg.content} />
            ) : (
              <span className="text-muted-2">{pendingStatus || "Génération…"}</span>
            )}
            {pending && (
              <span className="ml-0.5 inline-block h-3.5 w-[7px] animate-pulse bg-accent align-middle" />
            )}
          </div>
        )}
        {!pending && msg.meta?.stats && (
          <div className="mt-2 flex items-center gap-2.5 text-[12px] text-muted-3">
            {msg.meta.stats.tokens_per_sec != null && (
              <span className="text-accent-2">
                {msg.meta.stats.tokens_per_sec} tok/s
              </span>
            )}
            <span>{msg.meta.stats.eval_count} jetons</span>
            {msg.meta.stats.prompt_eval_count > 0 && (
              <span>· {msg.meta.stats.prompt_eval_count} en entrée</span>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

/** Panneau « Raisonnement » : repliable et redimensionnable (poignée en bas). */
function ReasoningPanel({ text, live }: { text: string; live: boolean }) {
  const [open, setOpen] = useState(live);
  const bodyRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (live) setOpen(true);
  }, [live]);

  useEffect(() => {
    if (open && live) {
      bodyRef.current?.scrollTo({ top: bodyRef.current.scrollHeight });
    }
  }, [text, open, live]);

  if (!text) return null;

  return (
    <div className="mb-[11px] overflow-hidden border-[3px] border-line bg-card shadow-hard-sm" style={{ borderRadius: 7 }}>
      <button
        onClick={() => setOpen((o) => !o)}
        className="flex w-full items-center gap-2 px-3 py-2 text-left"
      >
        <ChevronDown
          size={12}
          className={`text-ink-2 transition-transform ${open ? "" : "-rotate-90"}`}
        />
        <span className="text-[13px] font-medium text-ink">Raisonnement</span>
        {live && (
          <span className="flex items-center gap-1 text-[12px] text-accent">
            <span className="h-2 w-2 animate-pulse border-2 border-line bg-accent" />
            en cours…
          </span>
        )}
        <span className="ml-auto text-[12px] text-muted-3">
          {open ? "réduire" : "afficher"}
        </span>
      </button>
      {open && (
        <div
          ref={bodyRef}
          className="scr max-h-[200px] min-h-[60px] resize-y overflow-auto whitespace-pre-wrap border-t-2 border-line-soft bg-card-deep px-3 py-2 text-[12.5px] leading-relaxed text-on-dark-2"
        >
          {text}
        </div>
      )}
    </div>
  );
}

function ShellConfirm({
  command,
  onApprove,
  onReject,
}: {
  command: string;
  onApprove: () => void;
  onReject: () => void;
}) {
  return (
    <div className="ml-[46px] overflow-hidden border-[3px] border-line bg-card shadow-hard" style={{ borderRadius: 7 }}>
      <div className="flex items-center gap-2 border-b-2 border-line px-3 py-2.5">
        <span className="text-[14px] text-accent">run_shell</span>
        <span className="text-[13px] text-muted-2">· commande sensible à valider</span>
      </div>
      <div className="px-3 py-3">
        <pre className="m-0 mb-3 overflow-auto whitespace-pre-wrap border-2 border-line bg-card-deep px-3 py-2.5 text-[12.5px] text-on-dark">
          $ {command}
        </pre>
        <div className="flex items-center gap-2">
          <button
            onClick={onApprove}
            className="flex h-[32px] items-center gap-1.5 border-[3px] border-line bg-card-deep px-3.5 text-[13px] text-white shadow-hard-accent"
            style={{ borderRadius: 7 }}
          >
            Approuver &amp; exécuter
          </button>
          <button
            onClick={onReject}
            className="flex h-[32px] items-center border-[3px] border-line bg-card px-3.5 text-[13px] text-ink-2"
            style={{ borderRadius: 7 }}
          >
            Refuser
          </button>
        </div>
      </div>
    </div>
  );
}

function Chip({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex h-7 items-center gap-1.5 border-2 border-line bg-card px-2.5 text-[13px] text-ink-2">
      {children}
    </div>
  );
}
