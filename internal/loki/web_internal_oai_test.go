package loki

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func appelOAIInterne(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set(internalTokenHeader, jetonTest)
	w := httptest.NewRecorder()
	withInternalAPI(http.NotFoundHandler()).ServeHTTP(w, r)
	return w
}

func enVol() int {
	engineGate.mu.Lock()
	defer engineGate.mu.Unlock()
	return engineGate.inflight
}

// Une requête /v1 relayée par Rust compte « en vol » de begin à end, comme
// le defer engineProxyBegin(r, side)() d'oaiHandler.
func TestOAIInterneTicket(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	avant := enVol()
	w := appelOAIInterne(t, "POST", "/internal/v1/oai/begin", `{"read":false,"side":false,"post_v1":true}`)
	var got struct {
		Ticket uint64         `json:"ticket"`
		Refuse map[string]any `json:"refuse"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Ticket == 0 || got.Refuse != nil {
		t.Fatalf("begin : %d %s", w.Code, w.Body.String())
	}
	if enVol() != avant+1 || oaiInflight() != 1 {
		t.Fatalf("en vol : %d (avant %d), tickets %d", enVol(), avant, oaiInflight())
	}
	body, _ := json.Marshal(map[string]any{"ticket": got.Ticket})
	if w := appelOAIInterne(t, "POST", "/internal/v1/oai/end", string(body)); w.Code != 204 {
		t.Fatalf("end : %d", w.Code)
	}
	if enVol() != avant || oaiInflight() != 0 {
		t.Fatalf("après end : en vol %d, tickets %d", enVol(), oaiInflight())
	}
	if w := appelOAIInterne(t, "POST", "/internal/v1/oai/end", string(body)); w.Code != 404 {
		t.Fatalf("second end : %d", w.Code)
	}
}

// Pendant un benchmark, une complétion externe est refusée — et ne reste pas
// comptée en vol.
func TestOAIInterneRefusBenchmark(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	benchJob.mu.Lock()
	benchJob.running = true
	benchJob.mu.Unlock()
	t.Cleanup(func() {
		benchJob.mu.Lock()
		benchJob.running = false
		benchJob.mu.Unlock()
	})
	avant := enVol()
	w := appelOAIInterne(t, "POST", "/internal/v1/oai/begin", `{"post_v1":true}`)
	var got struct {
		Refuse struct {
			Status     int    `json:"status"`
			Code       string `json:"code"`
			RetryAfter string `json:"retry_after"`
		} `json:"refuse"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Refuse.Status != 503 || got.Refuse.Code != "benchmark_running" || got.Refuse.RetryAfter != "30" {
		t.Fatalf("refus : %s", w.Body.String())
	}
	if enVol() != avant || oaiInflight() != 0 {
		t.Fatalf("refus compté en vol : %d", enVol())
	}
}

func TestOAIInterneRoute(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	if err := putStr(bkConfig, "PORT", "8099"); err != nil {
		t.Fatal(err)
	}
	if err := putStr(bkState, "api_key", "cle-oai"); err != nil {
		t.Fatal(err)
	}
	w := appelOAIInterne(t, "GET", "/internal/v1/oai/route?method=GET&path=/v1/models", "")
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["port"] != float64(8099) || got["key"] != "cle-oai" || got["key_ok"] != true || got["side_live"] != false {
		t.Fatalf("route : %s", w.Body.String())
	}
}
