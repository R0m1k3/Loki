package loki

// Tests de la surface compatible OpenAI servie par Loki (mountOAI).
//
// Elle est joignable partout où l'interface l'est : c'est tout l'intérêt, et
// c'est aussi ce qui rend sa garde d'accès critique. Ces tests verrouillent les
// quatre promesses : la clé est exigée quand elle existe, l'amont n'est jamais
// appelé quand elle manque, l'en-tête du client arrive INTACT au moteur (qui la
// revalide), et rien d'autre que /v1 n'est monté.

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fauxMoteur monte un llama-server de comédie et pointe la configuration
// dessus : le proxy vise 127.0.0.1:LLMPort(), c'est donc tout ce qu'il faut.
// Renvoie le serveur et un pointeur sur les en-têtes de la dernière requête
// reçue (nil tant qu'il n'a rien reçu).
func fauxMoteur(t *testing.T, h http.HandlerFunc) (*httptest.Server, func() http.Header) {
	t.Helper()
	var vues http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vues = r.Header.Clone()
		if h != nil {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"loki"}]}`))
	}))
	t.Cleanup(srv.Close)
	port := srv.URL[strings.LastIndexByte(srv.URL, ':')+1:]
	if _, err := strconv.Atoi(port); err != nil {
		t.Fatalf("port du faux moteur illisible : %q", srv.URL)
	}
	if err := SetConfigKey("PORT", port); err != nil {
		t.Fatal(err)
	}
	return srv, func() http.Header { return vues }
}

// appelOAI joue une requête à travers la garde + le proxy, sans monter tout le
// serveur web (newWebMux a des effets de bord : MCP, migrations, conversation).
func appelOAI(t *testing.T, méthode, chemin, clé string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(méthode, chemin, nil)
	if clé != "" {
		req.Header.Set("Authorization", "Bearer "+clé)
	}
	rec := httptest.NewRecorder()
	requireCompletionKey(oaiHandler()).ServeHTTP(rec, req)
	return rec
}

// erreurOAI décode le corps au format d'erreur d'OpenAI — celui que les SDK
// savent présenter à l'utilisateur.
func erreurOAI(t *testing.T, rec *httptest.ResponseRecorder) (message, code string) {
	t.Helper()
	var body struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("corps illisible (%d) : %s", rec.Code, rec.Body.String())
	}
	return body.Error.Message, body.Error.Code
}

// Sans clé configurée, l'endpoint est ouvert — comme llama-server sans
// --api-key. Le panneau et le démarrage le disent en rouge ; ici on vérifie
// seulement que le comportement est bien celui-là, et pas un refus silencieux.
func TestOAISansCleLaissePasser(t *testing.T) {
	testHome(t)
	_, vues := fauxMoteur(t, nil)
	rec := appelOAI(t, "GET", "/v1/models", "")
	if rec.Code != 200 {
		t.Fatalf("code %d, corps %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"loki"`) {
		t.Errorf("réponse du moteur non relayée : %s", rec.Body.String())
	}
	if vues() == nil {
		t.Error("le moteur n'a pas été appelé")
	}
}

// La bonne clé passe, ET l'en-tête arrive INTACT au moteur : c'est le contrat.
// llama-server la valide lui aussi (--api-key), il reste donc protégé même si
// on l'expose un jour en direct.
func TestOAIBonneCleEtEnTeteRelayee(t *testing.T) {
	testHome(t)
	_, vues := fauxMoteur(t, nil)
	if err := writeAPIKey("sk-loki-test"); err != nil {
		t.Fatal(err)
	}
	rec := appelOAI(t, "GET", "/v1/models", "sk-loki-test")
	if rec.Code != 200 {
		t.Fatalf("code %d, corps %s", rec.Code, rec.Body.String())
	}
	if got := vues().Get("Authorization"); got != "Bearer sk-loki-test" {
		t.Errorf("en-tête transmis au moteur = %q, attendu la clé du client", got)
	}
}

