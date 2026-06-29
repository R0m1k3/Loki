/** Panneau droit : onglets Aperçu / Code / Logs + zone d'aperçu. */
export function PreviewPanel() {
  return (
    <div className="flex w-[452px] flex-none flex-col border-l border-line-soft bg-panel">
      {/* Onglets */}
      <div className="flex h-[42px] flex-none items-center gap-1 border-b border-line-soft px-3.5 pr-2">
        <div className="flex gap-0.5">
          <Tab active>Aperçu</Tab>
          <Tab>Code</Tab>
          <Tab>
            Logs
            <span className="ml-1.5 rounded-[5px] bg-line-strong px-1.5 py-px font-mono text-[10px] text-muted">
              0
            </span>
          </Tab>
        </div>
      </div>

      {/* URL bar */}
      <div className="flex flex-none items-center gap-2 px-3.5 py-[9px]">
        <div className="flex h-[30px] flex-1 items-center gap-2 rounded-lg border border-line bg-sunken px-[11px]">
          <span className="font-mono text-[11.5px] text-muted-3">
            workspace/
          </span>
        </div>
      </div>

      {/* Viewport */}
      <div className="scr mx-3.5 mb-3.5 flex-1 overflow-auto rounded-xl border border-line bg-[#faf6ef]">
        <div className="flex h-full flex-col items-center justify-center gap-2 p-8 text-center">
          <div className="font-mono text-[11px] text-[#a98b63]">
            [ aucun aperçu ]
          </div>
          <div className="max-w-[240px] text-xs text-[#8a7a66]">
            L'aperçu HTML s'affichera ici dès que l'agent générera une page.
          </div>
        </div>
      </div>
    </div>
  );
}

function Tab({
  children,
  active,
}: {
  children: React.ReactNode;
  active?: boolean;
}) {
  return (
    <div
      className={`flex h-7 items-center rounded-lg px-[13px] text-[12.5px] ${
        active
          ? "bg-[#252019] font-semibold text-ink"
          : "font-medium text-muted-2"
      }`}
    >
      {children}
    </div>
  );
}
