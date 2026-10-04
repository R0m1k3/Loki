package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SLOT_PERSIST, côté process web : garder l'état du slot de la discussion
// avant une bascule de preset, le recharger au retour (backend_serve_persist.go
// pour la clé et les refus au lancement).
//
// Sauver : seulement si la dernière requête de Loki passée par le slot 0 était
// une étape d'un tour de la discussion (le tampon d'engineMarkMain) — un
// vérificateur, une tâche, un titre ou un client /v1 passés depuis, et le slot
// ne porte plus ce qu'on croit. Ni brouillon spéculatif actif, ni état trop
// gros (plafond fixe, place libre vérifiée), ni contexte trop court pour valoir
// une écriture. Le fichier est écrit sous un nom provisoire et ne prend son
// nom définitif que si aucune requête n'est partie pendant l'écriture.
//
// Recharger : avant la première requête de la discussion sur le moteur relancé,
// et seulement si le fichier porte la clé de CE moteur (même modèle, même
// build, mêmes réglages — tout ce qui touche au cache) et l'empreinte de CETTE
// discussion, que le slot 0 n'a encore servi aucune tâche (/slots sans
// id_task) et qu'aucun brouillon spéculatif n'y tourne. Une seule chance : le
// fichier est retiré après la tentative, réussie ou non. Toute erreur retombe
// en silence sur un calcul normal — llama.cpp vide lui-même le slot sur un
// rechargement raté.
//
// Sur un modèle hybride (Qwen3.5/3.6), le fichier n'a pas de points de reprise
// : il ne sert que si la requête suivante prolonge exactement les jetons
// gardés, ce qui suppose un gabarit qui rend le dernier tour à l'identique.

// slotPersistMaxBytes : au-delà, l'état n'est pas gardé. Un 27B dense en f16 à
// 32k jetons pèse 8 Gio ; plus gros, l'écriture et la relecture coûtent autant
// que le recalcul sur un disque lent.
const slotPersistMaxBytes = 8 << 30

// slotPersistMinTokens : en dessous, le recalcul est trop court pour valoir une
// écriture.
const slotPersistMinTokens = 4096

// Bornes des appels au moteur : écrire ou relire 8 Gio sur un disque lent.
var (
	slotPersistSaveTimeout    = 3 * time.Minute
	slotPersistRestoreTimeout = 2 * time.Minute
)

// slotPersistName : le fichier d'une discussion sous une clé de moteur.
func slotPersistName(key, conv string) string {
	h := fnv.New64a()
	h.Write([]byte(conv))
	return fmt.Sprintf("persist-%s-%012x.bin", key, h.Sum64()&0xffffffffffff)
}

func slotPersistDir() string { return filepath.Join(LokiHome(), "slots") }

func slotPersistLog(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[slot] "+format+"\n", args...)
}

// engineMainConv : la discussion dont le slot porte plausiblement le tour
// (engineSlotHolds), et le numéro de cette requête ; vide = aucune.
func engineMainConv() (conv string, seq uint64) {
	model, window := engineMainNow()
	engineGate.mu.Lock()
	defer engineGate.mu.Unlock()
	m := engineGate.main
	if m.seq == 0 || m.seq != engineGate.seq || m.model != model || m.window != window {
		return "", 0
	}
	return m.conv, m.seq
}

// slotPersistSnap : ce qu'une bascule doit savoir du moteur EN SERVICE, relevé
// avant que la configuration ne change (le modèle et la fenêtre du tampon se
// lisent dans la configuration).
type slotPersistSnap struct {
	conv, key string
	seq       uint64
	cfg       map[string]string
	iso       slotIsolator // port et clé d'API du moteur en service
}

