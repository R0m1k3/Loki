package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// API interne de la surface /v1 servie par la façade Rust (lot 5).
//
// Rust relaie désormais /v1 directement au moteur, mais les DÉCISIONS restent
// ici, parce qu'elles dépendent de l'état de la conversation que ce process
// tient en mémoire :
//
//   - route : ce qu'il faut pour router (port du moteur, Strata, clé des
//     complétions, second slot en service) ;
//   - begin : la requête entre « en vol » (engineRequestBeginSide : le slot de
//     la discussion n'est pas effacé pendant ce temps, un préchauffage est
//     annulé si besoin), puis les refus benchmark / optimisation, puis la trace
//     de perte de cache (perfNoteForeign) — dans l'ordre d'oaiHandler ;
//   - end : la requête est terminée (fin du flux, ou client parti).
//
// Un ticket jamais rendu (façade tuée en plein flux) expire au bout de
// oaiTicketMaxAge : sans ça, le compteur de requêtes en vol resterait bloqué
// et le slot ne serait plus jamais isolé.

const oaiTicketMaxAge = 6 * time.Hour

type oaiTicket struct {
	end  func()
	when time.Time
	// seq / mainConv : requête d'un tour de discussion tenu par la façade —
	// une fois acceptée par le moteur (served), le slot porte cette discussion
	// (engineMarkMain). Vides pour un client /v1.
	seq      uint64
	mainConv string
}

var oaiTickets = struct {
	mu   sync.Mutex
	next uint64
	m    map[uint64]oaiTicket
}{m: map[uint64]oaiTicket{}}

func handleInternalOAIRoute(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	method, path := q.Get("method"), q.Get("path")
	key, err := effectiveAPIKeyErr()
	out := map[string]any{
		"port":     LLMPort(),
		"strata":   strataActive(),
		"key":      key,
		"key_ok":   err == nil,
		"max_body": sideSlotProxyMaxBody,
	}
	// La sonde du second slot interroge le moteur (avec cache) : seulement
	// pour une complétion, comme sideSlotProxyRewrite.
	out["side_live"] = method == http.MethodPost && strings.HasPrefix(path, "/v1/") &&
		sideSlotLive(resolveChatEndpoint())
	sendJSON(w, http.StatusOK, out)
}

func handleInternalOAIBegin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Read   bool `json:"read"`    // GET/HEAD : ne calcule rien dans un slot
		Side   bool `json:"side"`    // corps routé vers le second slot
		PostV1 bool `json:"post_v1"` // complétion d'un client externe
		// MainConv : étape d'un tour de cette discussion, tenue par la façade
		// (LOKI_CHAT=rust) — comptée comme la requête principale du Go.
		MainConv string `json:"main_conv"`
		// PrewarmKeep : étape d'un tour de la façade qui prolonge exactement
		// le préchauffage en vol (PREWARM, facade_prewarm.go) — il n'est pas
		// annulé, la requête le suit dans le même slot.
		PrewarmKeep bool `json:"prewarm_keep"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&in) != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "POST JSON attendu"})
		return
	}
	var end func()
	var seq uint64
	if in.Read {
		end, seq = engineRequestBeginSide(func(*prewarmRun) bool { return true }, true)
	} else {
		var keep func(*prewarmRun) bool
		if in.PrewarmKeep {
			keep = func(*prewarmRun) bool { return true }
		}
		end, seq = engineRequestBeginSide(keep, in.Side)
	}
	if in.PostV1 {
		refuse := func(retry, msg, code string) {
			end()
			sendJSON(w, http.StatusOK, oaiRefusal(http.StatusServiceUnavailable, retry, msg, code))
		}
		if benchRunning() {
			refuse("30", "benchmark en cours sur ce moteur — réessaie dans un moment", "benchmark_running")
			return
		}
		if tuneGuard() != nil {
			refuse("60", "optimisation du moteur en cours — réessaie dans quelques minutes", "tuning_running")
			return
		}
		perfNoteForeign()
	}
	now := time.Now()
	oaiTickets.mu.Lock()
	oaiPurgeStaleLocked(now)
	// Bascule de modèle demandée par un client /v1 (web_internal_oai_switch.go) :
	// les clients /v1 attendent qu'elle soit finie. Le contrôle se fait sous
	// oaiTickets.mu, comme la levée du drapeau : pas de fenêtre entre les deux.
	// Les étapes des tours de la façade (main_conv) ne sont pas refusées : aucun
	// tour ne tourne pendant une bascule, qui tient le verrou de génération.
	if (in.PostV1 || in.Read) && oaiSwitching.Load() {
		oaiTickets.mu.Unlock()
		end()
		sendJSON(w, http.StatusOK, oaiRefusal(http.StatusServiceUnavailable, "30",
			"changement de modèle en cours — réessaie dans un moment", "model_switching"))
		return
	}
	oaiTickets.next++
	id := oaiTickets.next
	oaiTickets.m[id] = oaiTicket{end: end, when: now, seq: seq, mainConv: in.MainConv}
	oaiTickets.mu.Unlock()
	sendJSON(w, http.StatusOK, map[string]any{"ticket": id})
}

func handleInternalOAIEnd(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Ticket uint64 `json:"ticket"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&in) != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "POST JSON attendu"})
		return
	}
	oaiTickets.mu.Lock()
	t, ok := oaiTickets.m[in.Ticket]
	delete(oaiTickets.m, in.Ticket)
	oaiTickets.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	t.end()
	w.WriteHeader(http.StatusNoContent)
}

