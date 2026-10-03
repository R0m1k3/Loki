package loki

import (
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// chunked découpe s en morceaux de 1 à max octets (graine fixe : reproductible).
func chunked(s string, r *rand.Rand, max int) []string {
	var out []string
	for len(s) > 0 {
		n := 1 + r.Intn(max)
		if n > len(s) {
			n = len(s)
		}
		out = append(out, s[:n])
		s = s[n:]
	}
	return out
}

// checkArgPreview rejoue args morceau par morceau et compare à chaque étape
// argPreview au previewArgDone historique — à une barre oblique finale près,
// qu'argPreview garde en attente au lieu de l'écrire.
func checkArgPreview(t *testing.T, args, key string, chunks []string) {
	t.Helper()
	a := newArgPreview(key)
	prefix := ""
	for _, c := range chunks {
		prefix += c
		a.update(prefix)
		want, wantDone := previewArgDone(prefix, key)
		got := a.value()
		if a.phase == 3 && a.pos == len(prefix)-1 && prefix[a.pos] == '\\' {
			got += "\\"
		}
		if got != want || a.done() != wantDone {
			t.Fatalf("args=%q préfixe=%q : argPreview=(%q,%v), previewArgDone=(%q,%v)", args, prefix, got, a.done(), want, wantDone)
		}
		if n := strings.Count(a.value(), "\n"); n != a.lines {
			t.Fatalf("lines=%d, la valeur en compte %d", a.lines, n)
		}
	}
}

func TestArgPreviewMatchesPreviewArgDone(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	alphabet := []string{"a", "é", "\n", "\t", "\r", "\\", "\"", "<", "&", " ", "/", " ", "x", "content", ":", "{"}
	var cases []string
	for i := 0; i < 300; i++ {
		var b strings.Builder
		for j := r.Intn(200); j > 0; j-- {
			b.WriteString(alphabet[r.Intn(len(alphabet))])
		}
		v := b.String()
		m, _ := json.Marshal(map[string]string{"file": "a/b.go", "content": v})
		cases = append(cases, string(m))
		// Ordre inverse et clé citée avant la vraie : même règle de première occurrence.
		cases = append(cases, `{"content"  :  "`+strings.ReplaceAll(v, `"`, `\"`)+`","file":"x"}`)
		cases = append(cases, `{"note":"le champ \"content\" vient après","content":"`+strings.ReplaceAll(v, `"`, `\"`)+`"}`)
	}
	cases = append(cases,
		`{"content":"été \\u0041 fin"}`,
		`{"content":"ligne1\nligne2\\`,
		`{"content":`, `{"cont`, `{"content":"`, `{"content":"x"}`, ``,
	)
	for _, args := range cases {
		for _, max := range []int{1, 3, 17, 4096} {
			checkArgPreview(t, args, "content", chunked(args, r, max))
		}
	}
}

func FuzzArgPreview(f *testing.F) {
	f.Add(`{"file":"a","content":"x\ny\\\"zA"}`, int64(1))
	f.Add(`{"content":"a\\`, int64(2))
	f.Add(`{"content" : "x", "content":"y"}`, int64(3))
	f.Fuzz(func(t *testing.T, args string, seed int64) {
		checkArgPreview(t, args, "content", chunked(args, rand.New(rand.NewSource(seed)), 9))
	})
}

func TestBodyTail(t *testing.T) {
	lines := func(n int, w int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(strings.Repeat("x", w) + strconv.Itoa(i) + "\n")
		}
		return b.String()
	}
	long := strings.Repeat("é", 5000) // une seule ligne de 10 000 octets
	for _, tc := range []struct {
		name    string
		in      string
		cut     bool
		maxLine int
	}{
		{"vide", "", false, 0},
		{"court", "a\nb\nc", false, 3},
		{"40 lignes pile", lines(39, 3) + "fin", false, 40},
		{"beaucoup de lignes", lines(500, 3) + "fin", true, bodyTailLines},
		{"lignes larges", lines(100, 300), true, bodyTailLines},
		{"ligne seule énorme", long, true, 1},
	} {
		got, cut := bodyTail(tc.in)
		if cut != tc.cut || !strings.HasSuffix(tc.in, got) {
			t.Errorf("%s : cut=%v, suffixe=%v", tc.name, cut, strings.HasSuffix(tc.in, got))
		}
		if len(got) > bodyTailBytes {
			t.Errorf("%s : %d octets", tc.name, len(got))
		}
		if n := strings.Count(got, "\n") + 1; got != "" && n > tc.maxLine {
			t.Errorf("%s : %d lignes", tc.name, n)
		}
		if cut {
			if start := len(tc.in) - len(got); tc.in[start-1] != '\n' && !strings.Contains(tc.name, "seule") {
				t.Errorf("%s : coupé en milieu de ligne", tc.name)
			}
			if got != "" && !json.Valid([]byte(strconv.Quote(got))) || strings.ContainsRune(got, '�') {
				t.Errorf("%s : caractère coupé", tc.name)
			}
		}
	}
}

