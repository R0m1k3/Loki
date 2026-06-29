import { PlusIcon } from "../components/Icon";

const sessions = [
  { title: "Nouvelle session", meta: "à l'instant · 0 message", active: true },
];

/** Panneau gauche : historique des sessions + arborescence de fichiers. */
export function LeftPanel() {
  return (
    <div className="flex w-[268px] flex-none flex-col border-r border-line-soft bg-panel">
      {/* Historique */}
      <div className="flex items-center justify-between px-4 pb-2.5 pt-4">
        <span className="text-[11px] font-bold uppercase tracking-[.07em] text-label">
          Historique
        </span>
        <span className="flex cursor-pointer items-center gap-1 text-xs font-semibold text-accent">
          <PlusIcon />
          Nouvelle
        </span>
      </div>

      <div className="flex flex-col gap-0.5 px-2.5">
        {sessions.map((s, i) => (
          <div
            key={i}
            className={`relative cursor-pointer rounded-[9px] px-3 py-[9px] ${
              s.active ? "bg-[#252019]" : ""
            }`}
          >
            {s.active && (
              <div className="absolute bottom-[9px] left-0 top-[9px] w-[3px] rounded bg-accent" />
            )}
            <div className="truncate text-[13px] font-semibold text-ink">
              {s.title}
            </div>
            <div className="mt-[3px] font-mono text-[10.5px] text-muted-3">
              {s.meta}
            </div>
          </div>
        ))}
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
