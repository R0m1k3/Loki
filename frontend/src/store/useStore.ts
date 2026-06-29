import { create } from "zustand";
import {
  createSession,
  deleteSession,
  getModels,
  getSession,
  getStatus,
  listSessions,
  streamChat,
  type Message,
  type OllamaModel,
  type OllamaStatus,
  type Session,
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
  streamContent: string; // réponse de l'agent en cours de frappe

  setSelectedModel: (name: string) => void;
  refreshStatus: () => Promise<void>;
  refreshModels: () => Promise<void>;

  refreshSessions: () => Promise<void>;
  newSession: () => Promise<void>;
  openSession: (id: string) => Promise<void>;
  removeSession: (id: string) => Promise<void>;
  sendMessage: (content: string) => Promise<void>;
}

export const useStore = create<LokiState>((set, get) => ({
  status: null,
  models: [],
  selectedModel: "",
  loadingModels: false,

  sessions: [],
  currentSessionId: null,
  messages: [],
  streaming: false,
  streamContent: "",

  setSelectedModel: (name) => set({ selectedModel: name }),

  refreshStatus: async () => {
    try {
      const status = await getStatus();
      set({ status });
      if (!get().selectedModel && status.default_model) {
        set({ selectedModel: status.default_model });
      }
    } catch {
      set({ status: { connected: false, host: "", default_model: "" } });
    }
  },

  refreshModels: async () => {
    set({ loadingModels: true });
    try {
      const { models, default: def } = await getModels();
      set({ models });
      if (!get().selectedModel && def) set({ selectedModel: def });
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
    set({ currentSessionId: s.id, messages: [], streamContent: "" });
    await get().refreshSessions();
  },

  openSession: async (id) => {
    const { messages } = await getSession(id);
    set({ currentSessionId: id, messages, streamContent: "" });
  },

  removeSession: async (id) => {
    await deleteSession(id);
    if (get().currentSessionId === id) {
      set({ currentSessionId: null, messages: [] });
    }
    await get().refreshSessions();
  },

  sendMessage: async (content) => {
    if (get().streaming) return;
    content = content.trim();
    if (!content) return;

    // Crée une session à la volée si aucune n'est ouverte.
    let sid = get().currentSessionId;
    if (!sid) {
      const s = await createSession(get().selectedModel || undefined);
      sid = s.id;
      set({ currentSessionId: s.id });
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
      streamContent: "",
    });

    await streamChat(
      { session_id: sid, content, model: get().selectedModel || undefined },
      {
        onToken: (t) => set({ streamContent: get().streamContent + t }),
        onDone: async () => {
          set({ streaming: false, streamContent: "" });
          // Recharge depuis la base pour récupérer les ids/horodatages réels.
          if (get().currentSessionId === sid) await get().openSession(sid!);
          await get().refreshSessions();
        },
        onError: (msg) => {
          const errMsg: Message = {
            id: `err-${Date.now()}`,
            session_id: sid!,
            role: "assistant",
            content: `⚠️ Erreur : ${msg}`,
            created_at: Date.now() / 1000,
          };
          set({
            streaming: false,
            streamContent: "",
            messages: [...get().messages, errMsg],
          });
        },
      }
    );
  },
}));
