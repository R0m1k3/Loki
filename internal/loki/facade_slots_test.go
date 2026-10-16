package loki

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Le slot d'une requête de la façade : aucun sans la clé (et /props jamais
// lu), le fil de la discussion sur le 0 et le reste sur le 1 quand le second
// slot est en service.
func TestFacadeTurnSlot(t *testing.T) {
	testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	slot := func(body string) (int, float64) {
		w := appelOAIInterne(t, "POST", "/internal/v1/turn/slot", body)
		var out struct {
			Slot float64 `json:"slot"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Slot
	}
	prev := sideSlotProps
	sideSlotProps = func(chatEndpoint) (int, int, bool) { t.Fatal("/props lu sans la clé"); return 0, 0, false }
	for _, b := range []string{`{"conv":"c1","main":true}`, `{"main":false}`} {
		if code, s := slot(b); code != 200 || s != -1 {
			t.Fatalf("sans la clé %s : %d %v", b, code, s)
		}
	}
	sideSlotProps = prev
	if err := SetConfigKey("SIDE_SLOT", "on"); err != nil {
		t.Fatal(err)
	}
	if err := SetConfigKey("CTX", "8192"); err != nil {
		t.Fatal(err)
	}
	stubSideProps(t, 2, 8192)
	if _, s := slot(`{"conv":"c1","main":true}`); s != 0 {
		t.Fatalf("étape du tour : slot %v", s)
	}
	if _, s := slot(`{"main":false}`); s != 1 {
		t.Fatalf("vérification : slot %v", s)
	}
	if _, s := slot(`{"main":true}`); s != 0 {
		t.Fatalf("résumé en continuation : slot %v", s)
	}
	// Second slot hors service : plus d'id_slot.
	stubSideProps(t, 1, 8192)
	if _, s := slot(`{"main":false}`); s != -1 {
		t.Fatalf("un seul slot : %v", s)
	}
	if code, _ := slot(`{`); code != 400 {
		t.Fatalf("corps illisible : %d", code)
	}
	if w := appelOAIInterne(t, "GET", "/internal/v1/turn/slot", ""); w.Code != 405 {
		t.Fatalf("GET : %d", w.Code)
	}
}

// SLOT_PERSIST : l'état gardé à la bascule est rechargé avant la première
// étape du tour de CETTE discussion, une seule fois ; jamais pour une autre
// discussion, une requête annexe ou un résumé.
func TestFacadeTurnSlotRecharge(t *testing.T) {
	m, home := persistSetup(t, true)
	t.Setenv(internalTokenEnv, jetonTest)
	snap := slotPersistPrepare()
	if snap == nil {
		t.Fatal("rien à garder")
	}
	snap.save()
	name := slotPersistName(persistTestKey, "c1")
	for _, b := range []string{
		`{"conv":"autre","main":true}`,
		`{"conv":"c1","main":false}`,
		`{"main":true}`,
		`{"conv":"c1","main":true}`,
		`{"conv":"c1","main":true}`,
	} {
		if w := appelOAIInterne(t, "POST", "/internal/v1/turn/slot", b); w.Code != 200 {
			t.Fatalf("%s : %d", b, w.Code)
		}
	}
	want := []string{"save tmp-" + name, "restore " + name}
	if got := m.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("appels %v, attendu %v", got, want)
	}
	if _, err := os.Stat(filepath.Join(home, "slots", name)); !os.IsNotExist(err) {
		t.Fatal("fichier gardé après la tentative")
	}
}

// Une étape de tour de la façade acceptée par le moteur marque le slot :
// c'est ce que la sauvegarde SLOT_PERSIST relit à la bascule.
func TestFacadeTurnSlotSauvegarde(t *testing.T) {
	m, home := persistSetup(t, true)
	t.Setenv(internalTokenEnv, jetonTest)
	engineGate.mu.Lock()
	engineGate.main = engineMainStamp{}
	engineGate.mu.Unlock()
	if slotPersistPrepare() != nil {
		t.Fatal("slot sans discussion : rien à garder")
	}
	w := appelOAIInterne(t, "POST", "/internal/v1/oai/begin", `{"main_conv":"c2"}`)
	var b struct {
		Ticket uint64 `json:"ticket"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || b.Ticket == 0 {
		t.Fatalf("begin : %s", w.Body.String())
	}
	tk := `{"ticket":` + jsonNum(b.Ticket) + `}`
	appelOAIInterne(t, "POST", "/internal/v1/oai/served", tk)
	appelOAIInterne(t, "POST", "/internal/v1/oai/end", tk)
	snap := slotPersistPrepare()
	if snap == nil {
		t.Fatal("étape de la façade non vue par la sauvegarde")
	}
	snap.save()
	name := slotPersistName(persistTestKey, "c2")
	if _, err := os.Stat(filepath.Join(home, "slots", name)); err != nil {
		t.Fatalf("état non gardé : %v (%v)", err, m.got())
	}
}

func jsonNum(n uint64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
