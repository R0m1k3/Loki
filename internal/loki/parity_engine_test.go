package loki

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parity_engine_test.go — journal de référence du pilotage du moteur (lot 16
// de la migration Rust) : le bilan de VRAM après déchargement (gpuSettle), le
// contrôle d'avant démarrage (preflightEngine) sur une série d'installations,
// la dernière ligne d'erreur du superviseur (lastErrorLine) et les réponses
// des routes qui ne lancent aucun processus (GET refusé, optimisation en
// cours, génération en cours, moteur déjà arrêté, préflight en échec). Rangé
// dans rust/loki-rs/tests/parity_engine/engine.json ; tests/enginectl.rs le
// rejoue par la façade.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteMoteur

const parityEngineFile = "../../rust/loki-rs/tests/parity_engine/engine.json"

type parityPreflight struct {
	Name   string            `json:"name"`
	Config map[string]string `json:"config"`          // « {{HOME}} » = LOKI_HOME
	Files  []string          `json:"files,omitempty"` // relatifs à LOKI_HOME ; « / » final = dossier
	Dirs   []string          `json:"dirs,omitempty"`  // state/model_dirs, relatifs à LOKI_HOME
	Out    string            `json:"out"`
}

type parityEngineStep struct {
	Name       string            `json:"name"`
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	Body       string            `json:"body,omitempty"`
	Tune       bool              `json:"tune,omitempty"`       // une optimisation tient le moteur
	Generating bool              `json:"generating,omitempty"` // une réponse est en cours
	Config     map[string]string `json:"config,omitempty"`
	Status     int               `json:"status"`
	Allow      string            `json:"allow,omitempty"`
	Out        any               `json:"out"`
}

func TestPariteMoteur(t *testing.T) {
	home := testHome(t)
	t.Setenv("LOKI_CONTAINER", "1")
	t.Setenv("LOKI_MODEL_DIRS", "")
	t.Setenv("LOKI_ENGINE_BIN", "")

	var fx struct {
		Settle    []map[string]any   `json:"settle"`
		LastError map[string]string  `json:"last_error"`
		Plain     map[string]string  `json:"plain"`
		Preflight []parityPreflight  `json:"preflight"`
		Steps     []parityEngineStep `json:"steps"`
	}

	// --- gpuSettle ---
	seq := func(vals []int) func() (int, bool) {
		i := 0
		return func() (int, bool) {
			if i >= len(vals) {
				return vals[len(vals)-1], true
			}
			v := vals[i]
			i++
			if v < 0 {
				return 0, false
			}
			return v, true
		}
	}
	for _, c := range []struct {
		before int
		seq    []int
	}{
		{14000, []int{20000, 20000, 12000, 3000, 500, 500}},
		{14000, []int{15000, 8000, 400, 400}},
		{14000, []int{14000, -1, 600, 600}},
		{14000, []int{-1, -1, -1, -1, -1, -1, -1, -1}},
		{14000, []int{14000, 14000, 14000, 14000, 14000, 14000, 14000, 14000}},
		{0, []int{0, 0, 0}},
		{9000, []int{9000, 8999, 8999}},
		{9000, []int{-1, 9000, -1, 9000, 100, -1, 100}},
		{5000, []int{6000, 7000, 7000, 7000, 7000, 7000, 7000, 7000}},
		{5000, []int{4000, 3000, 2000, 1000, 900, 800, 700, 600}},
	} {
		fx.Settle = append(fx.Settle, map[string]any{"before": c.before, "seq": c.seq,
			"out": gpuSettle(c.before, seq(c.seq), 0)})
	}

	// --- lastErrorLine / plainErr ---
	fx.LastError = map[string]string{}
	for _, s := range []string{
		"[loki-rs engine] service loki-engine non démarré\n",
		"ligne 1\n[loki-rs engine]   moteur absent  \n\n",
		"pas de préfixe",
		"",
		"[ok] démarré\n[attention] 2 processus du moteur toujours là\n",
	} {
		fx.LastError[s] = lastErrorLine(s)
	}
	fx.Plain = map[string]string{}
	for _, s := range []string{
		"BIN non défini — installe un moteur : \x1b[1mloki llamacpp install\x1b[0m",
		"\x1b[1;36mcouleur\x1b[0m et \x1b[32mvert\x1b[0m",
		"rien à retirer",
		"\x1b[mvide\x1b[0;1;2m",
	} {
		fx.Plain[s] = ansiRe.ReplaceAllString(s, "")
	}

	// --- preflightEngine ---
	writeFiles := func(files []string) {
		for _, f := range files {
			p := filepath.Join(home, f)
			if strings.HasSuffix(f, "/") {
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	reset := func() {
		for _, d := range []string{"bin", "models", "strata", "ailleurs", "disque"} {
			_ = os.RemoveAll(filepath.Join(home, d))
		}
		cfg := ReadConfig()
		for k := range cfg {
			_ = SetConfigKey(k, "")
		}
		_ = putJSON(bkState, "model_dirs", []string{})
	}
	setCfg := func(cfg map[string]string) {
		for k, v := range cfg {
			if err := SetConfigKey(k, strings.ReplaceAll(v, "{{HOME}}", home)); err != nil {
				t.Fatal(err)
			}
		}
	}
	bin := "bin/llama-server"
	ok := []string{bin, "models/m.gguf"}
	for _, c := range []parityPreflight{
		{Name: "externe", Config: map[string]string{"EXTERNAL": "1"}},
		{Name: "externe sans BIN ni MODEL", Config: map[string]string{"EXTERNAL": " 1 ", "BIN": ""}},
		{Name: "strata sans config", Config: map[string]string{"ENGINE": "strata"}},
		{Name: "strata config absente", Config: map[string]string{"ENGINE": "Strata", "STRATA_CONFIG": "{{HOME}}/strata/m/config.json"}},
		{Name: "strata sans venv", Config: map[string]string{"ENGINE": "strata", "STRATA_CONFIG": "{{HOME}}/strata/m/config.json"},
			Files: []string{"strata/m/config.json"}},
		{Name: "strata prêt", Config: map[string]string{"ENGINE": "moe", "STRATA_CONFIG": "{{HOME}}/strata/m/config.json"},
			Files: []string{"strata/m/config.json", "strata/m/.venv/bin/python"}},
		{Name: "BIN vide", Config: map[string]string{"MODEL": "m.gguf"}},
		{Name: "BIN espaces", Config: map[string]string{"BIN": "   ", "MODEL": "m.gguf"}},
		{Name: "BIN relatif absent", Config: map[string]string{"BIN": "bin/absent", "MODEL": "m.gguf"}},
		{Name: "BIN absolu absent", Config: map[string]string{"BIN": "{{HOME}}/ailleurs/llama-server", "MODEL": "m.gguf"}},
		{Name: "BIN dossier", Config: map[string]string{"BIN": "bin/", "MODEL": "m.gguf"}, Files: []string{"bin/"}},
		{Name: "MODEL vide", Config: map[string]string{"BIN": bin}, Files: []string{bin}},
		{Name: "MODEL absent", Config: map[string]string{"BIN": bin, "MODEL": "absent.gguf"}, Files: []string{bin}},
		{Name: "MODEL relatif", Config: map[string]string{"BIN": "{{HOME}}/" + bin, "MODEL": "m.gguf"}, Files: ok},
		{Name: "MODEL remonte", Config: map[string]string{"BIN": bin, "MODEL": "../m.gguf"}, Files: ok},
		{Name: "MODEL sous-dossier", Config: map[string]string{"BIN": bin, "MODEL": "sub/m.gguf"}, Files: ok},
		{Name: "MODEL absolu hors dossiers", Config: map[string]string{"BIN": bin, "MODEL": "{{HOME}}/ailleurs/x.gguf"},
			Files: []string{bin, "ailleurs/x.gguf"}},
		{Name: "MODEL absolu pas gguf", Config: map[string]string{"BIN": bin, "MODEL": "{{HOME}}/ailleurs/x.bin"},
			Files: []string{bin, "ailleurs/x.bin"}},
		{Name: "MODEL absolu absent", Config: map[string]string{"BIN": bin, "MODEL": "{{HOME}}/ailleurs/y.gguf"},
			Files: []string{bin}},
		{Name: "MODEL dossier déclaré", Config: map[string]string{"BIN": bin, "MODEL": "d.gguf"},
			Files: []string{bin, "disque/d.gguf"}, Dirs: []string{"disque"}},
		{Name: "MODEL chemin dans dossier déclaré", Config: map[string]string{"BIN": bin, "MODEL": "{{HOME}}/disque/d.gguf"},
			Files: []string{bin, "disque/d.gguf"}, Dirs: []string{"disque"}},
		{Name: "tranches manquantes", Config: map[string]string{"BIN": bin, "MODEL": "big-00001-of-00003.gguf"},
			Files: []string{bin, "models/big-00001-of-00003.gguf", "models/big-00003-of-00003.gguf"}},
		{Name: "tranches dossier", Config: map[string]string{"BIN": bin, "MODEL": "big-00001-of-00002.gguf"},
			Files: []string{bin, "models/big-00001-of-00002.gguf", "models/big-00002-of-00002.gguf/"}},
		{Name: "tranches complètes", Config: map[string]string{"BIN": bin, "MODEL": "big-00001-of-00002.gguf"},
			Files: []string{bin, "models/big-00001-of-00002.gguf", "models/big-00002-of-00002.gguf"}},
		{Name: "tranche 2 désignée", Config: map[string]string{"BIN": bin, "MODEL": "big-00002-of-00002.gguf"},
			Files: []string{bin, "models/big-00002-of-00002.gguf"}},
		{Name: "MODEL guillemets", Config: map[string]string{"BIN": bin, "MODEL": "  m.gguf  "}, Files: ok},
	} {
		reset()
		writeFiles(c.Files)
		setCfg(c.Config)
		if len(c.Dirs) > 0 {
			var abs []string
			for _, d := range c.Dirs {
				abs = append(abs, filepath.Join(home, d))
			}
			if err := putJSON(bkState, "model_dirs", abs); err != nil {
				t.Fatal(err)
			}
		}
		c.Out = strings.ReplaceAll(plainErr(preflightEngine()), home, "{{HOME}}")
		fx.Preflight = append(fx.Preflight, c)
	}

	// --- routes sans processus lancé ---
	reset()
	run := func(st parityEngineStep) parityEngineStep {
		reset()
		setCfg(st.Config)
		var lock *tuneLock
		if st.Tune {
			l, err := tuneLockAcquire("web", false)
			if err != nil {
				t.Fatal(err)
			}
			lock = l
		}
		conv.mu.Lock()
		conv.Generating = st.Generating
		conv.mu.Unlock()
		defer func() {
			if lock != nil {
				lock.release()
			}
			conv.mu.Lock()
			conv.Generating = false
			conv.mu.Unlock()
		}()
		h := map[string]http.HandlerFunc{
			"/api/vram/unload": handleVramUnload,
			"/api/vram/reload": handleVramReload,
		}[st.Path]
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(st.Method, st.Path, strings.NewReader(st.Body)))
		st.Status = rec.Code
		st.Allow = rec.Header().Get("Allow")
		st.Out = parityDecode(t, rec.Body.String(), home)
		return st
	}
	for _, st := range []parityEngineStep{
		{Name: "déchargement en GET", Method: "GET", Path: "/api/vram/unload"},
		{Name: "rechargement en GET", Method: "GET", Path: "/api/vram/reload"},
		{Name: "rechargement en PUT", Method: "PUT", Path: "/api/vram/reload"},
		{Name: "génération en cours", Method: "POST", Path: "/api/vram/unload", Body: `{}`, Generating: true},
		{Name: "génération en cours, corps illisible", Method: "POST", Path: "/api/vram/unload", Body: `nimp`, Generating: true},
		{Name: "moteur déjà arrêté", Method: "POST", Path: "/api/vram/unload", Body: `{"force":true}`, Generating: true},
		{Name: "moteur arrêté, sans corps", Method: "POST", Path: "/api/vram/unload"},
		{Name: "optimisation en cours", Method: "POST", Path: "/api/vram/reload", Tune: true},
		{Name: "préflight sans BIN", Method: "POST", Path: "/api/vram/reload", Config: map[string]string{"MODEL": "m.gguf"}},
		{Name: "préflight sans MODEL", Method: "POST", Path: "/api/vram/reload",
			Config: map[string]string{"BIN": "/bin/sh"}},
	} {
		fx.Steps = append(fx.Steps, run(st))
	}

	got, err := parityJSON(fx)
	if err != nil {
		t.Fatal(err)
	}
	got = []byte(strings.ReplaceAll(string(got), home, "{{HOME}}"))
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityEngineFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityEngineFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityEngineFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("le pilotage du moteur ne produit plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteMoteur,\n"+
			"puis cargo test --test enginectl dit si le port Rust suit.\n--- obtenu ---\n%s", parityEngineFile, got)
	}
}

// parityDecode : le corps JSON d'une réponse, tel quel s'il n'en est pas un.
func parityDecode(t *testing.T, body, home string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(strings.ReplaceAll(body, home, "{{HOME}}")), &v); err != nil {
		return body
	}
	return v
}
