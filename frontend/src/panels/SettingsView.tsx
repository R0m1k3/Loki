import { useEffect, useState } from "react";
import { useStore } from "../store/useStore";
import { pullModel } from "../api/client";
import type { AgentConfig } from "../api/client";
import { DownloadIcon, RefreshIcon } from "../components/Icon";

const TOOL_DESC: Record<string, string> = {
  read_file: "Lire un fichier du projet",
  write_file: "Créer / modifier un fichier",
  list_dir: "Lister un répertoire",
};

/** Vue Configuration (Frame 2) — Modèle & génération, outils, invite système. */
export function SettingsView() {
  const {
    models,
    selectedModel,
    setSelectedModel,
    refreshModels,
    status,
    config,
    availableTools,
    refreshConfig,
    updateConfig,
  } = useStore();

  // Brouillon local édité, synchronisé depuis la config serveur.
  const [draft, setDraft] = useState<AgentConfig | null>(config);
  const [saved, setSaved] = useState(false);

  useEffect(() => {
    refreshConfig();
  }, [refreshConfig]);
  useEffect(() => {
    if (config) setDraft(config);
  }, [config]);

  const [pullName, setPullName] = useState("");
  const [pullStatus, setPullStatus] = useState<string | null>(null);
  const [pullPct, setPullPct] = useState(0);

  const doPull = async () => {
    if (!pullName.trim()) return;
    setPullStatus("démarrage…");
    setPullPct(0);
    try {
      await pullModel(pullName.trim(), (s, p) => {
        setPullStatus(s);
        if (p) setPullPct(p);
      });
      setPullStatus("terminé");
      await refreshModels();
    } catch {
      setPullStatus("échec");
    }
  };

  const dirty =
    draft && config && JSON.stringify(draft) !== JSON.stringify(config);

  const save = async () => {
    if (!draft) return;
    await updateConfig(draft);
    setSaved(true);
    setTimeout(() => setSaved(false), 2000);
  };

  const set = <K extends keyof AgentConfig>(key: K, value: AgentConfig[K]) =>
    setDraft((d) => (d ? { ...d, [key]: value } : d));

  const activeTools = draft
    ? Object.values(draft.tools).filter(Boolean).length
    : 0;

  return (
    <div className="scr flex-1 overflow-auto px-[30px] py-[26px]">
      <div className="mx-auto max-w-[1000px]">
        {/* En-tête */}
        <div className="mb-[22px] flex items-end justify-between">
          <div>
            <div className="text-xl font-bold tracking-tight">
              Modèle &amp; génération
            </div>
            <div className="mt-1 text-[13px] text-muted-2">
              Choisis le modèle local et règle son comportement.
            </div>
          </div>
          <div className="flex items-center gap-3">
            <div className="flex items-center gap-1.5 font-mono text-[11.5px] text-ok">
              <span className="h-1.5 w-1.5 rounded-full bg-ok" />
              {status?.connected ? "Ollama connecté" : "Ollama déconnecté"}
            </div>
            <button
              onClick={save}
              disabled={!dirty}
              className="flex h-[30px] items-center gap-1.5 rounded-lg bg-accent px-3.5 text-[12.5px] font-bold text-[#1f1b16] disabled:opacity-40"
            >
              {saved ? "Enregistré ✓" : dirty ? "Enregistrer" : "À jour"}
            </button>
          </div>
        </div>

        {!draft ? (
          <div className="py-20 text-center text-muted-3">
            Chargement de la configuration…
          </div>
        ) : (
          <>
            <div className="grid grid-cols-2 items-start gap-5">
              {/* Modèle Ollama */}
              <div className="rounded-card border border-line bg-card p-[18px]">
                <div className="mb-3.5 flex items-center justify-between">
                  <div className="text-sm font-bold">Modèle Ollama</div>
                  <button
                    onClick={refreshModels}
                    className="flex h-[26px] items-center gap-1.5 rounded-[7px] border border-line-strong bg-base px-2.5 text-[11px] text-muted"
                  >
                    <RefreshIcon size={12} />
                    Actualiser
                  </button>
                </div>

                <div className="flex flex-col gap-2">
                  {models.length === 0 && (
                    <div className="rounded-[11px] border border-line bg-base p-4 text-center text-xs text-muted-3">
                      Aucun modèle installé. Télécharge-en un ci-dessous.
                    </div>
                  )}
                  {models.map((m) => {
                    const on = m.name === selectedModel;
                    return (
                      <button
                        key={m.name}
                        onClick={() => setSelectedModel(m.name)}
                        className="flex items-center gap-3 rounded-[11px] border p-3 text-left"
                        style={{
                          background: on ? "rgba(240,161,92,.10)" : "#1a1714",
                          borderColor: on ? "rgba(240,161,92,.40)" : "#2c2720",
                        }}
                      >
                        <span
                          className="flex h-[18px] w-[18px] items-center justify-center rounded-full border-2"
                          style={{ borderColor: on ? "#f0a15c" : "#4a443b" }}
                        >
                          {on && <span className="h-2 w-2 rounded-full bg-accent" />}
                        </span>
                        <div className="flex-1">
                          <div className="flex items-center gap-2">
                            <span className="font-mono text-[13px] font-semibold text-ink">
                              {m.name}
                            </span>
                            {m.name === status?.default_model && (
                              <span
                                className="rounded-[5px] px-1.5 py-px text-[10px] font-bold text-accent"
                                style={{ background: "rgba(240,161,92,.14)" }}
                              >
                                PAR DÉFAUT
                              </span>
                            )}
                          </div>
                          <div className="mt-[3px] font-mono text-[10.5px] text-muted-2">
                            {[
                              m.size_go ? `${m.size_go} Go` : null,
                              m.quantization,
                              m.parameter_size,
                              m.family,
                            ]
                              .filter(Boolean)
                              .join(" · ")}
                          </div>
                        </div>
                      </button>
                    );
                  })}
                </div>

                <div className="my-4 h-px bg-line" />

                <div className="mb-2.5 text-[11px] font-bold uppercase tracking-[.05em] text-label">
                  Télécharger un modèle
                </div>
                <div className="mb-3 flex gap-2">
                  <input
                    value={pullName}
                    onChange={(e) => setPullName(e.target.value)}
                    placeholder="gemma2:9b"
                    className="h-[34px] flex-1 rounded-[9px] border border-line-strong bg-base px-3 font-mono text-xs text-ink outline-none placeholder:text-muted-3"
                  />
                  <button
                    onClick={doPull}
                    className="flex h-[34px] items-center gap-1.5 rounded-[9px] border border-line-strong bg-[#2a251f] px-3.5 text-[12.5px] font-semibold text-[#e7ddcd]"
                  >
                    <DownloadIcon />
                    Récupérer
                  </button>
                </div>

                {pullStatus && (
                  <div className="rounded-[10px] border border-line bg-base p-3">
                    <div className="mb-2 flex items-center justify-between">
                      <span className="font-mono text-xs text-ink-2">
                        {pullName}
                      </span>
                      <span className="font-mono text-[11px] text-accent">
                        {pullStatus} {pullPct ? `· ${pullPct}%` : ""}
                      </span>
                    </div>
                    <div className="h-1.5 overflow-hidden rounded bg-line">
                      <div
                        className="h-full rounded bg-accent-grad transition-all"
                        style={{ width: `${pullPct}%` }}
                      />
                    </div>
                  </div>
                )}
              </div>

              {/* Génération + Outils */}
              <div className="flex flex-col gap-5">
                <div className="rounded-card border border-line bg-card p-[18px]">
                  <div className="mb-4 text-sm font-bold">Génération</div>
                  <Slider
                    label="Température"
                    value={draft.temperature}
                    min={0}
                    max={2}
                    step={0.05}
                    fmt={(v) => v.toFixed(2)}
                    onChange={(v) => set("temperature", v)}
                  />
                  <Slider
                    label="Top-P"
                    value={draft.top_p}
                    min={0}
                    max={1}
                    step={0.05}
                    fmt={(v) => v.toFixed(2)}
                    onChange={(v) => set("top_p", v)}
                  />
                  <Slider
                    label="Top-K"
                    value={draft.top_k}
                    min={1}
                    max={100}
                    step={1}
                    fmt={(v) => String(Math.round(v))}
                    onChange={(v) => set("top_k", Math.round(v))}
                  />
                  <Slider
                    label="Jetons max"
                    value={draft.max_tokens}
                    min={256}
                    max={8192}
                    step={128}
                    fmt={(v) => String(Math.round(v))}
                    onChange={(v) => set("max_tokens", Math.round(v))}
                    last
                  />
                </div>

                <div className="rounded-card border border-line bg-card p-[18px]">
                  <div className="mb-3.5 flex items-center justify-between">
                    <div className="text-sm font-bold">Outils de l'agent</div>
                    <span className="font-mono text-[11px] text-muted-2">
                      {activeTools} / {availableTools.length} actifs
                    </span>
                  </div>
                  {availableTools.map((name, i) => {
                    const on = draft.tools[name] ?? false;
                    return (
                      <div
                        key={name}
                        className={`flex items-center gap-3 py-[9px] ${
                          i > 0 ? "border-t border-line-soft" : ""
                        }`}
                      >
                        <span className="w-32 flex-none font-mono text-[12.5px] text-ink-2">
                          {name}
                        </span>
                        <span className="flex-1 text-xs text-muted-2">
                          {TOOL_DESC[name] ?? ""}
                        </span>
                        <button
                          onClick={() =>
                            set("tools", { ...draft.tools, [name]: !on })
                          }
                          className="relative h-5 w-[34px] rounded-full transition-colors"
                          style={{ background: on ? "#f0a15c" : "#3a342c" }}
                        >
                          <span
                            className="absolute top-0.5 h-4 w-4 rounded-full transition-all"
                            style={{
                              background: on ? "#1f1b16" : "#6f675c",
                              left: on ? 16 : 2,
                            }}
                          />
                        </button>
                      </div>
                    );
                  })}
                  <div className="mt-1 border-t border-line-soft pt-2.5 font-mono text-[11px] text-muted-3">
                    web_search · run_shell — arrivent bientôt (sensibles)
                  </div>
                </div>
              </div>
            </div>

            {/* Invite système */}
            <div className="mt-5 rounded-card border border-line bg-card p-[18px]">
              <div className="mb-3 flex items-center justify-between">
                <div className="text-sm font-bold">Invite système</div>
                <span className="font-mono text-[11px] text-muted-2">
                  {draft.system_prompt.length} caractères
                </span>
              </div>
              <textarea
                value={draft.system_prompt}
                onChange={(e) => set("system_prompt", e.target.value)}
                rows={6}
                className="scr w-full resize-y rounded-[10px] border border-line bg-sunken p-4 font-mono text-[12.5px] leading-[1.7] text-ink-3 outline-none focus:border-line-strong"
              />
            </div>
          </>
        )}
      </div>
    </div>
  );
}