// oaiPurgeStaleLocked rend les tickets plus vieux qu'oaiTicketMaxAge (façade
// tuée en plein flux). À appeler sous oaiTickets.mu.
func oaiPurgeStaleLocked(now time.Time) {
	for id, t := range oaiTickets.m {
		if now.Sub(t.when) > oaiTicketMaxAge {
			t.end()
			delete(oaiTickets.m, id)
		}
	}
}

// oaiRefusal : la forme d'un refus rendu à la façade, qui le traduit en erreur
// OpenAI (status = code HTTP envoyé au client, retry_after = Retry-After).
func oaiRefusal(status int, retry, msg, code string) map[string]any {
	return map[string]any{"refuse": map[string]any{
		"status": status, "message": msg,
		"type": "server_error", "code": code, "retry_after": retry,
	}}
}

// oaiInflight : nombre de tickets ouverts (tests).
func oaiInflight() int {
	oaiTickets.mu.Lock()
	defer oaiTickets.mu.Unlock()
	return len(oaiTickets.m)
}

// handleInternalOAIServed : la requête du ticket a été acceptée par le moteur
// (réponse 200) — engineServed, et pour l'étape d'un tour de discussion,
// engineMarkMain : le slot porte désormais cette discussion. Le ticket reste
// ouvert jusqu'à end (fin de lecture du flux).
func handleInternalOAIServed(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Ticket uint64 `json:"ticket"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&in) != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "POST JSON attendu"})
		return
	}
	oaiTickets.mu.Lock()
	t, ok := oaiTickets.m[in.Ticket]
	oaiTickets.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	engineServed()
	engineMarkMain(t.seq, t.mainConv)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Bascule de modèle demandée par un client /v1
// ---------------------------------------------------------------------------
//
// La façade sert /v1/models avec les presets locaux d'au moins 64 000 jetons
// (le plancher de Hermes Agent). Une complétion qui nomme l'un d'eux, autre que
// l'actif, demande ici la bascule. Tout se décide dans ce process, qui tient le
// verrou de génération, les tickets /v1 et le moteur : la façade ne fait que
// sérialiser et traduire les refus. Rien n'est coupé : occupé = refus.

var errOAISwitchBusy = fmt.Errorf("un client externe (API /v1) change de modèle — réessaie dans une minute")

// oaiSwitching : une bascule /v1 est en cours. Écrit seulement sous
// oaiTickets.mu (avec le contrôle des tickets), lu sans verrou par les sites
// du verrou de génération (pas d'imbrication avec conv.mu).
var oaiSwitching atomic.Bool

// oaiLastSwitch : fin de la dernière bascule /v1 réussie (sous oaiTickets.mu).
var oaiLastSwitch time.Time

// oaiSwitchCooldown : au plus une bascule /v1 par minute — un client mal
// réglé ne recharge pas les modèles en boucle.
const oaiSwitchCooldown = 60 * time.Second

// Variables pour les tests (aucun moteur n'y tourne).
var (
	oaiSwitchHealth       = healthCheck
	oaiSwitchReadyTimeout = 180 * time.Second
	oaiSwitchPoll         = time.Second
	// oaiSwitchApply : la séquence synchrone de SwitchToPreset pour un preset
	// local, sans son tuneGuard ni sa branche externe (déjà traités), et par
	// presetServiceAction (la variable de test du chemin façade).
	oaiSwitchApply = func(abs string) error {
		persist := slotPersistPrepare()
		if err := applyPresetFile(abs); err != nil {
			return err
		}
		persist.save()
		return presetServiceAction("restart")
	}
)

// oaiSwitchGuard : errOAISwitchBusy pendant une bascule /v1. Appelé en tête
// des sites qui prennent le verrou de génération : un Reset (« nouvelle
// conversation ») le libère sans condition, le drapeau tient bon.
func oaiSwitchGuard() error {
	if oaiSwitching.Load() {
		return errOAISwitchBusy
	}
	return nil
}

// handleInternalOAISwitch : POST {path} → {"ok":true} ou {"refuse":{…}} (200),
// 400 {"error"} pour un corps illisible ou un chemin refusé.
func handleInternalOAISwitch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string `json:"path"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&in) != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "POST JSON attendu"})
		return
	}
	abs, err := presetPathCheck(in.Path)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	refuse := func(retry, msg, code string) {
		sendJSON(w, http.StatusOK, oaiRefusal(http.StatusServiceUnavailable, retry, msg, code))
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "preset illisible : " + err.Error()})
		return
	}
	if isExternalConfig(parseEnv(string(raw))) {
		sendJSON(w, http.StatusOK, oaiRefusal(http.StatusBadRequest, "", "preset externe : pas de bascule", "external_preset"))
		return
	}
	id := strings.TrimSuffix(filepath.Base(abs), ".env")

	oaiTickets.mu.Lock()
	wait := oaiSwitchCooldown - time.Since(oaiLastSwitch)
	oaiTickets.mu.Unlock()
	if wait > 0 {
		refuse(strconv.Itoa(int(math.Ceil(wait.Seconds()))),
			"une bascule a eu lieu il y a moins d'une minute", "switch_cooldown")
		return
	}
	if benchRunning() {
		refuse("30", "benchmark en cours sur ce moteur — réessaie dans un moment", "benchmark_running")
		return
	}
	if tuneGuard() != nil {
		refuse("60", "optimisation du moteur en cours — réessaie dans quelques minutes", "tuning_running")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release, err := conv.measureLease(cancel, errOAISwitchBusy)
	if err != nil {
		msg := err.Error()
		if err == ErrBusy {
			msg = conv.busyReason().Error()
		}
		refuse("30", msg, "engine_busy")
		return
	}
	defer release()

	oaiTickets.mu.Lock()
	oaiPurgeStaleLocked(time.Now())
	switch {
	case len(oaiTickets.m) > 0:
		oaiTickets.mu.Unlock()
		refuse("5", "requêtes /v1 en cours sur le modèle actuel", "requests_in_flight")
		return
	case sideJobs.Load() > 0:
		// Travaux annexes d'après un tour (titre, mémoire, résumé) : ils ne
		// tiennent pas le verrou de génération, une relance les tuerait.
		oaiTickets.mu.Unlock()
		refuse("5", "travaux annexes en cours sur le modèle actuel", "requests_in_flight")
		return
	}
	oaiSwitching.Store(true)
	oaiTickets.mu.Unlock()
	defer func() {
		oaiTickets.mu.Lock()
		oaiSwitching.Store(false)
		oaiTickets.mu.Unlock()
	}()

	fmt.Printf("[oai] bascule vers %s demandée par un client /v1\n", id)
	if err := oaiSwitchApply(abs); err != nil {
		refuse("30", err.Error(), "switch_failed")
		return
	}
	deadline := time.After(oaiSwitchReadyTimeout)
	for !oaiSwitchHealth() {
		select {
		case <-ctx.Done():
			refuse("30", "bascule interrompue — le moteur finit de charger "+id, "engine_not_ready")
			return
		case <-deadline:
			refuse("30", "le modèle "+id+" n'a pas démarré — voir le journal du moteur", "engine_not_ready")
			return
		case <-time.After(oaiSwitchPoll):
		}
	}
	oaiTickets.mu.Lock()
	oaiLastSwitch = time.Now()
	oaiTickets.mu.Unlock()
	sendJSON(w, http.StatusOK, map[string]any{"ok": true})
}
