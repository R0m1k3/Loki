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

// parity_test.go — journaux de référence de la boucle d'un tour (lot 6 de la
// migration Rust).
//
// Chaque scénario rejoue runChatTools contre un faux moteur aux réponses
// scriptées, outils aux résultats fixés (execToolHook). Ce qui en sort — corps
// des requêtes envoyées au moteur, événements d'affichage, messages ajoutés à
// l'historique, erreur — est rangé dans rust/loki-rs/tests/parity/<nom>.json.
// Le test Rust (tests/parity.rs) rejoue les mêmes scénarios dans chat::turn et
// doit retrouver EXACTEMENT ces journaux : l'octet envoyé au moteur compte (le
// cache de préfixe en dépend), l'affichage aussi.
//
// Ici, le journal produit doit être celui qui est rangé : un changement de
// comportement de la boucle Go se voit, et se régénère sciemment —
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteJournaux
//
// — après quoi le test Rust dit si le port suit.

const parityDir = "../../rust/loki-rs/tests/parity"

// parityReply : une réponse scriptée du moteur. Lignes du corps, chacune
// suivie d'une ligne vide (trames SSE).
type parityReply struct {
	Status int      `json:"status"`
	Lines  []string `json:"lines"`
}

type parityScenario struct {
	Name        string            `json:"name"`
	Cfg         map[string]string `json:"cfg"`
	Messages    []Message         `json:"messages"`
	Tools       []Tool            `json:"tools"`
	Temperature float64           `json:"temperature"`
	Replies     []parityReply     `json:"replies"`
	ToolResults map[string]string `json:"tool_results"`
}

type parityExpect struct {
	Requests []any            `json:"requests"`
	Events   []map[string]any `json:"events"`
	Extra    []Message        `json:"extra"`
	Err      string           `json:"err"`
}

type parityFixture struct {
	parityScenario
	Expect parityExpect `json:"expect"`
}

// --- Trames du faux moteur --------------------------------------------------

func pDelta(delta map[string]any) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
	return "data: " + string(b)
}

func pContent(s string) string   { return pDelta(map[string]any{"content": s}) }
func pReasoning(s string) string { return pDelta(map[string]any{"reasoning_content": s}) }

func pToolCall(i int, id, name, args string) string {
	return pDelta(map[string]any{"tool_calls": []any{map[string]any{
		"index": i, "id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": args},
	}}})
}

func pFinish(reason string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": reason}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 5},
	})
	return "data: " + string(b)
}

func pOK(lines ...string) parityReply {
	return parityReply{Status: 200, Lines: append(lines, "data: [DONE]")}
}

// --- Scénarios ---------------------------------------------------------------