func TestBodyLineCount(t *testing.T) {
	for in, want := range map[string]int{"": 0, "\n": 0, "a": 1, "a\n": 1, "a\nb": 2, "a\nb\n": 2, "\n\n": 2} {
		if got := bodyLineCount(in); got != want {
			t.Errorf("bodyLineCount(%q) = %d, attendu %d", in, got, want)
		}
	}
}

// Les événements de frappe d'un gros write ne portent qu'une fin bornée du
// corps, avec le vrai nombre de lignes ; les arguments exécutés restent entiers.
func TestWriteTypingBodyBounded(t *testing.T) {
	var content strings.Builder
	for i := 0; i < 3000; i++ {
		content.WriteString("ligne numéro " + strconv.Itoa(i) + " du fichier\n")
	}
	withWorkspace(t)
	args, _ := json.Marshal(map[string]string{"file": "gros.txt", "content": content.String()})
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&calls, 1) > 1 {
			_, _ = w.Write([]byte(sseChunk("fini")))
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
			return
		}
		s := string(args)
		for len(s) > 0 {
			n := 37
			if n > len(s) {
				n = len(s)
			}
			for n < len(s) && !utf8.RuneStart(s[n]) {
				n++
			}
			piece, _ := json.Marshal(s[:n])
			s = s[n:]
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"w1","type":"function","function":{"name":"write","arguments":` + string(piece) + `}}]}}]}` + "\n\n"))
		}
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}
	var sent, last int
	var lastEv *ToolUsedEvent
	_, err := runChat(context.Background(), []Message{{Role: "user", Content: "écris"}}, 0.7, Caps{Agent: true}, func(ev StreamEvent) bool {
		if tu := ev.ToolUsed; tu != nil && tu.Typing && tu.Body != "" {
			if len(tu.Body) > bodyTailBytes {
				t.Fatalf("corps de %d octets dans un événement de frappe", len(tu.Body))
			}
			if tu.BodyLines < last {
				t.Fatalf("body_lines recule : %d après %d", tu.BodyLines, last)
			}
			last = tu.BodyLines
			sent += len(tu.Body)
			lastEv = tu
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if lastEv == nil || !lastEv.BodyTail || lastEv.BodyLines < 2990 {
		t.Fatalf("dernier événement = %+v", lastEv)
	}
	if !strings.HasSuffix(content.String(), lastEv.Body) {
		t.Fatalf("la fin diffusée n'est pas la fin du corps : %q", lastEv.Body)
	}
	// Linéaire : 3000 lignes × 4 Kio au plus, loin des centaines de Mo d'avant.
	if sent > 3000*bodyTailBytes {
		t.Fatalf("%d octets de corps diffusés", sent)
	}
	// Ce qui est exécuté, lui, est entier.
	if b, err := os.ReadFile(filepath.Join(agentCwd(), "gros.txt")); err != nil || string(b) != content.String() {
		t.Fatalf("fichier écrit : %d octets, err=%v", len(b), err)
	}
}

// replayScan rejoue le balayage du flux : un morceau sans caractère déclencheur
// n'est pas balayé. Renvoie l'indice du morceau qui coupe le flux, -1 sinon.
func replayScan(chunks []string, windowed bool) int {
	var b strings.Builder
	scanned := 0
	for i, c := range chunks {
		b.WriteString(c)
		if !strings.ContainsAny(c, "<`{[_.") {
			continue
		}
		s := b.String()
		var hit bool
		if windowed {
			hit = textualToolCallFrom(s, scanned)
			scanned = len(s)
		} else {
			hit = textualToolCall(s)
		}
		if hit {
			return i
		}
	}
	return -1
}

