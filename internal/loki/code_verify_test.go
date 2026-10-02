package loki

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// fauxMoteurOutil : un llama-server de comédie qui, à la PREMIÈRE requête,
// répond par un appel d'outil (name/args), puis par un texte à la suivante.
func fauxMoteurOutil(t *testing.T, name, args string) (port string, requêtes *int32) {
	t.Helper()
	var n int32
	esc := strings.ReplaceAll(args, `"`, `\"`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if atomic.AddInt32(&n, 1) == 1 {
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"` + name + `","arguments":"` + esc + `"}}]},"finish_reason":null}]}` + "\n\n"))
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"))
		} else {
			_, _ = w.Write([]byte(sseChunk("Verdict enregistré.")))
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Port(), &n
}

// La passe de vérification (Role=verifier) doit pouvoir marquer un critère
// passed via l'outil criteria — c'est tout son rôle. Vécu : « en mode code,
// le système de vérification ne coche plus les critères ».
func TestPasseDeVerificationCocheLesCriteres(t *testing.T) {
	withWorkspace(t)
	id := convEnsureActive()
	toolCriteria(map[string]any{"action": "add", "texts": []any{"le build compile"}}, false)

	port, n := fauxMoteurOutil(t, "criteria", `{"action":"set","id":1,"status":"passed"}`)
	if err := SetConfigKey("PORT", port); err != nil {
		t.Fatal(err)
	}

	caps := Caps{Agent: true, Code: true, Role: "verifier"}
	var tools []string
	_, err := runChat(context.Background(), []Message{
		{Role: "system", Content: rolePrompt("verifier")},
		{Role: "user", Content: "Verify."},
	}, 0.2, caps, func(ev StreamEvent) bool {
		if ev.ToolUsed != nil && ev.ToolUsed.Done {
			tools = append(tools, ev.ToolUsed.Name+" → "+ev.ToolUsed.Result)
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if *n < 2 {
		t.Fatalf("%d requête(s) au moteur : l'appel d'outil n'a pas été suivi d'un second tour", *n)
	}
	list := critList(id)
	if len(list) != 1 || list[0].Status != "passed" {
		t.Fatalf("critère non coché par le vérificateur : %+v — outils : %v", list, tools)
	}
}
