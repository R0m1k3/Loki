package loki

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// etapesWhisperbuild découpe le Dockerfile en étapes « FROM … AS whisperbuild* »
// et rend le texte de chacune.
func etapesWhisperbuild(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatalf("lecture du Dockerfile : %v", err)
	}
	out := map[string]string{}
	for _, bloc := range strings.Split(string(b), "\nFROM ") {
		entete, reste, ok := strings.Cut(bloc, "\n")
		if !ok {
			continue
		}
		_, nom, ok := strings.Cut(entete, " AS ")
		if !ok || !strings.HasPrefix(strings.TrimSpace(nom), "whisperbuild") {
			continue
		}
		out[strings.TrimSpace(nom)] = entete + "\n" + reste
	}
	return out
}

// argWhisperFlags rend la valeur de l'ARG WHISPER_CMAKE_FLAGS, continuations
// de ligne comprises. Vide si l'ARG n'existe pas : les étapes devront alors
// porter les drapeaux en clair, et le test le vérifiera.
func argWhisperFlags(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatalf("lecture du Dockerfile : %v", err)
	}
	_, apres, ok := strings.Cut(string(b), "ARG WHISPER_CMAKE_FLAGS=")
	if !ok {
		return ""
	}
	var val strings.Builder
	for _, ligne := range strings.Split(apres, "\n") {
		val.WriteString(" " + ligne)
		if !strings.HasSuffix(strings.TrimSpace(ligne), "\\") {
			break
		}
	}
	return val.String()
}

// La dictée est morte en production parce que whisper avait été compilé avec
// -march=native, donc pour le processeur du runner GitHub (AVX-512, AMX) et pas
// pour la machine qui fait tourner l'image : SIGILL en pleine transcription.
// Les drapeaux qui l'évitent ne se voient pas à l'exécution — rien ne les
// rappelle au prochain qui touchera cette étape de build. Ce test le fait, sur
// CHAQUE étape, y compris celle ajoutée pour CUDA.
func TestDockerfileWhisperNonNatif(t *testing.T) {
	etapes := etapesWhisperbuild(t)
	if len(etapes) < 2 {
		t.Fatalf("%d étape(s) whisperbuild, 2 attendues (CPU et CUDA) : %v", len(etapes), clefs(etapes))
	}
	// Les drapeaux communs sont factorisés dans WHISPER_CMAKE_FLAGS. Une étape
	// est conforme si elle les porte en clair OU si elle référence cet ARG —
	// ce qui compte est qu'ils atteignent cmake, pas qu'ils soient recopiés.
	commun := argWhisperFlags(t)
	for nom, txt := range etapes {
		effectif := txt
		if strings.Contains(txt, "${WHISPER_CMAKE_FLAGS}") {
			effectif += " " + commun
		}
		for _, drapeau := range []string{"-DGGML_NATIVE=OFF", "-DGGML_AMX_TILE=OFF", "-DGGML_AVX512=OFF"} {
			if !strings.Contains(effectif, drapeau) {
				t.Errorf("étape %s : %s absent — le binaire sera compilé pour le processeur du runner et mourra d'un SIGILL ailleurs", nom, drapeau)
			}
		}
		if !strings.Contains(txt, "whisper-server") {
			t.Errorf("étape %s : ne construit pas la cible whisper-server", nom)
		}
		// « -j » nu autorise un parallélisme ILLIMITÉ chez Make. Sur l'étape
		// CUDA (~200 nvcc à 1-2 Go pièce), le runner GitHub était tué par
		// l'OOM sans écrire une ligne d'erreur. L'étape CPU y survivait, ce
		// qui rendait le piège invisible.
		for _, ligne := range strings.Split(txt, "\n") {
			if !strings.Contains(ligne, "cmake --build") {
				continue
			}
			if regexp.MustCompile(`-j(\s|$|\\)`).MatchString(ligne) {
				t.Errorf("étape %s : « cmake --build -j » sans nombre — parallélisme illimité, le runner sera tué par l'OOM. Utiliser -j\"$(nproc)\".", nom)
			}
		}
	}
}

// Deux binaires, pas un : sur une image runtime bâtie sans CUDA, un binaire lié
// à CUDA ne démarre pas du tout — l'éditeur de liens échoue avant la première
// instruction, donc aucun repli n'est possible depuis le programme.
func TestDockerfileDeuxBinairesWhisper(t *testing.T) {
	b, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatalf("lecture du Dockerfile : %v", err)
	}
	src := string(b)
	var avecCuda bool
	for nom, txt := range etapesWhisperbuild(t) {
		if strings.Contains(txt, "-DGGML_CUDA=ON") {
			avecCuda = true
			_ = nom
		}
	}
	if !avecCuda {
		t.Error("aucune étape whisperbuild ne passe -DGGML_CUDA=ON : la dictée ne pourra jamais utiliser le GPU")
	}
	for _, bin := range []string{"whisper-server-cpu", "whisper-server-cuda"} {
		if !strings.Contains(src, "/usr/local/bin/"+bin) {
			t.Errorf("le runtime ne reçoit pas %s — dictate_server.go le cherche à cet emplacement", bin)
		}
	}
}

func clefs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
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
