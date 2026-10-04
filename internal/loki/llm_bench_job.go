package loki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Le benchmark tourne en TÂCHE DE FOND. Le mode complet dure de une à cinq
// minutes : une requête HTTP synchrone était coupée bien avant par un reverse
// proxy (nginx d'Unraid à 60 s, Cloudflare à 100 s) ou par le délai de
// l'interface (30 s), pendant que le moteur, lui, continuait — et un second
// clic empilait un deuxième bench derrière le premier. Désormais :
//
//	POST /api/bench          lance (202 + id), 409 si le moteur est occupé
//	GET  /api/bench/status   phase en cours, puis résultat ou erreur
//	POST /api/bench/cancel   annule (chaque requête au moteur porte le contexte)
//	GET  /api/bench/last     dernier résultat enregistré
//
// Pendant toute la mesure, le bench tient le verrou de génération de la
// conversation : un message, une tâche planifiée ou une compaction reçoivent
// un refus clair au lieu d'attendre des minutes dans la file du moteur. Les
// clients /v1 reçoivent un 503 avec Retry-After (llm_oai.go). Restent hors du
// verrou les appels annexes déclenchés par la fin d'un tour (titre, mémoire) :
// le verrou les précède de toute façon.

var errBenchBusy = errors.New("un benchmark occupe le moteur — attends sa fin ou annule-le")

// benchLease prend le verrou de génération pour un benchmark. cancel est
// branché sur le bouton stop du chat, comme pour une tâche planifiée. La
// fonction rendue le libère ; si un Reset l'a déjà rendu entre-temps (epoch
// changé), elle ne touche pas au tour qui a pu démarrer depuis.
func (c *Conversation) benchLease(cancel context.CancelFunc) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Generating {
		return nil, ErrBusy
	}
	c.Generating, c.benching, c.cancel = true, true, cancel
	epoch := c.epoch
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.benching = false
			if c.epoch == epoch {
				c.Generating = false
				c.cancel = nil
			}
			c.mu.Unlock()
		})
	}, nil
}

// benchJob : le benchmark en cours ou le dernier terminé (un seul à la fois).
var benchJob struct {
	mu       sync.Mutex
	seq      int
	running  bool
	id       string
	mode     string
	phase    string
	step     int
	steps    int
	started  time.Time
	finished time.Time
	cancel   context.CancelFunc
	result   *benchResult
	err      string
	canceled bool
}

// benchRunning : un benchmark de ce processus est-il en cours ?
func benchRunning() bool {
	benchJob.mu.Lock()
	defer benchJob.mu.Unlock()
	return benchJob.running
}

// benchRunner est runBench, remplaçable dans les tests.
var benchRunner = runBench

// benchStart lance un benchmark en arrière-plan. code : le statut HTTP à
// renvoyer en cas de refus.
func benchStart(opts benchOpts) (id string, code int, err error) {
	// Hors verrou : healthCheck peut attendre le moteur quelques secondes, le
	// suivi de progression ne doit pas rester bloqué derrière.
	if externalActive() {
		return "", http.StatusBadRequest, errors.New("benchmark indisponible : le preset actif est une API externe")
	}
	if !healthCheck() {
		return "", http.StatusServiceUnavailable, errModelLoading
	}
	benchJob.mu.Lock()
	defer benchJob.mu.Unlock()
	if benchJob.running {
		return "", http.StatusConflict, errors.New("un benchmark tourne déjà")
	}
	ctx, cancel := context.WithCancel(context.Background())
	release, err := conv.benchLease(cancel)
	if err != nil {
		cancel()
		return "", http.StatusConflict, conv.busyReason()
	}
	benchJob.seq++
	id = fmt.Sprintf("b%d-%d", time.Now().Unix(), benchJob.seq)
	benchJob.running, benchJob.id, benchJob.mode = true, id, opts.Mode
	benchJob.phase, benchJob.step, benchJob.steps = "démarrage", 0, 0
	benchJob.started, benchJob.finished = time.Now(), time.Time{}
	benchJob.cancel, benchJob.result, benchJob.err, benchJob.canceled = cancel, nil, "", false
	go func() {
		defer cancel()
		defer release()
		res, err := benchRunner(ctx, opts, func(phase string, step, steps int) {
			benchJob.mu.Lock()
			if benchJob.id == id {
				benchJob.phase, benchJob.step, benchJob.steps = phase, step, steps
			}
			benchJob.mu.Unlock()
		})
		benchJob.mu.Lock()
		defer benchJob.mu.Unlock()
		if benchJob.id != id {
			return
		}
		benchJob.running, benchJob.finished, benchJob.cancel = false, time.Now(), nil
		benchJob.result = res
		if err != nil {
			benchJob.canceled = ctx.Err() != nil
			benchJob.err = err.Error()
			if benchJob.canceled {
				benchJob.err = "benchmark annulé"
			}
		}
	}()
	return id, http.StatusAccepted, nil
}

