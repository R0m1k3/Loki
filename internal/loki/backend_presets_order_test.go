package loki

import (
	"os"
	"path/filepath"
	"testing"
)

// Ordre choisi par glisser-déposer : les presets classés d'abord, dans cet
// ordre ; les nouveaux (non classés) ensuite, par nom.
func TestPresetOrder(t *testing.T) {
	testHome(t)
	_ = os.MkdirAll(presetsDir(), 0o755)
	for _, id := range []string{"alpha", "bravo", "charlie", "delta"} {
		if err := os.WriteFile(filepath.Join(presetsDir(), id+".env"), []byte("# NAME="+id+"\nCTX=4096\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ids := func() []string {
		list, err := ListPresets()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, p := range list {
			out = append(out, p.ID)
		}
		return out
	}
	if got := ids(); got[0] != "alpha" || got[3] != "delta" {
		t.Fatalf("sans ordre enregistré, tri par nom attendu : %v", got)
	}
	if err := savePresetOrder([]string{"charlie", "alpha"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"charlie", "alpha", "bravo", "delta"}
	got := ids()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ordre %v, attendu %v", got, want)
		}
	}
}

func TestPresetCtx(t *testing.T) {
	cases := map[string]string{
		"":                 "32K", // défaut de serve : 32768
		"CTX=40000\n":      "40K",
		"CTX=\"131072\"\n": "128K",
		"CTX=1000000\n":    "1M",
		"CTX=1500000\n":    "1.5M",
		"CTX=512\n":        "512",
		"CTX=abc\n":        "",
	}
	for in, want := range cases {
		if got := presetCtx(in); got != want {
			t.Errorf("presetCtx(%q) = %q, attendu %q", in, got, want)
		}
	}
}