func TestTextualToolCallWindowMatchesFullScan(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	prose := []string{
		"Voici la suite de l'analyse, ligne par ligne. ",
		"Le fichier contient trois fonctions ; aucune n'est exportée.\n",
		"```go\nfunc main() { fmt.Println(\"x\") }\n```\n",
		"- point important\n",
		"le JSON {\"name\": \"loki\"} de config, ",
		"sans ponctuation du tout mais avec des mots très longs ",
		"\n\n",
		"Un <div class=\"x\"> en HTML. ",
		"été à la plage ",
	}
	filler := func(n int) string {
		var b strings.Builder
		for b.Len() < n {
			b.WriteString(prose[r.Intn(len(prose))])
		}
		return b.String()
	}
	bad := []string{
		"<tool_call>{\"name\":\"bash\"}</tool_call>",
		"\n{\"name\": \"bash\", \"arguments\": {}}",
		"\n   \n  {\"name\":" + strings.Repeat(" \n", 400) + "\"mem_" + strings.Repeat("a", 700) + "\"",
		"\n" + strings.Repeat(" ", 900) + "{\"name\": \"web_open\"",
		"<function=" + strings.Repeat("b", 900) + ">",
		" functions." + strings.Repeat("c", 600) + strings.Repeat(" ", 300) + "(",
		"Je lance default_api:bash ",
		"```tool_code\nprint(1)\n```",
		"<parameter=command>",
		"[TOOL_REQUEST]",
	}
	neutral := []string{
		" milieu de ligne {\"name\": \"bash\"} ", // `^` : seulement en début de ligne
		strings.Repeat("x", 800) + " {\"name\": \"bash\"",
		"<tool_cal> presque",
	}
	var texts []string
	for i := 0; i < 120; i++ {
		head := filler(500 + r.Intn(5000))
		tail := filler(r.Intn(1500))
		switch i % 3 {
		case 0:
			texts = append(texts, head+bad[r.Intn(len(bad))]+tail)
		case 1:
			texts = append(texts, head+neutral[r.Intn(len(neutral))]+tail)
		default:
			texts = append(texts, head+tail)
		}
	}
	// Motif à cheval sur un morceau sans caractère déclencheur.
	texts = append(texts, filler(3000)+"<tool"+"_call"+">"+" puis du texte sans rien")
	hits, misses := 0, 0
	for _, s := range texts {
		for _, max := range []int{3, 23, 200} {
			ch := chunked(s, r, max)
			if s == texts[len(texts)-1] {
				ch = append(chunked(s[:len(s)-len("<tool_call> puis du texte sans rien")], r, max), "<tool", "_call", ">", " puis du texte", " sans rien")
			}
			w, f := replayScan(ch, true), replayScan(ch, false)
			if w != f {
				t.Fatalf("fenêtré coupe au morceau %d, balayage complet au %d (taille %d, max %d)", w, f, len(s), max)
			}
			if f >= 0 {
				hits++
			} else {
				misses++
			}
		}
	}
	if hits == 0 || misses == 0 {
		t.Fatalf("corpus déséquilibré : %d coupures, %d sans", hits, misses)
	}
}

// La fenêtre est bien une fenêtre sur un texte ordinaire, et démarre en début
// de ligne.
func TestRetryScanStartWindows(t *testing.T) {
	s := strings.Repeat("Une phrase ordinaire, avec sa ponctuation.\n", 500)
	start := retryScanStart(s, len(s))
	if start == 0 || len(s)-start > 2*retryScanMargin {
		t.Fatalf("fenêtre de %d octets sur %d", len(s)-start, len(s))
	}
	if s[start-1] != '\n' {
		t.Fatal("la fenêtre ne démarre pas en début de ligne")
	}
	// Une ligne sans fin plus longue que le recul permis : balayage complet.
	if got := retryScanStart(strings.Repeat("a b ", 20000), 80000); got != 0 {
		t.Fatalf("start = %d, attendu 0", got)
	}
}

