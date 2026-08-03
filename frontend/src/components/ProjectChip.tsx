import { useEffect, useRef, useState } from "react";
import { useStore } from "../store/useStore";
import { createProject } from "../api/client";

/** Sélecteur de projet du composer : 📁 <projet> + menu (liste, création). */
export function ProjectChip() {
  const { projects, refreshProjects, currentProject, setProject } = useStore();
  const [open, setOpen] = useState(false);
  const [creating, setCreating] = useState(false);
  const [draft, setDraft] = useState("");
  const [error, setError] = useState<string | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);

  const active = currentProject();

  useEffect(() => {
    if (open) void refreshProjects();
  }, [open, refreshProjects]);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) {
        setOpen(false);
        setCreating(false);
        setError(null);
      }
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open]);

  const choose = async (name: string | null) => {
    setOpen(false);
    await setProject(name);
  };

  const create = async () => {
    const name = draft.trim();
    if (!name) return;
    try {
      await createProject(name);
      setCreating(false);
      setDraft("");
      await choose(name);
    } catch (err) {
      setError(err instanceof Error ? err.message : "création impossible");
    }
  };

  return (
    <div ref={rootRef} className="relative">
      <button
        onClick={() => setOpen((o) => !o)}
        className="flex h-8 items-center gap-1.5 border border-line bg-base px-2.5 text-[12px] text-ink-2"
        title="Projet de travail de cette session"
      >
        📁 <span className="max-w-[140px] truncate">{active ?? "workspace"}</span>
      </button>

      {open && (
        <div
          className="absolute bottom-[calc(100%+6px)] left-0 z-30 w-[240px] border border-line bg-card shadow-hard"
          style={{ borderRadius: 7 }}
        >
          <button
            onClick={() => choose(null)}
            className={`block w-full px-3 py-2 text-left text-[13px] text-ink-2 hover:bg-base ${
              active === null ? "bg-base font-bold" : ""
            }`}
          >
            workspace (racine)
          </button>
          {projects.map((p) => (
            <button
              key={p.name}
              onClick={() => choose(p.name)}
              className={`block w-full border-t border-line-soft px-3 py-2 text-left text-[13px] text-ink-2 hover:bg-base ${
                active === p.name ? "bg-base font-bold" : ""
              }`}
            >
              📁 {p.name}
              <span className="ml-1.5 text-[11px] text-muted-2">
                {p.files} fichier{p.files > 1 ? "s" : ""}
              </span>
            </button>
          ))}
          <div className="border-t border-line-soft p-2">
            {creating ? (
              <input
                autoFocus
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") void create();
                  if (e.key === "Escape") {
                    setCreating(false);
                    setError(null);
                  }
                }}
                placeholder="nom-du-projet"
                className="w-full border border-line bg-base px-2 py-1 text-[12px] text-ink outline-none"
              />
            ) : (
              <button
                onClick={() => setCreating(true)}
                className="w-full text-left text-[13px] font-bold text-accent"
              >
                + Nouveau projet
              </button>
            )}
            {error && <div className="mt-1 text-[11px] text-warn">{error}</div>}
          </div>
        </div>
      )}
    </div>
  );
}
