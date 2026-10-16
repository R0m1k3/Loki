package loki

import (
	"net/http"
	"os"
	"strings"
	"sync"
)

// facade_bench.go — le banc d'essai tenu par la façade Rust (LOKI_BENCH=rust,
// lot 27 de la migration ; expérimental, le Go reste le défaut) : les routes
// /api/bench*, le job et le protocole de mesure (rust/loki-rs/src/bench). Le
// verrou de génération reste celui de la conversation du Go : la façade le
// prend par le bail du moteur, avec le motif « bench » (benching, et le refus
// errBenchBusy pour les autres). Le Go lui prête aussi l'isolation des travaux
// annexes (effacement du slot à la fin) et la télémétrie (perf_log.go).

const facadeBenchEnv = "LOKI_BENCH"

// facadeBench : la façade tient-elle le banc d'essai ?
func facadeBench() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeBenchEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// facadeBenchRoutes : les routes que la façade sert alors.
var facadeBenchRoutes = map[string]bool{
	"/api/bench":        true,
	"/api/bench/status": true,
	"/api/bench/cancel": true,
	"/api/bench/last":   true,
}

// facadeSideJobs : les travaux annexes ouverts par la façade (id → fin).
var facadeSideJobs = struct {
	mu  sync.Mutex
	seq int
	m   map[int]func()
}{m: map[int]func(){}}

// handleInternalSideJob : POST {} ouvre un travail annexe (engineSideJob) et
// rend son id ; POST {"end": id, "served": bool} le ferme — served : le
// moteur a servi au moins une requête du travail (engineServed), le slot
// porte donc son état et peut être effacé.
func handleInternalSideJob(w http.ResponseWriter, r *http.Request) {
	var in struct {
		End    int  `json:"end"`
		Served bool `json:"served"`
	}
	if !readLeaseBody(w, r, &in) {
		return
	}
	if in.End == 0 {
		done := engineSideJob()
		facadeSideJobs.mu.Lock()
		facadeSideJobs.seq++
		id := facadeSideJobs.seq
		facadeSideJobs.m[id] = done
		facadeSideJobs.mu.Unlock()
		sendJSON(w, http.StatusOK, map[string]any{"id": id})
		return
	}
	facadeSideJobs.mu.Lock()
	done := facadeSideJobs.m[in.End]
	delete(facadeSideJobs.m, in.End)
	facadeSideJobs.mu.Unlock()
	if done == nil {
		sendJSON(w, http.StatusNotFound, map[string]any{"error": "travail annexe inconnu"})
		return
	}
	if in.Served {
		engineServed()
	}
	done()
	w.WriteHeader(http.StatusNoContent)
}

// handleInternalBenchPerf : la trace d'un bench de la façade dans la
// télémétrie (le bench a pris le slot : la perte de cache du tour suivant lui
// revient).
func handleInternalBenchPerf(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PromptN         int     `json:"prompt_n"`
		PromptMs        float64 `json:"prompt_ms"`
		PromptPerSecond float64 `json:"prompt_per_second"`
		PredictedN      int     `json:"predicted_n"`
		PredictedPerSec float64 `json:"predicted_per_second"`
	}
	if !readLeaseBody(w, r, &in) {
		return
	}
	perfRecord(perfRec{Kind: perfBench, Complete: true, Total: in.PromptN, New: in.PromptN,
		PPms: in.PromptMs, PPtps: in.PromptPerSecond, Gen: in.PredictedN, TGtps: in.PredictedPerSec}, nil)
	w.WriteHeader(http.StatusNoContent)
}
