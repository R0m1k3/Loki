//go:build linux

package loki

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSuperviseurRustVoisin(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "loki")
	t.Setenv("LOKI_ENGINE_SUPERVISOR", "")
	if got := rustEngineSupervisorFor(exe); got != "" {
		t.Fatalf("sans loki-rs voisin : %q", got)
	}
	rs := filepath.Join(dir, "loki-rs")
	if err := os.WriteFile(rs, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := rustEngineSupervisorFor(exe); got != "" {
		t.Fatalf("loki-rs non exécutable retenu : %q", got)
	}
	if err := os.Chmod(rs, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := rustEngineSupervisorFor(exe); got != rs {
		t.Fatalf("loki-rs voisin : %q, attendu %q", got, rs)
	}
	t.Setenv("LOKI_ENGINE_SUPERVISOR", "go")
	if got := rustEngineSupervisorFor(exe); got != "" {
		t.Fatalf("LOKI_ENGINE_SUPERVISOR=go ignoré : %q", got)
	}
}

// Le superviseur Rust est réellement appelé, avec le bon environnement, et sa
// dernière ligne d'erreur devient l'erreur rendue à l'UI.
func TestActionDelegueeAuSuperviseurRust(t *testing.T) {
	home := testHome(t)
	dir := t.TempDir()
	trace := filepath.Join(dir, "trace")
	rs := filepath.Join(dir, "loki-rs")
	script := "#!/bin/sh\n" +
		"echo \"$1 $2 $LOKI_HOME $LOKI_SERVICE\" > " + trace + "\n" +
		"[ \"$2\" = start ] && { echo '[loki-rs engine] service loki-engine non démarré' >&2; exit 1; }\n" +
		"exit 0\n"
	if err := os.WriteFile(rs, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := rustSvcAction(rs, "stop"); err != nil {
		t.Fatalf("stop : %v", err)
	}
	b, _ := os.ReadFile(trace)
	if want := "engine stop " + home + " " + serviceName() + "\n"; string(b) != want {
		t.Fatalf("appel : %q, attendu %q", b, want)
	}
	err := rustSvcAction(rs, "start")
	if err == nil || err.Error() != "service loki-engine non démarré" {
		t.Fatalf("erreur rendue : %v", err)
	}
}
