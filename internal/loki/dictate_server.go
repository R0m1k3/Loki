// Supervision du serveur de transcription : le modèle est chargé UNE fois et
// reste en mémoire, au lieu d'être relu depuis le disque à chaque phrase dictée.
//
// Le moteur est sherpa-onnx (Parakeet, NVIDIA), qui a remplacé whisper.cpp. Ce
// qui n'a PAS changé : un processus local supervisé, arrêté après un temps
// d'inactivité, redémarré quand les réglages bougent. Ce qui a changé : le
// dialogue. whisper-server exposait du HTTP multipart ; sherpa-onnx n'expose
// qu'un WebSocket, dont le protocole tient en deux entiers et des flottants
// (voir asrInferSur).
package loki

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// asrServerBin : le binaire du serveur de transcription. Un seul, contrairement
// aux deux binaires whisper d'avant (CPU et CUDA) : le build sherpa-onnx livré
// est le statique CPU, il n'a donc aucune dépendance CUDA à satisfaire et
// démarre sur n'importe quelle base d'image.
func asrServerBin() string {
	for _, p := range []string{os.Getenv("LOKI_ASR_SERVER"), "/usr/local/bin/sherpa-onnx-offline-websocket-server"} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("sherpa-onnx-offline-websocket-server"); err == nil {
		return p
	}
	return ""
}

// asrServerArgs. Les arguments de sherpa-onnx s'écrivent --clef=valeur : la
// forme séparée par une espace n'est PAS reconnue et le serveur démarre alors
// sans modèle.
func asrServerArgs(c DictateCfg, port int) ([]string, error) {
	if asrModelDirFor(c.Model) == "" {
		return nil, fmt.Errorf("modèle de dictée inconnu : %s", c.Model)
	}
	return []string{
		"--encoder=" + asrModelFile(c.Model, "encoder.int8.onnx"),
		"--decoder=" + asrModelFile(c.Model, "decoder.int8.onnx"),
		"--joiner=" + asrModelFile(c.Model, "joiner.int8.onnx"),
		"--tokens=" + asrModelFile(c.Model, "tokens.txt"),
		// nemo_transducer : sans ce type, les sorties du modèle sont mal
		// interprétées et la transcription est du bruit, pas une erreur.
		"--model-type=nemo_transducer",
		"--port=" + strconv.Itoa(port),
		// Le serveur n'a AUCUNE authentification. Il n'écoute donc que la boucle
		// locale : sur un conteneur au réseau partagé, l'exposer offrirait la
		// transcription à tout le réseau.
		"--host=127.0.0.1",
	}, nil
}

// asrServerEnv masque tous les GPU, en REMPLAÇANT toute valeur déjà présente.
// Une variable dupliquée laisse le gagnant dépendre de la libc. Le binaire livré
// est CPU de toute façon ; la variable est là pour que ça reste vrai si l'image
// venait à embarquer un build GPU sans qu'on repense à la dictée.
func asrServerEnv(base []string) []string {
	const clef = "CUDA_VISIBLE_DEVICES="
	out := make([]string, 0, len(base)+1)
	for _, v := range base {
		if !strings.HasPrefix(v, clef) {
			out = append(out, v)
		}
	}
	return append(out, clef)
}

// portLibre demande au système un port disponible puis le relâche. Coder un
// port en dur casserait sur une machine qui l'occupe déjà ; la fenêtre entre
// la fermeture et la reprise par le serveur est négligeable devant le risque
// d'un conflit permanent.
func portLibre() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// asrMarqueur reconnaît ce qui n'est pas de la parole dictée : le vide, et les
// annotations entre crochets ou parenthèses. Parakeet en produit beaucoup moins
// que whisper, mais le filtre ne coûte rien et un « (silence) » collé dans le
// champ de saisie se remarque tout de suite.
func asrMarqueur(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	return (strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]")) ||
		(strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")"))
}

// ─── Protocole ───────────────────────────────────────────────────────────────

// asrEnteteOctets : l'en-tête du serveur sherpa-onnx, deux entiers 32 bits en
// petit-boutien — la fréquence d'échantillonnage, puis le NOMBRE D'OCTETS
// d'audio annoncé. Le serveur décode dès qu'il a reçu exactement ce nombre : un
// compte faux, et il attend indéfiniment de quoi finir.
const asrEnteteOctets = 8

