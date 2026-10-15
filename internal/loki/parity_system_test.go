package loki

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// parity_system_test.go — journal de référence des interrupteurs système
// (lot 17 de la migration Rust) : accès internet et Crawl4AI (moteur, URL,
// clé jamais rendue, joignabilité et son cache), pilotage du navigateur et
// vision, clé de complétion (génération, saisie, effacement, repli sur
// config.env, adresse à coller dans un client tiers, refus pendant une
// optimisation) et écoute réseau du moteur. Après chaque appel : les clés
// touchées en base et les en-têtes reçus par les faux serveurs Crawl4AI.
// Rangé dans rust/loki-rs/tests/parity_system/system.json ; tests/system.rs
// le rejoue par la façade.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteSysteme

const paritySystemFile = "../../rust/loki-rs/tests/parity_system/system.json"

type paritySystemStep struct {
	Name    string            `json:"name"`
	Config  map[string]string `json:"config,omitempty"` // posé avant l'appel ; "" = supprimé
	State   map[string]string `json:"state,omitempty"`
	Tune    bool              `json:"tune,omitempty"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Body    string            `json:"body,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Status  int               `json:"status"`
	Out     any               `json:"out"`
	After   map[string]string `json:"after"`
	Probes  []string          `json:"probes"` // « chemin Authorization » reçus depuis l'appel précédent
}

// paritySystemKeys : ce que les routes touchent, relu après chaque appel.
var paritySystemKeys = []string{
	"config/WEB_ENGINE", "config/CRAWL4AI_URL", "config/CRAWL4AI_KEY", "config/API_KEY", "config/HOST",
	"state/internet", "state/computer", "state/crawl_key", "state/api_key", "state/public_url",
}

var (
	parityKeyRe    = regexp.MustCompile(`sk-loki-[0-9a-f]{48}`)
	parityMaskedRe = regexp.MustCompile(`sk-loki-[0-9a-f]…[0-9a-f]{4}`)
)

