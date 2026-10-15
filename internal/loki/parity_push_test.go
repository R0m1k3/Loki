package loki

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// parity_push_test.go — journal de référence des notifications Web Push (lot
// 12 de la migration Rust) : clé VAPID (générée une fois, stable), abonnement,
// désabonnement, et le texte du corps de notification. Rangé dans
// rust/loki-rs/tests/parity_push/push.json ; tests/push.rs le rejoue par la
// façade. Les clés tirées au hasard sont masquées (seule leur forme compte).
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteNotifications

const parityPushFile = "../../rust/loki-rs/tests/parity_push/push.json"

type parityPushCall struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"body,omitempty"`
	Status int    `json:"status"`
	Out    any    `json:"out"`
	// Après l'appel : les abonnements enregistrés (JSON brut, tel qu'en base).
	Subs string `json:"subs"`
}

type parityPushFixture struct {
	Durations map[string]string `json:"durations"`
	Calls     []parityPushCall  `json:"calls"`
}

func TestPariteNotifications(t *testing.T) {
	testHome(t)
	var fx parityPushFixture
	fx.Durations = map[string]string{}
	for _, ms := range []int64{0, 400, 600, 42_000, 185_000, 3_600_000, 4_330_000, -5_000} {
		fx.Durations[strings.TrimSpace(time.Duration(ms*int64(time.Millisecond)).String())] = fmtDurFR(time.Duration(ms) * time.Millisecond)
	}
	keyRe := regexp.MustCompile(`"key":"[A-Za-z0-9_-]{87}"`)
	call := func(method, path, body string) parityPushCall {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		switch path {
		case "/api/push/key":
			handlePushKey(w, r)
		case "/api/push/subscribe":
			handlePushSubscribe(w, r)
		case "/api/push/unsubscribe":
			handlePushUnsubscribe(w, r)
		default:
			t.Fatalf("route inattendue %s", path)
		}
		out := keyRe.ReplaceAllString(w.Body.String(), `"key":"{{CLE}}"`)
		c := parityPushCall{Method: method, Path: path, Body: body, Status: w.Code, Subs: string(getBytes(bkState, stPushSubs))}
		if err := json.Unmarshal([]byte(out), &c.Out); err != nil {
			t.Fatalf("%s : %v (%s)", path, err, w.Body.String())
		}
		return c
	}
	for _, c := range []struct{ m, p, b string }{
		{"GET", "/api/push/key", ""},
		{"POST", "/api/push/subscribe", `{"endpoint":"https://push.exemple.fr/a","keys":{"p256dh":"BAAA","auth":"x1"}}`},
		{"POST", "/api/push/subscribe", `{"endpoint":"https://push.exemple.fr/b","keys":{"p256dh":"BBBB","auth":"x2"},"expirationTime":null}`},
		{"POST", "/api/push/subscribe", `{"endpoint":"https://push.exemple.fr/a","keys":{"p256dh":"BCCC","auth":"x3"}}`},
		{"POST", "/api/push/subscribe", `{"keys":{"p256dh":"B","auth":"y"}}`},
		{"POST", "/api/push/subscribe", `pas du json`},
		{"POST", "/api/push/unsubscribe", `{"endpoint":"https://push.exemple.fr/inconnu"}`},
		{"POST", "/api/push/unsubscribe", `{"endpoint":"https://push.exemple.fr/a"}`},
		{"POST", "/api/push/unsubscribe", `pas du json`},
		{"POST", "/api/push/unsubscribe", `{"endpoint":"https://push.exemple.fr/b"}`},
		{"GET", "/api/push/key", ""},
	} {
		fx.Calls = append(fx.Calls, call(c.m, c.p, c.b))
	}
	// La paire VAPID est stable : la clé servie est la clé rangée.
	if pub := getStr(bkState, stPushVAPIDPub); len(pub) != 87 || len(getStr(bkState, stPushVAPIDPriv)) != 43 {
		t.Fatalf("paire VAPID inattendue : %q", pub)
	}

	got, err := parityJSON(fx)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityPushFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityPushFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityPushFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("les notifications ne produisent plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteNotifications,\n"+
			"puis cargo test --test push dit si le port Rust suit.\n--- obtenu ---\n%s", parityPushFile, got)
	}
}
