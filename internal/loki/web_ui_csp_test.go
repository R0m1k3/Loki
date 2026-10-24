package loki

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// L'UI part avec sa CSP, et la façade Rust (qui sert la même UI) pose la même.
func TestIndexServesCSP(t *testing.T) {
	rec := httptest.NewRecorder()
	handleIndex(rec, httptest.NewRequest("GET", "/", nil))
	got := rec.Header().Get("Content-Security-Policy")
	if got != uiCSP {
		t.Fatalf("CSP = %q", got)
	}
	for _, d := range []string{"connect-src 'self'", "object-src 'none'", "base-uri 'self'", "frame-ancestors 'self'"} {
		if !strings.Contains(got, d) {
			t.Errorf("CSP sans %q", d)
		}
	}
	rs, err := os.ReadFile("../../rust/loki-rs/src/ui.rs")
	if err != nil {
		t.Skip("source Rust absente :", err)
	}
	if !strings.Contains(string(rs), `"`+uiCSP+`"`) {
		t.Fatal("UI_CSP de rust/loki-rs/src/ui.rs diffère de uiCSP")
	}
}

// DOMPurify est embarqué et chargé AVANT le script de l'UI (md() en dépend).
func TestUIShipsDOMPurify(t *testing.T) {
	if _, err := uiFS.ReadFile("ui/purify.min.js"); err != nil {
		t.Fatal(err)
	}
	b, _ := uiFS.ReadFile("ui/index.html")
	s := string(b)
	i := strings.Index(s, `<script src="purify.min.js"></script>`)
	if i < 0 || i > strings.Index(s, "DOMPurify.sanitize(") {
		t.Fatal("purify.min.js absent de index.html ou chargé après l'UI")
	}
}
