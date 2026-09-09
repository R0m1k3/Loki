package loki

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// writeModel pose un .gguf factice dans le dossier des modèles.
func writeModel(t *testing.T, name string) string {
	t.Helper()
	dir := modelsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// postDelete joue /api/models/delete et renvoie le code HTTP + le corps décodé.
func postDelete(t *testing.T, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	handleModelDelete(rec, httptest.NewRequest(http.MethodPost, "/api/models/delete", bytes.NewReader(b)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("réponse illisible (%d) : %s", rec.Code, rec.Body.String())
	}
	return rec.Code, out
}

// Le modèle que le moteur a ouvert ne se supprime pas : l'espace ne serait pas
// rendu tant que le processus tient le descripteur, et le démarrage suivant
// mourrait sur un fichier introuvable.
func TestModelDeleteRefuseLeModeleCharge(t *testing.T) {
	testHome(t)
	writeModel(t, "charge.gguf")
	if err := WriteConfig(map[string]string{"MODEL": "charge.gguf"}); err != nil {
		t.Fatal(err)
	}
	code, out := postDelete(t, map[string]any{"name": "charge.gguf"})
	if code != 409 || out["ok"] != false || out["loaded"] != true {
		t.Fatalf("modèle chargé supprimable : %d %v", code, out)
	}
	if _, err := os.Stat(filepath.Join(modelsDir(), "charge.gguf")); err != nil {
		t.Fatal("le modèle chargé a été supprimé")
	}
}

// Un modèle réclamé par des presets part quand même, mais seulement après un
// second « oui » : le serveur refuse sans force et NOMME les presets, pour que
// la confirmation dise ce qu'on casse.
func TestModelDeleteExigeForceQuandUnPresetLUtilise(t *testing.T) {
	testHome(t)
	writeModel(t, "vieux.gguf")
	if _, err := SavePreset("", "Mon preset", "MODEL=\"vieux.gguf\"\nCTX=8192\n"); err != nil {
		t.Fatal(err)
	}
	code, out := postDelete(t, map[string]any{"name": "vieux.gguf"})
	if code != 409 || out["needsForce"] != true {
		t.Fatalf("suppression non protégée : %d %v", code, out)
	}
	names, _ := out["presets"].([]any)
	if len(names) != 1 || names[0] != "Mon preset" {
		t.Fatalf("presets référents non renvoyés : %v", out["presets"])
	}
	if _, err := os.Stat(filepath.Join(modelsDir(), "vieux.gguf")); err != nil {
		t.Fatal("supprimé malgré le refus")
	}
	if code, out = postDelete(t, map[string]any{"name": "vieux.gguf", "force": true}); code != 200 || out["ok"] != true {
		t.Fatalf("force refusé : %d %v", code, out)
	}
	if _, err := os.Stat(filepath.Join(modelsDir(), "vieux.gguf")); !os.IsNotExist(err) {
		t.Fatal("le modèle n'a pas été supprimé")
	}
}

// Un modèle que personne ne réclame part du premier coup, et l'espace annoncé
// couvre TOUTES les tranches — c'est le chiffre qu'on affiche à l'utilisateur.
func TestModelDeleteLibereLaFamilleEntiere(t *testing.T) {
	testHome(t)
	writeModel(t, "libre-00001-of-00002.gguf")
	writeModel(t, "libre-00002-of-00002.gguf")
	code, out := postDelete(t, map[string]any{"name": "libre-00001-of-00002.gguf"})
	if code != 200 || out["ok"] != true {
		t.Fatalf("suppression refusée : %d %v", code, out)
	}
	if freed, _ := out["freed"].(float64); freed != 8 {
		t.Fatalf("espace libéré = %v ; attendu la famille entière (8 octets)", out["freed"])
	}
	for _, n := range []string{"libre-00001-of-00002.gguf", "libre-00002-of-00002.gguf"} {
		if _, err := os.Stat(filepath.Join(modelsDir(), n)); !os.IsNotExist(err) {
			t.Fatalf("tranche %s conservée", n)
		}
	}
}

// Un modèle absent ne doit pas répondre « supprimé » : sans ça, l'interface
// annonçait de la place libérée qui ne l'était pas.
func TestModelDeleteModeleAbsent(t *testing.T) {
	testHome(t)
	if code, out := postDelete(t, map[string]any{"name": "fantome.gguf"}); code != 404 || out["ok"] != false {
		t.Fatalf("modèle absent : %d %v", code, out)
	}
}

// /api/models annonce ce que le moteur a ouvert et qui réclame quoi : c'est ce
// qui permet à l'interface de griser le bouton « supprimer » du modèle chargé.
func TestModelsListeMarqueChargeEtPresets(t *testing.T) {
	testHome(t)
	writeModel(t, "a.gguf")
	writeModel(t, "b.gguf")
	if err := WriteConfig(map[string]string{"MODEL": "a.gguf"}); err != nil {
		t.Fatal(err)
	}
	if _, err := SavePreset("", "Preset B", "MODEL=\"b.gguf\"\n"); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handleModels(rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	byName := map[string]map[string]any{}
	for _, m := range list {
		byName[m["name"].(string)] = m
	}
	if byName["a.gguf"]["loaded"] != true {
		t.Fatalf("modèle chargé non marqué : %v", byName["a.gguf"])
	}
	refs, _ := byName["b.gguf"]["presets"].([]any)
	if len(refs) != 1 || refs[0] != "Preset B" {
		t.Fatalf("presets référents absents : %v", byName["b.gguf"])
	}
}

// La case « supprimer aussi le .gguf » de l'éditeur de preset passe par les
// mêmes garde-fous : un modèle qu'un AUTRE preset réclame reste sur le disque,
// sinon supprimer un preset laissait son voisin avec un moteur qui meurt au
// démarrage.
func TestSuppressionPresetConserveUnModelePartage(t *testing.T) {
	testHome(t)
	writeModel(t, "partage.gguf")
	if _, err := SavePreset("", "Preset A", "MODEL=\"partage.gguf\"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := SavePreset("", "Preset B", "MODEL=\"partage.gguf\"\nCTX=4096\n"); err != nil {
		t.Fatal(err)
	}
	deleted, errMsg := deletePresetModel("partage.gguf")
	if deleted != "" || errMsg == "" {
		t.Fatalf("modèle partagé supprimé : %q / %q", deleted, errMsg)
	}
	if _, err := os.Stat(filepath.Join(modelsDir(), "partage.gguf")); err != nil {
		t.Fatal("le modèle partagé a disparu")
	}
	// Plus aucun preset ne le réclame : il part.
	if err := DeletePreset("Preset A"); err != nil {
		t.Fatal(err)
	}
	if err := DeletePreset("Preset B"); err != nil {
		t.Fatal(err)
	}
	if deleted, errMsg = deletePresetModel("partage.gguf"); deleted == "" || errMsg != "" {
		t.Fatalf("modèle orphelin conservé : %q / %q", deleted, errMsg)
	}
}
