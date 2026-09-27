package loki

import "testing"

// Un bench n'est affiché que pour le modèle sur lequel il a été mesuré.
func TestBenchMatchesPreset(t *testing.T) {
	sb := savedBench{Model: "Qwen3-27B-Q4_K_M.gguf"}
	if !benchMatchesPreset(sb, map[string]string{"MODEL": "/models/Qwen3-27B-Q4_K_M.gguf"}) {
		t.Fatal("même modèle : le bench doit s'afficher")
	}
	if benchMatchesPreset(sb, map[string]string{"MODEL": "/models/Autre.gguf"}) {
		t.Fatal("autre modèle : le bench ne doit pas s'afficher")
	}
	if benchMatchesPreset(sb, map[string]string{}) {
		t.Fatal("preset sans modèle : pas de bench")
	}
}