func parityScenarios() []parityScenario {
	sys := Message{Role: "system", Content: "Tu es Loki."}
	user := func(s string) Message { return Message{Role: "user", Content: s} }
	tools := []Tool{readTool(), bashTool()}
	base := func(name string, msgs []Message, replies ...parityReply) parityScenario {
		return parityScenario{
			Name: name, Cfg: map[string]string{}, Messages: msgs, Tools: tools,
			Temperature: 0.7, Replies: replies,
			ToolResults: map[string]string{"read": "ligne 1\nligne 2", "bash": "a.txt\nb.txt"},
		}
	}
	return []parityScenario{
		base("reponse_simple", []Message{sys, user("Bonjour")},
			pOK(pContent("Bon"), pContent("jour !"), pFinish("stop"))),

		base("raisonnement_puis_reponse", []Message{sys, user("Combien font 2+2 ?")},
			pOK(pReasoning("Je calcule."), pReasoning(" 4."), pContent("4"), pFinish("stop"))),

		base("outil_puis_reponse", []Message{sys, user("Lis a.txt")},
			pOK(pToolCall(0, "c1", "read", `{"file":"a.txt"}`), pFinish("tool_calls")),
			pOK(pContent("Le fichier a deux lignes."), pFinish("stop"))),

		base("deux_outils_en_parallele", []Message{sys, user("Liste puis lis")},
			pOK(pToolCall(0, "c1", "bash", `{"command":"ls"}`),
				pToolCall(1, "c2", "read", `{"file":"a.txt"}`), pFinish("tool_calls")),
			pOK(pContent("Fait."), pFinish("stop"))),

		base("appel_repete", []Message{sys, user("Lis a.txt")},
			pOK(pToolCall(0, "c1", "read", `{"file":"a.txt"}`), pFinish("tool_calls")),
			pOK(pToolCall(0, "c2", "read", `{"file":"a.txt"}`), pFinish("tool_calls")),
			pOK(pContent("Toujours deux lignes."), pFinish("stop"))),

		base("arguments_illisibles", []Message{sys, user("Lis a.txt")},
			pOK(pToolCall(0, "c1", "read", `{"file":`), pFinish("tool_calls")),
			pOK(pContent("Pardon."), pFinish("stop"))),

		base("pense_sans_agir", []Message{sys, user("Cherche")},
			pOK(pReasoning("Il faudrait lister les fichiers."), pFinish("stop")),
			pOK(pContent("Voilà."), pFinish("stop"))),

		base("erreur_500_avec_outils", []Message{sys, user("Bonjour")},
			parityReply{Status: 500, Lines: []string{`{"error":{"code":500,"message":"Failed to parse tool call","type":"server_error"}}`}},
			pOK(pContent("Salut."), pFinish("stop"))),

		base("erreur_moteur", []Message{sys, user("Bonjour")},
			parityReply{Status: 400, Lines: []string{`{"error":{"code":400,"message":"requête invalide","type":"invalid_request_error"}}`}}),

		base("appel_textuel", []Message{sys, user("Lis a.txt")},
			pOK(pContent(`<tool_call>{"name":"read","arguments":{"file":"a.txt"}}</tool_call>`), pFinish("stop")),
			pOK(pToolCall(0, "c1", "read", `{"file":"a.txt"}`), pFinish("tool_calls")),
			pOK(pContent("Deux lignes."), pFinish("stop"))),

		base("outil_inconnu", []Message{sys, user("Fais-le")},
			pOK(pToolCall(0, "c1", "voler", `{}`), pFinish("tool_calls")),
			pOK(pContent("Impossible."), pFinish("stop"))),

		with(base("budget_depasse", []Message{sys, user("Explore")},
			pOK(pToolCall(0, "c1", "bash", `{"command":"ls 1"}`), pFinish("tool_calls")),
			pOK(pToolCall(0, "c2", "bash", `{"command":"ls 2"}`), pFinish("tool_calls")),
			pOK(pToolCall(0, "c3", "bash", `{"command":"ls 3"}`), pFinish("tool_calls")),
			pOK(pContent("J'arrête là."), pFinish("stop"))),
			"AGENT_BUDGET", "2"),

		base("pense_sans_agir_deux_fois", []Message{sys, user("Cherche")},
			pOK(pReasoning("Lister les fichiers."), pFinish("stop")),
			pOK(pReasoning("Toujours lister."), pFinish("stop")),
			pOK(pContent("Voilà."), pFinish("stop"))),

		with(base("pensee_en_ligne", []Message{sys, user("Salut")},
			pOK(pContent("<think>Il dit bonjour."), pContent("</think>Bonjour !"), pFinish("stop"))),
			"REASONING", "on"),

		with(with(with(base("echantillonnage_et_effort", []Message{sys, user("Salut")},
			pOK(pContent("Bonjour."), pFinish("stop"))),
			"TOP_P", "0.9"), "TOP_K", "40"), "REASONING_EFFORT", "low"),

		with(base("raisonnement_coupe", []Message{sys, user("Salut")},
			pOK(pContent("Bonjour."), pFinish("stop"))),
			"REASONING", "off"),

		base("erreur_500_deux_fois", []Message{sys, user("Bonjour")},
			parityReply{Status: 500, Lines: []string{`{"error":{"code":500,"message":"Failed to parse tool call","type":"server_error"}}`}},
			parityReply{Status: 500, Lines: []string{`{"error":{"code":500,"message":"Failed to parse tool call","type":"server_error"}}`}},
			pOK(pContent("Salut."), pFinish("stop"))),

		with(with(with(base("api_externe", []Message{sys, user("Lis a.txt")},
			pOK(pReasoning("Je lis."), pToolCall(0, "c1", "read", `{"file":"a.txt"}`), pFinish("tool_calls")),
			pOK(pContent("Deux lignes."), pFinish("stop"))),
			extKeyFlag, "1"), extKeyURL, parityURL), extKeyModel, "gpt-x"),

		base("historique_avec_outils", []Message{sys, user("Lis a.txt"),
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c0", Type: "function", Function: ToolCallFunc{Name: "read", Arguments: `{"file":"a.txt"}`}}}},
			{Role: "tool", ToolCallID: "c0", Content: "ligne 1"},
			{Role: "assistant", Content: "Une ligne."},
			user("Et b.txt ?")},
			pOK(pToolCall(0, "c1", "read", `{"file":"b.txt"}`), pFinish("tool_calls")),
			pOK(pContent("Deux lignes."), pFinish("stop"))),
	}
}

