package loki

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// parity_dictate_test.go — journal de référence de la dictée vocale (lot 9 de
// la migration Rust) : lecture des WAV, chemins d'archive, marqueurs,
// réglages, catalogue, arguments du serveur, et les routes qui ne lancent
// rien (réglages, catalogue, état, refus). Rangé dans
// rust/loki-rs/tests/parity_dictate/dictate.json ; tests/dictate.rs le rejoue.
// LOKI_HOME réécrit en {{HOME}}.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteDictee

const parityDictateFile = "../../rust/loki-rs/tests/parity_dictate/dictate.json"

type parityWav struct {
	Name string `json:"name"`
	// WAV en octets (JSON : base64).
	In   []byte    `json:"in"`
	Rate int       `json:"rate,omitempty"`
	Ech  []float32 `json:"ech,omitempty"`
	Err  string    `json:"err,omitempty"`
}

type parityDictateCall struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"body,omitempty"`
	Status int    `json:"status"`
	Out    any    `json:"out"`
}

type parityDictateFixture struct {
	Wav     []parityWav `json:"wav"`
	SafeRel []struct {
		In  string `json:"in"`
		Out string `json:"out,omitempty"`
		Err string `json:"err,omitempty"`
	} `json:"safe_rel"`
	Marker   map[string]bool     `json:"marker"`
	Defaults []map[string]string `json:"defaults"`
	LastLine map[string]string   `json:"last_line"`
	Args     []string            `json:"args"`
	ArgsErr  string              `json:"args_err"`
	URLs     map[string]string   `json:"urls"`
	Calls    []parityDictateCall `json:"calls"`
	Present  map[string]bool     `json:"present"`
}

// parityWavOf : un WAV avec les blocs donnés après l'en-tête RIFF/WAVE.
func parityWavOf(chunks ...[]byte) []byte {
	b := []byte("RIFF\x00\x00\x00\x00WAVE")
	for _, c := range chunks {
		b = append(b, c...)
	}
	binary.LittleEndian.PutUint32(b[4:8], uint32(len(b)-8))
	return b
}

func parityChunk(id string, body []byte) []byte {
	h := make([]byte, 8)
	copy(h, id)
	binary.LittleEndian.PutUint32(h[4:], uint32(len(body)))
	out := append(h, body...)
	if len(body)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func parityFmt(format, canaux, taux, bits int) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint16(b[0:], uint16(format))
	binary.LittleEndian.PutUint16(b[2:], uint16(canaux))
	binary.LittleEndian.PutUint32(b[4:], uint32(taux))
	binary.LittleEndian.PutUint32(b[8:], uint32(taux*canaux*bits/8))
	binary.LittleEndian.PutUint16(b[12:], uint16(canaux*bits/8))
	binary.LittleEndian.PutUint16(b[14:], uint16(bits))
	return parityChunk("fmt ", b)
}

func parityPCM16(v ...int16) []byte {
	b := make([]byte, 2*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(x))
	}
	return b
}

func TestPariteDictee(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("journal tiré sous Unix")
	}
	home := testHome(t)
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("LOKI_HOME", home)
	norm := func(s string) string { return strings.ReplaceAll(s, home, "{{HOME}}") }
	var fx parityDictateFixture

	f32 := make([]byte, 12)
	for i, v := range []float32{0.25, -1, 0.5} {
		binary.LittleEndian.PutUint32(f32[4*i:], math.Float32bits(v))
	}
	cases := []struct {
		name string
		in   []byte
	}{
		{"mono 16 bits", parityWavOf(parityFmt(1, 1, 16000, 16), parityChunk("data", parityPCM16(0, 16384, -16384, 32767, -32768)))},
		{"bloc LIST intercalé", parityWavOf(parityChunk("LIST", []byte("INFOxyz")), parityFmt(1, 1, 16000, 16), parityChunk("data", parityPCM16(100, -100)))},
		{"stéréo moyenné", parityWavOf(parityFmt(1, 2, 48000, 16), parityChunk("data", parityPCM16(0, 32767, -32768, 32767)))},
		{"flottant 32 bits", parityWavOf(parityFmt(3, 1, 16000, 32), parityChunk("data", f32))},
		{"data tronqué", parityWavOf(parityFmt(1, 1, 16000, 16), parityChunk("data", parityPCM16(1, 2, 3)))[:12+24+8+4]},
		{"data impair", parityWavOf(parityFmt(1, 1, 8000, 16), parityChunk("data", []byte{1, 2, 3}))},
		{"data vide", parityWavOf(parityFmt(1, 1, 16000, 16), parityChunk("data", nil))},
		{"24 bits", parityWavOf(parityFmt(1, 1, 16000, 24), parityChunk("data", make([]byte, 6)))},
		{"data avant fmt", parityWavOf(parityChunk("data", parityPCM16(1)), parityFmt(1, 1, 16000, 16))},
		{"fmt tronqué", parityWavOf(parityChunk("fmt ", make([]byte, 8)))},
		{"sans data", parityWavOf(parityFmt(1, 1, 16000, 16))},
		{"pas un WAV", []byte("ceci n'est pas un fichier audio du tout")},
		{"vide", []byte{}},
	}
	for _, c := range cases {
		w := parityWav{Name: c.name, In: c.in}
		taux, ech, err := wavVersFloat32(c.in)
		if err != nil {
			w.Err = err.Error()
		} else {
			w.Rate, w.Ech = taux, ech
			if w.Ech == nil {
				w.Ech = []float32{}
			}
		}
		fx.Wav = append(fx.Wav, w)
	}

	for _, in := range []string{"", "racine", "racine/", "racine/encoder.onnx", "racine/sous/tokens.txt",
		"/abs/x", "racine/../x", `racine\a\b.txt`, " racine/a ", "..", "a/b/../c"} {
		out, err := asrSafeRel(in)
		c := struct {
			In  string `json:"in"`
			Out string `json:"out,omitempty"`
			Err string `json:"err,omitempty"`
		}{In: in, Out: out}
		if err != nil {
			c.Err = err.Error()
		}
		fx.SafeRel = append(fx.SafeRel, c)
	}
	fx.Marker = map[string]bool{}
	for _, s := range []string{"", "  ", "[musique]", "(silence)", " (rires) ", "bonjour", "[a] b", "(x"} {
		fx.Marker[s] = asrMarqueur(s)
	}
	for _, c := range []DictateCfg{{}, {Model: "ggml-base.bin"}, {Model: "parakeet-tdt-0.6b-v2", Reactivity: "court"}} {
		d := c.withDefauts()
		fx.Defaults = append(fx.Defaults, map[string]string{"model": c.Model, "reactivity": c.Reactivity,
			"out_model": d.Model, "out_reactivity": d.Reactivity})
	}
	fx.LastLine = map[string]string{}
	for _, s := range []string{"", "a\nb\n\n  ", "un seul", "  \n x \n"} {
		fx.LastLine[s] = lastLine(s)
	}
	args, _ := asrServerArgs(DictateCfg{Model: "parakeet-tdt-0.6b-v3"}, 4567)
	for _, a := range args {
		fx.Args = append(fx.Args, norm(a))
	}
	if _, err := asrServerArgs(DictateCfg{Model: "x"}, 1); err != nil {
		fx.ArgsErr = err.Error()
	}
	fx.URLs = map[string]string{}
	for id := range asrCatalogue {
		fx.URLs[id] = asrModelURLFor(id)
	}
	fx.URLs["inconnu"] = asrModelURLFor("inconnu")

	// v2 installé (quatre fichiers non vides), v3 à moitié.
	for _, f := range asrFichiers {
		p := asrModelFile("parakeet-tdt-0.6b-v2", f)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("x"), 0o644)
	}
	p := asrModelFile("parakeet-tdt-0.6b-v3", "encoder.int8.onnx")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte("x"), 0o644)
	_ = os.WriteFile(asrModelFile("parakeet-tdt-0.6b-v3", "tokens.txt"), nil, 0o644)
	fx.Present = map[string]bool{}
	for id := range asrCatalogue {
		fx.Present[id] = asrModelPresent(id)
	}
	fx.Present["inconnu"] = asrModelPresent("inconnu")

	call := func(method, path, body string) parityDictateCall {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		switch path {
		case "/api/dictate/config":
			handleDictateConfig(w, r)
		case "/api/dictate/models":
			handleDictateModels(w, r)
		case "/api/dictate/models/download":
			handleDictateDownload(w, r)
		case "/api/dictate/state":
			handleDictateState(w, r)
		case "/api/transcribe":
			handleTranscribe(w, r)
		default:
			t.Fatalf("route inattendue %s", path)
		}
		var out any
		if err := json.Unmarshal([]byte(norm(w.Body.String())), &out); err != nil {
			t.Fatalf("%s %s : %v (%s)", method, path, err, w.Body.String())
		}
		return parityDictateCall{Method: method, Path: path, Body: body, Status: w.Code, Out: out}
	}
	for _, c := range []struct{ m, p, b string }{
		{"GET", "/api/dictate/config", ""},
		{"POST", "/api/dictate/config", `pas du json`},
		{"POST", "/api/dictate/config", `{"model":"ggml-base.bin"}`},
		{"POST", "/api/dictate/config", `{"model":"parakeet-tdt-0.6b-v2"}`},
		{"GET", "/api/dictate/config", ""},
		{"POST", "/api/dictate/config", `{"model":"parakeet-tdt-0.6b-v3","reactivity":"long","autre":1}`},
		{"GET", "/api/dictate/config", ""},
		{"GET", "/api/dictate/models", ""},
		{"GET", "/api/dictate/models/download", ""},
		{"POST", "/api/dictate/models/download", `{"id":" inconnu "}`},
		{"POST", "/api/dictate/models/download", `x`},
		{"GET", "/api/dictate/state", ""},
		{"POST", "/api/transcribe", `trop court`},
	} {
		fx.Calls = append(fx.Calls, call(c.m, c.p, c.b))
	}

	got, err := parityJSON(fx)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityDictateFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityDictateFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityDictateFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("la dictée ne produit plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteDictee,\n"+
			"puis cargo test --test dictate dit si le port Rust suit.\n--- obtenu ---\n%s", parityDictateFile, got)
	}
}
