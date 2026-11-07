package loki

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
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

// Une sortie de commande trop longue garde son début et sa fin, et dit combien
// de lignes manquent entre les deux (même découpe que la façade Rust).
func TestTailOutputDebutEtFin(t *testing.T) {
	court := strings.Repeat("un\ndeux\n", 10)
	if tailOutput(court) != court {
		t.Fatal("une sortie courte reste intacte")
	}
	if pile := strings.Repeat("y", toolMaxOutput); tailOutput(pile) != pile {
		t.Fatal("une sortie au budget reste intacte")
	}
	var b strings.Builder
	for i := 1; i <= 3000; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}
	out := tailOutput(b.String())
	head, rest, ok := strings.Cut(out, "… ")
	if !ok {
		t.Fatalf("marqueur absent : %q", out[:80])
	}
	marker, tail, _ := strings.Cut(rest, " …\n")
	if !strings.HasPrefix(head, "1\n2\n") || !strings.HasSuffix(head, "\n") || !strings.HasSuffix(tail, "2999\n3000\n") {
		t.Fatalf("début ou fin manquants : %q … %q", head[:10], tail[len(tail)-10:])
	}
	hl := strings.Split(strings.TrimSuffix(head, "\n"), "\n")
	lastHead, _ := strconv.Atoi(hl[len(hl)-1])
	firstKept, _ := strconv.Atoi(tail[:strings.IndexByte(tail, '\n')])
	if want := fmt.Sprintf("%d lignes omises", firstKept-lastHead-1); marker != want {
		t.Fatalf("marqueur %q, attendu %q", marker, want)
	}
	if n := utf8.RuneCountInString(head); n > cmdOutputHead || n <= cmdOutputHead/2 {
		t.Fatalf("début de %d runes", n)
	}
}

// Une ligne géante en multi-octets est coupée sur un bord de rune.
func TestTailOutputSansLignesNiRuneCoupee(t *testing.T) {
	out := tailOutput(strings.Repeat("é€", toolMaxOutput))
	head, tail, ok := strings.Cut(out, "\n… 1 ligne omise …\n")
	if !ok || utf8.RuneCountInString(head) != cmdOutputHead || utf8.RuneCountInString(tail) != toolMaxOutput-cmdOutputHead {
		t.Fatalf("coupe inattendue : %q", out[:40])
	}
	if !utf8.ValidString(out) || !strings.HasPrefix(head, "é") || !strings.HasSuffix(tail, "€") {
		t.Fatal("rune coupée")
	}
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&b, "ligne %d — ç\n", i)
	}
	out = tailOutput(b.String())
	if !strings.Contains(out, " lignes omises …\nligne ") || !strings.HasSuffix(out, "ligne 1999 — ç\n") {
		t.Fatalf("coupe hors bord de ligne : %q", out[:200])
	}
}

// Barres de progression : dernier dessin non vide, \r\n intact.
func TestCollapseCR(t *testing.T) {
	for in, want := range map[string]string{
		"debut\n 10%\r 50%\r100%\nfin\r\nok\r\n": "debut\n100%\nfin\r\nok\r\n",
		"a\rb\r\r\nc\rd\r":                       "b\r\nd",
		"rien\n":                                 "rien\n",
		"étape 1\rétape 2 — fini\n":              "étape 2 — fini\n",
	} {
		if got := string(collapseCR([]byte(in))); got != want {
			t.Errorf("collapseCR(%q) = %q, attendu %q", in, got, want)
		}
	}
}

// Mémoire chiffrée : le résultat complet est chiffré en base et relu intact ;
// verrouillée, rien n'est écrit (le flux portera le résultat entier) et le
// nettoyage des orphelins ne tourne pas (l'index illisible ferait tout effacer).
func TestToolResultsChiffres(t *testing.T) {
	testHome(t)
	clearMemDEK()
	if _, err := EnableMemEncryption("motdepasse-fort"); err != nil {
		t.Fatalf("EnableMemEncryption: %v", err)
	}
	full := strings.Repeat("ligne de sortie\n", 500)
	id := saveToolResult(full)
	if id == "" {
		t.Fatal("résultat non enregistré")
	}
	persistQ.flush() // l'écriture est différée (chat_persist.go)
	if !looksEncrypted(getBytes(bkToolRes, id)) {
		t.Fatal("résultat en clair alors que la mémoire est chiffrée")
	}
	if got, ok := loadToolResult(id); !ok || got != full {
		t.Fatal("résultat chiffré non restitué à l'identique")
	}
	clearMemDEK()
	if saveToolResult("autre") != "" {
		t.Fatal("mémoire verrouillée : rien n'aurait dû être écrit")
	}
	pruneToolResults()
	if getBytes(bkToolRes, id) == nil {
		t.Fatal("le nettoyage a effacé un résultat pendant que la mémoire était verrouillée")
	}
}
