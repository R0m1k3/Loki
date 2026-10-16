package loki

import (
	"os"
	"regexp"
	"testing"
)

// parity_retry_test.go — journal de référence des motifs retry_patterns
// (lot 30 de la migration Rust) : chaque regex, compilée par le Go en
// (?mi) comme compileRetryPatterns, et sa première correspondance sur des
// textes choisis (classes et limites de mot ASCII, texte littéral, classes
// aux caractères spéciaux, Unicode). Rangé dans
// rust/loki-rs/tests/parity_retry/patterns.json ; tests/retry.rs le rejoue.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteRetryPatterns

const parityRetryFile = "../../rust/loki-rs/tests/parity_retry/patterns.json"

func TestPariteRetryPatterns(t *testing.T) {
	pats := []string{
		`\w+`, `\W+`, `\d+`, `\D+`, `\s+`, `\S+`, `\bdo_\w+\(`, `\Bfoo`, `\bfoo\b`,
		`[\w-]+`, `[^\s]+`, `[\W]+`, `[\d\s]+`, `[\D]+`, `[^\w\s]+`,
		`\Qa.b\E`, `\Q(x`, `[\Q]-\E]+`, `a\Q\E+`,
		`[a[]+`, `[a&&b]+`, `[!--]+`, `[a--]`, `[~~a]+`, `[]a]+`, `[^]a]+`,
		`[[:alpha:]]+`, `[^[:alpha:]]+`, `[[:word:]]+`, `[[:^digit:]]+`,
		`^oups`, `\Aoups`, `fin$`, `\pL+`, `\p{Greek}+`, `x{2,3}`, `(?s:a.b)`, `a.b`,
		`(?U)a+`, `<function=\w+>`, `\{"name":\s*"(bash|read)"`, `[`, `(`, `\C`, `\z`, `a\z`,
		`(?P<n>ab)`, `\x41\x{42}`, `\101`, `\1`, `(a)\1`, `\0`, `\08`, `[\101-\103]+`, `[\x41-\x43]+`, `\.\*\+`, `é+`, `ÉTÉ`,
	}
	inputs := []string{
		"été do_it(1) éfoo foo bar_9",
		"x٣42 a  \tb\n",
		"axb a.b a(x a]-]b",
		"x[a[ x&ab a!#-b x~a x]a [ab",
		"1abc ab12 Ωμέγα",
		"Bonjour\nOUPS !\nla fin",
		"oups au début, puis la FIN",
		"aaaa xxx a\nb aéb",
		`<function=bash> {"name": "read"}`,
		"AB ABC éé ÉTÉ été",
		"x.*+y",
	}
	type match struct {
		In    int     `json:"in"`
		Match *string `json:"match"`
		At    int     `json:"at"`
	}
	type entry struct {
		Pattern  string  `json:"pattern"`
		Compiles bool    `json:"compiles"`
		Matches  []match `json:"matches"`
	}
	journal := map[string]any{"inputs": inputs}
	var es []entry
	for _, p := range pats {
		e := entry{Pattern: p, Matches: []match{}}
		re, err := regexp.Compile("(?mi)" + p)
		if err == nil {
			e.Compiles = true
			for i, in := range inputs {
				m := match{In: i, At: -1}
				if loc := re.FindStringIndex(in); loc != nil {
					s := in[loc[0]:loc[1]]
					m.Match, m.At = &s, loc[0]
				}
				e.Matches = append(e.Matches, m)
			}
		}
		es = append(es, e)
	}
	journal["patterns"] = es

	got, err := parityJSON(journal)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll("../../rust/loki-rs/tests/parity_retry", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityRetryFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityRetryFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("les motifs ne produisent plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteRetryPatterns,\n"+
			"puis cargo test --test retry dit si le port Rust suit.", parityRetryFile)
	}
}
