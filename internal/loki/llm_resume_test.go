package loki

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Flux coupé vers une API EXTERNE après un début de réponse : le tour reprend,
// le modèle reçoit le texte déjà affiché suivi d'un « continue », et la suite
// s'ajoute à l'écran sans doublon ni erreur.
func TestRepriseApresCoupureAPIExterne(t *testing.T) {
	testHome(t)
	var n int32
	var second []Message
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(sseChunk("Bonjour ")))
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		var body struct {
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		second = body.Messages
		w.WriteHeader(200)
		_, _ = w.Write([]byte(sseChunk("le monde")))
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	if err := WriteConfig(parseEnv(externalPresetContent(srv.URL, "m", "", "", false))); err != nil {
		t.Fatal(err)
	}

	var content strings.Builder
	var gotErr error
	_, err := runChat(context.Background(), []Message{{Role: "user", Content: "salut"}}, 0.7, Caps{}, func(ev StreamEvent) bool {
		if ev.Err != nil {
			gotErr = ev.Err
		}
		content.WriteString(ev.Content)
		return true
	})
	if err != nil || gotErr != nil {
		t.Fatalf("coupure externe non reprise : err=%v, ev=%v", err, gotErr)
	}
	if got := content.String(); got != "Bonjour le monde" {
		t.Fatalf("texte affiché = %q", got)
	}
	if len(second) < 3 || second[len(second)-2].Role != "assistant" || second[len(second)-2].Content != "Bonjour " ||
		second[len(second)-1].Role != "user" {
		t.Fatalf("la reprise n'a pas rendu le début de réponse au modèle : %+v", second)
	}
}

// Deux appels d'outils parallèles livrés chacun en position 0 de son chunk : le
// champ index les sépare. Avant, ils fusionnaient en un seul appel aux
// arguments collés « {…}{…} ».
func TestAppelsParallelesSuiventLIndex(t *testing.T) {
	withWorkspace(t)
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if atomic.AddInt32(&n, 1) == 1 {
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\"*.go\"}"}}]}}]}` + "\n\n"))
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","type":"function","function":{"name":"grep","arguments":"{\"pattern\":\"rien\"}"}}]}}]}` + "\n\n"))
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"))
		} else {
			_, _ = w.Write([]byte(sseChunk("fini")))
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}

	var done []string
	extra, err := runChat(context.Background(), []Message{{Role: "user", Content: "cherche"}}, 0.7, Caps{Agent: true, Code: true}, func(ev StreamEvent) bool {
		if ev.ToolUsed != nil && ev.ToolUsed.Done {
			done = append(done, ev.ToolUsed.Name)
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 2 || done[0] != "glob" || done[1] != "grep" {
		t.Fatalf("appels exécutés = %v, attendu [glob grep]", done)
	}
	if len(extra) == 0 || len(extra[0].ToolCalls) != 2 {
		t.Fatalf("message assistant : %+v", extra)
	}
}

// Serveur sans `timings` (API tierce) : le débit de décodage est mesuré côté
// Loki au lieu de rester à zéro.
func TestDebitMesureSansTimings(t *testing.T) {
	testHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, s := range []string{"un ", "deux ", "trois"} {
			_, _ = w.Write([]byte(sseChunk(s)))
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}` + "\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	if err := WriteConfig(parseEnv(externalPresetContent(srv.URL, "m", "", "", false))); err != nil {
		t.Fatal(err)
	}
	var last *StatsEvent
	if _, err := runChat(context.Background(), []Message{{Role: "user", Content: "compte"}}, 0.7, Caps{}, func(ev StreamEvent) bool {
		if ev.Stats != nil {
			last = ev.Stats
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if last == nil || last.GenTokens != 3 || last.GenPerSecond <= 0 {
		t.Fatalf("stats sans timings = %+v", last)
	}
}

// Un refus qui n'a rien d'un appel d'outil mal formé (401 d'une API externe)
// remonte tel quel : avant, n'importe quel statut coupait les outils et
// rejouait le tour, masquant la vraie cause derrière une réponse sans outils.
func TestRefusAPIHors500NeCoupePasLesOutils(t *testing.T) {
	testHome(t)
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided"}}`))
	}))
	t.Cleanup(srv.Close)
	if err := WriteConfig(parseEnv(externalPresetContent(srv.URL, "m", "", "", false))); err != nil {
		t.Fatal(err)
	}
	_, err := runChat(context.Background(), []Message{{Role: "user", Content: "salut"}}, 0.7, Caps{Agent: true}, func(StreamEvent) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("erreur = %v, attendu le 401 de l'API", err)
	}
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("%d requêtes : un 401 a été rejoué sans outils", got)
	}
}
