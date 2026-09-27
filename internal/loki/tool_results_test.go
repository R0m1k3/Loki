package loki

import (
	"strings"
	"testing"
)

// Un résultat coupé est rangé à part, relu en entier, et part avec sa discussion.
func TestToolResultsSaveLoadDelete(t *testing.T) {
	testHome(t)
	_ = putStr(bkChat, ckActive, "c42")
	long := strings.Repeat("ligne de sortie\n", 500)
	ev := fillToolResult(&ToolUsedEvent{Name: "bash", Done: true}, long)
	if ev.ResultID == "" || !strings.HasPrefix(ev.ResultID, "c42.") {
		t.Fatalf("id attendu préfixé par la discussion, got %q", ev.ResultID)
	}
	if len([]rune(ev.Result)) != toolPreviewChars || ev.ResultChars != len([]rune(long)) {
		t.Fatalf("aperçu %d car., taille %d — attendu %d et %d", len([]rune(ev.Result)), ev.ResultChars, toolPreviewChars, len([]rune(long)))
	}
	if got, ok := loadToolResult(ev.ResultID); !ok || got != long {
		t.Fatal("le résultat complet doit être relisible")
	}
	short := fillToolResult(&ToolUsedEvent{}, "ok")
	if short.ResultID != "" || short.Result != "ok" {
		t.Fatal("un résultat court part entier, sans id")
	}
	deleteToolResultsFor("c42")
	if _, ok := loadToolResult(ev.ResultID); ok {
		t.Fatal("les résultats doivent partir avec leur discussion")
	}
}

// Une sortie de commande coupée le DIT, avec sa taille réelle.
func TestTailOutputAnnouncesCut(t *testing.T) {
	if tailOutput("court") != "court" {
		t.Fatal("une sortie courte reste intacte")
	}
	out := tailOutput(strings.Repeat("x", toolMaxOutput+10))
	if !strings.HasPrefix(out, "[sortie tronquée : ") || !strings.Contains(out, "au total") {
		t.Fatalf("mention de coupe absente : %q", out[:60])
	}
}
