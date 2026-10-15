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
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// parity_turn_test.go — journaux de référence d'un TOUR COMPLET, tel que le
// voit l'interface (lot 6 de la migration Rust).
//
// Chaque scénario passe par la vraie route /api/chat/send du Go, contre un
// faux moteur : le journal d'affichage du tour (deltas, une fois le tour fini
// et coalescé), l'historique rangé et les corps envoyés au moteur sont rangés
// dans rust/loki-rs/tests/parity_turn/<nom>.json, avec ce que la façade
// demandera au Go pendant le même tour (réponses de /internal/v1/turn/start
// et /turn/view). Le test Rust (tests/lot6.rs, parite_du_tour_complet) rejoue
// le tour par chat::generate et doit retrouver ces journaux.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteTourComplet

const parityTurnDir = "../../rust/loki-rs/tests/parity_turn"

type parityTurnScenario struct {
	Name string            `json:"name"`
	Cfg  map[string]string `json:"cfg"`
	// Agent : mode agent allumé (réglage de la machine, clé d'état « agent »).
	Agent bool `json:"agent,omitempty"`
	// Code : discussion en mode Code, avec ces critères d'acceptation.
	Code        bool              `json:"code,omitempty"`
	Criteria    []Criterion       `json:"criteria,omitempty"`
	Send        map[string]any    `json:"send"`
	Replies     []parityReply     `json:"replies"`
	ToolResults map[string]string `json:"tool_results"`
}

type parityTurnExpect struct {
	Requests []any            `json:"requests"`
	Log      []map[string]any `json:"log"`
	Messages []Message        `json:"messages"`
	Status   int              `json:"status"`
}

type parityTurnFixture struct {
	parityTurnScenario
	// Réponses du Go à la façade pendant ce tour.
	Start    any              `json:"start"`
	View     any              `json:"view"`
	Verifier any              `json:"verifier,omitempty"`
	Expect   parityTurnExpect `json:"expect"`
}

func parityTurnScenarios() []parityTurnScenario {
	send := func(msg string) map[string]any { return map[string]any{"message": msg} }
	base := func(name string, s map[string]any, replies ...parityReply) parityTurnScenario {
		s["cid"] = "c-" + name // un envoi par scénario : jamais pris pour un réessai
		return parityTurnScenario{
			Name: name, Cfg: map[string]string{"MEM_MODE": "off"}, Send: s, Replies: replies,
			ToolResults: map[string]string{"bash": "a.txt\nb.txt", "read": "1: ligne"},
		}
	}
	agent := func(sc parityTurnScenario) parityTurnScenario {
		sc.Agent = true
		return sc
	}
	code := func(sc parityTurnScenario, crit ...Criterion) parityTurnScenario {
		sc.Agent, sc.Code, sc.Criteria = true, true, crit
		return sc
	}
	return []parityTurnScenario{
		base("reponse_simple", send("Bonjour"),
			pOK(pContent("Bon"), pContent("jour !"), pFinish("stop"))),
		base("raisonnement", send("Combien ?"),
			pOK(pReasoning("Je compte."), pContent("Quatre."), pFinish("stop"))),
		agent(base("outil_puis_reponse", send("Liste les fichiers"),
			pOK(pToolCall(0, "c1", "bash", `{"command":"ls"}`), pFinish("tool_calls")),
			pOK(pContent("Deux fichiers."), pFinish("stop")))),
		agent(base("pense_sans_agir", send("Cherche"),
			pOK(pReasoning("Il faudrait lister."), pFinish("stop")),
			pOK(pContent("Voilà."), pFinish("stop")))),
		code(base("code_contrat_deja_tenu", send("Corrige le test"),
			pOK(pContent("Corrigé."), pFinish("stop"))),
			Criterion{ID: 1, Text: "les tests passent", Status: "passed"}),
		code(base("code_contrat_abandonne", send("Construis le jeu"),
			pOK(pContent("r0"), pFinish("stop")), pOK(pContent("r1"), pFinish("stop")),
			pOK(pContent("r2"), pFinish("stop")), pOK(pContent("v1"), pFinish("stop")),
			pOK(pContent("r3"), pFinish("stop")), pOK(pContent("v2"), pFinish("stop")),
			pOK(pContent("r4"), pFinish("stop")), pOK(pContent("v3"), pFinish("stop"))),
			Criterion{ID: 1, Text: "le build passe", Status: "pending"}),
		base("erreur_moteur", send("Bonjour"),
			parityReply{Status: 400, Lines: []string{`{"error":{"code":400,"message":"requête invalide","type":"invalid_request_error"}}`}}),
	}
}

// parityTurnLog : le journal du tour, mesures et horaires masqués.
func parityTurnLog(evs []LogEvent) []map[string]any {
	out := make([]map[string]any, 0, len(evs))
	for _, e := range evs {
		d := map[string]any{}
		for k, v := range e.Delta {
			switch k {
			case "stats":
				v = "…"
			case "ts0":
				continue // horodatage du premier fragment coalescé
			}
			d[k] = v
		}
		out = append(out, d)
	}
	return out
}

