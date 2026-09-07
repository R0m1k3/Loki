package loki

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Le catalogue nourrit une liste déroulante ET des chemins de fichiers : une
// entrée bancale ne se verrait qu'au premier clic sur le micro.
func TestCatalogueCoherent(t *testing.T) {
	for id, m := range asrCatalogue {
		if m.Nom == "" || m.Dossier == "" || m.Langues == "" {
			t.Errorf("%s : entrée incomplète %+v", id, m)
		}
		if m.Octets <= 0 {
			t.Errorf("%s : taille annoncée %d — la confirmation de téléchargement en a besoin", id, m.Octets)
		}
		if !strings.HasPrefix(asrModelURLFor(id), "https://") {
			t.Errorf("%s : URL non https (%s)", id, asrModelURLFor(id))
		}
	}
	// Le défaut doit exister, et être celui qui parle français.
	def := DictateCfg{}.withDefauts().Model
	if _, ok := asrCatalogue[def]; !ok {
		t.Fatalf("le modèle par défaut %q n'est pas au catalogue", def)
	}
	if !strings.Contains(asrCatalogue[def].Langues, "français") {
		t.Errorf("le modèle par défaut (%s) ne parle pas français : %q", def, asrCatalogue[def].Langues)
	}
}

// Un identifiant hors catalogue ne doit JAMAIS produire de chemin : il vient
// d'une requête HTTP, et bâtir un chemin dessus ouvrirait le disque.
func TestCheminVideSiInconnu(t *testing.T) {
	testHome(t)
	if p := asrModelDirFor("../../etc"); p != "" {
		t.Errorf("chemin bâti sur un identifiant inconnu : %q", p)
	}
	if u := asrModelURLFor("nawak"); u != "" {
		t.Errorf("URL bâtie sur un identifiant inconnu : %q", u)
	}
	if f := asrModelFile("nawak", "encoder.int8.onnx"); f != "" {
		t.Errorf("chemin de fichier bâti sur un identifiant inconnu : %q", f)
	}
}

// Un modèle n'est « présent » que COMPLET : une extraction coupée laisse un
// dossier bien réel dont sherpa-onnx ne saura rien faire, et l'erreur tomberait
// au premier clic sur le micro plutôt qu'ici.
func TestPresentExigeTousLesFichiers(t *testing.T) {
	testHome(t)
	const id = "parakeet-tdt-0.6b-v3"
	dir := asrModelDirFor(id)
	if asrModelPresent(id) {
		t.Fatal("modèle annoncé présent alors que rien n'est installé")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, f := range asrFichiers {
		if asrModelPresent(id) {
			t.Fatalf("modèle annoncé présent avec seulement %d fichier(s) sur %d", i, len(asrFichiers))
		}
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !asrModelPresent(id) {
		t.Error("modèle complet non reconnu")
	}
	// Un fichier vide (disque plein en fin d'extraction) ne compte pas.
	if err := os.WriteFile(filepath.Join(dir, asrFichiers[0]), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if asrModelPresent(id) {
		t.Error("un fichier vide passe pour un modèle installé")
	}
}

func TestCatalogueTriParTaille(t *testing.T) {
	testHome(t)
	l := asrCatalogueTrie()
	if len(l) != len(asrCatalogue) {
		t.Fatalf("%d entrées, attendu %d", len(l), len(asrCatalogue))
	}
	for i := 1; i < len(l); i++ {
		if l[i-1]["octets"].(int64) > l[i]["octets"].(int64) {
			t.Error("catalogue non trié du plus léger au plus lourd")
		}
	}
	for _, e := range l {
		for _, clef := range []string{"id", "nom", "octets", "langues", "present"} {
			if _, ok := e[clef]; !ok {
				t.Errorf("clé %q manquante — l'UI en a besoin", clef)
			}
		}
	}
}

// ─── Extraction d'archive ────────────────────────────────────────────────────

// asrSafeRel est la seule chose entre une archive téléchargée et le disque du
// serveur. Une entrée qui remonte ou qui est absolue doit être REFUSÉE, pas
// nettoyée en silence.
func TestSafeRelRefuseLesEvasions(t *testing.T) {
	for _, mauvais := range []string{
		"/etc/passwd",
		"modele/../../etc/passwd",
		"../dehors.txt",
		`modele\..\..\dehors.txt`,
	} {
		if _, err := asrSafeRel(mauvais); err == nil {
			t.Errorf("entrée d'archive acceptée alors qu'elle sort du dossier : %q", mauvais)
		}
	}
	// Le dossier de premier niveau est retiré, le reste conservé.
	got, err := asrSafeRel("sherpa-onnx-nemo-parakeet/encoder.int8.onnx")
	if err != nil {
		t.Fatal(err)
	}
	if got != "encoder.int8.onnx" {
		t.Errorf("chemin = %q, le dossier racine de l'archive doit être retiré", got)
	}
	// L'entrée du dossier racine lui-même ne produit rien.
	if got, err := asrSafeRel("sherpa-onnx-nemo-parakeet/"); err != nil || got != "" {
		t.Errorf("racine de l'archive : (%q, %v), attendu (\"\", nil)", got, err)
	}
}

// Une extraction ratée ne doit jamais laisser un modèle à moitié installé sous
// son nom définitif : asrModelPresent le croirait bon et le serveur mourrait au
// démarrage. Elle ne doit pas non plus détruire un modèle déjà en place — le
// remplacement n'a lieu qu'une fois l'extraction terminée.
func TestExtractNeLaissePasDeDossierPartiel(t *testing.T) {
	testHome(t)
	dest := filepath.Join(t.TempDir(), "modele")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	temoin := filepath.Join(dest, "deja-la.txt")
	if err := os.WriteFile(temoin, []byte("modèle précédent"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Un flux qui n'est pas du bzip2 : la lecture échoue dès la première entrée.
	if err := asrExtract(bytes.NewReader([]byte("ceci n'est pas une archive bzip2")), dest); err == nil {
		t.Fatal("un flux illisible doit produire une erreur")
	}
	if _, err := os.Stat(dest + ".part"); err == nil {
		t.Error("le dossier temporaire n'a pas été nettoyé après l'échec")
	}
	if _, err := os.Stat(temoin); err != nil {
		t.Error("le modèle précédent a été détruit par une extraction qui a échoué")
	}
}
