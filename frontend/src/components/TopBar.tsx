import { useEffect, useState } from "react";
import { useStore } from "../store/useStore";
import { getVersion } from "../api/client";
import { LokiMark } from "./Icon";
import { ModelSelector } from "./ModelSelector";
import { SessionMenu } from "./SessionMenu";

/** Barre supérieure sombre : logo, fil d'Ariane, statut Ollama, sélecteur. */
export function TopBar() {
  const status = useStore((s) => s.status);
  const stats = useStore((s) => s.systemStats);
  const connected = status?.connected ?? false;
  const [version, setVersion] = useState("");

  useEffect(() => {
    getVersion().then(setVersion);
  }, []);

  return (
    <div className="flex h-[54px] flex-none items-center gap-3.5 border-b-[3px] border-line bg-bar px-4">
      <div className="flex items-center gap-2.5">
        <LokiMark />
        <div className="font-pixel text-[14px] text-white">LOKI</div>
        <div
          className="text-[13px] leading-none text-on-dark-2"
          title={version ? `build ${version}` : undefined}
        >
          agent local
          {version && version !== "dev" && (
            <span className="ml-1.5 text-on-dark-3">· {version.slice(0, 7)}</span>
          )}
        </div>
      </div>

      <div className="h-6 w-[3px] bg-chrome-3" />

      <SessionMenu />

      <div className="flex-1" />

      {/* Stats système temps réel : CPU, RAM, GPU/VRAM */}
      {stats && (
        <div className="flex h-8 items-center gap-2.5 border-[3px] border-chrome-3 bg-chrome-2 px-[11px] text-[13px] text-on-dark">
          <span>CPU {stats.cpu_pct.toFixed(0)}%</span>
          <span className="text-on-dark-3">·</span>
          <span>RAM {stats.ram_pct.toFixed(0)}%</span>
          {stats.gpu && (
            <>
              <span className="text-on-dark-3">·</span>
              <span>GPU {stats.gpu.util_pct.toFixed(0)}%</span>
              <span className="text-on-dark-3">
                VRAM {(stats.gpu.vram_used_mb / 1024).toFixed(1)}/
                {(stats.gpu.vram_total_mb / 1024).toFixed(1)}G
              </span>
            </>
          )}
        </div>
      )}

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
