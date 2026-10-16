package loki

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeLSP : un serveur LSP réduit à ce que check utilise. Pour chaque
// didOpen/didChange reçu, react(version, publish) décide des publications.
type fakeLSP struct {
	s   *lspServer
	out *io.PipeWriter
}

func lspFrame(w io.Writer, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(b), b)
}

func newFakeLSP(t *testing.T, react func(ver int, publish func(ver *int, diags []lspDiagnostic))) *fakeLSP {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &lspServer{
		lang: "go", root: t.TempDir(), stdin: inW,
		version: map[string]int{}, diags: map[string][]lspDiagnostic{}, diagVer: map[string]int{},
		diagCh: make(chan string, 16), lastUsed: time.Now(),
	}
	go s.readLoop(outR)
	go func() {
		br := bufio.NewReader(inR)
		for {
			n := 0
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimSpace(line)
				if line == "" {
					break
				}
				if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
					fmt.Sscanf(strings.TrimSpace(v), "%d", &n)
				}
			}
			buf := make([]byte, n)
			if _, err := io.ReadFull(br, buf); err != nil {
				return
			}
			var msg struct {
				Method string `json:"method"`
				Params struct {
					TextDocument struct {
						URI     string `json:"uri"`
						Version int    `json:"version"`
					} `json:"textDocument"`
				} `json:"params"`
			}
			if json.Unmarshal(buf, &msg) != nil {
				continue
			}
			if msg.Method != "textDocument/didOpen" && msg.Method != "textDocument/didChange" {
				continue
			}
			uri := msg.Params.TextDocument.URI
			go react(msg.Params.TextDocument.Version, func(ver *int, diags []lspDiagnostic) {
				params := map[string]any{"uri": uri, "diagnostics": diags}
				if ver != nil {
					params["version"] = *ver
				}
				lspFrame(outW, map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics", "params": params})
			})
		}
	}()
	t.Cleanup(func() { inW.Close(); outW.Close() })
	return &fakeLSP{s: s, out: outW}
}

func lspErr(msg string) []lspDiagnostic {
	d := lspDiagnostic{Severity: 1, Message: msg}
	return []lspDiagnostic{d}
}

func lspTempFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(p, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func withLSPQuiet(t *testing.T, d time.Duration) {
	old := lspDiagQuiet
	lspDiagQuiet = d
	t.Cleanup(func() { lspDiagQuiet = old })
}

// Un signal laissé dans le tampon par une republication tardive ne doit pas
// faire rendre « aucun diagnostic » à l'édition suivante.
func TestLSPCheckIgnoresStaleSignal(t *testing.T) {
	withLSPQuiet(t, 80*time.Millisecond)
	p := lspTempFile(t)
	f := newFakeLSP(t, func(ver int, publish func(*int, []lspDiagnostic)) {
		time.Sleep(60 * time.Millisecond)
		publish(&ver, lspErr("undefined: x"))
	})
	uri := fileURI(p)
	f.s.version[uri] = 1 // document déjà ouvert
	f.s.diagCh <- uri    // republication tardive du check précédent
	f.s.diags[uri] = nil // …dont la liste était vide
	d, ok := f.s.check(p)
	if !ok || len(d) != 1 || d[0].Message != "undefined: x" {
		t.Fatalf("diagnostics = %+v, %v ; attendu l'erreur de la nouvelle version", d, ok)
	}
}

// Une liste vide publiée d'abord (« clear ») puis l'analyse : c'est l'analyse
// qui compte.
func TestLSPCheckWaitsPastInitialClear(t *testing.T) {
	withLSPQuiet(t, 150*time.Millisecond)
	p := lspTempFile(t)
	f := newFakeLSP(t, func(ver int, publish func(*int, []lspDiagnostic)) {
		publish(&ver, nil)
		time.Sleep(60 * time.Millisecond)
		publish(&ver, lspErr("missing return"))
	})
	d, ok := f.s.check(p)
	if !ok || len(d) != 1 || d[0].Message != "missing return" {
		t.Fatalf("diagnostics = %+v, %v ; attendu l'erreur publiée après la liste vide", d, ok)
	}
}

// Une publication d'une version antérieure, arrivée en retard, est ignorée.
func TestLSPCheckIgnoresOlderVersion(t *testing.T) {
	withLSPQuiet(t, 80*time.Millisecond)
	p := lspTempFile(t)
	f := newFakeLSP(t, func(ver int, publish func(*int, []lspDiagnostic)) {
		old := ver - 1
		publish(&old, lspErr("ancienne erreur"))
		time.Sleep(40 * time.Millisecond)
		publish(&ver, nil)
	})
	f.s.version[fileURI(p)] = 3
	d, ok := f.s.check(p)
	if !ok || len(d) != 0 {
		t.Fatalf("diagnostics = %+v, %v ; attendu aucun (seule la version 4 compte)", d, ok)
	}
}

// Sans version dans la publication (champ facultatif), elle est prise.
func TestLSPCheckAcceptsUnversionedPublish(t *testing.T) {
	withLSPQuiet(t, 50*time.Millisecond)
	p := lspTempFile(t)
	f := newFakeLSP(t, func(ver int, publish func(*int, []lspDiagnostic)) {
		publish(nil, lspErr("syntax error"))
	})
	start := time.Now()
	d, ok := f.s.check(p)
	if !ok || len(d) != 1 {
		t.Fatalf("diagnostics = %+v, %v", d, ok)
	}
	if time.Since(start) > lspDiagWait {
		t.Fatalf("check a attendu la borne au lieu du calme")
	}
}

// Une écriture ratée vers le serveur ne bloque plus check (markDead prenait
// mu, déjà tenu par check).
func TestLSPCheckWriteFailureDoesNotDeadlock(t *testing.T) {
	p := lspTempFile(t)
	r, w := io.Pipe()
	r.Close() // toute écriture échoue
	s := &lspServer{
		lang: "go", stdin: w,
		version: map[string]int{}, diags: map[string][]lspDiagnostic{}, diagVer: map[string]int{},
		diagCh: make(chan string, 16),
	}
	done := make(chan struct{})
	go func() {
		s.check(p)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(lspDiagWait + 2*time.Second):
		t.Fatal("check bloqué après une écriture ratée")
	}
	if !s.dead.Load() {
		t.Fatal("serveur pas marqué mort après une écriture ratée")
	}
}
