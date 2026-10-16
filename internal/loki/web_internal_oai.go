package loki

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
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
			sendJSON(w, http.StatusOK, map[string]any{"refuse": map[string]any{
				"status": http.StatusServiceUnavailable, "message": msg,
				"type": "server_error", "code": code, "retry_after": retry,
			}})
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
	for id, t := range oaiTickets.m {
		if now.Sub(t.when) > oaiTicketMaxAge {
			t.end()
			delete(oaiTickets.m, id)
		}
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
