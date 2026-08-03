import { useStore } from "../store/useStore";
import {
  ChatIcon,
  ClockIcon,
  LokiMark,
  NodesIcon,
  PlusIcon,
  SettingsIcon,
} from "./Icon";

export type View = "chat" | "history" | "tools" | "settings";

// Les fichiers ne sont plus une vue : ils vivent dans l'onglet « Fichiers »
// du panneau droit, à côté de Code.
const NAV: { id: View; label: string; icon: React.ReactNode }[] = [
  { id: "chat", label: "Discussion", icon: <ChatIcon /> },
  { id: "history", label: "Historique", icon: <ClockIcon /> },
  { id: "tools", label: "Outils", icon: <NodesIcon /> },
];

/** Nombre de discussions récentes listées sous « Discussion ». */
const RECENT = 5;

/** Barre latérale (264 px) : marque, navigation, état Ollama, compte. */
export function Sidebar({
  active,
  onChange,
}: {
  active: View;
  onChange: (v: View) => void;
}) {
  const {
    status,
    selectedModel,
    systemStats,
    sessions,
    currentSessionId,
    streamingSessionId,
    newSession,
    openSession,
  } = useStore();
  const gpu = systemStats?.gpu ?? null;
  const recent = sessions.slice(0, RECENT);

  const startChat = () => {
    void newSession();
    onChange("chat");
  };

  const pick = (id: string) => {
    void openSession(id);
    onChange("chat");
  };

  return (
    <aside className="flex w-[264px] flex-none flex-col border-r border-line bg-panel px-3.5 py-4">
      {/* Marque */}
      <div className="flex items-center gap-2.5 px-1.5 pb-4">
        <LokiMark size={26} />
        <span className="text-[16px] font-medium tracking-tight text-ink">Loki</span>
        <span className="ml-auto rounded-full border border-line px-2 py-0.5 text-[11px] text-muted-3">
          local
        </span>
      </div>

      <div className="mx-1.5 mb-3.5 h-px bg-line" />

      <nav className="flex flex-col gap-[3px]">
        <NavItem
          id="chat"
          label={NAV[0].label}
          icon={NAV[0].icon}
          active={active === "chat"}
          onClick={() => onChange("chat")}
        />

        {/* Discussions récentes, directement sous « Discussion ». */}
        <div className="mb-1 ml-2 flex flex-col gap-px border-l border-line pl-2">
          <button
            onClick={startChat}
            className="flex items-center gap-1.5 rounded-card px-2 py-1.5 text-left text-[13px] text-accent hover:bg-accent-ghost"
          >
            <PlusIcon />
            Nouvelle discussion
          </button>
          {recent.map((s) => (
            <button
              key={s.id}
              onClick={() => pick(s.id)}
              title={s.title}
              className={`flex items-center gap-1.5 truncate rounded-card px-2 py-1.5 text-left text-[13px] ${
                s.id === currentSessionId
                  ? "bg-accent-ghost text-ink"
                  : "text-muted hover:bg-accent-ghost hover:text-ink"
              }`}
            >
              <span className="min-w-0 flex-1 truncate">{s.title}</span>
              {s.id === streamingSessionId && (
                <span className="h-1.5 w-1.5 flex-none rounded-full bg-accent" />
              )}
            </button>
          ))}
          {recent.length === 0 && (
            <span className="px-2 py-1.5 text-[12.5px] text-muted-3">
              Aucune discussion
            </span>
          )}
        </div>

        {NAV.slice(1).map((it) => (
          <NavItem
            key={it.id}
            {...it}
            active={active === it.id}
            onClick={() => onChange(it.id)}
          />
        ))}
      </nav>

      <div className="font-pixel px-2.5 pb-2 pt-5 text-[10px] text-label">
        Configuration
      </div>
      <NavItem
        id="settings"
        label="Modèle & génération"
        icon={<SettingsIcon />}
        active={active === "settings"}
        onClick={() => onChange("settings")}
      />

      <div className="flex-1" />

      {/* État Ollama + GPU */}
      <div className="rounded-card border border-line p-3">
        <div className="mb-2 flex items-center gap-2">
          <span
            className={`h-1.5 w-1.5 rounded-full ${
              status?.connected ? "bg-accent" : "bg-warn"
            }`}
          />
          <span className="text-[12.5px] text-ink-3">
            {status?.connected ? "Ollama connecté" : "Ollama déconnecté"}
          </span>
        </div>
        <div className="mb-2.5 truncate font-mono text-[12px] text-muted-3">
          {selectedModel || "aucun modèle"}
        </div>
        {gpu && (
          <>
            <div className="h-1 overflow-hidden rounded-sm bg-line-soft">
              <span
                className="block h-full bg-accent"
                style={{
                  width: `${Math.min(
                    100,
                    (gpu.vram_used_mb / Math.max(gpu.vram_total_mb, 1)) * 100
                  )}%`,
                }}
              />
            </div>
            <div className="mt-2 text-[11.5px] text-muted-3">
              VRAM {(gpu.vram_used_mb / 1000).toFixed(1)} /{" "}
              {(gpu.vram_total_mb / 1000).toFixed(1)} Go · GPU{" "}
              {Math.round(gpu.util_pct)} %
            </div>
          </>
        )}
      </div>

      {/* Compte local */}
      <div className="mt-3 flex items-center gap-2.5 rounded-card border border-line px-3 py-2.5">
        <span className="grid h-[30px] w-[30px] flex-none place-items-center rounded-full bg-line-soft text-[12px] text-ink-3">
          M
        </span>
        <span className="min-w-0">
          <span className="block text-[13.5px] text-ink">Compte local</span>
          <span className="block truncate font-mono text-[11.5px] text-muted-3">
            {status?.host || "workspace"}
          </span>
        </span>
      </div>
    </aside>
  );
}

function NavItem({
  label,
  icon,
  active,
  onClick,
}: {
  id: View;
  label: string;
  icon: React.ReactNode;
  active: boolean;
  onClick: () => void;
}) {
  return (
    <button
      onClick={onClick}
      className={`flex w-full items-center gap-2.5 rounded-card border px-2.5 py-2 text-left text-[14px] ${
        active
          ? "border-accent-line bg-accent-ghost text-ink"
          : "border-transparent text-ink-3 hover:bg-accent-ghost hover:text-ink"
      }`}
    >
      {icon}
      {label}
    </button>
  );
}
