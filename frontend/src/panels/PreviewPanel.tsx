import { useEffect, useState } from "react";
import { useStore } from "../store/useStore";
import {
  downloadFile,
  getGitDiff,
  getGitLog,
  revertCommit,
  type GitCommit,
  type ToolCall,
} from "../api/client";
import { DownloadIcon, TrashIcon } from "../components/Icon";

type TabId = "preview" | "code" | "logs" | "git";

/** Panneau droit : onglets Aperçu / Code / Logs. */
export function PreviewPanel() {
  const { previewPath, previewContent, messages, streamTools, removeFile } =
    useStore();
  const [tab, setTab] = useState<TabId>("preview");
  const [width, setWidth] = useState(() => {
    const saved = Number(window.localStorage.getItem("loki.preview.width"));
    return Number.isFinite(saved) && saved >= 300 ? saved : 452;
  });

  const updateWidth = (next: number) => {
    const bounded = Math.max(300, Math.min(next, window.innerWidth - 520));
    setWidth(bounded);
    window.localStorage.setItem("loki.preview.width", String(bounded));
  };

  const startResize = (event: React.PointerEvent<HTMLDivElement>) => {
    event.preventDefault();
    const startX = event.clientX;
    const startWidth = width;
    const onMove = (move: PointerEvent) =>
      updateWidth(startWidth + startX - move.clientX);
    const onUp = () => {
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onUp);
    };
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onUp);
  };

  const isHtml = previewPath ? /\.html?$/.test(previewPath) : false;

  // Journal d'activité : tous les appels d'outils de la session + en cours.
  const logs: ToolCall[] = [
    ...messages.flatMap((m) => m.meta?.tools ?? []),
    ...streamTools,
  ];

  return (
    <div
      className="relative flex flex-none flex-col border-l-[3px] border-line bg-panel"
      style={{ width }}
    >
      <div
        role="separator"
        aria-label="Redimensionner le panneau d'aperçu"
        aria-orientation="vertical"
        tabIndex={0}
        onPointerDown={startResize}
        onKeyDown={(event) => {
          if (event.key === "ArrowLeft") updateWidth(width + 20);
          if (event.key === "ArrowRight") updateWidth(width - 20);
        }}
        className="absolute -left-[6px] top-0 z-20 h-full w-[9px] cursor-col-resize bg-transparent hover:bg-accent/50 focus:bg-accent/50 focus:outline-none"
        title="Glisser pour redimensionner"
      />
      {/* Onglets */}
      <div className="flex h-[46px] flex-none items-center gap-1.5 border-b-[3px] border-line bg-base px-3">
        <Tab active={tab === "preview"} onClick={() => setTab("preview")}>
          Aperçu
        </Tab>
        <Tab active={tab === "code"} onClick={() => setTab("code")}>
          Code
        </Tab>
        <Tab active={tab === "logs"} onClick={() => setTab("logs")}>
          Logs
          <span className="font-pixel ml-1.5 border-2 border-line bg-accent px-1 py-0.5 text-[8px] text-white">
            {logs.length}
          </span>
        </Tab>
        <Tab active={tab === "git"} onClick={() => setTab("git")}>
          Git
        </Tab>
      </div>

      {/* URL bar */}
      <div className="flex flex-none items-center gap-2 px-3 py-2.5">
        <div className="flex h-8 flex-1 items-center gap-2 border-[3px] border-line bg-card px-[11px]">
          <span className="text-[13px]">🔒</span>
          <span className="truncate text-[13px] text-muted-2">
            {previewPath ? `workspace/${previewPath}` : "workspace/"}
          </span>
        </div>
        {previewPath && (
          <>
            <button
              onClick={() => downloadFile(previewPath, useStore.getState().currentProject())}
              className="flex h-8 items-center gap-1.5 border-[3px] border-line bg-card px-2.5 text-[12px] text-accent"
              title={`Télécharger ${previewPath}`}
            >
              <DownloadIcon size={13} />
              Télécharger
            </button>
            <button
              onClick={() => {
                if (window.confirm(`Supprimer ${previewPath} ?`)) {
                  void removeFile(previewPath);
                }
              }}
              className="flex h-8 items-center gap-1.5 border-[3px] border-line bg-card px-2.5 text-[12px] text-warn"
              title={`Supprimer ${previewPath}`}
            >
              <TrashIcon size={13} />
              Supprimer
            </button>
          </>
        )}
      </div>

      {/* Onglet Git */}
      {tab === "git" ? (
        <GitPanel />
      ) : /* Onglet Logs (fond sombre) */
      tab === "logs" ? (
        <div className="scr mx-3 mb-3 flex-1 overflow-auto border-[3px] border-line bg-card-deep p-3 text-[12.5px]">
          {logs.length === 0 ? (
            <div className="py-10 text-center text-on-dark-3">
              Aucune activité d'outil pour cette session.
            </div>
          ) : (
            logs.map((l, i) => (
              <div
                key={i}
                className="flex items-center gap-2 border-b-2 border-chrome-2 py-[7px] last:border-0"
              >
                <span
                  className={
                    l.status === "error" || l.status === "pending"
                      ? "text-accent"
                      : l.status === "running"
                      ? "text-on-dark-2"
                      : "text-ok"
                  }
                >
                  {l.status === "error"
                    ? "✕"
                    : l.status === "pending"
                    ? "⏸"
                    : l.status === "running"
                    ? "…"
                    : "✓"}
                </span>
                <span className="text-on-dark">{l.name}</span>
                <span className="flex-1 truncate text-on-dark-3">
                  {(l.args?.path as string) ??
                    (l.args?.query as string) ??
                    (l.args?.command as string) ??
                    ""}
                </span>
                <span className="text-on-dark-3">{l.summary}</span>
              </div>
            ))
          )}
        </div>
      ) : (
        /* Viewport Aperçu / Code (fond clair) */
        <div className="scr mx-3 mb-3 flex-1 overflow-auto border-[3px] border-line bg-[#faf6ef] shadow-hard">
          {!previewPath ? (
            <Empty>
              L'aperçu s'affichera ici dès que l'agent générera un fichier (clique
              aussi un fichier à gauche).
            </Empty>
          ) : tab === "code" ? (
            <pre className="m-0 whitespace-pre-wrap p-4 text-[12px] leading-relaxed text-[#2a2018]">
              {previewContent}
            </pre>
          ) : isHtml ? (
            <iframe
              title="aperçu"
              srcDoc={previewContent}
              className="h-full w-full border-0 bg-white"
              // allow-scripts : sans ça, le JS de la page générée ne s'exécute pas
              // (animations, canvas…). On garde une origine opaque (pas
              // d'allow-same-origin) pour que le script ne puisse pas atteindre Loki.
              sandbox="allow-scripts"
            />
          ) : (
            <pre className="m-0 whitespace-pre-wrap p-4 text-[12px] leading-relaxed text-[#2a2018]">
              {previewContent}
            </pre>
          )}
        </div>
      )}
    </div>
  );
}

