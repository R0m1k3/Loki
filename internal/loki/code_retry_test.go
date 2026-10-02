package loki

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Les appels d'outils écrits en texte doivent être détectés…
func TestAppelOutilTextuelDetecte(t *testing.T) {
	for _, s := range []string{
		"<tool_call>{\"name\":\"bash\"}</tool_call>",
		"Je vais lancer default_api:bash pour vérifier.",
		"default_api.run_shell(command='ls')",
		"functions.bash({\"command\": \"ls\"})",
		"```tool_code\nprint('x')\n```",
		"{\"name\": \"bash\", \"arguments\": {\"command\": \"ls\"}}",
	} {
		if !textualToolCall(s) {
			t.Errorf("non détecté : %q", s)
		}
	}
}

// … sans jamais relancer un tour normal (code cité, JSON quelconque).
func TestReponseNormaleNonRelancee(t *testing.T) {
	for _, s := range []string{
		"Voici la réponse : le fichier contient trois fonctions.",
		"```go\nfunc main() {}\n```",
		"Le JSON de config : {\"name\": \"loki\", \"port\": 8090}",
		"La fonction bash() de ce script accepte un paramètre.",
		"",
	} {
		if textualToolCall(s) {
			t.Errorf("relance à tort : %q", s)
		}
	}
}

// Format XML de Qwen3-Coder resté en texte : détecté, et l'extrait est cité.
func TestAppelOutilQwenXMLDetecte(t *testing.T) {
	s := "Je regarde.\n<function=bash>\n<parameter=command>ls -la</parameter>\n</function>"
	snip := textualToolCallSnippet(s)
	if snip == "" || !strings.HasPrefix(snip, "<function=bash>") {
		t.Fatalf("extrait = %q", snip)
	}
	if textualToolCall("Un <div class=\"x\"> et un <param name=\"a\"> en HTML.") {
		t.Fatal("HTML ordinaire pris pour un appel d'outil")
	}
}

// Le faux appel est repéré PENDANT le flux : la génération est coupée (la suite
// n'arrive jamais à l'écran) et la relance cite l'extrait fautif.
func TestAppelTextuelCoupeLeFlux(t *testing.T) {
	testHome(t)
	var n int32
	var second string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if atomic.AddInt32(&n, 1) == 1 {
			_, _ = w.Write([]byte(sseChunk("Je lance <tool_call>")))
			w.(http.Flusher).Flush()
			time.Sleep(300 * time.Millisecond)
			_, _ = w.Write([]byte(sseChunk("FIN-INUTILE")))
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
			return
		}
		var body struct {
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		second = msgText(body.Messages[len(body.Messages)-1])
		_, _ = w.Write([]byte(sseChunk("ok")))
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}
	var content strings.Builder
	if _, err := runChat(context.Background(), []Message{{Role: "user", Content: "liste"}}, 0.7, Caps{Agent: true}, func(ev StreamEvent) bool {
		content.WriteString(ev.Content)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content.String(), "FIN-INUTILE") {
		t.Fatal("le flux n'a pas été coupé sur l'appel écrit en texte")
	}
	if !strings.Contains(second, "<tool_call>") || !strings.Contains(second, "WRITTEN AS TEXT") {
		t.Fatalf("relance sans l'extrait fautif : %q", second)
	}
}