// asrTrameMax borne la taille d'une trame WebSocket envoyée. Le serveur
// reconstitue l'utterance à partir du compte annoncé, donc le découpage est
// libre — mais pas la TAILLE : une pile WebSocket refuse par défaut les messages
// au-delà de quelques dizaines de kilooctets et ferme la connexion au lieu de
// répondre. 10 Ko est la valeur qu'emploie le client de référence de
// sherpa-onnx ; on s'aligne dessus plutôt que de parier sur une limite.
const asrTrameMax = 10240

// asrInferSur prend l'adresse du serveur en paramètre : c'est ce qui rend
// l'appel testable sans lancer de processus.
//
// Le protocole, en entier : on ouvre une connexion, on envoie UNE trame binaire
// contenant l'en-tête suivi des échantillons en float32 petit-boutien, et on lit
// UNE trame texte qui porte le résultat en JSON.
func asrInferSur(ctx context.Context, adresse string, wav []byte) (string, error) {
	taux, ech, err := wavVersFloat32(wav)
	if err != nil {
		return "", err
	}
	if len(ech) == 0 {
		return "", nil // rien à transcrire : ce n'est pas une erreur
	}
	c, _, err := websocket.Dial(ctx, adresse, nil)
	if err != nil {
		return "", fmt.Errorf("connexion au serveur de dictée : %w", err)
	}
	defer func() { _ = c.CloseNow() }()
	// Une transcription peut légitimement dépasser la taille par défaut acceptée
	// en lecture ; on ne borne pas ce que le serveur nous renvoie.
	c.SetReadLimit(-1)

	corps := make([]byte, asrEnteteOctets+4*len(ech))
	binary.LittleEndian.PutUint32(corps[0:4], uint32(taux))
	binary.LittleEndian.PutUint32(corps[4:8], uint32(4*len(ech)))
	for i, v := range ech {
		binary.LittleEndian.PutUint32(corps[asrEnteteOctets+4*i:], math.Float32bits(v))
	}
	for off := 0; off < len(corps); off += asrTrameMax {
		fin := off + asrTrameMax
		if fin > len(corps) {
			fin = len(corps)
		}
		if err := c.Write(ctx, websocket.MessageBinary, corps[off:fin]); err != nil {
			return "", fmt.Errorf("envoi de l'audio : %w", err)
		}
	}
	_, brut, err := c.Read(ctx)
	if err != nil {
		return "", fmt.Errorf("lecture du résultat : %w", err)
	}
	var j struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(brut, &j); err != nil {
		return "", fmt.Errorf("réponse illisible du serveur de dictée : %w", err)
	}
	if asrMarqueur(j.Text) {
		return "", nil
	}
	return strings.TrimSpace(j.Text), nil
}

// wavVersFloat32 lit le WAV produit par le navigateur (16 kHz mono) et rend ses
// échantillons normalisés dans [-1, 1], ce qu'attend sherpa-onnx.
//
// On parcourt les blocs plutôt que de sauter à un offset fixe : un WAV n'a pas
// d'en-tête de taille garantie (« LIST », « fact » et consorts s'intercalent
// avant « data »), et l'offset 44 codé en dur transformerait ces octets en
// craquement au début de chaque phrase.
func wavVersFloat32(wav []byte) (taux int, ech []float32, err error) {
	if len(wav) < 12 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return 0, nil, errors.New("audio illisible : ce n'est pas un WAV")
	}
	var canaux, bits, format int
	pos := 12
	for pos+8 <= len(wav) {
		id := string(wav[pos : pos+4])
		taille := int(binary.LittleEndian.Uint32(wav[pos+4 : pos+8]))
		corps := pos + 8
		if taille < 0 || corps+taille > len(wav) {
			// Bloc annoncé plus grand que le fichier : on prend ce qui reste
			// plutôt que d'échouer — un enregistrement coupé net reste utile.
			taille = len(wav) - corps
		}
		switch id {
		case "fmt ":
			if taille < 16 {
				return 0, nil, errors.New("audio illisible : bloc fmt tronqué")
			}
			format = int(binary.LittleEndian.Uint16(wav[corps : corps+2]))
			canaux = int(binary.LittleEndian.Uint16(wav[corps+2 : corps+4]))
			taux = int(binary.LittleEndian.Uint32(wav[corps+4 : corps+8]))
			bits = int(binary.LittleEndian.Uint16(wav[corps+14 : corps+16]))
		case "data":
			if canaux <= 0 || taux <= 0 {
				return 0, nil, errors.New("audio illisible : bloc data avant fmt")
			}
			ech, err = pcmVersFloat32(wav[corps:corps+taille], format, bits, canaux)
			if err != nil {
				return 0, nil, err
			}
			return taux, ech, nil
		}
		// Les blocs sont alignés sur un nombre pair d'octets.
		pos = corps + taille + taille%2
	}
	return 0, nil, errors.New("audio illisible : aucun bloc data")
}

