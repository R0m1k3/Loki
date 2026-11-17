package loki

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Bascule de modèle demandée par un client /v1 (handleInternalOAISwitch).

type oaiSwitchReply struct {
	OK     bool           `json:"ok"`
	Error  string         `json:"error"`
	Refuse map[string]any `json:"refuse"`
}

// oaiSwitchSetup : un LOKI_HOME de test, un preset local et un externe, des
// crochets neutres (aucun moteur ne tourne) et les globaux remis à zéro.
func oaiSwitchSetup(t *testing.T) (local, external string, applied *[]string) {
	t.Helper()
	home := testHome(t)
	t.Setenv(internalTokenEnv, jetonTest)
	dir := filepath.Join(home, "presets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	local = filepath.Join(dir, "grand.env")
	external = filepath.Join(dir, "api.env")
	if err := os.WriteFile(local, []byte("MODEL=m.gguf\nCTX=65536\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(external, []byte("EXTERNAL=1\nEXT_URL=https://x\nCTX=131072\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls []string
	saved := struct {
		apply   func(string) error
		health  func() bool
		timeout time.Duration
		poll    time.Duration
	}{oaiSwitchApply, oaiSwitchHealth, oaiSwitchReadyTimeout, oaiSwitchPoll}
	oaiSwitchApply = func(abs string) error { calls = append(calls, filepath.Base(abs)); return nil }
	oaiSwitchHealth = func() bool { return true }
	oaiSwitchReadyTimeout = time.Second
	oaiSwitchPoll = 5 * time.Millisecond
	reset := func() {
		oaiTickets.mu.Lock()
		oaiSwitching.Store(false)
		oaiLastSwitch = time.Time{}
		for id, tk := range oaiTickets.m {
			tk.end()
			delete(oaiTickets.m, id)
		}
		oaiTickets.mu.Unlock()
		conv.mu.Lock()
		conv.Generating, conv.benching, conv.leaseWhy, conv.cancel = false, false, nil, nil
		conv.mu.Unlock()
	}
	reset()
	t.Cleanup(func() {
		oaiSwitchApply, oaiSwitchHealth = saved.apply, saved.health
		oaiSwitchReadyTimeout, oaiSwitchPoll = saved.timeout, saved.poll
		reset()
	})
	return local, external, &calls
}

func oaiSwitchCall(t *testing.T, path string) (int, oaiSwitchReply) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"path": path})
	w := appelOAIInterne(t, "POST", "/internal/v1/oai/switch", string(body))
	var got oaiSwitchReply
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("réponse illisible : %d %s", w.Code, w.Body.String())
	}
	return w.Code, got
}

func refusCode(r oaiSwitchReply) string {
	if r.Refuse == nil {
		return ""
	}
	s, _ := r.Refuse["code"].(string)
	return s
}

func generating() bool {
	conv.mu.Lock()
	defer conv.mu.Unlock()
	return conv.Generating
}

func TestOAISwitchSucces(t *testing.T) {
	local, _, calls := oaiSwitchSetup(t)
	code, got := oaiSwitchCall(t, local)
	if code != 200 || !got.OK {
		t.Fatalf("bascule : %d %+v", code, got)
	}
	if len(*calls) != 1 || (*calls)[0] != "grand.env" {
		t.Fatalf("apply : %v", *calls)
	}
	if oaiSwitching.Load() || generating() {
		t.Fatal("drapeau ou verrou non rendus après la bascule")
	}
	// Seconde bascule dans la minute : refusée, avec le temps restant.
	_, got = oaiSwitchCall(t, local)
	if refusCode(got) != "switch_cooldown" {
		t.Fatalf("attendu switch_cooldown : %+v", got)
	}
	ra, _ := strconv.Atoi(got.Refuse["retry_after"].(string))
	if ra < 1 || ra > 60 {
		t.Fatalf("retry_after hors bornes : %v", got.Refuse["retry_after"])
	}
	if len(*calls) != 1 {
		t.Fatal("la seconde bascule a été appliquée")
	}
}

func TestOAISwitchTicketEnVol(t *testing.T) {
	local, _, calls := oaiSwitchSetup(t)
	w := appelOAIInterne(t, "POST", "/internal/v1/oai/begin", `{"post_v1":true}`)
	var b struct {
		Ticket uint64 `json:"ticket"`
	}
	if json.Unmarshal(w.Body.Bytes(), &b) != nil || b.Ticket == 0 {
		t.Fatalf("begin : %s", w.Body.String())
	}
	_, got := oaiSwitchCall(t, local)
	if refusCode(got) != "requests_in_flight" || got.Refuse["retry_after"] != "5" {
		t.Fatalf("attendu requests_in_flight : %+v", got)
	}
	if oaiSwitching.Load() || generating() || len(*calls) != 0 {
		t.Fatal("refus avec effets de bord")
	}
}

func TestOAISwitchTravauxAnnexes(t *testing.T) {
	local, _, calls := oaiSwitchSetup(t)
	sideJobs.Add(1)
	defer sideJobs.Add(-1)
	_, got := oaiSwitchCall(t, local)
	if refusCode(got) != "requests_in_flight" || len(*calls) != 0 || generating() {
		t.Fatalf("attendu requests_in_flight sans effet : %+v", got)
	}
}

