package loki

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Une base vide donne les délais par défaut : 20 s de connexion, 120 s
// d'appel.
func TestMCPTimeoutsDefautsSansConfig(t *testing.T) {
	testHome(t)
	got := LoadMCPTimeouts()
	if got != (MCPTimeouts{Connect: 20, Call: 120}) {
		t.Fatalf("défauts = %+v", got)
	}
	if d := mcpTimeoutFor(MCPServerConfig{Command: "node"}, got); d != 20*time.Second {
		t.Errorf("connexion = %s", d)
	}
	if d := mcpCallTimeoutFor(MCPServerConfig{}, got, nil); d != 120*time.Second {
		t.Errorf("appel = %s", d)
	}
}

// Enregistrés puis relus ; une valeur nulle retombe sur son défaut, une
// valeur trop grande (base modifiée à la main) sur la borne, une valeur
// illisible sur les défauts.
func TestMCPTimeoutsAllerRetour(t *testing.T) {
	testHome(t)
	if err := SaveMCPTimeouts(MCPTimeouts{Connect: 5, Call: 300}); err != nil {
		t.Fatal(err)
	}
	if got := LoadMCPTimeouts(); got != (MCPTimeouts{Connect: 5, Call: 300}) {
		t.Fatalf("relecture = %+v", got)
	}
	for raw, want := range map[string]MCPTimeouts{
		`{"connect":0,"call":0}`:         {20, 120},
		`{"connect":-4,"call":99999}`:    {20, 3600},
		`{"connect":"x","call":50}`:      {20, 120},
		`{"CONNECT":7}`:                  {7, 120},
		`null`:                           {20, 120},
		`{"connect":30,"call":40} reste`: {20, 120},
	} {
		if err := putStr(bkState, mcpTimeoutsKey, raw); err != nil {
			t.Fatal(err)
		}
		if got := LoadMCPTimeouts(); got != want {
			t.Errorf("%s : %+v, attendu %+v", raw, got, want)
		}
	}
}

// Un lanceur npx/uvx garde au moins mcpConnectTimeoutCold ; un délai réglé
// plus long l'emporte.
func TestMCPTimeoutForLanceurFroid(t *testing.T) {
	npx := MCPServerConfig{Command: "npx"}
	if d := mcpTimeoutFor(npx, MCPTimeouts{Connect: 20, Call: 120}); d != mcpConnectTimeoutCold {
		t.Errorf("npx, 20 s : %s", d)
	}
	if d := mcpTimeoutFor(npx, MCPTimeouts{Connect: 600, Call: 120}); d != 10*time.Minute {
		t.Errorf("npx, 600 s : %s", d)
	}
	if d := mcpTimeoutFor(MCPServerConfig{URL: "http://x"}, MCPTimeouts{Connect: 45, Call: 120}); d != 45*time.Second {
		t.Errorf("http, 45 s : %s", d)
	}
}

func mcpRoute(t *testing.T, h func(w *httptest.ResponseRecorder), want int) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	h(w)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("réponse %q : %v", w.Body.String(), err)
	}
	if w.Code != want {
		t.Fatalf("code %d, attendu %d : %s", w.Code, want, w.Body.String())
	}
	return out
}

// Les routes des délais globaux : lecture, refus hors bornes, enregistrement
// qui ferme les sessions ouvertes.
func TestMCPTimeoutsRoutes(t *testing.T) {
	testHome(t)
	t.Setenv(facadeMCPEnv, "")
	get := func() map[string]any {
		return mcpRoute(t, func(w *httptest.ResponseRecorder) {
			handleMCPTimeouts(w, httptest.NewRequest("GET", "/api/mcp/timeouts", nil))
		}, 200)
	}
	post := func(body string, want int) map[string]any {
		return mcpRoute(t, func(w *httptest.ResponseRecorder) {
			handleMCPTimeoutsSave(w, httptest.NewRequest("POST", "/api/mcp/timeouts/save", strings.NewReader(body)))
		}, want)
	}
	if v := get(); v["connect"] != 20.0 || v["call"] != 120.0 {
		t.Fatalf("GET : %v", v)
	}
	for body, msg := range map[string]string{
		`{"connect":0,"call":120}`:    "connexion : 1 à 3600 s",
		`{"connect":20,"call":3601}`:  "appel d'outil : 1 à 3600 s",
		`{"connect":20}`:              "appel d'outil : 1 à 3600 s",
		`{"connect":"20","call":120}`: "json: cannot unmarshal string into Go struct field MCPTimeouts.connect of type int",
	} {
		if v := post(body, 400); v["error"] != msg {
			t.Errorf("%s : %v", body, v["error"])
		}
	}
	// Une session ouverte sur un serveur configuré : fermée à l'enregistrement.
	if err := SetMCPServer("lent", MCPServerConfig{URL: "http://127.0.0.1:1/mcp", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	fakeMCPSlow(t)
	if v := post(`{"connect":30,"call":600}`, 200); v["connect"] != 30.0 || v["call"] != 600.0 {
		t.Fatalf("POST : %v", v)
	}
	mcpMgr.mu.Lock()
	_, open := mcpMgr.sessions["lent"]
	mcpMgr.mu.Unlock()
	if open {
		t.Error("la session est restée ouverte")
	}
	if v := get(); v["connect"] != 30.0 || v["call"] != 600.0 {
		t.Fatalf("GET après enregistrement : %v", v)
	}
	if got := getStr(bkState, mcpTimeoutsKey); got != `{"connect":30,"call":600}` {
		t.Errorf("stocké : %s", got)
	}
}

// Le délai d'un serveur passe par /api/mcp/save, revient dans /api/mcp et
// survit à un nouvel enregistrement sans lui (0 = réglage global).
func TestMCPSaveCallTimeoutSec(t *testing.T) {
	testHome(t)
	t.Setenv(facadeMCPEnv, "")
	t.Cleanup(mcpCloseAll)
	save := func(body string, want int) map[string]any {
		return mcpRoute(t, func(w *httptest.ResponseRecorder) {
			handleMCPSave(w, httptest.NewRequest("POST", "/api/mcp/save", strings.NewReader(body)))
		}, want)
	}
	v := save(`{"name":"long","url":"http://127.0.0.1:1/mcp","enabled":false,"callTimeoutSec":900}`, 200)
	srv := v["servers"].([]any)[0].(map[string]any)
	if srv["callTimeoutSec"] != 900.0 {
		t.Fatalf("état : %v", srv)
	}
	servers, _ := LoadMCPConfig()
	if servers["long"].CallTimeoutSec != 900 {
		t.Fatalf("config : %+v", servers["long"])
	}
	st, _ := MCPStatus()
	if len(st) != 1 || st[0].CallTimeoutSec != 900 {
		t.Fatalf("MCPStatus : %+v", st)
	}
	// Champ vidé dans l'UI : retour au réglage global, clé absente.
	v = save(`{"name":"long","url":"http://127.0.0.1:1/mcp","enabled":false}`, 200)
	if _, ok := v["servers"].([]any)[0].(map[string]any)["callTimeoutSec"]; ok {
		t.Fatalf("callTimeoutSec resté : %v", v)
	}
	for _, body := range []string{
		`{"name":"x","url":"http://h","callTimeoutSec":-1}`,
		`{"name":"x","url":"http://h","callTimeoutSec":3601}`,
	} {
		if v := save(body, 400); v["error"] != "délai d'un appel : 0 à 3600 s (vide = réglage global)" {
			t.Errorf("%s : %v", body, v["error"])
		}
	}
}
