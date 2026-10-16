package loki

import (
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

// parity_tplprobe_test.go — journal de référence de la sonde de gabarit
// (lot 23 de la migration Rust) : pour chaque comportement de moteur (gabarit
// Qwen3, gabarit qui garde tout, raisonnement jamais rendu, amorce ignorée,
// 404, exception du gabarit, clé exigée, 503 relancés ou persistants, /props
// illisible, moteur pas prêt, réponse sans prompt, rendu incohérent, preset
// externe), chaque échange avec le moteur — méthode, chemin, clé, corps envoyé,
// réponse — puis le verdict, sa ligne de journal, ce qu'en disent
// tplCapsCurrent et tplCapsFor, et le cache (même moteur : pas de nouveau
// rendu ; gabarit changé : nouvelle sonde). Puis le relevé du rendu
// (tplRenderPrint), sa comparaison (tplRenderDiff) et l'empreinte des formes.
// Rangé dans rust/loki-rs/tests/parity_tplprobe/tplprobe.json ;
// tests/tplprobe.rs rejoue chaque échange contre le port Rust.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteSondeGabarit

const parityTplFile = "../../rust/loki-rs/tests/parity_tplprobe/tplprobe.json"

// Un échange avec le moteur, tel que le moteur l'a vu.
type parityTplExchange struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Auth   string `json:"auth,omitempty"`
	Body   string `json:"body,omitempty"`
	Status int    `json:"status"`
	Reply  string `json:"reply"`
}

// parityTplEngine : le faux moteur des tests de la sonde (fakeTpl), avec des
// réponses imposées par chemin et le journal des échanges.
type parityTplEngine struct {
	fake      *fakeTpl
	mu        sync.Mutex
	override  map[string][]parityTplExchange // chemin → réponses imposées, consommées dans l'ordre
	exchanges []parityTplExchange
}