// slotPersistPrepare : nil = rien à garder (clé absente, moteur sans clé de
// persistance, slot qui ne porte pas un tour de discussion).
func slotPersistPrepare() *slotPersistSnap {
	cfg := ReadConfig()
	if !slotPersistOn(cfg) || isExternalConfig(cfg) {
		return nil
	}
	key := readSlotPersistMarker(LokiHome())
	if key == "" {
		return nil
	}
	conv, seq := engineMainConv()
	if conv == "" {
		return nil
	}
	return &slotPersistSnap{conv: conv, key: key, seq: seq, cfg: cfg, iso: localSlotIsolator(resolveChatEndpoint())}
}

// slotState : ce que /slots dit du slot 0.
type slotState struct {
	found, processing, speculative, used bool
	tokens                               int
}

func (s slotIsolator) slot0(ctx context.Context) (slotState, error) {
	var slots []map[string]json.RawMessage
	if err := s.get(ctx, "/slots", &slots); err != nil {
		return slotState{}, err
	}
	for _, sl := range slots {
		var id int
		if json.Unmarshal(sl["id"], &id) != nil || id != 0 {
			continue
		}
		st := slotState{found: true}
		_ = json.Unmarshal(sl["is_processing"], &st.processing)
		_ = json.Unmarshal(sl["speculative"], &st.speculative)
		_ = json.Unmarshal(sl["n_prompt_tokens"], &st.tokens)
		_, st.used = sl["id_task"]
		return st, nil
	}
	return slotState{}, nil
}

// action : POST /slots/0?action=…, borné par timeout. Rend le nombre d'octets
// écrits ou lus d'après la réponse (0 = non dit).
func (s slotIsolator) action(ctx context.Context, action, filename string, timeout time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"filename": filename})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/slots/0?action="+action, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	s.auth(req.Header.Set)
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s : %s %s", action, resp.Status, strings.TrimSpace(string(raw[:min(len(raw), 300)])))
	}
	var out struct {
		Written int64 `json:"n_written"`
		Read    int64 `json:"n_read"`
	}
	_ = json.Unmarshal(raw, &out)
	return max(out.Written, out.Read), nil
}

func localSlotIsolator(ep chatEndpoint) slotIsolator {
	return slotIsolator{base: fmt.Sprintf("http://localhost:%d", LLMPort()), auth: ep.auth, client: http.DefaultClient}
}

// slotPersistSize : slotPersistEstimate, remplaçable par les tests.
var slotPersistSize = slotPersistEstimate

// slotPersistEstimate : octets de l'état de tokens jetons sous cfg ; 0 =
// inconnu (et l'état n'est alors pas gardé : on ne borne pas l'inconnu).
func slotPersistEstimate(cfg map[string]string, tokens int) int64 {
	p, err := resolveServeModelPath(strings.TrimSpace(cfg["MODEL"]))
	if err != nil {
		return 0
	}
	g, err := ggufMeta(p)
	if err != nil {
		return 0
	}
	argEnv := map[string]string{}
	for _, k := range fidelityArgEnv {
		if v := os.Getenv(k); v != "" {
			argEnv[k] = v
		}
	}
	kt, vt, _ := effectiveKVTypes(cfg, splitArgs(cfg["EXTRA_ARGS"]), argEnv)
	perTok, rec := ggufStateBytes(g, kt, vt)
	if perTok <= 0 {
		return 0
	}
	return int64(math.Ceil(perTok*float64(tokens))) + rec
}

