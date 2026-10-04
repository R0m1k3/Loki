package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Les faux moteurs des autres tests ne répondent pas tous en JSON sur /props :
// la sonde automatique (lancée après chaque complétion du fil principal) y est
// coupée, et appelée explicitement ici.
func TestMain(m *testing.M) {
	tplProbeAuto = false
	os.Exit(m.Run())
}

func freshTplProbe(t *testing.T) {
	t.Helper()
	reset := func() {
		tplProbeMu.Lock()
		tplProbeCache = map[string]tplProbeResult{}
		tplProbeLast = nil
		tplProbeByShape = map[string]tplProbeResult{}
		tplProbeRunning = false
		tplProbeTried = time.Time{}
		tplProbeLogged = ""
		tplProbeMu.Unlock()
	}
	reset()
	old := tplProbeBackoff
	tplProbeBackoff = time.Millisecond
	t.Cleanup(func() { reset(); tplProbeBackoff = old })
}

// fakeTpl : un llama-server réduit à /health, /props et /apply-template, avec
// un gabarit façon ChatML écrit à la main.
//
//	qwen3 : le raisonnement n'est rendu qu'après le dernier message utilisateur ;
//	keep  : il est toujours rendu ;
//	noagp : comme keep, mais add_generation_prompt est ignoré (vieux moteur) ;
//	drop  : reasoning_content n'est jamais rendu.
type fakeTpl struct {
	mu          sync.Mutex
	mode        string
	key         string // Bearer exigé hors /health
	applyStatus []int  // statuts imposés aux prochains /apply-template (0 = rendu)
	applyBody   string
	paths       []string
	applies     []map[string]any
}

var reCallID = regexp.MustCompile(`^[A-Za-z0-9]{9}$`)

func (f *fakeTpl) render(body map[string]any) (string, int) {
	msgs, _ := body["messages"].([]any)
	lastUser := -1
	for i, m := range msgs {
		if m.(map[string]any)["role"] == "user" {
			lastUser = i
		}
	}
	var b strings.Builder
	if tools, ok := body["tools"].([]any); ok {
		fmt.Fprintf(&b, "<tools n=%d>\n", len(tools))
	}
	for i, raw := range msgs {
		m := raw.(map[string]any)
		role, _ := m["role"].(string)
		b.WriteString("<|im_start|>" + role + "\n")
		if r, _ := m["reasoning_content"].(string); r != "" && role == "assistant" && f.mode != "drop" && (f.mode != "qwen3" || i > lastUser) {
			b.WriteString("<think>" + r + "</think>")
		}
		c, _ := m["content"].(string)
		b.WriteString(c)
		calls, _ := m["tool_calls"].([]any)
		for _, rc := range calls {
			call := rc.(map[string]any)
			id, _ := call["id"].(string)
			// Gabarit strict : identifiant de 9 alphanumériques, résultat qui suit.
			if !reCallID.MatchString(id) || i+1 >= len(msgs) || msgs[i+1].(map[string]any)["tool_call_id"] != id {
				return `{"error":{"code":500,"message":"Jinja Exception: tool call id invalide"}}`, 500
			}
			fn := call["function"].(map[string]any)
			b.WriteString("<tool_call>" + fn["name"].(string) + "</tool_call>")
		}
		b.WriteString("<|im_end|>\n")
	}
	if agp, _ := body["add_generation_prompt"].(bool); agp || f.mode == "noagp" {
		b.WriteString("<|im_start|>assistant\n")
	}
	out, _ := json.Marshal(map[string]string{"prompt": b.String()})
	return string(out), 200
}

func (f *fakeTpl) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.URL.Path)
	if r.URL.Path == "/health" {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return
	}
	if f.key != "" && r.Header.Get("Authorization") != "Bearer "+f.key {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":401,"message":"Invalid API Key"}}`))
		return
	}
	switch r.URL.Path {
	case "/props":
		fmt.Fprintf(w, `{"build_info":"b7000-abc","model_path":"/models/Qwen3-8B-Q8_0.gguf","chat_template":%q}`, f.mode)
	case "/apply-template":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.applies = append(f.applies, body)
		if len(f.applyStatus) > 0 {
			s := f.applyStatus[0]
			f.applyStatus = f.applyStatus[1:]
			if s != 0 {
				w.WriteHeader(s)
				_, _ = w.Write([]byte(f.applyBody))
				return
			}
		}
		out, status := f.render(body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(out))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeTpl) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeTpl) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.paths {
		if p == path {
			n++
		}
	}
	return n
}

