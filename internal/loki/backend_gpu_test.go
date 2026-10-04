package loki

import (
	"errors"
	"strings"
	"testing"
)

// La sortie enrichie : liaison et horloge lues, « [N/A] » et « [Not
// Supported] » valent zéro sans faire tomber la carte.
func TestParseGPUQueryLiaison(t *testing.T) {
	out := `0, NVIDIA GeForce RTX 5060 Ti, 16311, 900, 12.0, 00000000:01:00.0, 1, 5, 8, 8, 14001
1, NVIDIA GeForce RTX 3060, 12288, 300, 8.6, 00000000:05:00.0, [N/A], 4, [Not Supported], 16, [N/A]
`
	g := parseGPUQuery(out)
	if len(g) != 2 {
		t.Fatalf("%d carte(s), attendu 2 : %+v", len(g), g)
	}
	if g[0].BusID != "00000000:01:00.0" || g[0].LinkGen != 1 || g[0].LinkGenMax != 5 ||
		g[0].LinkWidth != 8 || g[0].LinkWidthMax != 8 || g[0].MemClockMax != 14001 {
		t.Errorf("carte 0 mal lue : %+v", g[0])
	}
	if g[1].LinkGen != 0 || g[1].LinkGenMax != 4 || g[1].LinkWidth != 0 || g[1].LinkWidthMax != 16 || g[1].MemClockMax != 0 {
		t.Errorf("valeurs [N/A] mal tolérées : %+v", g[1])
	}
	if g[1].MemTotal != "12288" || g[1].Cap != "8.6" {
		t.Errorf("champs historiques perdus : %+v", g[1])
	}
	if s := g[0].linkSummary(); s != "PCIe 5 ×8 (actuellement 1 ×8, réduit au repos)" {
		t.Errorf("résumé de liaison : %q", s)
	}
	if s := g[1].linkSummary(); s != "PCIe 4 ×16" {
		t.Errorf("résumé sans valeur actuelle : %q", s)
	}
}

// La sortie historique (cinq colonnes) se lit comme avant, sans liaison.
func TestParseGPUQueryHistorique(t *testing.T) {
	g := parseGPUQuery("0, NVIDIA GeForce RTX 3080 Ti, 12288, 1442, 8.6\n")
	if len(g) != 1 || g[0].Name != "NVIDIA GeForce RTX 3080 Ti" || g[0].MemTotal != "12288" {
		t.Fatalf("lecture historique cassée : %+v", g)
	}
	if g[0].LinkGenMax != 0 || g[0].BusID != "" || g[0].linkSummary() != "" {
		t.Errorf("liaison inventée : %+v", g[0])
	}
}

// Un pilote qui refuse un champ de liaison : la requête historique prend le
// relais, les cartes restent détectées. Une autre erreur (délai dépassé) n'est
// pas relancée.
func TestQueryGPUsRepli(t *testing.T) {
	var calls []string
	refuse := func(fields string) ([]byte, error) {
		calls = append(calls, fields)
		if strings.Contains(fields, "pcie") {
			return []byte(`Field "pcie.link.gen.max" is not a valid field to query.` + "\n"), errors.New("exit status 2")
		}
		return []byte("0, NVIDIA GeForce RTX 3060, 12288, 300, 8.6\n"), nil
	}
	g, err := queryGPUs(refuse)
	if err != nil || len(g) != 1 || g[0].Name != "NVIDIA GeForce RTX 3060" {
		t.Fatalf("repli raté : %+v, %v", g, err)
	}
	if len(calls) != 2 || calls[1] != gpuBaseFields {
		t.Errorf("requêtes : %q", calls)
	}

	calls = nil
	hang := func(fields string) ([]byte, error) {
		calls = append(calls, fields)
		return nil, errors.New("signal: killed")
	}
	if _, err := queryGPUs(hang); err == nil {
		t.Error("erreur avalée")
	}
	if len(calls) != 1 {
		t.Errorf("un délai dépassé relancé : %d requêtes", len(calls))
	}

	calls = nil
	ok := func(fields string) ([]byte, error) {
		calls = append(calls, fields)
		return []byte("0, NVIDIA GeForce RTX 3060, 12288, 300, 8.6, 00000000:05:00.0, 4, 4, 16, 16, 7501\n"), nil
	}
	if g, err := queryGPUs(ok); err != nil || len(calls) != 1 || g[0].LinkWidthMax != 16 {
		t.Errorf("une seule requête attendue : %d, %+v, %v", len(calls), g, err)
	}
}