func (e *parityTplEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	e.mu.Lock()
	var forced *parityTplExchange
	if l := e.override[r.URL.Path]; len(l) > 0 {
		forced = &l[0]
		e.override[r.URL.Path] = l[1:]
	}
	e.mu.Unlock()
	rec := httptest.NewRecorder()
	if forced != nil {
		rec.WriteHeader(forced.Status)
		_, _ = rec.WriteString(forced.Reply)
	} else {
		r2 := r.Clone(r.Context())
		r2.Body = io.NopCloser(strings.NewReader(string(body)))
		e.fake.handler(rec, r2)
	}
	ex := parityTplExchange{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"),
		Body: string(body), Status: rec.Code, Reply: rec.Body.String()}
	e.mu.Lock()
	e.exchanges = append(e.exchanges, ex)
	e.mu.Unlock()
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

// parityTplResult : le verdict sans l'instant, plus ce qui n'est pas exporté.
func parityTplResult(r tplProbeResult) map[string]any {
	b, _ := json.Marshal(r)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	delete(m, "at")
	m["cacheable"] = r.cacheable
	return m
}

func TestPariteSondeGabarit(t *testing.T) {
	type step struct {
		Shape     string                         `json:"shape"` // forme rejouée (shapes)
		Mode      string                         `json:"mode,omitempty"`
		Template  string                         `json:"template,omitempty"`
		Build     string                         `json:"build,omitempty"`
		Override  map[string][]parityTplExchange `json:"override,omitempty"`
		Exchanges []parityTplExchange            `json:"exchanges"`
		Result    any                            `json:"result"`
		Line      string                         `json:"line"`
		Current   any                            `json:"current"`
		For       any                            `json:"for"`
	}
	type scenario struct {
		Name     string            `json:"name"`
		Key      string            `json:"key,omitempty"`      // clé exigée par le moteur
		APIKey   string            `json:"api_key,omitempty"`  // clé connue de Loki (state/api_key)
		External bool              `json:"external,omitempty"` // preset externe actif
		Config   map[string]string `json:"config,omitempty"`
		Steps    []step            `json:"steps"`
	}
	shapes := map[string]struct {
		Tools  string         `json:"tools"`
		Kwargs map[string]any `json:"kwargs"`
		Effort string         `json:"effort"`
	}{
		"nue":      {},
		"outils":   {Tools: `[{"type":"function","function":{"name":"bash","description":"d <b> & é","parameters":{"type":"object"}}}]`, Kwargs: map[string]any{"enable_thinking": false}},
		"kwargs":   {Kwargs: map[string]any{"enable_thinking": true, "reasoning_effort": "high"}},
		"effort":   {Effort: "low"},
		"deux":     {Tools: `[{"type":"function","function":{"name":"lire","description":"","parameters":{"type":"object","properties":{"p":{"type":"string"}}}}},{"type":"function","function":{"name":"écrire","description":"x","parameters":null}}]`},
		"kw-vides": {Kwargs: map[string]any{}},
	}
	shapeOf := func(name string) tplShape {
		s := shapes[name]
		var tools []Tool
		if s.Tools != "" {
			if err := json.Unmarshal([]byte(s.Tools), &tools); err != nil {
				t.Fatal(err)
			}
		}
		return newTplShape(tools, s.Kwargs, s.Effort)
	}
	jsonErr := `{"error":{"code":500,"message":"Jinja Exception: ` + strings.Repeat("é", 200) + `"}}`
	scenarios := []scenario{
		{Name: "Qwen3, outils et kwargs", Steps: []step{
			{Shape: "outils", Mode: "qwen3"},
			{Shape: "outils", Mode: "qwen3"}, // en cache : /health et /props seulement
			{Shape: "nue", Mode: "qwen3"},    // autre forme : nouvelle sonde
			{Shape: "outils", Mode: "qwen3", Template: "qwen3 v2"},
			{Shape: "outils", Mode: "qwen3", Template: "qwen3 v2", Build: "b8000-def"},
		}},
		{Name: "garde tout", Steps: []step{{Shape: "nue", Mode: "keep"}, {Shape: "deux", Mode: "keep"}}},
		{Name: "raisonnement jamais rendu", Steps: []step{{Shape: "kwargs", Mode: "drop"}}},
		{Name: "amorce ignorée", Steps: []step{{Shape: "effort", Mode: "noagp"}}},
		{Name: "route absente", Steps: []step{
			{Shape: "nue", Mode: "keep", Override: map[string][]parityTplExchange{"/apply-template": {
				{Status: 404, Reply: "Not Found"}, {Status: 404, Reply: ""}, {Status: 404, Reply: ""}, {Status: 404, Reply: ""},
			}}},
			{Shape: "nue", Mode: "keep"}, // 404 définitif : en cache
		}},
		{Name: "exception du gabarit", Steps: []step{{Shape: "nue", Mode: "keep",
			Override: map[string][]parityTplExchange{"/apply-template": {{Status: 500, Reply: jsonErr}}}}}},
		{Name: "clé exigée, inconnue", Key: "sk-loki-k", Steps: []step{{Shape: "nue", Mode: "keep"}, {Shape: "nue", Mode: "keep"}}},
		{Name: "clé exigée, connue", Key: "sk-loki-k", APIKey: "sk-loki-k", Steps: []step{{Shape: "nue", Mode: "keep"}}},
		{Name: "503 puis rendu", Steps: []step{{Shape: "nue", Mode: "qwen3", Override: map[string][]parityTplExchange{"/apply-template": {
			{Status: 503, Reply: `{"error":{"code":503,"message":"Loading model"}}`}, {Status: 503, Reply: `{"error":{"code":503,"message":"Loading model"}}`},
		}}}}},
		{Name: "503 persistant", Steps: []step{{Shape: "nue", Mode: "qwen3", Override: map[string][]parityTplExchange{"/apply-template": {
			{Status: 503, Reply: "x"}, {Status: 503, Reply: "x"}, {Status: 503, Reply: "x"},
			{Status: 503, Reply: "y"}, {Status: 503, Reply: "y"}, {Status: 503, Reply: "y"},
		}}}, {Shape: "nue", Mode: "qwen3"}}}, // non rangé : resondé
		{Name: "messages aux bornes", Steps: []step{{Shape: "nue", Mode: "keep", Override: map[string][]parityTplExchange{"/apply-template": {
			{Status: 500, Reply: " " + strings.Repeat("à", 160) + " "}, {Status: 500, Reply: strings.Repeat("ù", 161)},
		}}}}},
		{Name: "props de travers", Steps: []step{{Shape: "nue", Mode: "keep", Override: map[string][]parityTplExchange{"/props": {{Status: 200, Reply: `{"build_info":5}`}}}}}},
		{Name: "historique sans la question B", Steps: []step{{Shape: "nue", Mode: "keep", Override: map[string][]parityTplExchange{"/apply-template": {
			{Status: 200, Reply: `{"prompt":"` + tplMarkC1 + `"}`},
			{Status: 200, Reply: `{"prompt":"` + tplMarkT1 + `"}`},
			{Status: 200, Reply: `{"prompt":"` + tplMarkT1 + ` amorce"}`},
			{Status: 200, Reply: `{"prompt":"` + tplMarkT1 + ` ` + tplMarkB + `"}`},
		}}}}},
		{Name: "préfixe sans la question B", Steps: []step{{Shape: "nue", Mode: "keep", Override: map[string][]parityTplExchange{"/apply-template": {
			{Status: 200, Reply: `{"prompt":"` + tplMarkC1 + ` ` + tplMarkB + `"}`},
			{Status: 200, Reply: `{"prompt":"` + tplMarkT1 + `"}`},
			{Status: 200, Reply: `{"prompt":"` + tplMarkT1 + ` amorce"}`},
			{Status: 200, Reply: `{"prompt":"` + tplMarkT1 + ` suite"}`},
		}}}}},
		{Name: "historique en erreur, outil rendu", Steps: []step{{Shape: "nue", Mode: "keep", Override: map[string][]parityTplExchange{"/apply-template": {
			{Status: 500, Reply: "boum"},
			{Status: 200, Reply: `{"prompt":"` + tplMarkT1 + `"}`},
			{Status: 200, Reply: `{"prompt":"` + tplMarkT1 + ` amorce"}`},
			{Status: 200, Reply: `{"prompt":"` + tplMarkT1 + ` ` + tplMarkB + `"}`},
		}}}}},
		{Name: "trop de requêtes", Steps: []step{{Shape: "nue", Mode: "keep", Override: map[string][]parityTplExchange{"/apply-template": {
			{Status: 429, Reply: "  lent  "},
		}}}}},
		{Name: "props illisible", Steps: []step{
			{Shape: "nue", Mode: "keep", Override: map[string][]parityTplExchange{"/props": {{Status: 200, Reply: "<html>"}}}},
			{Shape: "nue", Mode: "keep", Override: map[string][]parityTplExchange{"/props": {{Status: 500, Reply: "panne"}}}},
		}},
		{Name: "moteur pas prêt", Steps: []step{{Shape: "outils", Mode: "keep", Override: map[string][]parityTplExchange{"/health": {{Status: 503, Reply: `{"status":"loading"}`}}}}}},
		{Name: "réponse sans prompt", Steps: []step{{Shape: "nue", Mode: "keep", Override: map[string][]parityTplExchange{"/apply-template": {
			{Status: 200, Reply: `{}`}, {Status: 200, Reply: `{"prompt":null}`}, {Status: 200, Reply: `pas du json`},
		}}}}},
		{Name: "rendu incohérent", Steps: []step{{Shape: "nue", Mode: "keep", Override: map[string][]parityTplExchange{"/apply-template": {
			{Status: 200, Reply: `{"prompt":"x"}`}, {Status: 200, Reply: `{"prompt":"y"}`}, {Status: 200, Reply: `{"prompt":"z"}`}, {Status: 200, Reply: `{"prompt":"yz"}`},
		}}}}},
		{Name: "préfixe incohérent", Steps: []step{{Shape: "nue", Mode: "keep", Override: map[string][]parityTplExchange{"/apply-template": {
			{Status: 200, Reply: `{"prompt":"x"}`}, {Status: 200, Reply: `{"prompt":"y"}`}, {Status: 200, Reply: `{"prompt":"z"}`}, {Status: 200, Reply: `{"prompt":"w"}`},
		}}}}},
		{Name: "preset externe", External: true, Steps: []step{{Shape: "nue", Mode: "keep"}}},
	}

	for i := range scenarios {
		sc := &scenarios[i]
		t.Run(sc.Name, func(t *testing.T) {
			testHome(t)
			freshTplProbe(t)
			eng := &parityTplEngine{fake: &fakeTpl{key: sc.Key}, override: map[string][]parityTplExchange{}}
			srv := httptest.NewServer(eng)
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			if sc.External {
				if err := WriteConfig(parseEnv(externalPresetContent("http://127.0.0.1:1", "gpt-4o-mini", "", "", false))); err != nil {
					t.Fatal(err)
				}
				sc.Config = ReadConfig()
			}
			if err := SetConfigKey("PORT", u.Port()); err != nil {
				t.Fatal(err)
			}
			if sc.APIKey != "" {
				if err := writeAPIKey(sc.APIKey); err != nil {
					t.Fatal(err)
				}
			}
			for j := range sc.Steps {
				st := &sc.Steps[j]
				eng.mu.Lock()
				eng.fake.mode, eng.fake.tpl, eng.fake.build = st.Mode, st.Template, st.Build
				for p, l := range st.Override {
					eng.override[p] = append([]parityTplExchange(nil), l...)
				}
				eng.exchanges = nil
				eng.mu.Unlock()
				r := tplProbeEnsure(context.Background(), shapeOf(st.Shape))
				eng.mu.Lock()
				st.Exchanges = append([]parityTplExchange(nil), eng.exchanges...)
				eng.override = map[string][]parityTplExchange{}
				eng.mu.Unlock()
				st.Result = parityTplResult(r)
				st.Line = tplProbeLine(r)
				if c, ok := tplCapsCurrent(); ok {
					st.Current = parityTplResult(c)
				}
				if c, ok := tplCapsFor(shapeOf(st.Shape)); ok {
					st.For = parityTplResult(c)
				}
			}
			if sc.External {
				delete(sc.Config, "PORT")
			}
		})
	}

	// --- relevé du rendu et comparaison ---
	type printCase struct {
		Shape     string                         `json:"shape"`
		Mode      string                         `json:"mode"`
		Override  map[string][]parityTplExchange `json:"override,omitempty"`
		Exchanges []parityTplExchange            `json:"exchanges"`
		Out       string                         `json:"out"`
		Err       string                         `json:"err"`
	}
	prints := []printCase{
		{Shape: "nue", Mode: "qwen3"},
		{Shape: "outils", Mode: "keep"},
		{Shape: "kwargs", Mode: "drop"},
		{Shape: "nue", Mode: "keep", Override: map[string][]parityTplExchange{"/apply-template": {{Status: 200, Reply: `{"prompt":"a"}`}, {Status: 500, Reply: "boum"}}}},
	}
	func() {
		testHome(t)
		freshTplProbe(t)
		eng := &parityTplEngine{fake: &fakeTpl{}, override: map[string][]parityTplExchange{}}
		srv := httptest.NewServer(eng)
		defer srv.Close()
		u, _ := url.Parse(srv.URL)
		if err := SetConfigKey("PORT", u.Port()); err != nil {
			t.Fatal(err)
		}
		for i := range prints {
			pc := &prints[i]
			eng.mu.Lock()
			eng.fake.mode = pc.Mode
			for p, l := range pc.Override {
				eng.override[p] = append([]parityTplExchange(nil), l...)
			}
			eng.exchanges = nil
			eng.mu.Unlock()
			out, err := tplRenderPrint(context.Background(), tplLocalProber(), shapeOf(pc.Shape))
			pc.Out = out
			if err != nil {
				pc.Err = err.Error()
			}
			eng.mu.Lock()
			pc.Exchanges = append([]parityTplExchange(nil), eng.exchanges...)
			eng.override = map[string][]parityTplExchange{}
			eng.mu.Unlock()
		}
	}()
	type diffCase struct {
		Before string `json:"before"`
		After  string `json:"after"`
		Same   bool   `json:"same"`
		Detail string `json:"detail"`
	}
	long := strings.Repeat("abcdéfgh\n", 30)
	var diffs []diffCase
	for _, c := range [][2]string{
		{prints[0].Out, prints[0].Out},
		{prints[0].Out, prints[1].Out},
		{prints[1].Out, prints[2].Out},
		{"", "x"},
		{"x", ""},
		{"abc", "abd"},
		{long + "X" + long, long + "Y" + long},
		{tplRenderSep + "section ---\n" + "ééé" + long, tplRenderSep + "section ---\n" + "éèé" + long},
		{"sans\x00section", "sans\x00sectioN"},
	} {
		same, d := tplRenderDiff(c[0], c[1])
		diffs = append(diffs, diffCase{Before: c[0], After: c[1], Same: same, Detail: d})
	}

	// --- empreinte des formes ---
	hashes := map[string]string{}
	for name := range shapes {
		hashes[name] = shapeOf(name).hash()
	}

	got, err := parityJSON(map[string]any{
		"shapes":    shapes,
		"scenarios": scenarios,
		"prints":    prints,
		"diffs":     diffs,
		"hashes":    hashes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityTplFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityTplFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityTplFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("la sonde de gabarit ne produit plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteSondeGabarit,\n"+
			"puis cargo test --test tplprobe dit si le port Rust suit.\n--- obtenu ---\n%s", parityTplFile, got)
	}
}
