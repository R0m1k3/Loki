package loki

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Les trois formes d'URL qu'on colle en pratique. Exiger la forme exacte serait
// une source d'échecs muets : la page d'un fournisseur donne tantôt la base,
// tantôt l'URL complète, et se tromper ne produit qu'un 404 illisible.
func TestNormalisationDeLURLDeComplétions(t *testing.T) {
	cas := map[string]string{
		"https://api.openai.com/v1":                  "https://api.openai.com/v1/chat/completions",
		"https://api.groq.com/openai/v1":             "https://api.groq.com/openai/v1/chat/completions",
		"https://api.openai.com/v1/chat/completions": "https://api.openai.com/v1/chat/completions",
		"https://api.openai.com/v1/":                 "https://api.openai.com/v1/chat/completions",
		"http://192.168.1.20:8080":                   "http://192.168.1.20:8080/v1/chat/completions",
		"":                                           "",
	}
	for in, want := range cas {
		if got := completionsURL(in); got != want {
			t.Errorf("completionsURL(%q) = %q, attendu %q", in, got, want)
		}
	}
}

// Un preset externe actif détourne les complétions ; sans lui, on reste sur le
// llama-server local avec la clé LOCALE.
func TestEndpointSuitLePresetActif(t *testing.T) {
	testHome(t)
	if err := SetConfigKey("PORT", "9999"); err != nil {
		t.Fatal(err)
	}
	ep := resolveChatEndpoint()
	if ep.External {
		t.Fatal("aucun preset externe : l'endpoint doit rester local")
	}
	if ep.URL != "http://localhost:9999/v1/chat/completions" {
		t.Fatalf("URL locale = %q", ep.URL)
	}
	if ep.Model != "loki" {
		t.Fatalf("modèle local = %q, attendu loki", ep.Model)
	}

	if err := WriteConfig(parseEnv(externalPresetContent(
		"https://api.groq.com/openai/v1", "llama-3.3-70b", "sk-secret", "65536"))); err != nil {
		t.Fatal(err)
	}
	ep = resolveChatEndpoint()
	if !ep.External {
		t.Fatal("EXTERNAL=1 non reconnu")
	}
	if ep.URL != "https://api.groq.com/openai/v1/chat/completions" {
		t.Fatalf("URL externe = %q", ep.URL)
	}
	if ep.Model != "llama-3.3-70b" || ep.Key != "sk-secret" {
		t.Fatalf("modèle/clé externes = %q / %q", ep.Model, ep.Key)
	}
	// CTX passe par la clé standard : c'est elle qui pilote la jauge de contexte
	// et le seuil de compaction, exactement comme pour un preset local.
	if got := ReadConfig()["CTX"]; got != "65536" {
		t.Fatalf("CTX = %q, attendu 65536", got)
	}
}

// La clé du serveur LOCAL ne doit jamais partir chez un tiers : ce serait fuiter
// un secret en même temps qu'un 401 garanti.
func TestLaCleLocaleNePartJamaisChezUnTiers(t *testing.T) {
	testHome(t)
	if err := WriteConfig(parseEnv(externalPresetContent(
		"https://api.openai.com/v1", "gpt-4o-mini", "", ""))); err != nil {
		t.Fatal(err)
	}
	// Une clé d'API locale existe et n'a rien à faire dans la requête sortante.
	if err := SetConfigKey("API_KEY", "sk-loki-locale"); err != nil {
		t.Fatal(err)
	}
	ep := resolveChatEndpoint()
	if ep.Key != "" {
		t.Fatalf("clé envoyée à l'API externe = %q — le preset n'en déclare aucune", ep.Key)
	}
	got := map[string]string{}
	ep.auth(func(k, v string) { got[k] = v })
	if len(got) != 0 {
		t.Fatalf("en-tête posé sans clé de preset : %v", got)
	}
}

