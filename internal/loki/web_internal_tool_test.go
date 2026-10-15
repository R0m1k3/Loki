package loki

import (
	"encoding/json"
	"strings"
	"testing"
)

func appelOutil(t *testing.T, body string) map[string]any {
	t.Helper()
	w := appelOAIInterne(t, "POST", "/internal/v1/tool/exec", body)
	if w.Code != 200 {
		t.Fatalf("code %d : %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// Un outil exécuté par l'API interne rend exactement ce que la boucle Go
// aurait obtenu : même fonction (execTool), mêmes garde-fous.
func TestOutilInterneMemesResultats(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)

	got := appelOutil(t, `{"name":"outil_qui_n_existe_pas","args":{}}`)
	if got["result"] != "[erreur] outil inconnu: outil_qui_n_existe_pas" {
		t.Fatalf("outil inconnu : %v", got)
	}

	// Commande catastrophique : refusée AVANT exécution, comme dans la boucle.
	got = appelOutil(t, `{"name":"bash","args":{"command":"rm -rf /"},"caps":{"Agent":true}}`)
	res, _ := got["result"].(string)
	if want := refusedCommandResult(dangerousCommand("rm -rf /")); res != want {
		t.Fatalf("garde-fou bash : %q, attendu %q", res, want)
	}
	if got["label"] != "rm -rf /" {
		t.Fatalf("libellé : %v", got["label"])
	}

	// Question à l'utilisateur : l'événement remonte à l'appelant.
	got = appelOutil(t, `{"name":"ask","args":{"question":"On continue ?","options":["oui","non"]},"caps":{"Agent":true}}`)
	ask, _ := got["ask"].(map[string]any)
	if r, _ := got["result"].(string); strings.HasPrefix(r, "[erreur]") || ask["question"] != "On continue ?" {
		t.Fatalf("ask : %v", got)
	}
}

func TestOutilInterneAppelIllisible(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	for _, body := range []string{"", "{", `{"args":{}}`} {
		if w := appelOAIInterne(t, "POST", "/internal/v1/tool/exec", body); w.Code != 400 {
			t.Fatalf("%q : %d", body, w.Code)
		}
	}
	if w := appelOAIInterne(t, "GET", "/internal/v1/tool/exec", ""); w.Code != 405 {
		t.Fatalf("GET : %d", w.Code)
	}
}

// Ce que la boucle tire du résultat après l'exécution : capture de la bulle,
// image relayée au modèle — rien après une erreur.
func TestOutilInterneSuitesDuResultat(t *testing.T) {
	testHome(t)
	image, shot, relay := toolStepExtras("web_screenshot", "", toolExecResult{
		Result: "Capture : ![page](/api/chat/image?path=captures/a.png)",
	})
	if image != "captures/a.png" || shot != "" || relay != nil {
		t.Fatalf("capture : %q %q %v (vision coupée : pas de relais)", image, shot, relay)
	}
	img := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA=="}}
	_, _, relay = toolStepExtras("see_image", "photo.png", toolExecResult{Result: "ok", VisionImg: img})
	if relay == nil || relay.Role != "user" {
		t.Fatalf("see_image : %v", relay)
	}
	b, _ := json.Marshal(relay)
	if !strings.Contains(string(b), "Image demandée (photo.png)") || !strings.Contains(string(b), "data:image/png") {
		t.Fatalf("relais : %s", b)
	}
	if _, shot, _ := toolStepExtras("browser_click", "x", toolExecResult{Result: "[erreur] rien"}); shot != "" {
		t.Fatalf("aperçu après une erreur : %q", shot)
	}
}
