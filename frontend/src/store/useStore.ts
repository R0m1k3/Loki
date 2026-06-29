import { create } from "zustand";
import {
  getModels,
  getStatus,
  type OllamaModel,
  type OllamaStatus,
} from "../api/client";

interface LokiState {
  status: OllamaStatus | null;
  models: OllamaModel[];
  selectedModel: string;
  loadingModels: boolean;

  setSelectedModel: (name: string) => void;
  refreshStatus: () => Promise<void>;
  refreshModels: () => Promise<void>;
}

export const useStore = create<LokiState>((set, get) => ({
  status: null,
  models: [],
  selectedModel: "",
  loadingModels: false,

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
}));