// benchCancel annule le benchmark en cours. false : aucun ne tournait.
func benchCancel() bool {
	benchJob.mu.Lock()
	defer benchJob.mu.Unlock()
	if !benchJob.running || benchJob.cancel == nil {
		return false
	}
	benchJob.cancel()
	return true
}

// benchStatus : l'état à renvoyer à l'interface.
func benchStatus() map[string]any {
	benchJob.mu.Lock()
	defer benchJob.mu.Unlock()
	st := map[string]any{"ok": true, "running": benchJob.running, "id": benchJob.id, "mode": benchJob.mode}
	if benchJob.id == "" {
		return st
	}
	st["phase"], st["step"], st["steps"] = benchJob.phase, benchJob.step, benchJob.steps
	end := benchJob.finished
	if benchJob.running {
		end = time.Now()
	}
	st["elapsed_sec"] = end.Sub(benchJob.started).Seconds()
	if !benchJob.running {
		if benchJob.result != nil {
			st["result"] = benchJob.result
			st["saved"] = benchSavable(benchJob.result)
		}
		if benchJob.err != "" {
			st["error"] = benchJob.err
			st["canceled"] = benchJob.canceled
		}
	}
	return st
}

// handleBench lance un benchmark : POST seulement — il occupe le moteur des
// minutes durant, une balise <img> sur une page tierce ne doit pas y suffire.
// Corps : {"mode":"quick"|"full", "prompt":N, "n":N}, tous facultatifs.
func handleBench(w http.ResponseWriter, r *http.Request) {
	if !postOnly(w, r) {
		return
	}
	var body struct {
		Mode   string `json:"mode"`
		Prompt int    `json:"prompt"`
		N      int    `json:"n"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	opts := benchOpts{Mode: benchModeQuick, Prompt: 2000, Predict: 300}
	if strings.EqualFold(strings.TrimSpace(body.Mode), benchModeFull) {
		opts.Mode = benchModeFull
	}
	if body.Prompt > 0 {
		opts.Prompt = min(body.Prompt, 32768)
	}
	if body.N > 0 {
		opts.Predict = min(body.N, 4096)
	}
	id, code, err := benchStart(opts)
	if err != nil {
		sendJSON(w, code, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sendJSON(w, code, map[string]any{"ok": true, "id": id})
}

// handleBenchStatus : progression du benchmark en cours, ou issue du dernier.
func handleBenchStatus(w http.ResponseWriter, r *http.Request) {
	sendJSON(w, 200, benchStatus())
}

// handleBenchCancel annule le benchmark en cours.
func handleBenchCancel(w http.ResponseWriter, r *http.Request) {
	if !postOnly(w, r) {
		return
	}
	sendJSON(w, 200, map[string]any{"ok": benchCancel()})
}

// handleBenchLast returns the most recent persisted benchmark, or {ok:false}
// when none has been run yet.
func handleBenchLast(w http.ResponseWriter, r *http.Request) {
	sb := loadLastBench()
	if sb == nil {
		sendJSON(w, 200, map[string]any{"ok": false})
		return
	}
	sendJSON(w, 200, map[string]any{"ok": true, "result": sb.Result, "model": sb.Model, "at": sb.At})
}
