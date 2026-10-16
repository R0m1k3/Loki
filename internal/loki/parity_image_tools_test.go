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
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// parity_image_tools_test.go — journal de référence des outils d'image de
// l'agent (lot 45 de la migration Rust) : see_image et web_screenshot, avec la
// préparation des images (orientation EXIF, grand côté sous 1568 px) et la
// sonde « le moteur voit-il les images » (faux /props).
//
// Une image laissée telle quelle est comparée à l'octet (empreinte) ; une
// image réencodée, par ses dimensions et son type (l'encodeur diffère), et
// une PNG réduite par quelques pixels (même réduction des deux côtés).
// web_screenshot passe par un faux Playwright qui range ses arguments et
// écrit l'image demandée par le scénario. Rangé dans
// rust/loki-rs/tests/parity_images/images.json ; tests/imagetools.rs le
// rejoue contre le port.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteOutilsImage

const parityImagesFile = "../../rust/loki-rs/tests/parity_images/images.json"

// parityFakePlaywright : écrit le fichier « image » de son dossier à la
// place de la capture (dernier argument), ou échoue selon « mode ».
const parityFakePlaywright = `#!/bin/sh
d=$(dirname "$0")
printf '%s\n' "$@" > "$d/args"
for a in "$@"; do out="$a"; done
mode=$(cat "$d/mode" 2>/dev/null)
case "$mode" in
  echec) echo "Launching chromium" ; echo "" ; echo "page.goto: net::ERR_NAME_NOT_RESOLVED at https://x" >&2 ; echo "Call log:" >&2 ; echo "  - navigating" >&2 ; exit 1 ;;
  vide) : > "$out" ;;
  rien) ;;
  *) cat "$d/image" > "$out" ;;
esac
`

type parityImageStep struct {
	Name     string         `json:"name"`
	Vision   bool           `json:"vision,omitempty"`
	Sees     bool           `json:"sees,omitempty"`  // le faux /props annonce la vision
	Mode     string         `json:"mode,omitempty"`  // faux Playwright
	Image    string         `json:"image,omitempty"` // fichier rendu par le faux Playwright
	Internet bool           `json:"internet,omitempty"`
	Tool     string         `json:"tool"`
	Args     map[string]any `json:"args"`
	Label    string         `json:"label"`
	Result   string         `json:"result"`
	Part     map[string]any `json:"part,omitempty"`  // image relayée
	Shown    string         `json:"shown,omitempty"` // web_screenshot : la capture montrée dans le fil
	PWArgs   []string       `json:"pw_args,omitempty"`
	Probes   int            `json:"probes"`
	Captures []string       `json:"captures,omitempty"`
}

var parityImgStampRe = regexp.MustCompile(`[0-9]{8}-[0-9]{6}`)

// parityImageFiles : les images du dossier de travail.
func parityImageFiles() map[string][]byte {
	grad := func(w, h int) *image.NRGBA {
		img := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 255 / w), G: uint8(y * 255 / h), B: uint8((x + y) % 256), A: 255})
			}
		}
		return img
	}
	enc := func(img image.Image, kind string) []byte {
		var b bytes.Buffer
		if kind == "png" {
			_ = png.Encode(&b, img)
		} else {
			_ = jpeg.Encode(&b, img, &jpeg.Options{Quality: 90})
		}
		return b.Bytes()
	}
	// JPEG avec un tag EXIF Orientation=6 (photo de téléphone tenue debout).
	withExif := func(j []byte, orient byte) []byte {
		tiff := []byte{'M', 'M', 0, 0x2A, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, orient, 0, 0, 0, 0, 0, 0}
		payload := append([]byte("Exif\x00\x00"), tiff...)
		seg := []byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}
		seg = append(seg, payload...)
		return append(append(append([]byte{}, j[:2]...), seg...), j[2:]...)
	}
	return map[string][]byte{
		"petite.png":    enc(grad(10, 8), "png"),
		"grande.png":    enc(grad(2000, 1000), "png"),
		"haute.png":     enc(grad(300, 1700), "png"),
		"photo.jpg":     enc(grad(40, 20), "jpeg"),
		"tournee.jpg":   withExif(enc(grad(40, 20), "jpeg"), 6),
		"grande.jpeg":   enc(grad(1800, 900), "jpeg"),
		"anim.gif":      []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;"),
		"image.webp":    []byte("RIFF\x10\x00\x00\x00WEBPVP8 faux"),
		"image.BMP":     []byte("BM pas vraiment une image"),
		"cassee.png":    []byte("\x89PNG\r\n\x1a\nabîmé"),
		"texte.txt":     []byte("pas une image"),
		"vide.png":      {},
		"capture.jpg":   enc(grad(1280, 800), "jpeg"),
		"pleine.jpg":    enc(grad(1280, 3000), "jpeg"),
		"capture.png":   enc(grad(64, 40), "png"),
		"sous/tres.png": enc(grad(3, 3), "png"),
	}
}

