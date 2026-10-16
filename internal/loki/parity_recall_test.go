package loki

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parity_recall_test.go — journal de référence des outils recall et
// recall_search (lot 46 de la migration Rust) : une archive variée (accents,
// casse Unicode, contenus longs, beaucoup de blocs), puis les appels de
// l'agent, des ids tolérés aux limites de recherche. Rangé dans
// rust/loki-rs/tests/parity_recall/recall.json avec les blocs tels que
// l'archive les sert ; tests/recall.rs le rejoue contre le port.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteRecall

const parityRecallFile = "../../rust/loki-rs/tests/parity_recall/recall.json"

type parityRecallStep struct {
	Name   string         `json:"name"`
	Tool   string         `json:"tool"`
	Args   map[string]any `json:"args"`
	Result string         `json:"result"`
}

func TestPariteRecall(t *testing.T) {
	testHome(t)
	arch := func(role, tool, content string) {
		t.Helper()
		if _, err := archiveRecallBlock(recallLabel(role, tool, content), role, content); err != nil {
			t.Fatal(err)
		}
	}
	arch("tool", "web_read", "Recette du gratin dauphinois\n"+strings.Repeat("pommes de terre, crème, ail ", 30))
	arch("tool", "web_read", "Horaires SNCF\nle train de 14h12 part quai 3, le TRAIN suivant à 15h")
	arch("tool", "bash", "\n\n   $ ls -la\na.go b.go\tc.go")
	arch("assistant", "", "Voici le résumé de l'ÉTÉ : plage, soleil, Été indien.")
	arch("user", "", "İSTANBUL et ΣΟΦΙΑ en majuscules")
	arch("tool", "", "outil sans nom : train de marchandises")
	arch("tool", "read_file", strings.Repeat("é", 200)+" fin")
	arch("tool", "web_read", "<html><b>balises</b> & entités &amp; \"guillemets\"</html>")
	arch("tool", "bash", "")
	arch("tool", "mem_search", strings.Repeat("très longue première ligne sans fin ", 5)+"\nsuite")
	for i := 0; i < 25; i++ {
		arch("tool", "bash", fmt.Sprintf("lot %d de la migration : sortie numéro %d", i, i))
	}

	var steps []parityRecallStep
	run := func(name, tool string, args map[string]any) {
		var r string
		if tool == "recall" {
			r = toolRecall(args)
		} else {
			r = toolRecallSearch(args)
		}
		steps = append(steps, parityRecallStep{Name: name, Tool: tool, Args: args, Result: r})
	}
	id := func(v any) map[string]any { return map[string]any{"id": v} }
	q := func(v any, kv ...any) map[string]any {
		m := map[string]any{"query": v}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	run("sans id", "recall", map[string]any{})
	run("id non texte", "recall", id(7.0))
	run("id blanc", "recall", id("  "))
	run("r1", "recall", id("r1"))
	run("R2 majuscule", "recall", id("R2"))
	run("espaces", "recall", id(" 3 "))
	run("sans préfixe", "recall", id("4"))
	run("espace après le préfixe", "recall", id("r 5"))
	run("deux préfixes", "recall", id("rR6"))
	run("préfixes à l'envers", "recall", id("Rr6"))
	run("zéros de tête", "recall", id("r007"))
	run("contenu long", "recall", id("r7"))
	run("caractères spéciaux", "recall", id("r8"))
	run("bloc vide", "recall", id("r9"))
	run("mal formé", "recall", id("rr"))
	run("négatif", "recall", id("r-1"))
	run("signe plus", "recall", id("+1"))
	run("zéro", "recall", id("r0"))
	run("absent", "recall", id("r999"))
	run("démesuré", "recall", id("r99999999999999999999"))
	run("guillemets dans l'id", "recall", id(`r"9\`))

	run("sans requête", "recall_search", map[string]any{})
	run("requête non texte", "recall_search", q(3.0))
	run("requête blanche", "recall_search", q("   "))
	run("mots trop courts", "recall_search", q("a b ! ?"))
	run("train horaires", "recall_search", q("train horaires"))
	run("casse", "recall_search", q("TRAIN"))
	run("accents majuscules", "recall_search", q("ÉTÉ"))
	run("i turc", "recall_search", q("İstanbul"))
	run("sigma", "recall_search", q("σοφια"))
	run("souligné coupé", "recall_search", q("web_read"))
	run("extrait borné", "recall_search", q("fin éé"))
	run("première ligne longue", "recall_search", q("suite"))
	run("limite 1", "recall_search", q("train", "limit", 1.0))
	run("limite tronquée", "recall_search", q("train", "limit", 2.7))
	run("limite négative", "recall_search", q("lot", "limit", -3.0))
	run("défaut", "recall_search", q("migration"))
	run("vingt au plus", "recall_search", q("lot migration", "limit", 50.0))
	run("limite texte ignorée", "recall_search", q("lot", "limit", "3"))
	run("rien ne correspond", "recall_search", q("motabsolumentabsent"))
	run("ponctuation", "recall_search", q("«entités»,&amp;"))

	blocks := recallAll()
	for i := range blocks {
		blocks[i].Created = 0
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{"blocks": blocks, "steps": steps}); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityRecallFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityRecallFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityRecallFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("recall ne produit plus le journal rangé dans %s.\n"+
			"LOKI_PARITY_UPDATE=1 pour le régénérer si le changement est voulu.", parityRecallFile)
	}
}
