import { useEffect } from "react";
import { useStore } from "../store/useStore";
import { PlusIcon } from "../components/Icon";

/** Panneau gauche : historique des sessions + arborescence de fichiers. */
export function LeftPanel() {
  const {
    sessions,
    currentSessionId,
    refreshSessions,
    newSession,
    openSession,
    removeSession,
  } = useStore();

  useEffect(() => {
    refreshSessions();
  }, [refreshSessions]);

  return (
    <div className="flex w-[268px] flex-none flex-col border-r border-line-soft bg-panel">
      {/* Historique */}
      <div className="flex items-center justify-between px-4 pb-2.5 pt-4">
        <span className="text-[11px] font-bold uppercase tracking-[.07em] text-label">
          Historique
        </span>
        <button
          onClick={newSession}
          className="flex cursor-pointer items-center gap-1 text-xs font-semibold text-accent"
        >
          <PlusIcon />
          Nouvelle
        </button>
      </div>

      <div className="scr flex max-h-[45%] flex-col gap-0.5 overflow-auto px-2.5">
        {sessions.length === 0 && (
          <div className="px-3 py-3 text-xs text-muted-3">
            Aucune session. Lance une conversation ci-dessous.
          </div>
        )}
        {sessions.map((s) => {
          const active = s.id === currentSessionId;
          return (
            <div
              key={s.id}
              onClick={() => openSession(s.id)}
              className={`group relative cursor-pointer rounded-[9px] px-3 py-[9px] ${
                active ? "bg-[#252019]" : "hover:bg-[#221e18]"
              }`}
            >
              {active && (
                <div className="absolute bottom-[9px] left-0 top-[9px] w-[3px] rounded bg-accent" />
              )}
              <div className="flex items-center gap-1">
                <div
                  className={`flex-1 truncate text-[13px] ${
                    active ? "font-semibold text-ink" : "font-medium text-ink-3"
                  }`}
                >
                  {s.title}
                </div>
                <button
                  onClick={(e) => {
                    e.stopPropagation();
                    removeSession(s.id);
                  }}
                  className="hidden text-muted-3 hover:text-warn group-hover:block"
                  title="Supprimer"
                >
                  ×
                </button>
              </div>
              <div className="mt-[3px] font-mono text-[10.5px] text-muted-3">
                {relTime(s.updated_at)} · {s.message_count ?? 0} message
                {(s.message_count ?? 0) > 1 ? "s" : ""}
              </div>
            </div>
          );
        })}
      </div>

      <div className="mx-4 my-3.5 h-px bg-line-soft" />

      {/* Fichiers */}
      <div className="flex items-center justify-between px-4 pb-2">
        <span className="text-[11px] font-bold uppercase tracking-[.07em] text-label">
          Fichiers
        </span>
        <span className="font-mono text-[10.5px] text-muted-4">workspace/</span>
      </div>

      <div className="scr flex-1 overflow-auto px-2.5 pb-3.5 font-mono text-xs">
        <div className="px-2 py-4 text-center text-muted-3">
          Aucun fichier pour l'instant.
          <br />
          L'agent créera des fichiers ici.
        </div>
      </div>
    </div>
  );
}

function relTime(ts: number): string {
  const diff = Date.now() / 1000 - ts;
  if (diff < 60) return "à l'instant";
  if (diff < 3600) return `il y a ${Math.floor(diff / 60)} min`;
  if (diff < 86400) return `il y a ${Math.floor(diff / 3600)} h`;
  return `il y a ${Math.floor(diff / 86400)} j`;
}
