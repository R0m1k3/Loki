package loki

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withWorkspace pointe le dossier de travail sur un dossier temporaire. Le
// chemin est mémoïsé par un sync.Once (chat_workspace.go), donc on force la
// variable directement — c'est le seul moyen d'isoler ces tests.
func withWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// EvalSymlinks : sur macOS /var est un lien vers /private/var, et
	// workspaceRel compare des chemins résolus. Sans ça tout serait « hors du
	// dossier de travail ».
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	// On laisse d'abord agentWorkspace s'initialiser NORMALEMENT : sinon on
	// consomme le sync.Once avec une valeur vide, et tout test ultérieur qui
	// appelle agentWorkspace récupère "" après restauration — une panne à
	// distance, dans un autre fichier, sans rapport visible avec celui-ci.
	prevPath := agentWorkspace()
	workspacePath = dir
	t.Cleanup(func() { workspacePath = prevPath })
	return dir
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func listFiles(t *testing.T, dir string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	handleChatFiles(rec, httptest.NewRequest("GET", "/api/chat/files?dir="+dir, nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("réponse illisible (%d) : %s", rec.Code, rec.Body.String())
	}
	out["_code"] = rec.Code
	return out
}

func TestFilesListSortsAndSizes(t *testing.T) {
	ws := withWorkspace(t)
	writeFile(t, filepath.Join(ws, "rapport.md"), "bonjour")
	writeFile(t, filepath.Join(ws, "captures", "conv1", "a.jpg"), "0123456789")
	writeFile(t, filepath.Join(ws, "captures", "conv1", "b.jpg"), "01234")

	out := listFiles(t, "")
	if out["ok"] != true {
		t.Fatalf("liste refusée : %v", out)
	}
	entries := out["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("attendu 2 entrées à la racine, obtenu %d : %v", len(entries), entries)
	}
	// Les dossiers passent devant, quelle que soit leur date.
	first := entries[0].(map[string]any)
	if first["name"] != "captures" || first["dir"] != true {
		t.Errorf("le dossier doit être en tête, obtenu %v", first)
	}
	// La taille d'un dossier est celle de TOUT son contenu : c'est ce qu'on
	// libère en le supprimant, donc c'est le chiffre qui compte.
	if got := first["size"].(float64); got != 15 {
		t.Errorf("taille du dossier = %v, attendu 15 (10+5)", got)
	}
	if got := first["items"].(float64); got != 2 {
		t.Errorf("items = %v, attendu 2", got)
	}
	// Le total porte sur le dossier de travail entier, pas sur le dossier affiché.
	if got := out["total"].(float64); got != 22 {
		t.Errorf("total = %v, attendu 22 (7+10+5)", got)
	}
	if out["at_root"] != true || out["parent"] != "" {
		t.Errorf("racine mal signalée : %v", out)
	}
}

func TestFilesListSubdirAndImageFlag(t *testing.T) {
	ws := withWorkspace(t)
	writeFile(t, filepath.Join(ws, "captures", "conv1", "shot.jpg"), "x")
	writeFile(t, filepath.Join(ws, "captures", "conv1", "notes.txt"), "y")

	out := listFiles(t, "captures/conv1")
	if out["ok"] != true {
		t.Fatalf("liste refusée : %v", out)
	}
	if out["dir"] != "captures/conv1" || out["parent"] != "captures" {
		t.Errorf("fil d'Ariane cassé : dir=%v parent=%v", out["dir"], out["parent"])
	}
	img := map[string]bool{}
	for _, e := range out["entries"].([]any) {
		m := e.(map[string]any)
		img[m["name"].(string)] = m["image"] == true
		// Le chemin renvoyé doit être utilisable tel quel par /api/chat/file.
		if !strings.HasPrefix(m["path"].(string), "captures/conv1/") {
			t.Errorf("chemin non préfixé : %v", m["path"])
		}
	}
	if !img["shot.jpg"] {
		t.Error("shot.jpg devrait être marqué image")
	}
	if img["notes.txt"] {
		t.Error("notes.txt ne devrait pas être marqué image")
	}
}

// Le bornage est la partie qui compte : ces routes lisent et SUPPRIMENT sur
// disque à partir d'un chemin fourni par le navigateur.
func TestFilesRejectsEscapes(t *testing.T) {
	ws := withWorkspace(t)
	writeFile(t, filepath.Join(ws, "dedans.txt"), "ok")
	secret := filepath.Join(filepath.Dir(ws), "dehors.txt")
	writeFile(t, secret, "secret")

	for _, bad := range []string{"..", "../", "../..", "/etc", "captures/../..", `..\..`} {
		out := listFiles(t, bad)
		if out["ok"] == true {
			t.Errorf("dir=%q accepté alors qu'il sort du dossier de travail : %v", bad, out)
		}
	}

	// Suppression : même bornage, et le fichier voisin doit survivre.
	for _, bad := range []string{"../dehors.txt", "/etc/passwd", "..", ""} {
		rec := httptest.NewRecorder()
		body := strings.NewReader(`{"path":` + jsonQuote(bad) + `}`)
		handleChatFileDelete(rec, httptest.NewRequest("POST", "/api/chat/file/delete", body))
		if rec.Code == http.StatusOK {
			t.Errorf("suppression de %q acceptée : %s", bad, rec.Body.String())
		}
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("le fichier hors workspace a été supprimé : %v", err)
	}
}

// La racine elle-même ne se supprime pas : l'agent perdrait le dossier dans
// lequel il écrit, et « tout effacer » ne doit pas être à un clic de distance.
func TestFilesRootNotDeletable(t *testing.T) {
	ws := withWorkspace(t)
	writeFile(t, filepath.Join(ws, "a.txt"), "a")
	for _, p := range []string{".", "./", "/"} {
		rec := httptest.NewRecorder()
		handleChatFileDelete(rec, httptest.NewRequest("POST", "/api/chat/file/delete",
			strings.NewReader(`{"path":`+jsonQuote(p)+`}`)))
		if rec.Code == http.StatusOK {
			t.Errorf("path=%q a supprimé la racine", p)
		}
	}
	if _, err := os.Stat(ws); err != nil {
		t.Fatalf("dossier de travail détruit : %v", err)
	}
}

func TestFilesDeleteFileAndDir(t *testing.T) {
	ws := withWorkspace(t)
	writeFile(t, filepath.Join(ws, "rapport.md"), "bonjour")
	writeFile(t, filepath.Join(ws, "captures", "conv1", "a.jpg"), "0123456789")

	del := func(p string) map[string]any {
		rec := httptest.NewRecorder()
		handleChatFileDelete(rec, httptest.NewRequest("POST", "/api/chat/file/delete",
			strings.NewReader(`{"path":`+jsonQuote(p)+`}`)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		out["_code"] = rec.Code
		return out
	}

	r := del("rapport.md")
	if r["ok"] != true || r["freed"].(float64) != 7 {
		t.Errorf("suppression de fichier : %v", r)
	}
	if _, err := os.Stat(filepath.Join(ws, "rapport.md")); !os.IsNotExist(err) {
		t.Error("le fichier est toujours là")
	}

	// Un dossier part avec tout son contenu — c'est ce que l'interface annonce.
	r = del("captures")
	if r["ok"] != true || r["freed"].(float64) != 10 {
		t.Errorf("suppression de dossier : %v", r)
	}
	if _, err := os.Stat(filepath.Join(ws, "captures")); !os.IsNotExist(err) {
		t.Error("le dossier est toujours là")
	}
	// Le dossier de travail, lui, doit avoir survécu.
	if _, err := os.Stat(ws); err != nil {
		t.Fatalf("dossier de travail détruit : %v", err)
	}
}

// Un lien symbolique posé dans le dossier de travail ne doit pas servir de
// passage vers le reste du disque — ni en lecture, ni en suppression.
func TestFilesSymlinkEscape(t *testing.T) {
	ws := withWorkspace(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "cible.txt"), "secret")
	if err := os.Symlink(outside, filepath.Join(ws, "evasion")); err != nil {
		t.Skipf("liens symboliques indisponibles : %v", err)
	}
	if out := listFiles(t, "evasion"); out["ok"] == true {
		t.Errorf("le lien symbolique a été suivi : %v", out)
	}
	rec := httptest.NewRecorder()
	handleChatFileDelete(rec, httptest.NewRequest("POST", "/api/chat/file/delete",
		strings.NewReader(`{"path":"evasion/cible.txt"}`)))
	if rec.Code == http.StatusOK {
		t.Errorf("suppression à travers le lien acceptée : %s", rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(outside, "cible.txt")); err != nil {
		t.Fatalf("fichier hors workspace supprimé : %v", err)
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
