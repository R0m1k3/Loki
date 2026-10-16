package loki

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fausseFacade : enregistre ce qui lui arrive ; /api/chat rend un flux.
type fausseFacade struct {
	mu    sync.Mutex
	vu    []string // "METHODE chemin jeton-ok corps"
	srv   *httptest.Server
	t     *testing.T
	sse   string
	reply string
	// Réponse de l'API interne (0 : 204 sans corps).
	internalStatus int
	internalBody   string
}

func demarrerFacade(t *testing.T) *fausseFacade {
	t.Helper()
	f := &fausseFacade{t: t, reply: `{"ok":true,"de":"facade"}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		tok := r.Header.Get(internalTokenHeader) == jetonTest
		f.mu.Lock()
		f.vu = append(f.vu, r.Method+" "+r.URL.Path+" "+map[bool]string{true: "jeton", false: "sans-jeton"}[tok]+" "+string(b))
		f.mu.Unlock()
		if r.URL.Path == "/api/chat" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, f.sse)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/internal/facade/") {
			f.mu.Lock()
			st, body := f.internalStatus, f.internalBody
			f.mu.Unlock()
			if st == 0 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.WriteHeader(st)
			io.WriteString(w, body)
			return
		}
		io.WriteString(w, f.reply)
	}))
	t.Cleanup(f.srv.Close)
	t.Setenv(internalTokenEnv, jetonTest)
	t.Setenv(facadeChatEnv, "rust")
	t.Setenv(facadeAddrEnv, strings.TrimPrefix(f.srv.URL, "http://"))
	return f
}

func (f *fausseFacade) recu() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.vu...)
}

// Les routes du chat arrivées ici (tunnel, redistribution E2E) partent à la
// façade avec le jeton interne ; les autres restent ici ; sans la variable,
// rien ne change.
func TestFacadeRelaieLesRoutesDuChat(t *testing.T) {
	testHome(t)
	f := demarrerFacade(t)
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }

	w := httptest.NewRecorder()
	withFacadeChat("/api/chat/send", local)(w, httptest.NewRequest("POST", "/api/chat/send", strings.NewReader(`{"message":"salut"}`)))
	if w.Body.String() != f.reply {
		t.Fatalf("réponse : %q", w.Body.String())
	}
	if got := f.recu(); len(got) != 1 || got[0] != `POST /api/chat/send jeton {"message":"salut"}` {
		t.Fatalf("reçu par la façade : %v", got)
	}
	// Mode Code, critères, export : à la façade aussi (lot 19).
	for _, p := range []string{"/api/chat/mode", "/api/chat/criteria", "/api/chat/export"} {
		w = httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, nil))
		if w.Body.String() != f.reply {
			t.Fatalf("%s non relayé : %q", p, w.Body.String())
		}
	}
	// L'aperçu du navigateur piloté aussi : le navigateur est le sien (lot 44).
	w = httptest.NewRecorder()
	withFacadeChat("/api/chat/cushot", local)(w, httptest.NewRequest("GET", "/api/chat/cushot", nil))
	if w.Body.String() != f.reply {
		t.Fatalf("aperçu non relayé : %q", w.Body.String())
	}
	// Sans LOKI_CHAT=rust : tout reste ici.
	t.Setenv(facadeChatEnv, "")
	w = httptest.NewRecorder()
	withFacadeChat("/api/chat/send", local)(w, httptest.NewRequest("POST", "/api/chat/send", nil))
	if w.Body.String() != "go" || len(f.recu()) != 5 {
		t.Fatalf("relayé sans la variable : %q", w.Body.String())
	}
}

// /api/e2e/chat suit le journal de la façade : un événement par trame, les
// battements de la façade ignorés, les nombres repartent tels quels.
func TestFacadeFluxE2E(t *testing.T) {
	testHome(t)
	f := demarrerFacade(t)
	f.sse = "data: {\"choices\":[{\"delta\":{\"pad\":\"··\"}}]}\n\n" +
		": ping\n\n" +
		"data: {\"choices\":[{\"delta\":{\"seq\":12345678901,\"user\":\"salut\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"caught_up\":true,\"id\":\"c1\"}}]}\n\n"
	w := httptest.NewRecorder()
	var trames []string
	out := &sseStream{w: w, flusher: w, mu: &sync.Mutex{}, frame: func(obj map[string]any) ([]byte, bool) {
		b, _ := json.Marshal(obj)
		trames = append(trames, string(b))
		return []byte("data: " + string(b) + "\n\n"), true
	}}
	facadeChatStream(t.Context(), []byte(`{"from":0,"tail":20}`), out)
	want := []string{`{"pad":"··"}`, `{"seq":12345678901,"user":"salut"}`, `{"caught_up":true,"id":"c1"}`}
	if strings.Join(trames, "|") != strings.Join(want, "|") {
		t.Fatalf("trames : %v", trames)
	}
	if got := f.recu(); len(got) != 1 || got[0] != `POST /api/chat jeton {"from":0,"tail":20}` {
		t.Fatalf("abonnement : %v", got)
	}
}

// Les événements d'affichage nés ici vont au journal de la façade, pas à
// celui du Go.
func TestFacadeEvenementsEtExport(t *testing.T) {
	testHome(t)
	f := demarrerFacade(t)
	resetConvForTest()
	critNotify([]Criterion{{ID: 1, Text: "les tests passent", Status: "pending"}})
	got := f.recu()
	if len(got) != 1 || !strings.HasPrefix(got[0], "POST /internal/facade/v1/delta jeton ") || !strings.Contains(got[0], "les tests passent") {
		t.Fatalf("critères : %v", got)
	}
	conv.mu.Lock()
	n := len(conv.Log)
	conv.mu.Unlock()
	if n != 0 {
		t.Fatal("événement journalisé aussi dans la conversation du Go")
	}

	// Export : la discussion enregistrée, pas la conversation du Go.
	id := convEnsureActive()
	b, _ := json.Marshal(map[string]any{"messages": []Message{{Role: "user", Content: "fil de la façade"}}, "log": []any{}, "seq": 0, "ctx_used": 0})
	if err := putStoreBytes(bkChat, convKey(id), b); err != nil {
		t.Fatal(err)
	}
	body, _, _, err := exportBody(exportOpts{Format: "json"})
	if err != nil || !strings.Contains(string(body), "fil de la façade") {
		t.Fatalf("export : %v %s", err, body)
	}
}

// Changer de projet quand la façade tient la conversation : c'est elle qui
// enregistre sa discussion et en ouvre une autre (la copie d'ici est périmée,
// l'enregistrer écraserait la sienne). Son refus (tour en cours) remonte tel
// quel ; un projet inconnu est refusé ici sans la déranger.
func TestFacadeChangeDeProjet(t *testing.T) {
	testHome(t)
	ensureDefaultProject()
	p, err := createProject("Deux")
	if err != nil {
		t.Fatal(err)
	}
	f := demarrerFacade(t)
	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handleProjects(w, httptest.NewRequest("POST", "/api/projects", strings.NewReader(body)))
		return w
	}
	before := activeProjectSlug()
	if w := post(`{"action":"switch","slug":"` + p.Slug + `"}`); w.Code != 200 {
		t.Fatalf("bascule : %d %s", w.Code, w.Body.String())
	}
	if got := f.recu(); len(got) != 1 || got[0] != `POST /internal/facade/v1/project/switch jeton {"slug":"`+p.Slug+`"}` {
		t.Fatalf("reçu par la façade : %v", got)
	}
	// Le projet actif ne change que par la façade (/internal/v1/project/activate).
	if activeProjectSlug() != before {
		t.Fatalf("projet actif changé sans la façade : %s", activeProjectSlug())
	}
	if w := post(`{"action":"switch","slug":"inconnu"}`); w.Code != 400 || len(f.recu()) != 1 {
		t.Fatalf("projet inconnu : %d, façade %v", w.Code, f.recu())
	}
	f.mu.Lock()
	f.internalStatus, f.internalBody = 409, `{"ok":false,"error":"une génération est en cours"}`
	f.mu.Unlock()
	w := post(`{"action":"switch","slug":"` + p.Slug + `"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "une génération est en cours") {
		t.Fatalf("refus de la façade : %d %s", w.Code, w.Body.String())
	}
}

// Supprimer le projet actif : la façade le quitte d'abord (discussion
// enregistrée, une du projet restant ouverte), puis supprime ses discussions.
func TestFacadeSuppressionDeProjet(t *testing.T) {
	testHome(t)
	ensureDefaultProject()
	p, err := createProject("Deux")
	if err != nil {
		t.Fatal(err)
	}
	if err := setActiveProject(p.Slug); err != nil {
		t.Fatal(err)
	}
	f := demarrerFacade(t)
	w := httptest.NewRecorder()
	handleProjects(w, httptest.NewRequest("POST", "/api/projects", strings.NewReader(`{"action":"delete","slug":"`+p.Slug+`"}`)))
	if w.Code != 200 {
		t.Fatalf("suppression : %d %s", w.Code, w.Body.String())
	}
	got := f.recu()
	if len(got) != 2 ||
		got[0] != `POST /internal/facade/v1/project/switch jeton {"slug":"`+defaultProjectSlug+`"}` ||
		got[1] != `POST /internal/facade/v1/project/drop jeton {"slug":"`+p.Slug+`"}` {
		t.Fatalf("reçu par la façade : %v", got)
	}
	if projectExists(p.Slug) {
		t.Fatal("projet toujours là")
	}
}

// /internal/v1/project/activate : le projet actif change, s'il existe.
func TestFacadeActivationDeProjet(t *testing.T) {
	testHome(t)
	ensureDefaultProject()
	p, err := createProject("Trois")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(internalTokenEnv, jetonTest)
	if w := appelOAIInterne(t, "POST", "/internal/v1/project/activate", `{"slug":"`+p.Slug+`"}`); w.Code != 204 {
		t.Fatalf("activation : %d %s", w.Code, w.Body.String())
	}
	if activeProjectSlug() != p.Slug {
		t.Fatalf("actif : %s", activeProjectSlug())
	}
	if w := appelOAIInterne(t, "POST", "/internal/v1/project/activate", `{"slug":"nulle-part"}`); w.Code != 400 {
		t.Fatalf("inconnu : %d", w.Code)
	}
}

// Plus aucun réglage ne garde la conversation ici (lot 34) : avec chacune des
// clés opt-in posée, la façade la tient et le trafic de chat lui est remis.
// Sans LOKI_CHAT=rust, ou sans son adresse : le Go, et pourquoi.
func TestFacadeAucunReglageNeGardeLaConversation(t *testing.T) {
	testHome(t)
	f := demarrerFacade(t)
	t.Setenv(internalTokenEnv, jetonTest)
	for _, k := range []string{"PROJ_SNAPSHOT", "NUDGE_IN_TOOL", "SIDE_SLOT", "SLOT_PERSIST", "KEEP_TURN_IMAGES", "PREWARM"} {
		if err := SetConfigKey(k, "on"); err != nil {
			t.Fatal(err)
		}
	}
	putStr(bkState, "retry_patterns", `["oups"]`)
	if !facadeChat() {
		t.Fatal("réglages opt-in posés : la façade doit tenir la conversation")
	}
	if w := appelOAIInterne(t, "GET", "/internal/v1/chat/holder", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"holder":"rust"`) {
		t.Fatalf("détenteur : %d %s", w.Code, w.Body.String())
	}
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }
	rec := httptest.NewRecorder()
	withFacadeChat("/api/chat/send", local)(rec, httptest.NewRequest("POST", "/api/chat/send", strings.NewReader(`{}`)))
	if rec.Body.String() == "go" || len(f.recu()) != 1 {
		t.Fatalf("pas remis à la façade : %q %v", rec.Body.String(), f.recu())
	}
	putStr(bkState, "retry_patterns", "")
	t.Setenv(facadeAddrEnv, "")
	if w := appelOAIInterne(t, "GET", "/internal/v1/chat/holder", ""); !strings.Contains(w.Body.String(), "adresse de la façade") {
		t.Fatalf("sans adresse : %s", w.Body.String())
	}
	t.Setenv(facadeChatEnv, "")
	if w := appelOAIInterne(t, "GET", "/internal/v1/chat/holder", ""); !strings.Contains(w.Body.String(), "LOKI_CHAT absent") {
		t.Fatalf("sans la variable : %s", w.Body.String())
	}
}

// LOKI_MODELS=rust : les routes des modèles partent à la façade (avec ou sans
// LOKI_CHAT), les autres restent ici ; sans la variable, rien ne change.
// Façade muette : erreur qui le dit.
func TestFacadeRelaieLesModeles(t *testing.T) {
	testHome(t)
	f := demarrerFacade(t)
	t.Setenv(facadeChatEnv, "")
	t.Setenv(facadeModelsEnv, "rust")
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }

	routes := []string{"/api/models/download", "/api/models/download/probe",
		"/api/models/download/status", "/api/models/download/cancel", "/api/models",
		"/api/models/delete", "/api/models/dirs", "/api/hf/search", "/api/hf/files", "/api/hf/token"}
	for _, p := range routes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader(`{"url":"u"}`)))
		if w.Body.String() != f.reply {
			t.Fatalf("%s : réponse %q", p, w.Body.String())
		}
	}
	if got := f.recu(); len(got) != len(routes) || got[0] != `POST /api/models/download jeton {"url":"u"}` {
		t.Fatalf("reçu par la façade : %v", got)
	}
	// Le chat, lui, reste ici (LOKI_CHAT absent), et les routes voisines aussi.
	for _, p := range []string{"/api/chat/send", "/api/mem/health", "/api/presets"} {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, nil))
		if w.Body.String() != "go" {
			t.Fatalf("%s relayé : %q", p, w.Body.String())
		}
	}
	// Façade injoignable : 502 qui nomme ce qui manque.
	f.srv.Close()
	w := httptest.NewRecorder()
	withFacadeChat("/api/models/download/status", local)(w, httptest.NewRequest("GET", "/api/models/download/status", nil))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "modèles indisponibles : ") {
		t.Fatalf("façade muette : %d %s", w.Code, w.Body.String())
	}
	// Sans LOKI_MODELS=rust : tout reste ici.
	t.Setenv(facadeModelsEnv, "")
	w = httptest.NewRecorder()
	withFacadeChat("/api/models/download", local)(w, httptest.NewRequest("POST", "/api/models/download", nil))
	if w.Body.String() != "go" {
		t.Fatalf("relayé sans la variable : %q", w.Body.String())
	}
}

// Les .part orphelins ne sont plus vidés par ce Go quand la façade tient les
// modèles : elle les vide à son démarrage, et un Go relancé seul
// effacerait celui qu'elle mène.
func TestFacadeTelechargementsGardeLesPart(t *testing.T) {
	home := testHome(t)
	dir := filepath.Join(home, "models")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(dir, "m.gguf.part")
	if err := os.WriteFile(part, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(internalTokenEnv, jetonTest)
	t.Setenv(facadeAddrEnv, "127.0.0.1:1")
	t.Setenv(facadeModelsEnv, "rust")
	cleanStartupPartFiles()
	if _, err := os.Stat(part); err != nil {
		t.Fatalf(".part effacé alors que la façade tient les téléchargements : %v", err)
	}
	t.Setenv(facadeModelsEnv, "")
	cleanStartupPartFiles()
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatalf(".part orphelin gardé : %v", err)
	}
}

// LOKI_DICTATE=rust : les routes de la dictée partent à la façade, et arrêter
// la dictée (libération de la VRAM, optimiseur) le lui demande au lieu de
// toucher au serveur de ce process. Sans la variable, rien ne change.
func TestFacadeDictee(t *testing.T) {
	testHome(t)
	f := demarrerFacade(t)
	t.Setenv(facadeChatEnv, "")
	t.Setenv(facadeDictateEnv, "rust")
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }
	for p := range facadeDictateRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if w.Body.String() != f.reply {
			t.Fatalf("%s : réponse %q", p, w.Body.String())
		}
	}
	asrShutdownVite()
	got := f.recu()
	if len(got) != len(facadeDictateRoutes)+1 || got[len(got)-1] != "POST /internal/facade/v1/dictate/stop jeton {}" {
		t.Fatalf("reçu par la façade : %v", got)
	}
	t.Setenv(facadeDictateEnv, "")
	w := httptest.NewRecorder()
	withFacadeChat("/api/transcribe", local)(w, httptest.NewRequest("POST", "/api/transcribe", nil))
	if w.Body.String() != "go" {
		t.Fatalf("relayé sans la variable : %q", w.Body.String())
	}
	asrShutdownVite()
	if n := len(f.recu()); n != len(facadeDictateRoutes)+1 {
		t.Fatalf("arrêt demandé à la façade sans la variable (%d appels)", n)
	}
}

// LOKI_SETTINGS=rust : les réglages partagés partent à la façade ; sans la
// variable, ils restent ici.
func TestFacadeReglages(t *testing.T) {
	testHome(t)
	f := demarrerFacade(t)
	t.Setenv(facadeChatEnv, "")
	t.Setenv(facadeSettingsEnv, "rust")
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }
	for p := range facadeSettingsRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("GET", p, nil))
		if w.Body.String() != f.reply {
			t.Fatalf("%s : réponse %q", p, w.Body.String())
		}
	}
	if n := len(f.recu()); n != len(facadeSettingsRoutes) {
		t.Fatalf("%d appels reçus par la façade", n)
	}
	t.Setenv(facadeSettingsEnv, "")
	w := httptest.NewRecorder()
	withFacadeChat("/api/prefs", local)(w, httptest.NewRequest("GET", "/api/prefs", nil))
	if w.Body.String() != "go" {
		t.Fatalf("relayé sans la variable : %q", w.Body.String())
	}
}

// LOKI_FILES=rust : les routes des fichiers de la discussion partent à la
// façade ; /internal/v1/conv/workspace lui donne le dossier de la discussion
// active ; les .part d'envois ne sont plus vidés ici au démarrage.
func TestFacadeFichiers(t *testing.T) {
	home := testHome(t)
	f := demarrerFacade(t)
	t.Setenv(facadeChatEnv, "")
	t.Setenv(facadeFilesEnv, "rust")
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }
	for p := range facadeFilesRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("GET", p, nil))
		if w.Body.String() != f.reply {
			t.Fatalf("%s : réponse %q", p, w.Body.String())
		}
	}
	prev := agentWorkspace()
	workspacePath = filepath.Join(home, "ws")
	t.Cleanup(func() { workspacePath = prev })
	convActiveRemember("c-test")
	w := httptest.NewRecorder()
	handleInternalConvWorkspace(w, httptest.NewRequest("POST", "/internal/v1/conv/workspace", strings.NewReader("{}")))
	var ws map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &ws); err != nil {
		t.Fatal(err)
	}
	if ws["dir"] != filepath.Join(home, "ws", "discussions", "c-test") || ws["root"] != filepath.Join(home, "ws") || ws["id"] != "c-test" {
		t.Fatalf("dossier de la discussion : %v", ws)
	}
	if st, err := os.Stat(ws["dir"]); err != nil || !st.IsDir() {
		t.Fatalf("dossier non créé : %v", err)
	}
	part := filepath.Join(ws["dir"], "uploads", ".upload-1.part")
	_ = os.MkdirAll(filepath.Dir(part), 0o755)
	_ = os.WriteFile(part, []byte("x"), 0o644)
	cleanStartupUploadParts()
	if _, err := os.Stat(part); err != nil {
		t.Fatalf(".part effacé alors que la façade tient les fichiers : %v", err)
	}
	t.Setenv(facadeFilesEnv, "")
	cleanStartupUploadParts()
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatalf(".part gardé sans la variable : %v", err)
	}
}

// LOKI_PUSH=rust : les routes des notifications partent à la façade, et les
// envois de ce process (fin de tour, fin de tâche) lui sont délégués.
func TestFacadeNotifications(t *testing.T) {
	testHome(t)
	f := demarrerFacade(t)
	t.Setenv(facadeChatEnv, "")
	t.Setenv(facadePushEnv, "rust")
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }
	for p := range facadePushRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if w.Body.String() != f.reply {
			t.Fatalf("%s : réponse %q", p, w.Body.String())
		}
	}
	sendPushToAll("Loki", "Réponse prête · 3s")
	got := f.recu()
	if len(got) != len(facadePushRoutes)+1 || got[len(got)-1] != `POST /internal/facade/v1/push/send jeton {"body":"Réponse prête · 3s","title":"Loki"}` {
		t.Fatalf("reçu par la façade : %v", got)
	}
	t.Setenv(facadePushEnv, "")
	sendPushToAll("Loki", "x") // aucun abonné : rien à envoyer, rien de délégué
	if n := len(f.recu()); n != len(facadePushRoutes)+1 {
		t.Fatalf("envoi délégué sans la variable (%d appels)", n)
	}
}

func TestFacadeMemoire(t *testing.T) {
	testHome(t)
	f := demarrerFacade(t)
	t.Setenv(facadeChatEnv, "")
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }
	// Sans la variable : le Go sert lui-même.
	for p := range facadeMemRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("GET", p, nil))
		if w.Body.String() != "go" {
			t.Fatalf("%s servi ailleurs sans LOKI_MEM : %q", p, w.Body.String())
		}
	}
	t.Setenv(facadeMemEnv, "rust")
	for p := range facadeMemRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if w.Body.String() != f.reply {
			t.Fatalf("%s : réponse %q", p, w.Body.String())
		}
	}
	if n := len(f.recu()); n != len(facadeMemRoutes) {
		t.Fatalf("reçu par la façade : %d appels", n)
	}
	// Les routes voisines (coffre, projets) restent au Go.
	for _, p := range []string{"/api/mem/health", "/api/mem/unlock", "/api/projects", "/api/backup/export"} {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("GET", p, nil))
		if w.Body.String() != "go" {
			t.Fatalf("%s remis à la façade : %q", p, w.Body.String())
		}
	}
}

func TestFacadePresets(t *testing.T) {
	testHome(t)
	f := demarrerFacade(t)
	t.Setenv(facadeChatEnv, "")
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }
	for p := range facadePresetsRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("GET", p, nil))
		if w.Body.String() != "go" {
			t.Fatalf("%s servi ailleurs sans LOKI_PRESETS : %q", p, w.Body.String())
		}
	}
	t.Setenv(facadePresetsEnv, "rust")
	for p := range facadePresetsRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if w.Body.String() != f.reply {
			t.Fatalf("%s : réponse %q", p, w.Body.String())
		}
	}
	if n := len(f.recu()); n != len(facadePresetsRoutes) {
		t.Fatalf("reçu par la façade : %d appels", n)
	}
}

// Le pilotage du moteur (LOKI_ENGINE=rust, lot 16) part à la façade, et l'API
// interne lui rend ce qui reste ici : verrou de l'optimiseur avant un
// démarrage, génération en cours, arrêt de la dictée.
func TestFacadeMoteur(t *testing.T) {
	testHome(t)
	f := demarrerFacade(t)
	t.Setenv(facadeChatEnv, "")
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }
	for p := range facadeEngineRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, nil))
		if w.Body.String() != "go" {
			t.Fatalf("%s servi ailleurs sans LOKI_ENGINE : %q", p, w.Body.String())
		}
	}
	t.Setenv(facadeEngineEnv, "rust")
	for p := range facadeEngineRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if w.Body.String() != f.reply {
			t.Fatalf("%s : réponse %q", p, w.Body.String())
		}
	}
	if n := len(f.recu()); n != len(facadeEngineRoutes) {
		t.Fatalf("reçu par la façade : %d appels", n)
	}

	// Avant un démarrage : le verrou, puis le ménage.
	appels := 0
	old := enginePrestart
	t.Cleanup(func() { enginePrestart = old })
	enginePrestart = func() error {
		appels++
		if appels == 2 {
			return errTuneBusy
		}
		return nil
	}
	for i, want := range []string{`{"error":""}`, `{"error":"` + errTuneBusy.Error() + `"}`} {
		w := httptest.NewRecorder()
		handleInternalServicePrestart(w, httptest.NewRequest("POST", "/internal/v1/service/prestart", nil))
		if got := strings.TrimSpace(w.Body.String()); w.Code != 200 || got != want {
			t.Fatalf("prestart %d : %d %s", i, w.Code, got)
		}
	}
	w := httptest.NewRecorder()
	handleInternalServicePrestart(w, httptest.NewRequest("GET", "/internal/v1/service/prestart", nil))
	if w.Code != 405 || appels != 2 {
		t.Fatalf("prestart en GET : %d (%d appels)", w.Code, appels)
	}

	// La génération en cours de la conversation tenue ici.
	for _, gen := range []bool{false, true} {
		conv.mu.Lock()
		conv.Generating = gen
		conv.mu.Unlock()
		w := httptest.NewRecorder()
		handleInternalChatGenerating(w, httptest.NewRequest("GET", "/internal/v1/chat/generating", nil))
		var out struct{ Generating bool }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Generating != gen {
			t.Fatalf("generating=%v : %s", gen, w.Body.String())
		}
	}
	conv.mu.Lock()
	conv.Generating = false
	conv.mu.Unlock()

	// L'arrêt de la dictée (rien ne tourne : immédiat).
	w = httptest.NewRecorder()
	handleInternalDictateStop(w, httptest.NewRequest("POST", "/internal/v1/dictate/stop", nil))
	if w.Code != 204 {
		t.Fatalf("arrêt de la dictée : %d", w.Code)
	}
}

// Une clé de complétion d'un ou deux octets ne fait plus tomber /api/apikey.
func TestMaskAPIKeyCourte(t *testing.T) {
	for k, want := range map[string]string{
		"":                         "",
		"k":                        "…",
		"ab":                       "…",
		"abc":                      "…bc",
		"abcdefghijklm":            "…lm",
		"sk-loki-0123456789abcdef": "sk-loki-0…cdef",
	} {
		if got := maskAPIKey(k); got != want {
			t.Errorf("maskAPIKey(%q) = %q, attendu %q", k, got, want)
		}
	}
}

// Les interrupteurs système (LOKI_SYSTEM=rust, lot 17) partent à la façade ;
// l'API interne lui rend ce qui vit ici : cache de joignabilité des outils
// web, navigateur piloté, redémarrage du moteur.
func TestFacadeSysteme(t *testing.T) {
	testHome(t)
	t.Setenv("LOKI_CONTAINER", "1")
	f := demarrerFacade(t)
	t.Setenv(facadeChatEnv, "")
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }
	for p := range facadeSystemRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("GET", p, nil))
		if w.Body.String() != "go" {
			t.Fatalf("%s servi ailleurs sans LOKI_SYSTEM : %q", p, w.Body.String())
		}
	}
	t.Setenv(facadeSystemEnv, "rust")
	for p := range facadeSystemRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if w.Body.String() != f.reply {
			t.Fatalf("%s : réponse %q", p, w.Body.String())
		}
	}
	if n := len(f.recu()); n != len(facadeSystemRoutes) {
		t.Fatalf("reçu par la façade : %d appels", n)
	}

	// Cache de joignabilité vidé.
	reachMu.Lock()
	reachURL = "http://ancien"
	reachMu.Unlock()
	w := httptest.NewRecorder()
	handleInternalReachReset(w, httptest.NewRequest("POST", "/internal/v1/web/reach/reset", nil))
	reachMu.Lock()
	vide := reachURL == ""
	reachMu.Unlock()
	if w.Code != 204 || !vide {
		t.Fatalf("reach/reset : %d, cache vidé : %v", w.Code, vide)
	}
	w = httptest.NewRecorder()
	handleInternalCDPShutdown(w, httptest.NewRequest("POST", "/internal/v1/cdp/shutdown", nil))
	if w.Code != 204 {
		t.Fatalf("cdp/shutdown : %d", w.Code)
	}

	// Redémarrage : seulement si le moteur tourne quand if_active est posé.
	var actions []string
	old := internalServiceAction
	t.Cleanup(func() { internalServiceAction = old })
	internalServiceAction = func(a string) error {
		actions = append(actions, a)
		if a == "start" {
			return errTuneBusy
		}
		return nil
	}
	for _, c := range []struct {
		body, want string
		code       int
	}{
		{`{"action":"restart","if_active":true}`, `{"error":"","ran":false}`, 200},
		{`{"action":"restart"}`, `{"error":"","ran":true}`, 200},
		{`{"action":"start"}`, `{"error":"` + errTuneBusy.Error() + `","ran":true}`, 200},
		{`{"action":"reload"}`, `{"error":"action inconnue : reload"}`, 400},
		{`{"action":`, `{"error":"unexpected EOF"}`, 400},
	} {
		w := httptest.NewRecorder()
		handleInternalServiceAction(w, httptest.NewRequest("POST", "/internal/v1/service/action", strings.NewReader(c.body)))
		if got := strings.TrimSpace(w.Body.String()); w.Code != c.code || got != c.want {
			t.Fatalf("%s : %d %s", c.body, w.Code, got)
		}
	}
	if strings.Join(actions, ",") != "restart,start" {
		t.Fatalf("actions : %v", actions)
	}
}

// Le relais vers la façade garde l'hôte et le schéma du client : la clé de
// complétion y calcule l'adresse à coller dans un client tiers.
func TestFacadeRelaisGardeHote(t *testing.T) {
	var host, proto string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, proto = r.Host, r.Header.Get("X-Forwarded-Proto")
	}))
	defer srv.Close()
	t.Setenv(internalTokenEnv, jetonTest)
	t.Setenv(facadeAddrEnv, strings.TrimPrefix(srv.URL, "http://"))
	r := httptest.NewRequest("GET", "/api/apikey", nil)
	r.Host = "loki.exemple.fr"
	r.Header.Set("X-Forwarded-Proto", "https")
	facadeRelayAs(httptest.NewRecorder(), r, "")
	if host != "loki.exemple.fr" || proto != "https" {
		t.Fatalf("hôte %q, schéma %q", host, proto)
	}
}

// Le hub des projets (LOKI_PROJECTS=rust, lot 18) part à la façade ; quand le
// Go tient la conversation, la bascule et les discussions d'un projet
// supprimé restent ici (API interne).
func TestFacadeProjets(t *testing.T) {
	testHome(t)
	// Le projet actif est gardé en RAM d'un test à l'autre.
	activeProjMu.Lock()
	activeProjCache = ""
	activeProjMu.Unlock()
	t.Cleanup(func() {
		activeProjMu.Lock()
		activeProjCache = ""
		activeProjMu.Unlock()
	})
	ensureDefaultProject()
	f := demarrerFacade(t)
	t.Setenv(facadeChatEnv, "")
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }
	w := httptest.NewRecorder()
	withFacadeChat("/api/projects", local)(w, httptest.NewRequest("GET", "/api/projects", nil))
	if w.Body.String() != "go" {
		t.Fatalf("servi ailleurs sans LOKI_PROJECTS : %q", w.Body.String())
	}
	t.Setenv(facadeProjectsEnv, "rust")
	w = httptest.NewRecorder()
	withFacadeChat("/api/projects", local)(w, httptest.NewRequest("POST", "/api/projects", strings.NewReader("{}")))
	if w.Body.String() != f.reply || len(f.recu()) != 1 {
		t.Fatalf("relais : %q, %d appels", w.Body.String(), len(f.recu()))
	}

	if _, err := createProject("Alpha"); err != nil {
		t.Fatal(err)
	}
	convIndexSave([]convMeta{{ID: "a1", Project: "alpha", Updated: 1}, {ID: "g1", Project: "generale", Updated: 2}})
	_ = putBytes(bkChat, convKey("a1"), []byte(`{"id":"a1"}`))
	_ = putBytes(bkChat, convKey("g1"), []byte(`{"id":"g1"}`))
	post := func(h http.HandlerFunc, body string) (int, string) {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest("POST", "/internal/v1/project/x", strings.NewReader(body)))
		return w.Code, strings.TrimSpace(w.Body.String())
	}
	if c, b := post(handleInternalProjectSwitch, `{"slug":"nope"}`); c != 400 || b != `{"error":"projet introuvable"}` {
		t.Fatalf("bascule inconnue : %d %s", c, b)
	}
	if c, _ := post(handleInternalProjectSwitch, `{"slug":" alpha "}`); c != 204 || activeProjectSlug() != "alpha" {
		t.Fatalf("bascule : %d, actif %q", c, activeProjectSlug())
	}
	conv.mu.Lock()
	conv.Generating = true
	conv.mu.Unlock()
	c1, _ := post(handleInternalProjectSwitch, `{"slug":"generale"}`)
	c2, _ := post(handleInternalProjectDropConvs, `{"slug":"alpha"}`)
	conv.mu.Lock()
	conv.Generating = false
	conv.mu.Unlock()
	if c1 != 409 || c2 != 409 {
		t.Fatalf("pendant une génération : %d, %d", c1, c2)
	}
	if c, b := post(handleInternalProjectDropConvs, `{"slug":"alpha"}`); c != 200 || b != `{"dropped":1}` {
		t.Fatalf("discussions supprimées : %d %s", c, b)
	}
	if n := len(convIndexForProject("alpha")); n != 0 || len(convIndexForProject("generale")) == 0 {
		t.Fatalf("index après : %d d'Alpha", n)
	}
	if c, _ := post(handleInternalProjectDropConvs, `nimp`); c != 400 {
		t.Fatalf("corps illisible : %d", c)
	}
}

// Les serveurs MCP (LOKI_MCP=rust, lot 20) : routes relayées, et le Go
// demande à la façade les outils MCP et leurs appels (un seul pool).
func TestFacadeMCP(t *testing.T) {
	testHome(t)
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(internalTokenHeader) != jetonTest {
			w.WriteHeader(403)
			return
		}
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, r.URL.Path+" "+string(b))
		switch r.URL.Path {
		case "/internal/facade/v1/mcp/tools":
			io.WriteString(w, `{"tools":[{"type":"function","function":{"name":"mcp__a__t","description":"[MCP: a] d","parameters":{"type":"object"}}}]}`)
		case "/internal/facade/v1/mcp/call":
			io.WriteString(w, `{"result":"fait"}`)
		case "/internal/facade/v1/mcp/prewarm":
			io.WriteString(w, `{"ok":true}`)
		default:
			io.WriteString(w, `{"ok":true,"de":"facade"}`)
		}
	}))
	defer srv.Close()
	t.Setenv(internalTokenEnv, jetonTest)
	t.Setenv(facadeAddrEnv, strings.TrimPrefix(srv.URL, "http://"))
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }

	t.Setenv(facadeMCPEnv, "")
	w := httptest.NewRecorder()
	withFacadeChat("/api/mcp", local)(w, httptest.NewRequest("GET", "/api/mcp", nil))
	if w.Body.String() != "go" || len(calls) != 0 {
		t.Fatalf("sans LOKI_MCP : %q", w.Body.String())
	}
	t.Setenv(facadeMCPEnv, "rust")
	for p := range facadeMCPRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if !strings.Contains(w.Body.String(), "facade") {
			t.Fatalf("%s non relayé : %q", p, w.Body.String())
		}
	}
	tools := mcpTools()
	if len(tools) != 1 || tools[0].Function.Name != "mcp__a__t" || mcpPromptLine(tools) == "" {
		t.Fatalf("outils : %+v", tools)
	}
	if got := mcpCall(context.Background(), "mcp__a__t", map[string]any{"x": 1}); got != "fait" {
		t.Fatalf("appel : %q", got)
	}
	if got := mcpCall(context.Background(), "mcp__a__t", nil); got != "fait" || !strings.HasSuffix(calls[len(calls)-1], `{"args":{},"name":"mcp__a__t"}`) {
		t.Fatalf("appel sans arguments : %q (%v)", got, calls)
	}
	MCPPrewarm()
	for i := 0; i < 50 && !strings.HasPrefix(calls[len(calls)-1], "/internal/facade/v1/mcp/prewarm"); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.HasPrefix(calls[len(calls)-1], "/internal/facade/v1/mcp/prewarm") {
		t.Fatalf("pré-chauffage non demandé : %v", calls)
	}
	// Façade injoignable : une erreur lisible, pas de panique.
	t.Setenv(facadeAddrEnv, "127.0.0.1:1")
	if got := mcpCall(context.Background(), "mcp__a__t", nil); !strings.HasPrefix(got, "[erreur MCP] façade injoignable") {
		t.Fatalf("façade injoignable : %q", got)
	}
	if tools := mcpTools(); tools != nil {
		t.Fatalf("outils sans façade : %v", tools)
	}
}

// La mise à jour (LOKI_UPDATE=rust, lot 21) part à la façade.
func TestFacadeMiseAJour(t *testing.T) {
	testHome(t)
	f := demarrerFacade(t)
	t.Setenv(facadeChatEnv, "")
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }
	for _, on := range []string{"", "rust"} {
		t.Setenv(facadeUpdateEnv, on)
		for p := range facadeUpdateRoutes {
			w := httptest.NewRecorder()
			withFacadeChat(p, local)(w, httptest.NewRequest("GET", p, nil))
			want := "go"
			if on == "rust" {
				want = f.reply
			}
			if w.Body.String() != want {
				t.Fatalf("%s (LOKI_UPDATE=%q) : %q", p, on, w.Body.String())
			}
		}
	}
}

// Lot 22 : les routes des backends partent à la façade, et les jobs du Go
// (compilation, moteur de l'image, Strata) lui rapportent chaque étape — le
// job vit chez elle.
func TestFacadeBackends(t *testing.T) {
	testHome(t)
	resetBackendsState()
	t.Cleanup(resetBackendsState)
	var mu sync.Mutex
	var calls []string
	busy := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(internalTokenHeader) != jetonTest {
			w.WriteHeader(403)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.RequestURI()+" "+string(b))
		refuse := busy
		mu.Unlock()
		switch r.URL.Path {
		case "/internal/facade/v1/build-job/start":
			if refuse {
				w.WriteHeader(409)
				io.WriteString(w, `{"ok":false,"error":"un job engine est déjà en cours"}`)
				return
			}
			io.WriteString(w, `{"ok":true}`)
		case "/internal/facade/v1/build-job/snapshot":
			io.WriteString(w, `{"exists":true,"action":"engine","running":true}`)
		default:
			io.WriteString(w, `{"ok":true,"de":"facade"}`)
		}
	}))
	defer srv.Close()
	t.Setenv(internalTokenEnv, jetonTest)
	t.Setenv(facadeAddrEnv, strings.TrimPrefix(srv.URL, "http://"))
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }

	t.Setenv(facadeBackendsEnv, "")
	w := httptest.NewRecorder()
	withFacadeChat("/api/llamacpp", local)(w, httptest.NewRequest("GET", "/api/llamacpp", nil))
	if w.Body.String() != "go" || len(calls) != 0 {
		t.Fatalf("sans LOKI_BACKENDS : %q", w.Body.String())
	}
	t.Setenv(facadeBackendsEnv, "rust")
	for p := range facadeBackendsRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if !strings.Contains(w.Body.String(), "facade") {
			t.Fatalf("%s non relayé : %q", p, w.Body.String())
		}
	}
	// La compilation depuis les sources part aussi (lot 41), sauf sous Windows.
	for _, p := range []string{"/api/llamacpp/install", "/api/llamacpp/update", "/api/llamacpp/install-custom"} {
		if facadeBackendsRoutes[p] != (runtime.GOOS != "windows") {
			t.Fatalf("%s : tenu par la façade = %v sous %s", p, facadeBackendsRoutes[p], runtime.GOOS)
		}
	}
	mu.Lock()
	calls = nil
	mu.Unlock()

	// Un job du Go : chaque étape part à la façade, dans l'ordre.
	done := make(chan struct{})
	if err := startLcJob("engine", func() {
		lcPhase("recherche…")
		emitBuildLine("[ 10%] Building CXX object a/b.cpp.o")
		lcSetCommit("old", "")
		lcSetCommit("new", "bbb")
		lcFail(errors.New("raté"))
		lcDone("fini")
		close(done)
	}); err != nil {
		t.Fatal(err)
	}
	<-done
	want := []string{
		`POST /internal/facade/v1/build-job/start {"action":"engine"}`,
		`POST /internal/facade/v1/build-job/phase {"phase":"recherche…"}`,
		`POST /internal/facade/v1/build-job/line {"line":"[ 10%] Building CXX object a/b.cpp.o"}`,
		`POST /internal/facade/v1/build-job/commits {"old":""}`,
		`POST /internal/facade/v1/build-job/commits {"new":"bbb"}`,
		`POST /internal/facade/v1/build-job/fail {"error":"raté"}`,
		`POST /internal/facade/v1/build-job/done {"msg":"fini"}`,
		`POST /internal/facade/v1/build-job/end {}`,
	}
	for i := 0; i < 100; i++ {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n >= len(want) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	got := append([]string(nil), calls...)
	calls = nil
	busy = true
	mu.Unlock()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("étapes transmises :\n%s", strings.Join(got, "\n"))
	}
	// Rien ne reste dans le job du Go, et le sink est retiré.
	if lcCur != nil {
		t.Fatalf("job tenu par le Go : %+v", lcCur)
	}
	// Un second job : la façade le refuse, le Go le dit.
	if err := startLcJob("strata", func() { t.Error("ne doit pas tourner") }); err == nil || err.Error() != "un job engine est déjà en cours" {
		t.Fatalf("refus : %v", err)
	}
	// La vue du job (statut du moteur de l'image) vient de la façade.
	if v := lcJobSnapshot(0, false); v["action"] != "engine" || v["running"] != true {
		t.Fatalf("vue : %v", v)
	}
	mu.Lock()
	last := calls[len(calls)-1]
	mu.Unlock()
	if last != "GET /internal/facade/v1/build-job/snapshot?from=0&lines=0 " {
		t.Fatalf("vue demandée : %q", last)
	}
	if v := lcJobSnapshot(3, true); v["exists"] != true {
		t.Fatalf("vue avec lignes : %v", v)
	}
	// Façade injoignable : le job ne démarre pas, la vue dit « aucun job ».
	t.Setenv(facadeAddrEnv, "127.0.0.1:1")
	if err := startLcJob("engine", func() { t.Error("ne doit pas tourner") }); err == nil {
		t.Fatal("job démarré sans façade")
	}
	if v := lcJobSnapshot(0, true); v["exists"] != false {
		t.Fatalf("vue sans façade : %v", v)
	}
	lcAppend("perdue") // tracée, sans panique
}

// Lot 23 : quand la façade tient la conversation, c'est elle qui sonde le
// gabarit ; le résumé des performances relit son verdict.
func TestFacadeSondeDeGabarit(t *testing.T) {
	testHome(t)
	freshTplProbe(t)
	verdict := `{"ok":true,"result":{"preserves_history":"no","prefix_stable":"no","renders_reasoning":"yes","model":"m1.gguf","tools":2,"at":"2026-10-15T20:00:00.5Z"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(internalTokenHeader) != jetonTest || r.URL.Path != "/internal/facade/v1/chat/tpl" {
			w.WriteHeader(404)
			return
		}
		io.WriteString(w, verdict)
	}))
	defer srv.Close()
	t.Setenv(internalTokenEnv, jetonTest)
	t.Setenv(facadeAddrEnv, strings.TrimPrefix(srv.URL, "http://"))
	t.Setenv(facadeChatEnv, "rust")
	r, ok := tplCapsCurrent()
	if !ok || r.PrefixStable != tplNo || r.Model != "m1.gguf" || r.Tools != 2 || r.At.IsZero() {
		t.Fatalf("verdict de la façade : %+v %v", r, ok)
	}
	rec := httptest.NewRecorder()
	handlePerfSummary(rec, httptest.NewRequest(http.MethodGet, "/api/perf/summary", nil))
	if !strings.Contains(rec.Body.String(), `"prefix_stable":"no"`) {
		t.Fatalf("résumé : %s", rec.Body.String())
	}
	// Aucun verdict encore, ou façade injoignable : rien.
	verdict = `{"ok":false}`
	if _, ok := tplCapsCurrent(); ok {
		t.Fatal("verdict inventé")
	}
	t.Setenv(facadeAddrEnv, "127.0.0.1:1")
	if _, ok := tplCapsCurrent(); ok {
		t.Fatal("verdict sans façade")
	}
}

// Le panneau « moteur de l'image » part à la façade avec LOKI_ENGINE_IMAGE=rust
// (lot 24) ; la commande du dernier lancement reste au Go.
func TestFacadeMoteurImage(t *testing.T) {
	testHome(t)
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(internalTokenHeader) != jetonTest {
			w.WriteHeader(403)
			return
		}
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		io.WriteString(w, `{"ok":true,"de":"facade"}`)
	}))
	defer srv.Close()
	t.Setenv(internalTokenEnv, jetonTest)
	t.Setenv(facadeAddrEnv, strings.TrimPrefix(srv.URL, "http://"))
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }

	t.Setenv(facadeEngineImageEnv, "")
	for p := range facadeEngineImageRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if w.Body.String() != "go" {
			t.Fatalf("sans LOKI_ENGINE_IMAGE, %s : %q", p, w.Body.String())
		}
	}
	if len(calls) != 0 {
		t.Fatalf("appels à la façade sans LOKI_ENGINE_IMAGE : %v", calls)
	}

	t.Setenv(facadeEngineImageEnv, "Rust ")
	if len(facadeEngineImageRoutes) != 7 {
		t.Fatalf("routes : %v", facadeEngineImageRoutes)
	}
	for p := range facadeEngineImageRoutes {
		w := httptest.NewRecorder()
		withFacadeChat(p, local)(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if !strings.Contains(w.Body.String(), "facade") {
			t.Fatalf("%s non relayé : %q", p, w.Body.String())
		}
	}
	mu.Lock()
	n := len(calls)
	mu.Unlock()
	if n != len(facadeEngineImageRoutes) {
		t.Fatalf("relais : %v", calls)
	}
	w := httptest.NewRecorder()
	withFacadeChat("/api/llamacpp/install", local)(w, httptest.NewRequest("POST", "/api/llamacpp/install", nil))
	if w.Body.String() != "go" {
		t.Fatalf("/api/llamacpp/install doit rester au Go : %q", w.Body.String())
	}

	// Sans jeton interne, rien ne part.
	t.Setenv(internalTokenEnv, "")
	if facadeEngineImage() {
		t.Fatal("tenu sans jeton interne")
	}
}
