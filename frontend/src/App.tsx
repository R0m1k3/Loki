import { useEffect, useState } from "react";
import { ActivityBar, type View } from "./components/ActivityBar";
import { TopBar } from "./components/TopBar";
import { LeftPanel } from "./panels/LeftPanel";
import { ChatPanel } from "./panels/ChatPanel";
import { PreviewPanel } from "./panels/PreviewPanel";
import { SettingsView } from "./panels/SettingsView";
import { FilesView, HistoryView, ToolsView } from "./panels/ActivityViews";
import { useStore } from "./store/useStore";

export default function App() {
  const [view, setView] = useState<View>("chat");
  const {
    refreshStatus,
    refreshSystemStats,
    refreshModels,
    refreshLoadedModels,
    refreshSessions,
  } = useStore();

  // Au démarrage : statut Ollama + modèles + sessions. La config est chargée
  // par setSelectedModel (via refreshModels) — pas de double fetch. Poll du
  // statut, des stats système (5 s, apparié au cache GPU serveur) et des
  // modèles chargés en VRAM (indicateur de préchargement).
  useEffect(() => {
    refreshStatus();
    refreshSystemStats();
    refreshModels();
    refreshLoadedModels();
    refreshSessions();
    const statusId = setInterval(refreshStatus, 10000);
    const statsId = setInterval(refreshSystemStats, 5000);
    const loadedId = setInterval(refreshLoadedModels, 8000);
    return () => {
      clearInterval(statusId);
      clearInterval(statsId);
      clearInterval(loadedId);
    };
  }, [refreshStatus, refreshSystemStats, refreshModels, refreshLoadedModels, refreshSessions]);

  return (
    <div className="flex h-full flex-col bg-base text-ink">
      <TopBar />
      <div className="flex min-h-0 flex-1">
        <ActivityBar active={view} onChange={setView} />

        {view === "chat" && (
          <>
            <LeftPanel />
            <ChatPanel />
            <PreviewPanel />
          </>
        )}
        {view === "history" && <HistoryView onOpen={() => setView("chat")} />}
        {view === "files" && (
          <>
            <FilesView />
            <PreviewPanel />
          </>
        )}
        {view === "tools" && <ToolsView onSettings={() => setView("settings")} />}
        {view === "settings" && <SettingsView />}
      </div>
    </div>
  );
}
