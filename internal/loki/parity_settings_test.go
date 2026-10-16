package loki

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parity_settings_test.go — journal de référence des réglages partagés entre
// appareils (lot 10 de la migration Rust) : préférences d'affichage, prompt
// système (par preset, repli global), intensité du raisonnement, adresse
// publique. Rangé dans rust/loki-rs/tests/parity_settings/settings.json ;
// tests/settings.rs le rejoue par la façade.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteReglages

const paritySettingsFile = "../../rust/loki-rs/tests/parity_settings/settings.json"

type paritySettingsStep struct {
	// Avant l'appel : configuration remplacée, preset écrit.
	Config map[string]string `json:"config,omitempty"`
	Preset map[string]string `json:"preset,omitempty"`
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Body   string            `json:"body,omitempty"`
	Host   string            `json:"host,omitempty"`
	Status int               `json:"status"`
	Out    any               `json:"out"`
	// Après l'appel : le contenu des clés touchées en base.
	Prefs map[string]string `json:"prefs,omitempty"`
	State map[string]string `json:"state,omitempty"`
}

type paritySettingsFixture struct {
	PublicURL []struct {
		In  string `json:"in"`
		Out string `json:"out,omitempty"`
		Err string `json:"err,omitempty"`
	} `json:"public_url"`
	EffortValue map[string]string    `json:"effort_value"`
	Steps       []paritySettingsStep `json:"steps"`
}

