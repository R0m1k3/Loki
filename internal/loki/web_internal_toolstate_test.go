package loki

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Le contexte des outils dit à la façade où travaille l'agent, ce que le shell
// n'a pas le droit de toucher, et s'il vise un poste distant.
func TestOutilsInterneContexte(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	w := appelOAIInterne(t, "POST", "/internal/v1/tool/context", `{}`)
	if w.Code != 200 {
		t.Fatalf("contexte : %d %s", w.Code, w.Body.String())
	}
	got := leaseJSON(t, w.Body.String())
	if got["cwd"] != agentCwd() || got["memory_dir"] != memoryDir() || got["target"] != "" {
		t.Fatalf("contexte : %v", got)
	}
	if w := appelOAIInterne(t, "GET", "/internal/v1/tool/context", ``); w.Code != 405 {
		t.Fatalf("GET : %d", w.Code)
	}
}

// Une lecture notée par la façade autorise l'écriture, exactement comme une
// lecture faite ici ; une écriture notée aussi.
func TestOutilsInterneTracker(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	f := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(f, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	check := func() string {
		t.Helper()
		w := appelOAIInterne(t, "POST", "/internal/v1/tool/tracker", `{"op":"check","path":`+jsonQuote(f)+`,"for_write":true}`)
		if w.Code != 200 {
			t.Fatalf("check : %d %s", w.Code, w.Body.String())
		}
		r, _ := leaseJSON(t, w.Body.String())["refusal"].(string)
		return r
	}
	if r := check(); !strings.Contains(r, "pas lu") {
		t.Fatalf("fichier jamais lu accepté : %q", r)
	}
	if w := appelOAIInterne(t, "POST", "/internal/v1/tool/tracker", `{"op":"read","path":`+jsonQuote(f)+`}`); w.Code != 200 {
		t.Fatalf("read : %d", w.Code)
	}
	if r := check(); r != "" {
		t.Fatalf("lecture notée mais refus : %q", r)
	}
	g := filepath.Join(filepath.Dir(f), "b.txt")
	if err := os.WriteFile(g, []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := appelOAIInterne(t, "POST", "/internal/v1/tool/wrote", `{"path":`+jsonQuote(g)+`,"caps":{"Code":true}}`)
	if w.Code != 200 {
		t.Fatalf("wrote : %d %s", w.Code, w.Body.String())
	}
	if trackerCheck(g, false) != "" {
		t.Fatal("écriture notée mais edit refusé")
	}
	for _, body := range []string{"", "{", `{"op":"read"}`, `{"op":"x","path":"/a"}`} {
		if w := appelOAIInterne(t, "POST", "/internal/v1/tool/tracker", body); w.Code != 400 {
			t.Fatalf("%q : %d", body, w.Code)
		}
	}
	if w := appelOAIInterne(t, "POST", "/internal/v1/tool/wrote", `{}`); w.Code != 400 {
		t.Fatalf("wrote sans chemin : %d", w.Code)
	}
}
