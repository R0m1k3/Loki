import { useEffect, useState } from "react";
import { Sidebar, type View } from "./components/Sidebar";
import { TopBar } from "./components/TopBar";
import { ChatPanel } from "./panels/ChatPanel";
import { PreviewPanel } from "./panels/PreviewPanel";
import { SettingsView } from "./panels/SettingsView";
import { HistoryView, ToolsView } from "./panels/ActivityViews";
import { useStore } from "./store/useStore";

export default function App() {
  const [view, setView] = useState<View>("chat");
  const { refreshPulse, refreshModels, refreshSessions } = useStore();

  // Sondage frugal. Trois principes, contre l'app qui chauffait le CPU à vide :
  //   - UNE seule requête groupée (/api/system/pulse) au lieu de trois ;
  //   - RIEN quand l'onglet est caché (l'utilisateur ne regarde pas) ;
  //   - cadence adaptative : réactif pendant le travail, lent au repos.
  // Le serveur, lui, ne tourne jamais en boucle : sans navigateur, zéro CPU.
  useEffect(() => {
    refreshPulse();
    refreshModels();
    refreshSessions();

    let timer: number | undefined;
    const tick = () => {
      // Onglet caché : on ne sonde pas du tout, on repassera au retour.
      if (document.hidden) return schedule();
      refreshPulse().finally(schedule);
    };
    const schedule = () => {
      if (document.hidden) {
        timer = window.setTimeout(schedule, 30000);
        return;
      }
      // Pendant un flux, les indicateurs (VRAM, GPU) doivent suivre.
      const busy = useStore.getState().streaming;
      timer = window.setTimeout(tick, busy ? 4000 : 20000);
    };
    schedule();

    // Retour sur l'onglet : rafraîchit tout de suite plutôt que d'attendre.
    const onVisible = () => {
      if (!document.hidden) {
        window.clearTimeout(timer);
        tick();
      }
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      window.clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [refreshPulse, refreshModels, refreshSessions]);

  return (
    <div className="flex h-full flex-col bg-base text-ink">
      <TopBar />
      <div className="flex min-h-0 flex-1">
        <Sidebar active={view} onChange={setView} />

        {view === "chat" && (
          <>
            <ChatPanel />
            <PreviewPanel />
          </>
        )}
        {view === "history" && <HistoryView onOpen={() => setView("chat")} />}
        {view === "tools" && <ToolsView onSettings={() => setView("settings")} />}
        {view === "settings" && <SettingsView />}
      </div>
    </div>
  );
}
