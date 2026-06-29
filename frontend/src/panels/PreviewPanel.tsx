import { useState } from "react";
import { useStore } from "../store/useStore";

type TabId = "preview" | "code";

/** Panneau droit : onglets Aperçu / Code + rendu du fichier sélectionné. */
export function PreviewPanel() {
  const { previewPath, previewContent } = useStore();
  const [tab, setTab] = useState<TabId>("preview");

  const isHtml = previewPath ? /\.html?$/.test(previewPath) : false;

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

      {/* Viewport */}
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
            sandbox="allow-same-origin"
          />
        ) : (
          <pre className="m-0 whitespace-pre-wrap p-4 font-mono text-[11.5px] leading-relaxed text-[#2a2018]">
            {previewContent}
          </pre>
        )}
      </div>
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
