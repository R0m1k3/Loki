import { create } from "zustand";
import {
  createSession,
  deleteFile,
  deleteSession,
  getConfig,
  listProjects,
  setSessionProject,
  getModels,
  getSession,
  getStatus,
  getSystemStats,
  getLoadedModels,
  warmModel,
  fileContent,
  listFiles,
  listSessions,
  renameSession as apiRenameSession,
  runShell,
  saveConfig,
  streamChat,
  type AgentConfig,
  type FileNode,
  type Message,
  type OllamaModel,
  type OllamaStatus,
  type Session,
  type LoadedModel,
  type SystemStats,
  type ToolCall,
} from "../api/client";

interface LokiState {
  status: OllamaStatus | null;
  systemStats: SystemStats | null;
  loadedModels: LoadedModel[];
  warmingModel: string | null;
  warmError: string | null;
  models: OllamaModel[];
  selectedModel: string;
  loadingModels: boolean;

  sessions: Session[];
  currentSessionId: string | null;
  messages: Message[];
  streaming: boolean;
  streamingSessionId: string | null;
  streamContent: string; // réponse de l'agent en cours de frappe
  streamThinking: string; // raisonnement de l'agent en cours
  streamStatus: string;
  streamNotice: string | null;
  streamTools: ToolCall[]; // appels d'outils de la réponse en cours
  streamPlan: string[]; // plan de la réponse en cours

  fileTree: FileNode[];
  previewPath: string | null;
  previewContent: string;

  config: AgentConfig | null;
  availableTools: string[];
  refreshConfig: () => Promise<void>;
  updateConfig: (patch: Partial<AgentConfig>) => Promise<void>;

  pendingShell: string | null; // commande shell en attente de validation
  approveShell: () => Promise<void>;
  rejectShell: () => Promise<void>;

  openPreview: (path: string) => Promise<void>;
  setSelectedModel: (name: string) => void;
  refreshStatus: () => Promise<void>;
  refreshSystemStats: () => Promise<void>;
  refreshLoadedModels: () => Promise<void>;
  refreshModels: () => Promise<void>;
  refreshFiles: () => Promise<void>;

  refreshSessions: () => Promise<void>;
  newSession: () => Promise<void>;
  openSession: (id: string) => Promise<void>;
  removeSession: (id: string) => Promise<void>;
  renameSession: (id: string, title: string) => Promise<void>;
  removeFile: (path: string) => Promise<void>;

  projects: { name: string; files: number }[];
  refreshProjects: () => Promise<void>;
  currentProject: () => string | null;
  setProject: (name: string | null) => Promise<void>;
  sendMessage: (content: string) => Promise<void>;
  stopStreaming: () => void;

  mode: "plan" | "build" | "yolo";
  setMode: (m: "plan" | "build" | "yolo") => void;
}

let activeStreamController: AbortController | null = null;

