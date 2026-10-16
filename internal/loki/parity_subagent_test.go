package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// parity_subagent_test.go — journal de référence de l'outil subagent du mode
// Code (lot 47 de la migration Rust) : refus avant tout appel au moteur, puis
// des tours délégués contre un faux moteur scripté (rapport après le dernier
// outil, texte vide, rapport tronqué, refus du moteur, cadrage du planner).
// Chaque étape garde ce que le rôle a envoyé au moteur (système identique à
// la vue du rôle, message de la tâche, température, outils). Les vues des
// rôles (/internal/v1/turn/subagent) sont rangées avec le journal.
//
// Rangé dans rust/loki-rs/tests/parity_subagent/subagent.json ; le test
// subagent_comme_le_go de tests/lot6.rs le rejoue par un tour de la façade.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteSubagent

const paritySubagentFile = "../../rust/loki-rs/tests/parity_subagent/subagent.json"

// Une réponse scriptée du faux moteur.
type paritySubReply struct {
	Tool    string `json:"tool,omitempty"` // appel d'outil (arguments : Args)
	Args    string `json:"args,omitempty"`
	Content string `json:"content,omitempty"`
	Status  int    `json:"status,omitempty"` // refus du moteur (corps : Content)
}

type paritySubRequest struct {
	SystemIsView bool     `json:"system_is_view"`
	User         string   `json:"user"`
	Temperature  any      `json:"temperature"`
	Tools        []string `json:"tools"`
	Messages     int      `json:"messages"`
}

type paritySubStep struct {
	Name       string             `json:"name"`
	Args       map[string]any     `json:"args"`
	ParentRole string             `json:"parent_role,omitempty"`
	Replies    []paritySubReply   `json:"replies,omitempty"`
	Result     string             `json:"result"`
	Requests   []paritySubRequest `json:"requests"`
}

