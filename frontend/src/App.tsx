import { useEffect, useState } from "react";
import { ActivityBar, type View } from "./components/ActivityBar";
import { TopBar } from "./components/TopBar";
import { LeftPanel } from "./panels/LeftPanel";
import { ChatPanel } from "./panels/ChatPanel";
import { PreviewPanel } from "./panels/PreviewPanel";
import { SettingsView } from "./panels/SettingsView";
import { useStore } from "./store/useStore";

export default function App() {
  const [view, setView] = useState<View>("chat");
  const { refreshStatus, refreshModels } = useStore();

  // Au démarrage : statut Ollama + liste des modèles. Poll du statut.
  useEffect(() => {
    refreshStatus();
    refreshModels();
    const id = setInterval(refreshStatus, 10000);
    return () => clearInterval(id);
  }, [refreshStatus, refreshModels]);

  return (
    <div className="flex h-full flex-col bg-base text-ink">
      <TopBar />
      <div className="flex min-h-0 flex-1">
        <ActivityBar active={view} onChange={setView} />

        {view === "settings" ? (
          <SettingsView />
        ) : (
          <>
            <LeftPanel />
            <ChatPanel />
            <PreviewPanel />
          </>
        )}
      </div>
    </div>
  );
}
