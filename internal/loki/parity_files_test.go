package loki

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// parity_files_test.go — journal de référence des fichiers de la discussion
// (lot 11 de la migration Rust) : noms de dépôt, slug, orientation EXIF,
// redressement et réduction des pixels, reniflage du type, puis un scénario
// de routes (panneau Fichiers, téléchargement, image, vignette, dépôt en
// morceaux, suppression, archive .zip) sur une arborescence fixe. Rangé dans
// rust/loki-rs/tests/parity_files/files.json ; tests/convfiles.rs le rejoue
// par la façade. LOKI_HOME réécrit en {{HOME}}, la date en {{DATE}}.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteFichiers

const parityFilesFile = "../../rust/loki-rs/tests/parity_files/files.json"

type parityPix struct {
	W   int    `json:"w"`
	H   int    `json:"h"`
	Pix []byte `json:"pix"`
}

type parityFileNode struct {
	// Contenu (base64) d'un fichier, ou dossier si Dir.
	Data  []byte `json:"data,omitempty"`
	Dir   bool   `json:"dir,omitempty"`
	Mtime int64  `json:"mtime"`
}

type parityFilesCall struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	Body   string `json:"body,omitempty"`
	Status int    `json:"status"`
	// JSON (masqué), ou corps brut (base64) et en-têtes choisis.
	Out     any               `json:"out,omitempty"`
	Raw     []byte            `json:"raw,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Vignette : dimensions de l'image décodée.
	Thumb []int `json:"thumb,omitempty"`
	// Archive : entrées (nom → contenu base64).
	Zip map[string][]byte `json:"zip,omitempty"`
	// Après l'appel : les fichiers de la discussion (chemin relatif → taille).
	Tree map[string]int64 `json:"tree,omitempty"`
}

type parityFilesFixture struct {
	UploadName map[string]string `json:"upload_name"`
	Slug       map[string]string `json:"slug"`
	Exif       []struct {
		In  []byte `json:"in"`
		Out int    `json:"out"`
	} `json:"exif"`
	Orient []struct {
		Orient int       `json:"orient"`
		Out    parityPix `json:"out"`
	} `json:"orient"`
	OrientIn parityPix `json:"orient_in"`
	Scale    []struct {
		In  parityPix `json:"in"`
		Max int       `json:"max"`
		Out parityPix `json:"out"`
	} `json:"scale"`
	Sniff []struct {
		In  []byte `json:"in"`
		Out string `json:"out"`
	} `json:"sniff"`
	// Ce que /internal/v1/conv/workspace répond pendant le scénario.
	Workspace map[string]string         `json:"workspace"`
	Tree      map[string]parityFileNode `json:"tree"`
	Calls     []parityFilesCall         `json:"calls"`
}

func parityNRGBA(w, h int, seed int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = byte((i*37 + seed*11) % 256)
	}
	return img
}

func pixOf(img image.Image) parityPix {
	n := toNRGBA(img)
	b := n.Bounds()
	return parityPix{W: b.Dx(), H: b.Dy(), Pix: append([]byte(nil), n.Pix...)}
}

// parityJPEGExif : un en-tête JPEG avec un segment APP1 EXIF portant
// l'orientation o (ordre d'octets « II » ou « MM »), précédé d'un APP0.
func parityJPEGExif(order string, o uint16, tagFirst bool) []byte {
	be := order == "MM"
	u16 := func(v uint16) []byte {
		if be {
			return []byte{byte(v >> 8), byte(v)}
		}
		return []byte{byte(v), byte(v >> 8)}
	}
	u32 := func(v uint32) []byte {
		if be {
			return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
		}
		return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
	}
	tiff := []byte(order)
	tiff = append(tiff, u16(0x2A)...)
	tiff = append(tiff, u32(8)...)
	entries := [][]byte{}
	other := append(append(append(u16(0x010F), u16(2)...), u32(1)...), u32(0)...)
	orient := append(append(append(u16(0x0112), u16(3)...), u32(1)...), append(u16(o), 0, 0)...)
	if tagFirst {
		entries = append(entries, orient, other)
	} else {
		entries = append(entries, other, orient)
	}
	tiff = append(tiff, u16(uint16(len(entries)))...)
	for _, e := range entries {
		tiff = append(tiff, e...)
	}
	app1 := append([]byte("Exif\x00\x00"), tiff...)
	out := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x06, 'J', 'F', 'I', 'F'}
	out = append(out, 0xFF, 0xE1, byte((len(app1)+2)>>8), byte(len(app1)+2))
	out = append(out, app1...)
	return append(out, 0xFF, 0xDA, 0x00, 0x02)
}

func TestPariteFichiers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("journal tiré sous Unix")
	}
	home := testHome(t)
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("LOKI_HOME", home)
	t.Setenv("LOKI_WORKSPACE", filepath.Join(home, "workspace"))
	t.Setenv("TMPDIR", filepath.Join(home, "tmp"))
	// agentWorkspace est mémorisé pour tout le process (sync.Once) : le dossier
	// de travail de CE test est imposé, comme dans withWorkspace.
	prevWS := agentWorkspace()
	workspacePath = filepath.Join(home, "workspace")
	_ = os.MkdirAll(workspacePath, 0o755)
	t.Cleanup(func() { workspacePath = prevWS })
	_ = os.MkdirAll(filepath.Join(home, "tmp"), 0o755)
	date := time.Now().Format("2006-01-02")
	norm := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, home, "{{HOME}}"), date, "{{DATE}}")
	}
	var fx parityFilesFixture

	fx.UploadName = map[string]string{}
	for _, n := range []string{"rapport.pdf", "../../etc/passwd", `C:\Users\moi\photo.jpg`, "a:b*c?d\"e<f>g|h.txt",
		"  .cache  ", "..", "", "x\x01y.txt", strings.Repeat("n", 130) + ".pdf", strings.Repeat("n", 130) + "." + strings.Repeat("e", 20)} {
		fx.UploadName[n] = safeUploadName(n)
	}
	fx.Slug = map[string]string{}
	for _, s := range []string{"Déjà   vu", "  Rapport_Final.v2 ", "!!!", "", strings.Repeat("abc ", 20), "--a--"} {
		fx.Slug[s] = slugify(s)
	}
	for _, c := range []struct {
		in []byte
	}{
		{parityJPEGExif("II", 6, true)}, {parityJPEGExif("MM", 8, false)}, {parityJPEGExif("II", 1, false)},
		{parityJPEGExif("II", 6, true)[:30]}, {[]byte{0xFF, 0xD8, 0xFF, 0xDA}}, {[]byte("pas un jpeg")},
	} {
		fx.Exif = append(fx.Exif, struct {
			In  []byte `json:"in"`
			Out int    `json:"out"`
		}{c.in, exifOrientation(c.in)})
	}
	src := parityNRGBA(5, 3, 1)
	fx.OrientIn = pixOf(src)
	for o := 1; o <= 8; o++ {
		fx.Orient = append(fx.Orient, struct {
			Orient int       `json:"orient"`
			Out    parityPix `json:"out"`
		}{o, pixOf(applyOrientation(src, o))})
	}
	for _, c := range []struct{ w, h, max int }{{7, 5, 3}, {5, 9, 4}, {10, 10, 3}, {3, 2, 5}} {
		in := parityNRGBA(c.w, c.h, c.w*c.h)
		fx.Scale = append(fx.Scale, struct {
			In  parityPix `json:"in"`
			Max int       `json:"max"`
			Out parityPix `json:"out"`
		}{pixOf(in), c.max, pixOf(scaleDownToMax(in, c.max))})
	}
	var pngBuf bytes.Buffer
	_ = png.Encode(&pngBuf, parityNRGBA(4, 3, 2))
	pngBytes := pngBuf.Bytes()
	for _, in := range [][]byte{pngBytes[:16], {0xFF, 0xD8, 0xFF, 0xE0}, []byte("GIF89a\x01\x00"), []byte("RIFF\x10\x00\x00\x00WEBPVP8 "),
		[]byte("BM\x00\x00"), {0, 0, 1, 0, 1, 0}, []byte("<svg xmlns='x'/>"), []byte("<html><body>x"), []byte("bonjour"), {}} {
		fx.Sniff = append(fx.Sniff, struct {
			In  []byte `json:"in"`
			Out string `json:"out"`
		}{in, http.DetectContentType(in)})
	}

	// Arborescence : la discussion c-fichiers, une ancienne capture d'une autre
	// discussion, des fichiers d'avant le rangement à la racine.
	convActiveRemember("c-fichiers")
	conv := "workspace/discussions/c-fichiers/"
	fx.Tree = map[string]parityFileNode{
		conv + "rapport.md":                            {Data: []byte("# Rapport\n"), Mtime: 1700000030},
		conv + "image.png":                             {Data: pngBytes, Mtime: 1700000040},
		conv + "faux.png":                              {Data: []byte("<html>pas une image</html>"), Mtime: 1700000005},
		conv + "notes/a.txt":                           {Data: []byte("alpha"), Mtime: 1700000010},
		conv + "notes/sous/b.txt":                      {Data: []byte("beta!!"), Mtime: 1700000011},
		conv + "notes/sous":                            {Dir: true, Mtime: 1700000012},
		conv + "notes":                                 {Dir: true, Mtime: 1700000020},
		"workspace/vieux.txt":                          {Data: []byte("old"), Mtime: 1700000001},
		"workspace/uploads/ancien.pdf":                 {Data: []byte("%PDF"), Mtime: 1700000002},
		"workspace/uploads":                            {Dir: true, Mtime: 1700000003},
		"workspace/discussions/c-autre/captures/x.jpg": {Data: []byte("jpeg?"), Mtime: 1700000004},
	}
	keys := make([]string, 0, len(fx.Tree))
	for k := range fx.Tree {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) }) // fichiers profonds d'abord
	for _, k := range keys {
		n := fx.Tree[k]
		p := filepath.Join(home, k)
		if n.Dir {
			_ = os.MkdirAll(p, 0o755)
			continue
		}
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, n.Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range keys { // dates des dossiers après leur contenu
		n := fx.Tree[k]
		ts := time.Unix(n.Mtime, 0)
		if err := os.Chtimes(filepath.Join(home, k), ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	handleInternalConvWorkspace(w, httptest.NewRequest("POST", "/internal/v1/conv/workspace", strings.NewReader("{}")))
	var ws map[string]string
	_ = json.Unmarshal([]byte(norm(w.Body.String())), &ws)
	fx.Workspace = ws

	idRe := regexp.MustCompile(`\.upload-[0-9]+\.part`)
	convDir := filepath.Join(home, conv)
	tree := func() map[string]int64 {
		out := map[string]int64{}
		_ = filepath.Walk(convDir, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				rel, _ := filepath.Rel(convDir, p)
				out[idRe.ReplaceAllString(filepath.ToSlash(rel), ".upload-{{N}}.part")] = info.Size()
			}
			return nil
		})
		return out
	}
	lastID := ""
	call := func(method, url, body string) parityFilesCall {
		body = strings.ReplaceAll(body, "{{ID}}", lastID)
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, url, strings.NewReader(body))
		path := url
		if i := strings.IndexByte(url, '?'); i >= 0 {
			path = url[:i]
		}
		switch path {
		case "/api/chat/files":
			handleChatFiles(w, r)
		case "/api/chat/files/zip":
			handleChatFilesZip(w, r)
		case "/api/chat/file":
			handleChatFile(w, r)
		case "/api/chat/file/delete":
			handleChatFileDelete(w, r)
		case "/api/chat/image":
			handleChatImage(w, r)
		case "/api/chat/upload":
			handleChatUpload(w, r)
		default:
			t.Fatalf("route inattendue %s", url)
		}
		shown := body
		if lastID != "" {
			shown = strings.ReplaceAll(body, lastID, "{{ID}}")
		}
		c := parityFilesCall{Method: method, URL: url, Body: shown, Status: w.Code}
		ct := w.Header().Get("Content-Type")
		switch {
		case strings.HasPrefix(ct, "application/json"):
			txt := norm(w.Body.String())
			var m map[string]any
			if err := json.Unmarshal([]byte(txt), &m); err == nil {
				if id, ok := m["id"].(string); ok {
					lastID = id
					m["id"] = "{{ID}}"
				}
				if d, ok := m["data"].(string); ok && path == "/api/chat/files/zip" {
					raw, _ := base64.StdEncoding.DecodeString(d)
					zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
					if err != nil {
						t.Fatal(err)
					}
					c.Zip = map[string][]byte{}
					for _, f := range zr.File {
						rc, _ := f.Open()
						b, _ := io.ReadAll(rc)
						rc.Close()
						c.Zip[f.Name] = b
					}
					m["data"] = "…"
				}
				if _, ok := m["size"]; ok && path == "/api/chat/files/zip" {
					m["size"] = "…"
				}
				c.Out = m
			} else {
				_ = json.Unmarshal([]byte(txt), &c.Out)
			}
		case ct == "image/jpeg" && strings.Contains(url, "thumb="):
			img, _, err := image.Decode(bytes.NewReader(w.Body.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			c.Thumb = []int{img.Bounds().Dx(), img.Bounds().Dy()}
			c.Headers = map[string]string{}
		default:
			c.Raw = w.Body.Bytes()
			c.Headers = map[string]string{}
		}
		if c.Headers != nil {
			for _, h := range []string{"Content-Type", "Content-Disposition", "X-Content-Type-Options", "Content-Security-Policy", "Cache-Control"} {
				if v := w.Header().Get(h); v != "" {
					c.Headers[h] = v
				}
			}
		}
		if method == "POST" {
			c.Tree = tree()
		}
		if s, ok := c.Out.(map[string]any); ok {
			if p, ok := s["path"].(string); ok {
				s["path"] = idRe.ReplaceAllString(p, ".upload-{{N}}.part")
			}
		}
		return c
	}
	for _, c := range []struct{ m, u, b string }{
		{"GET", "/api/chat/files", ""},
		{"GET", "/api/chat/files?dir=notes", ""},
		{"GET", "/api/chat/files?dir=notes/sous/", ""},
		{"GET", "/api/chat/files?dir=../../..", ""},
		{"GET", "/api/chat/files?dir=rapport.md", ""},
		{"GET", "/api/chat/files?scope=legacy", ""},
		{"GET", "/api/chat/file?path=rapport.md&meta=1", ""},
		{"GET", "/api/chat/file?path=rapport.md&b64=1&offset=2&len=4", ""},
		{"GET", "/api/chat/file?path=rapport.md&b64=1&offset=5", ""},
		{"GET", "/api/chat/file?path=rapport.md&b64=1&offset=99", ""},
		{"GET", "/api/chat/file?path=../../vieux.txt", ""},
		{"GET", "/api/chat/file?path=absent.txt", ""},
		{"GET", "/api/chat/file?path=", ""},
		{"GET", "/api/chat/file?path=captures/c-autre/x.jpg&meta=1", ""},
		{"GET", "/api/chat/file?path=uploads/ancien.pdf&meta=1", ""},
		{"GET", "/api/chat/file?path=notes", ""},
		{"GET", "/api/chat/file?path=/notes/a.txt", ""},
		{"GET", "/api/chat/image?path=faux.png", ""},
		{"GET", "/api/chat/image?path=image.png", ""},
		{"GET", "/api/chat/image?path=image.png&thumb=2", ""},
		{"GET", "/api/chat/image?path=rapport.md&thumb=64", ""},
		{"POST", "/api/chat/upload", `{"name":"../Mon fichier?.txt","data":"aGVsbG8="}`},
		{"POST", "/api/chat/upload", `{"name":"Mon fichier?.txt","data":"aGVsbG8gIQ=="}`},
		{"POST", "/api/chat/upload", `{"name":"gros.bin","data":"AAEC","more":true,"size":5}`},
		{"POST", "/api/chat/upload", `{"id":"{{ID}}","data":"data:application/octet-stream;base64,AwQ=","more":false}`},
		{"POST", "/api/chat/upload", `{"id":"inconnu","data":"AA=="}`},
		{"POST", "/api/chat/upload", `{"name":"vide.txt","data":""}`},
		{"POST", "/api/chat/upload", `{"name":"x","data":"!!!"}`},
		{"POST", "/api/chat/upload", `pas du json`},
		{"POST", "/api/chat/file/delete", `{"path":""}`},
		{"POST", "/api/chat/file/delete", `{"path":"."}`},
		{"POST", "/api/chat/file/delete", `{"path":"../.."}`},
		{"POST", "/api/chat/file/delete", `{"path":"discussions","scope":"legacy"}`},
		{"POST", "/api/chat/file/delete", `{"path":"absent"}`},
		{"POST", "/api/chat/file/delete", `{"path":"faux.png"}`},
		{"POST", "/api/chat/file/delete", `{"path":"notes/sous"}`},
		{"GET", "/api/chat/files/zip", ""},
		{"GET", "/api/chat/files/zip?meta=1", ""},
		{"GET", "/api/chat/files/zip?b64=1&offset=0&len=99999999", ""},
		{"GET", "/api/chat/files/zip?b64=1&offset=-1", ""},
		{"GET", "/api/chat/files/zip?meta=1&scope=legacy", ""},
		{"GET", "/api/chat/files/zip?b64=1&scope=legacy", ""},
	} {
		fx.Calls = append(fx.Calls, call(c.m, c.u, c.b))
	}

	got, err := parityJSON(fx)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityFilesFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityFilesFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityFilesFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("les fichiers de la discussion ne produisent plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteFichiers,\n"+
			"puis cargo test --test convfiles dit si le port Rust suit.\n--- obtenu ---\n%s", parityFilesFile, got)
	}
}
