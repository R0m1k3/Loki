package loki

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Aller-retour complet en base : ce qui est enregistré se relit à l'identique,
// la liste est triée par nom (affichage stable) et la suppression efface bien.
func TestTasksCRUD(t *testing.T) {
	testHome(t)
	if got := listTasks(); len(got) != 0 {
		t.Fatalf("base neuve : %d tâches, attendu 0", len(got))
	}
	a := Task{ID: newTaskID(), Name: "zèbre", Prompt: "p", Schedule: "@every 2h", Enabled: true}
	b := Task{ID: newTaskID(), Name: "abeille", Prompt: "p", Schedule: "0 9 * * 1-5"}
	if err := saveTask(a); err != nil {
		t.Fatal(err)
	}
	if err := saveTask(b); err != nil {
		t.Fatal(err)
	}
	list := listTasks()
	if len(list) != 2 || list[0].Name != "abeille" || list[1].Name != "zèbre" {
		t.Fatalf("liste non triée par nom : %+v", list)
	}
	got, ok := getTask(a.ID)
	if !ok || got.Prompt != "p" || got.Schedule != "@every 2h" || !got.Enabled {
		t.Fatalf("relecture incorrecte : %+v (ok=%v)", got, ok)
	}
	if err := deleteTask(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := getTask(a.ID); ok {
		t.Error("tâche supprimée toujours présente")
	}
	if len(listTasks()) != 1 {
		t.Error("la suppression n'a pas retiré la tâche de la liste")
	}
}

// L'interrupteur maître est un FREIN : base neuve = tâches actives, sans
// dépendre d'une écriture préalable.
func TestTasksPauseDefautActif(t *testing.T) {
	testHome(t)
	if tasksPaused() {
		t.Error("base neuve : les tâches devraient être actives")
	}
	if err := setTasksPaused(true); err != nil {
		t.Fatal(err)
	}
	if !tasksPaused() {
		t.Error("pause non enregistrée")
	}
	_ = setTasksPaused(false)
	if tasksPaused() {
		t.Error("reprise non enregistrée")
	}
}

// Le dossier de travail d'une tâche lui appartient : jamais celui d'une
// discussion (qui disparaît avec elle), et stable d'un passage à l'autre — une
// tâche qui tient un fichier de suivi doit le retrouver.
func TestTaskWorkspaceIsole(t *testing.T) {
	testHome(t)
	dir := taskWorkspaceDir("abc123")
	if dir == agentWorkspace() || dir == convWorkspace() {
		t.Fatalf("la tâche partage le dossier d'une discussion : %s", dir)
	}
	if filepath.Base(dir) != "abc123" || filepath.Base(filepath.Dir(dir)) != "tasks" {
		t.Errorf("emplacement inattendu : %s", dir)
	}
	if again := taskWorkspaceDir("abc123"); again != dir {
		t.Errorf("dossier instable entre deux passages : %s puis %s", dir, again)
	}
	// La bascule ne doit toucher QUE les outils : ce que voit l'utilisateur
	// (panneau Fichiers, dépôts) reste sur sa discussion.
	setTaskWorkspace(dir)
	defer setTaskWorkspace("")
	if agentCwd() != dir {
		t.Errorf("agentCwd = %s, attendu %s", agentCwd(), dir)
	}
	if convWorkspace() == dir {
		t.Error("convWorkspace suit la tâche : le panneau Fichiers changerait de dossier en cours de route")
	}
}

// Les accès d'une tâche : l'agent commande tout, la tâche ne peut que RETIRER
// (no_mem/no_web), et le mode code reste hors sujet sans personne en face.
func TestTaskCaps(t *testing.T) {
	testHome(t)
	if c := taskCaps(Task{}); c.Agent || c.Internet || c.Mem != MemOff {
		t.Errorf("agent coupé : aucun outil attendu, obtenu %+v", c)
	}
	if err := setAgentEnabled(true); err != nil {
		t.Fatal(err)
	}
	c := taskCaps(Task{})
	if !c.Agent {
		t.Fatal("mode agent actif : la tâche doit avoir les outils")
	}
	if c.Code {
		t.Error("le mode code n'a pas de sens pour une tâche autonome")
	}
	// Par défaut, la tâche suit le mode mémoire de la machine.
	if c.Mem != memMode() {
		t.Errorf("mémoire = %q, attendu celle de la machine (%q)", c.Mem, memMode())
	}
	// Machine mémoire COUPÉE mais tâche qui la demande : elle garde au moins les
	// outils à la demande, sans l'injection proactive.
	if err := setMemMode(MemOff); err != nil {
		t.Fatal(err)
	}
	if got := taskCaps(Task{}).Mem; got != MemOnDemand {
		t.Errorf("mémoire = %q, attendu %q", got, MemOnDemand)
	}
	if c := taskCaps(Task{NoMem: true, NoWeb: true}); c.Mem != MemOff || c.Internet {
		t.Errorf("refus explicite non respecté : %+v", c)
	}
	// Web : borné à ce que la machine offre réellement — une tâche ne peut pas
	// l'inventer (aucun serveur Crawl4AI joignable ici).
	if c := taskCaps(Task{}); c.Internet {
		t.Error("accès web accordé alors que la machine ne l'offre pas")
	}
}

// Fin d'exécution : succès, échec et interruption volontaire n'écrivent pas la
// même chose — et une interruption n'est PAS un échec (pas d'indicateur rouge).
func TestRecordTaskEnd(t *testing.T) {
	testHome(t)
	base := Task{ID: "t1", Name: "n", Prompt: "p", Schedule: "@every 1h", Enabled: true}
	if err := saveTask(base); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-2 * time.Second)

	recordTaskEnd("t1", start, strings.Repeat("x", reportMax+50), nil)
	got, _ := getTask("t1")
	if !got.LastOK || got.LastError != "" {
		t.Errorf("succès mal enregistré : %+v", got)
	}
	if len(got.LastReport) != reportMax+len("…") {
		t.Errorf("compte-rendu non borné : %d caractères", len(got.LastReport))
	}
	if got.LastRun == 0 || got.NextRun <= got.LastRun {
		t.Errorf("horodatages incohérents : last=%d next=%d", got.LastRun, got.NextRun)
	}
	if got.LastDurMs < 1000 {
		t.Errorf("durée non mesurée : %d ms", got.LastDurMs)
	}

	recordTaskEnd("t1", start, "", errors.New("boum"))
	got, _ = getTask("t1")
	if got.LastOK || got.LastError != "boum" {
		t.Errorf("échec mal enregistré : %+v", got)
	}

	recordTaskEnd("t1", start, "", context.Canceled)
	got, _ = getTask("t1")
	if !got.LastOK || got.LastError != "" || !strings.Contains(got.LastReport, "interrompue") {
		t.Errorf("interruption prise pour un échec : %+v", got)
	}

	// Tâche supprimée entre-temps : on n'en ressuscite pas une.
	recordTaskEnd("disparue", start, "r", nil)
	if _, ok := getTask("disparue"); ok {
		t.Error("recordTaskEnd a recréé une tâche supprimée")
	}
}

