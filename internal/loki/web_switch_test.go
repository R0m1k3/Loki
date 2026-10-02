package loki

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// La bascule vise le preset par son id, pas par sa position : une liste
// réordonnée sur un autre appareil faisait charger un AUTRE preset. Presets
// externes ici : la bascule n'a alors aucun moteur local à redémarrer.
func TestBasculeParIdentifiant(t *testing.T) {
	testHome(t)
	if _, err := SavePreset("", "Alpha", externalPresetContent("https://a.example/v1", "modele-a", "", "", false)); err != nil {
		t.Fatal(err)
	}
	idB, err := SavePreset("", "Beta", externalPresetContent("https://b.example/v1", "modele-b", "", "", false))
	if err != nil {
		t.Fatal(err)
	}
	post := func(body string) int {
		w := httptest.NewRecorder()
		handleSwitch(w, httptest.NewRequest("POST", "/api/switch", strings.NewReader(body)))
		return w.Code
	}
	if code := post(`{"id":"` + idB + `","n":1}`); code != 200 {
		t.Fatalf("bascule par id : code %d", code)
	}
	if got := ReadConfig()[extKeyModel]; got != "modele-b" {
		t.Fatalf("preset chargé = %q, attendu celui de l'id (modele-b) et non la position 1", got)
	}
	if code := post(`{"id":"inexistant"}`); code != 404 {
		t.Fatalf("id inconnu : code %d, attendu 404", code)
	}
}
