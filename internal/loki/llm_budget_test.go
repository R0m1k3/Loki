package loki

import "testing"

// Le budget ne se déclenche pas avant son palier, et il revient à chaque
// palier suivant : un modèle qui ignore le premier rappel doit en recevoir un
// second, plus ferme.
func TestBudgetNudgePaliers(t *testing.T) {
	const budget = 24
	cases := []struct {
		calls, sent int
		want        bool
	}{
		{0, 0, false},
		{1, 0, false},
		{23, 0, false},
		{24, 0, true},  // 1er palier
		{40, 1, false}, // rappel déjà envoyé, palier suivant pas atteint
		{48, 1, true},  // 2e palier
		{72, 2, true},  // 3e palier
		{95, 3, false},
		{96, 3, true}, // 4e palier : la pression ne s'éteint pas
	}
	for _, c := range cases {
		got := budgetNudge(c.calls, budget, c.sent) != ""
		if got != c.want {
			t.Errorf("budgetNudge(calls=%d, sent=%d) = %v, attendu %v", c.calls, c.sent, got, c.want)
		}
	}
}

// Le message durcit à chaque rappel : trois textes distincts, tous porteurs du
// nombre d'appels (c'est LUI qui informe le modèle, pas le ton).
func TestBudgetNudgeEscalade(t *testing.T) {
	const budget = 10
	seen := map[string]bool{}
	for sent := 0; sent < 3; sent++ {
		m := budgetNudge(budget*(sent+1), budget, sent)
		if m == "" {
			t.Fatalf("rappel n°%d vide", sent+1)
		}
		if seen[m] {
			t.Errorf("rappel n°%d identique à un précédent : %q", sent+1, m)
		}
		seen[m] = true
	}
	// Au-delà, on réutilise le texte le plus ferme plutôt que d'en inventer un
	// nouveau à chaque palier (à nombre d'appels égal, le message est le même).
	if a, b := budgetNudge(50, budget, 3), budgetNudge(50, budget, 4); a != b {
		t.Errorf("les rappels tardifs devraient partager le même texte :\n%q\n%q", a, b)
	}
}

// budget=0 (AGENT_BUDGET=off) : plus aucun rappel, quel que soit le nombre
// d'appels — le mécanisme doit pouvoir être totalement coupé.
func TestBudgetNudgeDesactive(t *testing.T) {
	for _, calls := range []int{0, 24, 500, 10000} {
		if m := budgetNudge(calls, 0, 0); m != "" {
			t.Errorf("budget désactivé mais rappel émis à %d appels : %q", calls, m)
		}
	}
	if m := budgetNudge(100, -5, 0); m != "" {
		t.Errorf("budget négatif devrait être inerte, obtenu %q", m)
	}
}

func TestAgentBudgetConfig(t *testing.T) {
	testHome(t)
	set := func(v string) {
		cfg := ReadConfig()
		cfg["AGENT_BUDGET"] = v
		if err := WriteConfig(cfg); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		val  string
		want int
	}{
		{"", agentBudgetDefault},
		{"40", 40},
		{"0", 0},
		{"off", 0},
		{"NON", 0},
		{"n'importe quoi", agentBudgetDefault}, // valeur illisible : on ne l'invente pas
		{"-3", agentBudgetDefault},
	} {
		set(c.val)
		if got := agentBudget(); got != c.want {
			t.Errorf("AGENT_BUDGET=%q → %d, attendu %d", c.val, got, c.want)
		}
	}
}
