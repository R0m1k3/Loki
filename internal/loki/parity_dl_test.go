package loki

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// parity_dl_test.go — journal de référence des fonctions pures des
// téléchargements de modèles (lot « modèles » de la migration Rust) : lien
// collé normalisé, famille de tranches, messages d'erreur de la source,
// dossier de destination. Rangé dans
// rust/loki-rs/tests/parity_models/download.json ; le test Rust
// (tests/downloads.rs) doit retrouver chaque sortie. LOKI_HOME réécrit en
// {{HOME}}.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteTelechargements

const parityDLFile = "../../rust/loki-rs/tests/parity_models/download.json"

type parityDLNorm struct {
	In   string   `json:"in"`
	URL  string   `json:"url,omitempty"`
	Name string   `json:"name,omitempty"`
	Err  string   `json:"err,omitempty"`
	URLs []string `json:"urls,omitempty"`
	// Names : noms de la famille de tranches (shardURLSet).
	Names []string `json:"names,omitempty"`
}

type parityDLSource struct {
	Status   int    `json:"status"`
	Code     string `json:"code"`
	URL      string `json:"url"`
	TokenSet bool   `json:"token_set"`
	Err      string `json:"err"`
}

type parityDLDest struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
	Path string `json:"path,omitempty"`
	Err  string `json:"err,omitempty"`
}

type parityDLFixture struct {
	Normalize    []parityDLNorm    `json:"normalize"`
	ContentRange map[string]int64  `json:"content_range"`
	Source       []parityDLSource  `json:"source"`
	HFHost       map[string]bool   `json:"hf_host"`
	HFRepo       map[string]string `json:"hf_repo"`
	Dest         []parityDLDest    `json:"dest"`
}

func TestPariteTelechargements(t *testing.T) {
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

	var fx parityDLFixture
	for _, in := range []string{
		"", "   ", "ftp://h/a.gguf", "pas un lien", "https://exemple.org/dossier/",
		"https://huggingface.co/u/r/blob/main/m.gguf",
		"https://huggingface.co/u/r/blob/main/blob/m.gguf",
		"https://huggingface.co/u/r/resolve/main/m-Q4_K_M.gguf?download=true",
		"  https://huggingface.co/u/r/blob/main/m.bin  ",
		"http://exemple.org/modeles/M.GGUF",
		"https://huggingface.co/u/r/blob/main/Q4/m-Q4-00002-of-00003.gguf",
		"https://huggingface.co/u/r/resolve/main/m-00001-of-00012.gguf",
		"https://exemple.org/m-1-of-2.gguf",
		"https://exemple.org/m-02-of-01.gguf",
	} {
		c := parityDLNorm{In: in}
		u, name, err := normalizeHFURL(in)
		if err != nil {
			c.Err = err.Error()
		} else {
			c.URL, c.Name = u, name
			c.URLs, c.Names = shardURLSet(u, name)
		}
		fx.Normalize = append(fx.Normalize, c)
	}

	fx.ContentRange = map[string]int64{}
	for _, v := range []string{"bytes 0-0/12345", "bytes 0-0/*", "", "bytes 0-0/0", "bytes 0-0/ 99 "} {
		fx.ContentRange[v] = contentRangeTotal(v)
	}

	hf := "https://huggingface.co/auteur/depot/resolve/main/m.gguf"
	for _, c := range []parityDLSource{
		{Status: 401, Code: "GatedRepo", URL: hf},
		{Status: 401, Code: "GatedRepo", URL: hf, TokenSet: true},
		{Status: 403, Code: "RepoNotFound", URL: hf},
		{Status: 401, Code: "GatedRepo", URL: "https://exemple.org/m.gguf"},
		{Status: 403, Code: "", URL: hf, TokenSet: true},
		{Status: 403, Code: "", URL: "https://exemple.org/m.gguf"},
		{Status: 404, Code: "EntryNotFound", URL: hf},
		{Status: 404, Code: "EntryNotFound", URL: "https://exemple.org/"},
		{Status: 404, Code: "RevisionNotFound", URL: hf},
		{Status: 404, Code: "", URL: hf},
		{Status: 416, URL: hf}, {Status: 429, URL: hf}, {Status: 502, URL: hf}, {Status: 418, URL: hf},
	} {
		tok := ""
		if c.TokenSet {
			tok = "hf_x"
		}
		t.Setenv("HF_TOKEN", tok)
		resp := &http.Response{StatusCode: c.Status, Header: http.Header{}}
		if c.Code != "" {
			resp.Header.Set("X-Error-Code", c.Code)
		}
		c.Err = dlSourceError(resp, c.URL).Error()
		fx.Source = append(fx.Source, c)
	}
	t.Setenv("HF_TOKEN", "")

	fx.HFHost = map[string]bool{}
	for _, h := range []string{"huggingface.co", "HUGGINGFACE.CO:443", "cdn-lfs.huggingface.co", "hf.co", "x.hf.co",
		"evilhuggingface.co", "huggingface.co.evil.org", "exemple.org"} {
		fx.HFHost[h] = isHFHost(h)
	}
	fx.HFRepo = map[string]string{}
	for _, u := range []string{hf, "https://huggingface.co/a/b/blob/main/x.gguf", "https://huggingface.co/a/b/tree/main",
		"https://huggingface.co/a/b", "https://exemple.org/a/b/resolve/main/x.gguf", "https://huggingface.co/-a/b/resolve/main/x.gguf"} {
		fx.HFRepo[u] = hfRepoFromURL(u)
	}

	extra := filepath.Join(home, "disque")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveExtraModelDirs([]string{extra}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []parityDLDest{
		{Name: "a.gguf"}, {Name: " ../../x.gguf "}, {Name: "x.bin"}, {Name: ""}, {Name: "/"},
		{Name: "a.gguf", Dir: "/etc"}, {Name: "a.gguf", Dir: "{{HOME}}/disque"}, {Name: "a.gguf", Dir: "{{HOME}}/disque/"},
		{Name: "a.gguf", Dir: "{{HOME}}/models"},
	} {
		p, err := downloadDestPath(c.Name, strings.ReplaceAll(c.Dir, "{{HOME}}", home))
		if err != nil {
			c.Err = norm(err.Error())
		} else {
			c.Path = norm(p)
		}
		fx.Dest = append(fx.Dest, c)
	}

	got, err := parityJSON(fx)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityDLFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityDLFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityDLFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("les téléchargements ne produisent plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteTelechargements,\n"+
			"puis cargo test --test downloads dit si le port Rust suit.\n--- obtenu ---\n%s", parityDLFile, got)
	}
}
