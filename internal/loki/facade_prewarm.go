package loki

import (
	"net/http"
	"sync"
)

// facade_prewarm.go — PREWARM quand la façade tient la conversation (lot 33 de
// la migration). La façade assemble la requête du prochain tour (elle tient
// l'historique) et l'envoie ; le Go garde ce qui décide si elle peut partir
// et ce qui l'annule :
//
//   - /internal/v1/prewarm/check : le moteur est-il libre d'un vrai travail
//     (travail annexe, bench, optimiseur), loin de minuit, en bonne santé, et
//     sur quel slot viser (un seul slot, ou celui de la discussion avec
//     SIDE_SLOT) ;
//   - /internal/v1/prewarm/begin : recharge l'état SLOT_PERSIST de la
//     discussion, puis inscrit le préchauffage comme LA requête en vol
//     (prewarmBegin). Toute requête de Loki qui part ensuite l'annule — sauf
//     un tour de la façade qui le prolonge (/internal/v1/oai/begin,
//     prewarm_keep) : l'annulation est relayée à la façade, qui coupe sa
//     connexion au moteur ;
//   - /internal/v1/prewarm/end : la fin, avec ce que le moteur a repris du
//     cache (noteEnginePrompt) ;
//   - après une tâche planifiée, le déclencheur part à la façade
//     (prewarmKick).

// Préchauffages de la façade inscrits ici : id → fin.
var facadePrewarms = struct {
	mu   sync.Mutex
	next uint64
	m    map[uint64]func()
}{m: map[uint64]func(){}}

// handleInternalPrewarmCheck : POST {} → {"ok":true,"slot":-1|0} ou
// {"ok":false,"why":"…"}.
func handleInternalPrewarmCheck(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	if !readInternalJSON(w, r, &in) {
		return
	}
	refuse := func(why string) {
		sendJSON(w, http.StatusOK, map[string]any{"ok": false, "why": why})
	}
	if prewarmMode(ReadConfig()) == "" {
		refuse("clé absente")
		return
	}
	ep := resolveChatEndpoint()
	if ep.External {
		refuse("preset externe")
		return
	}
	if sideJobs.Load() > 0 || benchRunning() || tuneGuard() != nil {
		refuse("moteur occupé")
		return
	}
	if prewarmNearMidnight(prewarmNow()) {
		refuse("minuit proche")
		return
	}
	if !healthCheck() {
		refuse("moteur indisponible")
		return
	}
	// Un seul slot, ou les deux de SIDE_SLOT : le préchauffage vise alors le
	// slot de la discussion (id_slot 0), celui où partira le vrai tour.
	slot := -1
	if n := prewarmSlots(ep); n != 1 {
		if slot = engineSlotFor(ep, true); n != 2 || slot != 0 {
			refuse("plusieurs slots")
			return
		}
	}
	sendJSON(w, http.StatusOK, map[string]any{"ok": true, "slot": slot})
}

// handleInternalPrewarmBegin : POST {"conv","n","prefix","opts"} →
// {"ok":true,"id":n} ou {"ok":false} (une requête de Loki est en vol, ou un
// autre préchauffage). n : messages du préfixe préchauffé (sans la
// sentinelle) ; prefix : son empreinte cumulée (perfPrefix) ; opts : celle
// des champs qui façonnent le rendu.
func handleInternalPrewarmBegin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Conv   string `json:"conv"`
		N      int    `json:"n"`
		Prefix uint64 `json:"prefix"`
		Opts   uint64 `json:"opts"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	// SLOT_PERSIST : l'état gardé de la discussion d'abord — le préchauffage
	// le prolonge au lieu de tout recalculer.
	slotPersistRestore(resolveChatEndpoint(), in.Conv)
	facadePrewarms.mu.Lock()
	facadePrewarms.next++
	id := facadePrewarms.next
	facadePrewarms.mu.Unlock()
	run := &prewarmRun{n: in.N, prefix: in.Prefix, opts: in.Opts, done: make(chan struct{})}
	// Annulé par une autre requête : la façade coupe sa connexion.
	run.cancel = func() {
		go func() { _ = facadePost("/internal/facade/v1/chat/prewarm/cancel", map[string]any{"id": id}) }()
	}
	end, ok := prewarmBegin(run)
	if !ok {
		sendJSON(w, http.StatusOK, map[string]any{"ok": false})
		return
	}
	facadePrewarms.mu.Lock()
	facadePrewarms.m[id] = func() {
		end()
		close(run.done)
	}
	facadePrewarms.mu.Unlock()
	sendJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// handleInternalPrewarmEnd : POST {"id","total","cached"} → 204. total : la
// taille du prompt préchauffé (0 : inconnue) ; cached : ce que le moteur a
// repris de son cache (absent : inconnu).
func handleInternalPrewarmEnd(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID     uint64 `json:"id"`
		Total  int    `json:"total"`
		Cached *int   `json:"cached"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	facadePrewarms.mu.Lock()
	end := facadePrewarms.m[in.ID]
	delete(facadePrewarms.m, in.ID)
	facadePrewarms.mu.Unlock()
	if end == nil {
		sendJSON(w, http.StatusNotFound, map[string]any{"error": "préchauffage inconnu"})
		return
	}
	end()
	// Après une tâche, c'est lui qui retrouve (ou non) la conversation dans le
	// cache RAM : la vérification de llm_slots.go se fait ici.
	if in.Total > 0 {
		cached := 0
		if in.Cached != nil {
			cached = *in.Cached
		}
		noteEnginePrompt(in.Total, cached, in.Cached != nil)
	}
	w.WriteHeader(http.StatusNoContent)
}
