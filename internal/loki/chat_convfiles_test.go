package loki

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Le cœur de la demande : deux discussions ne partagent pas leurs fichiers.
// Avant, uploads/ était commun — on retrouvait les mêmes pièces jointes partout.
func TestFichiersIsolesParDiscussion(t *testing.T) {
	withWorkspace(t)

	a := convEnsureActive()
	dirA, err := uploadsDir()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dirA, "note-a.txt"), "aaa")

	b := convNew() // b devient active
	if a == b {
		t.Fatal("convNew n'a pas créé de nouvelle discussion")
	}
	dirB, err := uploadsDir()
	if err != nil {
		t.Fatal(err)
	}
	if dirA == dirB {
		t.Fatalf("les deux discussions déposent au même endroit : %s", dirA)
	}
	writeFile(t, filepath.Join(dirB, "note-b.txt"), "bbb")

	// La discussion ouverte ne voit QUE ses propres dépôts.
	names := listedNames(t, "uploads")
	if names["note-a.txt"] || !names["note-b.txt"] {
		t.Fatalf("la liste de la discussion b montre %v", names)
	}
	// Et le message ne peut pas joindre le fichier de l'autre discussion.
	if got := attachFiles([]string{"uploads/note-a.txt"}); len(got) != 0 {
		t.Fatalf("pièce jointe d'une autre discussion retenue : %+v", got)
	}
	if got := attachFiles([]string{"uploads/note-b.txt"}); len(got) != 1 {
		t.Fatalf("pièce jointe de la discussion courante refusée : %+v", got)
	}

	// Retour sur a : ses fichiers sont là, ceux de b ont disparu de la vue.
	if err := convSwitch(a); err != nil {
		t.Fatal(err)
	}
	names = listedNames(t, "uploads")
	if !names["note-a.txt"] || names["note-b.txt"] {
		t.Fatalf("la liste de la discussion a montre %v", names)
	}
}

// Supprimer une discussion emporte TOUS ses fichiers, pas seulement ses
// captures — et surtout pas ceux des autres.
func TestConvDeleteSupprimeLesFichiers(t *testing.T) {
	ws := withWorkspace(t)

	a := convEnsureActive()
	writeFile(t, filepath.Join(ws, "uploads", "joint.pdf"), "pdf")
	writeFile(t, filepath.Join(ws, "rapport.md"), "rapport")

	b := convNew()
	autre := convWorkspace()
	writeFile(t, filepath.Join(autre, "garde-moi.txt"), "ok")

	if err := convDelete(a); err != nil {
		t.Fatalf("suppression : %v", err)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatalf("le dossier de la discussion supprimée est toujours là (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(autre, "garde-moi.txt")); err != nil {
		t.Fatalf("les fichiers d'une AUTRE discussion ont été supprimés : %v", err)
	}
	if convEnsureActive() != b {
		t.Fatalf("discussion active = %q, attendu %q", convEnsureActive(), b)
	}
}

// Vider une discussion (« clear chat ») efface aussi ses fichiers : les messages
// qui les mentionnaient partent avec.
func TestResetSupprimeLesFichiersDeLaDiscussion(t *testing.T) {
	ws := withWorkspace(t)
	writeFile(t, filepath.Join(ws, "uploads", "joint.pdf"), "pdf")

	conv.Reset()

	if _, err := os.Stat(filepath.Join(ws, "uploads", "joint.pdf")); !os.IsNotExist(err) {
		t.Fatalf("le fichier de la discussion vidée a survécu (%v)", err)
	}
}

// Un identifiant de discussion douteux ne doit JAMAIS devenir un segment de
// chemin : dropConvFiles effacerait alors la racine du dossier de travail.
func TestDropConvFilesRefuseUnIdDouteux(t *testing.T) {
	ws := withWorkspace(t)
	writeFile(t, filepath.Join(ws, "a.txt"), "a")
	racine := agentWorkspace()

	for _, bad := range []string{"", ".", "..", "../..", "c1/../..", "/etc", `..\..`} {
		dropConvFiles(bad)
		if _, err := os.Stat(racine); err != nil {
			t.Fatalf("id %q a détruit le dossier de travail : %v", bad, err)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "a.txt")); err != nil {
		t.Fatalf("fichier de la discussion détruit : %v", err)
	}
}

// Les liens des anciens messages doivent continuer d'ouvrir leur fichier : ils
// pointent vers uploads/ et captures/<id>/ à la RACINE du dossier de travail.
func TestTelechargementDesCheminsDAvant(t *testing.T) {
	withWorkspace(t)
	racine := agentWorkspace()
	id := convEnsureActive()

	writeFile(t, filepath.Join(racine, "uploads", "ancien.txt"), "avant")
	// Capture d'avant, déplacée par la migration là où elle est désormais rangée.
	writeFile(t, filepath.Join(convDirFor(id), captureDir, "vue.jpg"), "jpeg")

	for path, want := range map[string]string{
		"uploads/ancien.txt":               "avant",
		captureDir + "/" + id + "/vue.jpg": "jpeg",
		captureDir + "/vue.jpg":            "jpeg", // le chemin d'aujourd'hui
	} {
		rec := httptest.NewRecorder()
		handleChatFile(rec, httptest.NewRequest("GET", "/api/chat/file?path="+url.QueryEscape(path), nil))
		if rec.Code != 200 || rec.Body.String() != want {
			t.Errorf("path=%q : code %d, corps %q", path, rec.Code, rec.Body.String())
		}
	}
}

// La migration range les fichiers des versions précédentes : les captures par
// leur identifiant, les dépôts par la discussion qui les mentionne. Ce que
// personne ne réclame reste à la racine, atteignable par « hors discussion ».
func TestMigrationRangeLesFichiersDAvant(t *testing.T) {
	withWorkspace(t)
	racine := agentWorkspace()

	a := convEnsureActive()
	// Un tour avec pièce jointe : c'est cette trace, dans le journal de la
	// discussion, qui rattache le fichier à elle.
	state, _ := json.Marshal(map[string]any{
		"log": []map[string]any{{"seq": 1, "delta": map[string]any{
			"user":  "regarde",
			"files": []attachInfo{{Name: "joint.pdf", Path: "uploads/joint.pdf", Size: 3}},
		}}},
	})
	if err := putBytes(bkChat, convKey(a), state); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(racine, "uploads", "joint.pdf"), "pdf")
	writeFile(t, filepath.Join(racine, "uploads", "orphelin.bin"), "??")
	writeFile(t, filepath.Join(racine, captureDir, a, "vue.jpg"), "jpeg")

	// La migration ne joue qu'une fois par process : on rearme le verrou pour
	// pouvoir l'observer ici.
	convFilesMigrateOnce = sync.Once{}
	migrateConvFiles()

	if _, err := os.Stat(filepath.Join(convDirFor(a), uploadsSub, "joint.pdf")); err != nil {
		t.Errorf("le dépôt n'a pas rejoint sa discussion : %v", err)
	}
	if _, err := os.Stat(filepath.Join(convDirFor(a), captureDir, "vue.jpg")); err != nil {
		t.Errorf("la capture n'a pas rejoint sa discussion : %v", err)
	}
	// Le fichier que personne ne réclame reste lisible plutôt que rattaché au hasard.
	if _, err := os.Stat(filepath.Join(racine, uploadsSub, "orphelin.bin")); err != nil {
		t.Errorf("le dépôt orphelin a été perdu : %v", err)
	}
	if legacyCount() == 0 {
		t.Error("le reliquat hors discussion n'est pas signalé à l'UI")
	}
}

