package loki

import (
	"path/filepath"
	"strings"
	"testing"
)

// Un chemin relatif du modèle doit atterrir dans le dossier de la DISCUSSION
// ouverte, jamais dans le répertoire courant du processus (Bureau,
// C:\ProgramData\loki\bin…) ni dans un dossier commun à toutes les discussions.
func TestResolveAgentPathRelative(t *testing.T) {
	ws := withWorkspace(t)
	got := resolveAgentPath("meteo.json")
	want := filepath.Join(ws, "meteo.json")
	if got != want {
		t.Fatalf("resolveAgentPath = %q, attendu %q", got, want)
	}
	if !strings.Contains(got, convFilesRoot) {
		t.Fatalf("le fichier n'est pas rangé par discussion : %q", got)
	}
}

// Un chemin absolu demandé explicitement reste intouché.
func TestResolveAgentPathAbsolute(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "rapport.md")
	if got := resolveAgentPath(abs); got != abs {
		t.Fatalf("resolveAgentPath = %q, attendu %q", got, abs)
	}
}

func TestResolveAgentPathSubdir(t *testing.T) {
	ws := withWorkspace(t)
	got := resolveAgentPath("notes/2026/a.txt")
	if !strings.HasPrefix(got, ws) {
		t.Fatalf("%q hors du dossier de la discussion %q", got, ws)
	}
	if strings.Contains(got, "/") && filepath.Separator != '/' {
		t.Fatalf("séparateurs non normalisés : %q", got)
	}
}
