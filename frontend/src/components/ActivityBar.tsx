import {
  ChatIcon,
  ClockIcon,
  FilesIcon,
  NodesIcon,
  SettingsIcon,
} from "./Icon";

export type View = "chat" | "history" | "files" | "tools" | "settings";

const items: { id: View; label: string; icon: React.ReactNode }[] = [
  { id: "chat", label: "Chat", icon: <ChatIcon /> },
  { id: "history", label: "Historique", icon: <ClockIcon /> },
  { id: "files", label: "Fichiers", icon: <FilesIcon /> },
  { id: "tools", label: "Outils", icon: <NodesIcon /> },
];

const btn = (on: boolean) =>
  `flex h-10 w-10 items-center justify-center border-[3px] ${
    on
      ? "border-white bg-accent text-white shadow-accent-soft"
      : "border-chrome-3 text-on-dark-2 hover:text-white"
  }`;

/** Barre d'activité verticale (60px) sombre à gauche. */
export function ActivityBar({
  active,
  onChange,
}: {
  active: View;
  onChange: (v: View) => void;
}) {
  return (
    <div className="flex w-[60px] flex-none flex-col items-center gap-2 border-r-[3px] border-line bg-bar py-3">
      {items.map((it) => (
        <button
          key={it.id}
          onClick={() => onChange(it.id)}
          className={btn(active === it.id)}
          title={it.label}
          aria-label={it.label}
        >
          {it.icon}
        </button>
      ))}

      <div className="flex-1" />

      <button
        onClick={() => onChange("settings")}
        className={btn(active === "settings")}
        title="Configuration"
        aria-label="Configuration"
      >
        <SettingsIcon />
      </button>

      <div className="font-pixel flex h-[34px] w-[34px] items-center justify-center border-[3px] border-white bg-white text-[11px] text-line">
        M
      </div>
    </div>
  );
}
