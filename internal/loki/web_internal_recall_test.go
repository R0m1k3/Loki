package loki

import (
	"encoding/json"
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

// Lot 46 : la façade lit l'archive (un bloc par sa séquence, ou tous les
// blocs lisibles par séquence croissante) ; le reste est chez elle.
func TestRecallInterneLecture(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	if w := appelOAIInterne(t, "GET", "/internal/v1/recall/blocks", ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("archive vide : %d %s", w.Code, w.Body.String())
	}
	for _, c := range []string{"premier <bloc>", "second bloc"} {
		if _, err := archiveRecallBlock("bash: "+c, "tool", c); err != nil {
			t.Fatal(err)
		}
	}
	w := appelOAIInterne(t, "GET", "/internal/v1/recall/block?seq=1", "")
	if w.Code != 200 {
		t.Fatalf("bloc 1 : %d %s", w.Code, w.Body.String())
	}
	var blk recallBlock
	if err := json.Unmarshal(w.Body.Bytes(), &blk); err != nil || blk.ID != "r1" || blk.Content != "premier <bloc>" || blk.Label != "bash: premier <bloc>" {
		t.Fatalf("bloc 1 : %v %+v", err, blk)
	}
	if w := appelOAIInterne(t, "GET", "/internal/v1/recall/block?seq=9", ""); w.Code != 404 {
		t.Fatalf("absent : %d", w.Code)
	}
	for _, q := range []string{"", "?seq=", "?seq=r1", "?seq=-1"} {
		if w := appelOAIInterne(t, "GET", "/internal/v1/recall/block"+q, ""); w.Code != 400 {
			t.Fatalf("%q : %d", q, w.Code)
		}
	}
	for _, p := range []string{"/internal/v1/recall/block?seq=1", "/internal/v1/recall/blocks"} {
		if w := appelOAIInterne(t, "POST", p, ""); w.Code != 405 {
			t.Fatalf("POST %s : %d", p, w.Code)
		}
	}
	w = appelOAIInterne(t, "GET", "/internal/v1/recall/blocks", "")
	var all []recallBlock
	if err := json.Unmarshal(w.Body.Bytes(), &all); err != nil || len(all) != 2 || all[0].Seq != 1 || all[1].Content != "second bloc" {
		t.Fatalf("tous : %v %s", err, w.Body.String())
	}
}
