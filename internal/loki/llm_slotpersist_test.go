package loki

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// persistSI : moteur qui connaît --slot-save-path, dossier prêt, boucle locale.
func persistSI() serveSysInfo {
	si := denseGPU()
	si.SlotDir = "/data/slots"
	return si
}

func TestSlotPersistPlan(t *testing.T) {
	loop := map[string]string{"SLOT_PERSIST": "on", "HOST": "127.0.0.1"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range loop {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	cases := []struct {
		name  string
		cfg   map[string]string
		extra []string
		si    func(*serveSysInfo)
		ok    bool
		why   string
	}{
		{name: "clé absente", cfg: map[string]string{"HOST": "127.0.0.1"}},
		{name: "boucle locale", cfg: loop, ok: true},
		{name: "0.0.0.0 avec clé d'API", cfg: with("HOST", "0.0.0.0"), si: func(s *serveSysInfo) { s.APIKey = "k" }, ok: true},
		{name: "0.0.0.0 sans clé", cfg: with("HOST", ""), why: "sans clé"},
		{name: "SPEC=auto", cfg: with("SPEC", "auto"), why: "spéculatif"},
		{name: "brouillon dans EXTRA_ARGS", cfg: loop, extra: []string{"-md", "d.gguf"}, why: "spéculatif"},
		{name: "--slot-save-path à la main", cfg: loop, extra: []string{"--slot-save-path", "/x"}, why: "à la main"},
		{name: "variable LLAMA_ARG_SLOT_SAVE_PATH", cfg: loop, si: func(s *serveSysInfo) { s.ArgEnv["LLAMA_ARG_SLOT_SAVE_PATH"] = "/x" }, why: "à la main"},
		{name: "experts sur CPU", cfg: loop, extra: []string{"--n-cpu-moe", "30"}, why: "CPU"},
		{name: "moteur sans le drapeau", cfg: loop, si: func(s *serveSysInfo) { s.Help = helpRecent }, why: "--slot-save-path"},
		{name: "dossier indisponible", cfg: loop, si: func(s *serveSysInfo) { s.SlotDir = "" }, why: "dossier"},
		{name: "preset externe", cfg: with(extKeyFlag, "1"), why: "externe"},
	}
	for _, c := range cases {
		si := persistSI()
		if c.si != nil {
			c.si(&si)
		}
		ok, why := slotPersistPlan(c.cfg, c.extra, si)
		if ok != c.ok || !strings.Contains(why, c.why) {
			t.Errorf("%s : ok=%v why=%q", c.name, ok, why)
		}
	}
}

// --slot-save-path : posé pour SLOT_PERSIST même quand l'isolation n'en veut
// pas (cache coupé) ; sans la clé, la règle du lot 1 inchangée.
func TestSlotPersistSlotSaveArgs(t *testing.T) {
	si := serveSysInfo{Help: helpCacheRAM, SlotDir: "/data/slots", ArgEnv: map[string]string{}}
	base := map[string]string{"HOST": "127.0.0.1", "CACHE_RAM": "0"}
	if got := slotSaveArgs(base, nil, si); got != nil {
		t.Fatalf("sans la clé : %v", got)
	}
	on := map[string]string{"HOST": "127.0.0.1", "CACHE_RAM": "0", "SLOT_PERSIST": "on"}
	if got := slotSaveArgs(on, nil, si); !reflect.DeepEqual(got, []string{"--slot-save-path", "/data/slots"}) {
		t.Fatalf("avec la clé : %v", got)
	}
	open := map[string]string{"CACHE_RAM": "0", "SLOT_PERSIST": "on"}
	if got := slotSaveArgs(open, nil, si); got != nil {
		t.Fatalf("0.0.0.0 sans clé : %v", got)
	}
}

// Refus : la ligne est celle sans la clé, une note de plus.
func TestSlotPersistRefusLigneInchangee(t *testing.T) {
	si := persistSI()
	cfg := map[string]string{"CTX": "32768", "SPEC": "auto"}
	want, _, wantNotes := buildServeArgs(cfg, nil, "/bin/llama-server", si)
	cfg["SLOT_PERSIST"] = "on"
	got, _, notes := buildServeArgs(cfg, nil, "/bin/llama-server", si)
	if !reflect.DeepEqual(got, want) || len(notes) != len(wantNotes)+1 {
		t.Fatalf("refus :\n%v\n%v\n%q", got, want, notes)
	}
}

func TestSlotPersistKey(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "llama-server")
	lib := filepath.Join(dir, "libggml-cuda.so")
	model := filepath.Join(dir, "m.gguf")
	for _, p := range []string{bin, lib, model} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	si := serveSysInfo{Model: model}
	args := []string{bin, "-m", model, "-c", "32768", "--api-key", "secret"}
	k0 := slotPersistKey(args, nil, si, bin)
	if len(k0) != 16 || k0 != slotPersistKey(args, nil, si, bin) {
		t.Fatalf("clé instable : %q", k0)
	}
	if slotPersistKey([]string{bin, "-m", model, "-c", "32768", "--api-key", "autre"}, nil, si, bin) != k0 {
		t.Fatal("la clé d'API ne doit pas entrer dans l'empreinte")
	}
	if slotPersistKey([]string{bin, "-m", model, "-c", "65536"}, nil, si, bin) == k0 {
		t.Fatal("CTX ignoré")
	}
	if slotPersistKey(args, map[string]string{"GGML_CUDA_GRAPH_OPT": "1"}, si, bin) == k0 {
		t.Fatal("environnement ignoré")
	}
	later := time.Now().Add(time.Hour)
	for _, p := range []string{lib, model, bin} {
		if err := os.Chtimes(p, later, later); err != nil {
			t.Fatal(err)
		}
		k := slotPersistKey(args, nil, si, bin)
		if k == k0 {
			t.Fatalf("%s modifié, clé inchangée", filepath.Base(p))
		}
		k0 = k
	}
}

func TestSlotPersistFichiers(t *testing.T) {
	home := t.TempDir()
	dir := prepareSlotDir(home, true)
	names := []string{"persist-0123456789abcdef-000000000001.bin", "persist-0123456789abcdef-000000000002.bin",
		"persist-fedcba9876543210-000000000003.bin"}
	for i, n := range append(append([]string{}, names...), "tmp-"+names[0], "autre.bin") {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(time.Duration(i) * time.Minute)
		_ = os.Chtimes(p, at, at)
	}
	prepareSlotDir(home, true)
	slotPersistPrune(dir)
	left, _ := os.ReadDir(dir)
	var got []string
	for _, e := range left {
		got = append(got, e.Name())
	}
	if !reflect.DeepEqual(got, names[1:]) {
		t.Fatalf("restent %v, attendu les deux plus récents %v", got, names[1:])
	}
	prepareSlotDir(home, false)
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("clé retirée : %d fichier(s) gardé(s)", len(left))
	}
	if readSlotPersistMarker(home) != "" {
		t.Fatal("marqueur absent : clé vide attendue")
	}
	writeSlotPersistMarker(home, "0123456789abcdef")
	if readSlotPersistMarker(home) != "0123456789abcdef" {
		t.Fatal("marqueur illisible")
	}
	writeSlotPersistMarker(home, "")
	if readSlotPersistMarker(home) != "" {
		t.Fatal("marqueur non retiré")
	}
}

