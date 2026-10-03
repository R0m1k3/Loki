package loki

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeEngine : un llama-server réduit à /props, /slots et l'effacement.
type fakeEngine struct {
	build      string
	totalSlots int
	processing bool
	eraseCode  int

	mu     sync.Mutex
	erases []*http.Request
}

func (f *fakeEngine) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/props":
			_ = json.NewEncoder(w).Encode(map[string]any{"build_info": f.build, "total_slots": f.totalSlots})
		case r.Method == http.MethodGet && r.URL.Path == "/slots":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 0, "is_processing": f.processing}})
		case strings.HasPrefix(r.URL.Path, "/slots/"):
			f.mu.Lock()
			f.erases = append(f.erases, r)
			f.mu.Unlock()
			w.WriteHeader(f.eraseCode)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeEngine) isolator(srv *httptest.Server) slotIsolator {
	ep := chatEndpoint{Key: "sk-local"}
	return slotIsolator{base: srv.URL, auth: ep.auth, client: srv.Client()}
}

func TestSlotEraseAppel(t *testing.T) {
	f := &fakeEngine{build: "b8700-50e0ad08", totalSlots: 1, eraseCode: 200}
	srv := f.server(t)
	erased, err := f.isolator(srv).eraseIfIdle(context.Background())
	if err != nil || !erased {
		t.Fatalf("eraseIfIdle = %v, %v", erased, err)
	}
	if len(f.erases) != 1 {
		t.Fatalf("%d effacements", len(f.erases))
	}
	r := f.erases[0]
	if r.Method != http.MethodPost || r.URL.Path != "/slots/0" || r.URL.Query().Get("action") != "erase" {
		t.Fatalf("requête %s %s", r.Method, r.URL)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer sk-local" {
		t.Fatalf("Authorization = %q", got)
	}
}

// Moteur sans --slot-save-path : 501, toléré.
func TestSlotErase501Tolere(t *testing.T) {
	f := &fakeEngine{build: "b10678-abc", totalSlots: 1, eraseCode: http.StatusNotImplemented}
	erased, err := f.isolator(f.server(t)).eraseIfIdle(context.Background())
	if erased || err == nil {
		t.Fatalf("501 : eraseIfIdle = %v, %v", erased, err)
	}
}

// Chaque garde-fou suffit à empêcher l'effacement — sans erreur : s'abstenir
// est le comportement normal.
func TestSlotEraseGardeFous(t *testing.T) {
	cases := []struct {
		name       string
		build      string
		slots      int
		processing bool
		busy       bool
	}{
		{name: "build avant le rechargement d'un slot vide", build: "b8640-abc", slots: 1},
		{name: "build inconnu (fork)", build: "", slots: 1},
		{name: "plusieurs slots", build: "b9000-abc", slots: 2},
		{name: "slot 0 au travail", build: "b9000-abc", slots: 1, processing: true},
		{name: "requête de Loki en vol (proxy /v1, tour de chat)", build: "b9000-abc", slots: 1, busy: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeEngine{build: c.build, totalSlots: c.slots, processing: c.processing, eraseCode: 200}
			srv := f.server(t)
			if c.busy {
				done := engineRequestStart()
				defer done()
			}
			erased, err := f.isolator(srv).eraseIfIdle(context.Background())
			if erased || err != nil || len(f.erases) != 0 {
				t.Fatalf("eraseIfIdle = %v, %v ; %d effacements", erased, err, len(f.erases))
			}
		})
	}
}

// La fonction de fin d'une requête ne décompte qu'une fois, même rappelée
// (runChat la rappelle sur plusieurs chemins de sortie).
func TestEngineRequestStartIdempotent(t *testing.T) {
	done := engineRequestStart()
	done()
	done()
	engineGate.mu.Lock()
	n := engineGate.inflight
	engineGate.mu.Unlock()
	if n != 0 {
		t.Fatalf("inflight = %d", n)
	}
}