// Le tic ne déclenche rien quand l'interrupteur maître est en pause, et pose le
// NextRun manquant sans lancer la tâche pour autant.
func TestTickTasks(t *testing.T) {
	testHome(t)
	now := time.Now()
	due := Task{ID: "due", Name: "due", Prompt: "p", Schedule: "@every 1h", Enabled: true,
		NextRun: now.Add(-time.Minute).UnixMilli()}
	if err := saveTask(due); err != nil {
		t.Fatal(err)
	}
	_ = setTasksPaused(true)
	tickTasks(now)
	if got, _ := getTask("due"); got.LastRun != 0 {
		t.Error("une tâche s'est exécutée alors que tout est suspendu")
	}
	_ = setTasksPaused(false)

	// NextRun absent : le tic le pose (relatif à maintenant) sans exécuter.
	neuve := Task{ID: "neuve", Name: "neuve", Prompt: "p", Schedule: "@every 2h", Enabled: true}
	if err := saveTask(neuve); err != nil {
		t.Fatal(err)
	}
	primeNextRuns()
	got, _ := getTask("neuve")
	if got.NextRun == 0 || got.LastRun != 0 {
		t.Errorf("amorçage incorrect : next=%d last=%d", got.NextRun, got.LastRun)
	}
	if d := time.UnixMilli(got.NextRun).Sub(now); d < 110*time.Minute || d > 130*time.Minute {
		t.Errorf("prochain passage à %v, attendu ~2 h", d)
	}
	// Tâche désactivée : jamais amorcée, jamais lancée.
	off := Task{ID: "off", Name: "off", Prompt: "p", Schedule: "@every 1h"}
	if err := saveTask(off); err != nil {
		t.Fatal(err)
	}
	primeNextRuns()
	if got, _ := getTask("off"); got.NextRun != 0 {
		t.Error("une tâche désactivée a été planifiée")
	}
}

