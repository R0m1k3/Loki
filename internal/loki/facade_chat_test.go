package loki

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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

// oublierDecisionFacade : la décision « qui tient la conversation » est prise
// une fois par process ; chaque test repart de zéro.
func oublierDecisionFacade(t *testing.T) {
	t.Helper()
	reset := func() {
		facadeDecisionMu.Lock()
		facadeDecisionDone, facadeDecisionWhy = false, ""
		facadeDecisionMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func demarrerFacade(t *testing.T) *fausseFacade {
	t.Helper()
	oublierDecisionFacade(t)
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
	// Mode Code : reste au Go.
	w = httptest.NewRecorder()
	withFacadeChat("/api/chat/mode", local)(w, httptest.NewRequest("POST", "/api/chat/mode", nil))
	if w.Body.String() != "go" {
		t.Fatalf("mode relayé : %q", w.Body.String())
	}
	// Sans LOKI_CHAT=rust : tout reste ici.
	t.Setenv(facadeChatEnv, "")
	w = httptest.NewRecorder()
	withFacadeChat("/api/chat/send", local)(w, httptest.NewRequest("POST", "/api/chat/send", nil))
	if w.Body.String() != "go" || len(f.recu()) != 1 {
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

// Un réglage que la façade ne sait pas honorer garde la conversation ici :
// rien ne lui est remis, et elle l'apprend au démarrage. La décision tient
// jusqu'au redémarrage.
func TestFacadeReglageQuiGardeLaConversation(t *testing.T) {
	testHome(t)
	f := demarrerFacade(t)
	t.Setenv(internalTokenEnv, jetonTest)
	if w := appelOAIInterne(t, "GET", "/internal/v1/chat/holder", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"holder":"rust"`) {
		t.Fatalf("détenteur : %d %s", w.Code, w.Body.String())
	}
	// Posé après la décision : sans effet avant le redémarrage.
	if err := SetConfigKey("KEEP_TURN_IMAGES", "on"); err != nil {
		t.Fatal(err)
	}
	if !facadeChat() {
		t.Fatal("décision changée en cours de route")
	}
	oublierDecisionFacade(t)
	if facadeChat() {
		t.Fatal("KEEP_TURN_IMAGES posé : la conversation doit rester au Go")
	}
	w := appelOAIInterne(t, "GET", "/internal/v1/chat/holder", "")
	if !strings.Contains(w.Body.String(), `"holder":"go"`) || !strings.Contains(w.Body.String(), "KEEP_TURN_IMAGES") {
		t.Fatalf("détenteur : %s", w.Body.String())
	}
	local := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "go") }
	rec := httptest.NewRecorder()
	withFacadeChat("/api/chat/send", local)(rec, httptest.NewRequest("POST", "/api/chat/send", strings.NewReader(`{}`)))
	if rec.Body.String() != "go" || len(f.recu()) != 0 {
		t.Fatalf("remis à la façade malgré le réglage : %q %v", rec.Body.String(), f.recu())
	}
	for k, want := range map[string]string{
		"PROJ_SNAPSHOT": "PROJ_SNAPSHOT", "NUDGE_IN_TOOL": "NUDGE_IN_TOOL", "SIDE_SLOT": "SIDE_SLOT",
		"SLOT_PERSIST": "SLOT_PERSIST", "PREWARM": "PREWARM",
	} {
		if got := facadeChatBlocker(map[string]string{k: "on"}); got != want {
			t.Errorf("%s : %q", k, got)
		}
	}
	if got := facadeChatBlocker(map[string]string{}); got != "" {
		t.Errorf("sans réglage : %q", got)
	}
	// Sans LOKI_CHAT=rust : le Go, et pourquoi.
	oublierDecisionFacade(t)
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
	for _, p := range []string{"/api/chat/send", "/api/model/layers", "/api/presets"} {
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
