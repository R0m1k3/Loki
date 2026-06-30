import { useEffect } from "react";
import { downloadFile, type FileNode } from "../api/client";
import { DownloadIcon } from "../components/Icon";
import { useStore } from "../store/useStore";

export function HistoryView({ onOpen }: { onOpen: () => void }) {
  const { sessions, refreshSessions, openSession, removeSession } = useStore();

  useEffect(() => {
    refreshSessions();
  }, [refreshSessions]);

  return (
    <Page title="Historique" subtitle="Toutes les conversations enregistrées localement.">
      <div className="grid grid-cols-1 gap-3 lg:grid-cols-2 xl:grid-cols-3">
        {sessions.map((session) => (
          <div key={session.id} className="border-[3px] border-line bg-card p-4 shadow-hard">
            <div className="truncate text-[15px] font-semibold text-ink">{session.title}</div>
            <div className="mt-1 text-[12px] text-muted-2">
              {session.message_count ?? 0} message(s) · {new Date(session.updated_at * 1000).toLocaleString("fr-FR")}
            </div>
            <div className="mt-4 flex gap-2">
              <button
                onClick={async () => {
                  await openSession(session.id);
                  onOpen();
                }}
                className="border-2 border-line bg-accent px-3 py-1.5 text-[12px] font-bold text-white"
              >
                Ouvrir
              </button>
              <button
                onClick={() => removeSession(session.id)}
                className="border-2 border-line bg-base px-3 py-1.5 text-[12px] text-warn"
              >
                Supprimer
              </button>
            </div>
          </div>
        ))}
        {sessions.length === 0 && <Empty text="Aucune conversation enregistrée." />}
      </div>
    </Page>
  );
}

export function FilesView() {
  const { fileTree, refreshFiles } = useStore();

  useEffect(() => {
    refreshFiles();
  }, [refreshFiles]);

  return (
    <Page title="Fichiers" subtitle="Workspace créé et modifié par l’agent.">
      <div className="border-[3px] border-line bg-panel p-3 shadow-hard">
        {fileTree.length ? <WorkspaceTree nodes={fileTree} depth={0} /> : <Empty text="Le workspace est vide." />}
      </div>
    </Page>
  );
}

function WorkspaceTree({ nodes, depth }: { nodes: FileNode[]; depth: number }) {
  const { openPreview, previewPath } = useStore();
  return (
    <>
      {nodes.map((node) => (
        <div key={node.path}>
          <div
            className={`mb-1 flex items-center gap-2 border-2 px-3 py-2 ${
              node.path === previewPath
                ? "border-accent bg-card-deep text-white"
                : "border-line bg-card text-ink-2"
            }`}
            style={{ marginLeft: depth * 18 }}
          >
            <button
              onClick={() => node.type === "file" && openPreview(node.path)}
              className="flex min-w-0 flex-1 items-center gap-2 text-left"
            >
              <span>{node.type === "dir" ? "▾" : "▪"}</span>
              <span className="truncate">{node.name}</span>
              {node.size !== undefined && (
                <span className="ml-auto text-[11px] text-muted-2">{formatSize(node.size)}</span>
              )}
            </button>
            {node.type === "file" && (
              <button
                onClick={() => downloadFile(node.path)}
                className="flex items-center gap-1 border-2 border-line bg-base px-2 py-1 text-[11px] text-accent"
                title={`Télécharger ${node.name}`}
              >
                <DownloadIcon size={12} /> Télécharger
              </button>
            )}
          </div>
          {node.children && <WorkspaceTree nodes={node.children} depth={depth + 1} />}
        </div>
      ))}
    </>
  );
}

export function ToolsView({ onSettings }: { onSettings: () => void }) {
  const { config, availableTools, refreshConfig, selectedModel } = useStore();

  useEffect(() => {
    refreshConfig();
  }, [refreshConfig, selectedModel]);

  return (
    <Page title="Outils" subtitle={`Capacités proposées à ${selectedModel || "l’agent"}.`}>
      <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
        {availableTools.map((name) => {
          const enabled = config?.tools[name] ?? false;
          return (
            <div key={name} className="border-[3px] border-line bg-card p-4 shadow-hard">
              <div className="flex items-center justify-between gap-3">
                <span className="font-mono text-[14px] text-ink">{name}</span>
                <span className={`border-2 border-line px-2 py-1 text-[10px] ${enabled ? "bg-ok text-white" : "bg-base text-muted-2"}`}>
                  {enabled ? "ACTIF" : "INACTIF"}
                </span>
              </div>
            </div>
          );
        })}
      </div>
      <button
        onClick={onSettings}
        className="mt-5 border-[3px] border-line bg-accent px-4 py-2 text-[13px] font-bold text-white shadow-hard"
      >
        Configurer les outils
      </button>
    </Page>
  );
}

function Page({ title, subtitle, children }: { title: string; subtitle: string; children: React.ReactNode }) {
  return (
    <div className="scr min-w-0 flex-1 overflow-auto bg-base p-7">
      <div className="mx-auto max-w-[1100px]">
        <h1 className="m-0 text-xl font-bold text-ink">{title}</h1>
        <p className="mb-6 mt-1 text-[13px] text-muted-2">{subtitle}</p>
        {children}
      </div>
    </div>
  );
}

function Empty({ text }: { text: string }) {
  return <div className="border-2 border-line bg-card p-8 text-center text-muted-2">{text}</div>;
}

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} o`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} Ko`;
  return `${(bytes / 1024 / 1024).toFixed(1)} Mo`;
}