// save garde l'état du slot de la discussion, juste avant l'arrêt du moteur.
// Jamais d'erreur rendue : au pire, rien n'est gardé.
func (s *slotPersistSnap) save() {
	if s == nil {
		return
	}
	iso := s.iso
	ctx := context.Background()
	st, err := iso.slot0(ctx)
	switch {
	case err != nil:
		slotPersistLog("état non gardé : /slots illisible (%v)", err)
		return
	case !st.found || st.processing:
		return
	case st.speculative:
		slotPersistLog("état non gardé : décodage spéculatif actif (le brouillon ne serait pas sauvé)")
		return
	case st.tokens < slotPersistMinTokens:
		return
	}
	est := slotPersistSize(s.cfg, st.tokens)
	if est <= 0 || est > slotPersistMaxBytes {
		slotPersistLog("état non gardé : %d jetons, taille estimée %d Mio (plafond %d Mio)", st.tokens, est>>20, slotPersistMaxBytes>>20)
		return
	}
	dir := slotPersistDir()
	if free := diskFree(dir); free < 2*est+(1<<30) {
		slotPersistLog("état non gardé : place libre insuffisante ou inconnue dans %s", dir)
		return
	}
	name := slotPersistName(s.key, s.conv)
	tmp := "tmp-" + name
	end, seq := engineRequestBegin(nil)
	t0 := time.Now()
	n, err := iso.action(ctx, "save", tmp, slotPersistSaveTimeout)
	end()
	engineGate.mu.Lock()
	moved := seq != s.seq+1 || engineGate.seq != seq
	engineGate.mu.Unlock()
	if err != nil || moved {
		_ = os.Remove(filepath.Join(dir, tmp))
		if err != nil {
			slotPersistLog("état non gardé : %v", err)
		}
		return
	}
	if err := os.Rename(filepath.Join(dir, tmp), filepath.Join(dir, name)); err != nil {
		_ = os.Remove(filepath.Join(dir, tmp))
		slotPersistLog("état non gardé : %v", err)
		return
	}
	slotPersistPrune(dir)
	slotPersistLog("état de la discussion gardé : %d jetons, %d Mio en %s", st.tokens, n>>20, time.Since(t0).Round(time.Millisecond))
}

// slotPersistTried : fichiers déjà tentés (nom et date), pour qu'un fichier
// qu'on n'aurait pas pu retirer ne soit pas relu à chaque requête.
var slotPersistTried struct {
	mu sync.Mutex
	m  map[string]bool
}

// slotPersistRestore recharge l'état gardé de conv dans le slot 0, si tout
// concorde. Appelée juste avant une requête de la discussion vers le moteur
// local ; sans la clé, une lecture de configuration et rien d'autre.
func slotPersistRestore(ep chatEndpoint, conv string) {
	if ep.External || conv == "" {
		return
	}
	cfg := ReadConfig()
	if !slotPersistOn(cfg) || isExternalConfig(cfg) {
		return
	}
	key := readSlotPersistMarker(LokiHome())
	if key == "" {
		return
	}
	name := slotPersistName(key, conv)
	path := filepath.Join(slotPersistDir(), name)
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	// Un fichier, une tentative : le même nom réécrit par une bascule suivante
	// (A→B→A→B→A) est un autre fichier.
	once := fmt.Sprintf("%s|%d", name, fi.ModTime().UnixNano())
	iso := localSlotIsolator(ep)
	ctx := context.Background()
	st, err := iso.slot0(ctx)
	if err != nil || !st.found {
		return // moteur pas encore prêt : la requête suivante retentera
	}
	slotPersistTried.mu.Lock()
	if slotPersistTried.m == nil {
		slotPersistTried.m = map[string]bool{}
	}
	tried := slotPersistTried.m[once]
	slotPersistTried.m[once] = true
	slotPersistTried.mu.Unlock()
	if tried {
		return
	}
	// Une seule chance, quoi qu'il arrive.
	defer os.Remove(path)
	switch {
	case st.used || st.processing:
		slotPersistLog("état gardé ignoré : le slot a déjà servi depuis le lancement")
		return
	case st.speculative:
		slotPersistLog("état gardé ignoré : décodage spéculatif actif")
		return
	}
	end, _ := engineRequestBegin(nil)
	t0 := time.Now()
	n, err := iso.action(ctx, "restore", name, slotPersistRestoreTimeout)
	end()
	if err != nil {
		slotPersistLog("état gardé non rechargé, calcul normal : %v", err)
		return
	}
	slotPersistLog("état de la discussion rechargé : %d Mio en %s", n>>20, time.Since(t0).Round(time.Millisecond))
}
