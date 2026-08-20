package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

const gb = int64(1) << 30

// Arborescence réelle de ggml-org/Qwen3.8-27B-GGUF, relevée sur l'API Hugging
// Face. Trois familles de .gguf y cohabitent, et deux d'entre elles ne sont PAS
// des modèles : c'est tout l'enjeu du classement.
func ggmlOrgTree() []hfFile {
	return []hfFile{
		{Path: ".gitattributes", Size: 2011},
		{Path: "README.md", Size: 397},
		{Path: "convert.log", Size: 485915},
		{Path: "Qwen3.8-27B-BF16.gguf", Size: 53808281952},
		{Path: "Qwen3.8-27B-Q4_K_M.gguf", Size: 18973870432},
		{Path: "Qwen3.8-27B-Q8_0.gguf", Size: 28595763552},
		{Path: "mmproj-Qwen3.8-27B-BF16.gguf", Size: 931145888},
		{Path: "mmproj-Qwen3.8-27B-Q8_0.gguf", Size: 629247008},
		{Path: "mtp-Qwen3.8-27B-BF16.gguf", Size: 5946009888},
		{Path: "mtp-Qwen3.8-27B-Q4_0.gguf", Size: 1680271648},
		{Path: "mtp-Qwen3.8-27B-Q8_0.gguf", Size: 3164006688},
	}
}

func names(list []hfEntry) []string {
	out := make([]string, len(list))
	for i, e := range list {
		out[i] = e.Name
	}
	return out
}

func TestHFClassifySeparatesProjectorsAndDrafts(t *testing.T) {
	got := hfClassify("ggml-org/Qwen3.8-27B-GGUF", ggmlOrgTree())

	if len(got.Models) != 3 {
		t.Fatalf("modèles = %v, attendu 3 entrées", names(got.Models))
	}
	// Un mmproj proposé comme modèle, c'est un llama-server lancé sur un
	// encodeur d'images : il démarre et ne répond rien de sensé.
	for _, m := range got.Models {
		if strings.HasPrefix(m.Name, "mmproj") || strings.HasPrefix(m.Name, "mtp-") {
			t.Errorf("%q classé comme modèle", m.Name)
		}
	}
	if len(got.Projectors) != 2 {
		t.Errorf("projecteurs = %v, attendu 2", names(got.Projectors))
	}
	if len(got.Drafts) != 3 {
		t.Errorf("drafts = %v, attendu 3", names(got.Drafts))
	}
	// Les fichiers non-.gguf n'ont rien à faire dans une liste installable.
	for _, e := range append(append(got.Models, got.Projectors...), got.Drafts...) {
		if !strings.HasSuffix(e.Name, ".gguf") {
			t.Errorf("%q n'est pas un .gguf", e.Name)
		}
	}
}

func TestHFClassifyBuildsUsableURLs(t *testing.T) {
	got := hfClassify("ggml-org/Qwen3.8-27B-GGUF", ggmlOrgTree())
	want := "https://huggingface.co/ggml-org/Qwen3.8-27B-GGUF/resolve/main/Qwen3.8-27B-Q4_K_M.gguf"
	found := false
	for _, m := range got.Models {
		if m.URL == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("URL attendue absente ; obtenu %v", got.Models)
	}
	// L'URL doit traverser normalizeHFURL sans retouche : c'est elle que
	// /api/models/download/probe recevra.
	for _, m := range got.Models {
		if _, name, err := normalizeHFURL(m.URL); err != nil {
			t.Errorf("normalizeHFURL(%q) : %v", m.URL, err)
		} else if name != m.Name {
			t.Errorf("normalizeHFURL(%q) → %q, attendu %q", m.URL, name, m.Name)
		}
	}
}

