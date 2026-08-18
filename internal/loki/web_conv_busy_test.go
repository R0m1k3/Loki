package loki

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// La liste des discussions doit dire LAQUELLE travaille : c'est ce qui fait
// tourner l'anneau dans la barre latérale dès le chargement de la page, sans
// attendre que le flux SSE ait rejoué son journal.
func TestConvListRapporteLaGeneration(t *testing.T) {
	withWorkspace(t)

	busy := func() bool {
		rr := httptest.NewRecorder()
		handleConvList(rr, httptest.NewRequest("GET", "/api/conversations", nil))
		if rr.Code != 200 {
			t.Fatalf("code %d : %s", rr.Code, rr.Body.String())
		}
		var out struct {
			Active string `json:"active"`
			Busy   bool   `json:"busy"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Active == "" {
			t.Fatal("aucune discussion active dans la liste")
		}
		return out.Busy
	}

	if busy() {
		t.Fatal("busy=true alors qu'aucune génération ne tourne")
	}
	conv.mu.Lock()
	conv.Generating = true
	conv.mu.Unlock()
	defer func() {
		conv.mu.Lock()
		conv.Generating = false
		conv.mu.Unlock()
	}()
	if !busy() {
		t.Fatal("busy=false alors qu'une génération tourne")
	}
}