func TestPariteSysteme(t *testing.T) {
	home := testHome(t)
	t.Setenv("LOKI_CONTAINER", "1")
	t.Setenv("LOKI_CHROME", "/opt/faux/chrome")

	var mu sync.Mutex
	var probes []string
	fake := func(code int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			probes = append(probes, strings.TrimSpace(r.URL.Path+" "+r.Header.Get("Authorization")))
			mu.Unlock()
			w.WriteHeader(code)
		}))
	}
	ok, down, notFound := fake(200), fake(500), fake(404)
	defer ok.Close()
	defer down.Close()
	defer notFound.Close()

	mask := func(s string) string {
		s = strings.ReplaceAll(s, ok.URL, "{{OK}}")
		s = strings.ReplaceAll(s, down.URL, "{{DOWN}}")
		s = strings.ReplaceAll(s, notFound.URL, "{{NOTFOUND}}")
		s = strings.ReplaceAll(s, home, "{{HOME}}")
		if ip := localIP(); ip != "" {
			s = strings.ReplaceAll(s, "http://"+ip+":", "http://{{IP}}:")
		}
		s = parityKeyRe.ReplaceAllString(s, "{{KEY}}")
		return parityMaskedRe.ReplaceAllString(s, "{{MASKED}}")
	}
	unmask := func(s string) string {
		s = strings.ReplaceAll(s, "{{OK}}", ok.URL)
		s = strings.ReplaceAll(s, "{{DOWN}}", down.URL)
		return strings.ReplaceAll(s, "{{NOTFOUND}}", notFound.URL)
	}
	handlers := map[string]http.HandlerFunc{
		"/api/internet": handleInternet,
		"/api/computer": handleComputer,
		"/api/apikey":   handleAPIKey,
		"/api/network":  handleNetwork,
	}
	run := func(st paritySystemStep) paritySystemStep {
		for k, v := range st.Config {
			if err := SetConfigKey(k, unmask(v)); err != nil {
				t.Fatal(err)
			}
		}
		for k, v := range st.State {
			if err := putStr(bkState, k, unmask(v)); err != nil {
				t.Fatal(err)
			}
		}
		var lock *tuneLock
		if st.Tune {
			l, err := tuneLockAcquire("web", false)
			if err != nil {
				t.Fatal(err)
			}
			lock = l
		}
		mu.Lock()
		probes = nil
		mu.Unlock()
		req := httptest.NewRequest(st.Method, st.Path, strings.NewReader(unmask(st.Body)))
		req.Host = "loki.local:8090"
		for k, v := range st.Headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		handlers[st.Path](rec, req)
		if lock != nil {
			lock.release()
		}
		st.Status = rec.Code
		var out any
		if err := json.Unmarshal([]byte(mask(rec.Body.String())), &out); err != nil {
			t.Fatalf("%s : corps illisible %q", st.Name, rec.Body.String())
		}
		st.Out = out
		st.After = map[string]string{}
		for _, k := range paritySystemKeys {
			b, key, _ := strings.Cut(k, "/")
			if v := getStr(b, key); v != "" {
				st.After[k] = mask(v)
			}
		}
		mu.Lock()
		st.Probes = append([]string{}, probes...)
		mu.Unlock()
		for i := range st.Probes {
			st.Probes[i] = mask(st.Probes[i])
		}
		return st
	}

	var steps []paritySystemStep
	for _, st := range []paritySystemStep{
		// --- accès internet ---
		{Name: "internet par défaut", Method: "GET", Path: "/api/internet"},
		{Name: "allumé, moteur intégré", Method: "POST", Path: "/api/internet", Body: `{"enabled":true}`},
		{Name: "moteur crawl4ai sans URL", Method: "POST", Path: "/api/internet", Body: `{"engine":" CRAWL4AI "}`},
		{Name: "moteur inconnu", Method: "POST", Path: "/api/internet", Body: `{"engine":"bing"}`},
		{Name: "moteur inconnu vide", Method: "POST", Path: "/api/internet", Body: `{"engine":"  "}`},
		{Name: "URL joignable", Method: "POST", Path: "/api/internet", Body: `{"url":"  {{OK}}/// "}`},
		{Name: "relu dans le cache", Method: "GET", Path: "/api/internet"},
		{Name: "clé posée", Method: "POST", Path: "/api/internet", Body: `{"key":" secret-clé-crawl "}`},
		{Name: "clé courte", Method: "POST", Path: "/api/internet", Body: `{"key":"abcd"}`},
		{Name: "clé d'un caractère", Method: "POST", Path: "/api/internet", Body: `{"key":"é"}`},
		{Name: "clé résiduelle en config", Config: map[string]string{"CRAWL4AI_KEY": "vieille"},
			Method: "POST", Path: "/api/internet", Body: `{"key":"neuve12345"}`},
		{Name: "URL en panne", Method: "POST", Path: "/api/internet", Body: `{"url":"{{DOWN}}"}`},
		{Name: "URL 404", Method: "POST", Path: "/api/internet", Body: `{"url":"{{NOTFOUND}}"}`},
		{Name: "URL injoignable", Method: "POST", Path: "/api/internet", Body: `{"url":"http://127.0.0.1:1"}`},
		{Name: "URL sans schéma", Method: "POST", Path: "/api/internet", Body: `{"url":"exemple"}`},
		{Name: "type faux, le reste passe", Method: "POST", Path: "/api/internet", Body: `{"enabled":"oui","url":"{{OK}}"}`},
		{Name: "corps illisible", Method: "POST", Path: "/api/internet", Body: `{"enabled":false`},
		{Name: "éteint, clé effacée", Method: "POST", Path: "/api/internet", Body: `{"enabled":false,"key":""}`},
		{Name: "null ne touche à rien", Method: "POST", Path: "/api/internet", Body: `{"enabled":null,"url":null,"engine":null}`},
		{Name: "URL vidée, moteur choisi gardé", Method: "POST", Path: "/api/internet", Body: `{"url":"","enabled":true}`},
		{Name: "moteur go", Method: "POST", Path: "/api/internet", Body: `{"engine":"Go"}`},
		{Name: "moteur non choisi, URL héritée", Config: map[string]string{"WEB_ENGINE": "", "CRAWL4AI_URL": "{{OK}}"},
			Method: "GET", Path: "/api/internet"},
		{Name: "moteur illisible en config", Config: map[string]string{"WEB_ENGINE": "autre", "CRAWL4AI_URL": ""},
			Method: "GET", Path: "/api/internet"},
		{Name: "URL en config avec slashs", Config: map[string]string{"WEB_ENGINE": "crawl4ai", "CRAWL4AI_URL": " {{DOWN}}// "},
			Method: "GET", Path: "/api/internet"},
		{Name: "internet en PUT", Method: "PUT", Path: "/api/internet", Body: `{"enabled":true}`},

		// --- navigateur ---
		{Name: "navigateur par défaut", Method: "GET", Path: "/api/computer"},
		{Name: "navigateur allumé", Method: "POST", Path: "/api/computer", Body: `{"on":true}`},
		{Name: "navigateur, corps illisible", Method: "POST", Path: "/api/computer", Body: `oui`},
		{Name: "vision par projecteur", Config: map[string]string{"MMPROJ": " mmproj.gguf "},
			Method: "GET", Path: "/api/computer"},
		{Name: "vision externe", Config: map[string]string{"MMPROJ": "", "EXTERNAL": "1", "EXTERNAL_VISION": "1"},
			Method: "GET", Path: "/api/computer"},
		{Name: "externe sans vision", Config: map[string]string{"EXTERNAL_VISION": "0", "MMPROJ": "m.gguf"},
			Method: "GET", Path: "/api/computer"},
		{Name: "vision strata", Config: map[string]string{"EXTERNAL": "", "ENGINE": "strata"},
			Method: "GET", Path: "/api/computer"},
		{Name: "strata sans vision", Config: map[string]string{"STRATA_VISION": "0", "MMPROJ": ""},
			Method: "GET", Path: "/api/computer"},
		{Name: "navigateur éteint", Config: map[string]string{"ENGINE": "", "STRATA_VISION": ""},
			Method: "POST", Path: "/api/computer", Body: `{"on":false}`},

		// --- clé de complétion ---
		{Name: "clé par défaut", Method: "GET", Path: "/api/apikey"},
		{Name: "clé générée", Method: "POST", Path: "/api/apikey", Body: `{"action":"generate"}`},
		{Name: "clé relue", Method: "GET", Path: "/api/apikey", Headers: map[string]string{"X-Forwarded-Proto": "HTTPS, http"}},
		{Name: "clé saisie", Method: "POST", Path: "/api/apikey", Body: `{"action":"set","key":"  ma-clé-perso-é  "}`},
		{Name: "clé saisie courte", Method: "POST", Path: "/api/apikey", Body: `{"action":"set","key":"abc"}`},
		{Name: "clé de 13 octets", Method: "POST", Path: "/api/apikey", Body: `{"action":"set","key":"abcdefghijklm"}`},
		{Name: "clé effacée", Method: "POST", Path: "/api/apikey", Body: `{"action":"clear"}`},
		{Name: "repli sur config.env", Config: map[string]string{"API_KEY": "  résiduelle-config  "},
			Method: "GET", Path: "/api/apikey", Headers: map[string]string{"X-Forwarded-Proto": "ftp"}},
		{Name: "adresse publique", State: map[string]string{"public_url": "https://loki.exemple.fr"},
			Method: "POST", Path: "/api/apikey", Body: `{"action":"set","key":"k"}`},
		{Name: "action inconnue", Method: "POST", Path: "/api/apikey", Body: `{"action":"rotate"}`},
		{Name: "action vide", Method: "POST", Path: "/api/apikey", Body: `{}`},
		{Name: "corps vide", Method: "POST", Path: "/api/apikey", Body: ``},
		{Name: "corps illisible", Method: "POST", Path: "/api/apikey", Body: `{"action":`},
		{Name: "type faux", Method: "POST", Path: "/api/apikey", Body: `{"action":1}`},
		{Name: "optimisation en cours", Tune: true, Method: "POST", Path: "/api/apikey", Body: `{"action":"clear"}`},
		{Name: "lecture pendant une optimisation", Tune: true, Method: "GET", Path: "/api/apikey"},

		// --- réseau ---
		{Name: "réseau par défaut", Method: "GET", Path: "/api/network"},
		{Name: "réseau fermé", Method: "POST", Path: "/api/network", Body: `{"exposed":false}`},
		{Name: "réseau ouvert", Method: "POST", Path: "/api/network", Body: `{"exposed":true}`},
		{Name: "réseau sans champ", Method: "POST", Path: "/api/network", Body: `{}`},
		{Name: "réseau illisible", Method: "POST", Path: "/api/network", Body: `{"exposed":1}`},
		{Name: "réseau corps vide", Method: "POST", Path: "/api/network", Body: ``},
		{Name: "HOST localhost", Config: map[string]string{"HOST": " LocalHost ", "PORT": "9090"}, Method: "GET", Path: "/api/network"},
		{Name: "HOST ::1", Config: map[string]string{"HOST": "::1", "PORT": "abc"}, Method: "GET", Path: "/api/network"},
		{Name: "HOST d'une carte", Config: map[string]string{"HOST": "192.168.1.5", "PORT": ""}, Method: "GET", Path: "/api/network"},
	} {
		steps = append(steps, run(st))
	}

	// --- le Chromium de Playwright, le plus récent ---
	type chromiumCase struct {
		Files []string `json:"files"` // relatifs à la racine ; « / » final = dossier
		Out   string   `json:"out"`
	}
	var chromium []chromiumCase
	for _, files := range [][]string{
		{"chromium-1000/chrome-linux/chrome", "chromium-1200/chrome-linux/chrome"},
		{"chromium-1000/chrome-linux/chrome", "chromium-1200/chrome-linux/headless_shell"},
		{"chromium-1200/chrome-linux/headless_shell", "chromium-1200/chrome-linux/chrome"},
		{"chromium-1000/chrome-linux/chrome", "chromium_headless_shell-1100/chrome-linux/headless_shell"},
		{"chromium-1000/chrome-linux/chrome", "chromium-1300/chrome-linux/chrome/"},
		{"chromiumX/chrome-linux/chrome", "chromium-1/chrome-linux/chrome"},
		{"firefox-1/chrome-linux/chrome", "chromium-/chrome-linux/chrome"},
		{"chromium-9/chrome-linux/chrome", "chromium-10/chrome-linux/chrome"},
	} {
		root := t.TempDir()
		for _, f := range files {
			p := filepath.Join(root, f)
			if strings.HasSuffix(f, "/") {
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, nil, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("PLAYWRIGHT_BROWSERS_PATH", root)
		chromium = append(chromium, chromiumCase{Files: files,
			Out: strings.TrimPrefix(playwrightChromium(), root+"/")})
	}

	got, err := parityJSON(map[string]any{"steps": steps, "chromium": chromium})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(paritySystemFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paritySystemFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(paritySystemFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("les interrupteurs système ne produisent plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteSysteme,\n"+
			"puis cargo test --test system dit si le port Rust suit.\n--- obtenu ---\n%s", paritySystemFile, got)
	}
}
