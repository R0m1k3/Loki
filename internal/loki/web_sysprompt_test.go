package loki

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTestPreset pose un preset minimal et renvoie son id. Le contenu doit
// différer d'un preset à l'autre : c'est l'empreinte du contenu qui décide
// lequel est « actif » (presetFingerprint).
func writeTestPreset(t *testing.T, id, model string) string {
	t.Helper()
	dir := presetsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# NAME=" + id + "\nMODEL=" + model + "\nCTX=8192\n"
	if err := os.WriteFile(filepath.Join(dir, id+".env"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return id
}

// Le prompt système appartient au PRESET : basculer de modèle doit changer la
// consigne, pas la traîner d'un modèle à l'autre. C'était le défaut du prompt
// global — une consigne écrite pour un modèle à raisonnement suivait un petit
// modèle d'instruction, qu'elle dessert.
func TestSysPromptSuitLePresetActif(t *testing.T) {
	newProjectHome(t)
	a := writeTestPreset(t, "modele-a", "/models/a.gguf")
	b := writeTestPreset(t, "modele-b", "/models/b.gguf")

	if err := applyPresetFile(filepath.Join(presetsDir(), a+".env")); err != nil {
		t.Fatal(err)
	}
	if id := activePresetID(); id != a {
		t.Fatalf("preset actif %q, attendu %q", id, a)
	}
	if err := saveSysPrompt("réponds en une phrase"); err != nil {
		t.Fatal(err)
	}
	if got := readSysPrompt(); got != "réponds en une phrase" {
		t.Fatalf("prompt du preset A = %q", got)
	}

	if err := applyPresetFile(filepath.Join(presetsDir(), b+".env")); err != nil {
		t.Fatal(err)
	}
	if got := readSysPrompt(); got != "" {
		t.Fatalf("le prompt du preset A a suivi jusqu'au preset B : %q", got)
	}
	if err := saveSysPrompt("tu es un assistant technique"); err != nil {
		t.Fatal(err)
	}

	// Retour sur A : sa consigne doit être intacte.
	if err := applyPresetFile(filepath.Join(presetsDir(), a+".env")); err != nil {
		t.Fatal(err)
	}
	if got := readSysPrompt(); got != "réponds en une phrase" {
		t.Fatalf("le prompt du preset A n'a pas survécu à l'aller-retour : %q", got)
	}
}

// Un prompt multiligne doit passer tel quel : c'est le cas courant, et c'est
// précisément ce qu'un fichier .env (lu ligne à ligne) ne saurait porter — d'où
// le stockage en base.
func TestSysPromptMultiligne(t *testing.T) {
	newProjectHome(t)
	id := writeTestPreset(t, "modele-a", "/models/a.gguf")
	if err := applyPresetFile(filepath.Join(presetsDir(), id+".env")); err != nil {
		t.Fatal(err)
	}
	prompt := "Tu es concis.\n\nRègles :\n- pas de préambule\n- du français"
	if err := saveSysPrompt(prompt); err != nil {
		t.Fatal(err)
	}
	if got := readSysPrompt(); got != prompt {
		t.Fatalf("prompt multiligne altéré :\n%q", got)
	}
}

// Repli : une installation sans preset identifiable (config bricolée à la main)
// doit garder un champ qui fonctionne, et retrouver son ancien prompt global.
func TestSysPromptReplieSurLeGlobal(t *testing.T) {
	newProjectHome(t)
	if id := activePresetID(); id != "" {
		t.Fatalf("preset actif %q alors qu'aucun n'existe", id)
	}
	if err := saveSysPrompt("consigne globale"); err != nil {
		t.Fatal(err)
	}
	if got := readSysPrompt(); got != "consigne globale" {
		t.Fatalf("prompt global = %q", got)
	}
	// Un preset apparaît et devient actif : tant qu'il n'a pas SA consigne, le
	// global continue de s'appliquer — pas de perte silencieuse après migration.
	id := writeTestPreset(t, "modele-a", "/models/a.gguf")
	if err := applyPresetFile(filepath.Join(presetsDir(), id+".env")); err != nil {
		t.Fatal(err)
	}
	if got := readSysPrompt(); got != "consigne globale" {
		t.Fatalf("le prompt global n'est plus le repli : %q", got)
	}
}

// Supprimer un preset doit emporter sa consigne : sinon elle ressuscite sur un
// preset recréé plus tard sous le même identifiant.
func TestDeletePresetEmporteSonSysPrompt(t *testing.T) {
	newProjectHome(t)
	a := writeTestPreset(t, "modele-a", "/models/a.gguf")
	b := writeTestPreset(t, "modele-b", "/models/b.gguf")
	if err := applyPresetFile(filepath.Join(presetsDir(), a+".env")); err != nil {
		t.Fatal(err)
	}
	if err := saveSysPrompt("consigne du A"); err != nil {
		t.Fatal(err)
	}
	// On quitte A : un preset actif n'est pas supprimable.
	if err := applyPresetFile(filepath.Join(presetsDir(), b+".env")); err != nil {
		t.Fatal(err)
	}
	if err := DeletePreset(a); err != nil {
		t.Fatal(err)
	}
	writeTestPreset(t, a, "/models/a.gguf")
	if err := applyPresetFile(filepath.Join(presetsDir(), a+".env")); err != nil {
		t.Fatal(err)
	}
	if got := readSysPrompt(); got != "" {
		t.Fatalf("la consigne d'un preset supprimé est revenue : %q", got)
	}
}
