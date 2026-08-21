// Supervision de whisper-server : le modèle est chargé UNE fois et reste en
// mémoire, au lieu d'être relu depuis le disque à chaque phrase dictée.
//
// L'ancien chemin lançait whisper-cli par requête. Le chargement du modèle
// dominait le temps de réponse (~3 s pour 3,4 s d'audio), et découper la
// dictée en tranches par-dessus ce mécanisme aurait rechargé le modèle toutes
// les quelques secondes.
package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// whisperServerBin : deux binaires sont livrés, pas un.
//
// L'image runtime peut être bâtie sur la variante CPU de llama.cpp
// (LLAMACPP_IMAGE=ghcr.io/ggml-org/llama.cpp:server). Sur cette base les
// bibliothèques CUDA sont absentes, et un binaire lié à CUDA ne démarre pas du
// tout : l'éditeur de liens échoue avant la première instruction, il n'y a
// donc aucun repli possible depuis l'intérieur du programme. D'où deux
// binaires et un choix fait ici.
func whisperServerBin(gpu bool) string {
	env, defaut := "LOKI_WHISPER_SERVER_CPU", "/usr/local/bin/whisper-server-cpu"
	if gpu {
		env, defaut = "LOKI_WHISPER_SERVER_CUDA", "/usr/local/bin/whisper-server-cuda"
	}
	for _, p := range []string{os.Getenv(env), defaut} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if gpu {
		// Pas de binaire CUDA : l'appelant retombe sur le CPU en le disant.
		return ""
	}
	if p, err := exec.LookPath("whisper-server"); err == nil {
		return p
	}
	return ""
}

func whisperServerArgs(c DictateCfg, port int) ([]string, error) {
	model := whisperModelPathFor(c.Model)
	if model == "" {
		return nil, fmt.Errorf("modèle de dictée inconnu : %s", c.Model)
	}
	return []string{
		"-m", model,
		"-l", c.Lang,
		// 127.0.0.1 et pas 0.0.0.0 : whisper-server n'a aucune
		// authentification. Sur un conteneur au réseau partagé, l'exposer
		// offrirait la transcription à tout le réseau.
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
	}, nil
}

// whisperServerEnv pose CUDA_VISIBLE_DEVICES en REMPLAÇANT toute valeur déjà
// présente. Une variable dupliquée laisse le gagnant dépendre de la libc, et
// le choix de GPU deviendrait un coup de dé.
func whisperServerEnv(c DictateCfg, base []string) []string {
	const clef = "CUDA_VISIBLE_DEVICES="
	out := make([]string, 0, len(base)+1)
	for _, v := range base {
		if !strings.HasPrefix(v, clef) {
			out = append(out, v)
		}
	}
	// Valeur vide en mode CPU : masque TOUS les GPU. Retirer la variable
	// ferait l'inverse — ggml les verrait tous.
	return append(out, clef+c.cudaVisibleDevices())
}

// portLibre demande au système un port disponible puis le relâche. Coder un
// port en dur casserait sur une machine qui l'occupe déjà ; la fenêtre entre
// la fermeture et la reprise par whisper-server est négligeable devant le
// risque d'un conflit permanent.
func portLibre() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// whisperMarqueur reconnaît les annotations que whisper produit à la place
// d'un texte : [BLANK_AUDIO], (silence), [SOUND]… Un contenu entièrement
// entouré de crochets ou de parenthèses n'est jamais de la parole dictée, et
// le laisser passer le collerait tel quel dans le champ de saisie.
func whisperMarqueur(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	return (strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]")) ||
		(strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")"))
}

// whisperInferSur prend l'URL de base en paramètre : c'est ce qui rend l'appel
// testable sans lancer de processus.
func whisperInferSur(ctx context.Context, base string, wav []byte) (string, error) {
	var corps bytes.Buffer
	mw := multipart.NewWriter(&corps)
	part, err := mw.CreateFormFile("file", "dictee.wav")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(wav); err != nil {
		return "", err
	}
	if err := mw.WriteField("response_format", "json"); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/inference", &corps)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	brut, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("whisper-server a répondu %d : %s", resp.StatusCode, lastLine(string(brut)))
	}
	var j struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(brut, &j); err != nil {
		return "", fmt.Errorf("réponse illisible de whisper-server : %w", err)
	}
	if whisperMarqueur(j.Text) {
		return "", nil
	}
	return strings.TrimSpace(j.Text), nil
}

// ─── Supervision ─────────────────────────────────────────────────────────────

// whisperIdle : au-delà, le serveur s'éteint et rend sa VRAM. Une dictée n'est
// pas un service permanent ; garder un modèle chargé toute la journée pour
// quelques phrases prive le moteur de chat de mémoire GPU.
const whisperIdle = 10 * time.Minute

// whisperReady : plafond d'attente au démarrage. Un modèle de 1 Go lu depuis un
// disque lent puis poussé sur le GPU prend du temps ; échouer trop tôt
// afficherait une panne là où il n'y a qu'une lenteur.
const whisperReady = 120 * time.Second

var wsrvMu sync.Mutex
var wsrv struct {
	cmd    *exec.Cmd
	port   int
	cfg    DictateCfg
	log    *bytes.Buffer
	timer  *time.Timer
	note   string // pourquoi on ne tourne pas sur le matériel demandé
	depuis time.Time
}

