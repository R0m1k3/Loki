// Dictée vocale — POST /api/transcribe : un WAV (16 kHz mono, encodé côté
// navigateur) entre, le texte transcrit sort. La transcription est 100 % locale,
// par whisper-cli (whisper.cpp, même famille que llama.cpp), compilé dans
// l'image Docker. Le modèle (ggml-small-q5_1, ~190 Mo, multilingue) est
// téléchargé au premier usage dans LOKI_HOME/whisper/ — comme le moteur, il ne
// gonfle pas l'image et survit aux recréations du conteneur via /data.
package loki

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const whisperModelURL = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-small-q5_1.bin"

// whisperBin localise whisper-cli : variable d'environnement (posée par le
// Dockerfile), sinon le PATH. Vide = dictée indisponible (image trop ancienne,
// ou poste de dev sans whisper.cpp) — le handler le dit clairement.
func whisperBin() string {
	if p := os.Getenv("LOKI_WHISPER_BIN"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("whisper-cli"); err == nil {
		return p
	}
	return ""
}

func whisperModelPath() string {
	return filepath.Join(LokiHome(), "whisper", "ggml-small-q5_1.bin")
}

// Téléchargement du modèle : UNE goroutine, progression consultable. Le premier
// POST le déclenche et répond 503 {downloading, pct} ; l'UI invite à réessayer.
var wspMu sync.Mutex
var wspDl struct {
	running bool
	pct     int
	err     string
}

func whisperStartDownload() {
	wspMu.Lock()
	defer wspMu.Unlock()
	if wspDl.running {
		return
	}
	wspDl.running, wspDl.pct, wspDl.err = true, 0, ""
	go func() {
		err := whisperDownload()
		wspMu.Lock()
		wspDl.running = false
		if err != nil {
			wspDl.err = err.Error()
		}
		wspMu.Unlock()
	}()
}

func whisperDownload() error {
	dst := whisperModelPath()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	resp, err := http.Get(whisperModelURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("téléchargement du modèle whisper : HTTP %d", resp.StatusCode)
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	total := resp.ContentLength
	var done int64
	buf := make([]byte, 1<<20)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(tmp)
				return werr
			}
			done += int64(n)
			if total > 0 {
				wspMu.Lock()
				wspDl.pct = int(done * 100 / total)
				wspMu.Unlock()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(tmp)
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func handleTranscribe(w http.ResponseWriter, r *http.Request) {
	bin := whisperBin()
	if bin == "" {
		sendJSON(w, 501, map[string]any{"error": "dictée indisponible : whisper-cli absent (image à reconstruire, ou poste sans whisper.cpp)"})
		return
	}
	model := whisperModelPath()
	if _, err := os.Stat(model); err != nil {
		wspMu.Lock()
		derr := wspDl.err
		pct := wspDl.pct
		wspMu.Unlock()
		if derr != "" {
			// Échec précédent : on le dit ET on relance — un réseau revenu suffit.
			wspMu.Lock()
			wspDl.err = ""
			wspMu.Unlock()
			whisperStartDownload()
			sendJSON(w, 503, map[string]any{"downloading": true, "pct": 0, "error": derr})
			return
		}
		whisperStartDownload()
		sendJSON(w, 503, map[string]any{"downloading": true, "pct": pct})
		return
	}

	audio, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil || len(audio) < 1000 {
		sendJSON(w, 400, map[string]any{"error": "audio vide ou trop court"})
		return
	}
	tmp, err := os.CreateTemp("", "loki-dictee-*.wav")
	if err != nil {
		sendJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(audio); err != nil {
		tmp.Close()
		sendJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	tmp.Close()

	// -l auto : détection de langue (le français sort en français) ; -nt sans
	// horodatages ; -np sans bavardage de chargement. CPU seul : une dictée de
	// quelques dizaines de secondes se transcrit en 1-3 s avec small-q5_1.
	threads := runtime.NumCPU()
	if threads > 8 {
		threads = 8
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-m", model, "-f", tmpPath, "-l", "auto", "-nt", "-np", "-t", strconv.Itoa(threads))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// Ne garder QUE stderr est trompeur quand le binaire meurt d'un signal :
		// la dernière ligne écrite avant l'exécution du coup fatal ressemble à
		// une explication (« AMX is not ready to be used! ») alors que la vraie
		// cause est la mort brutale. On dit donc toujours comment il a fini —
		// un SIGILL/SIGSEGV désigne un binaire compilé pour un autre
		// processeur, pas un problème d'audio.
		msg := whisperExitReason(err)
		if last := lastLine(stderr.String()); last != "" {
			msg += " — dernière sortie : " + last
		}
		sendJSON(w, 500, map[string]any{"error": "whisper-cli : " + msg})
		return
	}
	sendJSON(w, 200, map[string]any{"text": strings.TrimSpace(string(out))})
}

// whisperExitReason traduit la fin du processus en une phrase utilisable.
// ProcessState.String() dit déjà « exit status 2 » ou « signal: illegal
// instruction » ; le second cas mérite son explication, parce que rien dans la
// dictée ne laisse deviner que le binaire ne tourne pas sur ce processeur.
func whisperExitReason(err error) string {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return err.Error()
	}
	st := ee.ProcessState.String()
	if strings.Contains(st, "signal:") {
		return st + " (binaire compilé pour un autre processeur : image à reconstruire)"
	}
	return st
}

// lastLine : la dernière ligne non vide. whisper-cli bavarde beaucoup avant de
// tomber ; seule la fin renseigne, et un pavé ne tient pas dans un bandeau.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
