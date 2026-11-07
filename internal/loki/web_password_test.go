package loki

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func authCall(t *testing.T, method, path, body, bearer string) (int, map[string]any) {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Host = "localhost:8090"
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	handleAuth(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func resetLoginLimiter(t *testing.T) {
	t.Helper()
	webLogin.Lock()
	webLogin.fails, webLogin.until = 0, time.Time{}
	webLogin.Unlock()
	t.Cleanup(func() {
		webLogin.Lock()
		webLogin.fails, webLogin.until = 0, time.Time{}
		webLogin.Unlock()
	})
}

// Installation neuve : l'UI propose de choisir le mot de passe, qui renvoie la
// clé de pilotage ; ensuite le mot de passe est demandé, et un second « setup »
// est refusé (personne ne peut remplacer le mot de passe depuis l'UI).
func TestMotDePassePremierDemarrage(t *testing.T) {
	testHome(t)
	resetLoginLimiter(t)
	if _, err := ensureWebKey(); err != nil {
		t.Fatal(err)
	}
	key := readWebKey()
	if c, out := authCall(t, "GET", "/api/auth/state", "", ""); c != 200 || out["state"] != "setup" {
		t.Fatalf("state = %d %v ; attendu setup", c, out)
	}
	if c, _ := authCall(t, "POST", "/api/auth/setup", `{"password":"court"}`, ""); c != 400 {
		t.Fatalf("mot de passe court : %d ; attendu 400", c)
	}
	c, out := authCall(t, "POST", "/api/auth/setup", `{"password":"mon-mot-de-passe"}`, "")
	if c != 200 || out["token"] != key {
		t.Fatalf("setup = %d %v ; attendu la clé %q", c, out, key)
	}
	if c, _ := authCall(t, "POST", "/api/auth/setup", `{"password":"autre-mot-de-passe"}`, ""); c != 409 {
		t.Fatalf("second setup : %d ; attendu 409", c)
	}
	if c, out := authCall(t, "GET", "/api/auth/state", "", ""); out["state"] != "login" {
		t.Fatalf("state sans clé = %d %v ; attendu login", c, out)
	}
	if _, out := authCall(t, "GET", "/api/auth/state", "", key); out["state"] != "ok" {
		t.Fatalf("state avec la clé = %v ; attendu ok", out)
	}
	if c, out := authCall(t, "POST", "/api/auth/login", `{"password":"mon-mot-de-passe"}`, ""); c != 200 || out["token"] != key {
		t.Fatalf("login = %d %v", c, out)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"password":"faux-mot-de-passe"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "localhost"
	handleAuth(rec, req)
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("mauvais mot de passe : %d, WWW-Authenticate=%q ; attendu 401 sans en-tête", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

// Installation existante (clé déjà en place, pas de mot de passe) : la clé est
// gardée telle quelle — les navigateurs et scripts qui l'ont continuent.
func TestMotDePasseGardeLaCleExistante(t *testing.T) {
	testHome(t)
	resetLoginLimiter(t)
	if err := putStr(bkState, "web_key", "ancienne-cle"); err != nil {
		t.Fatal(err)
	}
	if _, out := authCall(t, "GET", "/api/auth/state", "", "ancienne-cle"); out["state"] != "setup" {
		t.Fatalf("state = %v ; attendu setup même avec la clé", out)
	}
	if c, out := authCall(t, "POST", "/api/auth/setup", `{"password":"mon-mot-de-passe"}`, ""); c != 200 || out["token"] != "ancienne-cle" {
		t.Fatalf("setup = %d %v", c, out)
	}
}

// Clé retirée exprès (set-web-key off) : rien à demander tant qu'il n'y a pas de
// mot de passe ; en choisir un régénère une clé et lève le retrait.
func TestMotDePasseApresRetraitDeLaCle(t *testing.T) {
	testHome(t)
	resetLoginLimiter(t)
	if err := cmdSetWebKey([]string{"off"}); err != nil {
		t.Fatal(err)
	}
	if _, out := authCall(t, "GET", "/api/auth/state", "", ""); out["state"] != "ok" {
		t.Fatalf("state = %v ; attendu ok (API ouverte exprès)", out)
	}
	c, out := authCall(t, "POST", "/api/auth/setup", `{"password":"mon-mot-de-passe"}`, "")
	tok, _ := out["token"].(string)
	if c != 200 || !strings.HasPrefix(tok, "loki-web-") || readWebKey() != tok {
		t.Fatalf("setup = %d %v ; clé = %q", c, out, readWebKey())
	}
	if getStr(bkState, webKeyOpenFlag) != "" {
		t.Fatal("le retrait explicite de la clé aurait dû être levé")
	}
}

// « loki password » (setWebPassword sans condition) remplace un mot de passe
// perdu ; l'ancien ne passe plus.
func TestMotDePasseRemplaceParLaCLI(t *testing.T) {
	testHome(t)
	resetLoginLimiter(t)
	if _, err := setWebPassword("premier-mot", true); err != nil {
		t.Fatal(err)
	}
	if _, err := setWebPassword("second-mot", false); err != nil {
		t.Fatal(err)
	}
	if c, _ := authCall(t, "POST", "/api/auth/login", `{"password":"premier-mot"}`, ""); c != 401 {
		t.Fatalf("ancien mot de passe : %d ; attendu 401", c)
	}
	if c, _ := authCall(t, "POST", "/api/auth/login", `{"password":"second-mot"}`, ""); c != 200 {
		t.Fatalf("nouveau mot de passe : %d ; attendu 200", c)
	}
}

// Après 5 échecs, les essais sont freinés (429), même avec le bon mot de passe.
func TestMotDePasseFreineLesEssais(t *testing.T) {
	testHome(t)
	resetLoginLimiter(t)
	if _, err := setWebPassword("le-bon-mot", true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < webLoginFreeFails+1; i++ {
		if c, _ := authCall(t, "POST", "/api/auth/login", `{"password":"mauvais"}`, ""); c != 401 {
			t.Fatalf("essai %d : %d ; attendu 401", i+1, c)
		}
	}
	if c, _ := authCall(t, "POST", "/api/auth/login", `{"password":"le-bon-mot"}`, ""); c != 429 {
		t.Fatalf("après %d échecs : %d ; attendu 429", webLoginFreeFails+1, c)
	}
}

// Pas de formulaire d'un site tiers : JSON obligatoire.
func TestMotDePasseExigeDuJSON(t *testing.T) {
	testHome(t)
	req := httptest.NewRequest("POST", "/api/auth/setup", strings.NewReader(`{"password":"mon-mot-de-passe"}`))
	req.Header.Set("Content-Type", "text/plain")
	req.Host = "localhost"
	rec := httptest.NewRecorder()
	handleAuth(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain : %d ; attendu 415", rec.Code)
	}
}

// X-Loki-Via forgé sur l'écoute directe : effacé, il ne dispense plus du contrôle
// d'hôte.
func TestEcouteDirecteEffaceLeMarqueurTunnel(t *testing.T) {
	var got string
	h := stripTunnelMark(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(viaTunnelHeader)
	}))
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.Header.Set(viaTunnelHeader, "tunnel")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "" {
		t.Fatalf("X-Loki-Via = %q ; attendu effacé", got)
	}
}