// Jamais de complétion : avec un seul slot, elle évincerait le cache de la
// conversation que la sonde est censée servir.
func (f *fakeTpl) assertNoCompletion(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.paths {
		if strings.Contains(p, "completion") {
			t.Fatalf("la sonde a appelé %s", p)
		}
	}
}

func probeTools() []Tool {
	return []Tool{{Type: "function", Function: ToolFunction{Name: "bash", Description: "d", Parameters: map[string]any{"type": "object"}}}}
}

func TestSondeGabaritQwen3(t *testing.T) {
	testHome(t)
	freshTplProbe(t)
	f := &fakeTpl{mode: "qwen3"}
	f.start(t)
	kw := map[string]any{"enable_thinking": false}
	r := tplProbeEnsure(context.Background(), newTplShape(probeTools(), kw, ""))
	if r.PreservesHistory != tplNo || r.PrefixStable != tplNo || r.RendersReasoning != tplYes {
		t.Fatalf("Qwen3 retire le raisonnement passé : %+v", r)
	}
	if r.Build != "b7000-abc" || r.Model != "Qwen3-8B-Q8_0.gguf" || r.Tools != 1 || !r.cacheable {
		t.Fatalf("métadonnées : %+v", r)
	}
	f.assertNoCompletion(t)
	// Mêmes outils et chat_template_kwargs que la complétion ; jamais
	// d'amorce de réponse, sauf le rendu qui vérifie que le moteur l'honore.
	gen := 0
	for _, a := range f.applies {
		if tools, _ := a["tools"].([]any); len(tools) != 1 {
			t.Fatalf("outils non transmis : %v", a["tools"])
		}
		if k, _ := a["chat_template_kwargs"].(map[string]any); k["enable_thinking"] != false {
			t.Fatalf("chat_template_kwargs : %v", a["chat_template_kwargs"])
		}
		if a["add_generation_prompt"] == true {
			gen++
		}
	}
	if len(f.applies) != 4 || gen != 1 {
		t.Fatalf("%d rendus dont %d avec amorce", len(f.applies), gen)
	}
	// Même moteur, même modèle, même gabarit, même forme : réponse en cache.
	tplProbeEnsure(context.Background(), newTplShape(probeTools(), kw, ""))
	if n := f.count("/apply-template"); n != 4 {
		t.Fatalf("cache ignoré : %d rendus", n)
	}
	// Autre forme (raisonnement non coupé) : nouvelle sonde.
	tplProbeEnsure(context.Background(), newTplShape(probeTools(), nil, ""))
	if n := f.count("/apply-template"); n != 8 {
		t.Fatalf("forme différente non resondée : %d rendus", n)
	}
	// Exposé dans /api/perf/summary.
	rec := httptest.NewRecorder()
	handlePerfSummary(rec, httptest.NewRequest(http.MethodGet, "/api/perf/summary", nil))
	var sum struct {
		Template *struct {
			PreservesHistory string `json:"preserves_history"`
			PrefixStable     string `json:"prefix_stable"`
		} `json:"template"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil || sum.Template == nil || sum.Template.PrefixStable != "no" {
		t.Fatalf("résumé : %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "loki-sonde") {
		t.Fatal("un rendu a fuité dans le résumé")
	}
}

func TestSondeGabaritConserveLHistorique(t *testing.T) {
	testHome(t)
	freshTplProbe(t)
	f := &fakeTpl{mode: "keep"}
	f.start(t)
	r := tplProbeEnsure(context.Background(), newTplShape(nil, nil, ""))
	if r.PreservesHistory != tplYes || r.PrefixStable != tplYes || r.RendersReasoning != tplYes {
		t.Fatalf("gabarit qui garde tout : %+v", r)
	}
	for _, a := range f.applies {
		if _, ok := a["tools"]; ok {
			t.Fatal("outils envoyés alors que la complétion n'en avait pas")
		}
	}
}

// Gabarit (ou moteur) qui ignore reasoning_content partout : « jamais rendu »,
// ce qui suffit à REASONING_ECHO pour ne rien renvoyer d'inutile.
func TestSondeGabaritRaisonnementJamaisRendu(t *testing.T) {
	testHome(t)
	freshTplProbe(t)
	f := &fakeTpl{mode: "drop"}
	f.start(t)
	r := tplProbeEnsure(context.Background(), newTplShape(probeTools(), nil, ""))
	if r.RendersReasoning != tplNo || r.PreservesHistory != tplNo || !strings.Contains(r.Note, "jamais rendu") {
		t.Fatalf("raisonnement jamais rendu : %+v", r)
	}
	if !strings.Contains(tplProbeLine(r), "raisonnement_rendu=no") {
		t.Fatalf("journal : %s", tplProbeLine(r))
	}
}

// Un moteur qui ignore add_generation_prompt ne doit pas faire conclure
// « instable » : l'amorce rendue dans les deux cas fausserait la comparaison.
func TestSondeGabaritAmorceIgnoree(t *testing.T) {
	testHome(t)
	freshTplProbe(t)
	f := &fakeTpl{mode: "noagp"}
	f.start(t)
	r := tplProbeEnsure(context.Background(), newTplShape(nil, nil, ""))
	if r.PrefixStable != tplUnknown || r.PreservesHistory != tplYes {
		t.Fatalf("amorce ignorée : %+v", r)
	}
}

// Moteur ancien ou fork sans la route : inconnu, rangé (ça ne changera pas
// avant un autre binaire), et aucun repli sur une complétion.
func TestSondeGabarit404(t *testing.T) {
	testHome(t)
	freshTplProbe(t)
	f := &fakeTpl{mode: "qwen3", applyStatus: []int{404, 404, 404, 404}, applyBody: "Not Found"}
	f.start(t)
	r := tplProbeEnsure(context.Background(), newTplShape(probeTools(), nil, ""))
	if r.PreservesHistory != tplUnknown || r.PrefixStable != tplUnknown || r.RendersReasoning != tplUnknown || !r.cacheable {
		t.Fatalf("404 : %+v", r)
	}
	f.assertNoCompletion(t)
	tplProbeEnsure(context.Background(), newTplShape(probeTools(), nil, ""))
	if n := f.count("/apply-template"); n != 2 {
		t.Fatalf("404 resondé : %d rendus", n)
	}
}

// Gabarit qui lève une exception (raise_exception) : inconnu, jamais oui/non.
func TestSondeGabaritExceptionDuGabarit(t *testing.T) {
	testHome(t)
	freshTplProbe(t)
	f := &fakeTpl{mode: "qwen3", applyStatus: []int{500, 500, 500, 500},
		applyBody: `{"error":{"code":500,"message":"Jinja Exception: Conversation roles must alternate"}}`}
	f.start(t)
	r := tplProbeEnsure(context.Background(), newTplShape(probeTools(), nil, ""))
	if r.PreservesHistory != tplUnknown || r.PrefixStable != tplUnknown || !strings.Contains(r.Note, "500") {
		t.Fatalf("exception du gabarit : %+v", r)
	}
	f.assertNoCompletion(t)
}

// Moteur protégé par API_KEY : la sonde s'authentifie comme les autres appels
// internes. Sans clé, inconnu — et pas rangé : la clé peut arriver.
func TestSondeGabarit401(t *testing.T) {
	testHome(t)
	freshTplProbe(t)
	f := &fakeTpl{mode: "keep", key: "sk-loki-test"}
	f.start(t)
	r := tplProbeEnsure(context.Background(), newTplShape(nil, nil, ""))
	if r.PreservesHistory != tplUnknown || r.PrefixStable != tplUnknown || !strings.Contains(r.Note, "401") {
		t.Fatalf("401 : %+v", r)
	}
	if err := writeAPIKey("sk-loki-test"); err != nil {
		t.Fatal(err)
	}
	r = tplProbeEnsure(context.Background(), newTplShape(nil, nil, ""))
	if r.PreservesHistory != tplYes || r.PrefixStable != tplYes {
		t.Fatalf("avec la clé : %+v", r)
	}
}

// Modèle en chargement : les 503 sont relancés, sans rien conclure entre-temps.
func TestSondeGabarit503Relance(t *testing.T) {
	testHome(t)
	freshTplProbe(t)
	f := &fakeTpl{mode: "qwen3", applyStatus: []int{503, 503}, applyBody: `{"error":{"code":503,"message":"Loading model"}}`}
	f.start(t)
	r := tplProbeEnsure(context.Background(), newTplShape(nil, nil, ""))
	if r.PreservesHistory != tplNo || r.PrefixStable != tplNo {
		t.Fatalf("après 503 : %+v", r)
	}
	// 503 persistant : inconnu, non rangé.
	freshTplProbe(t)
	f.mu.Lock()
	f.applyStatus = []int{503, 503, 503, 503, 503, 503, 503, 503, 503, 503, 503, 503}
	f.mu.Unlock()
	r = tplProbeEnsure(context.Background(), newTplShape(nil, nil, ""))
	if r.PreservesHistory != tplUnknown || r.cacheable {
		t.Fatalf("503 persistant : %+v", r)
	}
}

// Preset externe : aucune requête, nulle part, et rien d'affiché.
func TestSondeGabaritPresetExterne(t *testing.T) {
	testHome(t)
	freshTplProbe(t)
	f := &fakeTpl{mode: "keep"}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	if err := WriteConfig(parseEnv(externalPresetContent(srv.URL, "gpt-4o-mini", "", "", false))); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(srv.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}
	r := tplProbeEnsure(context.Background(), newTplShape(nil, nil, ""))
	if r.PreservesHistory != tplUnknown || r.PrefixStable != tplUnknown {
		t.Fatalf("preset externe : %+v", r)
	}
	if n := len(f.paths); n != 0 {
		t.Fatalf("preset externe sondé : %v", f.paths)
	}
	if _, ok := tplCapsCurrent(); ok {
		t.Fatal("verdict affiché pour un preset externe")
	}
}

// Bout en bout : une complétion du fil principal déclenche la sonde en tâche
// de fond, avec les outils et kwargs de CETTE requête, sans rien ajouter à la
// requête de complétion.
func TestSondeGabaritDeclencheeParRunChat(t *testing.T) {
	testHome(t)
	freshTplProbe(t)
	freshPerf(t)
	tplProbeAuto = true
	t.Cleanup(func() { tplProbeAuto = false })
	if err := SetConfigKey("REASONING", "off"); err != nil {
		t.Fatal(err)
	}
	f := &fakeTpl{mode: "qwen3"}
	var mu sync.Mutex
	var chat map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&chat)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(sseChunk("ok") +
				`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":1},"timings":{"prompt_n":50,"cache_n":0}}` + "\n\n" +
				"data: [DONE]\n\n"))
			return
		}
		f.handler(w, r)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}
	tools := probeTools()
	if _, err := runChatTools(context.Background(), []Message{{Role: "user", Content: "salut"}}, tools, 0.7, Caps{}, func(StreamEvent) bool { return true }); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var r tplProbeResult
	for {
		var ok bool
		if r, ok = tplCapsCurrent(); ok || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.PrefixStable != tplNo || r.Tools != 1 || r.Kwargs["enable_thinking"] != false {
		t.Fatalf("sonde après complétion : %+v", r)
	}
	f.mu.Lock()
	for _, a := range f.applies {
		mu.Lock()
		same := fmt.Sprint(a["tools"]) == fmt.Sprint(chat["tools"]) && fmt.Sprint(a["chat_template_kwargs"]) == fmt.Sprint(chat["chat_template_kwargs"])
		mu.Unlock()
		if !same {
			f.mu.Unlock()
			t.Fatalf("forme différente de la complétion :\nsonde %v / %v\nchat  %v / %v", a["tools"], a["chat_template_kwargs"], chat["tools"], chat["chat_template_kwargs"])
		}
	}
	f.mu.Unlock()
	// La sonde ne laisse aucune trace dans la requête de complétion.
	mu.Lock()
	defer mu.Unlock()
	for k := range chat {
		if strings.Contains(k, "template") && k != "chat_template_kwargs" || k == "add_generation_prompt" {
			t.Fatalf("clé %q ajoutée à la complétion", k)
		}
	}
}