func TestPariteReglages(t *testing.T) {
	testHome(t)
	var fx paritySettingsFixture
	for _, in := range []string{"", "  ", "exemple.fr", "https://ia.exemple.fr/", "http://10.0.0.5:8090/v1/",
		"HTTPS://Exemple.FR:443", "ftp://x.fr", "https://", "https://moi:secret@x.fr", "https://x.fr/?a=1",
		"https://x.fr/#ancre", "https://x.fr/api", "https://x.fr:0", "https://x.fr:99999", "https://x.fr:abc",
		"https://[::1]:8090", "https://::1", "https://:8080", "exemple.fr:8090/v1"} {
		c := struct {
			In  string `json:"in"`
			Out string `json:"out,omitempty"`
			Err string `json:"err,omitempty"`
		}{In: in}
		out, err := normalizePublicURL(in)
		if err != nil {
			c.Err = err.Error()
		} else {
			c.Out = out
		}
		fx.PublicURL = append(fx.PublicURL, c)
	}
	fx.EffortValue = map[string]string{}
	for _, v := range []string{"", "auto", "Default", "off", "NONE", "0", "low", " High ", "medium", "xhigh", "bizarre"} {
		fx.EffortValue[v] = reasoningEffortValue(v)
	}

	keys := []string{"sysprompt", "sysprompt:alpha", "sysprompt:beta", "public_url"}
	run := func(st paritySettingsStep) paritySettingsStep {
		if st.Config != nil {
			if err := WriteConfig(st.Config); err != nil {
				t.Fatal(err)
			}
		}
		for id, content := range st.Preset {
			if err := os.MkdirAll(presetsDir(), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(presetsDir(), id+".env"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(st.Method, st.Path, strings.NewReader(st.Body))
		if st.Host != "" {
			r.Host = st.Host
		}
		switch st.Path {
		case "/api/prefs":
			handleWebPrefs(w, r)
		case "/api/sysprompt":
			handleSysPrompt(w, r)
		case "/api/reasoning-effort":
			handleReasoningEffort(w, r)
		case "/api/oai/public":
			handlePublicAddress(w, r)
		default:
			t.Fatalf("route inattendue %s", st.Path)
		}
		st.Status = w.Code
		if err := json.Unmarshal(w.Body.Bytes(), &st.Out); err != nil {
			t.Fatalf("%s %s : %v (%s)", st.Method, st.Path, err, w.Body.String())
		}
		st.Prefs = loadWebPrefs()
		st.State = map[string]string{}
		for _, k := range keys {
			if v := getStr(bkState, k); v != "" {
				st.State[k] = v
			}
		}
		if v := ReadConfig()["REASONING_EFFORT"]; v != "" {
			st.State["config:REASONING_EFFORT"] = v
		}
		return st
	}
	for _, st := range []paritySettingsStep{
		{Method: "GET", Path: "/api/prefs"},
		{Method: "POST", Path: "/api/prefs", Body: `{"theme":" DARK ","hide_tools":"1","hide_stats":"2","inconnue":"x","user_name":"  Ma\u0007rie  ","avatar_user":"🦊","avatar_ai":"💩"}`},
		{Method: "POST", Path: "/api/prefs", Body: `{"user_name":"Une très très longue signature éèà","fold_tools":1,"hide_side":"0"}`},
		{Method: "POST", Path: "/api/prefs", Body: `pas du json`},
		{Method: "POST", Path: "/api/prefs", Body: `{"user_name":"   ","avatar_user":""}`},
		{Method: "GET", Path: "/api/prefs"},
		// Prompt système : sans preset actif, le global.
		{Method: "GET", Path: "/api/sysprompt"},
		{Method: "POST", Path: "/api/sysprompt", Body: `{"text":"  Réponds en français.  "}`},
		{Method: "POST", Path: "/api/sysprompt", Body: `{"text":5}`},
		{Method: "PUT", Path: "/api/sysprompt"},
		// Preset alpha actif : son propre prompt, repli sur le global tant qu'il
		// n'en a pas.
		{Config: map[string]string{"MODEL": "a.gguf", "CTX": "8192"},
			Preset: map[string]string{"alpha": "# Alpha\nMODEL=a.gguf\nCTX=8192\n", "beta": "# Beta\nMODEL=b.gguf\n"},
			Method: "GET", Path: "/api/sysprompt"},
		{Method: "POST", Path: "/api/sysprompt", Body: `{"text":"Prompt d'Alpha"}`},
		{Method: "GET", Path: "/api/sysprompt"},
		{Config: map[string]string{"MODEL": "b.gguf"}, Method: "GET", Path: "/api/sysprompt"},
		{Method: "POST", Path: "/api/sysprompt", Body: `{"text":""}`},
		{Config: map[string]string{"MODEL": "a.gguf", "CTX": "8192"}, Method: "POST", Path: "/api/sysprompt", Body: `{"text":"   "}`},
		{Method: "GET", Path: "/api/sysprompt"},
		// Intensité du raisonnement.
		{Config: map[string]string{"MODEL": "a.gguf"}, Method: "GET", Path: "/api/reasoning-effort"},
		{Method: "POST", Path: "/api/reasoning-effort", Body: `{"value":" HIGH "}`},
		{Method: "POST", Path: "/api/reasoning-effort", Body: `{"value":"auto"}`},
		{Config: map[string]string{"MODEL": "a.gguf", "REASONING": "on", "REASONING_EFFORT": "off"}, Method: "GET", Path: "/api/reasoning-effort"},
		{Method: "POST", Path: "/api/reasoning-effort", Body: `{"value":""}`},
		{Config: map[string]string{"REASONING": "off", "REASONING_EFFORT": "bizarre"}, Method: "GET", Path: "/api/reasoning-effort"},
		// Adresse publique.
		{Method: "GET", Path: "/api/oai/public"},
		{Method: "POST", Path: "/api/oai/public", Body: `{"url":"ftp://x"}`},
		{Method: "POST", Path: "/api/oai/public", Body: `pas du json`},
		{Method: "POST", Path: "/api/oai/public", Body: `{"url":" ia.exemple.fr/v1 "}`},
		{Method: "GET", Path: "/api/oai/public"},
		{Method: "POST", Path: "/api/oai/public", Body: `{"url":""}`},
	} {
		fx.Steps = append(fx.Steps, run(st))
	}

	got, err := parityJSON(fx)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(paritySettingsFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paritySettingsFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(paritySettingsFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("les réglages ne produisent plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteReglages,\n"+
			"puis cargo test --test settings dit si le port Rust suit.\n--- obtenu ---\n%s", paritySettingsFile, got)
	}
}
