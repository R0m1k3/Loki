import {
  ChatIcon,
  ClockIcon,
  FilesIcon,
  NodesIcon,
  SettingsIcon,
} from "./Icon";

export type View = "chat" | "history" | "files" | "tools" | "settings";

const items: { id: View; icon: React.ReactNode }[] = [
  { id: "chat", icon: <ChatIcon /> },
  { id: "history", icon: <ClockIcon /> },
  { id: "files", icon: <FilesIcon /> },
  { id: "tools", icon: <NodesIcon /> },
];

/** Barre d'activité verticale (56px) à gauche. */
export function ActivityBar({
  active,
  onChange,
}: {
  active: View;
  onChange: (v: View) => void;
}) {
  return (
    <div className="flex w-14 flex-none flex-col items-center gap-1 border-r border-line-soft bg-sunken py-3">
      {items.map((it) => {
        const on = active === it.id;
        return (
          <button
            key={it.id}
            onClick={() => onChange(it.id)}
            className={`relative flex h-10 w-10 items-center justify-center rounded-[10px] ${
              on ? "text-accent" : "text-muted-3 hover:text-ink-3"
            }`}
            style={on ? { background: "rgba(240,161,92,.14)" } : undefined}
          >
            {on && (
              <span className="absolute left-[-12px] top-[11px] h-[18px] w-[3px] rounded bg-accent" />
            )}
            {it.icon}
          </button>
        );
      })}

      <div className="flex-1" />

      <button
        onClick={() => onChange("settings")}
        className={`relative flex h-10 w-10 items-center justify-center rounded-[10px] ${
          active === "settings" ? "text-accent" : "text-muted-3 hover:text-ink-3"
        }`}
        style={
          active === "settings"
            ? { background: "rgba(240,161,92,.14)" }
            : undefined
        }
      >
        {active === "settings" && (
          <span className="absolute left-[-12px] top-[11px] h-[18px] w-[3px] rounded bg-accent" />
        )}
        <SettingsIcon />
      </button>

      <div className="mt-1 flex h-[30px] w-[30px] items-center justify-center rounded-full bg-[#3a332b] text-xs font-semibold text-ink-2">
        M
      </div>
    </div>
  );
}