// parityURL : remplacé par l'adresse du faux moteur au rejeu (côté Rust,
// l'adresse ne compte pas : l'hôte répond lui-même).
const parityURL = "{{URL}}"

// with pose une clé de configuration sur le scénario.
func with(sc parityScenario, k, v string) parityScenario {
	cfg := map[string]string{}
	for a, b := range sc.Cfg {
		cfg[a] = b
	}
	cfg[k] = v
	sc.Cfg = cfg
	return sc
}

// --- Rejeu côté Go -------------------------------------------------------------

// parityEvent : forme comparable d'un événement (nil : ignoré — mesures, qui
// dépendent du temps).
func parityEvent(ev StreamEvent) map[string]any {
	switch {
	case ev.Content != "":
		return map[string]any{"content": ev.Content}
	case ev.Reasoning != "":
		return map[string]any{"reasoning": ev.Reasoning}
	case ev.ToolUsed != nil:
		t := ev.ToolUsed
		m := map[string]any{"name": t.Name}
		put := func(k string, v any, keep bool) {
			if keep {
				m[k] = v
			}
		}
		put("label", t.Label, t.Label != "")
		put("result", t.Result, t.Result != "")
		put("done", true, t.Done)
		put("typing", true, t.Typing)
		put("body", t.Body, t.Body != "")
		put("body_tail", true, t.BodyTail)
		put("body_lines", t.BodyLines, t.BodyLines != 0)
		put("diff", t.Diff, len(t.Diff) > 0)
		put("added", t.Added, t.Added != 0)
		put("removed", t.Removed, t.Removed != 0)
		put("image", t.Image, t.Image != "")
		put("shot", t.Shot, t.Shot != "")
		put("result_chars", t.ResultChars, t.ResultChars != 0)
		put("result_id", t.ResultID, t.ResultID != "")
		return map[string]any{"tool_used": m}
	case ev.Ask != nil:
		return map[string]any{"ask": ev.Ask}
	case ev.Err != nil:
		return map[string]any{"err": ev.Err.Error()}
	case ev.DropReasoning:
		return map[string]any{"drop_reasoning": true}
	case ev.Compacting != nil:
		return map[string]any{"compacting": *ev.Compacting}
	case ev.NewHistory != nil:
		return map[string]any{"new_history": ev.NewHistory}
	case ev.Echo != nil:
		return map[string]any{"echo": map[string]any{"text": ev.Echo.Text, "model": ev.Echo.Model}}
	}
	return nil
}

