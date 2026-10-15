package loki

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// parity_models_test.go — journal de référence des autres routes des modèles
// (lot 8 de la migration Rust) : liste du sélecteur, dossiers de modèles,
// suppression, et les fonctions pures du catalogue Hugging Face (classement
// d'un dépôt, verdict mémoire, jeton masqué…). Rangé dans
// rust/loki-rs/tests/parity_models/models.json ; tests/models.rs le rejoue.
// LOKI_HOME réécrit en {{HOME}} ; l'espace libre, propre à la machine, masqué.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteModeles

const parityModelsFile = "../../rust/loki-rs/tests/parity_models/models.json"

type parityHFFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type parityVerdict struct {
	Hardware hardwareInfo `json:"hardware"`
	Weights  int64        `json:"weights"`
	MMProj   int64        `json:"mmproj"`
	Ctx      int          `json:"ctx"`
	Verdict  string       `json:"verdict"`
	Why      string       `json:"why"`
}

type parityModelsCall struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"body,omitempty"`
	Status int    `json:"status"`
	Out    any    `json:"out"`
	// Après l'appel : fichiers restants (suppression) ou dossiers enregistrés.
	Files  []string `json:"files,omitempty"`
	Stored []string `json:"stored,omitempty"`
}

type parityModelsScenario struct {
	// Fichiers sous LOKI_HOME (taille ; -1 = dossier).
	Files   map[string]int64   `json:"files"`
	Extra   []string           `json:"extra"`
	Presets map[string]string  `json:"presets"`
	Config  map[string]string  `json:"config"`
	Calls   []parityModelsCall `json:"calls"`
}

type parityModelsFixture struct {
	Quant     map[string]string `json:"quant"`
	SplitArgs []struct {
		In  string   `json:"in"`
		Out []string `json:"out"`
	} `json:"split_args"`
	Mask  map[string]string `json:"mask"`
	Gated []struct {
		In  json.RawMessage `json:"in"`
		Out bool            `json:"out"`
	} `json:"gated"`
	Classify struct {
		Repo  string         `json:"repo"`
		Files []parityHFFile `json:"files"`
		Out   hfListing      `json:"out"`
	} `json:"classify"`
	Projector []struct {
		In   []hfEntry `json:"in"`
		Name string    `json:"name"`
	} `json:"projector"`
	Verdicts []parityVerdict      `json:"verdicts"`
	Scenario parityModelsScenario `json:"scenario"`
}

// parityMaskFree remplace l'espace libre (propre à la machine) par « … ».
func parityMaskFree(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if k == "free" || k == "free_exact" {
				x[k] = "…"
				continue
			}
			x[k] = parityMaskFree(e)
		}
	case []any:
		for i, e := range x {
			x[i] = parityMaskFree(e)
		}
	}
	return v
}

func parityTreeFiles(t *testing.T, home string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(home, p)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "models/") || strings.HasPrefix(rel, "disque/") {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func TestPariteModeles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("journal tiré sous Unix")
	}
	home := testHome(t)
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("LOKI_HOME", home)
	t.Setenv("LOKI_MODEL_DIRS", "")
	t.Setenv("HF_TOKEN", "")
	norm := func(s string) string { return strings.ReplaceAll(s, home, "{{HOME}}") }
	var fx parityModelsFixture

	fx.Quant = map[string]string{}
	for _, n := range []string{"Qwen3-8B-Q4_K_M.gguf", "m-IQ4_XS-00001-of-00002.gguf", "x.Q8_0.gguf",
		"model-bf16.gguf", "a-4bpw.gguf", "plain.gguf", "m-Q4.gguf", "m-Q4_K_XL-Q5_K_M.gguf", "mmproj-F16.GGUF", "noext"} {
		fx.Quant[n] = quantFromName(n)
	}
	for _, in := range []string{"", "--mmproj /a/b.gguf -ngl 99", `--chat-template-file "/etc/x y.jinja" --x ''`,
		"a\tb\nc", `'un "deux"' trois`, `--mmproj`} {
		fx.SplitArgs = append(fx.SplitArgs, struct {
			In  string   `json:"in"`
			Out []string `json:"out"`
		}{in, splitArgs(in)})
	}
	fx.Mask = map[string]string{}
	for _, tk := range []string{"", "  ", "abc", "hf_12345678", "hf_abcdefghijklmnop", "  hf_padded_token  "} {
		fx.Mask[tk] = maskHFToken(tk)
	}
	for _, raw := range []string{`true`, `false`, `"auto"`, `"manual"`, `"false"`, `""`, `"None"`, `null`, `1`} {
		var v any
		_ = json.Unmarshal([]byte(raw), &v)
		fx.Gated = append(fx.Gated, struct {
			In  json.RawMessage `json:"in"`
			Out bool            `json:"out"`
		}{json.RawMessage(raw), hfGatedFlag(v)})
	}

	fx.Classify.Repo = "unsloth/Qwen3-8B-GGUF"
	fx.Classify.Files = []parityHFFile{
		{"README.md", 1000},
		{"Qwen3-8B-Q8_0.gguf", 8 << 30},
		{"Qwen3-8B-Q4_K_M.gguf", 5 << 30},
		{"mmproj-F16.gguf", 900 << 20},
		{"mmproj-Q8_0.gguf", 600 << 20},
		{"mtp-Qwen3-8B-Q8_0.gguf", 300 << 20},
		{"BF16/Qwen3-8B-BF16-00001-of-00002.gguf", 9 << 30},
		{"BF16/Qwen3-8B-BF16-00002-of-00002.gguf", 7 << 30},
		{"Q2/Qwen3-8B-BF16-00001-of-00002.gguf", 1 << 30},
		{"dossier avec espace/Qwen3 8B Q3.gguf", 4 << 30},
	}
	var files []hfFile
	for _, f := range fx.Classify.Files {
		files = append(files, hfFile{Path: f.Path, Size: f.Size})
	}
	fx.Classify.Out = hfClassify(fx.Classify.Repo, files)

	for _, in := range [][]hfEntry{
		nil,
		{{Name: "mmproj-F16.gguf", Quant: "F16", Size: 900}, {Name: "mmproj-BF16.gguf", Quant: "BF16", Size: 800}},
		{{Name: "mmproj-F16.gguf", Quant: "F16", Size: 900}, {Name: "mmproj-q8_0.gguf", Quant: "q8_0", Size: 950}},
	} {
		p, ok := hfPickProjector(in)
		name := ""
		if ok {
			name = p.Name
		}
		fx.Projector = append(fx.Projector, struct {
			In   []hfEntry `json:"in"`
			Name string    `json:"name"`
		}{in, name})
	}

	gpu := hardwareInfo{OS: "linux", Arch: "amd64", RAMGB: 64, VRAMGB: 24, GPUs: []gpuBrief{{Name: "RTX", VRAMGB: 24}}}
	cpu := hardwareInfo{OS: "linux", Arch: "amd64", RAMGB: 16}
	none := hardwareInfo{OS: "linux", Arch: "amd64"}
	for _, c := range []parityVerdict{
		{Hardware: gpu, Weights: 5 << 30, Ctx: 32768},
		{Hardware: gpu, Weights: 20 << 30, MMProj: 600 << 20, Ctx: 32768},
		{Hardware: gpu, Weights: 22 << 30, Ctx: 32768},
		{Hardware: gpu, Weights: 30 << 30, Ctx: 0},
		{Hardware: cpu, Weights: 8 << 30, Ctx: 131072},
		{Hardware: none, Weights: 1 << 30, Ctx: 4096},
	} {
		c.Verdict, c.Why = fitVerdict(c.Hardware, c.Weights, c.MMProj, c.Ctx)
		fx.Verdicts = append(fx.Verdicts, c)
	}

	// Scénario sur disque : liste, dossiers, suppressions.
	sc := parityModelsScenario{
		Files: map[string]int64{
			"models/a-Q4_K_M.gguf":         100,
			"models/b.gguf":                200,
			"models/actif.gguf":            50,
			"models/vision-mmproj.gguf":    30,
			"models/m-00001-of-00002.gguf": 10,
			"models/m-00002-of-00002.gguf": 20,
			"models/t-00001-of-00003.gguf": 5,
			"models/t-00003-of-00003.gguf": 7,
			"models/notes.txt":             3,
			"models/sous-dossier.gguf":     -1,
			"disque/ext.GGUF":              400,
			"disque/a-Q4_K_M.gguf":         100,
		},
		Extra: []string{"{{HOME}}/disque", "{{HOME}}/absent"},
		Presets: map[string]string{
			"alpha": "NAME=Alpha\nMODEL=a-Q4_K_M.gguf\nMMPROJ=vision-mmproj.gguf\n",
			"beta":  "NAME=Beta\nMODEL={{HOME}}/disque/ext.GGUF\n",
			"gamma": "NAME=Gamma\nMODEL=b.gguf\nEXTRA_ARGS=--mmproj \"vision-mmproj.gguf\" -ngl 99 --mmproj b.gguf\n",
			"delta": "NAME=Delta\nMODEL=m-00001-of-00002.gguf\n",
		},
		Config: map[string]string{"MODEL": "actif.gguf", "MODEL_DRAFT": "{{HOME}}/disque/a-Q4_K_M.gguf"},
	}
	for rel, size := range sc.Files {
		p := filepath.Join(home, rel)
		if size < 0 {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	unhome := func(s string) string { return strings.ReplaceAll(s, "{{HOME}}", home) }
	var extra []string
	for _, d := range sc.Extra {
		extra = append(extra, unhome(d))
	}
	if err := saveExtraModelDirs(extra); err != nil {
		t.Fatal(err)
	}
	for id, content := range sc.Presets {
		if err := os.MkdirAll(presetsDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(presetsDir(), id+".env"), []byte(unhome(content)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := map[string]string{}
	for k, v := range sc.Config {
		cfg[k] = unhome(v)
	}
	if err := WriteConfig(cfg); err != nil {
		t.Fatal(err)
	}

	call := func(method, path, body string) parityModelsCall {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(unhome(body)))
		switch {
		case path == "/api/models":
			handleModels(w, r)
		case path == "/api/models/delete":
			handleModelDelete(w, r)
		case path == "/api/models/dirs":
			handleModelDirs(w, r)
		default:
			t.Fatalf("route inattendue %s", path)
		}
		var out any
		if err := json.Unmarshal([]byte(norm(w.Body.String())), &out); err != nil {
			t.Fatalf("%s %s : %v (%s)", method, path, err, w.Body.String())
		}
		c := parityModelsCall{Method: method, Path: path, Body: body, Status: w.Code, Out: parityMaskFree(out)}
		if path == "/api/models/delete" {
			c.Files = parityTreeFiles(t, home)
		}
		if path == "/api/models/dirs" && method == "POST" {
			var stored []string
			getJSON(bkState, "model_dirs", &stored)
			for _, s := range stored {
				c.Stored = append(c.Stored, norm(s))
			}
		}
		return c
	}
	for _, c := range []struct{ m, p, b string }{
		{"GET", "/api/models", ""},
		{"GET", "/api/models/dirs", ""},
		{"POST", "/api/models/delete", `{"name":"actif.gguf"}`},
		{"POST", "/api/models/delete", `{"name":"a-Q4_K_M.gguf"}`},
		{"POST", "/api/models/delete", `{"name":"{{HOME}}/disque/a-Q4_K_M.gguf"}`},
		{"POST", "/api/models/delete", `{"name":"b.gguf","force":true}`},
		{"POST", "/api/models/delete", `{"name":"m-00001-of-00002.gguf","force":true}`},
		{"POST", "/api/models/delete", `{"name":"t-00001-of-00003.gguf"}`},
		{"POST", "/api/models/delete", `{"name":"inconnu.gguf"}`},
		{"POST", "/api/models/delete", `{"name":"/etc/passwd.gguf"}`},
		{"POST", "/api/models/delete", `{"name":"notes.txt"}`},
		{"POST", "/api/models/delete", `{"name":""}`},
		{"POST", "/api/models/dirs", `{"path":""}`},
		{"POST", "/api/models/dirs", `{"path":"{{HOME}}/nulle-part"}`},
		{"POST", "/api/models/dirs", `{"path":"{{HOME}}/models/sous-dossier.gguf"}`},
		{"POST", "/api/models/dirs", `{"path":"{{HOME}}/disque/","action":"remove"}`},
		{"POST", "/api/models/dirs", `{"path":"{{HOME}}/disque/../disque","action":"add"}`},
		{"POST", "/api/models/dirs", `{"path":"{{HOME}}/models","action":"add"}`},
		{"GET", "/api/models", ""},
		{"GET", "/api/models/dirs", ""},
	} {
		sc.Calls = append(sc.Calls, call(c.m, c.p, c.b))
	}
	fx.Scenario = sc

	got, err := parityJSON(fx)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityModelsFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityModelsFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityModelsFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("les routes des modèles ne produisent plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteModeles,\n"+
			"puis cargo test --test models dit si le port Rust suit.\n--- obtenu ---\n%s", parityModelsFile, got)
	}
}
