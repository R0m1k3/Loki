package loki

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// moteurScripte : faux llama-server qui répond selon le DERNIER message reçu.
// Un message `tool` en queue → texte de clôture ; sinon la règle dont le motif
// figure dans le dernier message utilisateur → appel d'outil criteria.
func moteurScripte(t *testing.T, regles map[string]string) (port string, vus *[]string) {
	t.Helper()
	var mu sync.Mutex
	var log []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		last := body.Messages[len(body.Messages)-1]
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		args := ""
		if last.Role == "user" {
			txt := msgText(last)
			mu.Lock()
			log = append(log, txt)
			mu.Unlock()
			for motif, a := range regles {
				if strings.Contains(txt, motif) {
					args = a
				}
			}
		}
		if args != "" {
			esc := strings.ReplaceAll(args, `"`, `\"`)
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"criteria","arguments":"` + esc + `"}}]},"finish_reason":null}]}` + "\n\n"))
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"))
		} else {
			_, _ = w.Write([]byte(sseChunk("ok")))
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Port(), &log
}

// Le builder rend la main avec un critère encore ouvert : on le RELANCE (il le
// marque completed) avant de vérifier, au lieu de vérifier un travail
// inachevé (repris du buildAgentNudge d'OpenFox).
func TestVerificationAttendQueLeBuilderAitFini(t *testing.T) {
	withWorkspace(t)
	id := convEnsureActive()
	toolCriteria(map[string]any{"action": "add", "texts": []any{"le build compile"}}, false)
	port, vus := moteurScripte(t, map[string]string{
		"Not done yet":         `{"action":"set","id":"1","status":"completed"}`,
		"Verify each non-pass": `{"action":"set","id":1,"status":"passed"}`,
	})
	if err := SetConfigKey("PORT", port); err != nil {
		t.Fatal(err)
	}
	c := newTestConv()
	c.Messages = []Message{{Role: "user", Content: "fais le build"}}
	c.codeVerifyLoop(context.Background(), Caps{Agent: true, Code: true}, 0.2, c.epoch, false)

	if len(*vus) < 2 || !strings.Contains((*vus)[0], "Not done yet") || !strings.Contains((*vus)[1], "Verify each non-pass") {
		t.Fatalf("ordre des passes = %q, attendu relance du builder PUIS vérification", *vus)
	}
	if l := critList(id); len(l) != 1 || l[0].Status != "passed" {
		t.Fatalf("critère final : %+v", l)
	}
}

// Tour terminé sur une question à l'utilisateur : aucune passe ne part avant
// sa réponse.
func TestPasDeVerificationApresUneQuestion(t *testing.T) {
	withWorkspace(t)
	toolCriteria(map[string]any{"action": "add", "texts": []any{"le build compile"}}, false)
	port, vus := moteurScripte(t, nil)
	if err := SetConfigKey("PORT", port); err != nil {
		t.Fatal(err)
	}
	c := newTestConv()
	c.Messages = []Message{{Role: "user", Content: "fais le build"}}
	c.codeVerifyLoop(context.Background(), Caps{Agent: true, Code: true}, 0.2, c.epoch, true)
	if len(*vus) != 0 {
		t.Fatalf("%d requête(s) au moteur alors qu'une question attend l'utilisateur", len(*vus))
	}
}

// Un id de critère écrit entre guillemets (« "2" », « "#2" ») vise bien #2.
func TestCritereIdEnChaine(t *testing.T) {
	withWorkspace(t)
	id := convEnsureActive()
	toolCriteria(map[string]any{"action": "add", "texts": []any{"a", "b"}}, false)
	if out := toolCriteria(map[string]any{"action": "set", "id": "#2", "status": "completed"}, false); !strings.HasPrefix(out, "[ok]") {
		t.Fatalf("id en chaîne refusé : %s", out)
	}
	if l := critList(id); l[1].Status != "completed" || l[0].Status != "pending" {
		t.Fatalf("mauvais critère modifié : %+v", l)
	}
}
