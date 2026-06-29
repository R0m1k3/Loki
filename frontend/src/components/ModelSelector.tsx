import { useEffect, useRef, useState } from "react";
import { useStore } from "../store/useStore";
import { ChevronDown } from "./Icon";

/** Sélecteur de modèle Ollama (chip de la barre supérieure). */
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
        className="flex h-7 items-center gap-2 rounded-lg border border-line-strong bg-[#2a251f] px-[11px]"
      >
        <span className="h-1.5 w-1.5 rounded-full bg-accent" />
        <span className="font-mono text-xs font-medium text-[#e7ddcd]">
          {selectedModel || "—"}
        </span>
        <ChevronDown size={12} className="text-muted-2" />
      </button>

      {open && (
        <div className="absolute right-0 top-9 z-20 w-64 rounded-lg border border-line bg-card p-1 shadow-frame">
          {models.length === 0 && (
            <div className="px-3 py-2 text-xs text-muted-3">
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
              className={`flex w-full items-center gap-2 rounded-md px-2 py-2 text-left hover:bg-[#252019] ${
                m.name === selectedModel ? "bg-[#252019]" : ""
              }`}
            >
              <span
                className={`h-1.5 w-1.5 rounded-full ${
                  m.name === selectedModel ? "bg-accent" : "bg-muted-3"
                }`}
              />
              <span className="flex-1 font-mono text-xs text-ink-2">
                {m.name}
              </span>
              {m.size_go > 0 && (
                <span className="font-mono text-[10px] text-muted-3">
                  {m.size_go} Go
                </span>
              )}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
