import { create } from "zustand";
import {
  createSession,
  deleteSession,
  getConfig,
  getModels,
  getSession,
  getStatus,
  fileContent,
  listFiles,
  listSessions,
  runShell,
  saveConfig,
  streamChat,
  type AgentConfig,
  type FileNode,
  type Message,
  type OllamaModel,
  type OllamaStatus,
  type Session,
  type ToolCall,
} from "../api/client";

interface LokiState {
  status: OllamaStatus | null;
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
  refreshModels: () => Promise<void>;
  refreshFiles: () => Promise<void>;

  refreshSessions: () => Promise<void>;
  newSession: () => Promise<void>;
  openSession: (id: string) => Promise<void>;
  removeSession: (id: string) => Promise<void>;
  sendMessage: (content: string) => Promise<void>;
  stopStreaming: () => void;
}

let activeStreamController: AbortController | null = null;

export const useStore = create<LokiState>((set, get) => ({
  status: null,
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
  fileTree: [],
  previewPath: null,
  previewContent: "",
  config: null,
  availableTools: [],
  pendingShell: null,

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
    const { config, available_tools } = await getConfig(
      get().selectedModel || undefined
    );
    set({ config, availableTools: available_tools });
  },

  updateConfig: async (patch) => {
    const config = await saveConfig(patch, get().selectedModel || undefined);
    set({ config });
  },

  openPreview: async (path) => {
    const content = await fileContent(path);
    set({ previewPath: path, previewContent: content });
  },

  setSelectedModel: (name) => {
    set({ selectedModel: name });
    void get().refreshConfig();
  },

  refreshFiles: async () => {
    try {
      set({ fileTree: await listFiles() });
    } catch {
      /* workspace indisponible */
    }
  },

  refreshStatus: async () => {
    try {
      const status = await getStatus();
      set({ status });
    } catch {
      set({ status: { connected: false, host: "", default_model: "" } });
    }
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
      set({ models, selectedModel });
      await get().refreshConfig();
    } finally {
      set({ loadingModels: false });
    }
  },

  refreshSessions: async () => {
    const sessions = await listSessions();
    set({ sessions });
  },

  newSession: async () => {
    const s = await createSession(get().selectedModel || undefined);
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
  },

  removeSession: async (id) => {
    await deleteSession(id);
    if (get().currentSessionId === id) {
      set({ currentSessionId: null, messages: [] });
    }
    await get().refreshSessions();
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
      { session_id: sid, content, model: get().selectedModel || undefined },
      {
        onToken: (t) => set({ streamContent: get().streamContent + t }),
        onThinking: (t) => set({ streamThinking: get().streamThinking + t }),
        onStatus: (message) => set({ streamStatus: message }),
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
          // Recharge depuis la base + l'arborescence (fichiers créés par l'agent).
          if (get().currentSessionId === sid) await get().openSession(sid!);
          await get().refreshSessions();
          await get().refreshFiles();
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
