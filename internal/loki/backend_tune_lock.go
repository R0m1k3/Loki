package loki

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Verrou de l'optimiseur (loki tune, bouton « Optimiser »).
//
// Pendant une optimisation, la VRAM appartient au moteur d'ESSAI : le vrai
// moteur est arrêté, et le relancer à côté (bascule de preset, enregistrement du
// preset actif, choix des GPU, mise à jour du moteur, « recharger » la VRAM…)
// chargerait deux modèles sur des cartes qui n'en tiennent qu'un — un OOM certain,
// et une mesure faussée. Un drapeau en mémoire ne suffit pas : « loki tune » en
// ligne de commande et le processus web sont deux processus distincts. Le verrou
// est donc un FICHIER de LOKI_HOME, créé de façon exclusive, que tout le monde
// lit :
//
//   - serviceAction("start"/"restart") le consulte sur toutes les plateformes —
//     aucun démarrage du moteur ne passe à côté ;
//   - le chat, les tâches planifiées (qui attendent), les jobs du mode code, les
//     clients /v1, le bench, la bascule et l'enregistrement de preset refusent
//     avec une phrase claire.
//
// Le fichier porte l'identité du processus qui le tient (PID, instant de
// démarrage, binaire) et celle de l'essai en cours. Un verrou dont le
// propriétaire est mort (Loki redémarré en plein essai) est périmé : avant tout
// démarrage du moteur, l'essai orphelin est arrêté — seulement si PID, instant
// de démarrage ET binaire concordent, jamais sur la foi d'un PID recyclé — puis
// le fichier est retiré.

const tuneLockName = "tune.lock"

// tuneLockFreshness : un verrou illisible plus jeune que ça est en cours
// d'écriture, pas abandonné.
const tuneLockFreshness = 10 * time.Second

var errTuneBusy = errors.New("optimisation en cours (loki tune) : le moteur est réservé aux essais — " +
	"attends la fin ou annule-la")

// tuneProc : un processus désigné sans ambiguïté.
type tuneProc struct {
	PID   int      `json:"pid"`
	Start string   `json:"start,omitempty"` // procIdentity ; vide = illisible au moment de l'écrire
	Exes  []string `json:"exes,omitempty"`  // binaires admis (un « loki serve » devient llama-server par exec)
}

// tuneOwner : le contenu du verrou.
type tuneOwner struct {
	Owner tuneProc  `json:"owner"`
	Via   string    `json:"via"`   // « web » ou « cli »
	Phase string    `json:"phase"` // « essais » ou « application »
	Since int64     `json:"since"`
	Trial *tuneProc `json:"trial,omitempty"`
	// MainWasActive : le vrai moteur tournait avant l'optimisation. Un verrou
	// périmé trouvé au démarrage du processus web le relance.
	MainWasActive bool `json:"main_was_active"`
	// Application en cours (phase « application ») : la version d'avant du
	// preset, sauvée avant toute écriture. Un verrou périmé dans cette phase
	// veut dire que l'application n'a jamais été vérifiée : elle est défaite.
	Backup     string `json:"backup,omitempty"`
	PresetID   string `json:"preset_id,omitempty"`
	PresetName string `json:"preset_name,omitempty"`
}

func tuneLockPath() string { return filepath.Join(LokiHome(), tuneLockName) }

// tuneProcAlive dit si p désigne encore le MÊME processus. Variable pour les
// tests.
var tuneProcAlive = func(p tuneProc) bool {
	start, exe, ok := procIdentity(p.PID)
	if !ok {
		return false
	}
	if p.Start != "" && start != p.Start {
		return false
	}
	if exe == "" || len(p.Exes) == 0 {
		return true
	}
	for _, e := range p.Exes {
		if sameExe(e, exe) {
			return true
		}
	}
	return false
}

