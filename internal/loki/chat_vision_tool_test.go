package loki

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pngPixel : un PNG valide d'un pixel, assez pour qu'imageMime le reconnaisse.
var pngPixel, _ = base64.StdEncoding.DecodeString(
	"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")

// visionOn arme la vision (clé MMPROJ) et renvoie le dossier de travail.
func visionOn(t *testing.T) string {
	t.Helper()
	home := testHome(t)
	if err := SetConfigKey("MMPROJ", "mmproj-F16.gguf"); err != nil {
		t.Fatal(err)
	}
	return home
}

// Sans projecteur, l'outil DIT ce qui manque au lieu d'échouer vaguement — et
// il ne renvoie surtout aucune image.
func TestSeeImageSansVisionRefuseEtExplique(t *testing.T) {
	testHome(t)
	txt, img := toolSeeImage("photo.png")
	if img != nil {
		t.Fatal("une image a été chargée alors que la vision est inactive")
	}
	if !strings.Contains(txt, "MMPROJ") {
		t.Fatalf("le refus ne dit pas ce qui manque : %q", txt)
	}
}

// Les refus qui ne dépendent pas du moteur : format, absence, dossier, poids.
// Chacun doit nommer son motif — un modèle qui reçoit « impossible » ne peut
// pas corriger son geste, alors qu'un « ce n'est pas une image » se rattrape.
func TestSeeImageMotifsDeRefus(t *testing.T) {
	home := visionOn(t)
	write := func(name string, b []byte) string {
		p := filepath.Join(home, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("notes.txt", []byte("bonjour"))
	if err := os.MkdirAll(filepath.Join(home, "dossier.png"), 0o755); err != nil {
		t.Fatal(err)
	}
	gros := write("gros.png", append(append([]byte(nil), pngPixel...), make([]byte, maxVisionBytes)...))

	cas := []struct{ path, attendu string }{
		{"", "chemin de fichier manquant"},
		{filepath.Join(home, "notes.txt"), "format non reconnu"},
		{filepath.Join(home, "absente.png"), "introuvable"},
		{filepath.Join(home, "dossier.png"), "dossier"},
		{gros, "trop lourde"},
	}
	for _, c := range cas {
		txt, img := toolSeeImage(c.path)
		if img != nil {
			t.Errorf("%q : une image a été chargée malgré le refus", c.path)
		}
		if !strings.Contains(txt, c.attendu) {
			t.Errorf("%q : motif = %q, attendu contenir %q", c.path, txt, c.attendu)
		}
	}
}

// Le message porteur nomme le fichier : après compactage il ne restera que sa
// légende et imageLostMarker, et « Image demandée : » tout court ne dirait pas
// LAQUELLE rouvrir.
func TestMessagePorteurNommeLeFichier(t *testing.T) {
	img := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}}
	m := seeImageMessage("captures/bug.png", img)
	if m.Role != "user" {
		t.Fatalf("rôle = %q, attendu user (un message tool ne porte que du texte)", m.Role)
	}
	parts, ok := m.Content.([]map[string]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("contenu multimodal attendu, obtenu %#v", m.Content)
	}
	txt, _ := parts[0]["text"].(string)
	if !strings.Contains(txt, "captures/bug.png") {
		t.Fatalf("la légende ne nomme pas le fichier : %q", txt)
	}
	if parts[1]["type"] != "image_url" {
		t.Fatalf("seconde partie = %v, attendu image_url", parts[1]["type"])
	}
	// Et cette légende survit au compactage, suivie du marqueur de perte.
	got := msgText(m)
	if !strings.Contains(got, "captures/bug.png") || !strings.Contains(got, imageLostMarker) {
		t.Fatalf("après compaction : %q", got)
	}
}

