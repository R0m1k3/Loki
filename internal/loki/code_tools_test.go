package loki

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// read numérote depuis 1, respecte offset/limit et annonce le reste.
func TestReadOffsetLimit(t *testing.T) {
	ws := withWorkspace(t)
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, "ligne")
	}
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := toolRead(map[string]any{"file": "f.txt", "offset": float64(3), "limit": float64(2)}, true)
	if !strings.Contains(out, "3: ligne") || !strings.Contains(out, "4: ligne") {
		t.Fatalf("lignes 3-4 attendues :\n%s", out)
	}
	if strings.Contains(out, "5: ligne") {
		t.Fatalf("limite ignorée :\n%s", out)
	}
	if !strings.Contains(out, "restantes") {
		t.Fatalf("suite non annoncée :\n%s", out)
	}
}

// read refuse un binaire et le dit, au lieu de déverser du bruit.
func TestReadBinaire(t *testing.T) {
	ws := withWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws, "bin.dat"), []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	if out := toolRead(map[string]any{"file": "bin.dat"}, true); !strings.Contains(out, "[binaire]") {
		t.Fatalf("binaire non détecté : %s", out)
	}
}

// read enregistre la lecture : le tracker doit ensuite autoriser l'écriture.
func TestReadNourritLeTracker(t *testing.T) {
	ws := withWorkspace(t)
	path := filepath.Join(ws, "suivi.txt")
	if err := os.WriteFile(path, []byte("contenu"), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := trackerCheck(path, true); msg == "" {
		t.Fatal("écriture acceptée avant lecture")
	}
	toolRead(map[string]any{"file": "suivi.txt"}, true)
	if msg := trackerCheck(path, true); msg != "" {
		t.Fatalf("écriture refusée après lecture : %s", msg)
	}
	// Fichier modifié APRÈS la lecture : refus (lecture périmée).
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if msg := trackerCheck(path, true); !strings.Contains(msg, "changé") {
		t.Fatalf("lecture périmée non détectée : %q", msg)
	}
}

// grep trouve, borne et ignore les dossiers de dépendances.
func TestGrep(t *testing.T) {
	ws := withWorkspace(t)
	writeFile(t, filepath.Join(ws, "a.go"), "package main\nfunc Cible() {}\n")
	writeFile(t, filepath.Join(ws, "node_modules", "x.js"), "Cible aussi\n")
	out := toolGrep(map[string]any{"pattern": "Cible"}, true)
	if !strings.Contains(out, "a.go:2") {
		t.Fatalf("correspondance manquée :\n%s", out)
	}
	if strings.Contains(out, "node_modules") {
		t.Fatalf("node_modules aurait dû être ignoré :\n%s", out)
	}
	if out := toolGrep(map[string]any{"pattern": "IntrouvableXYZ"}, true); !strings.Contains(out, "aucun résultat") {
		t.Fatalf("absence non signalée : %s", out)
	}
}

// glob : ** traverse les dossiers, tri par modification décroissante.
func TestGlob(t *testing.T) {
	ws := withWorkspace(t)
	writeFile(t, filepath.Join(ws, "src", "a.ts"), "a")
	writeFile(t, filepath.Join(ws, "src", "sub", "b.ts"), "b")
	writeFile(t, filepath.Join(ws, "autre.txt"), "c")
	out := toolGlob(map[string]any{"pattern": "**/*.ts"}, true)
	if !strings.Contains(out, "src/a.ts") || !strings.Contains(out, "src/sub/b.ts") {
		t.Fatalf("fichiers .ts manquants :\n%s", out)
	}
	if strings.Contains(out, "autre.txt") {
		t.Fatalf("autre.txt ne matche pas **/*.ts :\n%s", out)
	}
}

// globToRegexp : cas aux limites du **.
func TestGlobToRegexp(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "src/main.go", false},
		{"**/*.go", "src/deep/main.go", true},
		{"**/*.go", "main.go", true}, // **/ = zéro dossier ou plus
		{"src/**", "src/a/b/c.txt", true},
		{"src/*.ts", "src/a.ts", true},
		{"src/*.ts", "src/sub/a.ts", false},
	}
	for _, c := range cases {
		re, err := globToRegexp(c.pattern)
		if err != nil {
			t.Fatalf("%s : %v", c.pattern, err)
		}
		if got := re.MatchString(c.path); got != c.want {
			t.Errorf("%s sur %s : %v, attendu %v", c.pattern, c.path, got, c.want)
		}
	}
}

// fileEdit préserve les fins de ligne CRLF d'un fichier quand le modèle
// envoie du LF (et n'introduit jamais de mélange).
func TestEditPreserveCRLF(t *testing.T) {
	ws := withWorkspace(t)
	path := filepath.Join(ws, "w.txt")
	if err := os.WriteFile(path, []byte("un\r\ndeux\r\ntrois\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := fileEdit(path, "deux", "DEUX")
	if !strings.HasPrefix(out, "[ok]") {
		t.Fatalf("édition refusée : %s", out)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "un\r\nDEUX\r\ntrois\r\n" {
		t.Fatalf("fins de ligne perdues : %q", b)
	}
}

// fileEdit multi-lignes en LF sur un fichier CRLF : la normalisation couvre
// aussi le old qui contient des sauts de ligne.
func TestEditCRLFMultiligne(t *testing.T) {
	ws := withWorkspace(t)
	path := filepath.Join(ws, "m.txt")
	if err := os.WriteFile(path, []byte("a\r\nb\r\nc\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := fileEdit(path, "a\nb", "a\nB")
	if !strings.HasPrefix(out, "[ok]") {
		t.Fatalf("édition refusée : %s", out)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "a\r\nB\r\nc\r\n" {
		t.Fatalf("résultat inattendu : %q", b)
	}
}
