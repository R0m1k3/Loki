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
		s = strings.ToLower(filepath.Base(strings.ReplaceAll(strings.TrimSpace(s), `\`, "/")))
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
	return o, tuneProcAlive(o.Owner)
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
	return os.Rename(tmp, l.path)
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

// release rend le verrou — seulement s'il est encore le nôtre.
func (l *tuneLock) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
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
		// Illisible (écriture interrompue) : périmé lui aussi, s'il est là.
		if _, err := os.Stat(path); err == nil {
			_ = os.Remove(path)
		}
		return false
	}
	if tuneProcAlive(o.Owner) {
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
	return o.MainWasActive
}

// tuneRecoverAtBoot : au démarrage du processus web, une optimisation
// interrompue (Loki tué en plein essai) a laissé le vrai moteur arrêté et peut-
// être un essai en VRAM. On arrête l'essai, puis on relance le moteur s'il
// tournait avant — ce que l'optimisation aurait fait en finissant.
func tuneRecoverAtBoot() {
	if _, err := os.Stat(tuneLockPath()); err != nil {
		return
	}
	if !tuneReapStale() || externalActive() || serviceIsActive() {
		return
	}
	if err := preflightEngine(); err != nil {
		return
	}
	fmt.Println(dim("[loki tune] optimisation interrompue : relance du moteur"))
	_ = serviceActionOS("start")
}
