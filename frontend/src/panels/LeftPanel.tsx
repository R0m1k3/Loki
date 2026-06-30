import { useEffect } from "react";
import { useStore } from "../store/useStore";
import { DownloadIcon, PlusIcon } from "../components/Icon";
import { downloadFile, type FileNode } from "../api/client";

/** Panneau gauche : historique des sessions + arborescence de fichiers. */
export function LeftPanel() {
  const {
    sessions,
    currentSessionId,
    refreshSessions,
    refreshFiles,
    fileTree,
    newSession,
    openSession,
    removeSession,
  } = useStore();

  useEffect(() => {
    refreshSessions();
    refreshFiles();
  }, [refreshSessions, refreshFiles]);

  return (
    <div className="flex w-[280px] flex-none flex-col border-r-[3px] border-line bg-panel">
      {/* Historique */}
      <div className="flex items-center justify-between px-4 pb-2.5 pt-4">
        <span className="font-pixel text-[10px] text-label">HISTORIQUE</span>
        <button
          onClick={newSession}
          className="flex cursor-pointer items-center gap-1 text-[13px] text-accent"
        >
          <PlusIcon />
          Nouvelle
        </button>
      </div>

      <div className="scr flex max-h-[45%] flex-col gap-2 overflow-auto px-3 py-1">
        {sessions.length === 0 && (
          <div className="px-1 py-2 text-xs text-muted-2">
            Aucune session. Lance une conversation ci-dessous.
          </div>
        )}
        {sessions.map((s) => {
          const active = s.id === currentSessionId;
          return (
            <div
              key={s.id}
              onClick={() => openSession(s.id)}
              className={`group cursor-pointer border-[3px] border-line px-3 py-2.5 ${
                active ? "bg-card-deep shadow-hard-accent" : "bg-card"
              }`}
              style={{ borderRadius: 7 }}
            >
              <div className="flex items-center gap-1">
                <div
                  className={`flex-1 truncate text-[14px] leading-tight ${
                    active ? "text-white" : "text-ink-2"
                  }`}
                >
                  {s.title}
                </div>
                <button
                  onClick={(e) => {
                    e.stopPropagation();
                    removeSession(s.id);
                  }}
                  className={`hidden group-hover:block ${
                    active ? "text-on-dark-2 hover:text-accent" : "text-muted-3 hover:text-warn"
                  }`}
                  title="Supprimer"
                >
                  ×
                </button>
              </div>
              <div className="mt-1 text-[13px] text-muted-3">
                {relTime(s.updated_at)} · {s.message_count ?? 0} message
                {(s.message_count ?? 0) > 1 ? "s" : ""}
              </div>
            </div>
          );
        })}
      </div>

      <div className="mx-0 my-4 h-[3px] bg-line" />

      {/* Fichiers */}
      <div className="flex items-center justify-between px-4 pb-2">
        <span className="font-pixel text-[10px] text-label">FICHIERS</span>
        <span className="text-[13px] text-muted-3">workspace/</span>
      </div>

      <div className="scr flex-1 overflow-auto px-3 pb-3.5 text-[14px]">
        {fileTree.length === 0 ? (
          <div className="px-2 py-4 text-center text-muted-2">
            Aucun fichier pour l'instant.
            <br />
            L'agent créera des fichiers ici.
          </div>
        ) : (
          <FileTree nodes={fileTree} depth={0} />
        )}
      </div>
    </div>
  );
}

function FileTree({ nodes, depth }: { nodes: FileNode[]; depth: number }) {
  const { openPreview, previewPath } = useStore();
  return (
    <>
      {nodes.map((n) => {
        const active = n.type === "file" && n.path === previewPath;
        return (
          <div key={n.path}>
            <div
              onClick={() => n.type === "file" && openPreview(n.path)}
              className={`group mb-[3px] flex items-center gap-2 px-2 py-[5px] ${
                n.type === "file"
                  ? active
                    ? "cursor-pointer border-2 border-line bg-card-deep text-white"
                    : "cursor-pointer text-ink-2 hover:bg-base"
                  : "text-muted"
              }`}
              style={{ paddingLeft: 8 + depth * 14 }}
            >
              {n.type === "dir" ? (
                <span className="text-muted-2">▾</span>
              ) : (
                <span
                  className={`h-2 w-2 border-2 ${
                    active ? "border-white bg-accent" : "border-line bg-muted-2"
                  }`}
                />
              )}
              <span className="min-w-0 flex-1 truncate">{n.name}</span>
              {n.type === "file" && (
                <button
                  onClick={(event) => {
                    event.stopPropagation();
                    downloadFile(n.path);
                  }}
                  className="hidden flex-none text-muted-2 hover:text-accent group-hover:block"
                  title={`Télécharger ${n.name}`}
                >
                  <DownloadIcon size={13} />
                </button>
              )}
            </div>
            {n.children && <FileTree nodes={n.children} depth={depth + 1} />}
          </div>
        );
      })}
    </>
  );
}

function relTime(ts: number): string {
  const diff = Date.now() / 1000 - ts;
  if (diff < 60) return "à l'instant";
  if (diff < 3600) return `il y a ${Math.floor(diff / 60)} min`;
  if (diff < 86400) return `il y a ${Math.floor(diff / 3600)} h`;
  return `il y a ${Math.floor(diff / 86400)} j`;
}
