import { useEffect, useState } from "react";
import { useStore } from "../store/useStore";
import { deleteModel, pullModel } from "../api/client";
import type { AgentConfig } from "../api/client";
import { DownloadIcon, RefreshIcon } from "../components/Icon";

const TOOL_DESC: Record<string, string> = {
  read_file: "Lire un fichier du projet",
  write_file: "Créer / modifier un fichier",
  list_dir: "Lister un répertoire",
  web_search: "Recherche web",
  run_shell: "Exécuter une commande",
};
const SENSITIVE = new Set(["run_shell"]);

/** Vue Configuration — Modèle & génération, outils, invite système. */
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

  const [draft, setDraft] = useState<AgentConfig | null>(config);
  const [saved, setSaved] = useState(false);

  useEffect(() => {
    refreshConfig();
  }, [refreshConfig, selectedModel]);
  useEffect(() => {
    if (config) setDraft(config);
  }, [config]);

  const [pullName, setPullName] = useState("");
  const [pullStatus, setPullStatus] = useState<string | null>(null);
  const [pullPct, setPullPct] = useState(0);
  const [deletingModel, setDeletingModel] = useState<string | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);

  const doDeleteModel = async (name: string) => {
    if (!window.confirm(`Supprimer définitivement le modèle ${name} ?`)) return;
    setDeletingModel(name);
    setDeleteError(null);
    try {
      await deleteModel(name);
      await refreshModels();
    } catch (err) {
      setDeleteError(err instanceof Error ? err.message : "suppression impossible");
    } finally {
      setDeletingModel(null);
    }
  };

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
    <div className="scr flex-1 overflow-auto bg-base px-[30px] py-[26px]">
      <div className="mx-auto max-w-[1000px]">
        {/* En-tête */}
        <div className="mb-[22px] flex items-end justify-between">
          <div>
            <div className="font-pixel text-[13px] text-ink">
              MODÈLE &amp; GÉNÉRATION
            </div>
            <div className="mt-2 text-[14px] text-muted-2">
              Choisis le modèle local et règle son comportement. Les changements
              sont sauvegardés pour le modèle sélectionné.
            </div>
          </div>
          <div className="flex items-center gap-3">
            <div className="flex items-center gap-1.5 text-[13px] text-ok">
              <span className="h-2 w-2 border-2 border-line bg-ok" />
              {status?.connected ? "Ollama connecté" : "Ollama déconnecté"}
            </div>
            <button
              onClick={save}
              disabled={!dirty}
              className="flex h-[34px] items-center gap-1.5 border-[3px] border-line bg-accent px-4 text-[14px] text-white shadow-accent-soft disabled:opacity-40"
              style={{ borderRadius: 7 }}
            >
              {saved ? "✓ ENREGISTRÉ" : dirty ? "ENREGISTRER" : "À JOUR"}
            </button>
          </div>
        </div>

        {!draft ? (
          <div className="py-20 text-center text-muted-2">
            Chargement de la configuration…
          </div>
        ) : (
          <>
            <div className="grid grid-cols-2 items-start gap-[18px]">
              {/* Modèle Ollama */}
              <Card>
                <div className="mb-3.5 flex items-center justify-between">
                  <div className="font-pixel text-[11px] text-ink">
                    MODÈLE OLLAMA
                  </div>
                  <button
                    onClick={refreshModels}
                    className="flex h-7 items-center gap-1.5 border-2 border-line bg-card-soft px-2.5 text-[13px] text-muted-2"
                  >
                    <RefreshIcon size={12} />
                    Actualiser
                  </button>
                </div>

                <div className="flex flex-col gap-2">
                  {models.length === 0 && (
                    <div className="border-2 border-line bg-base p-4 text-center text-[13px] text-muted-2">
                      Aucun modèle installé. Télécharge-en un ci-dessous.
                    </div>
                  )}
                  {models.map((m) => {
                    const on = m.name === selectedModel;
                    return (
                      <div
                        key={m.name}
                        className={`flex items-center gap-3 border-[3px] border-line p-3 text-left ${
                          on ? "bg-card-deep" : "bg-card-soft"
                        }`}
                      >
                        <button
                          onClick={() => setSelectedModel(m.name)}
                          className="flex min-w-0 flex-1 items-center gap-3 text-left"
                        >
                          <span
                            className={`flex h-[18px] w-[18px] flex-none items-center justify-center border-[3px] ${
                              on ? "border-accent" : "border-muted-3"
                            }`}
                          >
                            {on && <span className="h-2 w-2 bg-accent" />}
                          </span>
                          <div className="min-w-0 flex-1">
                            <div className="flex items-center gap-2">
                              <span className={`truncate text-[14px] ${on ? "text-white" : "text-ink"}`}>
                                {m.name}
                              </span>
                              {m.name === status?.default_model && (
                                <span className="font-pixel flex-none border-2 border-white bg-accent px-1.5 py-0.5 text-[7px] text-white">
                                  DÉFAUT
                                </span>
                              )}
                            </div>
                            <div className={`mt-1 text-[13px] ${on ? "text-on-dark-2" : "text-muted-2"}`}>
                              {[m.size_go ? `${m.size_go} Go` : null, m.quantization, m.parameter_size, m.family]
                                .filter(Boolean)
                                .join(" · ")}
                            </div>
                          </div>
                        </button>
                        <button
                          onClick={() => doDeleteModel(m.name)}
                          disabled={deletingModel === m.name}
                          className="flex-none border-2 border-line bg-base px-2 py-1 text-[12px] text-warn disabled:opacity-50"
                          title={`Supprimer ${m.name}`}
                        >
                          {deletingModel === m.name ? "…" : "Supprimer"}
                        </button>
                      </div>
                    );
                  })}
                  {deleteError && (
                    <div className="border-2 border-warn bg-base px-3 py-2 text-[12px] text-warn">
                      {deleteError}
                    </div>
                  )}
                </div>

                <div className="my-4 h-[3px] bg-line" />

                <div className="font-pixel mb-2.5 text-[10px] text-label">
                  TÉLÉCHARGER
                </div>
                <div className="mb-3 flex gap-2">
                  <input
                    value={pullName}
                    onChange={(e) => setPullName(e.target.value)}
                    placeholder="gemma2:9b"
                    className="h-9 flex-1 border-[3px] border-line bg-card-soft px-3 text-[13px] text-ink outline-none placeholder:text-muted-3"
                  />
                  <button
                    onClick={doPull}
                    className="flex h-9 items-center gap-1.5 border-[3px] border-line bg-card-deep px-3.5 text-[13px] text-white"
                  >
                    <DownloadIcon />
                    Récupérer
                  </button>
                </div>

                {pullStatus && (
                  <div className="border-[3px] border-line bg-card-soft p-3">
                    <div className="mb-2 flex items-center justify-between">
                      <span className="text-[13px] text-ink-2">{pullName}</span>
                      <span className="text-[13px] text-accent">
                        {pullStatus} {pullPct ? `· ${pullPct}%` : ""}
                      </span>
                    </div>
                    <div className="flex h-3.5 gap-px border-2 border-line bg-card p-px">
                      <span
                        className="bg-accent transition-all"
                        style={{ width: `${pullPct}%` }}
                      />
                    </div>
                  </div>
                )}
              </Card>

              {/* Génération + Outils */}
              <div className="flex flex-col gap-[18px]">
                <Card>
                  <div className="font-pixel mb-4 text-[11px] text-ink">
                    GÉNÉRATION
                  </div>

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
                  />
                  <Slider
                    label="Cache KV / contexte"
                    value={draft.num_ctx}
                    min={2048}
                    max={32768}
                    step={1024}
                    fmt={(v) => `${Math.round(v / 1024)}K`}
                    onChange={(v) => set("num_ctx", Math.round(v))}
                  />
                  <Slider
                    label="Couches GPU"
                    value={draft.num_gpu}
                    min={-1}
                    max={120}
                    step={1}
                    fmt={(v) => (v < 0 ? "auto" : String(Math.round(v)))}
                    onChange={(v) => set("num_gpu", Math.round(v))}
                  />
                  <Slider
                    label="Batch"
                    value={draft.num_batch}
                    min={64}
                    max={1024}
                    step={64}
                    fmt={(v) => String(Math.round(v))}
                    onChange={(v) => set("num_batch", Math.round(v))}
                    last
                  />
                  <div className="mt-4 flex items-center gap-3 border-2 border-line bg-base px-3 py-2.5">
                    <div className="flex-1">
                      <div className="text-[14px] text-ink">Mode réflexion</div>
                      <div className="text-[12px] text-muted-2">
                        Désactive si le modèle ne renvoie que du raisonnement sans
                        réponse.
                      </div>
                    </div>
                    <Toggle on={draft.think} onClick={() => set("think", !draft.think)} />
                  </div>
                  <div className="mt-3 border-2 border-line bg-card-soft px-3 py-2 text-[12px] leading-relaxed text-muted-2">
                    La précision KV (<code>f16</code>/<code>q8_0</code>) est un
                    réglage global du serveur Ollama et nécessite son redémarrage.
                  </div>
                </Card>

                <Card>
                  <div className="mb-3.5 flex items-center justify-between">
                    <div className="font-pixel text-[11px] text-ink">OUTILS</div>
                    <span className="text-[13px] text-muted-2">
                      {activeTools} / {availableTools.length} actifs
                    </span>
                  </div>
                  {availableTools.map((name, i) => {
                    const on = draft.tools[name] ?? false;
                    return (
                      <div
                        key={name}
                        className={`flex items-center gap-3 py-2 ${
                          i > 0 ? "border-t-2 border-line-soft" : ""
                        }`}
                      >
                        <span
                          className={`w-32 flex-none text-[14px] ${on ? "text-ink" : "text-muted-3"}`}
                        >
                          {name}
                        </span>
                        <span className="flex-1 text-[13px] text-muted-2">
                          {TOOL_DESC[name] ?? ""}
                          {SENSITIVE.has(name) && (
                            <span className="text-accent"> · sensible</span>
                          )}
                        </span>
                        <Toggle on={on} onClick={() => set("tools", { ...draft.tools, [name]: !on })} />
                      </div>
                    );
                  })}
                  {draft.tools.run_shell && (
                    <div className="mt-2 flex items-center gap-3 border-2 border-line bg-base px-3 py-2.5">
                      <span className="flex-1 text-[13px] text-ink-2">
                        Demander une validation avant chaque commande shell
                      </span>
                      <Toggle
                        on={draft.confirm_shell}
                        onClick={() => set("confirm_shell", !draft.confirm_shell)}
                      />
                    </div>
                  )}
                </Card>
              </div>
            </div>

            {/* Invite système */}
            <Card className="mt-[18px]">
              <div className="mb-3 flex items-center justify-between">
                <div className="font-pixel text-[11px] text-ink">
                  INVITE SYSTÈME
                </div>
                <span className="text-[13px] text-muted-2">
                  {draft.system_prompt.length} caractères
                </span>
              </div>
              <textarea
                value={draft.system_prompt}
                onChange={(e) => set("system_prompt", e.target.value)}
                rows={6}
                className="scr w-full resize-y border-[3px] border-line bg-card-deep p-4 text-[13px] leading-[1.6] text-on-dark outline-none"
              />
            </Card>
          </>
        )}
      </div>
    </div>
  );
}