// La vue « hors discussion » ne doit pas devenir une porte dérobée vers les
// fichiers des autres discussions.
func TestScopeLegacyNeDonnePasLesDiscussions(t *testing.T) {
	ws := withWorkspace(t)
	racine := agentWorkspace()
	writeFile(t, filepath.Join(ws, "secret.txt"), "x")
	writeFile(t, filepath.Join(racine, "ancien.txt"), "y")

	out := listScope(t, "", "legacy")
	if out["ok"] != true {
		t.Fatalf("liste refusée : %v", out)
	}
	for _, e := range out["entries"].([]any) {
		if e.(map[string]any)["name"] == convFilesRoot {
			t.Fatalf("le dossier des discussions est listé : %v", out)
		}
	}
	// Et il ne se supprime pas d'un clic : ce serait toutes les discussions.
	rec := httptest.NewRecorder()
	handleChatFileDelete(rec, httptest.NewRequest("POST", "/api/chat/file/delete",
		strings.NewReader(`{"path":`+jsonQuote(convFilesRoot)+`,"scope":"legacy"}`)))
	if rec.Code == http.StatusOK {
		t.Errorf("suppression du dossier des discussions acceptée : %s", rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "secret.txt")); err != nil {
		t.Fatalf("fichier d'une discussion supprimé : %v", err)
	}
}

// listedNames renvoie les noms listés dans un dossier du panneau.
func listedNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := listFiles(t, dir)
	names := map[string]bool{}
	if out["ok"] != true {
		return names
	}
	for _, e := range out["entries"].([]any) {
		names[e.(map[string]any)["name"].(string)] = true
	}
	return names
}

func listScope(t *testing.T, dir, scope string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	handleChatFiles(rec, httptest.NewRequest("GET",
		"/api/chat/files?dir="+url.QueryEscape(dir)+"&scope="+url.QueryEscape(scope), nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("réponse illisible (%d) : %s", rec.Code, rec.Body.String())
	}
	out["_code"] = rec.Code
	return out
}
