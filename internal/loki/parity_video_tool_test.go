package loki

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// parity_video_tool_test.go — journal de référence de l'outil see_video
// (repris d'AJEAN, tenu par la façade comme see_image, lot 45) : refus dans
// le même ordre que see_image, moteur qui ne lit pas la vidéo (consigne
// ffmpeg), sonde « le moteur voit-il » (faux /props) et vidéo relayée au
// modèle, à l'octet (une vidéo n'est jamais réencodée).
//
// La capacité vidéo du moteur (videoInputSupported : aide du binaire) est
// fixée par chaque étape : la façade la calcule de son côté par le même
// chemin (imagetools::video_input_supported) et la passe à see_video. Rangé
// dans rust/loki-rs/tests/parity_images/video.json ; tests/videotools.rs le
// rejoue contre le port.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteOutilVideo

const parityVideoFile = "../../rust/loki-rs/tests/parity_images/video.json"

type parityVideoStep struct {
	Name   string         `json:"name"`
	Vision bool           `json:"vision,omitempty"`
	Sees   bool           `json:"sees,omitempty"`  // le faux /props annonce la vision
	Video  bool           `json:"video,omitempty"` // le moteur lit la vidéo (--video-fps)
	Args   map[string]any `json:"args"`
	Label  string         `json:"label"`
	Result string         `json:"result"`
	Part   map[string]any `json:"part,omitempty"` // vidéo relayée
	Probes int            `json:"probes"`
}

