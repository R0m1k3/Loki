import { useEffect, useRef, useState } from "react";
import { useStore } from "../store/useStore";
import { ChevronDown } from "./Icon";

/** Sélecteur de modèle Ollama (chip orange de la barre supérieure). */
export function ModelSelector() {
  const { models, selectedModel, setSelectedModel } = useStore();
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const onClick = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onClick);
    return () => document.removeEventListener("mousedown", onClick);
  }, []);

  return (
    <div className="relative" ref={ref}>
      <button
        onClick={() => setOpen((v) => !v)}
        className="flex h-[34px] min-w-0 max-w-[260px] items-center gap-2 border-[3px] border-white bg-accent px-3 text-white shadow-accent-soft"
        title={selectedModel || undefined}
        style={{ borderRadius: 7 }}
      >
        <span className="h-2.5 w-2.5 border-2 border-line bg-white" />
        <span className="min-w-0 truncate text-[14px] leading-none">{selectedModel || "—"}</span>
        <ChevronDown size={12} className="flex-none" />
      </button>

      {open && (
        <div className="absolute right-0 top-11 z-20 w-64 border-[3px] border-line bg-card p-1 shadow-hard">
          {models.length === 0 && (
            <div className="px-3 py-2 text-xs text-muted-2">
              Aucun modèle installé
            </div>
          )}
          {models.map((m) => (
            <button
              key={m.name}
              onClick={() => {
                setSelectedModel(m.name);
                setOpen(false);
              }}
              className={`flex w-full items-center gap-2 px-2 py-2 text-left hover:bg-base ${
                m.name === selectedModel ? "bg-base" : ""
              }`}
            >
              <span
                className={`h-2 w-2 border-2 border-line ${
                  m.name === selectedModel ? "bg-accent" : "bg-card"
                }`}
              />
              <span className="min-w-0 flex-1 truncate text-xs text-ink-2" title={m.name}>
                {m.name}
              </span>
              {m.size_go > 0 && (
                <span className="text-[10px] text-muted-2">{m.size_go} Go</span>
              )}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
