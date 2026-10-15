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
