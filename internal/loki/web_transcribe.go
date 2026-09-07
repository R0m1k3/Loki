// Dictée vocale — POST /api/transcribe : un WAV (16 kHz mono, encodé côté
// navigateur) entre, le texte transcrit sort. La transcription est 100 % locale,
// par Parakeet (NVIDIA) servi par sherpa-onnx. Le modèle est téléchargé au
// premier usage dans LOKI_HOME/asr/ — comme le moteur, il ne gonfle pas l'image
// et survit aux recréations du conteneur via /data.
//
// La transcription elle-même passe par le serveur de dictée (dictate_server.go),
// qui garde le modèle chargé entre deux phrases.
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

func asrStartDownload(id string) {
	wspMu.Lock()
	defer wspMu.Unlock()
	if wspDl.running {
		return
	}
	if asrModelURLFor(id) == "" {
		wspDl.err = "modèle de dictée inconnu : " + id
		return
	}
	wspDl.id, wspDl.running, wspDl.pct, wspDl.err = id, true, 0, ""
	go func() {
		err := asrDownload(id)
		wspMu.Lock()
		wspDl.running = false
		if err != nil {
			wspDl.err = err.Error()
		}
		wspMu.Unlock()
	}()
}

// asrDownload télécharge et DÉCOMPRESSE un modèle. Un modèle Parakeet est une
// archive .tar.bz2 qui contient un dossier de quatre fichiers, là où whisper.cpp
// livrait un .bin unique : on décompresse donc au fil du téléchargement plutôt
// que de garder l'archive sur le disque (près de 500 Mo pour rien).
//
// La progression est mesurée sur les octets REÇUS, pas sur les octets écrits :
// c'est le téléchargement qui prend le temps, la décompression suit.
func asrDownload(id string) error {
	dst := asrModelDirFor(id)
	if dst == "" {
		return fmt.Errorf("modèle de dictée inconnu : %s", id)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	resp, err := http.Get(asrModelURLFor(id))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return fmt.Errorf("téléchargement du modèle de dictée : HTTP %d", resp.StatusCode)
	}
	// asrExtract écrit dans un dossier .part renommé à la fin : une extraction
	// interrompue ne laisse jamais un modèle à moitié installé sous son nom
	// définitif, que asrModelPresent croirait bon.
	return asrExtract(&asrProgress{r: resp.Body, total: resp.ContentLength}, dst)
}

// asrProgress compte les octets lus et publie l'avancement. Enveloppe le corps
// de la réponse pour que la barre bouge pendant la décompression, qui consomme
// le flux à son rythme.
type asrProgress struct {
	r     io.Reader
	total int64
	lus   int64
}

func (p *asrProgress) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 && p.total > 0 {
		p.lus += int64(n)
		wspMu.Lock()
		wspDl.pct = int(p.lus * 100 / p.total)
		wspMu.Unlock()
	}
	return n, err
}

// asrDlEtat : instantané de la progression, pour l'UI et pour les 503.
func asrDlEtat() (id string, running bool, pct int, errMsg string) {
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
	if !asrModelPresent(cfg.Model) {
		_, _, pct, derr := asrDlEtat()
		if derr != "" {
			// Échec précédent : on le dit ET on relance — un réseau revenu suffit.
			wspMu.Lock()
			wspDl.err = ""
			wspMu.Unlock()
			asrStartDownload(cfg.Model)
			sendJSON(w, 503, map[string]any{"downloading": true, "pct": 0, "error": derr})
			return
		}
		asrStartDownload(cfg.Model)
		sendJSON(w, 503, map[string]any{"downloading": true, "pct": pct})
		return
	}
	// Le chargement du modèle au premier appel peut être long ; la
	// transcription elle-même ne l'est pas.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	txt, err := asrInfer(ctx, audio)
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
		asrShutdown()
		sendJSON(w, 200, map[string]any{"ok": true})
		return
	}
	sendJSON(w, 200, dictateCfgLoad())
}

func handleDictateModels(w http.ResponseWriter, r *http.Request) {
	sendJSON(w, 200, asrCatalogueTrie())
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
		if asrModelURLFor(id) == "" {
			sendJSON(w, 400, map[string]any{"error": "modèle de dictée inconnu : " + id})
			return
		}
		asrStartDownload(id)
	}
	id, running, pct, errMsg := asrDlEtat()
	sendJSON(w, 200, map[string]any{"id": id, "running": running, "pct": pct, "error": errMsg})
}

func handleDictateState(w http.ResponseWriter, r *http.Request) {
	etat := asrEtat()
	id, running, pct, errMsg := asrDlEtat()
	etat["dl"] = map[string]any{"id": id, "running": running, "pct": pct, "error": errMsg}
	sendJSON(w, 200, etat)
}

// asrExitReason traduit la fin du processus en une phrase utilisable.
// ProcessState.String() dit déjà « exit status 2 » ou « signal: illegal
// instruction » ; le second cas mérite son explication, parce que rien dans la
// dictée ne laisse deviner que le binaire ne tourne pas sur ce processeur.
func asrExitReason(err error) string {
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

// lastLine : la dernière ligne non vide. Le serveur de dictée bavarde avant de
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
