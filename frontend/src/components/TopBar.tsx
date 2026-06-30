import { useStore } from "../store/useStore";
import { ChevronDown, LokiMark } from "./Icon";
import { ModelSelector } from "./ModelSelector";

/** Barre supérieure sombre : logo, fil d'Ariane, statut Ollama, sélecteur. */
export function TopBar() {
  const status = useStore((s) => s.status);
  const connected = status?.connected ?? false;

  return (
    <div className="flex h-[54px] flex-none items-center gap-3.5 border-b-[3px] border-line bg-bar px-4">
      <div className="flex items-center gap-2.5">
        <LokiMark />
        <div className="font-pixel text-[14px] text-white">LOKI</div>
        <div className="text-[13px] leading-none text-on-dark-2">agent local</div>
      </div>

      <div className="h-6 w-[3px] bg-chrome-3" />

      <div className="flex items-center gap-2 text-[13px] text-on-dark">
        <span>Nouvelle session</span>
        <ChevronDown className="text-on-dark-3" />
      </div>

      <div className="flex-1" />

      {/* Statut Ollama */}
      <div
        className="flex h-8 items-center gap-2 border-[3px] border-chrome-3 bg-chrome-2 px-[11px]"
        title={connected ? status?.host : status?.error}
      >
        <span
          className={`h-[11px] w-[11px] border-2 border-line ${
            connected ? "bg-ok" : "bg-warn"
          }`}
        />
        <span className="text-[13px] text-on-dark">Ollama</span>
        <span className="text-[13px] text-on-dark-3">
          {connected ? status?.host.replace(/^https?:\/\//, "") : "déconnecté"}
        </span>
      </div>

      <ModelSelector />
    </div>
  );
}