// sameExe compare deux binaires par leur nom de fichier (sans .exe, sans
// casse) : /proc/<pid>/exe résout les liens symboliques, ps n'en donne parfois
// que le nom.
func sameExe(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSuffix(strings.TrimSpace(s), " (deleted)") // binaire remplacé (Linux)
		s = strings.ToLower(filepath.Base(strings.ReplaceAll(s, `\`, "/")))
		return strings.TrimSuffix(s, ".exe")
	}
	return a != "" && b != "" && norm(a) == norm(b)
}

// selfProc : ce processus, tel que le verrou le note.
func selfProc() tuneProc {
	p := tuneProc{PID: os.Getpid()}
	if start, exe, ok := procIdentity(p.PID); ok {
		p.Start = start
		if exe != "" {
			p.Exes = []string{exe}
		}
	}
	if self, err := os.Executable(); err == nil && len(p.Exes) == 0 {
		p.Exes = []string{self}
	}
	return p
}

func readTuneOwner(path string) (tuneOwner, bool) {
	var o tuneOwner
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &o) != nil || o.Owner.PID <= 0 {
		return tuneOwner{}, false
	}
	return o, true
}

// tuneActive : une optimisation tient-elle le moteur, dans ce processus ou dans
// un autre ? Un verrou périmé (propriétaire mort) ne compte pas.
func tuneActive() (tuneOwner, bool) {
	o, ok := readTuneOwner(tuneLockPath())
	if !ok {
		return tuneOwner{}, false
	}
	return o, tuneOwnerAlive(o)
}

// tuneHeld : verrous pris par CE processus et pas encore rendus (tuneHeldKey).
var tuneHeld sync.Map

func tuneHeldKey(o tuneOwner) string { return fmt.Sprintf("%s|%d", o.Via, o.Since) }

// tuneOwnerAlive : le propriétaire d'un verrou vit-il encore ? Un verrou qui
// porte NOTRE PID sans être l'un des nôtres vient d'un processus mort dont le
// PID nous est échu (redémarrage) : périmé, quoi que dise son identité.
func tuneOwnerAlive(o tuneOwner) bool {
	if o.Owner.PID == os.Getpid() {
		if _, ours := tuneHeld.Load(tuneHeldKey(o)); !ours {
			return false
		}
	}
	return tuneProcAlive(o.Owner)
}

// tuneGuard : nil si rien ne tient le moteur, errTuneBusy sinon.
func tuneGuard() error {
	if _, busy := tuneActive(); busy {
		return errTuneBusy
	}
	return nil
}

// tuneDenyHTTP répond 409 et rend true quand une optimisation tient le moteur.
func tuneDenyHTTP(w http.ResponseWriter) bool {
	if err := tuneGuard(); err != nil {
		sendJSON(w, http.StatusConflict, map[string]any{"ok": false, "tuning": true, "error": err.Error()})
		return true
	}
	return false
}

// tuneLock : le verrou tenu par CE processus.
type tuneLock struct {
	mu    sync.Mutex
	path  string
	owner tuneOwner
	held  bool // inscrit dans tuneHeld, jusqu'à release
}

// tuneLockAcquire prend le verrou, ou dit qui le tient. Un verrou périmé est
// d'abord écarté (tuneReapStale), essai orphelin compris.
func tuneLockAcquire(via string, mainActive bool) (*tuneLock, error) {
	path := tuneLockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	l := &tuneLock{path: path, owner: tuneOwner{Owner: selfProc(), Via: via, Phase: "essais",
		Since: time.Now().Unix(), MainWasActive: mainActive}}
	for attempt := 0; attempt < 3; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			b, _ := json.Marshal(l.owner)
			_, werr := f.Write(b)
			cerr := f.Close()
			if werr != nil || cerr != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("verrou de l'optimiseur : %v", errors.Join(werr, cerr))
			}
			l.held = true
			tuneHeld.Store(tuneHeldKey(l.owner), true)
			return l, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if o, busy := tuneActive(); busy {
			return nil, fmt.Errorf("une optimisation tourne déjà (%s, PID %d, depuis %s)", o.Via, o.Owner.PID,
				time.Unix(o.Since, 0).Format("15:04"))
		}
		tuneReapStale()
	}
	return nil, errors.New("verrou de l'optimiseur indisponible : réessaie")
}

// write réécrit le verrou d'un bloc (fichier voisin puis renommage) : un
// lecteur ne voit jamais un JSON à moitié écrit.
func (l *tuneLock) write() error {
	b, err := json.Marshal(l.owner)
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	// Windows refuse de remplacer un fichier qu'un autre processus lit à cet
	// instant (tuneGuard d'une requête) : on réessaie un peu avant de renoncer.
	for i := 0; ; i++ {
		err := os.Rename(tmp, l.path)
		if err == nil || i == 4 {
			if err != nil {
				_ = os.Remove(tmp)
			}
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// setTrial note l'essai en cours (nil : aucun), AVANT qu'il ne charge quoi que
// ce soit d'important : c'est ce qui permet à un Loki redémarré de l'arrêter.
func (l *tuneLock) setTrial(p *tuneProc) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.owner.Trial = p
	_ = l.write()
}

func (l *tuneLock) setPhase(phase string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.owner.Phase = phase
	_ = l.write()
}

// setBackup note la sauvegarde du preset avant son écriture (tuneApply).
func (l *tuneLock) setBackup(backup, id, name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.owner.Backup, l.owner.PresetID, l.owner.PresetName = backup, id, name
	_ = l.write()
}

// release rend le verrou — seulement s'il est encore le nôtre.
func (l *tuneLock) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held {
		l.held = false
		tuneHeld.Delete(tuneHeldKey(l.owner))
	}
	if o, ok := readTuneOwner(l.path); ok && o.Owner.PID == l.owner.Owner.PID && o.Owner.Start == l.owner.Owner.Start {
		_ = os.Remove(l.path)
	}
}

// tuneReapStale écarte un verrou dont le propriétaire est mort : l'essai qu'il
// désigne est arrêté s'il est encore là (identité complète vérifiée), puis le
// fichier disparaît. Rend MainWasActive du verrou écarté (false sans verrou
// périmé). Le renommage d'abord : de deux processus qui trouvent le même verrou
// périmé, un seul l'emporte, et aucun ne retire un verrou neuf posé entre-temps.
func tuneReapStale() (mainWasActive bool) {
	path := tuneLockPath()
	o, ok := readTuneOwner(path)
	if !ok {
		// Illisible (écriture interrompue) : périmé lui aussi, s'il est là —
		// mais pas tout juste créé : entre sa création exclusive et l'écriture de
		// son contenu, un verrou neuf est vide quelques instants, et le retirer
		// laisserait passer une seconde optimisation ou un démarrage du moteur.
		if fi, err := os.Stat(path); err == nil && time.Since(fi.ModTime()) > tuneLockFreshness {
			_ = os.Remove(path)
		}
		return false
	}
	if tuneOwnerAlive(o) {
		return false
	}
	stale := fmt.Sprintf("%s.stale-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	if os.Rename(path, stale) != nil {
		return false
	}
	defer os.Remove(stale)
	if o.Trial != nil && tuneProcAlive(*o.Trial) {
		fmt.Fprintf(os.Stderr, "[loki tune] essai orphelin (PID %d) d'une optimisation interrompue : arrêt\n", o.Trial.PID)
		tuneKillTree(o.Trial.PID, nil)
	}
	tuneUndoApply(o)
	return o.MainWasActive
}

// tuneUndoApply : une application interrompue avant sa vérification (Loki tué
// pendant la sonde) remet la version d'avant du preset — c'est ce que la
// vérification aurait fait en échouant. Sans sauvegarde notée, rien.
func tuneUndoApply(o tuneOwner) {
	if o.Phase != "application" || o.Backup == "" || o.PresetID == "" {
		return
	}
	b, err := os.ReadFile(o.Backup)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[loki tune] application interrompue : sauvegarde illisible (%v)\n", err)
		return
	}
	if _, _, err := SavePresetApplying(o.PresetID, o.PresetName, string(b)); err != nil {
		fmt.Fprintf(os.Stderr, "[loki tune] application interrompue : retour impossible (%v) — l'ancienne version est dans %s\n", err, o.Backup)
		return
	}
	fmt.Fprintf(os.Stderr, "[loki tune] application interrompue avant sa vérification : ancienne version du preset rétablie\n")
}

// tuneRecoverStale : une optimisation interrompue (Loki redémarré en plein
// essai, « loki tune » tué par kill -9) a laissé le vrai moteur arrêté et peut-
// être un essai en VRAM. On arrête l'essai, puis on relance le moteur s'il
// tournait avant — ce que l'optimisation aurait fait en finissant. Sans
// verrou, un simple stat : rien d'autre n'est lu.
func tuneRecoverStale() {
	if _, err := os.Stat(tuneLockPath()); err != nil {
		return
	}
	if tuneReapStale() {
		tuneRecoverStart()
	}
}

// tuneRecoverStart relance le moteur après une optimisation interrompue, sauf
// preset externe, moteur déjà reparti ou configuration incomplète. Variable
// pour les tests.
var tuneRecoverStart = func() {
	if externalActive() || serviceIsActive() {
		return
	}
	if err := preflightEngine(); err != nil {
		return
	}
	fmt.Println(dim("[loki tune] optimisation interrompue : relance du moteur"))
	_ = serviceActionOS("start")
}

// tuneRecoverEvery : la période de la reprise dans le processus web.
const tuneRecoverEvery = 30 * time.Second

// tuneRecoverWatch : au démarrage du processus web, puis à chaque tic. Le
// démarrage seul ne suffisait pas : un « loki tune » en ligne de commande tué
// sans ses defers (kill -9, terminal perdu) laissait le moteur arrêté jusqu'au
// prochain redémarrage de l'interface. Le tic ne coûte qu'un stat tant
// qu'aucun verrou n'existe ; pendant une optimisation vivante, une lecture du
// verrou et de l'identité de son propriétaire.
func tuneRecoverWatch(tick <-chan time.Time) {
	tuneRecoverStale()
	for range tick {
		tuneRecoverStale()
	}
}
