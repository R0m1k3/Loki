import { useStore } from "../store/useStore";
import { ClipIcon, LokiMark, SendIcon } from "../components/Icon";

/** Panneau central : barre de contexte, fil de conversation, composer. */
export function ChatPanel() {
  const selectedModel = useStore((s) => s.selectedModel);

  return (
    <div className="flex min-w-0 flex-1 flex-col bg-base">
      {/* Barre de contexte */}
      <div className="flex h-[42px] flex-none items-center gap-2 border-b border-line-soft px-[18px]">
        <Chip>Invite système</Chip>
        <Chip>
          <span className="h-1.5 w-1.5 rounded-full bg-ok" />0 outil actif
        </Chip>
        <Chip>
          Température <b className="font-semibold text-[#e7ddcd]">0.7</b>
        </Chip>
        <Chip>
          Contexte
          <span className="h-[5px] w-[46px] overflow-hidden rounded bg-line-strong">
            <span className="block h-full w-0 bg-accent" />
          </span>
          <span className="font-mono text-[10.5px] text-muted-2">0/128K</span>
        </Chip>
        <div className="flex-1" />
        <span className="font-mono text-[11px] text-muted-4">≈ 0 jeton</span>
      </div>

      {/* Messages (vide pour l'instant — phase 3) */}
      <div className="scr flex-1 overflow-auto px-7 py-6">
        <div className="mx-auto flex max-w-[680px] flex-col items-center justify-center gap-4 pt-24 text-center">
          <LokiMark size={44} />
          <div className="text-lg font-semibold text-ink">
            Prêt à travailler.
          </div>
          <div className="max-w-[360px] text-sm leading-relaxed text-muted">
            Décris une tâche à l'agent. Il pourra lire et écrire des fichiers
            dans le workspace, et te montrer un aperçu en direct.
          </div>
        </div>
      </div>

      {/* Composer */}
      <div className="flex-none border-t border-line-soft px-7 pb-[18px] pt-3.5">
        <div className="mx-auto max-w-[680px]">
          <div className="rounded-[14px] border border-line-strong bg-card-soft p-[13px]">
            <textarea
              rows={1}
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
              <button className="flex h-[34px] items-center gap-1.5 rounded-[9px] bg-accent px-4 text-[13px] font-bold text-[#1f1b16]">
                Envoyer
                <SendIcon />
              </button>
            </div>
          </div>
        </div>
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