// Le marqueur ne doit plus parler QUE de captures d'écran : une image perdue
// peut venir du disque, et « reprends la capture » enverrait photographier une
// page web à la place.
func TestMarqueurDePerteNestPlusSpecifiqueAuxCaptures(t *testing.T) {
	if strings.Contains(imageLostMarker, "take the screenshot again") {
		t.Fatal("le marqueur impose encore la capture d'écran comme seul recours")
	}
	if !strings.Contains(imageLostMarker, "see_image") {
		t.Fatal("le marqueur ne mentionne pas see_image : le modèle ne saura pas rouvrir un fichier")
	}
}

// L'outil n'est proposé QUE si la vision est active : l'annoncer sans projecteur
// ferait promettre au modèle de regarder, puis se contredire.
func TestOutilProposeSeulementAvecLaVision(t *testing.T) {
	testHome(t)
	has := func() bool {
		for _, tl := range EnabledTools(Caps{Agent: true}) {
			if tl.Function.Name == "see_image" {
				return true
			}
		}
		return false
	}
	if has() {
		t.Fatal("see_image proposé sans projecteur configuré")
	}
	if err := SetConfigKey("MMPROJ", "mmproj-F16.gguf"); err != nil {
		t.Fatal(err)
	}
	if !has() {
		t.Fatal("see_image absent alors que la vision est active")
	}
	// Hors mode agent, jamais : l'outil lit un fichier du disque.
	if err := SetConfigKey("MMPROJ", "mmproj-F16.gguf"); err != nil {
		t.Fatal(err)
	}
	for _, tl := range EnabledTools(Caps{}) {
		if tl.Function.Name == "see_image" {
			t.Fatal("see_image proposé hors mode agent")
		}
	}
}

// Le chemin doit remonter du JSON d'appel jusqu'à l'outil. Le libellé d'un appel
// est dérivé par une table nom d'outil → argument ; see_image n'y figurait pas,
// donc il recevait une chaîne vide et répondait invariablement « chemin de
// fichier manquant » — l'outil aurait été inutilisable sans rien casser ailleurs.
func TestLeCheminDeLImageRemonteJusquALOutil(t *testing.T) {
	if got := toolCallLabel("see_image", map[string]any{"file": "captures/bug.png"}); got != "captures/bug.png" {
		t.Fatalf("libellé de see_image = %q, attendu le chemin du fichier", got)
	}
}

// Contre-épreuve de l'extraction : les libellés des autres outils n'ont pas
// bougé. Déplacer une table de 40 lignes hors d'une fonction de 700, c'est
// exactement le genre de geste qui casse un cas au passage sans bruit.
func TestLibellesDesAutresOutilsInchanges(t *testing.T) {
	cas := []struct{ tool, key, val, want string }{
		{"bash", "command", "ls -la", "ls -la"},
		{"read", "file", "main.go", "main.go"},
		{"web_search", "query", "météo", "météo"},
		{"grep", "pattern", "TODO", "TODO"},
		{"recall", "id", "b12", "b12"},
		{"git_clone", "url", "https://x/y", "https://x/y"},
		{"outil_inconnu", "file", "x", ""},
	}
	for _, c := range cas {
		if got := toolCallLabel(c.tool, map[string]any{c.key: c.val}); got != c.want {
			t.Errorf("toolCallLabel(%q) = %q, attendu %q", c.tool, got, c.want)
		}
	}
	if got := toolCallLabel("web_grep", map[string]any{"url": "https://x", "pattern": "p"}); got != "p @ https://x" {
		t.Errorf("web_grep : %q", got)
	}
	if got := toolCallLabel("tracker", map[string]any{"action": "add", "name": "poids"}); got != "add poids" {
		t.Errorf("tracker : %q", got)
	}
	// Argument absent ou du mauvais type : chaîne vide, jamais de panique.
	if got := toolCallLabel("read", map[string]any{"file": 42}); got != "" {
		t.Errorf("argument non-chaîne : %q", got)
	}
	if got := toolCallLabel("bash", nil); got != "" {
		t.Errorf("arguments nil : %q", got)
	}
}
