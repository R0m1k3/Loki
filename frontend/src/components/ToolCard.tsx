import type { ToolCall } from "../api/client";

/** Carte d'appel d'outil rendue dans le fil (fidèle à la maquette). */
export function ToolCard({ call }: { call: ToolCall }) {
  const running = call.status === "running";
  const error = call.status === "error";
  const pending = call.status === "pending";

  // Argument principal affiché entre parenthèses selon l'outil.
  const mainArg =
    (call.args?.path as string) ??
    (call.args?.query as string) ??
    (call.args?.command as string);
  const argPreview =
    typeof mainArg === "string"
      ? `("${mainArg.length > 48 ? mainArg.slice(0, 48) + "…" : mainArg}")`
      : Object.keys(call.args ?? {}).length
      ? "(…)"
      : "()";

  return (
    <div className="mb-[9px] overflow-hidden rounded-[11px] border border-line bg-card-deep">
      <div className="flex items-center gap-2.5 px-[13px] py-2.5">
        <span
          className="flex h-6 w-6 items-center justify-center rounded-[7px] text-accent"
          style={{ background: "rgba(240,161,92,.13)" }}
        >
          <ToolGlyph name={call.name} />
        </span>
        <span className="font-mono text-[12.5px] font-semibold text-[#e7ddcd]">
          {call.name}
        </span>
        <span className="font-mono text-xs text-muted-2">{argPreview}</span>
        <div className="flex-1" />
        {running ? (
          <span className="flex items-center gap-1.5 font-mono text-[11px] text-muted">
            <span className="h-2.5 w-2.5 animate-spin rounded-full border-2 border-muted-3 border-t-accent" />
            en cours
          </span>
        ) : pending ? (
          <span className="font-mono text-[11px] text-warn">⏸ à valider</span>
        ) : (
          <span
            className={`flex items-center gap-1.5 font-mono text-[11px] ${
              error ? "text-warn" : "text-ok"
            }`}
          >
            {error ? "✕ échec" : "✓ terminé"}
          </span>
        )}
      </div>
      {call.summary && !running && (
        <div className="border-t border-line-soft px-[13px] py-2.5 font-mono text-[11.5px] text-muted-3">
          → {call.summary}
        </div>
      )}
    </div>
  );
}

function ToolGlyph({ name }: { name: string }) {
  const common = {
    width: 13,
    height: 13,
    viewBox: "0 0 24 24",
    fill: "none",
    stroke: "currentColor",
    strokeWidth: 1.9,
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
  };
  if (name === "write_file")
    return (
      <svg {...common}>
        <path d="M5 19h14M7 14l9-9 3 3-9 9-4 1z" />
      </svg>
    );
  if (name === "list_dir")
    return (
      <svg {...common}>
        <path d="M4 7h6l2 2h8v9H4z" />
      </svg>
    );
  if (name === "web_search")
    return (
      <svg {...common}>
        <circle cx="11" cy="11" r="7" />
        <path d="m21 21-4.3-4.3" />
      </svg>
    );
  if (name === "run_shell")
    return (
      <svg {...common}>
        <path d="m6 9 3 3-3 3M13 15h5" />
        <rect x="2" y="4" width="20" height="16" rx="2" />
      </svg>
    );
  // read_file (défaut)
  return (
    <svg {...common}>
      <path d="M7 4h7l4 4v12H7z" />
      <path d="M14 4v4h4" />
    </svg>
  );
}