func TestPariteOutilVideo(t *testing.T) {
	testHome(t)
	files := filepath.Join(t.TempDir(), "fichiers")
	raw := map[string][]byte{
		"clip.mp4":       []byte("\x00\x00\x00\x18ftypmp42 fausse vidéo"),
		"CLIP.MOV":       []byte("\x00\x00\x00\x14ftypqt   mov"),
		"anim.webm":      []byte("\x1aE\xdf\xa3 webm"),
		"film.mkv":       []byte("\x1aE\xdf\xa3 matroska"),
		"vieux.avi":      []byte("RIFF\x10\x00\x00\x00AVI LIST"),
		"sous/court.mp4": []byte("ftyp sous-dossier"),
		"texte.txt":      []byte("pas une vidéo"),
		"vide.mp4":       {},
	}
	for name, b := range raw {
		p := filepath.Join(files, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.MkdirAll(filepath.Join(files, "dossier.mp4"), 0o755)
	lourd, _ := os.Create(filepath.Join(files, "lourd.mp4"))
	_ = lourd.Truncate(maxVideoBytes + 1)
	lourd.Close()
	setTaskWorkspace(files)
	t.Cleanup(func() { setTaskWorkspace("") })

	// Faux moteur : /props, authentifié.
	var mu sync.Mutex
	sees, probes := false, 0
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/props" || r.Header.Get("Authorization") != "Bearer cle-moteur" {
			w.WriteHeader(401)
			return
		}
		probes++
		_ = json.NewEncoder(w).Encode(map[string]any{"modalities": map[string]any{"vision": sees}})
	}))
	defer eng.Close()
	_ = SetConfigKey("PORT", strings.TrimPrefix(eng.URL, "http://127.0.0.1:"))
	_ = putStr(bkState, "api_key", "cle-moteur")
	prevVideo := videoInputSupported
	t.Cleanup(func() { videoInputSupported = prevVideo })

	var steps []parityVideoStep
	run := func(st parityVideoStep) {
		if st.Vision {
			_ = SetConfigKey("MMPROJ", "vision.gguf")
		} else {
			_ = SetConfigKey("MMPROJ", "")
		}
		video := st.Video
		videoInputSupported = func() bool { return video }
		mu.Lock()
		sees, probes = st.Sees, 0
		mu.Unlock()
		visionProbeMu.Lock()
		visionProbeAt = time.Time{}
		visionProbeMu.Unlock()
		args := map[string]any{}
		b, _ := json.Marshal(st.Args)
		_ = json.Unmarshal([]byte(strings.ReplaceAll(string(b), "{{FICHIERS}}", filepath.ToSlash(files))), &args)
		label := toolCallLabel("see_video", args)
		st.Label = strings.ReplaceAll(label, filepath.ToSlash(files), "{{FICHIERS}}")
		res := execTool(t.Context(), "see_video", args, label, Caps{Agent: true})
		_, _, relay := toolStepExtras("see_video", label, res)
		st.Result = strings.ReplaceAll(res.Result, filepath.ToSlash(files), "{{FICHIERS}}")
		if relay != nil {
			for _, p := range contentParts(relay.Content) {
				switch p["type"] {
				case "text":
					st.Result += "\n[relais] " + strings.ReplaceAll(fmt.Sprint(p["text"]), filepath.ToSlash(files), "{{FICHIERS}}")
				case "input_video":
					st.Part = parityVideoFacts(t, p, raw)
				}
			}
		}
		mu.Lock()
		st.Probes = probes
		mu.Unlock()
		if st.Args == nil {
			st.Args = map[string]any{}
		}
		steps = append(steps, st)
	}
	a := func(f string) map[string]any { return map[string]any{"file": f} }

	run(parityVideoStep{Name: "sans vision", Video: true, Args: a("clip.mp4")})
	run(parityVideoStep{Name: "chemin manquant", Vision: true, Video: true, Args: a("")})
	run(parityVideoStep{Name: "sans argument", Vision: true, Video: true})
	run(parityVideoStep{Name: "moteur sans vidéo", Vision: true, Sees: true, Args: a("clip.mp4")})
	run(parityVideoStep{Name: "pas une vidéo", Vision: true, Video: true, Args: a("texte.txt")})
	run(parityVideoStep{Name: "image", Vision: true, Video: true, Args: a("photo.png")})
	run(parityVideoStep{Name: "introuvable", Vision: true, Video: true, Args: a("absente.mp4")})
	run(parityVideoStep{Name: "dossier", Vision: true, Video: true, Args: a("dossier.mp4")})
	run(parityVideoStep{Name: "trop lourde", Vision: true, Video: true, Args: a("lourd.mp4")})
	run(parityVideoStep{Name: "le moteur ne voit pas", Vision: true, Video: true, Args: a("clip.mp4")})
	run(parityVideoStep{Name: "vide", Vision: true, Sees: true, Video: true, Args: a("vide.mp4")})
	for _, f := range []string{"clip.mp4", "CLIP.MOV", "anim.webm", "film.mkv", "vieux.avi", "sous/court.mp4"} {
		run(parityVideoStep{Name: "voir " + f, Vision: true, Sees: true, Video: true, Args: a(f)})
	}
	run(parityVideoStep{Name: "chemin absolu", Vision: true, Sees: true, Video: true, Args: a("{{FICHIERS}}/clip.mp4")})

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{"files": raw, "steps": steps}); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityVideoFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityVideoFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityVideoFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("see_video ne produit plus le journal rangé dans %s.\n"+
			"LOKI_PARITY_UPDATE=1 pour le régénérer si le changement est voulu.", parityVideoFile)
	}
}

// parityVideoFacts : le type et le fichier d'origine de la vidéo relayée (à
// l'octet : une vidéo part telle quelle).
func parityVideoFacts(t *testing.T, part map[string]any, raw map[string][]byte) map[string]any {
	u := partVideoURL(part)
	i := strings.Index(u, ";base64,")
	if i < 0 {
		t.Fatalf("partie vidéo sans base64 : %.60q", u)
	}
	b, err := base64.StdEncoding.DecodeString(u[i+8:])
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{"mime": strings.TrimPrefix(u[:i], "data:")}
	for name, r := range raw {
		if bytes.Equal(r, b) {
			out["same_as"] = name
		}
	}
	return out
}
