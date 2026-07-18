import { useEffect, useRef, useState } from "react";
import { useStore } from "../store/useStore";
import { ChevronDown, PlusIcon, TrashIcon } from "./Icon";
import { relTime } from "../lib/time";

/** Sélecteur de session de la barre supérieure : titre courant + menu déroulant
 *  (nouvelle session, liste, renommage inline, suppression). */
export function SessionMenu() {
  const {
    sessions,
    currentSessionId,
    streamingSessionId,
    newSession,
    openSession,
    removeSession,
    renameSession,
  } = useStore();
  const [open, setOpen] = useState(false);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [draft, setDraft] = useState("");
  const rootRef = useRef<HTMLDivElement>(null);

  const current = sessions.find((s) => s.id === currentSessionId);

  // Fermeture au clic extérieur + Escape.
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) {
        setOpen(false);
        setEditingId(null);
      }
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setOpen(false);
        setEditingId(null);
      }
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  const commitRename = async (id: string) => {
    const title = draft.trim();
    setEditingId(null);
    if (title) await renameSession(id, title);
  };

  return (
    <div ref={rootRef} className="relative">
      <button
        onClick={() => setOpen((o) => !o)}
        className="flex cursor-pointer items-center gap-2 text-[13px] text-on-dark hover:text-white"
        title="Sessions"
      >
        <span className="max-w-[220px] truncate">
          {current?.title ?? "Nouvelle session"}
        </span>
        <ChevronDown className="text-on-dark-3" />
      </button>

      {open && (
        <div
          className="absolute left-0 top-[calc(100%+9px)] z-30 w-[300px] border-[3px] border-line bg-chrome-2 shadow-hard"
          style={{ borderRadius: 7 }}
        >
          <button
            onClick={async () => {
              setOpen(false);
              await newSession();
            }}
            className="flex w-full cursor-pointer items-center gap-2 border-b-[3px] border-chrome-3 px-3 py-2.5 text-[13px] font-bold text-accent hover:bg-chrome-3"
          >
            <PlusIcon />
            Nouvelle session
          </button>

          <div className="scr max-h-[320px] overflow-auto">
            {sessions.length === 0 && (
              <div className="px-3 py-4 text-center text-[12px] text-on-dark-3">
                Aucune session enregistrée.
              </div>
            )}
            {sessions.map((s) => {
              const active = s.id === currentSessionId;
              const working = s.id === streamingSessionId;
              return (
                <div
                  key={s.id}
                  onClick={() => {
                    if (editingId === s.id) return;
                    setOpen(false);
                    void openSession(s.id);
                  }}
                  className={`group cursor-pointer border-b-2 border-chrome-3 px-3 py-2 last:border-b-0 ${
                    active ? "bg-chrome-3" : "hover:bg-chrome-3"
                  }`}
                >
                  <div className="flex items-center gap-1.5">
                    {editingId === s.id ? (
                      <input
                        autoFocus
                        value={draft}
                        onChange={(e) => setDraft(e.target.value)}
                        onClick={(e) => e.stopPropagation()}
                        onKeyDown={(e) => {
                          if (e.key === "Enter") void commitRename(s.id);
                          if (e.key === "Escape") {
                            e.stopPropagation();
                            setEditingId(null);
                          }
                        }}
                        onBlur={() => setEditingId(null)}
                        className="min-w-0 flex-1 border-2 border-line bg-card-deep px-1.5 py-0.5 text-[13px] text-white outline-none"
                      />
                    ) : (
                      <span
                        className={`min-w-0 flex-1 truncate text-[13px] ${
                          active ? "text-white" : "text-on-dark"
                        }`}
                      >
                        {s.title}
                      </span>
                    )}
                    {working && (
                      <span
                        className="flex-none border-2 border-line bg-accent px-1 py-0.5 text-[9px] leading-none text-white"
                        title="Session en cours de travail"
                      >
                        EN COURS
                      </span>
                    )}
                    <button
                      onClick={(e) => {
                        e.stopPropagation();
                        setEditingId(s.id);
                        setDraft(s.title);
                      }}
                      className="hidden flex-none text-[11px] text-on-dark-3 hover:text-white group-hover:block"
                      title="Renommer la session"
                    >
                      ✎
                    </button>
                    <button
                      onClick={(e) => {
                        e.stopPropagation();
                        if (window.confirm("Supprimer cette session ?")) {
                          void removeSession(s.id);
                        }
                      }}
                      className="hidden flex-none text-on-dark-3 hover:text-accent group-hover:block"
                      title="Supprimer la session"
                    >
                      <TrashIcon size={12} />
                    </button>
                  </div>
                  <div className="mt-0.5 text-[11px] text-on-dark-3">
                    {relTime(s.updated_at)} · {s.message_count ?? 0} message
                    {(s.message_count ?? 0) > 1 ? "s" : ""}
                  </div>
                </div>
              );
            })}
          </div>
        </div>
      )}
    </div>
  );
}