function Slider({
  label,
  value,
  min,
  max,
  step,
  fmt,
  onChange,
  last,
}: {
  label: string;
  value: number;
  min: number;
  max: number;
  step: number;
  fmt: (v: number) => string;
  onChange: (v: number) => void;
  last?: boolean;
}) {
  const pct = ((value - min) / (max - min)) * 100;
  return (
    <div className={last ? "" : "mb-4"}>
      <div className="mb-2 flex items-center justify-between">
        <span className="text-[13px] text-ink-2">{label}</span>
        <span className="font-mono text-xs font-semibold text-accent">
          {fmt(value)}
        </span>
      </div>
      <div className="relative h-4">
        {/* Piste + remplissage + pouce (visuel fidèle) */}
        <div className="absolute top-1/2 h-1.5 w-full -translate-y-1/2 rounded bg-line" />
        <div
          className="absolute top-1/2 h-1.5 -translate-y-1/2 rounded bg-accent"
          style={{ width: `${pct}%` }}
        />
        <div
          className="absolute top-1/2 h-4 w-4 -translate-x-1/2 -translate-y-1/2 rounded-full border-[3px] border-card bg-accent"
          style={{ left: `${pct}%` }}
        />
        {/* Input réel transparent par-dessus */}
        <input
          type="range"
          min={min}
          max={max}
          step={step}
          value={value}
          onChange={(e) => onChange(parseFloat(e.target.value))}
          className="absolute inset-0 h-full w-full cursor-pointer opacity-0"
        />
      </div>
    </div>
  );
}