function Card({
  children,
  className = "",
}: {
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div
      className={`border-[3px] border-line bg-card p-4 shadow-hard-lg ${className}`}
      style={{ borderRadius: 9 }}
    >
      {children}
    </div>
  );
}

function Toggle({ on, onClick }: { on: boolean; onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      className={`relative h-5 w-[38px] flex-none ${
        on ? "bg-card-deep" : "border-2 border-line bg-base"
      }`}
    >
      <span
        className={`absolute h-3.5 w-3.5 ${
          on ? "right-0.5 top-0.5 bg-accent" : "left-0.5 top-[3px] bg-muted-3"
        }`}
      />
    </button>
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
    <div className={last ? "" : "mb-[15px]"}>
      <div className="mb-[7px] flex items-center justify-between">
        <span className="text-[13px] text-ink-2">{label}</span>
        <span className="text-[13px] text-accent">{fmt(value)}</span>
      </div>
      <div className="relative h-[14px] border-2 border-line bg-card">
        <div
          className="absolute left-0 top-0 bottom-0 bg-line"
          style={{ width: `${pct}%` }}
        />
        <div
          className="absolute top-1/2 h-5 w-4 -translate-x-1/2 -translate-y-1/2 border-[3px] border-line bg-accent"
          style={{ left: `${pct}%` }}
        />
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
