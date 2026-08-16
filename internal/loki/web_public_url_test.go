package loki

// L'adresse annoncée dans le panneau « Accès OpenAI » est ce qu'on recopie dans
// la configuration d'un autre logiciel : une valeur fausse ne se voit pas, elle
// se paie en « ça ne marche pas » sans indice. D'où ces tests.

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestNormalisePublicURL(t *testing.T) {
	for _, c := range []struct {
		saisie, veut string
		refus        bool
	}{
		// Un nom nu vaut https : personne ne tape le schéma.
		{saisie: "ia.exemple.fr", veut: "https://ia.exemple.fr"},
		{saisie: "  ia.exemple.fr  ", veut: "https://ia.exemple.fr"},
		{saisie: "http://192.168.1.20:8090", veut: "http://192.168.1.20:8090"},
		{saisie: "192.168.1.20:8090", veut: "https://192.168.1.20:8090"},
		{saisie: "HTTPS://Exemple.fr/", veut: "https://Exemple.fr"},
		// Le « /v1 » recopié depuis l'adresse affichée est toléré et retiré :
		// c'est le geste naturel, pas une faute.
		{saisie: "https://ia.exemple.fr/v1", veut: "https://ia.exemple.fr"},
		{saisie: "https://ia.exemple.fr/v1/", veut: "https://ia.exemple.fr"},
		{saisie: "[::1]:8090", veut: "https://[::1]:8090"},
		// Vide = effacement, pas erreur.
		{saisie: "   ", veut: ""},
		// Refus, chacun pour une raison qu'on veut pouvoir expliquer.
		{saisie: "ftp://ia.exemple.fr", refus: true},
		{saisie: "https://ia.exemple.fr/base", refus: true},
		{saisie: "https://user:mdp@ia.exemple.fr", refus: true},
		{saisie: "https://ia.exemple.fr?x=1", refus: true},
		{saisie: "https://ia.exemple.fr:port", refus: true},
	} {
		got, err := normalizePublicURL(c.saisie)
		if c.refus {
			if err == nil {
				t.Errorf("%q accepté → %q, refus attendu", c.saisie, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q refusé : %v", c.saisie, err)
			continue
		}
		if got != c.veut {
			t.Errorf("%q → %q, attendu %q", c.saisie, got, c.veut)
		}
	}
}

// L'origine de la requête : le domaine réellement tapé, et le schéma que le
// reverse proxy annonce. Sans X-Forwarded-Proto, une interface servie en https
// afficherait une adresse en http — inutilisable telle quelle.
func TestRequestBase(t *testing.T) {
	for _, c := range []struct{ nom, hôte, proto, veut string }{
		{"ip et port", "192.168.1.20:8090", "", "http://192.168.1.20:8090"},
		{"domaine derrière un proxy TLS", "ia.exemple.fr", "https", "https://ia.exemple.fr"},
		{"chaîne de proxys", "ia.exemple.fr", "https, http", "https://ia.exemple.fr"},
		{"en-tête douteux ignoré", "ia.exemple.fr", "gopher", "http://ia.exemple.fr"},
	} {
		t.Run(c.nom, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://placeholder/api/apikey", nil)
			r.Host = c.hôte
			if c.proto != "" {
				r.Header.Set("X-Forwarded-Proto", c.proto)
			}
			if got := requestBase(r); got != c.veut {
				t.Errorf("requestBase = %q, attendu %q", got, c.veut)
			}
		})
	}
}

// L'adresse enregistrée l'emporte sur celle du navigateur : c'est tout l'objet
// du champ, pour le cas du reverse proxy où Loki ne voit qu'un appel interne.
func TestAdresseAffichee(t *testing.T) {
	testHome(t)

	lire := func() map[string]any {
		r := httptest.NewRequest("GET", "http://placeholder/api/apikey", nil)
		r.Host = "ia.exemple.fr"
		r.Header.Set("X-Forwarded-Proto", "https")
		rec := httptest.NewRecorder()
		handleAPIKey(rec, r) // GET seulement : le POST redémarre le moteur
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("réponse illisible (%d) : %s", rec.Code, rec.Body.String())
		}
		return out
	}

	if got := lire()["url"]; got != "https://ia.exemple.fr/v1" {
		t.Errorf("sans adresse enregistrée : url = %v", got)
	}
	if _, err := setPublicURL("autre.exemple.fr"); err != nil {
		t.Fatal(err)
	}
	out := lire()
	if out["url"] != "https://autre.exemple.fr/v1" {
		t.Errorf("avec adresse enregistrée : url = %v", out["url"])
	}
	if out["public_url"] != "https://autre.exemple.fr" {
		t.Errorf("public_url = %v", out["public_url"])
	}
	// Effacement : on repasse sur l'origine de la requête.
	if _, err := setPublicURL(""); err != nil {
		t.Fatal(err)
	}
	if got := lire()["url"]; got != "https://ia.exemple.fr/v1" {
		t.Errorf("après effacement : url = %v", got)
	}
}
