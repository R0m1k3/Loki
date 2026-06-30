import { useState } from "react";
import { useStore } from "../store/useStore";
import { downloadFile, type ToolCall } from "../api/client";
import { DownloadIcon } from "../components/Icon";

type TabId = "preview" | "code" | "logs";

/** Panneau droit : onglets Aperçu / Code / Logs. */
export function PreviewPanel() {
  const { previewPath, previewContent, messages, streamTools } = useStore();
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
          <button
            onClick={() => downloadFile(previewPath)}
            className="flex h-8 items-center gap-1.5 border-[3px] border-line bg-card px-2.5 text-[12px] text-accent"
            title={`Télécharger ${previewPath}`}
          >
            <DownloadIcon size={13} />
            Télécharger
          </button>
        )}
      </div>

      {/* Onglet Logs (fond sombre) */}
      {tab === "logs" ? (
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