func TestHFClassifyQuantAndSort(t *testing.T) {
	got := hfClassify("unsloth/Qwen3.8-27B-GGUF", []hfFile{
		{Path: "Qwen3.8-27B-UD-Q4_K_XL.gguf", Size: 17923394624},
		{Path: "Qwen3.8-27B-Q8_0.gguf", Size: 29047086048},
		{Path: "Qwen3.8-27B-UD-IQ2_XXS.gguf", Size: 9010048064},
	})
	// Du plus léger au plus lourd : la liste sert à choisir ce qui tient.
	if n := names(got.Models); n[0] != "Qwen3.8-27B-UD-IQ2_XXS.gguf" || n[2] != "Qwen3.8-27B-Q8_0.gguf" {
		t.Fatalf("tri par taille cassé : %v", n)
	}
	want := map[string]string{
		"Qwen3.8-27B-UD-Q4_K_XL.gguf": "Q4_K_XL",
		"Qwen3.8-27B-Q8_0.gguf":       "Q8_0",
		"Qwen3.8-27B-UD-IQ2_XXS.gguf": "IQ2_XXS",
	}
	for _, m := range got.Models {
		if want[m.Name] != m.Quant {
			t.Errorf("quant de %q = %q, attendu %q", m.Name, m.Quant, want[m.Name])
		}
	}
}

// Une famille de tranches doit donner UNE entrée, de taille totale. Annoncer le
// poids de la première tranche promet 15 Go là où le disque en verra 45.
func TestHFClassifyFoldsShards(t *testing.T) {
	got := hfClassify("unsloth/Big-GGUF", []hfFile{
		{Path: "UD-IQ4_XS/Big-UD-IQ4_XS-00001-of-00003.gguf", Size: 15 * gb},
		{Path: "UD-IQ4_XS/Big-UD-IQ4_XS-00002-of-00003.gguf", Size: 15 * gb},
		{Path: "UD-IQ4_XS/Big-UD-IQ4_XS-00003-of-00003.gguf", Size: 12 * gb},
	})
	if len(got.Models) != 1 {
		t.Fatalf("modèles = %v, attendu 1 famille repliée", names(got.Models))
	}
	m := got.Models[0]
	if m.Shards != 3 {
		t.Errorf("shards = %d, attendu 3", m.Shards)
	}
	if m.Size != 42*gb {
		t.Errorf("taille = %d, attendu %d (total de la famille)", m.Size, 42*gb)
	}
	if !strings.HasSuffix(m.Name, "-00001-of-00003.gguf") {
		t.Errorf("l'entrée doit désigner la PREMIÈRE tranche, obtenu %q", m.Name)
	}
}

// Deux dossiers de quantification portant des tranches distinctes ne doivent pas
// mélanger leurs tailles : le repli se fait par dossier, pas par nom de base.
func TestHFClassifyShardsStayInTheirDirectory(t *testing.T) {
	got := hfClassify("x/y", []hfFile{
		{Path: "Q4/M-00001-of-00002.gguf", Size: 4 * gb},
		{Path: "Q4/M-00002-of-00002.gguf", Size: 4 * gb},
		{Path: "Q8/M-00001-of-00002.gguf", Size: 9 * gb},
		{Path: "Q8/M-00002-of-00002.gguf", Size: 9 * gb},
	})
	if len(got.Models) != 2 {
		t.Fatalf("attendu 2 familles, obtenu %d", len(got.Models))
	}
	if got.Models[0].Size != 8*gb || got.Models[1].Size != 18*gb {
		t.Errorf("tailles = %d et %d, attendu %d et %d",
			got.Models[0].Size, got.Models[1].Size, 8*gb, 18*gb)
	}
}

func TestHFPickProjectorPrefersQ8(t *testing.T) {
	list := hfClassify("ggml-org/Qwen3.8-27B-GGUF", ggmlOrgTree()).Projectors
	p, ok := hfPickProjector(list)
	if !ok {
		t.Fatal("aucun projecteur trouvé alors que le dépôt en publie deux")
	}
	if p.Name != "mmproj-Qwen3.8-27B-Q8_0.gguf" {
		t.Errorf("projecteur choisi = %q, attendu le Q8_0", p.Name)
	}
	// Dépôt sans vision : il faut le DIRE, pas aller chercher ailleurs. Un
	// projecteur d'un autre modèle ne correspond jamais.
	if _, ok := hfPickProjector(nil); ok {
		t.Error("un dépôt sans projecteur ne doit rien proposer")
	}
}

func TestHFRepoValidation(t *testing.T) {
	for _, bad := range []string{"", "pasdeslash", "../../api/whoami", "/leading", "a//b", "x/y/z"} {
		if _, err := hfFiles(t.Context(), bad); err == nil {
			t.Errorf("dépôt %q accepté alors qu'il est invalide", bad)
		}
	}
}

