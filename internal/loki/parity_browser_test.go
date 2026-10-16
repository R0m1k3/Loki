//go:build linux

package loki

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// parity_browser_test.go — journal de référence du navigateur piloté (lot 44
// de la migration Rust) : les outils browser_*, de bout en bout contre un faux
// Chrome. Un script tient lieu de binaire (il annonce le port d'un faux
// serveur CDP dans DevToolsActivePort) ; le faux CDP répond selon un scénario
// réglé à chaque étape et note chaque commande reçue (méthode, paramètres,
// scripts résumés par leur empreinte). Chaque étape porte le résultat, les
// commandes, et pour une capture ses dimensions et quelques pixels de la
// grille. Rangé dans rust/loki-rs/tests/parity_browser/browser.json ;
// tests/browser.rs le rejoue contre le port.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteNavigateur

const parityBrowserFile = "../../rust/loki-rs/tests/parity_browser/browser.json"

// parityFakeChrome : le faux navigateur. Il annonce le port du faux CDP,
// range ses arguments, et attend.
const parityFakeChrome = `#!/bin/sh
for a in "$@"; do
  case "$a" in
    --user-data-dir=*) dir="${a#--user-data-dir=}" ;;
  esac
done
printf '%s\n/devtools/browser/faux\n' "$FAKE_CDP_PORT" > "$dir/DevToolsActivePort"
printf '%s\n' "$@" > "$FAKE_CDP_ARGS"
exec sleep 600
`

// parityCDPScene : ce que le faux CDP répond (réglé étape par étape).
type parityCDPScene struct {
	Snap     any               `json:"snap,omitempty"`   // valeur du script de snapshot ; "<absent>" : sans valeur
	Find     any               `json:"find,omitempty"`   // valeur de browser_find
	Center   any               `json:"center,omitempty"` // valeur de refCenter
	Typed    any               `json:"typed,omitempty"`  // valeur de typeInto
	Href     string            `json:"href,omitempty"`
	NavError string            `json:"nav_error,omitempty"`
	Throw    bool              `json:"throw,omitempty"`  // les scripts lèvent une exception
	Errors   map[string]string `json:"errors,omitempty"` // méthode → message d'erreur CDP
}

type parityBrowserStep struct {
	Name   string          `json:"name"`
	Scene  *parityCDPScene `json:"scene,omitempty"` // remplace le scénario courant
	Vision bool            `json:"vision,omitempty"`
	Tool   string          `json:"tool"`
	Args   map[string]any  `json:"args"`
	Result string          `json:"result"`
	Image  map[string]any  `json:"image,omitempty"` // dimensions et pixels de la capture du modèle
	Saved  []string        `json:"saved"`           // captures rangées dans le dossier de travail
	Calls  []string        `json:"calls"`
}

// parityCDPAbbrev résume un script : début, longueur, empreinte.
func parityCDPAbbrev(s string) string {
	if len(s) <= 60 {
		return s
	}
	h := sha256.Sum256([]byte(s))
	r := []rune(s)
	head := string(r[:40])
	return fmt.Sprintf("%s…[%d:%s]", head, len(s), hex.EncodeToString(h[:6]))
}

// parityCDPCall : une commande reçue, en une ligne stable.
func parityCDPCall(method string, params map[string]any) string {
	if params == nil {
		return method
	}
	p := map[string]any{}
	for k, v := range params {
		if s, ok := v.(string); ok && (k == "expression" || k == "source") {
			v = parityCDPAbbrev(s)
		}
		p[k] = v
	}
	b, _ := json.Marshal(p)
	return method + " " + string(b)
}

// parityPNG : une capture unie (la grille s'y lit sans ambiguïté).
func parityPNG(w, h int) string {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 40, G: 80, B: 120, A: 255})
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

// parityImageFacts : dimensions et pixels témoins d'une image relayée.
func parityImageFacts(t *testing.T, part map[string]any) map[string]any {
	u, _ := part["image_url"].(map[string]any)["url"].(string)
	const pre = "data:image/png;base64,"
	if !strings.HasPrefix(u, pre) {
		t.Fatalf("image relayée : %.60s", u)
	}
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(u, pre))
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	px := map[string]string{}
	for _, p := range [][2]int{{0, 0}, {100, 50}, {101, 50}, {50, 100}, {2, 2}, {104, 4}, {110, 12}, {300, 799}, {1279, 700}, {4, 204}} {
		if p[0] >= b.Dx() || p[1] >= b.Dy() {
			continue
		}
		r, g, bl, a := img.At(p[0], p[1]).RGBA()
		px[fmt.Sprintf("%d,%d", p[0], p[1])] = fmt.Sprintf("%d,%d,%d,%d", r>>8, g>>8, bl>>8, a>>8)
	}
	return map[string]any{"w": b.Dx(), "h": b.Dy(), "px": px}
}

