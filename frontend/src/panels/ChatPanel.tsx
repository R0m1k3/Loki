import { useEffect, useRef, useState } from "react";
import { useStore } from "../store/useStore";
import { ChevronDown, LokiMark, SendIcon } from "../components/Icon";
import { ToolCard } from "../components/ToolCard";
import { MessageContent } from "../components/MessageContent";
import { ProjectChip } from "../components/ProjectChip";
import { ModelSelector } from "../components/ModelSelector";
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
    streamPlan,
    streamPlanDone,
    sendMessage,
    currentSessionId,
    config,
    pendingShell,
    approveShell,
    rejectShell,
    sessions,
    stopStreaming,
  } = useStore();

  const activeTools = config
    ? Object.values(config.tools).filter(Boolean).length
    : 0;

  const [draft, setDraft] = useState("");
  const scrollRef = useRef<HTMLDivElement>(null);

  // Auto-scroll vers le bas à chaque token / message.
  useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight });
  }, [messages, streamContent, streamThinking, streamTools, streamPlan, streamPlanDone]);

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
      <div className="flex h-[46px] flex-none items-center gap-2 border-b border-line bg-panel px-4">
        <Chip>⚑ Invite système</Chip>
        <Chip>
          <span className="h-2 w-2 border border-line bg-ok" />
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
          <Welcome
            sessionCount={sessions.length}
            onPick={(text) => setDraft(text)}
          />
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
                  meta: { tools: streamTools, plan: streamPlan },
                  created_at: Date.now() / 1000,
                }}
                pending
                pendingStatus={streamStatus}
                notice={streamNotice}
                thinking={streamThinking}
                planDone={streamPlanDone}
              />
            )}
            {/* La validation shell persiste APRÈS la fin du flux : l'agent
                termine son tour en attendant l'utilisateur, donc le streaming
                s'arrête — la carte doit rester tant qu'on n'a pas tranché. */}
            {pendingShell && (
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
      <div className="flex-none border-t border-line bg-panel px-7 pb-[18px] pt-3.5">
        <div className="mx-auto max-w-[680px]">
          <div className="border border-line bg-card p-3 shadow-hard" style={{ borderRadius: 8 }}>
            <textarea
              rows={1}
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              onKeyDown={onKeyDown}
              placeholder="Envoyer un message à l'agent…"
              className="min-h-[40px] w-full resize-none bg-transparent text-[14px] leading-relaxed text-ink outline-none placeholder:text-muted-3"
            />
            <div className="mt-1.5 flex items-center gap-2">
              <ModeSelector />
              <ProjectChip />
              <ModelSelector variant="composer" />
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
                onClick={streaming ? stopStreaming : submit}
                disabled={!streaming && !draft.trim()}
                className={`flex h-[38px] items-center gap-1.5 border border-line px-4 text-[14px] text-white shadow-hard-accent disabled:opacity-40 ${
                  streaming ? "bg-warn" : "bg-accent"
                }`}
                style={{ borderRadius: 7 }}
              >
                {streaming ? "ARRÊTER" : "ENVOYER"}
                {!streaming && <SendIcon />}
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

/** Pistes proposées sur l'écran d'accueil (maquette « Loki App »). */
const STARTERS = [
  ["📄", "Résumer un dossier", "Résume le contenu du dossier "],
  ["🐞", "Corriger un bug", "Corrige le bug suivant : "],
  ["✏️", "Refactorer un fichier", "Refactore le fichier "],
  ["⌨️", "Écrire un script", "Écris un script qui "],
  ["🔍", "Chercher dans le code", "Cherche dans le code "],
  ["📊", "Analyser un CSV", "Analyse le fichier CSV "],
  ["🌿", "Préparer un commit", "Prépare un commit pour "],
  ["📥", "Générer un rapport", "Génère un rapport sur "],
] as const;

/** Écran d'accueil : accroche, pistes cliquables, renvoi vers l'historique. */
function Welcome({
  sessionCount,
  onPick,
}: {
  sessionCount: number;
  onPick: (text: string) => void;
}) {
  return (
    <div className="flex h-full flex-col items-center justify-center px-7 py-10 text-center">
      <LokiMark size={44} />
      <h1 className="mt-6 text-[34px] font-medium text-ink">
        Bienvenue dans Loki
      </h1>
      <p className="mt-3 max-w-[52ch] text-[15.5px] leading-relaxed text-muted">
        Choisissez une piste ci-dessous, ou écrivez directement votre demande.
        Tout s'exécute sur cette machine, aucune donnée ne sort.
      </p>

      <div className="mt-8 flex max-w-[760px] flex-wrap justify-center gap-2.5">
        {STARTERS.map(([icon, label, prefill]) => (
          <button
            key={label}
            onClick={() => onPick(prefill)}
            className="flex items-center gap-2 whitespace-nowrap rounded-full border border-line px-3.5 py-2 text-[13.5px] text-ink-2 hover:border-accent hover:bg-accent-ghost"
          >
            <span aria-hidden>{icon}</span>
            {label}
          </button>
        ))}
      </div>

      {sessionCount > 0 && (
        <div className="mt-9 flex items-center gap-3 text-[12.5px] text-muted-3">
          <span className="h-px w-11 bg-line" />
          {sessionCount} conversation{sessionCount > 1 ? "s" : ""} enregistrée
          {sessionCount > 1 ? "s" : ""} localement
          <span className="h-px w-11 bg-line" />
        </div>
      )}
    </div>
  );
}

/** Plan d'exécution affiché avant le travail de l'agent. */
const MODES = [
  { id: "plan", label: "Plan", icon: "🔍", desc: "Lecture seule : analyse et propose, sans rien modifier" },
  { id: "build", label: "Build", icon: "🔨", desc: "Normal : écrit les fichiers, confirme les commandes shell" },
  { id: "yolo", label: "Yolo", icon: "⚡", desc: "Auto : approuve tout, y compris le shell" },
] as const;

/** Sélecteur de mode d'exécution (Plan / Build / Yolo) dans le composer. */
function ModeSelector() {
  const { mode, setMode } = useStore();
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const onClick = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onClick);
    return () => document.removeEventListener("mousedown", onClick);
  }, []);

  const current = MODES.find((m) => m.id === mode) ?? MODES[1];

  return (
    <div className="relative" ref={ref}>
      <button
        onClick={() => setOpen((v) => !v)}
        className={`flex h-8 items-center gap-1.5 border border-line px-2.5 text-[13px] ${
          mode === "plan"
            ? "bg-info text-white"
            : mode === "yolo"
              ? "bg-accent text-white"
              : "bg-card text-ink-2"
        }`}
        title={current.desc}
      >
        <span>{current.icon}</span>
        <span>{current.label}</span>
        <ChevronDown size={11} />
      </button>
      {open && (
        <div className="absolute bottom-10 left-0 z-20 w-64 border border-line bg-card p-1 shadow-hard">
          {MODES.map((m) => (
            <button
              key={m.id}
              onClick={() => {
                setMode(m.id);
                setOpen(false);
              }}
              className={`flex w-full flex-col items-start gap-0.5 px-2 py-2 text-left hover:bg-base ${
                m.id === mode ? "bg-base" : ""
              }`}
            >
              <span className="text-[13px] text-ink">
                {m.icon} {m.label}
              </span>
              <span className="text-[11px] leading-tight text-muted-2">{m.desc}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

function PlanCard({
  steps,
  done,
  live,
}: {
  steps: string[];
  done?: number[];
  live?: boolean;
}) {
  const doneSet = new Set(done ?? []);
  // Étape active = la première non validée, uniquement pendant le stream.
  const activeIdx = live
    ? steps.findIndex((_, i) => !doneSet.has(i))
    : -1;
  const doneCount = doneSet.size;

  return (
    <div
      className="mb-[11px] overflow-hidden border border-line bg-card shadow-hard-sm"
      style={{ borderRadius: 7 }}
    >
      <div className="flex items-center gap-2 border-b border-line-soft px-3 py-2">
        <span className="font-pixel text-[9px] text-accent">PLAN</span>
        <span className="text-[12px] text-muted-2">
          {doneCount > 0
            ? `${doneCount}/${steps.length} validée${doneCount > 1 ? "s" : ""}`
            : `${steps.length} étape${steps.length > 1 ? "s" : ""}`}
        </span>
      </div>
      <ol className="m-0 list-none px-3 py-2">
        {steps.map((s, i) => {
          const isDone = doneSet.has(i);
          const isActive = i === activeIdx;
          return (
            <li
              key={i}
              className={`flex gap-2 py-[3px] text-[13px] ${
                isDone ? "text-muted-2" : "text-ink-2"
              }`}
            >
              <span
                className={`flex h-[18px] w-[18px] flex-none items-center justify-center border text-[11px] ${
                  isDone
                    ? "border-ok bg-ok text-white"
                    : isActive
                    ? "border-accent bg-base text-accent"
                    : "border-line bg-base text-ink"
                }`}
              >
                {isDone ? "✓" : isActive ? "…" : i + 1}
              </span>
              <span className={`min-w-0 ${isDone ? "line-through" : ""}`}>
                {s}
              </span>
            </li>
          );
        })}
      </ol>
    </div>
  );
}

function Bubble({
  msg,
  pending,
  pendingStatus,
  notice,
  thinking,
  planDone,
}: {
  msg: Message;
  pending?: boolean;
  pendingStatus?: string;
  notice?: string | null;
  thinking?: string;
  planDone?: number[];
}) {
  const time = new Date(msg.created_at * 1000).toLocaleTimeString("fr-FR", {
    hour: "2-digit",
    minute: "2-digit",
  });

  if (msg.role === "user") {
    return (
      <div className="flex gap-3">
        <div className="font-pixel flex h-[34px] w-[34px] flex-none items-center justify-center border border-line bg-card-deep text-[11px] text-ink">
          M
        </div>
        <div className="flex-1">
          <div className="mb-1.5 flex items-baseline gap-2">
            <span className="text-[14px] text-ink">VOUS</span>
            <span className="text-[13px] text-muted-3">{time}</span>
          </div>
          <div className="border border-line bg-card px-[14px] py-3 text-[14px] leading-snug text-ink shadow-hard-sm whitespace-pre-wrap" style={{ borderRadius: 7 }}>
            {msg.content}
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="flex gap-3">
      <div className="grid h-[34px] w-[34px] flex-none place-items-center rounded-[9px] border border-accent bg-accent-ghost">
        <div className="bg-accent" style={{ width: 10, height: 10, borderRadius: 2 }} />
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
        {(msg.meta?.plan?.length ?? 0) > 0 && (
          <PlanCard steps={msg.meta!.plan!} done={planDone} live={!!pending} />
        )}
        {(msg.meta?.tools ?? []).map((t: ToolCall, i: number) => (
          <ToolCard key={i} call={t} />
        ))}
        {notice && (
          <div className="mb-2 border border-line bg-card px-3 py-2 text-[13px] text-warn">
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
    <div className="mb-[11px] overflow-hidden border border-line bg-card shadow-hard-sm" style={{ borderRadius: 7 }}>
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
            <span className="h-2 w-2 animate-pulse border border-line bg-accent" />
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
          className="scr max-h-[200px] min-h-[60px] resize-y overflow-auto whitespace-pre-wrap border-t border-line-soft bg-card-deep px-3 py-2 text-[12.5px] leading-relaxed text-on-dark-2"
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
    <div className="ml-[46px] overflow-hidden border border-line bg-card shadow-hard" style={{ borderRadius: 7 }}>
      <div className="flex items-center gap-2 border-b border-line px-3 py-2.5">
        <span className="text-[14px] text-accent">run_shell</span>
        <span className="text-[13px] text-muted-2">· commande sensible à valider</span>
      </div>
      <div className="px-3 py-3">
        <pre className="m-0 mb-3 overflow-auto whitespace-pre-wrap border border-line bg-card-deep px-3 py-2.5 text-[12.5px] text-on-dark">
          $ {command}
        </pre>
        <div className="flex items-center gap-2">
          <button
            onClick={onApprove}
            className="flex h-[32px] items-center gap-1.5 border border-line bg-accent px-3.5 text-[13px] text-white shadow-hard-accent"
            style={{ borderRadius: 7 }}
          >
            Approuver &amp; exécuter
          </button>
          <button
            onClick={onReject}
            className="flex h-[32px] items-center border border-line bg-card px-3.5 text-[13px] text-ink-2"
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
    <div className="flex h-7 items-center gap-1.5 border border-line bg-card px-2.5 text-[13px] text-ink-2">
      {children}
    </div>
  );
}
