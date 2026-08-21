// Dictée vocale — POST /api/transcribe : un WAV (16 kHz mono, encodé côté
// navigateur) entre, le texte transcrit sort. La transcription est 100 % locale,
// par whisper.cpp. Le modèle est téléchargé au premier usage dans
// LOKI_HOME/whisper/ — comme le moteur, il ne gonfle pas l'image et survit aux
// recréations du conteneur via /data.
//
// La transcription elle-même passe par whisper-server (dictate_server.go), qui
// garde le modèle chargé. L'ancien chemin relançait whisper-cli à chaque
// requête, donc rechargeait le modèle à chaque phrase dictée.
package loki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Téléchargement d'un modèle : UNE goroutine à la fois, progression
// consultable. Le premier POST le déclenche et répond 503 {downloading, pct} ;
// l'UI invite à réessayer.
var wspMu sync.Mutex
var wspDl struct {
	id      string
	running bool
	pct     int
	err     string
}

func whisperStartDownload(id string) {
	wspMu.Lock()
	defer wspMu.Unlock()
	if wspDl.running {
		return
	}
	if whisperModelURLFor(id) == "" {
		wspDl.err = "modèle de dictée inconnu : " + id
		return
	}
	wspDl.id, wspDl.running, wspDl.pct, wspDl.err = id, true, 0, ""
	go func() {
		err := whisperDownload(id)
		wspMu.Lock()
		wspDl.running = false
		if err != nil {
			wspDl.err = err.Error()
		}
		wspMu.Unlock()
	}()
}

func whisperDownload(id string) error {
	dst := whisperModelPathFor(id)
	if dst == "" {
		return fmt.Errorf("modèle de dictée inconnu : %s", id)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	resp, err := http.Get(whisperModelURLFor(id))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return fmt.Errorf("téléchargement du modèle whisper : HTTP %d", resp.StatusCode)
	}
	// Écriture dans un .part renommé à la fin : un téléchargement interrompu
	// ne doit pas laisser un .bin tronqué que whisperModelPresent croirait bon.
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
				_ = f.Close()
				_ = os.Remove(tmp)
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
			_ = f.Close()
			_ = os.Remove(tmp)
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// whisperDlEtat : instantané de la progression, pour l'UI et pour les 503.
func whisperDlEtat() (id string, running bool, pct int, errMsg string) {
	wspMu.Lock()
	defer wspMu.Unlock()
	return wspDl.id, wspDl.running, wspDl.pct, wspDl.err
}

func handleTranscribe(w http.ResponseWriter, r *http.Request) {
	audio, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil || len(audio) < 1000 {
		sendJSON(w, 400, map[string]any{"error": "audio vide ou trop court"})
		return
	}
	cfg := dictateCfgLoad()
	if !whisperModelPresent(cfg.Model) {
		_, _, pct, derr := whisperDlEtat()
		if derr != "" {
			// Échec précédent : on le dit ET on relance — un réseau revenu suffit.
			wspMu.Lock()
			wspDl.err = ""
			wspMu.Unlock()
			whisperStartDownload(cfg.Model)
			sendJSON(w, 503, map[string]any{"downloading": true, "pct": 0, "error": derr})
			return
		}
		whisperStartDownload(cfg.Model)
		sendJSON(w, 503, map[string]any{"downloading": true, "pct": pct})
		return
	}
	// Le chargement du modèle au premier appel peut être long ; la
	// transcription elle-même ne l'est pas.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	txt, err := whisperInfer(ctx, audio)
	if err != nil {
		sendJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	sendJSON(w, 200, map[string]any{"text": txt})
}

// ─── Réglages de la dictée ───────────────────────────────────────────────────

func handleDictateConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var c DictateCfg
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			sendJSON(w, 400, map[string]any{"error": "requête illisible"})
			return
		}
		if err := dictateCfgSave(c); err != nil {
			sendJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		// Sans ça, le serveur continuerait de tourner avec l'ancien modèle et
		// le nouveau réglage semblerait sans effet.
		whisperShutdown()
		sendJSON(w, 200, map[string]any{"ok": true})
		return
	}
	sendJSON(w, 200, dictateCfgLoad())
}

func handleDictateModels(w http.ResponseWriter, r *http.Request) {
	sendJSON(w, 200, whisperCatalogueTrie())
}

func handleDictateDownload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			sendJSON(w, 400, map[string]any{"error": "requête illisible"})
			return
		}
		id := strings.TrimSpace(req.ID)
		if whisperModelURLFor(id) == "" {
			sendJSON(w, 400, map[string]any{"error": "modèle de dictée inconnu : " + id})
			return
		}
		whisperStartDownload(id)
	}
	id, running, pct, errMsg := whisperDlEtat()
	sendJSON(w, 200, map[string]any{"id": id, "running": running, "pct": pct, "error": errMsg})
}

func handleDictateState(w http.ResponseWriter, r *http.Request) {
	etat := whisperEtat()
	id, running, pct, errMsg := whisperDlEtat()
	etat["dl"] = map[string]any{"id": id, "running": running, "pct": pct, "error": errMsg}
	sendJSON(w, 200, etat)
}

// whisperExitReason traduit la fin du processus en une phrase utilisable.
// ProcessState.String() dit déjà « exit status 2 » ou « signal: illegal
// instruction » ; le second cas mérite son explication, parce que rien dans la
// dictée ne laisse deviner que le binaire ne tourne pas sur ce processeur.
func whisperExitReason(err error) string {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		if err == nil {
			return "arrêt sans erreur"
		}
		return err.Error()
	}
	st := ee.ProcessState.String()
	if strings.Contains(st, "signal:") {
		return st + " (binaire compilé pour un autre processeur : image à reconstruire)"
	}
	return st
}

// lastLine : la dernière ligne non vide. whisper bavarde beaucoup avant de
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
