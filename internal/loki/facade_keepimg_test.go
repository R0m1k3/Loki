package loki

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// L'état que la façade demande au début d'un tour : vision réellement active
// (projecteur déclaré ET vu par le moteur) et budget des images gardées.
func TestFacadeKeepImgEtat(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	if err := SetConfigKey("CTX", "2000"); err != nil {
		t.Fatal(err)
	}
	etat := func() (bool, float64) {
		w := appelOAIInterne(t, "POST", "/internal/v1/turn/keepimg", `{}`)
		var out struct {
			Vision bool    `json:"vision"`
			Budget float64 `json:"budget"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
			t.Fatalf("état : %d %s", w.Code, w.Body.String())
		}
		return out.Vision, out.Budget
	}
	if v, b := etat(); v || b != 200 {
		t.Fatalf("sans projecteur : vision=%v budget=%v", v, b)
	}
	visionServer(t, "")
	if v, _ := etat(); !v {
		t.Fatal("projecteur vu par le moteur : vision attendue")
	}
	if w := appelOAIInterne(t, "GET", "/internal/v1/turn/keepimg", ""); w.Code != 405 {
		t.Fatalf("GET : %d", w.Code)
	}
}

// Une image relayée rangée par référence, relue à l'octet près ; sinon elle
// reste éphémère.
func TestFacadeKeepImgRangement(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	store := func(content string) map[string]any {
		w := appelOAIInterne(t, "POST", "/internal/v1/turn/keepimg/store", `{"msg":{"role":"user","content":`+content+`}}`)
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
			t.Fatalf("rangement : %d %s", w.Code, w.Body.String())
		}
		return out
	}
	out := store(`[{"type":"text","text":"Voici la capture demandée."},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]`)
	if out["ok"] != true || out["weight"] != float64(8) {
		t.Fatalf("rangée : %v", out)
	}
	msg := out["msg"].(map[string]any)
	parts := msg["content"].([]any)
	if parts[0].(map[string]any)["text"] != "Voici la capture demandée." {
		t.Fatalf("légende : %v", parts)
	}
	ref := parts[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(ref, imgRefScheme) {
		t.Fatalf("pas par référence : %s", ref)
	}
	if back, ok := chatImageDataURL(ref); !ok || back != "data:image/png;base64,iVBORw0KGgo=" {
		t.Fatalf("relecture : %v %.40s", ok, back)
	}
	if ents, _ := os.ReadDir(chatImgDir()); len(ents) != 1 {
		t.Fatalf("%d fichier(s) rangé(s)", len(ents))
	}
	for _, c := range []string{
		`"texte seul"`,
		`[{"type":"image_url","image_url":{"url":"https://exemple.org/a.png"}}]`,
		`[{"type":"image_url","image_url":{"url":"data:image/x-inconnu;base64,AAAA"}}]`,
		`[{"type":"image_url","image_url":{"url":"data:image/png;base64,pas du base64"}}]`,
		`[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]`,
	} {
		if out := store(c); out["ok"] != false {
			t.Errorf("%s : %v", c, out)
		}
	}
	if w := appelOAIInterne(t, "POST", "/internal/v1/turn/keepimg/store", `{`); w.Code != 400 {
		t.Fatalf("corps illisible : %d", w.Code)
	}
}
