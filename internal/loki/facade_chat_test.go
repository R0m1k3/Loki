package loki

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
