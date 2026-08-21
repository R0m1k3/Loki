package loki

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigRouteAllerRetour(t *testing.T) {
	testHome(t)
	corps := strings.NewReader(`{"model":"small-q5_1","lang":"en","device":"1","reactivity":"court"}`)
	rec := httptest.NewRecorder()
	handleDictateConfig(rec, httptest.NewRequest("POST", "/api/dictate/config", corps))
	if rec.Code != 200 {
		t.Fatalf("POST = %d, corps %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handleDictateConfig(rec, httptest.NewRequest("GET", "/api/dictate/config", nil))
	var got DictateCfg
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("réponse illisible : %v", err)
	}
	if got.Device != "1" || got.Lang != "en" || got.Model != "small-q5_1" || got.Reactivity != "court" {
		t.Errorf("relu %+v, attendu le réglage enregistré", got)
	}
}

// Le refus doit venir de la ROUTE, pas seulement du magasin : sinon l'UI reçoit
// un 200 et croit son réglage pris en compte.
func TestConfigRouteRefuseModeleInconnu(t *testing.T) {
	testHome(t)
	corps := strings.NewReader(`{"model":"nawak","lang":"fr","device":"cpu","reactivity":"moyen"}`)
	rec := httptest.NewRecorder()
	handleDictateConfig(rec, httptest.NewRequest("POST", "/api/dictate/config", corps))
	if rec.Code == 200 {
		t.Errorf("modèle hors catalogue accepté (%d) : %s", rec.Code, rec.Body.String())
	}
}

func TestConfigRouteCorpsIllisible(t *testing.T) {
	testHome(t)
	rec := httptest.NewRecorder()
	handleDictateConfig(rec, httptest.NewRequest("POST", "/api/dictate/config", strings.NewReader("pas du json")))
	if rec.Code != 400 {
		t.Errorf("code = %d, attendu 400", rec.Code)
	}
}

func TestModelsRoute(t *testing.T) {
	testHome(t)
	rec := httptest.NewRecorder()
	handleDictateModels(rec, httptest.NewRequest("GET", "/api/dictate/models", nil))
	if rec.Code != 200 {
		t.Fatalf("GET = %d", rec.Code)
	}
	var l []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatalf("réponse illisible : %v", err)
	}
	if len(l) < 4 {
		t.Errorf("%d modèles renvoyés, au moins 4 attendus", len(l))
	}
}

// Un identifiant inconnu ne doit pas lancer de téléchargement : whisperDownload
// bâtirait une URL vide et le message d'erreur n'arriverait qu'en arrière-plan.
func TestDownloadRouteRefuseInconnu(t *testing.T) {
	testHome(t)
	rec := httptest.NewRecorder()
	corps := strings.NewReader(`{"id":"nawak"}`)
	handleDictateDownload(rec, httptest.NewRequest("POST", "/api/dictate/models/download", corps))
	if rec.Code != 400 {
		t.Errorf("code = %d, attendu 400 pour un modèle inconnu", rec.Code)
	}
}

func TestStateRoute(t *testing.T) {
	testHome(t)
	rec := httptest.NewRecorder()
	handleDictateState(rec, httptest.NewRequest("GET", "/api/dictate/state", nil))
	if rec.Code != 200 {
		t.Fatalf("GET = %d", rec.Code)
	}
	var e map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("réponse illisible : %v", err)
	}
	for _, clef := range []string{"actif", "modele", "present", "device", "dl"} {
		if _, ok := e[clef]; !ok {
			t.Errorf("clé %q manquante dans l'état", clef)
		}
	}
}

// Un audio trop court est rejeté AVANT toute tentative de lancer whisper-server :
// inutile de charger un modèle pour un clic parasite.
func TestTranscribeAudioTropCourt(t *testing.T) {
	testHome(t)
	rec := httptest.NewRecorder()
	handleTranscribe(rec, httptest.NewRequest("POST", "/api/transcribe", strings.NewReader("court")))
	if rec.Code != 400 {
		t.Errorf("code = %d, attendu 400", rec.Code)
	}
}