// Surcharge retry_patterns : ses motifs ne sont pas bornés, donc pas de fenêtre.
func TestTextualToolCallFromCustomFullScan(t *testing.T) {
	retryPatterns()
	saved := retryRes
	savedCustom := retryCustom
	defer func() { retryRes, retryCustom = saved, savedCustom }()
	retryRes, retryCustom = []*regexp.Regexp{regexp.MustCompile(`(?mi)\Aoups`)}, true
	s := "oups.\n" + strings.Repeat("Une phrase, puis une autre.\n", 400)
	if !textualToolCallFrom(s, len(s)-10) {
		t.Fatal("motif ancré en tête manqué")
	}
	retryCustom = false
	if textualToolCallFrom(s, len(s)-10) {
		t.Fatal("la fenêtre aurait dû le manquer (le test ne prouve rien sinon)")
	}
}

// sseStream : un lot = une écriture, les événements dans l'ordre, et un lot
// trop gros est découpé.
type countingRW struct {
	*httptest.ResponseRecorder
	writes int
}

func (c *countingRW) Write(b []byte) (int, error) {
	c.writes++
	return c.ResponseRecorder.Write(b)
}

func TestSSEStreamBatches(t *testing.T) {
	rw := &countingRW{ResponseRecorder: httptest.NewRecorder()}
	s := &sseStream{w: rw, mu: &sync.Mutex{}, frame: func(m map[string]any) ([]byte, bool) {
		return []byte("data: " + strconv.Itoa(m["seq"].(int)) + "\n\n"), true
	}}
	for i := 1; i <= 10; i++ {
		if !s.queue(map[string]any{"seq": i}) {
			t.Fatal("queue")
		}
	}
	if rw.writes != 0 {
		t.Fatalf("%d écritures avant flush", rw.writes)
	}
	s.flush()
	if rw.writes != 1 {
		t.Fatalf("%d écritures pour un lot", rw.writes)
	}
	for i := 11; i <= 10+2*sseBatchEvents; i++ {
		s.queue(map[string]any{"seq": i})
	}
	s.emit(map[string]any{"seq": 11 + 2*sseBatchEvents})
	if rw.writes != 4 {
		t.Fatalf("%d écritures, attendu 4", rw.writes)
	}
	var want strings.Builder
	for i := 1; i <= 11+2*sseBatchEvents; i++ {
		want.WriteString("data: " + strconv.Itoa(i) + "\n\n")
	}
	if rw.Body.String() != want.String() {
		t.Fatal("ordre des événements perdu")
	}
}

// Le direct passe par queue puis flush, dans l'ordre des seq.
func TestSubscribeSinkLiveBatchOrder(t *testing.T) {
	c := newTestConv()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var seqs []int
	flushed := 0
	caught := make(chan struct{}, 1)
	sink := tailSink{
		emit: func(m map[string]any) bool {
			if _, ok := m["caught_up"]; ok {
				caught <- struct{}{}
			}
			return true
		},
		queue: func(m map[string]any) bool {
			mu.Lock()
			if s, ok := m["seq"].(int); ok {
				seqs = append(seqs, s)
			}
			mu.Unlock()
			return true
		},
		flush: func() bool { mu.Lock(); flushed++; mu.Unlock(); return true },
	}
	go c.subscribeSink(ctx, 0, -1, "", sink)
	<-caught
	const n = 500
	for i := 0; i < n; i++ {
		c.appendDelta(c.epoch, map[string]any{"content": "t"})
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got, f := len(seqs), flushed
		mu.Unlock()
		if got == n && f > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d/%d événements reçus", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, s := range seqs {
		if s != i+1 {
			t.Fatalf("seq %d en position %d", s, i)
		}
	}
}

func BenchmarkWriteTypingPreview(b *testing.B) {
	var content strings.Builder
	for content.Len() < 120<<10 {
		content.WriteString("une ligne de code assez ordinaire, avec \"guillemets\"\n")
	}
	args, _ := json.Marshal(map[string]string{"file": "x", "content": content.String()})
	s := string(args)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a := newArgPreview("content")
		last := -1
		for end := 8; end <= len(s); end += 8 {
			a.update(s[:end])
			if a.lines > last {
				last = a.lines
				bodyTail(a.value())
			}
		}
	}
}

func BenchmarkTextualToolCallWindowed(b *testing.B) {
	s := strings.Repeat("Une phrase ordinaire, avec sa ponctuation. Et un point.\n", 1500)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		textualToolCallFrom(s, len(s)-8)
	}
}
