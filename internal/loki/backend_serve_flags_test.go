package loki

import (
	"reflect"
	"testing"
)

func TestHasAnyFlag(t *testing.T) {
	args := []string{"--parallel", "4", "--load-mode=mlock", "-fa"}
	if !hasAnyFlag(args, "--parallel", "-np") {
		t.Fatal("--parallel devrait être détecté")
	}
	// Forme --clé=valeur : le nom seul compte.
	if !hasAnyFlag(args, "--load-mode") {
		t.Fatal("--load-mode=… devrait être détecté")
	}
	if hasAnyFlag(args, "-ngl", "--n-gpu-layers") {
		t.Fatal("-ngl absent, ne devrait pas être détecté")
	}
	// Une VALEUR qui ressemble à un drapeau ne doit pas compter comme tel :
	// ici "-ngl" n'est pas posé, seul --parallel l'est.
	if hasAnyFlag(nil, "--parallel") {
		t.Fatal("liste vide : rien à détecter")
	}
}

func TestNormalizeLoadFlags(t *testing.T) {
	cases := []struct {
		name     string
		in       []string
		supports bool
		want     []string
	}{
		{"moteur ancien : rien ne bouge", []string{"--mlock", "--no-mmap"}, false,
			[]string{"--mlock", "--no-mmap"}},
		{"mlock traduit", []string{"--mlock", "-fa"}, true,
			[]string{"-fa", "--load-mode", "mlock"}},
		{"no-mmap traduit", []string{"--no-mmap"}, true,
			[]string{"--load-mode", "none"}},
		{"mmap traduit", []string{"--mmap"}, true,
			[]string{"--load-mode", "mmap"}},
		{"mlock l'emporte sur no-mmap", []string{"--no-mmap", "--mlock"}, true,
			[]string{"--load-mode", "mlock"}},
		{"--load-mode explicite gagne, vieux drapeaux retirés",
			[]string{"--mlock", "--load-mode", "dio"}, true,
			[]string{"--load-mode", "dio"}},
		{"rien à traduire", []string{"-fa", "--n-cpu-moe", "8"}, true,
			[]string{"-fa", "--n-cpu-moe", "8"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeLoadFlags(c.in, c.supports)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestNGLArgs(t *testing.T) {
	cases := []struct {
		name       string
		ngl        string
		fitsItself bool
		want       []string
		wantNote   bool
	}{
		// Le cas qui motive tout : defaultConfig() sème NGL=999 sur chaque
		// installation neuve. Personne ne l'a choisi, et le moteur récent
		// abandonne son calcul de VRAM dès qu'on lui impose un nombre.
		{"sentinelle 999, moteur récent", "999", true, []string{"-ngl", "auto"}, true},
		{"sentinelle 999, moteur ancien", "999", false, []string{"-ngl", "999"}, false},
		{"nombre choisi, jamais touché", "28", true, []string{"-ngl", "28"}, false},
		{"nombre choisi, moteur ancien", "28", false, []string{"-ngl", "28"}, false},
		{"all reste all", "all", true, []string{"-ngl", "all"}, false},
		{"auto : aucun drapeau", "auto", true, nil, false},
		{"auto insensible à la casse", "AUTO", true, nil, false},
		{"espaces parasites autour de la sentinelle", "  999  ", true, []string{"-ngl", "auto"}, true},
		{"clé absente, moteur récent", "", true, []string{"-ngl", "auto"}, false},
		{"clé absente, moteur ancien", "", false, []string{"-ngl", "999"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, note := nglArgs(c.ngl, c.fitsItself)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("args = %q, want %q", got, c.want)
			}
			if (note != "") != c.wantNote {
				t.Fatalf("note = %q, en voulait-on une ? %v", note, c.wantNote)
			}
		})
	}
}
