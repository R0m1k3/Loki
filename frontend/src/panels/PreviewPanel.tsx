import { useState } from "react";
import { useStore } from "../store/useStore";
import type { ToolCall } from "../api/client";

type TabId = "preview" | "code" | "logs";

/** Panneau droit : onglets Aperçu / Code / Logs. */
export function PreviewPanel() {
  const { previewPath, previewContent, messages, streamTools } = useStore();
  const [tab, setTab] = useState<TabId>("preview");

  const isHtml = previewPath ? /\.html?$/.test(previewPath) : false;

  // Journal d'activité : tous les appels d'outils de la session + en cours.
  const logs: ToolCall[] = [
    ...messages.flatMap((m) => m.meta?.tools ?? []),
    ...streamTools,
  ];

  return (
    <div className="flex w-[452px] flex-none flex-col border-l border-line-soft bg-panel">
      {/* Onglets */}
      <div className="flex h-[42px] flex-none items-center gap-1 border-b border-line-soft px-3.5 pr-2">
        <div className="flex gap-0.5">
          <Tab active={tab === "preview"} onClick={() => setTab("preview")}>
            Aperçu
          </Tab>
          <Tab active={tab === "code"} onClick={() => setTab("code")}>
            Code
          </Tab>
          <Tab active={tab === "logs"} onClick={() => setTab("logs")}>
            Logs
            <span className="ml-1.5 rounded-[5px] bg-line-strong px-1.5 py-px font-mono text-[10px] text-muted">
              {logs.length}
            </span>
          </Tab>
        </div>
      </div>

      {/* URL bar */}
      <div className="flex flex-none items-center gap-2 px-3.5 py-[9px]">
        <div className="flex h-[30px] flex-1 items-center gap-2 rounded-lg border border-line bg-sunken px-[11px]">
          <span className="truncate font-mono text-[11.5px] text-muted-3">
            {previewPath ? `workspace/${previewPath}` : "workspace/"}
          </span>
        </div>
      </div>

      {/* Onglet Logs (fond sombre) */}
      {tab === "logs" ? (
        <div className="scr mx-3.5 mb-3.5 flex-1 overflow-auto rounded-xl border border-line bg-sunken p-3 font-mono text-[11.5px]">
          {logs.length === 0 ? (
            <div className="py-10 text-center text-muted-3">
              Aucune activité d'outil pour cette session.
            </div>
          ) : (
            logs.map((l, i) => (
              <div
                key={i}
                className="flex items-center gap-2 border-b border-line-soft py-[7px] last:border-0"
              >
                <span
                  className={
                    l.status === "error"
                      ? "text-warn"
                      : l.status === "pending"
                      ? "text-warn"
                      : l.status === "running"
                      ? "text-muted"
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
                <span className="text-ink-2">{l.name}</span>
                <span className="flex-1 truncate text-muted-3">
                  {(l.args?.path as string) ??
                    (l.args?.query as string) ??
                    (l.args?.command as string) ??
                    ""}
                </span>
                <span className="text-muted-3">{l.summary}</span>
              </div>
            ))
          )}
        </div>
      ) : (
        /* Viewport Aperçu / Code (fond clair) */
        <div className="scr mx-3.5 mb-3.5 flex-1 overflow-auto rounded-xl border border-line bg-[#faf6ef]">
          {!previewPath ? (
          <Empty>
            L'aperçu s'affichera ici dès que l'agent générera un fichier (clique
            aussi un fichier à gauche).
          </Empty>
        ) : tab === "code" ? (
          <pre className="m-0 whitespace-pre-wrap p-4 font-mono text-[11.5px] leading-relaxed text-[#2a2018]">
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
            <pre className="m-0 whitespace-pre-wrap p-4 font-mono text-[11.5px] leading-relaxed text-[#2a2018]">
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
      <div className="font-mono text-[11px] text-[#a98b63]">[ aucun aperçu ]</div>
      <div className="max-w-[240px] text-xs text-[#8a7a66]">{children}</div>
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
      className={`flex h-7 items-center rounded-lg px-[13px] text-[12.5px] ${
        active
          ? "bg-[#252019] font-semibold text-ink"
          : "font-medium text-muted-2"
      }`}
    >
      {children}
    </button>
  );
}