func paritySubSSE(r paritySubReply) string {
	chunk := func(v any) string {
		b, _ := json.Marshal(v)
		return "data: " + string(b) + "\n\n"
	}
	var b strings.Builder
	if r.Tool != "" {
		b.WriteString(chunk(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": "s1", "type": "function", "function": map[string]any{"name": r.Tool, "arguments": r.Args},
		}}}}}}))
		if r.Content != "" {
			// Texte écrit avant l'appel : la narration, pas le rapport.
			b.Reset()
			b.WriteString(chunk(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": r.Content}}}}))
			b.WriteString(chunk(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": "s1", "type": "function", "function": map[string]any{"name": r.Tool, "arguments": r.Args},
			}}}}}}))
		}
		b.WriteString(chunk(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"}}}))
	} else {
		b.WriteString(chunk(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": r.Content}}}}))
		b.WriteString(chunk(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 40, "completion_tokens": 2}}))
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func TestPariteSubagent(t *testing.T) {
	ws := withWorkspace(t)
	convEnsureActive()
	_ = os.WriteFile(filepath.Join(ws, "cache.go"), []byte("package x\n// cache LRU\n"), 0o644)

	var mu sync.Mutex
	var queue []paritySubReply
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(404)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, body)
		var rep paritySubReply
		if len(queue) > 0 {
			rep, queue = queue[0], queue[1:]
		}
		mu.Unlock()
		if rep.Status != 0 {
			w.WriteHeader(rep.Status)
			_, _ = w.Write([]byte(rep.Content))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(paritySubSSE(rep)))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}

	views := map[string]any{}
	for _, role := range subagentRoles {
		caps, system := subagentView(role)
		views[role] = map[string]any{"system": system, "tools": EnabledTools(caps), "caps": caps}
	}

	var steps []paritySubStep
	run := func(st paritySubStep) {
		mu.Lock()
		queue, bodies = append([]paritySubReply(nil), st.Replies...), nil
		mu.Unlock()
		parent := Caps{Agent: true, Code: true, Role: st.ParentRole}
		st.Result = toolSubagent(context.Background(), st.Args, parent)
		mu.Lock()
		defer mu.Unlock()
		st.Requests = []paritySubRequest{}
		for _, b := range bodies {
			msgs, _ := b["messages"].([]any)
			req := paritySubRequest{Temperature: b["temperature"], Messages: len(msgs), Tools: []string{}}
			if len(msgs) >= 2 {
				sys, _ := msgs[0].(map[string]any)["content"].(string)
				role, _ := st.Args["role"].(string)
				_, want := subagentView(strings.TrimSpace(role))
				req.SystemIsView = sys == want
				req.User, _ = msgs[1].(map[string]any)["content"].(string)
			}
			tools, _ := b["tools"].([]any)
			for _, tl := range tools {
				f, _ := tl.(map[string]any)["function"].(map[string]any)
				n, _ := f["name"].(string)
				req.Tools = append(req.Tools, n)
			}
			st.Requests = append(st.Requests, req)
		}
		steps = append(steps, st)
	}
	a := func(role, task string) map[string]any { return map[string]any{"role": role, "task": task} }

	run(paritySubStep{Name: "rôle inconnu", Args: a("architecte", "x")})
	run(paritySubStep{Name: "verifier refusé", Args: a("verifier", "x")})
	run(paritySubStep{Name: "sans rôle", Args: map[string]any{"task": "x"}})
	run(paritySubStep{Name: "tâche vide", Args: a("explorer", "  ")})
	run(paritySubStep{Name: "tâche absente", Args: map[string]any{"role": "planner"}})
	run(paritySubStep{Name: "pas de récursion", Args: a("explorer", "x"), ParentRole: "explorer"})
	run(paritySubStep{Name: "vérificateur qui délègue", Args: a("planner", "x"), ParentRole: "verifier"})
	run(paritySubStep{Name: "réponse directe", Args: a("explorer", "  Où est le cache ?  "),
		Replies: []paritySubReply{{Content: "  Dans cache.go:2.  \n"}}})
	run(paritySubStep{Name: "rapport après le dernier outil", Args: a(" code-reviewer ", "Relis cache.go"),
		Replies: []paritySubReply{
			{Tool: "grep", Args: `{"pattern":"cache"}`, Content: "Je cherche d'abord."},
			{Tool: "read", Args: `{"path":"cache.go"}`},
			{Content: "RAS : cache.go:2 documente le LRU."},
		}})
	run(paritySubStep{Name: "rien répondu", Args: a("explorer", "Vide ?"),
		Replies: []paritySubReply{{Content: "   "}}})
	run(paritySubStep{Name: "rien après l'outil", Args: a("explorer", "Encore ?"),
		Replies: []paritySubReply{{Tool: "grep", Args: `{"pattern":"x"}`, Content: "narration"}, {Content: ""}}})
	run(paritySubStep{Name: "rapport tronqué", Args: a("explorer", "Tout ?"),
		Replies: []paritySubReply{{Content: strings.Repeat("é", 6100)}}})
	run(paritySubStep{Name: "cadrage du planner", Args: a("planner", "Découpe le chantier"),
		Replies: []paritySubReply{{Content: "1. lire\n2. écrire"}}})
	run(paritySubStep{Name: "moteur qui refuse", Args: a("explorer", "Panne ?"),
		Replies: []paritySubReply{{Status: 400, Content: `{"error":{"message":"requête refusée"}}`}}})

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{"views": views, "steps": steps}); err != nil {
		t.Fatal(err)
	}
	got := bytes.ReplaceAll(buf.Bytes(), []byte(ws), []byte("{{WS}}"))
	if h := os.Getenv("LOKI_HOME"); h != "" {
		got = bytes.ReplaceAll(got, []byte(h), []byte("{{HOME}}"))
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(paritySubagentFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paritySubagentFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(paritySubagentFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("subagent ne produit plus le journal rangé dans %s.\n"+
			"LOKI_PARITY_UPDATE=1 pour le régénérer si le changement est voulu.", paritySubagentFile)
	}
}
