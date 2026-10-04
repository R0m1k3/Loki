package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// stubSideProps remplace la lecture de /props : slots, fenêtre d'un slot.
func stubSideProps(t *testing.T, slots, nCtx int) {
	t.Helper()
	prev := sideSlotProps
	sideSlotProps = func(chatEndpoint) (int, int, bool) { return slots, nCtx, true }
	reset := func() {
		sideSlotProbe.mu.Lock()
		sideSlotProbe.key, sideSlotProbe.live = "", false
		sideSlotProbe.mu.Unlock()
	}
	reset()
	t.Cleanup(func() { sideSlotProps = prev; reset() })
}

// slotOf : l'id_slot d'un corps de requête ; -1 = absent.
func slotOf(t *testing.T, body string) int {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("corps illisible : %v", err)
	}
	v, ok := p["id_slot"]
	if !ok {
		return -1
	}
	return int(v.(float64))
}

// Le tour, une vérification puis un résumé sur transcription : un corps par
// requête, dans l'ordre.
func sideSlotScenario(t *testing.T, m *moteurCont) []string {
	t.Helper()
	ctx := withPerf(context.Background(), perfMain, convActiveID())
	if _, err := runChat(ctx, []Message{um("bonjour")}, 0.2, Caps{}, func(StreamEvent) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if _, err := runChat(withPerfKind(ctx, perfVerify), []Message{um("vérifie")}, 0.2, Caps{}, func(StreamEvent) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if _, err := summarizeTranscriptFor(ctx, "user: a\nassistant: b", false); err != nil {
		t.Fatal(err)
	}
	b := m.all()
	if len(b) != 3 {
		t.Fatalf("%d requêtes, attendu 3", len(b))
	}
	return b
}

// Sans la clé : aucun id_slot, et /props n'est même pas lu.
func TestSideSlotRoutageDefaut(t *testing.T) {
	m := contSetup(t, nil)
	prev := sideSlotProps
	sideSlotProps = func(chatEndpoint) (int, int, bool) { t.Fatal("/props lu sans la clé"); return 0, 0, false }
	t.Cleanup(func() { sideSlotProps = prev })
	for _, b := range sideSlotScenario(t, m) {
		if strings.Contains(b, "id_slot") {
			t.Fatalf("id_slot sans la clé : %s", b)
		}
	}
}

// Avec deux slots en service : le tour sur le 0, la vérification et la
// transcription sur le 1 — et le slot 0 porte toujours le tour après elles.
func TestSideSlotRoutage(t *testing.T) {
	m := contSetup(t, map[string]string{"SIDE_SLOT": "on", "COMPACT_CONTINUATION": "on"})
	stubSideProps(t, 2, 8192)
	b := sideSlotScenario(t, m)
	for i, want := range []int{0, 1, 1} {
		if got := slotOf(t, b[i]); got != want {
			t.Errorf("requête %d : id_slot %d, attendu %d", i, got, want)
		}
	}
	if !engineSlotHolds(convActiveID()) {
		t.Fatal("le slot 0 n'a pas bougé : le tampon du tour doit tenir après le second slot")
	}
}

// Un seul slot, une fenêtre trop petite (lancement refusé, PARALLEL=2 seul) ou
// un preset externe : aucun id_slot.
func TestSideSlotPasEnService(t *testing.T) {
	testHome(t)
	if err := SetConfigKey("SIDE_SLOT", "on"); err != nil {
		t.Fatal(err)
	}
	if err := SetConfigKey("CTX", "8192"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ slots, nCtx int }{{1, 8192}, {2, 4096}, {4, 8192}} {
		stubSideProps(t, c.slots, c.nCtx)
		if s := engineSlotFor(chatEndpoint{}, false); s != -1 {
			t.Errorf("%d slots de %d : id_slot %d", c.slots, c.nCtx, s)
		}
	}
	stubSideProps(t, 2, 8448) // arrondi de llama.cpp au multiple de 256
	if engineSlotFor(chatEndpoint{}, true) != 0 || engineSlotFor(chatEndpoint{}, false) != 1 {
		t.Fatal("deux slots de CTX jetons : routage attendu")
	}
	if s := engineSlotFor(chatEndpoint{External: true}, false); s != -1 {
		t.Fatalf("preset externe : id_slot %d", s)
	}
}

func TestSideSlotPoolError(t *testing.T) {
	for msg, want := range map[string]bool{
		`{"error":{"code":500,"message":"Context size has been exceeded.","type":"server_error"}}`:                                                               true,
		`{"error":{"message":"request (40000 tokens) exceeds the available context size (32768 tokens), try increasing it","type":"exceed_context_size_error"}}`: false,
		`the request exceeds the available context size`:                                                                                                         false,
		`failed to parse tool call`: false,
	} {
		if got := sideSlotPoolError(msg); got != want {
			t.Errorf("%q : %v", msg, got)
		}
	}
}

// Cache KV plein en plein calcul, deux slots en service : la requête est
// rejouée telle quelle, sans compaction ni réduction.
func TestSideSlotPoolErrorRejoue(t *testing.T) {
	testHome(t)
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		n := len(bodies)
		mu.Unlock()
		if n == 1 {
			sendJSON(w, 500, map[string]any{"error": map[string]any{"message": "Context size has been exceeded."}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseChunk("ok") + sseFinal("stop", 50, 1) + "data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	for k, v := range map[string]string{"PORT": port, "SIDE_SLOT": "on", "CTX": "8192"} {
		if err := SetConfigKey(k, v); err != nil {
			t.Fatal(err)
		}
	}
	stubSideProps(t, 2, 8192)
	msgs := []Message{um(strings.Repeat("x", 30000))} // près de la fenêtre : l'estimation crierait au débordement
	if _, err := runChat(withPerf(context.Background(), perfMain, "c"), msgs, 0.2, Caps{}, func(StreamEvent) bool { return true }); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("%d requêtes ; rejeu identique attendu", len(bodies))
	}
}

// Client /v1 : son corps part avec id_slot 1, quoi qu'il demande. Sans la clé,
// il passe octet pour octet.
func TestSideSlotProxyForceLeSecondSlot(t *testing.T) {
	testHome(t)
	var got []byte
	fauxMoteur(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		if r.ContentLength != int64(len(got)) {
			t.Errorf("Content-Length %d pour %d octets", r.ContentLength, len(got))
		}
		sendJSON(w, 200, map[string]any{"ok": true})
	})
	in := `{"model":"x","id_slot":0,"seed":12345678901234567890,"messages":[{"role":"user","content":"a"}]}`
	call := func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(in)))
		rec := httptest.NewRecorder()
		oaiHandler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("statut %d : %s", rec.Code, rec.Body.String())
		}
	}
	call()
	if string(got) != in {
		t.Fatalf("corps réécrit sans la clé : %s", got)
	}
	if err := SetConfigKey("SIDE_SLOT", "on"); err != nil {
		t.Fatal(err)
	}
	stubSideProps(t, 2, 32768)
	call()
	if slotOf(t, string(got)) != 1 || !strings.Contains(string(got), "12345678901234567890") {
		t.Fatalf("corps routé : %s", got)
	}
}

