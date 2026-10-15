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
			w.WriteHeader(http.StatusNoContent)
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
