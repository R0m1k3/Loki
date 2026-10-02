package loki

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Le schéma de write annonce le chemin AVANT le contenu.
func TestSchemaWriteCheminDAbord(t *testing.T) {
	b, _ := json.Marshal(writeTool().Function.Parameters)
	s := string(b)
	if i, j := strings.Index(s, `"file"`), strings.Index(s, `"content"`); i < 0 || j < 0 || i > j {
		t.Fatalf("ordre des propriétés : %s", s)
	}
}

// write sur un fichier existant jamais lu : refusé dès que le chemin est
// complet, sans attendre le contenu ; le fichier reste intact.
func TestPreVolCoupeLEcritureRefusee(t *testing.T) {
	withWorkspace(t)
	cible := filepath.Join(agentCwd(), "main.go")
	if err := os.WriteFile(cible, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var n int32
	var second []Message
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if atomic.AddInt32(&n, 1) == 1 {
			chunk := func(args string) {
				esc := strings.ReplaceAll(args, `"`, `\"`)
				_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"w1","type":"function","function":{"name":"write","arguments":"` + esc + `"}}]}}]}` + "\n\n"))
				w.(http.Flusher).Flush()
			}
			chunk(`{"file":"main.go",`)
			chunk(`"content":"package main // début`)
			time.Sleep(300 * time.Millisecond)
			chunk(` FIN-JAMAIS-LUE"}`)
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"))
			return
		}
		var body struct {
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		second = body.Messages
		_, _ = w.Write([]byte(sseChunk("je lis d'abord")))
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}
	var results []string
	if _, err := runChat(context.Background(), []Message{{Role: "user", Content: "réécris main.go"}}, 0.7, Caps{Agent: true, Code: true}, func(ev StreamEvent) bool {
		if ev.ToolUsed != nil && ev.ToolUsed.Done {
			results = append(results, ev.ToolUsed.Result)
		}
		if ev.ToolUsed != nil && strings.Contains(ev.ToolUsed.Body, "FIN-JAMAIS-LUE") {
			t.Error("le contenu a continué d'arriver après le refus")
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !strings.Contains(results[0], "[refusé]") || !strings.Contains(results[0], "interrompue") {
		t.Fatalf("résultats = %q", results)
	}
	if b, _ := os.ReadFile(cible); string(b) != "package main\n" {
		t.Fatalf("fichier modifié : %q", b)
	}
	if len(second) < 2 || second[len(second)-1].Role != "tool" || !strings.Contains(msgText(second[len(second)-1]), "[refusé]") {
		t.Fatalf("le refus n'a pas été rendu au modèle : %+v", second)
	}
}