// parityPartFacts : ce qu'on compare d'une image relayée.
func parityPartFacts(t *testing.T, part map[string]any, raw map[string][]byte) map[string]any {
	u, _ := part["image_url"].(map[string]any)["url"].(string)
	i := strings.Index(u, ";base64,")
	mime := strings.TrimPrefix(u[:i], "data:")
	b, err := base64.StdEncoding.DecodeString(u[i+8:])
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{"mime": mime}
	for name, r := range raw {
		if bytes.Equal(r, b) {
			out["same_as"] = name // laissée telle quelle : à l'octet
			return out
		}
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		h := sha256.Sum256(b)
		out["sha"] = hex.EncodeToString(h[:8])
		return out
	}
	bd := img.Bounds()
	out["w"], out["h"] = bd.Dx(), bd.Dy()
	if mime == "image/png" {
		px := map[string]string{}
		for _, p := range [][2]int{{0, 0}, {1, 1}, {100, 50}, {bd.Dx() - 1, bd.Dy() - 1}, {bd.Dx() / 2, bd.Dy() / 3}} {
			c := color.NRGBAModel.Convert(img.At(p[0], p[1])).(color.NRGBA)
			px[fmt.Sprintf("%d,%d", p[0], p[1])] = fmt.Sprintf("%d,%d,%d,%d", c.R, c.G, c.B, c.A)
		}
		out["px"] = px
	}
	return out
}

