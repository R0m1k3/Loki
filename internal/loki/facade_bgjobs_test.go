package loki

import (
	"strings"
	"testing"
)

// Lot 43 : les jobs d'arrière-plan des tours de la façade comptent pour
// l'optimiseur du Go ; façade en panne, seuls les siens.
func TestFacadeJobsArrierePlan(t *testing.T) {
	testHome(t)
	jobsMu.Lock()
	saved := jobs
	jobs = map[string]*bgJob{"job1": {ID: "job1"}}
	jobsMu.Unlock()
	t.Cleanup(func() {
		jobsMu.Lock()
		jobs = saved
		jobsMu.Unlock()
	})
	f := demarrerFacade(t)
	f.internalStatus = 200
	f.internalBody = `{"running":2}`
	if n := bgJobsRunning(); n != 3 {
		t.Fatalf("jobs : %d, attendu 3 (1 ici, 2 à la façade)", n)
	}
	vu := f.recu()
	if len(vu) != 1 || !strings.HasPrefix(vu[0], "POST /internal/facade/v1/bgjobs jeton ") {
		t.Fatalf("appel : %v", vu)
	}
	f.internalStatus = 500
	if n := bgJobsRunning(); n != 1 {
		t.Fatalf("façade en panne : %d", n)
	}
}