export const useStore = create<LokiState>((set, get) => ({
  status: null,
  systemStats: null,
  loadedModels: [],
  warmingModel: null,
  warmError: null,
  models: [],
  selectedModel: "",
  loadingModels: false,

  sessions: [],
  currentSessionId: null,
  messages: [],
  streaming: false,
  streamingSessionId: null,
  streamContent: "",
  streamThinking: "",
  streamStatus: "",
  streamNotice: null,
  streamTools: [],
  streamPlan: [],
  fileTree: [],
  previewPath: null,
  previewContent: "",
  projects: [],
  config: null,
  availableTools: [],
  pendingShell: null,
  mode: "build",

  setMode: (m) => set({ mode: m }),

  approveShell: async () => {
    const cmd = get().pendingShell;
    if (!cmd) return;
    set({ pendingShell: null });
    let report: string;
    try {
      const r = await runShell(cmd);
      report =
        `J'ai validé la commande \`${cmd}\` (code ${r.exit_code}).\n` +
        `Sortie :\n\`\`\`\n${r.output || "(vide)"}\n\`\`\``;
    } catch {
      report = `Échec de l'exécution de \`${cmd}\`.`;
    }
    await get().refreshFiles();
    // On renvoie le résultat à l'agent pour qu'il poursuive.
    await get().sendMessage(report);
  },

  rejectShell: async () => {
    const cmd = get().pendingShell;
    if (!cmd) return;
    set({ pendingShell: null });
    await get().sendMessage(`J'ai refusé la commande \`${cmd}\`. N'exécute pas cette commande.`);
  },

  refreshConfig: async () => {
    const model = get().selectedModel;
    const { config, available_tools } = await getConfig(model || undefined);
    if (get().selectedModel === model) {
      set({ config, availableTools: available_tools });
    }
  },

  updateConfig: async (patch) => {
    const config = await saveConfig(patch, get().selectedModel || undefined);
    set({ config });
  },

  openPreview: async (path) => {
    const content = await fileContent(path, get().currentProject());
    set({ previewPath: path, previewContent: content });
  },

  setSelectedModel: (name) => {
    set({ selectedModel: name, warmingModel: name || null, warmError: null });
    void (async () => {
      try {
        await get().refreshConfig();
        if (!name || get().selectedModel !== name) return;
        // La configuration est propre au modèle : on attend son chargement avant
        // d'utiliser keep_alive, sinon la valeur du modèle précédent est envoyée.
        const ka = get().config?.keep_alive ?? "30m";
        const st = await warmModel(name, ka);
        await get().refreshLoadedModels();
        // Modèle chargé hors GPU : prévenir (souvent trop gros pour la VRAM).
        if (
          (st.processor === "cpu" || st.processor === "mixte") &&
          get().selectedModel === name
        ) {
          set({
            warmError:
              st.processor === "cpu"
                ? "Modèle chargé sur le CPU (trop gros pour la VRAM) — lent. Choisis un modèle plus petit ou une quantization plus légère."
                : `Modèle en partie sur GPU (${st.gpu_percent ?? "?"}%) : dépasse la VRAM, chargement plus lent.`,
          });
        }
      } catch (err) {
        if (get().selectedModel === name) {
          set({
            warmError: err instanceof Error ? err.message : "préchargement impossible",
          });
        }
      } finally {
        if (get().warmingModel === name) set({ warmingModel: null });
      }
    })();
  },

  refreshFiles: async () => {
    try {
      set({ fileTree: await listFiles(get().currentProject()) });
    } catch {
      /* workspace indisponible */
    }
  },

  refreshProjects: async () => {
    try {
      const { projects } = await listProjects();
      set({ projects });
    } catch {
      /* backend indisponible */
    }
  },

  currentProject: () => {
    const s = get().sessions.find((x) => x.id === get().currentSessionId);
    return s?.project ?? null;
  },

  setProject: async (name) => {
    const sid = get().currentSessionId;
    if (!sid) {
      const s = await createSession(get().selectedModel || undefined, name);
      set({ currentSessionId: s.id, messages: [] });
    } else {
      await setSessionProject(sid, name);
    }
    await get().refreshSessions();
    set({ previewPath: null, previewContent: "" });
    await get().refreshFiles();
  },

  refreshStatus: async () => {
    try {
      const status = await getStatus();
      set({ status });
    } catch {
      set({ status: { connected: false, host: "", default_model: "" } });
    }
  },

  refreshSystemStats: async () => {
    try {
      set({ systemStats: await getSystemStats() });
    } catch {
      set({ systemStats: null });
    }
  },

  refreshLoadedModels: async () => {
    set({ loadedModels: await getLoadedModels() });
  },

  refreshModels: async () => {
    set({ loadingModels: true });
    try {
      const { models, default: def } = await getModels();
      const installed = new Set(models.map((model) => model.name));
      const current = get().selectedModel;
      const selectedModel = installed.has(current)
        ? current
        : installed.has(def)
          ? def
          : models[0]?.name ?? "";
      set({ models });
      // Passe par l'action de sélection afin de précharger aussi le modèle choisi
      // automatiquement au démarrage (défaut ou premier modèle installé).
      get().setSelectedModel(selectedModel);
    } finally {
      set({ loadingModels: false });
    }
  },

  refreshSessions: async () => {
    const sessions = await listSessions();
    set({ sessions });
  },

  newSession: async () => {
    // La nouvelle session hérite du projet de la session courante.
    const s = await createSession(
      get().selectedModel || undefined, get().currentProject()
    );
    const streaming = get().streaming;
    set({
      currentSessionId: s.id,
      messages: [],
      ...(streaming
        ? {}
        : { streamContent: "", streamStatus: "", streamNotice: null }),
    });
    await get().refreshSessions();
  },

  openSession: async (id) => {
    const { messages } = await getSession(id);
    const streaming = get().streaming;
    set({
      currentSessionId: id,
      messages,
      ...(streaming
        ? {}
        : { streamContent: "", streamStatus: "", streamNotice: null }),
    });
    // L'arborescence suit le projet de la session ouverte.
    await get().refreshFiles();
  },

  removeSession: async (id) => {
    await deleteSession(id);
    if (get().currentSessionId === id) {
      set({ currentSessionId: null, messages: [] });
    }
    await get().refreshSessions();
  },

  renameSession: async (id, title) => {
    const clean = title.trim();
    if (!clean) return;
    await apiRenameSession(id, clean);
    await get().refreshSessions();
  },

  removeFile: async (path) => {
    await deleteFile(path, get().currentProject());
    // Ferme l'aperçu si le fichier supprimé (ou son dossier) y est affiché.
    const preview = get().previewPath;
    if (preview && (preview === path || preview.startsWith(path + "/"))) {
      set({ previewPath: null, previewContent: "" });
    }
    await get().refreshFiles();
  },

  stopStreaming: () => {
    activeStreamController?.abort();
    activeStreamController = null;
    set({
      streaming: false,
      streamingSessionId: null,
      streamContent: "",
      streamThinking: "",
      streamStatus: "",
      streamNotice: null,
      streamTools: [],
      streamPlan: [],
      pendingShell: null,
    });
    void get().refreshSessions();
  },

  sendMessage: async (content) => {
    if (get().streaming) return;
    content = content.trim();
    if (!content) return;
    if (!get().selectedModel) {
      const errMsg: Message = {
        id: `err-${Date.now()}`,
        session_id: get().currentSessionId ?? "",
        role: "assistant",
        content: "⚠️ Aucun modèle Ollama installé ou sélectionné.",
        created_at: Date.now() / 1000,
      };
      set({ messages: [...get().messages, errMsg] });
      return;
    }

    // Crée une session à la volée si aucune n'est ouverte.
    let sid = get().currentSessionId;
    try {
      if (!sid) {
        const s = await createSession(get().selectedModel || undefined);
        sid = s.id;
        set({ currentSessionId: s.id });
      }
    } catch (err) {
      const detail = err instanceof Error ? err.message : "backend injoignable";
      const errMsg: Message = {
        id: `err-${Date.now()}`,
        session_id: "",
        role: "assistant",
        content: `⚠️ Impossible de créer la session : ${detail}`,
        created_at: Date.now() / 1000,
      };
      set({ messages: [...get().messages, errMsg] });
      return;
    }

    // Affichage optimiste du message utilisateur.
    const userMsg: Message = {
      id: `tmp-${Date.now()}`,
      session_id: sid,
      role: "user",
      content,
      created_at: Date.now() / 1000,
    };
    set({
      messages: [...get().messages, userMsg],
      streaming: true,
      streamingSessionId: sid,
      streamContent: "",
      streamThinking: "",
      streamStatus: "Connexion à Ollama…",
      streamNotice: null,
      streamTools: [],
      pendingShell: null,
    });

    const controller = new AbortController();
    activeStreamController = controller;

    await streamChat(
      {
        session_id: sid,
        content,
        model: get().selectedModel || undefined,
        mode: get().mode,
      },
      {
        onToken: (t) => set({ streamContent: get().streamContent + t }),
        onThinking: (t) => set({ streamThinking: get().streamThinking + t }),
        onStatus: (message) => set({ streamStatus: message }),
        onPlan: (steps) => set({ streamPlan: steps }),
        onRevision: (content) => set({ streamContent: content }),
        onNotice: (message) => set({ streamNotice: message }),
        onToolCall: (call) =>
          set({ streamTools: [...get().streamTools, call] }),
        onToolResult: (call) => {
          // Met à jour le dernier outil correspondant (statut + résumé).
          const tools = [...get().streamTools];
          for (let i = tools.length - 1; i >= 0; i--) {
            if (tools[i].name === call.name && tools[i].status === "running") {
              tools[i] = { ...tools[i], ...call };
              break;
            }
          }
          set({ streamTools: tools });
        },
        onToolConfirm: (command) => set({ pendingShell: command }),
        onDone: async () => {
          activeStreamController = null;
          // Repère un fichier HTML écrit pour l'afficher automatiquement.
          const writtenHtml = [...get().streamTools]
            .reverse()
            .find(
              (t) =>
                t.name === "write_file" &&
                typeof t.args?.path === "string" &&
                /\.html?$/.test(t.args.path as string)
            );
          set({
            streaming: false,
            streamingSessionId: null,
            streamContent: "",
            streamThinking: "",
            streamStatus: "",
            streamNotice: null,
            streamTools: [],
          });
          // Recharge depuis la base + l'arborescence (fichiers créés par
          // l'agent) — trois requêtes indépendantes, en parallèle.
          await Promise.all([
            get().currentSessionId === sid
              ? get().openSession(sid!)
              : Promise.resolve(),
            get().refreshSessions(),
            get().refreshFiles(),
          ]);
          if (writtenHtml) await get().openPreview(writtenHtml.args.path as string);
        },
        onError: (msg) => {
          activeStreamController = null;
          const errMsg: Message = {
            id: `err-${Date.now()}`,
            session_id: sid!,
            role: "assistant",
            content: `⚠️ Erreur : ${msg}`,
            created_at: Date.now() / 1000,
          };
          set({
            streaming: false,
            streamingSessionId: null,
            streamContent: "",
            streamThinking: "",
            streamStatus: "",
            streamNotice: null,
            streamTools: [],
            messages:
              get().currentSessionId === sid
                ? [...get().messages, errMsg]
                : get().messages,
          });
          void get().refreshSessions();
        },
        onAbort: () => {
          activeStreamController = null;
          set({
            streaming: false,
            streamingSessionId: null,
            streamContent: "",
            streamThinking: "",
            streamStatus: "",
            streamNotice: null,
            streamTools: [],
            pendingShell: null,
          });
          void get().refreshSessions();
        },
      },
      controller.signal
    );
  },
}));