/** Onglet Git : historique des commits, diff colorisé, retour arrière. */
function GitPanel() {
  const refreshFiles = useStore((s) => s.refreshFiles);
  const currentProject = useStore((s) => s.currentProject);
  const project = currentProject();
  const [commits, setCommits] = useState<GitCommit[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [diff, setDiff] = useState("");
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = () => getGitLog(project).then(setCommits).catch(() => {});
  useEffect(() => {
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [project]);

  const openDiff = async (hash: string) => {
    setSelected(hash);
    setDiff("Chargement…");
    setDiff((await getGitDiff(hash, project)) || "(diff vide)");
  };

  const doRevert = async (hash: string) => {
    if (!window.confirm(`Annuler le commit ${hash.slice(0, 7)} ? (crée un commit inverse)`))
      return;
    setBusy(hash);
    setError(null);
    try {
      await revertCommit(hash, project);
      await load();
      await refreshFiles();
      if (selected === hash) setSelected(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : "revert impossible");
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="scr mx-3 mb-3 flex flex-1 flex-col overflow-hidden border-[3px] border-line bg-card">
      {error && (
        <div className="border-b-2 border-line bg-warn/10 px-3 py-2 text-[12px] text-warn">
          {error}
        </div>
      )}
      {/* Liste des commits */}
      <div className="scr max-h-[45%] overflow-auto border-b-2 border-line">
        {commits.length === 0 ? (
          <div className="p-4 text-center text-[12px] text-muted-2">
            Aucun commit. L'agent commitera chaque modification de code ici.
          </div>
        ) : (
          commits.map((c) => (
            <div
              key={c.full_hash}
              className={`flex items-center gap-2 border-b border-line-soft px-3 py-2 last:border-0 ${
                selected === c.hash ? "bg-base" : ""
              }`}
            >
              <button
                onClick={() => openDiff(c.hash)}
                className="min-w-0 flex-1 text-left"
                title={c.subject}
              >
                <div className="truncate text-[13px] text-ink">{c.subject}</div>
                <div className="text-[11px] text-muted-2">
                  {c.hash} · {c.when} · {c.files_changed} fichier
                  {c.files_changed > 1 ? "s" : ""}
                </div>
              </button>
              <button
                onClick={() => doRevert(c.hash)}
                disabled={busy === c.hash}
                className="flex-none border-2 border-line bg-card px-2 py-1 text-[11px] text-warn disabled:opacity-40"
                title="Annuler ce commit (revert)"
              >
                {busy === c.hash ? "…" : "↶ Annuler"}
              </button>
            </div>
          ))
        )}
      </div>
      {/* Diff du commit sélectionné */}
      <div className="scr flex-1 overflow-auto bg-card-deep p-3">
        {selected ? (
          <DiffView diff={diff} />
        ) : (
          <div className="py-8 text-center text-[12px] text-on-dark-3">
            Clique un commit pour voir ses modifications.
          </div>
        )}
      </div>
    </div>
  );
}

/** Rendu coloré d'un diff unifié. */
function DiffView({ diff }: { diff: string }) {
  return (
    <pre className="m-0 whitespace-pre-wrap text-[11.5px] leading-relaxed">
      {diff.split("\n").map((line, i) => {
        let color = "text-on-dark-2";
        if (line.startsWith("+") && !line.startsWith("+++")) color = "text-ok";
        else if (line.startsWith("-") && !line.startsWith("---")) color = "text-accent";
        else if (line.startsWith("@@")) color = "text-info";
        else if (/^(commit|Author|Date|diff|index)/.test(line))
          color = "text-on-dark-3";
        return (
          <div key={i} className={color}>
            {line || " "}
          </div>
        );
      })}
    </pre>
  );
}

function Empty({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex h-full flex-col items-center justify-center gap-2 p-8 text-center">
      <div className="font-pixel text-[10px] text-[#a98b63]">AUCUN APERÇU</div>
      <div className="max-w-[240px] text-[13px] text-[#8a7a66]">{children}</div>
    </div>
  );
}

function Tab({
  children,
  active,
  onClick,
}: {
  children: React.ReactNode;
  active?: boolean;
  onClick?: () => void;
}) {
  return (
    <button
      onClick={onClick}
      className={`flex h-[30px] items-center border-[3px] border-line px-[13px] text-[13px] ${
        active ? "bg-card-deep text-white" : "bg-card text-muted"
      }`}
    >
      {children}
    </button>
  );
}
