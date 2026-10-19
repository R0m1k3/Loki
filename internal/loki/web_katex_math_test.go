package loki

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Détection des formules (ui/src/js/00-math.js), exécutée sous node : le dépôt
// n'a pas de banc de tests JS, et MATH_RE/mathExtract sont purs. Ignoré si node
// n'est pas installé (il l'est sur les runners de la CI).

type formule struct {
	Tex     string `json:"tex"`
	Display bool   `json:"display"`
}

// harnaisMath : charge 00-math.js et marked.min.js, puis pour chaque entrée
// (JSON sur stdin) rend {text, list} de mathExtract et, pour le bout en bout, le
// HTML de marked après mathRestore (chaque formule rendue « [tex] »).
const harnaisMath = `
const fs = require('fs');
const [mathPath, markedPath] = process.argv.slice(2);
const m = new Function(fs.readFileSync(mathPath, 'utf8') + '; return {mathExtract, mathRestore};')();
const {marked} = require(markedPath);
marked.setOptions({ gfm: true, breaks: false });
const cases = JSON.parse(fs.readFileSync(0, 'utf8'));
console.log(JSON.stringify(cases.map(s => {
  const r = m.mathExtract(s);
  const html = m.mathRestore(marked.parse(r.text), r.list, f => '[' + (f.display ? 'D:' : '') + f.tex + ']');
  return {text: r.text, list: r.list, html};
})));
`

type résultatMath struct {
	Text string    `json:"text"`
	List []formule `json:"list"`
	HTML string    `json:"html"`
}

func lanceMath(t *testing.T, cas []string) []résultatMath {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node absent : détection des formules non testée")
	}
	dir := t.TempDir()
	h := filepath.Join(dir, "harnais.js")
	if err := os.WriteFile(h, []byte(harnaisMath), 0o644); err != nil {
		t.Fatal(err)
	}
	math, _ := filepath.Abs("ui/src/js/00-math.js")
	marked, _ := filepath.Abs("ui/marked.min.js")
	in, _ := json.Marshal(cas)
	cmd := exec.Command(node, h, math, marked)
	cmd.Stdin = strings.NewReader(string(in))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node : %v\n%s", err, out)
	}
	var res []résultatMath
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("sortie illisible : %v\n%s", err, out)
	}
	if len(res) != len(cas) {
		t.Fatalf("%d résultats pour %d cas", len(res), len(cas))
	}
	return res
}

func TestMathDetection(t *testing.T) {
	in := func(tex string) formule { return formule{Tex: tex} }
	bloc := func(tex string) formule { return formule{Tex: tex, Display: true} }
	cas := []struct {
		src  string
		want []formule
	}{
		// Montants : jamais des formules.
		{"Ça coûte 5 $ et 10 $.", nil},
		{"Entre $5 et $10 par mois.", nil},
		{"Prix : 5$ ou 6$, voire 7 $.", nil},
		{`Échappés : \$5 et \$6.`, nil},
		{"Collé à un mot : x$y$z.", nil},
		{"Espace finale : $x $.", nil},
		{"De 5 $, à 10$.", nil},
		{"Montant : 5 $, code `$x_1$`.", nil},
		// Code : jamais touché.
		{"En ligne `$x$` et `$$y$$`.", nil},
		{"```\n$x$ et $$y$$\n```", nil},
		{"~~~tex\n\\[a\\]\n~~~", nil},
		{"```\nbloc non fermé $x$", nil},
		// Formules.
		{"Soit $x^2 + y^2 = z^2$ ici.", []formule{in("x^2 + y^2 = z^2")}},
		{"Deux : $a$, $b$.", []formule{in("a"), in("b")}},
		{"$$\n\\int_0^1 x\\,dx\n$$", []formule{bloc(`\int_0^1 x\,dx`)}},
		{`En ligne \(a+b\) et en bloc \[c\].`, []formule{in("a+b"), bloc("c")}},
		{"Code `$x$` puis $y$.", []formule{in("y")}},
		{`Échappement dedans : $a\$b$.`, []formule{in(`a\$b`)}},
	}
	srcs := make([]string, len(cas))
	for i, c := range cas {
		srcs[i] = c.src
	}
	res := lanceMath(t, srcs)
	for i, c := range cas {
		got := res[i].List
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q : %+v, attendu %+v", c.src, got, c.want)
		}
		if c.want == nil && res[i].Text != c.src {
			t.Errorf("%q : texte modifié en %q", c.src, res[i].Text)
		}
	}
}

// Bout en bout avec marked : \\, _ et * d'une formule arrivent intacts au rendu,
// le markdown autour reste du markdown, et aucun repère ne survit.
func TestMathAvecMarked(t *testing.T) {
	res := lanceMath(t, []string{
		"*Gras* : $a_1 * b_2$ et $$\\begin{matrix}a & b \\\\ c & d\\end{matrix}$$",
		"Montant : 5 $, code `$x_1$`.",
	})
	for _, want := range []string{"<em>Gras</em>", "[a_1 * b_2]", `[D:\begin{matrix}a & b \\ c & d\end{matrix}]`} {
		if !strings.Contains(res[0].HTML, want) {
			t.Errorf("HTML sans %q :\n%s", want, res[0].HTML)
		}
	}
	for _, r := range res {
		if strings.ContainsAny(r.HTML, "\uE000\uE001") {
			t.Errorf("repère resté dans le HTML : %q", r.HTML)
		}
	}
	if !strings.Contains(res[1].HTML, "5 $") || !strings.Contains(res[1].HTML, "<code>$x_1$</code>") {
		t.Errorf("montant ou code abîmé : %s", res[1].HTML)
	}
}