// Une tâche épinglée sur un preset qui n'existe plus échoue PROPREMENT : elle
// est marquée en erreur (et surtout, elle ne tourne pas sur le mauvais modèle).
func TestRunTaskPresetIntrouvable(t *testing.T) {
	testHome(t)
	tk := Task{ID: "p1", Name: "n", Prompt: "p", Schedule: "@every 1h", Enabled: true, Preset: "fantome"}
	if err := saveTask(tk); err != nil {
		t.Fatal(err)
	}
	runTask(tk)
	got, _ := getTask("p1")
	if got.LastOK || !strings.Contains(got.LastError, "preset") {
		t.Errorf("échec de bascule mal signalé : %+v", got)
	}
}

// La note de contexte dit à l'IA qu'elle tourne seule, et lui redonne son
// compte-rendu précédent — borné, pour ne pas manger le contexte.
func TestTaskContextNote(t *testing.T) {
	n := taskContextNote("Veille mails", "")
	if !strings.Contains(n, "Veille mails") || !strings.Contains(n, "sans utilisateur") {
		t.Errorf("note incomplète : %q", n)
	}
	if strings.Contains(n, "dernière exécution") {
		t.Error("premier passage : il n'y a pas de compte-rendu à rappeler")
	}
	long := taskContextNote("t", strings.Repeat("y", reportMax+100))
	if !strings.Contains(long, "dernière exécution") {
		t.Error("le compte-rendu précédent doit être réinjecté")
	}
	if len(long) > reportMax+600 {
		t.Errorf("note non bornée : %d caractères", len(long))
	}
}

// L'API refuse une tâche bancale AVANT de l'enregistrer : sans ça une fréquence
// illisible donnerait une tâche qui ne se déclenche jamais, sans rien dire.
func TestHandleTaskSaveValide(t *testing.T) {
	testHome(t)
	post := func(body string) (int, map[string]any) {
		rr := httptest.NewRecorder()
		handleTaskSave(rr, httptest.NewRequest("POST", "/api/tasks/save", strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out
	}
	if code, out := post(`{"name":"","prompt":"p","schedule":"@every 1h"}`); code != 400 {
		t.Errorf("nom vide accepté : %d %v", code, out)
	}
	if code, out := post(`{"name":"n","prompt":"","schedule":"@every 1h"}`); code != 400 {
		t.Errorf("consigne vide acceptée : %d %v", code, out)
	}
	if code, out := post(`{"name":"n","prompt":"p","schedule":"tous les jours"}`); code != 400 {
		t.Errorf("fréquence illisible acceptée : %d %v", code, out)
	}
	if len(listTasks()) != 0 {
		t.Fatal("une tâche refusée a quand même été enregistrée")
	}

	code, out := post(`{"name":"n","prompt":"p","schedule":"0 9 * * 1-5","tz":"Europe/Paris","enabled":true}`)
	if code != 200 || out["ok"] != true {
		t.Fatalf("création refusée : %d %v", code, out)
	}
	id, _ := out["id"].(string)
	got, ok := getTask(id)
	if !ok || got.TZ != "Europe/Paris" || got.NextRun == 0 {
		t.Fatalf("tâche mal créée : %+v", got)
	}

	// Édition : l'historique d'exécution SURVIT (on ne repart pas de zéro parce
	// qu'on a corrigé une faute dans la consigne).
	got.LastRun, got.LastReport, got.LastOK = 42, "vu hier", true
	if err := saveTask(got); err != nil {
		t.Fatal(err)
	}
	if code, out := post(`{"id":"` + id + `","name":"n2","prompt":"p2","schedule":"0 9 * * 1-5","enabled":true}`); code != 200 {
		t.Fatalf("édition refusée : %d %v", code, out)
	}
	after, _ := getTask(id)
	if after.Name != "n2" || after.Prompt != "p2" {
		t.Errorf("édition non appliquée : %+v", after)
	}
	if after.LastRun != 42 || after.LastReport != "vu hier" {
		t.Errorf("historique perdu à l'édition : %+v", after)
	}
}

// Un message envoyé pendant qu'une tâche tourne doit dire LAQUELLE occupe le
// modèle : « génération en cours » sur un fil vide et immobile n'explique rien.
func TestBusyReasonNommeLaTache(t *testing.T) {
	testHome(t)
	if err := conv.busyReason(); err != ErrBusy {
		t.Errorf("hors tâche : %v, attendu %v", err, ErrBusy)
	}
	conv.mu.Lock()
	conv.runningTaskName = "Veille mails"
	conv.mu.Unlock()
	defer func() {
		conv.mu.Lock()
		conv.runningTaskName = ""
		conv.mu.Unlock()
	}()
	if err := conv.busyReason(); err == nil || !strings.Contains(err.Error(), "Veille mails") {
		t.Errorf("la tâche n'est pas nommée : %v", err)
	}
}