func TestFitVerdict(t *testing.T) {
	gpu24 := hardwareInfo{VRAMGB: 24, RAMGB: 64}

	// Q8_0 de 29 Go : ne rentre pas dans 24 Go, quoi qu'il arrive.
	if v, why := fitVerdict(gpu24, 29*gb, 0, 32768); v != fitOver {
		t.Errorf("Q8_0 29 Go sur 24 Go de VRAM → %q (%s), attendu %q", v, why, fitOver)
	}
	// UD-Q4_K_XL de 17 Go + projecteur + contexte : ça tient.
	if v, why := fitVerdict(gpu24, 17*gb, 629<<20, 32768); v != fitOK {
		t.Errorf("Q4 17 Go sur 24 Go de VRAM → %q (%s), attendu %q", v, why, fitOK)
	}
	// Le projecteur doit peser dans la balance : à la limite, l'ajouter fait
	// basculer le verdict. C'est exactement le cas qu'on veut voir venir avant
	// de lancer 20 Go de téléchargement.
	sans, _ := fitVerdict(hardwareInfo{VRAMGB: 20}, 18*gb, 0, 32768)
	avec, _ := fitVerdict(hardwareInfo{VRAMGB: 20}, 18*gb, 3*gb, 32768)
	if sans == avec {
		t.Errorf("le projecteur ne change rien au verdict (%q dans les deux cas)", sans)
	}
	// Sans GPU, le verdict se prononce sur la RAM et le dit.
	if v, why := fitVerdict(hardwareInfo{RAMGB: 64}, 17*gb, 0, 32768); v != fitOK || !strings.Contains(why, "RAM") {
		t.Errorf("sans GPU → %q (%s), attendu %q mesuré en RAM", v, why, fitOK)
	}
	// Machine non mesurable : pas de verdict inventé.
	if v, _ := fitVerdict(hardwareInfo{}, 17*gb, 0, 32768); v != "" {
		t.Errorf("sans mesure mémoire → %q, attendu aucun verdict", v)
	}
	// Un contexte plus large coûte plus cher, et doit pouvoir faire basculer.
	court, _ := fitVerdict(hardwareInfo{VRAMGB: 24}, 21*gb, 0, 8192)
	long, _ := fitVerdict(hardwareInfo{VRAMGB: 24}, 21*gb, 0, 262144)
	if court == long {
		t.Errorf("le contexte ne change rien au verdict (%q dans les deux cas)", court)
	}
}

// Le champ `gated` de l'API n'est pas un booléen : false, "auto" ou "manual".
// Le lire comme un bool laissait passer les deux valeurs qui verrouillent
// vraiment le téléchargement.
func TestHFGatedFlag(t *testing.T) {
	for _, c := range []struct {
		in   any
		want bool
	}{
		{nil, false},
		{false, false},
		{true, true},
		{"auto", true},
		{"manual", true},
		{"false", false},
		{"", false},
	} {
		if got := hfGatedFlag(c.in); got != c.want {
			t.Errorf("hfGatedFlag(%#v) = %v, attendu %v", c.in, got, c.want)
		}
	}
}

// hfRepoFromURL sert à NOMMER le dépôt dans le message d'erreur : un lien qui
// ne vient pas de Hugging Face ne doit rien produire plutôt qu'un nom inventé.
func TestHFRepoFromURL(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"https://huggingface.co/orcarouter/Qwen3.8-27B-Uncensored-GGUF/resolve/main/m.gguf", "orcarouter/Qwen3.8-27B-Uncensored-GGUF"},
		{"https://huggingface.co/ggml-org/Qwen3.8-27B-GGUF/blob/main/sub/dir/m.gguf", "ggml-org/Qwen3.8-27B-GGUF"},
		{"https://example.com/ggml-org/Qwen3.8-27B-GGUF/resolve/main/m.gguf", ""},
		{"https://huggingface.co/ggml-org/Qwen3.8-27B-GGUF", ""},
		{"pas une url", ""},
	} {
		if got := hfRepoFromURL(c.in); got != c.want {
			t.Errorf("hfRepoFromURL(%q) = %q, attendu %q", c.in, got, c.want)
		}
	}
}