func TestPariteNavigateur(t *testing.T) {
	testHome(t)
	// Le dossier de travail est mis en cache par processus (workspaceOnce) :
	// celui d'un test précédent a pu disparaître avec son TempDir.
	ws := agentWorkspace()
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	tools := t.TempDir()
	chrome := filepath.Join(tools, "chrome")
	if err := os.WriteFile(chrome, []byte(parityFakeChrome), 0o755); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(tools, "args")
	t.Setenv("LOKI_CHROME", chrome)
	t.Setenv("FAKE_CDP_ARGS", argsFile)
	t.Setenv("LOKI_CU_HEADFUL", "")

	var mu sync.Mutex
	scene := parityCDPScene{}
	var calls []string
	reply := func(method string, params map[string]any) (any, string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, parityCDPCall(method, params))
		if msg, ok := scene.Errors[method]; ok {
			return nil, msg
		}
		val := func(v any) any {
			if v == "<absent>" {
				return map[string]any{"result": map[string]any{"type": "undefined"}}
			}
			return map[string]any{"result": map[string]any{"type": "object", "value": v}}
		}
		switch method {
		case "Runtime.evaluate":
			expr, _ := params["expression"].(string)
			switch {
			case expr == "1":
				return val(1), ""
			case expr == "document.readyState":
				return val("complete"), ""
			case expr == "location.href":
				return val(scene.Href), ""
			}
			if scene.Throw {
				return map[string]any{"result": map[string]any{"type": "object"},
					"exceptionDetails": map[string]any{"text": "Uncaught TypeError: x is not a function"}}, ""
			}
			switch {
			case strings.Contains(expr, "window.__lokiRefs=map"):
				return val(scene.Snap), ""
			case strings.Contains(expr, "var pool=ajQueryAll();"):
				return val(scene.Find), ""
			case strings.Contains(expr, "inline:'center'"):
				return val(scene.Center), ""
			case strings.Contains(expr, "return 'TEXT';"):
				return val(scene.Typed), ""
			case strings.HasPrefix(expr, "history.") || strings.HasPrefix(expr, "window.scrollBy("):
				return val("ok"), ""
			}
			return val("<absent>"), ""
		case "Page.navigate":
			r := map[string]any{"frameId": "F1", "loaderId": "L1"}
			if scene.NavError != "" {
				r["errorText"] = scene.NavError
			}
			return r, ""
		case "Page.captureScreenshot":
			return map[string]any{"data": parityPNG(1280, 800)}, ""
		}
		return map[string]any{}, ""
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json" {
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"type": "service_worker", "webSocketDebuggerUrl": "ws://" + r.Host + "/devtools/sw/0"},
				{"type": "page", "webSocketDebuggerUrl": "ws://" + r.Host + "/devtools/page/1"},
			})
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(-1)
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var in struct {
				ID     int            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			_ = json.Unmarshal(data, &in)
			res, errMsg := reply(in.Method, in.Params)
			out := map[string]any{"id": in.ID}
			if errMsg != "" {
				out["error"] = map[string]any{"code": -32000, "message": errMsg}
			} else {
				out["result"] = res
			}
			b, _ := json.Marshal(out)
			if err := c.Write(r.Context(), websocket.MessageText, b); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	t.Setenv("FAKE_CDP_PORT", strings.TrimPrefix(srv.URL, "http://127.0.0.1:"))
	cdpShutdown()
	t.Cleanup(cdpShutdown)

	capRe := regexp.MustCompile(`capture-[0-9]{8}-[0-9]{6}\.png`)
	var steps []parityBrowserStep
	run := func(st parityBrowserStep) {
		mu.Lock()
		if st.Scene != nil {
			scene = *st.Scene
		}
		calls = nil
		mu.Unlock()
		if st.Vision {
			_ = SetConfigKey("MMPROJ", "vision.gguf")
		} else {
			_ = SetConfigKey("MMPROJ", "")
		}
		args := map[string]any{}
		b, _ := json.Marshal(st.Args)
		_ = json.Unmarshal(b, &args)
		res := execTool(t.Context(), st.Tool, args, toolCallLabel(st.Tool, args), Caps{Agent: true})
		st.Result = capRe.ReplaceAllString(res.Result, "capture-{{T}}.png")
		if res.VisionImg != nil {
			st.Image = parityImageFacts(t, res.VisionImg)
		}
		st.Saved = []string{}
		entries, _ := os.ReadDir(ws)
		for _, e := range entries {
			if !capRe.MatchString(e.Name()) {
				continue
			}
			st.Saved = append(st.Saved, capRe.ReplaceAllString(e.Name(), "capture-{{T}}.png"))
			_ = os.Remove(filepath.Join(ws, e.Name()))
		}
		mu.Lock()
		st.Calls = append([]string{}, calls...)
		mu.Unlock()
		if st.Args == nil {
			st.Args = map[string]any{}
		}
		steps = append(steps, st)
	}
	a := func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	snapPage := map[string]any{"title": "Exemple", "url": "https://exemple.test/", "above": 0, "below": 900,
		"refs": []any{`[1] link "Accueil"`, `[2] button "Créer un compte"`, `[3] textbox "Nom = Jean"`}}
	snapScrolled := map[string]any{"title": "Exemple", "url": "https://exemple.test/", "above": 600, "below": 10,
		"refs": []any{`[4] link "Mentions"`}}
	snapEmpty := map[string]any{"title": "", "url": "about:blank", "refs": []any{}, "above": 0, "below": 0}

	run(parityBrowserStep{Name: "ouverture sans url", Tool: "browser_open", Args: a("url", "  ")})
	run(parityBrowserStep{Name: "ouverture", Scene: &parityCDPScene{Snap: snapPage, Href: "https://exemple.test/"},
		Tool: "browser_open", Args: a("url", "exemple.test")})
	run(parityBrowserStep{Name: "snapshot inchangé", Tool: "browser_snapshot"})
	run(parityBrowserStep{Name: "réouverture : page complète", Tool: "browser_open", Args: a("url", "https://exemple.test/")})
	run(parityBrowserStep{Name: "page vide", Scene: &parityCDPScene{Snap: snapEmpty, Href: "about:blank"}, Tool: "browser_snapshot"})
	run(parityBrowserStep{Name: "snapshot null", Scene: &parityCDPScene{Snap: nil, Href: "about:blank"}, Tool: "browser_snapshot"})
	run(parityBrowserStep{Name: "snapshot sans valeur", Scene: &parityCDPScene{Snap: "<absent>"}, Tool: "browser_snapshot"})
	run(parityBrowserStep{Name: "snapshot illisible", Scene: &parityCDPScene{Snap: "texte"}, Tool: "browser_snapshot"})
	run(parityBrowserStep{Name: "exception JS", Scene: &parityCDPScene{Throw: true}, Tool: "browser_snapshot"})
	run(parityBrowserStep{Name: "défilé", Scene: &parityCDPScene{Snap: snapScrolled, Href: "https://exemple.test/"},
		Tool: "browser_scroll", Args: a("direction", "down")})
	run(parityBrowserStep{Name: "défilement haut", Scene: &parityCDPScene{Snap: snapPage}, Tool: "browser_scroll", Args: a("direction", "UP")})
	run(parityBrowserStep{Name: "défilement gauche puis droite", Tool: "browser_scroll", Args: a("direction", "left")})
	run(parityBrowserStep{Name: "défilement par défaut", Tool: "browser_scroll", Args: a()})
	run(parityBrowserStep{Name: "direction inconnue", Tool: "browser_scroll", Args: a("direction", "diagonale")})
	run(parityBrowserStep{Name: "clic", Scene: &parityCDPScene{Snap: snapScrolled, Center: map[string]any{"x": 640, "y": 412.5}},
		Tool: "browser_click", Args: a("ref", 2.0)})
	run(parityBrowserStep{Name: "clic introuvable", Scene: &parityCDPScene{Snap: snapPage, Center: nil}, Tool: "browser_click", Args: a("ref", 9.0)})
	run(parityBrowserStep{Name: "clic sans ref", Tool: "browser_click", Args: a("ref", "2")})
	run(parityBrowserStep{Name: "clic refusé", Scene: &parityCDPScene{Snap: snapPage, Center: map[string]any{"x": 1, "y": 2},
		Errors: map[string]string{"Input.dispatchMouseEvent": "Cannot dispatch"}}, Tool: "browser_click", Args: a("ref", 1.0)})
	run(parityBrowserStep{Name: "clic en coordonnées", Scene: &parityCDPScene{Snap: snapPage}, Tool: "browser_click_xy", Args: a("x", 100.4, "y", 250.5)})
	run(parityBrowserStep{Name: "coordonnées manquantes", Tool: "browser_click_xy", Args: a("x", 10.0)})
	run(parityBrowserStep{Name: "saisie libre", Scene: &parityCDPScene{Snap: snapScrolled}, Tool: "browser_type", Args: a("text", "bonjour <monde> & co")})
	run(parityBrowserStep{Name: "saisie dans un champ", Scene: &parityCDPScene{Snap: snapPage, Typed: "TEXT"}, Tool: "browser_type", Args: a("text", "Jean", "ref", 3.0)})
	run(parityBrowserStep{Name: "liste déroulante", Scene: &parityCDPScene{Snap: snapScrolled, Typed: "SELECT"}, Tool: "browser_type", Args: a("text", "France", "ref", 5.0)})
	run(parityBrowserStep{Name: "option absente", Scene: &parityCDPScene{Typed: "NOOPT"}, Tool: "browser_type", Args: a("text", "Mars", "ref", 5.0)})
	run(parityBrowserStep{Name: "champ introuvable", Scene: &parityCDPScene{Typed: "NOTFOUND"}, Tool: "browser_type", Args: a("text", "x", "ref", 7.0)})
	run(parityBrowserStep{Name: "touche", Scene: &parityCDPScene{Snap: snapPage}, Tool: "browser_key", Args: a("key", " Enter ")})
	run(parityBrowserStep{Name: "touche esc", Tool: "browser_key", Args: a("key", "esc")})
	run(parityBrowserStep{Name: "touche inconnue", Tool: "browser_key", Args: a("key", "F13")})
	run(parityBrowserStep{Name: "recherche vide", Tool: "browser_find", Args: a("text", "")})
	run(parityBrowserStep{Name: "recherche", Scene: &parityCDPScene{Snap: snapPage, Find: map[string]any{"refs": []any{`[2] button "Créer un compte"`, `[7] link "Créer"`}}},
		Tool: "browser_find", Args: a("text", "créer <b>")})
	run(parityBrowserStep{Name: "recherche sans résultat", Scene: &parityCDPScene{Find: map[string]any{"refs": []any{}}}, Tool: "browser_find", Args: a("text", "zzz")})
	run(parityBrowserStep{Name: "navigation en échec", Scene: &parityCDPScene{Snap: snapPage, Href: "https://nulle-part.test/", NavError: "net::ERR_NAME_NOT_RESOLVED"},
		Tool: "browser_open", Args: a("url", "nulle-part.test")})
	run(parityBrowserStep{Name: "page d'erreur", Scene: &parityCDPScene{Snap: snapPage, Href: "chrome-error://chromewebdata/"},
		Tool: "browser_open", Args: a("url", "http://x.test")})
	run(parityBrowserStep{Name: "navigation interrompue bénigne", Scene: &parityCDPScene{Snap: snapPage, Href: "https://dl.test/f.zip", NavError: "net::ERR_ABORTED"},
		Tool: "browser_open", Args: a("url", "https://dl.test/f.zip")})
	run(parityBrowserStep{Name: "retour", Scene: &parityCDPScene{Snap: snapScrolled, Href: "https://exemple.test/"}, Tool: "browser_open", Args: a("url", " Back ")})
	run(parityBrowserStep{Name: "avance", Tool: "browser_open", Args: a("url", "forward")})
	run(parityBrowserStep{Name: "commande refusée", Scene: &parityCDPScene{Snap: snapPage, Errors: map[string]string{"Page.navigate": "Cannot navigate to invalid URL"}},
		Tool: "browser_open", Args: a("url", "https://exemple.test/")})
	run(parityBrowserStep{Name: "capture sans vision", Scene: &parityCDPScene{Snap: snapPage}, Tool: "browser_screenshot"})
	run(parityBrowserStep{Name: "capture", Vision: true, Tool: "browser_screenshot"})
	run(parityBrowserStep{Name: "capture refusée", Vision: true, Scene: &parityCDPScene{Errors: map[string]string{"Page.captureScreenshot": "Unable to capture"}}, Tool: "browser_screenshot"})
	long := []any{}
	for i := 1; i <= 200; i++ {
		long = append(long, fmt.Sprintf(`[%d] link "Un lien assez long pour remplir la sortie, numéro %d"`, i, i))
	}
	run(parityBrowserStep{Name: "sortie plafonnée", Scene: &parityCDPScene{Snap: map[string]any{"title": "Long", "url": "https://long.test/", "refs": long}}, Tool: "browser_snapshot"})

	// Le lancement : arguments du faux navigateur.
	b, _ := os.ReadFile(argsFile)
	launch := []string{}
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if strings.HasPrefix(l, "--user-data-dir=") {
			l = "--user-data-dir={{PROFIL}}"
		}
		launch = append(launch, l)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{"launch": launch, "steps": steps}); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityBrowserFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityBrowserFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityBrowserFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("le navigateur piloté ne produit plus le journal rangé dans %s.\n"+
			"LOKI_PARITY_UPDATE=1 pour le régénérer si le changement est voulu.", parityBrowserFile)
	}
}