func TestOAISwitchBeginRefuse(t *testing.T) {
	oaiSwitchSetup(t)
	oaiTickets.mu.Lock()
	oaiSwitching.Store(true)
	oaiTickets.mu.Unlock()
	avant := enVol()
	w := appelOAIInterne(t, "POST", "/internal/v1/oai/begin", `{"post_v1":true}`)
	var got oaiSwitchReply
	if json.Unmarshal(w.Body.Bytes(), &got) != nil || refusCode(got) != "model_switching" {
		t.Fatalf("attendu model_switching : %s", w.Body.String())
	}
	if enVol() != avant || oaiInflight() != 0 {
		t.Fatalf("refus compté en vol : %d (avant %d)", enVol(), avant)
	}
	// Une étape de tour de la façade (main_conv) n'est pas refusée.
	w = appelOAIInterne(t, "POST", "/internal/v1/oai/begin", `{"main_conv":"c1"}`)
	var tk struct {
		Ticket uint64 `json:"ticket"`
	}
	if json.Unmarshal(w.Body.Bytes(), &tk) != nil || tk.Ticket == 0 {
		t.Fatalf("étape de tour refusée : %s", w.Body.String())
	}
	body, _ := json.Marshal(map[string]any{"ticket": tk.Ticket})
	appelOAIInterne(t, "POST", "/internal/v1/oai/end", string(body))
}

func TestOAISwitchCheminEtExterne(t *testing.T) {
	_, external, calls := oaiSwitchSetup(t)
	code, got := oaiSwitchCall(t, filepath.Join(t.TempDir(), "x.env"))
	if code != 400 || got.Error == "" {
		t.Fatalf("chemin hors dossier : %d %+v", code, got)
	}
	before := ReadConfig()
	_, got = oaiSwitchCall(t, external)
	if refusCode(got) != "external_preset" || got.Refuse["status"] != float64(400) {
		t.Fatalf("attendu external_preset (400) : %+v", got)
	}
	if len(*calls) != 0 || len(ReadConfig()) != len(before) {
		t.Fatal("preset externe appliqué")
	}
}

func TestOAISwitchMoteurOccupe(t *testing.T) {
	local, _, calls := oaiSwitchSetup(t)
	conv.mu.Lock()
	conv.Generating = true
	conv.mu.Unlock()
	_, got := oaiSwitchCall(t, local)
	if refusCode(got) != "engine_busy" || len(*calls) != 0 {
		t.Fatalf("attendu engine_busy : %+v", got)
	}
}

func TestOAISwitchMoteurNeDemarrePas(t *testing.T) {
	local, _, _ := oaiSwitchSetup(t)
	oaiSwitchHealth = func() bool { return false }
	oaiSwitchReadyTimeout = 50 * time.Millisecond
	_, got := oaiSwitchCall(t, local)
	if refusCode(got) != "engine_not_ready" {
		t.Fatalf("attendu engine_not_ready : %+v", got)
	}
	oaiTickets.mu.Lock()
	last := oaiLastSwitch
	oaiTickets.mu.Unlock()
	if oaiSwitching.Load() || generating() || !last.IsZero() {
		t.Fatal("état non rendu après un échec (ou délai armé)")
	}
}

// Pendant la bascule : le motif du verrou, et les sites du verrou de génération
// refusent même après un Reset (verrou rendu, drapeau levé).
func TestOAISwitchGardes(t *testing.T) {
	local, _, _ := oaiSwitchSetup(t)
	inApply := make(chan struct{})
	finish := make(chan struct{})
	oaiSwitchApply = func(string) error { close(inApply); <-finish; return nil }
	done := make(chan oaiSwitchReply)
	go func() { _, r := oaiSwitchCall(t, local); done <- r }()
	<-inApply
	if err := conv.busyReason(); !errors.Is(err, errOAISwitchBusy) {
		t.Errorf("busyReason pendant la bascule : %v", err)
	}
	// Reset simulé : le verrou est rendu, le drapeau reste levé.
	conv.mu.Lock()
	conv.Generating, conv.benching, conv.leaseWhy = false, false, nil
	conv.mu.Unlock()
	checks := map[string]error{
		"StartTurn":    conv.StartTurn("x", nil, Caps{}, 0),
		"CompactNow":   conv.CompactNow(),
		"ensurePreset": ensureTaskPreset("grand"),
	}
	_, err := conv.RunAutonomous(t.Context(), "t", "t", "p", "", Caps{}, 0)
	checks["RunAutonomous"] = err
	_, err = conv.extLeaseAcquire(time.Second, nil)
	checks["extLeaseAcquire"] = err
	_, err = conv.measureLease(func() {}, nil)
	checks["measureLease"] = err
	_, _, err = benchStart(benchOpts{})
	checks["benchStart"] = err
	for name, err := range checks {
		if !errors.Is(err, errOAISwitchBusy) {
			t.Errorf("%s : %v, attendu errOAISwitchBusy", name, err)
		}
	}
	// Une tâche tombée pendant la bascule n'est pas marquée en échec.
	tk := Task{ID: "pendant", Name: "n", Prompt: "p", Schedule: "@every 1h", Enabled: true, Preset: "grand"}
	if err := saveTask(tk); err != nil {
		t.Fatal(err)
	}
	runTask(tk)
	if got, _ := getTask("pendant"); got.LastError != "" {
		t.Errorf("tâche marquée en échec pendant une bascule : %q", got.LastError)
	}
	close(finish)
	if r := <-done; !r.OK {
		t.Fatalf("bascule : %+v", r)
	}
	if oaiSwitching.Load() {
		t.Fatal("drapeau resté levé")
	}
}
