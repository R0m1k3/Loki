package loki

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

// L'archive embarque TOUT l'arbre — fichiers, dossiers, sous-dossiers — avec
// des chemins relatifs en avant-slash.
func TestZipToutLArbre(t *testing.T) {
	ws := withWorkspace(t)
	writeFile(t, filepath.Join(ws, "racine.txt"), "a")
	writeFile(t, filepath.Join(ws, "docs", "note.md"), "b")
	writeFile(t, filepath.Join(ws, "src", "sub", "deep", "main.go"), "c")

	dest := filepath.Join(t.TempDir(), "out.zip")
	n, err := buildZip(ws, dest, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("%d fichiers archivés, attendu 3", n)
	}
	zr, err := zip.OpenReader(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	got := map[string]bool{}
	for _, f := range zr.File {
		got[f.Name] = true
	}
	for _, want := range []string{"racine.txt", "docs/note.md", "src/sub/deep/main.go"} {
		if !got[want] {
			t.Errorf("%s absent de l'archive (contenu : %v)", want, got)
		}
	}
}

// Le scope hors-discussion n'aspire pas le dossier des discussions.
func TestZipLegacyExclutDiscussions(t *testing.T) {
	ws := withWorkspace(t)
	root := agentWorkspace()
	writeFile(t, filepath.Join(root, "orphelin.txt"), "x")
	writeFile(t, filepath.Join(ws, "dans-discussion.txt"), "y") // sous discussions/

	dest := filepath.Join(t.TempDir(), "out.zip")
	n, err := buildZip(root, dest, true)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == "discussions/"+filepath.Base(ws)+"/dans-discussion.txt" || filepath.Dir(f.Name) != "." && f.Name[:12] == "discussions/" {
			t.Errorf("le dossier des discussions a fuité dans l'archive : %s", f.Name)
		}
	}
	if n < 1 {
		t.Fatal("orphelin.txt manquant")
	}
}

// Une discussion vide : 404 propre, pas d'archive fantôme.
func TestZipVide(t *testing.T) {
	ws := withWorkspace(t)
	dest := filepath.Join(t.TempDir(), "out.zip")
	n, err := buildZip(ws, dest, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d fichiers dans une discussion vide", n)
	}
	_ = os.Remove(dest)
}

// slugify : titres réels → noms de fichiers sûrs.
func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Corrige le bug d'auth": "corrige-le-bug-dauth",
		"  Déjà   vu !  ":       "dj-vu",
		"":                      "",
		"UPPER case.and.dots":   "upper-case-and-dots",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, attendu %q", in, got, want)
		}
	}
}
