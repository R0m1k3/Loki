import { useEffect, useState } from "react";
import { useStore } from "../store/useStore";
import {
  deleteModel,
  getBenchScores,
  getHardware,
  listMcp,
  pullModel,
  runBench,
  testMcp,
  updateMcp,
} from "../api/client";
import type {
  AgentConfig,
  BenchResult,
  HardwareInfo,
  McpServer,
} from "../api/client";
import { DownloadIcon, RefreshIcon } from "../components/Icon";

const TOOL_DESC: Record<string, string> = {
  read_file: "Lire un fichier du projet",
  write_file: "Créer / modifier un fichier",
  edit_file: "Modification chirurgicale (recherche/remplacement)",
  list_dir: "Lister un répertoire",
  grep_search: "Chercher dans les fichiers",
  code_task: "Moteur code (multi-fichiers, git)",
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
    loadedModels,
    warmingModel,
    warmError,
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
                    const loaded = loadedModels.find((item) => item.name === m.name);
                    const warming = warmingModel === m.name;
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
                            <div className="mt-1.5 flex items-center gap-1.5 text-[11px]">
                              <span
                                className={`h-2 w-2 border border-line ${
                                  warming
                                    ? "bg-accent animate-pulse"
                                    : loaded?.on_gpu
                                      ? "bg-ok"
                                      : loaded
                                        ? "bg-warn"
                                        : "bg-card"
                                }`}
                              />
                              <span className={on ? "text-on-dark-2" : "text-muted-2"}>
                                {warming
                                  ? "Préchargement en cours…"
                                  : loaded?.on_gpu
                                    ? "Chargé sur GPU"
                                    : loaded
                                      ? `Chargé · ${loaded.gpu_percent}% GPU`
                                      : "Non chargé"}
                              </span>
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
                  {warmError && (
                    <div className="border-2 border-warn bg-base px-3 py-2 text-[12px] text-warn">
                      Préchargement : {warmError}
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

                  {/* Les deux seuls réglages qui comptent au quotidien. */}
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
                    label="Cache KV / contexte"
                    value={draft.num_ctx}
                    min={2048}
                    max={32768}
                    step={1024}
                    fmt={(v) => `${Math.round(v / 1024)}K`}
                    onChange={(v) => set("num_ctx", Math.round(v))}
                  />

                  <Advanced>
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
                    label="Couches GPU (auto conseillé)"
                    value={draft.num_gpu}
                    min={-1}
                    max={120}
                    step={1}
                    fmt={(v) => (v < 0 ? "auto" : String(Math.round(v)))}
                    onChange={(v) => set("num_gpu", Math.round(v))}
                  />
                  <div className="-mt-1 mb-3 text-[12px] text-muted-2">
                    Laisse « auto » : forcer un nombre de couches qui ne tient
                    pas en VRAM bascule toute l'inférence sur le CPU.
                  </div>
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
                  <div className="mt-3 border-2 border-line bg-card-soft px-3 py-2 text-[12px] leading-relaxed text-muted-2">
                    La précision KV (<code>f16</code>/<code>q8_0</code>) est un
                    réglage global du serveur Ollama et nécessite son redémarrage.
                  </div>
                  </Advanced>

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

                <Card>
                  <div className="mb-3.5 font-pixel text-[11px] text-ink">
                    INTELLIGENCE
                  </div>
                  {/* Au quotidien : comment il code, et s'il planifie. */}
                  {([
                    ["ponytail", "Code minimal",
                     "Va au plus simple qui marche, sans dépendance ni extra"] as const,
                    ["plan_mode", "Plan-puis-exécute",
                     "Découpe une nouvelle construction en étapes"] as const,
                  ]).map(([key, label, desc], i) => (
                    <ToggleRow
                      key={key}
                      label={label}
                      desc={desc}
                      first={i === 0}
                      on={Boolean(draft[key])}
                      onClick={() => set(key, !draft[key])}
                    />
                  ))}

                  <Advanced label="Options avancées">
                    {([
                      ["skills_enabled", "Skills automatiques",
                       "Méthodes expertes selon la tâche (débogage, web…)"] as const,
                      ["self_review", "Auto-critique",
                       "Relit et révise la réponse — plus lent"] as const,
                      ["rag_enabled", "Mémoire entre sessions",
                       "Rappelle d'AUTRES discussions ici. Chaque discussion garde toujours sa propre mémoire."] as const,
                    ]).map(([key, label, desc], i) => (
                      <ToggleRow
                        key={key}
                        label={label}
                        desc={desc}
                        first={i === 0}
                        on={Boolean(draft[key])}
                        onClick={() => set(key, !draft[key])}
                      />
                    ))}
                  </Advanced>
                  <div className="mt-2 flex items-center gap-3 border-t-2 border-line-soft pt-3">
                    <span className="flex-1">
                      <span className="block text-[14px] text-ink">
                        Maintien en VRAM (préchargement)
                      </span>
                      <span className="block text-[12px] text-muted-2">
                        Garde le modèle chargé pour des réponses instantanées
                      </span>
                    </span>
                    <select
                      value={draft.keep_alive}
                      onChange={(e) => set("keep_alive", e.target.value)}
                      className="h-8 border-[3px] border-line bg-card px-2 text-[13px] text-ink"
                    >
                      <option value="0">Décharger aussitôt</option>
                      <option value="5m">5 min</option>
                      <option value="30m">30 min</option>
                      <option value="2h">2 heures</option>
                      <option value="-1">Toujours</option>
                    </select>
                  </div>
                </Card>

                <McpCard />

                <HardwareCard />

                <BenchCard />
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

/** Benchmark : évalue le modèle sélectionné sur 5 mini-épreuves. */
/** Panneau Matériel : GPU vu par Loki vs GPU réellement utilisé par Ollama. */
function McpCard() {
  const [servers, setServers] = useState<McpServer[]>([]);
  const [testing, setTesting] = useState<string | null>(null);
  const [testResult, setTestResult] = useState<Record<string, string>>({});

  useEffect(() => {
    listMcp().then(setServers).catch(() => {});
  }, []);

  const toggle = async (s: McpServer) => {
    setServers(await updateMcp(s.id, !s.enabled, s.params));
  };

  const setParam = async (s: McpServer, key: string, value: string) => {
    setServers(await updateMcp(s.id, s.enabled, { ...s.params, [key]: value }));
  };

  const doTest = async (s: McpServer) => {
    setTesting(s.id);
    try {
      const r = await testMcp(s.id);
      setTestResult((m) => ({
        ...m,
        [s.id]: r.ok
          ? `✓ ${r.tools.length} outil(s) : ${r.tools.join(", ").slice(0, 120)}`
          : `✗ ${r.error ?? "échec"}`,
      }));
    } finally {
      setTesting(null);
    }
  };

  return (
    <Card>
      <div className="mb-1 font-pixel text-[11px] text-ink">SERVEURS MCP</div>
      <div className="mb-3.5 text-[12px] text-muted-2">
        Outils professionnels pour l'agent (navigateur, docs, web). Désactivé =
        zéro coût.
      </div>
      <div className="flex flex-col gap-3">
        {servers.map((s) => (
          <div key={s.id} className="border-[3px] border-line bg-base p-3">
            <div className="flex items-center gap-3">
              <span
                className={`h-[11px] w-[11px] flex-none border-2 border-line ${
                  s.state === "connected"
                    ? "bg-ok"
                    : s.state === "error"
                      ? "bg-warn"
                      : "bg-muted-2"
                }`}
                title={s.error ?? s.state}
              />
              <div className="min-w-0 flex-1">
                <div className="text-[14px] font-semibold text-ink">
                  {s.label}
                  {s.tools > 0 && (
                    <span className="ml-2 text-[11px] text-muted-2">
                      {s.tools} outil(s)
                    </span>
                  )}
                </div>
                <div className="truncate text-[12px] text-muted-2">
                  {s.description}
                </div>
              </div>
              <button
                onClick={() => doTest(s)}
                disabled={testing === s.id}
                className="border-2 border-line bg-card px-2 py-1 text-[11px] text-ink-2"
              >
                {testing === s.id ? "test…" : "Tester"}
              </button>
              <Toggle on={s.enabled} onClick={() => toggle(s)} />
            </div>
            {(s.url_param || s.env_params.length > 0) && (
              <div className="mt-2 flex flex-col gap-1.5">
                {s.url_param && (
                  <input
                    defaultValue={s.params.command ?? ""}
                    onBlur={(e) => setParam(s, "command", e.target.value)}
                    placeholder="commande stdio (ex. npx -y mon-mcp) ou URL"
                    className="border-2 border-line bg-card px-2 py-1 text-[12px] text-ink"
                  />
                )}
                {s.env_params.map((k) => (
                  <input
                    key={k}
                    defaultValue={s.params[k] ?? ""}
                    onBlur={(e) => setParam(s, k, e.target.value)}
                    placeholder={k}
                    className="border-2 border-line bg-card px-2 py-1 text-[12px] text-ink"
                  />
                ))}
              </div>
            )}
            {s.error && (
              <div className="mt-1.5 text-[12px] text-warn">{s.error}</div>
            )}
            {testResult[s.id] && (
              <div className="mt-1.5 text-[12px] text-muted-2">
                {testResult[s.id]}
              </div>
            )}
          </div>
        ))}
      </div>
    </Card>
  );
}

function HardwareCard() {
  const [hw, setHw] = useState<HardwareInfo | null>(null);

  const refresh = () => getHardware().then(setHw).catch(() => {});
  // 15 s et rien quand l'onglet est caché : chaque appel lance nvidia-smi côté
  // serveur. Le bouton ↻ reste là pour un rafraîchissement immédiat.
  useEffect(() => {
    refresh();
    const id = setInterval(() => {
      if (!document.hidden) refresh();
    }, 15000);
    return () => clearInterval(id);
  }, []);

  return (
    <Card>
      <div className="mb-3.5 flex items-center justify-between">
        <div className="font-pixel text-[11px] text-ink">MATÉRIEL</div>
        <button
          onClick={refresh}
          className="flex h-7 items-center gap-1.5 border-2 border-line bg-card-soft px-2.5 text-[13px] text-muted-2"
        >
          <RefreshIcon size={12} />
          Rafraîchir
        </button>
      </div>

      {/* GPU vu par le conteneur Loki */}
      <div className="mb-1 text-[12px] font-bold uppercase tracking-wide text-label">
        GPU vu par Loki (conteneur)
      </div>
      {hw?.loki_gpus.length ? (
        hw.loki_gpus.map((g) => (
          <div key={g.index} className="border-2 border-line bg-base px-3 py-2 text-[13px]">
            <div className="flex items-center justify-between">
              <span className="text-ink">{g.name}</span>
              <span className="text-muted-2">
                {(g.vram_used_mb / 1024).toFixed(1)}/
                {(g.vram_total_mb / 1024).toFixed(1)} Go · {g.util_pct.toFixed(0)}%
              </span>
            </div>
          </div>
        ))
      ) : hw?.gpu_override ? (
        <div className="border-2 border-line bg-base px-3 py-2 text-[13px] text-ink">
          {hw.gpu_override.name} ·{" "}
          {(hw.gpu_override.vram_total_mb / 1024).toFixed(0)} Go{" "}
          <span className="text-muted-2">(valeur déclarée)</span>
          <div className="mt-1 text-[11px] text-muted-2">
            C'est la valeur de <code>GPU_VRAM_MB</code> que tu as fixée, pas une
            détection réelle. Corrige-la (ex. 16000 pour 16 Go) ou mets 0.
          </div>
        </div>
      ) : (
        <div className="border-2 border-line bg-base px-3 py-2 text-[12px] text-muted-2">
          Aucun GPU visible depuis le conteneur — normal si Ollama tourne sur une
          autre machine. Déclare <code>GPU_VRAM_MB</code> pour l'auto-réglage.
        </div>
      )}

      {/* GPU utilisé par Ollama */}
      <div className="mb-1 mt-3 text-[12px] font-bold uppercase tracking-wide text-label">
        Ollama {hw?.ollama_is_local ? "(local)" : "(distant)"}
      </div>
      <div className="border-2 border-line bg-base px-3 py-2 text-[13px]">
        <div className="mb-1 flex items-center gap-2">
          <span
            className={`h-2 w-2 border-2 border-line ${
              hw?.ollama.connected ? "bg-ok" : "bg-warn"
            }`}
          />
          <span className="min-w-0 flex-1 truncate text-muted-2" title={hw?.ollama.host}>
            {hw?.ollama.host}
          </span>
          {hw?.ollama.version && (
            <span className="text-[11px] text-muted-3">v{hw.ollama.version}</span>
          )}
        </div>
        {hw?.ollama.running.length ? (
          hw.ollama.running.map((m) => (
            <div key={m.name} className="flex items-center gap-2 py-[2px]">
              <span className="min-w-0 flex-1 truncate text-ink" title={m.name}>
                {m.name}
              </span>
              <span
                className={
                  m.processor === "GPU"
                    ? "text-ok"
                    : m.processor === "CPU"
                      ? "text-warn"
                      : "text-ink"
                }
              >
                {m.processor}
                {m.processor === "mixte" ? ` ${m.gpu_percent}%` : ""}
              </span>
              <span className="text-[11px] text-muted-3">
                {(m.vram_mb / 1024).toFixed(1)}G VRAM
              </span>
            </div>
          ))
        ) : (
          <div className="text-[12px] text-muted-2">
            Aucun modèle chargé actuellement.
          </div>
        )}
      </div>

      <div className="mt-2 text-[11px] leading-relaxed text-muted-3">{hw?.note}</div>
    </Card>
  );
}

function BenchCard() {
  const selectedModel = useStore((s) => s.selectedModel);
  const [scores, setScores] = useState<Record<string, BenchResult>>({});
  const [running, setRunning] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [progress, setProgress] = useState<
    { task: string; score: number | null; detail?: string }[]
  >([]);

  useEffect(() => {
    getBenchScores().then(setScores).catch(() => {});
  }, []);

  const launch = async () => {
    if (!selectedModel || running) return;
    setRunning(true);
    setError(null);
    setProgress([]);
    try {
      const result = await runBench(selectedModel, (task, score, detail) => {
        setProgress((p) => {
          const rest = p.filter((x) => x.task !== task);
          return [...rest, { task, score, detail }];
        });
      });
      if (result) setScores((s) => ({ ...s, [selectedModel]: result }));
    } catch (err) {
      setError(err instanceof Error ? err.message : "benchmark impossible");
    } finally {
      setRunning(false);
    }
  };

  const current = scores[selectedModel];

  return (
    <Card>
      <div className="mb-3.5 flex items-center justify-between">
        <div className="font-pixel text-[11px] text-ink">BENCHMARK</div>
        <button
          onClick={launch}
          disabled={running || !selectedModel}
          className="flex h-8 items-center gap-1.5 border-[3px] border-line bg-accent px-3 text-[13px] text-white shadow-accent-soft disabled:opacity-40"
          style={{ borderRadius: 7 }}
        >
          {running ? "ÉVALUATION…" : "TESTER CE MODÈLE"}
        </button>
      </div>

      <div className="mb-2 text-[13px] text-muted-2">
        5 mini-épreuves (outils, code, consignes, JSON, format) — score /100
        pour <b className="text-ink">{selectedModel || "—"}</b>.
      </div>

      {running && (
        <div className="border-2 border-line bg-base px-3 py-2">
          {progress.map((p) => (
            <div key={p.task} className="flex items-center gap-2 py-[2px] text-[13px]">
              <span className="flex-1 text-ink-2">{p.task}</span>
              {p.score === null ? (
                <span className="text-muted-3">…</span>
              ) : (
                <span className={p.score >= 14 ? "text-ok" : p.score >= 8 ? "text-ink" : "text-warn"}>
                  {p.score}/20
                </span>
              )}
            </div>
          ))}
        </div>
      )}

      {error && (
        <div className="border-2 border-warn bg-base px-3 py-2 text-[12px] text-warn">
          Échec du test : {error}
        </div>
      )}

      {!running && current && (
        <div className="border-2 border-line bg-base px-3 py-2">
          <div className="mb-1 flex items-center justify-between">
            <span className="text-[13px] text-ink">Score</span>
            <span
              className={`font-pixel text-[13px] ${
                current.score >= 70 ? "text-ok" : current.score >= 45 ? "text-ink" : "text-warn"
              }`}
            >
              {current.score}/100
            </span>
          </div>
          {current.details.map((d) => (
            <div key={d.task} className="flex items-center gap-2 py-[2px] text-[12px]">
              <span className="flex-1 text-muted-2">{d.task}</span>
              <span className="text-muted" title={d.detail}>{d.score}/20</span>
            </div>
          ))}
        </div>
      )}

      {Object.keys(scores).length > 1 && (
        <div className="mt-2 text-[12px] text-muted-2">
          Autres :{" "}
          {Object.entries(scores)
            .filter(([m]) => m !== selectedModel)
            .map(([m, r]) => `${m} (${r.score})`)
            .join(" · ")}
        </div>
      )}
    </Card>
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

/** Repli « Avancé » : sort les réglages rares du chemin principal.
 *
 * <details> natif : pas d'état à gérer, repliement mémorisé par le navigateur
 * le temps de la page, et accessible au clavier sans code supplémentaire.
 */
function Advanced({
  children,
  label = "Réglages avancés",
}: {
  children: React.ReactNode;
  label?: string;
}) {
  return (
    <details className="group mt-3 border-t-2 border-line-soft pt-3">
      <summary className="flex cursor-pointer select-none items-center gap-1.5 text-[13px] text-muted-2 marker:content-['']">
        <span className="transition-transform group-open:rotate-90">▸</span>
        {label}
      </summary>
      <div className="mt-3">{children}</div>
    </details>
  );
}

/** Ligne interrupteur : libellé + description + bascule. */
function ToggleRow({
  label,
  desc,
  on,
  onClick,
  first,
}: {
  label: string;
  desc: string;
  on: boolean;
  onClick: () => void;
  first?: boolean;
}) {
  return (
    <div
      className={`flex items-center gap-3 py-2 ${
        first ? "" : "border-t-2 border-line-soft"
      }`}
    >
      <span className="flex-1">
        <span className="block text-[14px] text-ink">{label}</span>
        <span className="block text-[12px] text-muted-2">{desc}</span>
      </span>
      <Toggle on={on} onClick={onClick} />
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