// whisperEnsure garantit un serveur vivant pour la configuration COURANTE, et
// renvoie son port. Si les réglages ont changé depuis le lancement, l'ancien
// est arrêté : c'est ce qui rend un changement de modèle ou de GPU effectif
// sans redémarrer Loki.
func whisperEnsure() (int, error) {
	cfg := dictateCfgLoad()
	wsrvMu.Lock()
	defer wsrvMu.Unlock()
	if wsrv.cmd != nil && wsrv.cfg == cfg && wsrv.cmd.ProcessState == nil {
		whisperTouchLocked()
		return wsrv.port, nil
	}
	whisperStopLocked()
	return whisperStartLocked(cfg)
}

func whisperStartLocked(cfg DictateCfg) (int, error) {
	if !whisperModelPresent(cfg.Model) {
		return 0, fmt.Errorf("modèle de dictée absent : %s (à télécharger dans Paramètres → Dictée)", cfg.Model)
	}
	note := ""
	gpu := cfg.cudaVisibleDevices() != ""
	bin := whisperServerBin(gpu)
	if bin == "" && gpu {
		// Image bâtie sans CUDA : on le DIT, au lieu de laisser croire que le
		// GPU choisi est utilisé.
		note = "binaire CUDA absent de cette image — dictée sur le processeur"
		gpu = false
		cfg.Device = "cpu"
		bin = whisperServerBin(false)
	}
	if bin == "" {
		return 0, fmt.Errorf("whisper-server introuvable (image à reconstruire)")
	}
	port, err := portLibre()
	if err != nil {
		return 0, fmt.Errorf("aucun port libre pour whisper-server : %w", err)
	}
	args, err := whisperServerArgs(cfg, port)
	if err != nil {
		return 0, err
	}
	log := &bytes.Buffer{}
	cmd := exec.Command(bin, args...)
	cmd.Env = whisperServerEnv(cfg, os.Environ())
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("démarrage de whisper-server : %w", err)
	}
	wsrv.cmd, wsrv.port, wsrv.cfg = cmd, port, dictateCfgLoad()
	wsrv.log, wsrv.note, wsrv.depuis = log, note, time.Now()
	if err := whisperAttendPret(cmd, port, log); err != nil {
		whisperStopLocked()
		return 0, err
	}
	whisperTouchLocked()
	return port, nil
}

// whisperAttendPret sonde le serveur jusqu'à ce qu'il réponde. Si le processus
// meurt entre-temps, on rapporte COMMENT il est mort : sur une mort par
// signal, la dernière ligne du journal est celle d'avant le coup fatal — elle
// ressemble à une explication sans en être une (leçon de la PR #18).
func whisperAttendPret(cmd *exec.Cmd, port int, log *bytes.Buffer) error {
	fin := time.Now().Add(whisperReady)
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/"
	mort := make(chan error, 1)
	go func() { mort <- cmd.Wait() }()
	for time.Now().Before(fin) {
		select {
		case err := <-mort:
			msg := whisperExitReason(err)
			if l := lastLine(log.String()); l != "" {
				msg += " — dernière sortie : " + l
			}
			return fmt.Errorf("whisper-server s'est arrêté au démarrage : %s", msg)
		case <-time.After(200 * time.Millisecond):
		}
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		resp, err := http.DefaultClient.Do(req.WithContext(ctx))
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			return nil
		}
	}
	return fmt.Errorf("whisper-server n'a pas répondu en %s", whisperReady)
}

func whisperTouchLocked() {
	if wsrv.timer != nil {
		wsrv.timer.Stop()
	}
	wsrv.timer = time.AfterFunc(whisperIdle, whisperShutdown)
}

func whisperStopLocked() {
	if wsrv.timer != nil {
		wsrv.timer.Stop()
		wsrv.timer = nil
	}
	if wsrv.cmd != nil && wsrv.cmd.Process != nil {
		_ = wsrv.cmd.Process.Kill()
	}
	wsrv.cmd, wsrv.port, wsrv.log = nil, 0, nil
	wsrv.cfg, wsrv.note = DictateCfg{}, ""
}

// whisperShutdown arrête le serveur. Appelé par la minuterie d'inactivité et
// à chaque changement de réglages — sans ça, le serveur continuerait de
// tourner avec l'ancien modèle et le nouveau réglage semblerait sans effet.
func whisperShutdown() {
	wsrvMu.Lock()
	defer wsrvMu.Unlock()
	whisperStopLocked()
}

// whisperInfer garantit le serveur puis lui soumet le WAV.
func whisperInfer(ctx context.Context, wav []byte) (string, error) {
	port, err := whisperEnsure()
	if err != nil {
		return "", err
	}
	return whisperInferSur(ctx, "http://127.0.0.1:"+strconv.Itoa(port), wav)
}

func whisperEtat() map[string]any {
	cfg := dictateCfgLoad()
	wsrvMu.Lock()
	defer wsrvMu.Unlock()
	etat := map[string]any{
		"actif":   wsrv.cmd != nil,
		"modele":  cfg.Model,
		"present": whisperModelPresent(cfg.Model),
		"device":  cfg.Device,
		"note":    wsrv.note,
	}
	if wsrv.cmd != nil {
		etat["depuis_s"] = int(time.Since(wsrv.depuis).Seconds())
	}
	return etat
}
