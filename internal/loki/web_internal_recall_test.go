package loki

import (
	"strings"
	"testing"
)

// Un bloc archivé par la façade se relit par l'outil recall, comme s'il avait
// été archivé par la compaction du Go.
func TestRecallInterneArchive(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	w := appelOAIInterne(t, "POST", "/internal/v1/recall/archive", `{"label":"bash ls","role":"tool","content":"a.go\nb.go"}`)
	if w.Code != 200 {
		t.Fatalf("archive : %d %s", w.Code, w.Body.String())
	}
	id, _ := leaseJSON(t, w.Body.String())["id"].(string)
	if !strings.HasPrefix(id, "r") {
		t.Fatalf("id : %q", id)
	}
	if got := toolRecall(map[string]any{"id": id}); !strings.Contains(got, "a.go\nb.go") || !strings.Contains(got, "bash ls") {
		t.Fatalf("recall : %q", got)
	}
	for _, body := range []string{"", "{", `{"label":"x"}`} {
		if w := appelOAIInterne(t, "POST", "/internal/v1/recall/archive", body); w.Code != 400 {
			t.Fatalf("%q : %d", body, w.Code)
		}
	}
}