// pcmVersFloat32 convertit les échantillons bruts. Les canaux sont MOYENNÉS
// plutôt que de garder le premier : un navigateur qui livrerait du stéréo par
// erreur donnerait sinon un canal muet une fois sur deux.
func pcmVersFloat32(data []byte, format, bits, canaux int) ([]float32, error) {
	const pcmEntier, pcmFlottant = 1, 3
	switch {
	case format == pcmEntier && bits == 16:
		n := len(data) / 2 / canaux
		out := make([]float32, n)
		for i := 0; i < n; i++ {
			somme := 0.0
			for c := 0; c < canaux; c++ {
				v := int16(binary.LittleEndian.Uint16(data[2*(i*canaux+c):]))
				somme += float64(v) / 32768
			}
			out[i] = float32(somme / float64(canaux))
		}
		return out, nil
	case format == pcmFlottant && bits == 32:
		n := len(data) / 4 / canaux
		out := make([]float32, n)
		for i := 0; i < n; i++ {
			somme := 0.0
			for c := 0; c < canaux; c++ {
				somme += float64(math.Float32frombits(binary.LittleEndian.Uint32(data[4*(i*canaux+c):])))
			}
			out[i] = float32(somme / float64(canaux))
		}
		return out, nil
	}
	return nil, fmt.Errorf("format audio non géré (format %d, %d bits) — attendu PCM 16 bits", format, bits)
}

// ─── Supervision ─────────────────────────────────────────────────────────────

// asrIdle : au-delà, le serveur s'éteint et rend sa mémoire. Une dictée n'est
// pas un service permanent ; garder un modèle chargé toute la journée pour
// quelques phrases prive la machine de mémoire vive.
const asrIdle = 10 * time.Minute

// asrReady : plafond d'attente au démarrage. Un modèle de 600 Mo lu depuis un
// disque lent prend du temps ; échouer trop tôt afficherait une panne là où il
// n'y a qu'une lenteur.
const asrReady = 120 * time.Second

var wsrvMu sync.Mutex

// wsrvDemarrage : le processus en cours de démarrage, visible SANS le verrou.
// asrStartLocked garde wsrvMu pendant tout le chargement du modèle ; c'est la
// seule poignée qu'un autre appelant (asrShutdownVite) a sur lui pour
// l'interrompre. Nil hors démarrage.
var wsrvDemarrage atomic.Pointer[exec.Cmd]

var wsrv struct {
	cmd    *exec.Cmd
	port   int
	cfg    DictateCfg
	log    *bytes.Buffer
	timer  *time.Timer
	depuis time.Time
}

// asrEnsure garantit un serveur vivant pour la configuration COURANTE, et
// renvoie son port. Si les réglages ont changé depuis le lancement, l'ancien
// est arrêté : c'est ce qui rend un changement de modèle effectif sans
// redémarrer Loki.
func asrEnsure() (int, error) {
	cfg := dictateCfgLoad()
	wsrvMu.Lock()
	defer wsrvMu.Unlock()
	if wsrv.cmd != nil && wsrv.cfg == cfg && wsrv.cmd.ProcessState == nil {
		asrTouchLocked()
		return wsrv.port, nil
	}
	asrStopLocked()
	return asrStartLocked(cfg)
}

func asrStartLocked(cfg DictateCfg) (int, error) {
	if !asrModelPresent(cfg.Model) {
		return 0, fmt.Errorf("modèle de dictée absent : %s (à télécharger dans Paramètres → Dictée)", cfg.Model)
	}
	bin := asrServerBin()
	if bin == "" {
		return 0, fmt.Errorf("serveur de dictée introuvable (image à reconstruire)")
	}
	port, err := portLibre()
	if err != nil {
		return 0, fmt.Errorf("aucun port libre pour le serveur de dictée : %w", err)
	}
	args, err := asrServerArgs(cfg, port)
	if err != nil {
		return 0, err
	}
	log := &bytes.Buffer{}
	cmd := exec.Command(bin, args...)
	cmd.Env = asrServerEnv(os.Environ())
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("démarrage du serveur de dictée : %w", err)
	}
	wsrv.cmd, wsrv.port, wsrv.cfg = cmd, port, cfg
	wsrv.log, wsrv.depuis = log, time.Now()
	wsrvDemarrage.Store(cmd)
	err = asrAttendPret(cmd, port, log)
	wsrvDemarrage.Store(nil)
	if err != nil {
		asrStopLocked()
		return 0, err
	}
	asrTouchLocked()
	return port, nil
}