// parityMerge regroupe les fragments consécutifs de texte : le découpage
// d'un flux en événements n'est pas un comportement.
func parityMerge(evs []map[string]any) []map[string]any {
	var out []map[string]any
	for _, e := range evs {
		if n := len(out); n > 0 {
			for _, k := range []string{"content", "reasoning"} {
				a, ok1 := out[n-1][k].(string)
				b, ok2 := e[k].(string)
				if ok1 && ok2 && len(out[n-1]) == 1 && len(e) == 1 {
					out[n-1] = map[string]any{k: a + b}
					e = nil
					break
				}
			}
		}
		if e != nil {
			out = append(out, e)
		}
	}
	return out
}

func runParity(t *testing.T, sc parityScenario) parityExpect {
	t.Helper()
	testHome(t)
	var (
		mu       sync.Mutex
		requests []any
		next     int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		requests = append(requests, body)
		i := next
		next++
		mu.Unlock()
		if i >= len(sc.Replies) {
			t.Errorf("%s : requête %d non scriptée", sc.Name, i+1)
			http.Error(w, "non scripté", http.StatusTeapot)
			return
		}
		rep := sc.Replies[i]
		if rep.Status == 200 {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(rep.Status)
		var b strings.Builder
		for _, l := range rep.Lines {
			b.WriteString(l + "\n\n")
		}
		_, _ = w.Write([]byte(b.String()))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]string{"PORT": u.Port()}
	for k, v := range sc.Cfg {
		cfg[k] = strings.ReplaceAll(v, parityURL, srv.URL)
	}
	if err := WriteConfig(cfg); err != nil {
		t.Fatal(err)
	}
	execToolHook = func(_ context.Context, name string, _ map[string]any, _ string, _ Caps) toolExecResult {
		if r, ok := sc.ToolResults[name]; ok {
			return toolExecResult{Result: r}
		}
		return toolExecResult{Result: "[erreur] outil inconnu : " + name}
	}
	t.Cleanup(func() { execToolHook = nil })

	var evs []map[string]any
	msgs := append([]Message(nil), sc.Messages...)
	extra, err := runChatTools(context.Background(), msgs, sc.Tools, sc.Temperature, Caps{}, func(ev StreamEvent) bool {
		if e := parityEvent(ev); e != nil {
			evs = append(evs, e)
		}
		return true
	})
	exp := parityExpect{Requests: requests, Events: parityMerge(evs), Extra: extra}
	if err != nil {
		exp.Err = err.Error()
	}
	if exp.Requests == nil {
		exp.Requests = []any{}
	}
	if exp.Events == nil {
		exp.Events = []map[string]any{}
	}
	if exp.Extra == nil {
		exp.Extra = []Message{}
	}
	return exp
}

// parityJSON : JSON stable (clés triées par encoding/json), indenté.
func parityJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func TestPariteJournaux(t *testing.T) {
	update := os.Getenv("LOKI_PARITY_UPDATE") == "1"
	seen := map[string]bool{}
	for _, sc := range parityScenarios() {
		t.Run(sc.Name, func(t *testing.T) {
			if seen[sc.Name] {
				t.Fatalf("scénario en double : %s", sc.Name)
			}
			seen[sc.Name] = true
			fx := parityFixture{parityScenario: sc, Expect: runParity(t, sc)}
			got, err := parityJSON(fx)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parityDir, sc.Name+".json")
			if update {
				if err := os.MkdirAll(parityDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("la boucle Go ne produit plus le journal rangé dans %s.\n"+
					"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteJournaux,\n"+
					"puis cargo test --test parity dit si le port Rust suit.\n--- obtenu ---\n%s", path, got)
			}
		})
	}
	if update {
		return
	}
	// Pas de journal orphelin : un scénario retiré ici l'est aussi là-bas.
	files, _ := filepath.Glob(filepath.Join(parityDir, "*.json"))
	for _, f := range files {
		if name := strings.TrimSuffix(filepath.Base(f), ".json"); !seen[name] {
			t.Errorf("journal sans scénario : %s", f)
		}
	}
}