// Deux slots de SIDE_SLOT : le préchauffage vise le slot de la discussion.
// Deux slots sans la clé : toujours rien.
func TestSideSlotPrewarmSurLeSlot0(t *testing.T) {
	m := prewarmSetup(t, map[string]string{"PREWARM": "on", "SIDE_SLOT": "on", "CTX": "8192"})
	c := newTestConv()
	prewarmNoteCaps(convActiveID(), Caps{Agent: true})
	c.mu.Lock()
	c.Messages = []Message{um("bonjour"), am("salut")}
	c.mu.Unlock()
	m.mu.Lock()
	m.slots = 2
	m.mu.Unlock()
	stubSideProps(t, 2, 4096) // lancement refusé : fenêtre partagée
	c.prewarm(prewarmTurnEnd)
	if n := len(m.completions()); n != 0 {
		t.Fatalf("%d préchauffage(s) sans second slot en service", n)
	}
	stubSideProps(t, 2, 8192)
	c.prewarm(prewarmTurnEnd)
	got := m.completions()
	if len(got) != 1 {
		t.Fatalf("%d préchauffage(s), attendu 1", len(got))
	}
	if s := slotOf(t, got[0]); s != 0 {
		t.Fatalf("id_slot = %d, attendu 0", s)
	}
}
