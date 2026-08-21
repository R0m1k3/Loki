package loki

import (
	"os"
	"strings"
	"testing"
)

// La dictée est morte en production parce que whisper-cli avait été compilé
// avec -march=native, donc pour le processeur du runner GitHub (AVX-512, AMX)
// et pas pour la machine qui fait tourner l'image : SIGILL en pleine
// transcription. Le drapeau qui l'évite ne se voit pas à l'exécution — rien ne
// le rappelle au prochain qui touchera cette étape de build. Ce test le fait.
func TestDockerfileWhisperNonNatif(t *testing.T) {
	b, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatalf("lecture du Dockerfile : %v", err)
	}
	src := string(b)
	_, apres, ok := strings.Cut(src, "AS whisperbuild")
	if !ok {
		t.Fatal("étape whisperbuild introuvable dans le Dockerfile")
	}
	// L'étape suivante commence au FROM d'après : ne pas déborder dessus.
	if fin := strings.Index(apres, "\nFROM "); fin >= 0 {
		apres = apres[:fin]
	}
	for _, drapeau := range []string{"-DGGML_NATIVE=OFF", "-DGGML_AMX_TILE=OFF"} {
		if !strings.Contains(apres, drapeau) {
			t.Errorf("l'étape whisperbuild ne passe plus %s : le binaire sera compilé pour le processeur du runner et mourra d'un SIGILL ailleurs", drapeau)
		}
	}
}

func TestLastLine(t *testing.T) {
	cas := []struct{ in, want string }{
		{"", ""},
		{"   \n\n  ", ""},
		{"une seule ligne", "une seule ligne"},
		{"AMX is not ready to be used!\nread_audio_data: ...\n", "read_audio_data: ..."},
		{"fin utile\n\n   \n", "fin utile"},
	}
	for _, c := range cas {
		if got := lastLine(c.in); got != c.want {
			t.Errorf("lastLine(%q) = %q, attendu %q", c.in, got, c.want)
		}
	}
}
