import { useStore } from "../store/useStore";
import { ChevronDown, LokiMark } from "./Icon";
import { ModelSelector } from "./ModelSelector";

/** Barre supérieure : logo, fil d'Ariane, statut Ollama, sélecteur de modèle. */
export function TopBar() {
  const status = useStore((s) => s.status);
  const connected = status?.connected ?? false;

  return (
    <div className="flex h-12 flex-none items-center gap-3.5 border-b border-line-strong bg-bar px-4">
      <div className="flex items-center gap-2.5">
        <LokiMark />
        <div className="text-[15px] font-bold tracking-tight">Loki</div>
        <div className="pt-px font-mono text-[11px] text-muted-3">
          agent local
        </div>
      </div>

      <div className="h-5 w-px bg-line-strong" />

      <div className="flex items-center gap-2 text-[13px] font-medium text-ink-2">
        <span>Nouvelle session</span>
        <ChevronDown className="text-muted-3" />
      </div>

      <div className="flex-1" />

      {/* Statut Ollama */}
      <div
        className="flex h-7 items-center gap-2 rounded-lg border border-line-strong bg-card-soft px-[11px]"
        title={connected ? status?.host : status?.error}
      >
        <span
          className={`h-[7px] w-[7px] rounded-full ${
            connected ? "bg-ok" : "bg-warn"
          }`}
          style={
            connected
              ? { boxShadow: "0 0 0 3px rgba(134,199,154,.16)" }
              : undefined
          }
        />
        <span className="text-xs font-medium text-ink-3">Ollama</span>
        <span className="font-mono text-[11px] text-muted-3">
          {connected
            ? status?.host.replace(/^https?:\/\//, "")
            : "déconnecté"}
        </span>
      </div>

      <ModelSelector />
    </div>
  );
}