// moteurSlots : faux llama-server pour /slots, save et restore. Un save écrit
// le fichier dans le dossier des slots, comme le vrai.
type moteurSlots struct {
	mu          sync.Mutex
	actions     []string
	used, spec  bool
	restoreFail bool
	tokens      int
}

func (m *moteurSlots) start(t *testing.T, home string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch {
		case r.URL.Path == "/slots" && r.Method == http.MethodGet:
			s := map[string]any{"id": 0, "is_processing": false, "speculative": m.spec, "n_prompt_tokens": m.tokens}
			if m.used {
				s["id_task"] = 7
			}
			sendJSON(w, 200, []any{s})
		case r.URL.Path == "/slots/0" && r.Method == http.MethodPost:
			var body struct {
				Filename string `json:"filename"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			act := r.URL.Query().Get("action")
			m.actions = append(m.actions, act+" "+body.Filename)
			if act == "save" {
				_ = os.WriteFile(filepath.Join(home, "slots", body.Filename), []byte("kv"), 0o600)
				sendJSON(w, 200, map[string]any{"n_saved": m.tokens, "n_written": 2 << 20})
				return
			}
			if m.restoreFail {
				sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "Unable to restore slot"}})
				return
			}
			sendJSON(w, 200, map[string]any{"n_restored": m.tokens, "n_read": 2 << 20})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	if err := SetConfigKey("PORT", port); err != nil {
		t.Fatal(err)
	}
}

func (m *moteurSlots) got() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.actions...)
}

const persistTestKey = "0123456789abcdef"

// persistSetup : discussion « c1 » dont le tour vient de passer par le slot.
func persistSetup(t *testing.T, on bool) (*moteurSlots, string) {
	t.Helper()
	home := testHome(t)
	if err := os.MkdirAll(filepath.Join(home, "slots"), 0o700); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"MODEL": "m.gguf", "CTX": "32768"} {
		if err := SetConfigKey(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if on {
		if err := SetConfigKey("SLOT_PERSIST", "on"); err != nil {
			t.Fatal(err)
		}
	}
	writeSlotPersistMarker(home, persistTestKey)
	prev := slotPersistSize
	slotPersistSize = func(map[string]string, int) int64 { return 1 << 20 }
	reset := func() {
		engineGate.mu.Lock()
		engineGate.main = engineMainStamp{}
		engineGate.mu.Unlock()
	}
	reset()
	t.Cleanup(func() { slotPersistSize = prev; reset() })
	m := &moteurSlots{tokens: 20000}
	m.start(t, home)
	end, seq := engineRequestBegin(nil)
	end()
	engineMarkMain(seq, "c1")
	return m, home
}

// Bascule puis retour : gardé sous la clé du moteur, rechargé une fois avant
// la première requête de la discussion, puis retiré.
func TestSlotPersistSauveEtRecharge(t *testing.T) {
	m, home := persistSetup(t, true)
	snap := slotPersistPrepare()
	if snap == nil {
		t.Fatal("rien à garder")
	}
	snap.save()
	name := slotPersistName(persistTestKey, "c1")
	path := filepath.Join(home, "slots", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("état non gardé : %v (%v)", err, m.got())
	}
	if _, err := os.Stat(filepath.Join(home, "slots", "tmp-"+name)); !os.IsNotExist(err) {
		t.Fatal("fichier provisoire resté")
	}
	ep := resolveChatEndpoint()
	slotPersistRestore(ep, "autre") // autre discussion : rien
	slotPersistRestore(ep, "c1")
	slotPersistRestore(ep, "c1") // une seule fois
	want := []string{"save tmp-" + name, "restore " + name}
	if got := m.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("appels %v, attendu %v", got, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("fichier gardé après la tentative")
	}
}

// Sans la clé : ni relevé, ni appel au moteur.
func TestSlotPersistDefautRien(t *testing.T) {
	m, home := persistSetup(t, false)
	if snap := slotPersistPrepare(); snap != nil {
		t.Fatal("relevé sans la clé")
	}
	var snap *slotPersistSnap
	snap.save()
	name := slotPersistName(persistTestKey, "c1")
	if err := os.WriteFile(filepath.Join(home, "slots", name), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	slotPersistRestore(resolveChatEndpoint(), "c1")
	if got := m.got(); len(got) != 0 {
		t.Fatalf("appels sans la clé : %v", got)
	}
}

// Rien n'est gardé si le slot ne porte plus le tour, si un brouillon tourne,
// si l'état est trop court ou trop gros, ou si une requête part pendant
// l'écriture.
func TestSlotPersistSauvegardeRefusee(t *testing.T) {
	m, home := persistSetup(t, true)
	end := engineRequestStart() // une autre requête est passée par le slot
	end()
	if slotPersistPrepare() != nil {
		t.Fatal("relevé alors que le slot ne porte plus le tour")
	}
	for _, c := range []struct {
		name string
		set  func()
	}{
		{"brouillon spéculatif", func() { m.spec = true }},
		{"contexte court", func() { m.tokens = 100 }},
		{"état trop gros", func() { slotPersistSize = func(map[string]string, int) int64 { return slotPersistMaxBytes + 1 } }},
	} {
		m.mu.Lock()
		m.spec, m.tokens = false, 20000
		m.mu.Unlock()
		slotPersistSize = func(map[string]string, int) int64 { return 1 << 20 }
		e, seq := engineRequestBegin(nil)
		e()
		engineMarkMain(seq, "c1")
		snap := slotPersistPrepare()
		m.mu.Lock()
		c.set()
		m.mu.Unlock()
		snap.save()
		if got := m.got(); len(got) != 0 {
			t.Fatalf("%s : appels %v", c.name, got)
		}
	}
	// Une requête entre le relevé et l'écriture : fichier jeté.
	slotPersistSize = func(map[string]string, int) int64 { return 1 << 20 }
	m.mu.Lock()
	m.spec, m.tokens = false, 20000
	m.mu.Unlock()
	e, seq := engineRequestBegin(nil)
	e()
	engineMarkMain(seq, "c1")
	snap := slotPersistPrepare()
	e2 := engineRequestStart()
	e2()
	snap.save()
	left, _ := os.ReadDir(filepath.Join(home, "slots"))
	if len(left) != 0 {
		t.Fatalf("fichier gardé malgré une requête pendant l'écriture : %v", left[0].Name())
	}
}

// Rechargement refusé (slot déjà servi, brouillon) ou raté : calcul normal,
// fichier retiré, aucune erreur.
func TestSlotPersistRechargementPrudent(t *testing.T) {
	for _, c := range []struct {
		name    string
		set     func(*moteurSlots)
		restore bool
	}{
		{"slot déjà servi", func(m *moteurSlots) { m.used = true }, false},
		{"brouillon spéculatif", func(m *moteurSlots) { m.spec = true }, false},
		{"échec du moteur", func(m *moteurSlots) { m.restoreFail = true }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, home := persistSetup(t, true)
			name := slotPersistName(persistTestKey, "c1")
			path := filepath.Join(home, "slots", name)
			if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			c.set(m)
			slotPersistRestore(resolveChatEndpoint(), "c1")
			if got := m.got(); (len(got) == 1) != c.restore {
				t.Fatalf("appels %v", got)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("fichier gardé après la tentative")
			}
		})
	}
}

// Clé du moteur différente (autre build, autres réglages) : le fichier n'est
// même pas cherché.
func TestSlotPersistAutreMoteur(t *testing.T) {
	m, home := persistSetup(t, true)
	name := slotPersistName(persistTestKey, "c1")
	if err := os.WriteFile(filepath.Join(home, "slots", name), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSlotPersistMarker(home, "fedcba9876543210")
	slotPersistRestore(resolveChatEndpoint(), "c1")
	if got := m.got(); len(got) != 0 {
		t.Fatalf("rechargé sous une autre clé : %v", got)
	}
}

// SLOT_PERSIST survit à la bascule (A → B sans la clé → A) : sans ça, le
// lancement de B effacerait l'état gardé de A. Un preset qui la pose gagne.
func TestSlotPersistSurvitALaBascule(t *testing.T) {
	home := testHome(t)
	setConfig(t, "MODEL=a.gguf\nSLOT_PERSIST=on\n")
	b := filepath.Join(home, "b.env")
	if err := os.WriteFile(b, []byte("MODEL=b.gguf\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyPresetFile(b); err != nil {
		t.Fatal(err)
	}
	if got := ReadConfig()["SLOT_PERSIST"]; got != "on" {
		t.Fatalf("SLOT_PERSIST = %q après bascule", got)
	}
	c := filepath.Join(home, "c.env")
	if err := os.WriteFile(c, []byte("MODEL=c.gguf\nSLOT_PERSIST=off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyPresetFile(c); err != nil {
		t.Fatal(err)
	}
	if got := ReadConfig()["SLOT_PERSIST"]; got != "off" {
		t.Fatalf("le preset qui pose la clé doit gagner : %q", got)
	}
}
