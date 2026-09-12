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

// writeModel pose un .gguf factice dans dir et renvoie son chemin.
func writeModel(t *testing.T, dir, name string, size int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Le scénario complet du rapport : un modèle téléchargé depuis un preset, le
// preset supprimé sans son .gguf, et plus rien pour reprendre le fichier.
// Après ce correctif il doit être A) retrouvé par le sélecteur, B) effaçable.
func TestModeleOrphelinResteUtilisableEtEffaçable(t *testing.T) {
	testHome(t)
	writeModel(t, modelsDir(), "modele.gguf", 1024)

	// A) Présent sur le disque, sous la valeur qu'on écrira dans MODEL=.
	value, path, ok := modelPresence("modele.gguf", "")
	if !ok {
		t.Fatal("modèle présent non détecté — le téléchargement resterait un cul-de-sac")
	}
	if value != "modele.gguf" {
		t.Fatalf("valeur MODEL= = %q, attendu le simple nom de fichier", value)
	}
	if path != filepath.Join(modelsDir(), "modele.gguf") {
		t.Fatalf("chemin = %q", path)
	}

	// B) Plus personne ne le référence : il s'efface sans force.
	users, active := modelUsers("modele.gguf")
	if active || len(users) != 0 {
		t.Fatalf("modèle orphelin annoncé utilisé : actif=%v presets=%v", active, users)
	}
	freed, err := deleteModelFile("modele.gguf")
	if err != nil {
		t.Fatalf("suppression refusée : %v", err)
	}
	if freed != 1024 {
		t.Fatalf("octets libérés = %d, attendu 1024", freed)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("le fichier est toujours là")
	}
}

// Le modèle que le moteur a ouvert ne s'efface pas : la place ne serait même pas
// rendue, et le prochain démarrage mourrait sur un .gguf absent.
func TestSuppressionRefuseeSurLeModeleEnService(t *testing.T) {
	testHome(t)
	writeModel(t, modelsDir(), "servi.gguf", 8)
	if err := WriteConfig(map[string]string{"MODEL": "servi.gguf"}); err != nil {
		t.Fatal(err)
	}
	if _, active := modelUsers("servi.gguf"); !active {
		t.Fatal("modèle de la configuration active non reconnu")
	}
	if _, err := deleteModelFile("servi.gguf"); err == nil {
		t.Fatal("le modèle en service a été supprimé")
	}
	if _, err := os.Stat(filepath.Join(modelsDir(), "servi.gguf")); err != nil {
		t.Fatal("fichier supprimé malgré le refus")
	}
}

// Un modèle nommé par des presets n'est pas bloqué : il est SIGNALÉ, et
// force:true tranche. Refuser tout court recréerait l'impasse qu'on corrige.
func TestSuppressionSignaleLesPresetsPuisObeitAForce(t *testing.T) {
	testHome(t)
	p := writeModel(t, modelsDir(), "partage.gguf", 16)
	if _, err := SavePreset("", "Rapide", "MODEL=\"partage.gguf\"\nCTX=4096\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := SavePreset("", "Long", "MODEL=\"partage.gguf\"\nCTX=131072\n"); err != nil {
		t.Fatal(err)
	}
	users, active := modelUsers("partage.gguf")
	if active {
		t.Fatal("aucune configuration active ne devrait référencer ce modèle")
	}
	if len(users) != 2 {
		t.Fatalf("presets référents = %v, attendu les deux", users)
	}

	post := func(body string) (int, map[string]any) {
		r := httptest.NewRequest("POST", "/api/models/delete", strings.NewReader(body))
		w := httptest.NewRecorder()
		handleModelDelete(w, r)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	code, out := post(`{"name":"partage.gguf"}`)
	if code != 409 {
		t.Fatalf("code = %d, attendu 409 (presets référents)", code)
	}
	if len(out["used"].([]any)) != 2 {
		t.Fatalf("la réponse doit NOMMER les presets concernés : %v", out)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("fichier supprimé alors que la confirmation n'a pas eu lieu")
	}
	if code, out = post(`{"name":"partage.gguf","force":true}`); code != 200 {
		t.Fatalf("code = %d avec force:true — %v", code, out)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("force:true n'a pas supprimé le fichier")
	}
}

// MMPROJ et un --mmproj resté dans EXTRA_ARGS comptent aussi : effacer le
// projecteur du modèle en service casse la vision sans rien dire.
func TestProjecteurCompteCommeReference(t *testing.T) {
	testHome(t)
	writeModel(t, modelsDir(), "mmproj-F16.gguf", 4)
	writeModel(t, modelsDir(), "vieux-mmproj.gguf", 4)
	if err := WriteConfig(map[string]string{"MODEL": "m.gguf", "MMPROJ": "mmproj-F16.gguf"}); err != nil {
		t.Fatal(err)
	}
	if _, active := modelUsers("mmproj-F16.gguf"); !active {
		t.Fatal("MMPROJ= non compté comme référence")
	}
	if _, err := SavePreset("", "Vision", `MODEL="m.gguf"`+"\n"+`EXTRA_ARGS="--mmproj vieux-mmproj.gguf --flash-attn"`+"\n"); err != nil {
		t.Fatal(err)
	}
	users, _ := modelUsers("vieux-mmproj.gguf")
	if len(users) != 1 || users[0] != "Vision" {
		t.Fatalf("--mmproj d'EXTRA_ARGS non compté : %v", users)
	}
}

// Une famille de tranches incomplète n'est pas « déjà là » : il reste à
// télécharger, et prétendre le contraire donnerait un moteur qui meurt sur un
// tenseur introuvable.
func TestFamilleIncompleteNestPasPresente(t *testing.T) {
	testHome(t)
	writeModel(t, modelsDir(), "gros-00001-of-00002.gguf", 4)
	if _, _, ok := modelPresence("gros-00001-of-00002.gguf", ""); ok {
		t.Fatal("famille incomplète annoncée présente")
	}
	writeModel(t, modelsDir(), "gros-00002-of-00002.gguf", 4)
	if _, _, ok := modelPresence("gros-00001-of-00002.gguf", ""); !ok {
		t.Fatal("famille complète non détectée")
	}
}

// Un modèle rangé dans un dossier déclaré ailleurs (disque externe) est trouvé
// lui aussi, et sous son chemin complet — un simple nom ne le désignerait pas.
func TestPresenceDansUnDossierDeclare(t *testing.T) {
	home := testHome(t)
	ext := filepath.Join(home, "externe")
	writeModel(t, ext, "ailleurs.gguf", 32)
	if _, _, ok := modelPresence("ailleurs.gguf", ""); ok {
		t.Fatal("dossier non déclaré : le modèle ne doit pas être visible")
	}
	if err := saveExtraModelDirs([]string{ext}); err != nil {
		t.Fatal(err)
	}
	value, _, ok := modelPresence("ailleurs.gguf", "")
	if !ok {
		t.Fatal("modèle d'un dossier déclaré non détecté")
	}
	if value != filepath.Join(ext, "ailleurs.gguf") {
		t.Fatalf("valeur MODEL= = %q, attendu le chemin complet", value)
	}
}

// /api/models doit marquer « dossier loki » le dossier de téléchargement et en
// donner le SIMPLE NOM : c'est ce que resolveModelPath retrouve, et c'est ce qui
// permet au sélecteur de reconnaître un MODEL=modele.gguf écrit à la main.
func TestListeModelesNommeLeDossierDeLoki(t *testing.T) {
	testHome(t)
	writeModel(t, modelsDir(), "local.gguf", 64)
	if err := WriteConfig(map[string]string{"MODEL": "local.gguf"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handleModels(w, httptest.NewRequest("GET", "/api/models", nil))
	var out []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("liste = %v", out)
	}
	if out[0]["home"] != true {
		t.Fatal("le dossier de téléchargement n'est pas marqué « dossier loki »")
	}
	if out[0]["value"] != "local.gguf" {
		t.Fatalf("value = %v, attendu le simple nom de fichier", out[0]["value"])
	}
	if out[0]["active"] != true {
		t.Fatal("le modèle en service doit être signalé comme tel")
	}
}

// Télécharger un modèle DÉJÀ sur le disque n'est pas une erreur : c'est le
// résultat voulu. Les deux routes doivent répondre « ok, exists » avec la valeur
// à écrire dans MODEL= — sinon l'interface s'arrête en rouge sur « le modèle
// existe déjà » et il devient impossible de refaire un preset dessus.
func TestTelechargerUnModelePresentLeSelectionne(t *testing.T) {
	testHome(t)
	writeModel(t, modelsDir(), "deja.gguf", 2048)
	const url = "https://huggingface.co/auteur/depot/resolve/main/deja.gguf"

	call := func(h func(w http.ResponseWriter, r *http.Request), path string) (int, map[string]any) {
		body := `{"url":"` + url + `","dir":""}`
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	// La sonde ne doit même pas sortir sur le réseau : elle répond depuis le disque.
	code, out := call(handleModelDownloadProbe, "/api/models/download/probe")
	if code != 200 || out["ok"] != true || out["exists"] != true {
		t.Fatalf("sonde : code=%d out=%v", code, out)
	}
	if out["value"] != "deja.gguf" {
		t.Fatalf("sonde : value = %v, attendu le nom à mettre dans MODEL=", out["value"])
	}

	code, out = call(handleModelDownload, "/api/models/download")
	if code != 200 || out["ok"] != true || out["exists"] != true {
		t.Fatalf("téléchargement : code=%d out=%v", code, out)
	}
	if out["value"] != "deja.gguf" {
		t.Fatalf("téléchargement : value = %v", out["value"])
	}
}