func runParityTurn(t *testing.T, sc parityTurnScenario) parityTurnFixture {
	t.Helper()
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	// Playwright installé ou non selon la machine : web_screenshot entrerait ou
	// non dans les outils du mode Code. Hors du PATH, le journal ne dépend plus
	// de la machine qui le tire.
	for {
		p, err := exec.LookPath("playwright")
		if err != nil {
			break
		}
		var keep []string
		for _, d := range filepath.SplitList(os.Getenv("PATH")) {
			if filepath.Clean(d) != filepath.Dir(p) {
				keep = append(keep, d)
			}
		}
		t.Setenv("PATH", strings.Join(keep, string(os.PathListSeparator)))
	}
	var (
		mu       sync.Mutex
		requests []any
		next     int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			io.WriteString(w, `{"status":"ok"}`)
			return
		}
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
		}
		w.WriteHeader(rep.Status)
		var b strings.Builder
		for _, l := range rep.Lines {
			b.WriteString(l + "\n\n")
		}
		io.WriteString(w, b.String())
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	cfg := map[string]string{"PORT": u.Port()}
	for k, v := range sc.Cfg {
		cfg[k] = v
	}
	if err := WriteConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := setAgentEnabled(sc.Agent); err != nil {
		t.Fatal(err)
	}
	execToolHook = func(_ context.Context, name string, _ map[string]any, _ string, _ Caps) toolExecResult {
		if r, ok := sc.ToolResults[name]; ok {
			return toolExecResult{Result: r}
		}
		return toolExecResult{Result: "[erreur] outil inconnu : " + name}
	}
	t.Cleanup(func() { execToolHook = nil })

	conv.Reset()
	if sc.Code {
		id := convEnsureActive()
		if err := setConvMode(id, "code"); err != nil {
			t.Fatal(err)
		}
		critSave(id, sc.Criteria)
	}
	body, _ := json.Marshal(sc.Send)
	// Ce que la façade obtiendra du Go pour ce même envoi.
	start := turnJSON(t, "/internal/v1/turn/start", string(body), 200)
	caps, _ := json.Marshal(map[string]any{"caps": start["turn_caps"]})
	view := turnJSON(t, "/internal/v1/turn/view", string(caps), 200)
	var verifier any
	if sc.Code {
		verifier = turnJSON(t, "/internal/v1/turn/verifier", string(caps), 200)
	}

	w := httptest.NewRecorder()
	handleChatSend(w, httptest.NewRequest("POST", "/api/chat/send", bytes.NewReader(body)))
	deadline := time.Now().Add(10 * time.Second)
	for {
		conv.mu.Lock()
		done := !conv.Generating
		conv.mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	persistQ.flush()
	conv.mu.Lock()
	exp := parityTurnExpect{
		Requests: requests,
		Log:      parityTurnLog(conv.Log),
		Messages: append([]Message{}, conv.Messages...),
		Status:   w.Code,
	}
	conv.mu.Unlock()
	if exp.Requests == nil {
		exp.Requests = []any{}
	}
	return parityTurnFixture{parityTurnScenario: sc, Start: start, View: view, Verifier: verifier, Expect: exp}
}

// parityMasks : ce que le prompt de la machine porte de propre à CE lancement
// (dossiers temporaires, discussion, date, hôte). La façade reçoit ce prompt
// tel quel par /turn/view : masqué ici, il l'est des deux côtés.
var parityMasks = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`(/private)?` + regexp.QuoteMeta(filepath.Clean(os.TempDir())) + `/Test[^/"\\]*/[0-9]+`), "{{TMP}}"},
	{regexp.MustCompile(`\bc[0-9]{15,}\b`), "{{CONV}}"},
	{regexp.MustCompile(`Date: [0-9]{4}-[0-9]{2}-[0-9]{2}`), "Date: {{DATE}}"},
	{regexp.MustCompile(`Machine: host=[^,]*, [a-z0-9]+/[a-z0-9]+(, user=[^,.]*)?`), "Machine: host={{HOST}}"},
}

func parityMask(b []byte) []byte {
	for _, m := range parityMasks {
		b = m.re.ReplaceAll(b, []byte(m.with))
	}
	return b
}

func TestPariteTourComplet(t *testing.T) {
	update := os.Getenv("LOKI_PARITY_UPDATE") == "1"
	for _, sc := range parityTurnScenarios() {
		t.Run(sc.Name, func(t *testing.T) {
			got, err := parityJSON(runParityTurn(t, sc))
			if err != nil {
				t.Fatal(err)
			}
			got = parityMask(got)
			path := filepath.Join(parityTurnDir, sc.Name+".json")
			if update {
				if err := os.MkdirAll(parityTurnDir, 0o755); err != nil {
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
				t.Errorf("le tour Go ne produit plus le journal rangé dans %s.\n"+
					"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteTourComplet,\n"+
					"puis cargo test --test lot6 parite_du_tour_complet dit si le port Rust suit.\n--- obtenu ---\n%s", path, got)
			}
		})
	}
}
