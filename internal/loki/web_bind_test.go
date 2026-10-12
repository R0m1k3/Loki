package loki

import "testing"

// La façade Rust lance `loki web` derrière elle : le Go ne doit alors écouter
// qu'en local, sinon il resterait joignable en direct, sans la façade.
func TestWebBindAddr(t *testing.T) {
	t.Setenv("LOKI_WEB_BIND", "")
	if got := webBindAddr(8090); got != "0.0.0.0:8090" {
		t.Fatalf("défaut : %q", got)
	}
	t.Setenv("LOKI_WEB_BIND", "127.0.0.1")
	if got := webBindAddr(8091); got != "127.0.0.1:8091" {
		t.Fatalf("local : %q", got)
	}
}