func TestPariteOutilsImage(t *testing.T) {
	testHome(t)
	root := t.TempDir()
	files := filepath.Join(root, "fichiers")
	raw := parityImageFiles()
	for name, b := range raw {
		p := filepath.Join(files, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.MkdirAll(filepath.Join(files, "dossier.png"), 0o755)
	lourd, _ := os.Create(filepath.Join(files, "lourd.png"))
	_ = lourd.Truncate(13 << 20)
	lourd.Close()
	setTaskWorkspace(files)
	t.Cleanup(func() { setTaskWorkspace("") })

	// Faux moteur : /props, authentifié.
	var mu sync.Mutex
	sees, probes := false, 0
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/props" || r.Header.Get("Authorization") != "Bearer cle-moteur" {
			w.WriteHeader(401)
			return
		}
		probes++
		_ = json.NewEncoder(w).Encode(map[string]any{"modalities": map[string]any{"vision": sees}})
	}))
	defer eng.Close()
	_ = SetConfigKey("PORT", strings.TrimPrefix(eng.URL, "http://127.0.0.1:"))
	_ = putStr(bkState, "api_key", "cle-moteur")

	// Faux Playwright, seul « playwright » du PATH.
	pwDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pwDir, "playwright"), []byte(parityFakePlaywright), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pwDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	conv := convEnsureActive()
	capDir, _ := captureDirFor(conv)

	var steps []parityImageStep
	run := func(st parityImageStep) {
		if st.Vision {
			_ = SetConfigKey("MMPROJ", "vision.gguf")
		} else {
			_ = SetConfigKey("MMPROJ", "")
		}
		mu.Lock()
		sees, probes = st.Sees, 0
		mu.Unlock()
		visionProbeMu.Lock()
		visionProbeAt = time.Time{}
		visionProbeMu.Unlock()
		_ = os.WriteFile(filepath.Join(pwDir, "mode"), []byte(st.Mode), 0o644)
		if st.Image != "" {
			_ = os.WriteFile(filepath.Join(pwDir, "image"), raw[st.Image], 0o644)
		}
		_ = os.Remove(filepath.Join(pwDir, "args"))
		args := map[string]any{}
		b, _ := json.Marshal(st.Args)
		_ = json.Unmarshal([]byte(strings.ReplaceAll(string(b), "{{FICHIERS}}", files)), &args)
		label := toolCallLabel(st.Tool, args)
		st.Label = strings.ReplaceAll(label, files, "{{FICHIERS}}")
		res := execTool(t.Context(), st.Tool, args, label, Caps{Agent: true, Internet: st.Internet})
		image, _, relay := toolStepExtras(st.Tool, label, res)
		st.Result = strings.ReplaceAll(parityImgStampRe.ReplaceAllString(res.Result, "{{T}}"), files, "{{FICHIERS}}")
		st.Shown = parityImgStampRe.ReplaceAllString(image, "{{T}}")
		var part map[string]any
		if relay != nil {
			parts, _ := json.Marshal(relay.Content)
			var ps []map[string]any
			_ = json.Unmarshal(parts, &ps)
			for _, p := range ps {
				if p["type"] == "image_url" {
					part = p
				}
				if p["type"] == "text" {
					st.Result += "\n[relais] " + strings.ReplaceAll(fmt.Sprint(p["text"]), files, "{{FICHIERS}}")
				}
			}
		}
		if part != nil {
			st.Part = parityPartFacts(t, part, raw)
		}
		if b, err := os.ReadFile(filepath.Join(pwDir, "args")); err == nil {
			for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
				st.PWArgs = append(st.PWArgs, parityImgStampRe.ReplaceAllString(strings.ReplaceAll(l, capDir, "{{CAPTURES}}"), "{{T}}"))
			}
		}
		mu.Lock()
		st.Probes = probes
		mu.Unlock()
		if st.Tool == "web_screenshot" {
			entries, _ := os.ReadDir(capDir)
			st.Captures = []string{}
			for _, e := range entries {
				st.Captures = append(st.Captures, parityImgStampRe.ReplaceAllString(e.Name(), "{{T}}"))
			}
			sort.Strings(st.Captures)
		}
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

	// see_image.
	run(parityImageStep{Name: "sans vision", Tool: "see_image", Args: a("file", "petite.png")})
	run(parityImageStep{Name: "chemin manquant", Vision: true, Tool: "see_image", Args: a("file", "  ")})
	run(parityImageStep{Name: "pas une image", Vision: true, Tool: "see_image", Args: a("file", "texte.txt")})
	run(parityImageStep{Name: "introuvable", Vision: true, Tool: "see_image", Args: a("file", "absente.png")})
	run(parityImageStep{Name: "dossier", Vision: true, Tool: "see_image", Args: a("file", "dossier.png")})
	run(parityImageStep{Name: "trop lourde", Vision: true, Tool: "see_image", Args: a("file", "lourd.png")})
	run(parityImageStep{Name: "le moteur ne voit pas", Vision: true, Tool: "see_image", Args: a("file", "petite.png")})
	run(parityImageStep{Name: "vide", Vision: true, Sees: true, Tool: "see_image", Args: a("file", "vide.png")})
	for _, f := range []string{"petite.png", "grande.png", "haute.png", "photo.jpg", "tournee.jpg", "grande.jpeg", "anim.gif", "image.webp", "image.BMP", "cassee.png", "sous/tres.png"} {
		run(parityImageStep{Name: "voir " + f, Vision: true, Sees: true, Tool: "see_image", Args: a("file", f)})
	}
	run(parityImageStep{Name: "chemin absolu", Vision: true, Sees: true, Tool: "see_image", Args: a("file", "{{FICHIERS}}/petite.png")})

	// web_screenshot.
	run(parityImageStep{Name: "capture sans url", Tool: "web_screenshot", Args: a()})
	run(parityImageStep{Name: "url invalide", Tool: "web_screenshot", Args: a("url", "ftp://x")})
	run(parityImageStep{Name: "internet coupé", Tool: "web_screenshot", Args: a("url", "https://exemple.test/")})
	run(parityImageStep{Name: "capture locale sans vision", Image: "capture.jpg", Tool: "web_screenshot", Args: a("url", "http://localhost:3000/app?x=1")})
	run(parityImageStep{Name: "capture vue par le modèle", Vision: true, Sees: true, Internet: true, Image: "capture.jpg",
		Tool: "web_screenshot", Args: a("url", "https://www.exemple.test/page#ancre", "width", 1600.0)})
	run(parityImageStep{Name: "page entière réduite", Vision: true, Sees: true, Internet: true, Image: "pleine.jpg",
		Tool: "web_screenshot", Args: a("url", "https://pleine.test/", "full_page", true, "width", 100.0)})
	run(parityImageStep{Name: "vision mais moteur aveugle", Vision: true, Internet: true, Image: "capture.png",
		Tool: "web_screenshot", Args: a("url", "https://aveugle.test/")})
	run(parityImageStep{Name: "Playwright en échec", Internet: true, Mode: "echec", Tool: "web_screenshot", Args: a("url", "https://x.test/")})
	run(parityImageStep{Name: "image vide", Internet: true, Mode: "vide", Tool: "web_screenshot", Args: a("url", "https://vide.test/")})
	run(parityImageStep{Name: "aucune image", Internet: true, Mode: "rien", Tool: "web_screenshot", Args: a("url", "https://rien.test/")})
	// Ménage : 22 vieilles captures, plus la nouvelle → 20.
	old := time.Now().Add(-time.Hour)
	for i := 0; i < 22; i++ {
		p := filepath.Join(capDir, fmt.Sprintf("vieille-%02d.jpg", i))
		_ = os.WriteFile(p, []byte("x"), 0o644)
		mt := old.Add(time.Duration(i) * time.Second)
		_ = os.Chtimes(p, mt, mt)
	}
	run(parityImageStep{Name: "ménage des captures", Internet: true, Image: "capture.png", Tool: "web_screenshot", Args: a("url", "http://127.0.0.1:8080/")})

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{"files": raw, "steps": steps}); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityImagesFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityImagesFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityImagesFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("les outils d'image ne produisent plus le journal rangé dans %s.\n"+
			"LOKI_PARITY_UPDATE=1 pour le régénérer si le changement est voulu.", parityImagesFile)
	}
}
