package loki

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

// API interne du bail du moteur (lot 6 de la migration Rust).
//
// Le verrou de génération (conv.Generating) est partagé : tour de chat, tâche
// planifiée (RunAutonomous), bench et optimiseur (measureLease), et quatre
// sondes isGenerating (optimiseur, VRAM, projets, liste des discussions). Une
// conversation tenue par la façade Rust doit prendre CE verrou-là, sans quoi
// une tâche planifiée partirait en plein tour vers le même moteur.
//
//	POST /internal/v1/engine/lease          {"ttl_ms":N}  → 200 {"lease":id,"ttl_ms":N}
//	                                                        409 {"error":…,"task_id":…,"task_name":…}
//	POST /internal/v1/engine/lease/renew    {"lease":id}  → 200 {"stop":bool} · 410 bail perdu
//	POST /internal/v1/engine/lease/release  {"lease":id}  → 204 (idempotent)
//	POST /internal/v1/engine/stop                         → 204 : arrête le détenteur Go
//	                                                        (tâche, bench, optimiseur)
//
// Le bail expire s'il n'est pas renouvelé : une façade tuée en plein tour ne
// laisse pas le moteur réservé pour toujours. Reset et les bascules de
// discussion côté Go le rompent (renew → 410), comme ils libèrent un tour Go.
// « stop » : le bouton stop est passé par le Go (conv.Stop) — la façade arrête
// son tour.

const (
	extLeaseDefaultTTL = 30 * time.Second
	extLeaseMinTTL     = 5 * time.Second
	extLeaseMaxTTL     = 5 * time.Minute
	internalLeaseMax   = 4 << 10
)

// extLeaseState : le bail tenu par la façade. Protégé par conv.mu.
type extLeaseState struct {
	id    string
	stop  bool
	ttl   time.Duration
	timer *time.Timer
	// bench : le bail d'un benchmark de la façade (LOKI_BENCH=rust) — les
	// autres reçoivent errBenchBusy, et changer de discussion ne le rompt pas.
	bench bool
}

// extLeaseClearLocked oublie le bail de la façade. c.mu détenu.
// benchLeaseHeld : un bail « bench » de la façade est tenu. Lu sans le verrou
// de la conversation (benchRunning est appelé par des chemins qui le tiennent
// déjà, le préchauffage par exemple).
var benchLeaseHeld atomic.Bool

func (c *Conversation) extLeaseClearLocked() {
	if c.extLease != nil {
		if c.extLease.bench {
			benchLeaseHeld.Store(false)
		}
		c.extLease.timer.Stop()
		c.extLease = nil
	}
}

// extLeaseAcquire prend le verrou de génération pour la façade.
func (c *Conversation) extLeaseAcquire(ttl time.Duration, bench bool) (string, error) {
	if err := tuneGuard(); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Generating {
		return "", ErrBusy
	}
	b, err := randBytes(16)
	if err != nil {
		return "", err
	}
	st := &extLeaseState{id: fmt.Sprintf("%x", b), ttl: ttl, bench: bench}
	st.timer = time.AfterFunc(ttl, func() { c.extLeaseExpire(st) })
	c.Generating = true
	if bench {
		c.benching, c.leaseWhy = true, errBenchBusy
		benchLeaseHeld.Store(true)
	}
	c.cancel = func() {
		c.mu.Lock()
		st.stop = true
		c.mu.Unlock()
	}
	c.extLease = st
	return st.id, nil
}

// extLeaseExpire rend le verrou d'un bail que la façade n'a pas renouvelé.
func (c *Conversation) extLeaseExpire(st *extLeaseState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.extLease != st {
		return
	}
	c.extLease = nil
	c.Generating, c.cancel = false, nil
	if st.bench {
		c.benching, c.leaseWhy = false, nil
		benchLeaseHeld.Store(false)
	}
	fmt.Fprintf(os.Stderr, "[lease] bail de la façade expiré sans renouvellement (%s) : verrou du moteur rendu\n", st.ttl)
}

// extLeaseRenew prolonge le bail ; ok=false s'il n'est plus le bail courant.
func (c *Conversation) extLeaseRenew(id string) (stop, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.extLease
	if st == nil || st.id != id {
		return false, false
	}
	st.timer.Reset(st.ttl)
	return st.stop, true
}

// extLeaseRelease rend le verrou, si ce bail le tient encore.
func (c *Conversation) extLeaseRelease(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.extLease == nil || c.extLease.id != id {
		return
	}
	if c.extLease.bench {
		c.benching, c.leaseWhy = false, nil
	}
	c.extLeaseClearLocked()
	c.Generating, c.cancel = false, nil
}

// benchLeased : un benchmark de la façade tient-il le verrou ?
func benchLeased() bool { return benchLeaseHeld.Load() }

func readLeaseBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST attendu"})
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, internalLeaseMax+1))
	if err != nil || len(raw) > internalLeaseMax {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "corps illisible"})
		return false
	}
	if len(raw) > 0 && json.Unmarshal(raw, v) != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": "JSON illisible"})
		return false
	}
	return true
}

func handleInternalLease(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TTLms int64  `json:"ttl_ms"`
		Why   string `json:"why"` // "bench" : le bail d'un benchmark
	}
	if !readLeaseBody(w, r, &in) {
		return
	}
	ttl := extLeaseDefaultTTL
	if in.TTLms > 0 {
		ttl = min(max(time.Duration(in.TTLms)*time.Millisecond, extLeaseMinTTL), extLeaseMaxTTL)
	}
	id, err := conv.extLeaseAcquire(ttl, in.Why == "bench")
	if err != nil {
		if err == ErrBusy {
			err = conv.busyReason() // dit QUI occupe le moteur
		}
		out := map[string]any{"error": err.Error()}
		conv.mu.Lock()
		if conv.runningTaskID != "" {
			out["task_id"], out["task_name"] = conv.runningTaskID, conv.runningTaskName
		}
		conv.mu.Unlock()
		sendJSON(w, http.StatusConflict, out)
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{"lease": id, "ttl_ms": ttl.Milliseconds()})
}

func handleInternalLeaseRenew(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Lease string `json:"lease"`
	}
	if !readLeaseBody(w, r, &in) {
		return
	}
	stop, ok := conv.extLeaseRenew(in.Lease)
	if !ok {
		sendJSON(w, http.StatusGone, map[string]any{"error": "bail perdu"})
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{"stop": stop})
}

func handleInternalLeaseRelease(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Lease string `json:"lease"`
	}
	if !readLeaseBody(w, r, &in) {
		return
	}
	conv.extLeaseRelease(in.Lease)
	w.WriteHeader(http.StatusNoContent)
}

func handleInternalEngineStop(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	if !readLeaseBody(w, r, &in) {
		return
	}
	conv.Stop()
	w.WriteHeader(http.StatusNoContent)
}