// Un preset externe se reconnaît, un preset local ne se prend pas pour un externe.
func TestReconnaissanceDunPresetExterne(t *testing.T) {
	if !isExternalConfig(map[string]string{extKeyFlag: "1"}) {
		t.Error("EXTERNAL=1 non reconnu")
	}
	for _, v := range []string{"", "0", "off", "false"} {
		if isExternalConfig(map[string]string{extKeyFlag: v}) {
			t.Errorf("EXTERNAL=%q pris pour un preset externe", v)
		}
	}
	if isExternalConfig(parseEnv("MODEL=\"m.gguf\"\nCTX=4096\n")) {
		t.Error("un preset local est pris pour un externe")
	}
}

// La route de test rend un message LISIBLE plutôt que le JSON brut : c'est tout
// l'intérêt de tester avant d'enregistrer.
func TestRouteDeTestRemonteLErreurDeLAPI(t *testing.T) {
	testHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-bonne" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided"}}`))
			return
		}
		sendJSON(w, 200, map[string]any{"choices": []any{}})
	}))
	t.Cleanup(srv.Close)

	post := func(body string) map[string]any {
		w := httptest.NewRecorder()
		handlePresetExternalTest(w, httptest.NewRequest("POST", "/api/preset/external/test", strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	out := post(`{"url":"` + srv.URL + `","model":"m","key":"sk-mauvaise","keyTouched":true}`)
	if out["ok"] == true {
		t.Fatal("clé refusée annoncée comme valide")
	}
	if msg, _ := out["error"].(string); msg != "Incorrect API key provided" {
		t.Fatalf("message d'erreur = %q, attendu celui de l'API et non le JSON brut", msg)
	}
	if out = post(`{"url":"` + srv.URL + `","model":"m","key":"sk-bonne","keyTouched":true}`); out["ok"] != true {
		t.Fatalf("clé valide refusée : %v", out)
	}
}

// Rééditer un preset sans toucher au champ clé ne doit PAS l'effacer : la modale
// ne la reçoit jamais en clair, donc un champ vide n'est pas une demande
// d'effacement.
func TestEditionSansToucherALaCleLaConserve(t *testing.T) {
	testHome(t)
	id, err := SavePreset("", "Groq", externalPresetContent("https://api.groq.com/openai/v1", "llama", "sk-gardee", ""))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handlePresetExternalSave(w, httptest.NewRequest("POST", "/api/preset/external/save",
		strings.NewReader(`{"id":"`+id+`","name":"Groq","url":"https://api.groq.com/openai/v1","model":"llama-3.3","key":""}`)))
	if w.Code != 200 {
		t.Fatalf("code %d : %s", w.Code, w.Body.String())
	}
	content, err := ReadPreset(id)
	if err != nil {
		t.Fatal(err)
	}
	cfg := parseEnv(content)
	if cfg[extKeyToken] != "sk-gardee" {
		t.Fatalf("clé = %q — effacée par une édition qui n'y touchait pas", cfg[extKeyToken])
	}
	if cfg[extKeyModel] != "llama-3.3" {
		t.Fatalf("modèle non mis à jour : %q", cfg[extKeyModel])
	}
	// Et la clé ne ressort jamais en clair par la route de lecture.
	w = httptest.NewRecorder()
	handlePresetExternal(w, httptest.NewRequest("GET", "/api/preset/external?id="+id, nil))
	if strings.Contains(w.Body.String(), "sk-gardee") {
		t.Fatalf("la clé est renvoyée en clair : %s", w.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["hasKey"] != true {
		t.Fatal("hasKey doit signaler la présence d'une clé")
	}
}

// Un preset externe demande explicitement d'écraser la clé par une vide.
func TestEffacementExpliciteDeLaCle(t *testing.T) {
	testHome(t)
	id, err := SavePreset("", "Local distant", externalPresetContent("http://192.168.1.20:8080", "qwen", "sk-a-jeter", ""))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handlePresetExternalSave(w, httptest.NewRequest("POST", "/api/preset/external/save",
		strings.NewReader(`{"id":"`+id+`","name":"Local distant","url":"http://192.168.1.20:8080","model":"qwen","key":"","keyTouched":true}`)))
	if w.Code != 200 {
		t.Fatalf("code %d : %s", w.Code, w.Body.String())
	}
	content, _ := ReadPreset(id)
	if k := parseEnv(content)[extKeyToken]; k != "" {
		t.Fatalf("clé = %q, attendu effacée (keyTouched avec champ vide)", k)
	}
}
