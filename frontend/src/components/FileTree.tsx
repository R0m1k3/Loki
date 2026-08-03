import { useStore } from "../store/useStore";
import { downloadFile, type FileNode } from "../api/client";
import { DownloadIcon, TrashIcon } from "./Icon";

/** Arborescence du workspace : ouvrir dans l'aperçu, télécharger, supprimer. */
export function FileTree({
  nodes,
  depth = 0,
}: {
  nodes: FileNode[];
  depth?: number;
}) {
  const { openPreview, previewPath, removeFile, currentProject } = useStore();

  const confirmDelete = (n: FileNode) => {
    const msg =
      n.type === "dir"
        ? `Supprimer le dossier ${n.path} et tout son contenu ?`
        : `Supprimer ${n.path} ?`;
    if (window.confirm(msg)) void removeFile(n.path);
  };

  return (
    <>
      {nodes.map((n) => {
        const active = n.type === "file" && n.path === previewPath;
        return (
          <div key={n.path}>
            <div
              onClick={() => n.type === "file" && openPreview(n.path)}
              className={`group mb-[2px] flex items-center gap-2 rounded-card px-2 py-[5px] text-[13.5px] ${
                n.type === "file"
                  ? active
                    ? "cursor-pointer border border-accent-line bg-accent-ghost text-ink"
                    : "cursor-pointer border border-transparent text-ink-3 hover:bg-accent-ghost"
                  : "text-muted-2"
              }`}
              style={{ paddingLeft: 8 + depth * 14 }}
            >
              {n.type === "dir" ? (
                <span className="text-muted-3">▾</span>
              ) : (
                <span
                  className={`h-1.5 w-1.5 rounded-sm ${
                    active ? "bg-accent" : "bg-muted-3"
                  }`}
                />
              )}
              <span className="min-w-0 flex-1 truncate">{n.name}</span>
              {n.type === "file" && (
                <button
                  onClick={(event) => {
                    event.stopPropagation();
                    downloadFile(n.path, currentProject());
                  }}
                  className="hidden flex-none text-muted-3 hover:text-accent group-hover:block"
                  title={`Télécharger ${n.name}`}
                >
                  <DownloadIcon size={13} />
                </button>
              )}
              <button
                onClick={(event) => {
                  event.stopPropagation();
                  confirmDelete(n);
                }}
                className="hidden flex-none text-muted-3 hover:text-warn group-hover:block"
                title={`Supprimer ${n.name}`}
              >
                <TrashIcon size={13} />
              </button>
            </div>
            {n.children && <FileTree nodes={n.children} depth={depth + 1} />}
          </div>
        );
      })}
    </>
  );
}