// Le jeton enregistré dans l'interface prime sur HF_TOKEN, et son absence
// rend exactement le comportement d'avant : la variable d'environnement.
func TestHFTokenPrecedence(t *testing.T) {
	testHome(t)
	t.Setenv("HF_TOKEN", "")
	if hfToken() != "" || hfTokenSource() != "" || hfTokenSet() {
		t.Fatalf("sans rien : jeton=%q source=%q", hfToken(), hfTokenSource())
	}
	t.Setenv("HF_TOKEN", "hf_env")
	if hfToken() != "hf_env" || hfTokenSource() != "env" {
		t.Fatalf("HF_TOKEN seul : jeton=%q source=%q", hfToken(), hfTokenSource())
	}
	if err := writeHFToken("  hf_enregistre  "); err != nil {
		t.Fatal(err)
	}
	if hfToken() != "hf_enregistre" || hfTokenSource() != "config" {
		t.Fatalf("jeton enregistré : jeton=%q source=%q", hfToken(), hfTokenSource())
	}
	// Retiré ici, la variable du conteneur reprend la main plutôt que de laisser
	// Loki sans jeton alors que l'environnement en fournit un.
	if err := writeHFToken(""); err != nil {
		t.Fatal(err)
	}
	if hfToken() != "hf_env" || hfTokenSource() != "env" {
		t.Fatalf("après retrait : jeton=%q source=%q", hfToken(), hfTokenSource())
	}
}

// Le jeton ne doit jamais ressortir en clair de l'interface.
func TestMaskHFToken(t *testing.T) {
	if got := maskHFToken("hf_abcdefghijklmnop"); got != "hf_…mnop" {
		t.Errorf("masque = %q", got)
	}
	if got := maskHFToken(""); got != "" {
		t.Errorf("masque d'un jeton absent = %q", got)
	}
	if strings.Contains(maskHFToken("hf_abcdefghijklmnop"), "efghij") {
		t.Error("le masque laisse voir le milieu du jeton")
	}
}

// Un secret ne part QUE vers Hugging Face : un lien collé vers un autre
// hébergeur ne doit pas recevoir le jeton du compte.
func TestDLRequestTokenOnlyToHuggingFace(t *testing.T) {
	testHome(t)
	t.Setenv("HF_TOKEN", "hf_secret")
	for _, c := range []struct {
		url  string
		want bool
	}{
		{"https://huggingface.co/a/b/resolve/main/m.gguf", true},
		{"https://cdn-lfs.huggingface.co/a/b/m.gguf", true},
		{"https://hf.co/a/b/resolve/main/m.gguf", true},
		{"https://example.com/m.gguf", false},
		{"https://huggingface.co.evil.example/m.gguf", false},
	} {
		req, err := dlRequest(context.Background(), c.url, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization") != ""; got != c.want {
			t.Errorf("%s : en-tête Authorization présent=%v, attendu %v", c.url, got, c.want)
		}
	}
}

// La route décrit le jeton sans jamais le rendre, et l'efface sans appeler
// Hugging Face (un jeton vide n'a rien à vérifier).
func TestHandleHFTokenGetAndClear(t *testing.T) {
	testHome(t)
	t.Setenv("HF_TOKEN", "")
	if err := writeHFToken("hf_abcdefghijklmnop"); err != nil {
		t.Fatal(err)
	}

	call := func(method, body string) map[string]any {
		t.Helper()
		r := httptest.NewRequest(method, "http://placeholder/api/hf/token", strings.NewReader(body))
		w := httptest.NewRecorder()
		handleHFToken(w, r)
		if w.Code != 200 {
			t.Fatalf("%s → HTTP %d : %s", method, w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	got := call("GET", "")
	if got["set"] != true || got["source"] != "config" {
		t.Fatalf("état lu = %v", got)
	}
	if s, _ := got["masked"].(string); s == "" || strings.Contains(s, "efghij") {
		t.Fatalf("masque inattendu : %q", s)
	}
	if strings.Contains(fmt.Sprint(got), "hf_abcdefghijklmnop") {
		t.Fatalf("le jeton ressort en clair : %v", got)
	}

	got = call("POST", `{"token":""}`)
	if got["set"] != false || got["source"] != "" {
		t.Fatalf("après effacement = %v", got)
	}
	if hfToken() != "" {
		t.Fatalf("jeton toujours présent : %q", hfToken())
	}
}
