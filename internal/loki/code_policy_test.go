package loki

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// La politique doit bloquer les commandes catastrophiques…
func TestCommandesDangereusesBloquees(t *testing.T) {
	for _, cmd := range []string{
		"rm -rf /",
		"rm -fr / --no-preserve-root",
		"sudo rm -rf /*",
		"mkfs.ext4 /dev/sda1",
		"dd if=/dev/zero of=/dev/sda",
		"echo pwned > /dev/sda",
		"shutdown -h now",
		"reboot",
		":(){ :|:& };:",
		"format c:",
		"rd /s /q C:\\",
	} {
		if dangerousCommand(cmd) == "" {
			t.Errorf("aurait dû être bloquée : %q", cmd)
		}
	}
}

// … et laisser passer le travail normal, y compris les rm ciblés.
func TestCommandesNormalesAutorisees(t *testing.T) {
	for _, cmd := range []string{
		"ls -la",
		"go test ./...",
		"rm -rf node_modules",
		"rm -rf ./dist",
		"git rm --cached fichier.txt",
		"grep -r 'reboot the router' docs/",
		"npm run build",
		"dd if=disque.img of=copie.img",
		"echo bonjour > notes.txt",
	} {
		if r := dangerousCommand(cmd); r != "" {
			t.Errorf("bloquée à tort (%s) : %q", r, cmd)
		}
	}
}

// En mode code, les chemins sont bornés au dossier de la discussion :
// dedans OK, dehors refusé, y compris par remontée de ../.
func TestCodePathBorne(t *testing.T) {
	ws := withWorkspace(t)
	inside := filepath.Join(ws, "src", "main.go")
	if !codePathAllowed(inside) {
		t.Error("chemin intérieur refusé")
	}
	if !codePathAllowed(filepath.Join(ws, "nouveau.txt")) {
		t.Error("fichier à créer (inexistant) refusé")
	}
	outside := filepath.Join(filepath.Dir(ws), "ailleurs.txt")
	if codePathAllowed(outside) {
		t.Error("chemin extérieur accepté")
	}
	if codePathAllowed(filepath.Join(ws, "..", "..", "etc", "passwd")) {
		t.Error("remontée ../ acceptée")
	}
}

// codeWriteGuard : inactif en mode chat, actif en mode code (borne + tracker).
func TestCodeWriteGuard(t *testing.T) {
	ws := withWorkspace(t)
	chat := Caps{Agent: true}
	code := Caps{Agent: true, Code: true}

	// Mode chat : aucune garde, même hors workspace.
	if msg := codeWriteGuard(chat, "/tmp/hors-discussion.txt", true); msg != "" {
		t.Errorf("mode chat gardé à tort : %s", msg)
	}
	// Mode code : hors workspace refusé.
	if msg := codeWriteGuard(code, filepath.Join(filepath.Dir(ws), "x.txt"), true); msg == "" {
		t.Error("écriture hors workspace acceptée en mode code")
	}
	// Fichier existant jamais lu : write refusé (tracker).
	target := filepath.Join(ws, "present.txt")
	if err := os.WriteFile(target, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := codeWriteGuard(code, "present.txt", true); !strings.Contains(msg, "pas lu") {
		t.Errorf("écrasement sans lecture accepté : %q", msg)
	}
	// Après lecture : autorisé.
	trackerNoteRead(target)
	if msg := codeWriteGuard(code, "present.txt", true); msg != "" {
		t.Errorf("refusé après lecture : %s", msg)
	}
	// Création d'un fichier neuf : pas de lecture préalable exigée.
	if msg := codeWriteGuard(code, "neuf.txt", true); msg != "" {
		t.Errorf("création refusée : %s", msg)
	}
}
