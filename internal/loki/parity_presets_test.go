package loki

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// parity_presets_test.go — journal de référence des presets (lot 15 de la
// migration Rust) : fonctions pures (nom d'affichage, valeurs, environnement,
// quantification, raisonnement, URL des presets externes, copie en placement
// auto…) puis une suite d'appels aux routes (liste et pastilles, ordre,
// éditeur, suppression avec le modèle, presets externes et leur test, aperçus
// de l'éditeur), avec après chacun le dossier presets/ et les clés touchées en
// base. Rangé dans rust/loki-rs/tests/parity_presets/presets.json ;
// tests/presets_api.rs le rejoue par la façade.
//
// La bascule et l'enregistrement du preset EN SERVICE pilotent le moteur :
// ils sont vérifiés à part (TestFacadePresetApply côté Go, essais de bout en
// bout côté Rust).
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestParitePresets

const parityPresetsFile = "../../rust/loki-rs/tests/parity_presets/presets.json"

type parityPresetStep struct {
	Files  map[string]string `json:"files,omitempty"` // relatif à LOKI_HOME ; "" = suppression
	Store  map[string]string `json:"store,omitempty"` // « bucket/clé » ; "" = suppression
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Body   string            `json:"body,omitempty"`
	Status int               `json:"status"`
	Out    any               `json:"out"`
	Disk   map[string]string `json:"disk"`
	State  map[string]any    `json:"state"`
}