// asrAttendPret sonde le serveur jusqu'à ce qu'il accepte une connexion. Le
// serveur sherpa-onnx construit son moteur AVANT d'ouvrir sa socket : le simple
// fait qu'elle réponde signifie donc que le modèle est chargé — inutile de lui
// envoyer un audio de test.
//
// Si le processus meurt entre-temps, on rapporte COMMENT il est mort : sur une
// mort par signal, la dernière ligne du journal est celle d'avant le coup fatal —
// elle ressemble à une explication sans en être une.
func asrAttendPret(cmd *exec.Cmd, port int, log *bytes.Buffer) error {
	fin := time.Now().Add(asrReady)
	adr := "127.0.0.1:" + strconv.Itoa(port)
	mort := make(chan error, 1)
	go func() { mort <- cmd.Wait() }()
	for time.Now().Before(fin) {
		select {
		case err := <-mort:
			msg := asrExitReason(err)
			if l := lastLine(log.String()); l != "" {
				msg += " — dernière sortie : " + l
			}
			return fmt.Errorf("le serveur de dictée s'est arrêté au démarrage : %s", msg)
		case <-time.After(200 * time.Millisecond):
		}
		conn, err := net.DialTimeout("tcp", adr, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
	}
	return fmt.Errorf("le serveur de dictée n'a pas répondu en %s", asrReady)
}

func asrTouchLocked() {
	if wsrv.timer != nil {
		wsrv.timer.Stop()
	}
	wsrv.timer = time.AfterFunc(asrIdle, asrShutdown)
}

func asrStopLocked() {
	if wsrv.timer != nil {
		wsrv.timer.Stop()
		wsrv.timer = nil
	}
	if wsrv.cmd != nil && wsrv.cmd.Process != nil {
		_ = wsrv.cmd.Process.Kill()
	}
	wsrv.cmd, wsrv.port, wsrv.log = nil, 0, nil
	wsrv.cfg = DictateCfg{}
}

// asrShutdown arrête le serveur. Appelé par la minuterie d'inactivité et à
// chaque changement de réglages — sans ça, le serveur continuerait de tourner
// avec l'ancien modèle et le nouveau réglage semblerait sans effet.
func asrShutdown() {
	wsrvMu.Lock()
	defer wsrvMu.Unlock()
	asrStopLocked()
}

// asrShutdownVite arrête le serveur sans jamais attendre derrière un démarrage.
// asrShutdown prend wsrvMu, et asrEnsure le garde pendant tout le chargement du
// modèle — jusqu'à asrReady (deux minutes) depuis un disque lent. Appelé par un
// geste de l'interface, ce blocage ferait abandonner le navigateur alors que le
// moteur, lui, est déjà arrêté.
//
// Si le verrou est pris, on tue le processus en train de démarrer : sa mort fait
// sortir asrAttendPret, le verrou se libère, et on finit le ménage par la voie
// normale. La dictée qui attendait reçoit une erreur de démarrage — c'est
// l'utilisateur qui vient de demander la place, pas une panne.
func asrShutdownVite() {
	for i := 0; i < 20; i++ {
		if wsrvMu.TryLock() {
			asrStopLocked()
			wsrvMu.Unlock()
			return
		}
		if cmd := wsrvDemarrage.Load(); cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// asrInfer garantit le serveur puis lui soumet le WAV.
func asrInfer(ctx context.Context, wav []byte) (string, error) {
	port, err := asrEnsure()
	if err != nil {
		return "", err
	}
	return asrInferSur(ctx, "ws://127.0.0.1:"+strconv.Itoa(port), wav)
}

func asrEtat() map[string]any {
	cfg := dictateCfgLoad()
	wsrvMu.Lock()
	defer wsrvMu.Unlock()
	etat := map[string]any{
		"actif":   wsrv.cmd != nil,
		"modele":  cfg.Model,
		"present": asrModelPresent(cfg.Model),
		// Le moteur livré est le build statique CPU (voir asrServerBin) : le dire
		// vaut mieux que laisser chercher un réglage de carte qui n'existe pas.
		"device": "cpu",
	}
	if wsrv.cmd != nil {
		etat["depuis_s"] = int(time.Since(wsrv.depuis).Seconds())
	}
	return etat
}