// Le verdict est rangé par forme de requête : la dernière forme sondée ne
// décide pas pour une autre (NUDGE_IN_TOOL), et une forme jamais sondée n'a
// pas de verdict.
func TestSondeGabaritVerdictParForme(t *testing.T) {
	testHome(t)
	freshTplProbe(t)
	f := &fakeTpl{mode: "qwen3"}
	f.start(t)
	a := newTplShape(probeTools(), nil, "")
	b := newTplShape(nil, nil, "")
	if r := tplProbeEnsure(context.Background(), a); r.PrefixStable != tplNo {
		t.Fatalf("forme A : %+v", r)
	}
	f.mu.Lock()
	f.mode = "keep"
	f.mu.Unlock()
	if r := tplProbeEnsure(context.Background(), b); r.PrefixStable != tplYes {
		t.Fatalf("forme B : %+v", r)
	}
	if r, ok := tplCapsCurrent(); !ok || r.PrefixStable != tplYes {
		t.Errorf("affichage : la dernière sondée, B : %+v", r)
	}
	if r, ok := tplCapsFor(a); !ok || r.PrefixStable != tplNo {
		t.Errorf("forme A écrasée par B : %+v %v", r, ok)
	}
	if _, ok := tplCapsFor(newTplShape(probeTools(), map[string]any{"enable_thinking": false}, "")); ok {
		t.Error("verdict pour une forme jamais sondée")
	}
}