// Mauvaise clé, et clé absente : 401 au format OpenAI, et le moteur n'est JAMAIS
// appelé — une garde qui laisse passer la requête avant de refuser ne protège
// rien.
func TestOAICleRefusee(t *testing.T) {
	for _, cas := range []struct{ nom, envoyée string }{
		{"mauvaise clé", "sk-loki-pasbonne"},
		{"aucune clé", ""},
	} {
		t.Run(cas.nom, func(t *testing.T) {
			testHome(t)
			_, vues := fauxMoteur(t, nil)
			if err := writeAPIKey("sk-loki-test"); err != nil {
				t.Fatal(err)
			}
			rec := appelOAI(t, "POST", "/v1/chat/completions", cas.envoyée)
			if rec.Code != 401 {
				t.Fatalf("code %d, attendu 401 — corps %s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("en-tête WWW-Authenticate absent")
			}
			msg, code := erreurOAI(t, rec)
			if code != "invalid_api_key" || msg == "" {
				t.Errorf("erreur = {code:%q, message:%q}", code, msg)
			}
			if vues() != nil {
				t.Error("le moteur a été appelé malgré le refus")
			}
		})
	}
}

// La clé résiduelle de config.env fait foi quand la base n'en a pas : c'est
// celle que le moteur exige (backend_serve.go). Sans ce miroir, Loki laissait
// passer et llama refusait — un 401 que rien n'expliquait.
func TestOAICleResiduelleDeConfig(t *testing.T) {
	testHome(t)
	_, vues := fauxMoteur(t, nil)
	if err := SetConfigKey("API_KEY", "sk-ancienne"); err != nil {
		t.Fatal(err)
	}
	if rec := appelOAI(t, "GET", "/v1/models", ""); rec.Code != 401 {
		t.Fatalf("sans clé : code %d, attendu 401", rec.Code)
	}
	if vues() != nil {
		t.Fatal("le moteur a été appelé malgré le refus")
	}
	if rec := appelOAI(t, "GET", "/v1/models", "sk-ancienne"); rec.Code != 200 {
		t.Fatalf("avec la clé de config.env : code %d, attendu 200", rec.Code)
	}
}

// Un préflight CORS ne porte jamais d'Authorization. Le refuser casserait tout
// client tiers qui tourne dans un navigateur.
func TestOAIPreflightPasseSansCle(t *testing.T) {
	testHome(t)
	_, vues := fauxMoteur(t, nil)
	if err := writeAPIKey("sk-loki-test"); err != nil {
		t.Fatal(err)
	}
	if rec := appelOAI(t, "OPTIONS", "/v1/chat/completions", ""); rec.Code == 401 {
		t.Fatalf("préflight refusé : %s", rec.Body.String())
	}
	if vues() == nil {
		t.Error("le préflight n'a pas atteint le moteur, qui répond ses en-têtes CORS")
	}
}

// Seul /v1/ est monté. /metrics, /props et /slots divulgueraient le modèle
// chargé et l'état du moteur — sans clé, à qui joint l'adresse.
func TestOAIMonteSeulementV1(t *testing.T) {
	testHome(t)
	mux := http.NewServeMux()
	mountOAI(mux)
	témoin := false
	mux.HandleFunc("/", func(http.ResponseWriter, *http.Request) { témoin = true })

	for _, chemin := range []string{"/metrics", "/props", "/slots", "/api/ping", "/"} {
		témoin = false
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", chemin, nil))
		if !témoin {
			t.Errorf("%s n'a pas été servi par le handler racine — il est passé à l'endpoint OpenAI", chemin)
		}
	}
	// …et /v1/, lui, ne va PAS au handler racine.
	témoin = false
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if témoin {
		t.Error("/v1/models a été servi par le handler racine")
	}
}

// Le streaming doit arriver en morceaux. Si FlushInterval régresse, le premier
// événement n'arrive qu'à la fin de la génération — invisible en test unitaire
// classique, d'où ce test qui bloque l'amont entre deux écritures.
func TestOAIStreamingArriveEnMorceaux(t *testing.T) {
	testHome(t)
	débloque := make(chan struct{})
	fauxMoteur(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("le faux moteur ne sait pas vider son tampon")
			return
		}
		_, _ = w.Write([]byte("data: un\n\n"))
		fl.Flush()
		<-débloque
		_, _ = w.Write([]byte("data: deux\n\n"))
		fl.Flush()
	})

	front := httptest.NewServer(requireCompletionKey(oaiHandler()))
	defer front.Close()
	defer close(débloque)

	resp, err := http.Get(front.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	lu := make(chan string, 1)
	go func() {
		ligne, _ := bufio.NewReader(resp.Body).ReadString('\n')
		lu <- ligne
	}()
	select {
	case ligne := <-lu:
		if !strings.Contains(ligne, "un") {
			t.Fatalf("première ligne = %q", ligne)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rien reçu avant la fin de la génération : le flux est bufferisé")
	}
}