func TestParitePresets(t *testing.T) {
	home := testHome(t)
	t.Setenv("LOKI_ENGINE_BIN", "")
	t.Setenv("LOKI_MODEL_DIRS", "")
	for _, k := range append(append(append(append([]string{}, serveArgEnv...), fidelityArgEnv...), cacheArgEnv...), sideSlotArgEnv...) {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	var fx struct {
		DisplayName []map[string]string `json:"display_name"`
		Quote       map[string]string   `json:"quote"`
		FormatEnv   string              `json:"format_env"`
		Names       map[string]string   `json:"names"`
		Quant       map[string]string   `json:"quant"`
		Reasoning   map[string]string   `json:"reasoning"`
		Model       map[string]string   `json:"model"`
		Completions map[string]string   `json:"completions"`
		External    []map[string]any    `json:"external"`
		APIError    map[string]string   `json:"api_error"`
		Join        []map[string]any    `json:"join"`
		SetKey      []map[string]string `json:"set_key"`
		Autoplace   []map[string]any    `json:"autoplace"`
		BenchFP     []map[string]any    `json:"bench_fp"`
		Imposes     map[string]bool     `json:"imposes"`
		Seed        map[string]string   `json:"seed"`
		Steps       []parityPresetStep  `json:"steps"`
	}
	for _, c := range [][2]string{
		{"MODEL=a.gguf\n", "Alpha"}, {"# NAME=Vieux\nMODEL=a\n", "Neuf"}, {"  name = x\nCTX=1\n", "Y"},
		{"#NAME=a\n# name=b\n", "Deux"}, {"MODEL=a\n# NAME=x", "Prix $1 et ${x}"}, {"", "Vide"},
	} {
		fx.DisplayName = append(fx.DisplayName, map[string]string{"content": c[0], "name": c[1], "out": withDisplayName(c[0], c[1])})
	}
	fx.Quote = map[string]string{}
	for _, v := range []string{"", "simple", "a b", "a\tb", `a"b`, "'x'", `"y"`, "'z"} {
		fx.Quote[v] = quoteValue(v)
	}
	fx.FormatEnv = formatEnv(map[string]string{"Z": "1", "A": "deux mots", "M": "", "Q": `q"q`})
	fx.Names = map[string]string{}
	for _, n := range []string{"", "  ", ".", "..", "a/b", `a\b`, "a\x00b", "a\x01b", "Qwen 3 (27B) é", " pad "} {
		if _, err := safePresetPath(n); err != nil {
			fx.Names[n] = err.Error()
		} else {
			fx.Names[n] = "ok"
		}
	}
	fx.Quant, fx.Reasoning, fx.Model = map[string]string{}, map[string]string{}, map[string]string{}
	for _, c := range []string{"MODEL=Qwen3-27B-Q4_K_M.gguf\n", "# QUANT=iq3_xs\nMODEL=x-Q8_0.gguf\n", "QUANT='q5'\n",
		"MODEL=\"/m/Llama-3-UD-Q6_K_XL-00001-of-00002.gguf\"\nREASONING=On\n", "reasoning = 'off'\n#REASONING=on\n",
		"export MODEL=z.gguf\n", "MODEL = C:\\models\\w-BF16.gguf\n", ""} {
		fx.Quant[c], fx.Reasoning[c], fx.Model[c] = detectQuant(c), presetReasoning(c), modelFromPresetContent(c)
	}
	fx.Completions = map[string]string{}
	for _, u := range []string{"", " https://api.groq.com/openai/v1/ ", "http://x/chat/completions", "http://loki:8090", "https://o.ai/api/v1"} {
		fx.Completions[u] = completionsURL(u)
	}
	for _, c := range []struct {
		url, model, key, ctx string
		vision               bool
	}{{"https://a/v1", "gpt", "", "", false}, {" u ", " m ", " k ", " 128000 ", true}} {
		fx.External = append(fx.External, map[string]any{"url": c.url, "model": c.model, "key": c.key, "ctx": c.ctx,
			"vision": c.vision, "out": externalPresetContent(c.url, c.model, c.key, c.ctx, c.vision)})
	}
	fx.APIError = map[string]string{}
	for _, b := range []string{`{"error":{"message":" clé invalide "}}`, `{"error":"x"}`, `pas du json`, `{}`} {
		fx.APIError[b] = extractAPIError([]byte(b))
	}
	for _, a := range [][]string{{"-ngl", "30"}, {"--chat-template-file", "/a b/t.jinja"}, {"x", "l'apostrophe ici"}, {""}} {
		fx.Join = append(fx.Join, map[string]any{"args": a, "out": joinArgs(a)})
	}
	for _, c := range [][3]string{
		{"A=1\nB=2\n", "B", "3"}, {"A=1\nexport B=2\nB=4\n", "B", ""}, {"A=1", "C", "deux mots"}, {"", "X", "1"},
		{"NGL=999\n# NGL=1\n", "NGL", ""}, {"K.E=1\nKXE=2\n", "K.E", "v"},
	} {
		fx.SetKey = append(fx.SetKey, map[string]string{"content": c[0], "key": c[1], "val": c[2], "out": presetSetKey(c[0], c[1], c[2])})
	}
	for _, c := range []string{
		"# NAME=MoE\nMODEL=m.gguf\nCTX=65536\nNGL=999\nEXTRA_ARGS=-ot 'exps=CPU,per_layer_token_embd=CPU,blk\\.0=CUDA0' --n-cpu-moe 0 -ub 512 --mlock --fit off -ctk q8_0\n",
		"MODEL=m.gguf\nEXTRA_ARGS=--n-cpu-moe 30 -ts 1,1\n", "MODEL=m.gguf\nEXTRA_ARGS=--cpu-moe -sm row\n",
		"MODEL=m.gguf\nEXTRA_ARGS=--cpu-moe\nCTX=0\n", "MODEL=m.gguf\nEXTRA_ARGS=--cpu-moe\nSPLIT_MODE=tensor\n",
		"MODEL=m.gguf\nEXTRA_ARGS=-ngl 40\n", "EXTERNAL=1\n", "MODEL=m\nEXTRA_ARGS=--cpu-moe --fit=on -b=1024\nKV_TYPE=q4_0\nUBATCH=1024\n",
	} {
		out, changes, err := autoPlacementPreset(c)
		e := ""
		if err != nil {
			e = err.Error()
		}
		fx.Autoplace = append(fx.Autoplace, map[string]any{"in": c, "out": out, "changes": changes, "err": e})
	}
	for _, c := range []struct{ cfg, cur map[string]string }{
		{map[string]string{"MODEL": "a.gguf"}, map[string]string{}},
		{map[string]string{"MODEL": "a.gguf", "CUDA_VISIBLE_DEVICES": "1"}, map[string]string{"CUDA_VISIBLE_DEVICES": "0"}},
		{map[string]string{"MODEL": "a.gguf"}, map[string]string{"CUDA_VISIBLE_DEVICES": " 0,1 "}},
	} {
		fx.BenchFP = append(fx.BenchFP, map[string]any{"cfg": c.cfg, "cur": c.cur, "out": benchFingerprint(c.cfg, c.cur)})
	}
	fx.Imposes = map[string]bool{}
	for _, p := range []string{"", "/app/llama-server", filepath.Join(home, "engine", "b1", "llama-server"),
		filepath.Join(home, "backends", "llama.cpp-prebuilt", "llama-b2", "llama-server"), "/opt/custom/llama-server"} {
		fx.Imposes[strings.ReplaceAll(p, home, "{{HOME}}")] = presetImposesEngine(p)
	}

	fx.Seed = map[string]string{
		"config/BIN":   "/opt/custom/llama-server",
		"config/HOST":  "127.0.0.1",
		"config/PORT":  "8081",
		"config/MODEL": "live.gguf",
	}
	apply := func(store map[string]string) {
		keys := make([]string, 0, len(store))
		for k := range store {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b, key, _ := strings.Cut(k, "/")
			if err := putStr(b, key, store[k]); err != nil {
				t.Fatal(err)
			}
		}
	}
	apply(fx.Seed)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasPrefix(r.URL.Path, "/ok/"):
			if !strings.Contains(string(b), `"max_tokens":1`) || r.Header.Get("Authorization") != "Bearer k1" {
				w.WriteHeader(400)
				return
			}
			io.WriteString(w, "{}")
		case strings.HasPrefix(r.URL.Path, "/refus/"):
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"message":"clé invalide"}}`)
		case strings.HasPrefix(r.URL.Path, "/brut/"):
			w.WriteHeader(404)
			io.WriteString(w, " page absente ")
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	disk := func() map[string]string {
		out := map[string]string{}
		for _, d := range []string{"presets", "models"} {
			entries, _ := os.ReadDir(filepath.Join(home, d))
			for _, e := range entries {
				b, _ := os.ReadFile(filepath.Join(home, d, e.Name()))
				out[d+"/"+e.Name()] = string(b)
			}
		}
		return out
	}
	state := func() map[string]any {
		st := map[string]any{}
		for k, v := range allKV(bkState) {
			switch {
			case strings.HasPrefix(k, sysPromptKeyPrefix):
				st[k] = v
			case k == presetOrderKey || k == "bench_presets":
				var x any
				_ = json.Unmarshal([]byte(v), &x)
				st[k] = x
			}
		}
		return st
	}
	run := func(st parityPresetStep) parityPresetStep {
		for rel, content := range st.Files {
			p := filepath.Join(home, filepath.FromSlash(rel))
			if content == "" {
				_ = os.Remove(p)
				continue
			}
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		apply(st.Store)
		body := strings.ReplaceAll(st.Body, "{{URL}}", srv.URL)
		w := httptest.NewRecorder()
		r := httptest.NewRequest(st.Method, st.Path, strings.NewReader(body))
		switch r.URL.Path {
		case "/api/presets":
			handlePresets(w, r)
		case "/api/presets/order":
			handlePresetsOrder(w, r)
		case "/api/preset":
			handlePreset(w, r)
		case "/api/preset/save":
			handlePresetSave(w, r)
		case "/api/preset/cacheram":
			handlePresetCacheRAM(w, r)
		case "/api/preset/moe":
			handlePresetMoE(w, r)
		case "/api/preset/autoplace":
			handlePresetAutoPlace(w, r)
		case "/api/preset/delete":
			handlePresetDelete(w, r)
		case "/api/preset/external":
			handlePresetExternal(w, r)
		case "/api/preset/external/save":
			handlePresetExternalSave(w, r)
		case "/api/preset/external/test":
			handlePresetExternalTest(w, r)
		default:
			t.Fatalf("route inattendue %s", st.Path)
		}
		st.Status = w.Code
		if err := json.Unmarshal(w.Body.Bytes(), &st.Out); err != nil {
			t.Fatalf("%s %s : %v (%s)", st.Method, st.Path, err, w.Body.String())
		}
		st.Disk = disk()
		st.State = state()
		return st
	}

	moe := "# NAME=MoE\nMODEL=absent.gguf\nCTX=32768\nEXTRA_ARGS=--n-cpu-moe 20 --load-mode none\n"
	for _, st := range []parityPresetStep{
		{Method: "GET", Path: "/api/presets"},
		{Method: "GET", Path: "/api/preset"},
		{Method: "POST", Path: "/api/preset/save", Body: `{"name":"Qwen 27B","content":"MODEL=Qwen3-27B-Q4_K_M.gguf\nCTX=65536\nREASONING=on\n"}`},
		{Method: "POST", Path: "/api/preset/save", Body: `{"name":"Qwen 27B","content":"MODEL=Qwen3-27B-Q8_0.gguf\nMMPROJ=mm.gguf\n","sysprompt":"  Réponds en français.  "}`},
		{Method: "POST", Path: "/api/preset/save", Body: `{"name":"a/b\\c","content":"MODEL=x.gguf\nCTX=40000\n"}`},
		{Method: "POST", Path: "/api/preset/save", Body: `{"name":"  ","content":"X=1"}`},
		{Method: "POST", Path: "/api/preset/save", Body: `{"id":"../evil","name":"E","content":"X=1"}`},
		{Method: "POST", Path: "/api/preset/save", Body: `{"id":"Qwen 27B (2)","name":"Qwen Q8","content":"# NAME=ancien\nMODEL=Qwen3-27B-Q8_0.gguf\nQUANT=q8\n","sysprompt":""}`},
		{Method: "POST", Path: "/api/preset/save", Body: `pas du json`},
		{Method: "POST", Path: "/api/preset/save", Body: `{"deleteModel":"oui"}`},
		{Method: "POST", Path: "/api/preset/save", Body: `{"name":"Strata","content":"ENGINE=strata\nSTRATA_VISION=0\nCTX=1048576\n"}`},
		{Method: "GET", Path: "/api/presets"},
		{Method: "GET", Path: "/api/preset?id=Qwen%2027B"},
		{Method: "GET", Path: "/api/preset?id=Qwen%2027B%20(2)"},
		{Method: "GET", Path: "/api/preset?id=absent"},
		{Method: "GET", Path: "/api/preset?id=../x"},
		{Method: "POST", Path: "/api/presets/order", Body: `{"ids":["Strata","Qwen 27B (2)"]}`},
		{Method: "GET", Path: "/api/presets"},
		{Method: "POST", Path: "/api/presets/order", Body: `{}`},
		{Method: "GET", Path: "/api/presets/order"},
		{Method: "POST", Path: "/api/presets/order", Body: `{"ids":"x"}`},
		{Method: "POST", Path: "/api/presets/order", Body: `{"ids":["a-b-c"]}`},
		// Mesures : empreinte, repli sur le nom du modèle, autre configuration.
		{Store: map[string]string{"state/bench_presets": `{"Qwen 27B":{"result":{"prompt_per_second":1234.5,"predicted_per_second":40,"kv":"q8_0","kv_quantized":true,"depth":{"cold":{"new":30000},"decode_per_second":31.25}},"model":"Qwen3-27B-Q4_K_M.gguf","at":1700000000},` +
			`"Qwen 27B (2)":{"result":{"prompt_per_second":900,"predicted_per_second":20.5},"model":"autre.gguf","at":1700000001},` +
			`"a-b-c":{"result":{"prompt_per_second":1,"predicted_per_second":2},"model":"x.gguf","at":1,"fingerprint":"mauvaise"}}`},
			Method: "GET", Path: "/api/presets"},
		// Preset actif : la configuration est celle de « a-b-c ».
		{Store: map[string]string{"config/BIN": "", "config/HOST": "", "config/PORT": "", "config/MODEL": "x.gguf", "config/CTX": "40000", "config/MEM_MODE": "always"},
			Method: "GET", Path: "/api/presets"},
		{Method: "POST", Path: "/api/preset/delete", Body: `{"id":"a-b-c"}`},
		{Store: map[string]string{"config/MODEL": "autre.gguf"}, Method: "POST", Path: "/api/preset/delete", Body: `{"id":"a-b-c"}`},
		{Method: "POST", Path: "/api/preset/delete", Body: `{"id":"absent"}`},
		{Method: "POST", Path: "/api/preset/delete", Body: `{"id":"a/b"}`},
		{Files: map[string]string{"models/Qwen3-27B-Q4_K_M.gguf": "poids"}, Method: "POST", Path: "/api/preset/delete", Body: `{"id":"Qwen 27B","deleteModel":true}`},
		{Files: map[string]string{"models/Qwen3-27B-Q8_0.gguf": "poids8", "presets/Copie.env": "# NAME=Copie\nMODEL=Qwen3-27B-Q8_0.gguf\n"},
			Method: "POST", Path: "/api/preset/delete", Body: `{"id":"Qwen 27B (2)","deleteModel":true}`},
		{Method: "POST", Path: "/api/preset/delete", Body: `{"id":"Copie","deleteModel":true}`},
		{Method: "POST", Path: "/api/preset/delete", Body: `{"id":3}`},
		{Method: "GET", Path: "/api/presets"},
		// Presets externes.
		{Method: "GET", Path: "/api/preset/external"},
		{Method: "POST", Path: "/api/preset/external/save", Body: `{"name":" Groq ","url":" https://api.groq.com/openai/v1 ","model":" llama ","key":"k1","ctx":"131072","vision":true}`},
		{Method: "POST", Path: "/api/preset/external/save", Body: `{"name":"X","url":"","model":"m"}`},
		{Method: "POST", Path: "/api/preset/external/save", Body: `{"name":"X","url":"u","model":" "}`},
		{Method: "POST", Path: "/api/preset/external/save", Body: `{"name":"","url":"u","model":"m"}`},
		{Method: "GET", Path: "/api/preset/external?id=Groq"},
		{Method: "POST", Path: "/api/preset/external/save", Body: `{"id":"Groq","name":"Groq 2","url":"https://api.groq.com","model":"llama-3"}`},
		{Method: "GET", Path: "/api/preset/external?id=Groq"},
		{Method: "POST", Path: "/api/preset/external/save", Body: `{"id":"Groq","name":"Groq","url":"https://api.groq.com","model":"llama-3","keyTouched":true,"key":""}`},
		{Method: "GET", Path: "/api/preset/external?id=Groq"},
		{Method: "GET", Path: "/api/preset/external?id=Strata"},
		{Method: "GET", Path: "/api/preset/external?id=absent"},
		{Method: "GET", Path: "/api/presets"},
		{Method: "POST", Path: "/api/preset/external/test", Body: `{"url":"{{URL}}/ok","model":"m","key":"k1"}`},
		{Method: "POST", Path: "/api/preset/external/test", Body: `{"url":"{{URL}}/refus/v1","model":"m"}`},
		{Method: "POST", Path: "/api/preset/external/test", Body: `{"url":"{{URL}}/brut/v1/chat/completions","model":"m"}`},
		{Method: "POST", Path: "/api/preset/external/test", Body: `{"url":"{{URL}}/rien","model":"m"}`},
		{Method: "POST", Path: "/api/preset/external/test", Body: `{"url":"","model":"m"}`},
		{Method: "POST", Path: "/api/preset/external/save", Body: `{"name":"Clé","url":"{{URL}}/ok","model":"m","key":"k1"}`},
		{Method: "POST", Path: "/api/preset/external/test", Body: `{"id":"Clé","url":"{{URL}}/ok","model":"m"}`},
		{Method: "POST", Path: "/api/preset/external/test", Body: `[]`},
		// Placement auto et aperçus de l'éditeur.
		{Method: "GET", Path: "/api/preset/autoplace"},
		{Method: "POST", Path: "/api/preset/autoplace", Body: `{"id":"MoE","content":` + jsonString(moe) + `}`},
		{Method: "POST", Path: "/api/preset/autoplace", Body: `{"name":" Mon MoE ","content":"MODEL=m\nEXTRA_ARGS=-ngl 30\n"}`},
		{Method: "POST", Path: "/api/preset/autoplace", Body: `{"content":"MODEL=m\nEXTRA_ARGS=--cpu-moe\n"}`},
		{Method: "POST", Path: "/api/preset/moe", Body: `{"content":` + jsonString(moe) + `}`},
		{Method: "POST", Path: "/api/preset/moe", Body: `{"content":"EXTERNAL=1\n"}`},
		{Method: "POST", Path: "/api/preset/moe", Body: `{"content":"MODEL=absent.gguf\nEXTRA_ARGS=--cpu-moe -sm row\n"}`},
		{Method: "POST", Path: "/api/preset/moe", Body: `{"content":5}`},
		{Method: "POST", Path: "/api/preset/cacheram", Body: `{"content":"EXTERNAL=1\nSIDE_SLOT=on\n"}`},
		{Method: "POST", Path: "/api/preset/cacheram", Body: `{"content":"EXTRA_ARGS=-cram 2048\n"}`},
		{Method: "POST", Path: "/api/preset/cacheram", Body: `{"content":"CACHE_RAM=4096\nSIDE_SLOT=on\nMODEL=absent.gguf\n"}`},
		{Method: "POST", Path: "/api/preset/cacheram", Body: `{"content":"CACHE_RAM=beaucoup\n"}`},
		{Method: "POST", Path: "/api/preset/cacheram", Body: `{"content":"CACHE_RAM=auto\n"}`},
		{Method: "POST", Path: "/api/preset/cacheram", Body: `{"content":"MODEL=absent.gguf\nSIDE_SLOT=on\nPARALLEL=3\n"}`},
		{Method: "POST", Path: "/api/preset/cacheram", Body: `{}`},
		{Method: "GET", Path: "/api/presets"},
	} {
		fx.Steps = append(fx.Steps, run(st))
	}

	got, err := parityJSON(fx)
	if err != nil {
		t.Fatal(err)
	}
	got = []byte(strings.ReplaceAll(strings.ReplaceAll(string(got), srv.URL, "{{URL}}"), home, "{{HOME}}"))
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityPresetsFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityPresetsFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityPresetsFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("les presets ne produisent plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestParitePresets,\n"+
			"puis cargo test --test presets_api dit si le port Rust suit.\n--- obtenu ---\n%s", parityPresetsFile, got)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// La bascule et l'enregistrement du preset en service passent par
// presetApply (façade) : configuration appliquée, moteur relancé ou arrêté.
func TestFacadePresetApply(t *testing.T) {
	testHome(t)
	var actions []string
	old := presetServiceAction
	done := make(chan struct{}, 4)
	presetServiceAction = func(a string) error { actions = append(actions, a); done <- struct{}{}; return nil }
	t.Cleanup(func() { presetServiceAction = old })
	if err := os.MkdirAll(presetsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(presetsDir(), "local.env")
	ext := filepath.Join(presetsDir(), "ext.env")
	_ = os.WriteFile(local, []byte("MODEL=a.gguf\nCTX=8192\n"), 0o644)
	_ = os.WriteFile(ext, []byte("EXTERNAL=1\nEXTERNAL_URL=u\n"), 0o644)
	_ = SetConfigKey("HOST", "127.0.0.1")
	if err := presetApply(local, "switch"); err != nil {
		t.Fatal(err)
	}
	<-done
	if cfg := ReadConfig(); cfg["CTX"] != "8192" || cfg["HOST"] != "127.0.0.1" {
		t.Fatalf("configuration après bascule : %v", cfg)
	}
	if err := presetApply(local, "save"); err != nil {
		t.Fatal(err)
	}
	<-done
	if err := presetApply(ext, "apply"); err != nil {
		t.Fatal(err)
	}
	if !isExternalConfig(ReadConfig()) || len(actions) != 2 || actions[0] != "restart" || actions[1] != "restart" {
		t.Fatalf("actions %v, config %v", actions, ReadConfig())
	}
	if err := presetApply("/etc/passwd", "switch"); err == nil {
		t.Fatal("preset hors du dossier accepté")
	}
	if err := presetApply(local, "bizarre"); err == nil {
		t.Fatal("mode inconnu accepté")
	}
	w := httptest.NewRecorder()
	handleInternalTuneGuard(w, httptest.NewRequest("GET", "/internal/v1/tune/guard", nil))
	if w.Body.String() != "{\"busy\":false}\n" {
		t.Fatalf("garde : %s", w.Body.String())
	}
}