func TestNoteEnginePrompt(t *testing.T) {
	slotHintShown.Store(false)
	slotHintPending.Store(0)
	// Rien d'armé : jamais de conseil.
	if h := noteEnginePrompt(30000, 100, true); h != "" {
		t.Fatalf("sans travail annexe : %q", h)
	}
	if lastEnginePrompt.Load() != 30000 {
		t.Fatal("dernier prompt non retenu")
	}
	// Conversation rechargée du cache : rien à dire.
	slotHintPending.Store(30000)
	if h := noteEnginePrompt(30500, 30000, true); h != "" {
		t.Fatalf("cache tenu : %q", h)
	}
	// Moteur sans cached_tokens : on ne sait pas, on se tait.
	slotHintPending.Store(30000)
	if h := noteEnginePrompt(30500, 0, false); h != "" {
		t.Fatalf("sans cached_tokens : %q", h)
	}
	// Prompt plus court : une autre conversation, pas un recalcul.
	slotHintPending.Store(30000)
	if h := noteEnginePrompt(5000, 1000, true); h != "" {
		t.Fatalf("autre conversation : %q", h)
	}
	// Recalcul complet : le conseil, une seule fois.
	slotHintPending.Store(30000)
	if h := noteEnginePrompt(30500, 2000, true); !strings.Contains(h, "CACHE_RAM") {
		t.Fatalf("recalcul : %q", h)
	}
	slotHintPending.Store(30000)
	if h := noteEnginePrompt(30500, 2000, true); h != "" {
		t.Fatalf("second conseil : %q", h)
	}
}

// Les proxys de Loki ne laissent passer que les lectures de /slots.
func TestProxysRefusentLesActionsSlots(t *testing.T) {
	t.Setenv("LOKI_HOME", t.TempDir())
	t.Setenv("LOKI_LINK_ALLOW_OAI", "1")
	for name, h := range map[string]http.Handler{
		"oaiHandler":     oaiHandler(),
		"newLinkHandler": newLinkHandler(http.NewServeMux()),
	} {
		for _, action := range []string{"save", "restore", "erase"} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/slots/0?action="+action, strings.NewReader(`{"filename":"x.bin"}`))
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s : POST /slots/0?action=%s → %d, attendu 405", name, action, rec.Code)
			}
		}
	}
}

// Un travail annexe que le moteur n'a jamais servi (erreur avant l'envoi,
// refus, arrêt immédiat) n'a pas pris le slot : il porte encore la
// conversation, on n'efface rien. Servi, on efface et on arme la vérification.
func TestFinishSideJobSeulementApresUneRequeteServie(t *testing.T) {
	slotHintPending.Store(0)
	calls := 0
	erase := func() (bool, error) { calls++; return true, nil }

	finishSideJob(30000, engineServedCount(), erase)
	if calls != 0 || slotHintPending.Load() != 0 {
		t.Fatalf("sans requête servie : %d effacements, vérification %d", calls, slotHintPending.Load())
	}

	at := engineServedCount()
	engineServed()
	finishSideJob(30000, at, erase)
	if calls != 1 || slotHintPending.Load() != 30000 {
		t.Fatalf("après une requête servie : %d effacements, vérification %d", calls, slotHintPending.Load())
	}
	slotHintPending.Store(0)

	// Abstention ou échec : rien d'armé.
	at = engineServedCount()
	engineServed()
	finishSideJob(30000, at, func() (bool, error) { return false, nil })
	if slotHintPending.Load() != 0 {
		t.Fatal("vérification armée sans effacement")
	}
}

// Pendant un travail annexe, ses propres prompts ne comptent pas : ni retenus
// comme « la conversation », ni comparés à elle.
func TestNoteEnginePromptIgnoreLesTravauxAnnexes(t *testing.T) {
	slotHintShown.Store(false)
	lastEnginePrompt.Store(30000)
	slotHintPending.Store(30000)
	sideJobs.Add(1) // ce que fait engineSideJob en entrant
	if h := noteEnginePrompt(40000, 10, true); h != "" {
		t.Fatalf("prompt d'un travail annexe comparé : %q", h)
	}
	if lastEnginePrompt.Load() != 30000 || slotHintPending.Load() != 30000 {
		t.Fatalf("prompt annexe retenu : dernier %d, en attente %d", lastEnginePrompt.Load(), slotHintPending.Load())
	}
	sideJobs.Add(-1)
	slotHintPending.Store(0)
	slotHintShown.Store(false)
}
