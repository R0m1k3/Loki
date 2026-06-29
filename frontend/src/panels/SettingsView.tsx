import { useState } from "react";
import { useStore } from "../store/useStore";
import { pullModel } from "../api/client";
import { DownloadIcon, RefreshIcon } from "../components/Icon";

/** Vue Configuration (Frame 2) — Modèle & génération, outils, invite système. */
export function SettingsView() {
  const { models, selectedModel, setSelectedModel, refreshModels, status } =
    useStore();

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
          <div className="flex items-center gap-1.5 font-mono text-[11.5px] text-ok">
            <span className="h-1.5 w-1.5 rounded-full bg-ok" />
            {status?.connected ? "Ollama connecté" : "Ollama déconnecté"}
          </div>
        </div>

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
                      {on && (
                        <span className="h-2 w-2 rounded-full bg-accent" />
                      )}
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
              <Slider label="Température" value="0.7" pct={35} />
              <Slider label="Top-P" value="0.90" pct={90} />
              <Slider label="Top-K" value="40" pct={40} />
              <Slider label="Jetons max" value="2048" pct={25} last />
            </div>

            <div className="rounded-card border border-line bg-card p-[18px]">
              <div className="mb-3.5 flex items-center justify-between">
                <div className="text-sm font-bold">Outils de l'agent</div>
                <span className="font-mono text-[11px] text-muted-2">
                  bientôt
                </span>
              </div>
              {[
                ["read_file", "Lire un fichier du projet", true],
                ["write_file", "Créer / modifier un fichier", true],
                ["list_dir", "Lister un répertoire", true],
                ["html_preview", "Aperçu HTML en direct", true],
                ["web_search", "Recherche web", false],
                ["run_shell", "Exécuter une commande · sensible", false],
              ].map(([name, desc, on], i) => (
                <div
                  key={name as string}
                  className={`flex items-center gap-3 py-[9px] ${
                    i > 0 ? "border-t border-line-soft" : ""
                  }`}
                >
                  <span className="w-32 flex-none font-mono text-[12.5px] text-ink-2">
                    {name}
                  </span>
                  <span className="flex-1 text-xs text-muted-2">{desc}</span>
                  <span
                    className="relative h-5 w-[34px] rounded-full"
                    style={{ background: on ? "#f0a15c" : "#3a342c" }}
                  >
                    <span
                      className="absolute top-0.5 h-4 w-4 rounded-full"
                      style={{
                        background: on ? "#1f1b16" : "#6f675c",
                        left: on ? "auto" : "2px",
                        right: on ? "2px" : "auto",
                      }}
                    />
                  </span>
                </div>
              ))}
            </div>
          </div>
        </div>

        {/* Invite système */}
        <div className="mt-5 rounded-card border border-line bg-card p-[18px]">
          <div className="mb-3 flex items-center justify-between">
            <div className="text-sm font-bold">Invite système</div>
            <span className="font-mono text-[11px] text-muted-2">
              éditeur à venir
            </span>
          </div>
          <div className="rounded-[10px] border border-line bg-sunken p-4 font-mono text-[12.5px] leading-[1.7] text-ink-3">
            <span className="text-ok"># Rôle</span>
            <br />
            Tu es un assistant de développement local. Tu travailles dans le
            dossier <span className="text-accent">workspace/</span> et tu écris
            du code clair, commenté en français.
            <br />
            <br />
            <span className="text-ok"># Règles</span>
            <br />
            — Demande confirmation avant toute commande shell.
            <br />— Après chaque écriture de fichier, propose un aperçu.
          </div>
        </div>
      </div>
    </div>
  );
}

function Slider({
  label,
  value,
  pct,
  last,
}: {
  label: string;
  value: string;
  pct: number;
  last?: boolean;
}) {
  return (
    <div className={last ? "" : "mb-4"}>
      <div className="mb-2 flex items-center justify-between">
        <span className="text-[13px] text-ink-2">{label}</span>
        <span className="font-mono text-xs font-semibold text-accent">
          {value}
        </span>
      </div>
      <div className="relative h-1.5 rounded bg-line">
        <div
          className="absolute left-0 top-0 h-1.5 rounded bg-accent"
          style={{ width: `${pct}%` }}
        />
        <div
          className="absolute top-1/2 h-4 w-4 -translate-x-1/2 -translate-y-1/2 rounded-full border-[3px] border-card bg-accent"
          style={{ left: `${pct}%` }}
        />
      </div>
    </div>
  );
}
