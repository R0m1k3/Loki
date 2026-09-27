package loki

import (
	"net"
	"reflect"
	"testing"
	"time"
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
		{"moteur ancien : anciens drapeaux intacts", []string{"--mlock", "--no-mmap"}, false,
			[]string{"--mlock", "--no-mmap"}},
		// L'ancien --mlock gardait le mmap : « mlock » de llama.cpp le coupe.
		{"mlock seul → mmap+mlock", []string{"--mlock", "-fa"}, true,
			[]string{"-fa", "--load-mode", "mmap+mlock"}},
		{"no-mmap traduit", []string{"--no-mmap"}, true,
			[]string{"--load-mode", "none"}},
		{"mmap traduit", []string{"--mmap"}, true,
			[]string{"--load-mode", "mmap"}},
		{"mlock + no-mmap → mlock", []string{"--no-mmap", "--mlock"}, true,
			[]string{"--load-mode", "mlock"}},
		{"moteur ancien : none retraduit", []string{"-fa", "--load-mode", "none"}, false,
			[]string{"-fa", "--no-mmap"}},
		{"moteur ancien : mlock retraduit", []string{"--load-mode", "mlock"}, false,
			[]string{"--mlock", "--no-mmap"}},
		{"moteur ancien : mmap+mlock retraduit", []string{"-lm", "mmap+mlock"}, false,
			[]string{"--mlock"}},
		{"moteur ancien : dio abandonné", []string{"--load-mode=dio", "-fa"}, false,
			[]string{"-fa"}},
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

func TestArgValueEtPortLibre(t *testing.T) {
	if got := argValue([]string{"--port", "8080", "-c", "1", "--port", "9000"}, "--port"); got != "9000" {
		t.Fatalf("la dernière occurrence doit gagner, got %q", got)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	if waitPortFree("0.0.0.0", port, 600*time.Millisecond) == nil {
		t.Fatal("un port occupé doit être refusé")
	}
	ln.Close()
	if err := waitPortFree("0.0.0.0", port, time.Second); err != nil {
		t.Fatalf("un port libre doit passer : %v", err)
	}
}

func TestSlowKV(t *testing.T) {
	for _, c := range []struct {
		k, v string
		lent bool
	}{{"", "", false}, {"q8_0", "q8_0", false}, {"q4_0", "q4_0", false}, {"q8_0", "q4_0", true}, {"q5_1", "q5_1", true}} {
		if slowKV(c.k, c.v) != c.lent {
			t.Errorf("%s/%s : lent attendu %v", c.k, c.v, c.lent)
		}
	}
}
