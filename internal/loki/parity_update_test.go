package loki

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parity_update_test.go — journal de référence de la mise à jour de Loki en
// conteneur (lot 21 de la migration Rust) : l'image se met à jour par Docker,
// les deux routes le disent. Rangé dans
// rust/loki-rs/tests/parity_update/update.json ; tests/sysupdate.rs le rejoue
// par la façade.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteMiseAJour

const parityUpdateFile = "../../rust/loki-rs/tests/parity_update/update.json"

func TestPariteMiseAJour(t *testing.T) {
	testHome(t)
	t.Setenv("LOKI_CONTAINER", "1")
	var steps []map[string]any
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/update", ""},
		{"POST", "/api/update", "{}"},
		{"POST", "/api/update/apply", "{}"},
		{"GET", "/api/update/apply", ""},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		if c.path == "/api/update" {
			handleUpdateCheck(rec, req)
		} else {
			handleUpdateApply(rec, req)
		}
		var out any
		_ = json.Unmarshal([]byte(strings.ReplaceAll(rec.Body.String(), Version, "{{VERSION}}")), &out)
		steps = append(steps, map[string]any{"method": c.method, "path": c.path, "status": rec.Code, "out": out})
	}
	got, err := parityJSON(map[string]any{"steps": steps})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityUpdateFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityUpdateFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityUpdateFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("la mise à jour en conteneur ne produit plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteMiseAJour.\n--- obtenu ---\n%s",
			parityUpdateFile, got)
	}
}
