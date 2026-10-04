package loki

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// gatePersist remplace l'écriture par une version qu'on retient : chaque lot
// signale son arrivée sur entered puis attend que release soit fermé. Les
// échanges de persistCommit se font écrivain vidé : aucun lot ne le lit alors.
func gatePersist(t *testing.T) (entered chan []*convSnap, release chan struct{}) {
	t.Helper()
	entered = make(chan []*convSnap, 64)
	release = make(chan struct{})
	persistQ.flush()
	prev := persistCommit
	persistCommit = func(path string, s []*convSnap, r []toolResJob) ([]*convSnap, error) {
		entered <- s
		<-release
		return prev(path, s, r)
	}
	t.Cleanup(func() {
		persistQ.flush()
		persistCommit = prev
	})
	return entered, release
}

// waitEntered attend qu'un lot arrive à l'écriture.
func waitEntered(t *testing.T, entered chan []*convSnap) []*convSnap {
	t.Helper()
	select {
	case s := <-entered:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("aucun lot n'a atteint l'écriture")
		return nil
	}
}

// diskMessages relit le nombre de messages et le texte d'une discussion en base.
func diskMessages(t *testing.T, id string) (int, string) {
	t.Helper()
	b := storeBytes(bkChat, convKey(id))
	if len(b) == 0 {
		return -1, ""
	}
	var st struct {
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return len(st.Messages), string(b)
}

func addUserMsg(text string) {
	conv.mu.Lock()
	conv.Messages = append(conv.Messages, Message{Role: "user", Content: text})
	conv.Log = append(conv.Log, LogEvent{Seq: len(conv.Log) + 1, Delta: map[string]any{"user": text}})
	conv.mu.Unlock()
}

// Des instantanés pris en rafale depuis plusieurs goroutines sont écrits dans
// l'ordre où ils ont été pris : jamais un plus ancien après un plus récent, et
// la base finit sur le dernier état.
func TestPersisterNEcritJamaisUnEtatPlusAncien(t *testing.T) {
	testHome(t)
	resetConvForTest()
	persistQ.flush()
	var mu sync.Mutex
	var seqs []uint64
	prev := persistCommit
	persistCommit = func(path string, s []*convSnap, r []toolResJob) ([]*convSnap, error) {
		mu.Lock()
		for _, x := range s {
			seqs = append(seqs, x.seq)
		}
		mu.Unlock()
		time.Sleep(2 * time.Millisecond) // laisse la file s'accumuler
		return prev(path, s, r)
	}
	t.Cleanup(func() {
		persistQ.flush()
		persistCommit = prev
	})

	const workers, per = 8, 10
	var wg sync.WaitGroup
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				addUserMsg(fmt.Sprintf("g%d-%d", g, i))
				conv.persistAsync()
			}
		}(g)
	}
	wg.Wait()
	persistQ.flush()

	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("instantané %d écrit après %d : un état plus ancien a écrasé un plus récent", seqs[i], seqs[i-1])
		}
	}
	if n, _ := diskMessages(t, convEnsureActive()); n != workers*per {
		t.Fatalf("la base finit sur %d messages, attendu %d (dernier état)", n, workers*per)
	}
}

// persist (fin de tour, reset, bascule) est SYNCHRONE : il attend l'écriture en
// vol, puis la sienne. Au retour de Reset, la base porte déjà le fil vide.
func TestPersistSynchroneAttendLesEcrituresEnVol(t *testing.T) {
	testHome(t)
	resetConvForTest()
	entered, release := gatePersist(t)
	addUserMsg("avant le reset")
	conv.persistAsync()
	waitEntered(t, entered) // l'écrivain est retenu au milieu du lot

	done := make(chan struct{})
	go func() {
		conv.Reset()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Reset est revenu avant que l'écriture en vol soit terminée")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Reset bloqué")
	}
	if n, _ := diskMessages(t, convEnsureActive()); n != 0 {
		t.Fatalf("au retour de Reset, la base doit porter le fil vide (%d message(s))", n)
	}
}

// « Voir plus » marche avant même que le résultat soit sur disque, et la copie
// en mémoire disparaît une fois l'écriture faite.
func TestToolResultServiAvantLEcriture(t *testing.T) {
	testHome(t)
	entered, release := gatePersist(t)
	long := strings.Repeat("sortie\n", 1000)
	id := saveToolResult(long)
	if id == "" {
		t.Fatal("résultat non enregistré")
	}
	waitEntered(t, entered)
	if got, ok := loadToolResult(id); !ok || got != long {
		t.Fatal("le résultat doit être servi depuis la mémoire tant qu'il n'est pas écrit")
	}
	close(release)
	persistQ.flush()
	if _, ok := persistQ.toolResPending(id); ok {
		t.Fatal("la copie en mémoire doit partir une fois le résultat écrit")
	}
	if got, ok := loadToolResult(id); !ok || got != long {
		t.Fatal("résultat perdu après l'écriture")
	}
}

// Un résultat supprimé avec sa discussion avant d'être écrit n'est jamais écrit.
func TestToolResultSupprimeAvantEcriture(t *testing.T) {
	testHome(t)
	entered, release := gatePersist(t)
	saveToolResult(strings.Repeat("a", 3000)) // retient l'écrivain
	waitEntered(t, entered)
	second := saveToolResult(strings.Repeat("b", 3000)) // en attente derrière
	deleteToolResultsFor(toolResConv(second))
	close(release)
	persistQ.flush()
	if _, ok := loadToolResult(second); ok {
		t.Fatal("un résultat supprimé avant son écriture a été écrit quand même")
	}
}

// compactLog rend exactement ce que donne la compaction de fin de tour, sans
// toucher au journal d'origine.
func TestCompactLogPurEgalFinDeTour(t *testing.T) {
	log := []LogEvent{
		{Seq: 1, TS: 10, Delta: map[string]any{"user": "q"}},
		{Seq: 2, TS: 11, Delta: map[string]any{"reasoning_content": "je ", "toks": 1}},
		{Seq: 3, TS: 12, Delta: map[string]any{"reasoning_content": "pense", "toks": 1}},
		{Seq: 4, TS: 13, Delta: map[string]any{"tool_used": map[string]any{"name": "bash", "done": false}}},
		{Seq: 5, TS: 14, Delta: map[string]any{"tool_used": map[string]any{"name": "bash", "done": true}}},
		{Seq: 6, TS: 15, Delta: map[string]any{"content": "bon", "toks": 1}},
		{Seq: 7, TS: 16, Delta: map[string]any{"content": "jour", "toks": 1}},
	}
	orig := append([]LogEvent(nil), log...)
	got := compactLog(log)
	if !reflect.DeepEqual(log, orig) {
		t.Fatal("compactLog a modifié le journal d'origine")
	}
	c := newTestConv()
	c.Log = append([]LogEvent(nil), log...)
	c.compactLogLocked()
	if !reflect.DeepEqual(got, c.Log) {
		t.Fatalf("compactLog diffère de la fin de tour :\n%v\n%v", got, c.Log)
	}
	if len(got) != 4 {
		t.Fatalf("attendu 4 événements (user, réflexion, outil fini, texte), got %d", len(got))
	}
}

// En plein tour, la base reçoit le journal compacté ; le journal en mémoire
// (suivi par les abonnés) garde ses événements bruts.
func TestPersistEnPleinTourEcritLeJournalCompacte(t *testing.T) {
	testHome(t)
	resetConvForTest()
	conv.mu.Lock()
	conv.Generating = true
	for i := 0; i < 50; i++ {
		conv.Log = append(conv.Log, LogEvent{Seq: i + 1, Delta: map[string]any{"content": "x", "toks": 1}})
	}
	conv.mu.Unlock()
	t.Cleanup(func() {
		conv.mu.Lock()
		conv.Generating = false
		conv.mu.Unlock()
	})
	conv.persistAsync()
	persistQ.flush()
	var st struct {
		Log []LogEvent `json:"log"`
	}
	if err := json.Unmarshal(storeBytes(bkChat, convKey(convEnsureActive())), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Log) != 1 || st.Log[0].Delta["content"] != strings.Repeat("x", 50) {
		t.Fatalf("journal écrit non compacté : %d événement(s)", len(st.Log))
	}
	conv.mu.Lock()
	n := len(conv.Log)
	conv.mu.Unlock()
	if n != 50 {
		t.Fatalf("le journal en mémoire ne doit pas être touché : %d événement(s)", n)
	}
}

// Contenu et entrée d'index partent dans la MÊME écriture.
func TestPersistEcritContenuEtIndexEnUneFois(t *testing.T) {
	testHome(t)
	resetConvForTest()
	persistQ.flush()
	var calls int
	var mu sync.Mutex
	prev := persistCommit
	persistCommit = func(path string, s []*convSnap, r []toolResJob) ([]*convSnap, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return prev(path, s, r)
	}
	t.Cleanup(func() {
		persistQ.flush()
		persistCommit = prev
	})
	id := convEnsureActive()
	addUserMsg("titre de la discussion")
	conv.persist()
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Fatalf("%d écritures pour un persist, attendu 1", n)
	}
	if c, _ := diskMessages(t, id); c != 1 {
		t.Fatalf("contenu non écrit (%d)", c)
	}
	for _, m := range convIndex() {
		if m.ID == id {
			if m.Turns != 1 || m.Title != "titre de la discussion" {
				t.Fatalf("entrée d'index non rafraîchie : %+v", m)
			}
			return
		}
	}
	t.Fatal("discussion absente de l'index")
}

// Supprimer une discussion pendant qu'une écriture la vise ne la fait pas
// renaître : ni contenu, ni entrée d'index.
func TestSuppressionPendantEcritureNeRessusciteRien(t *testing.T) {
	testHome(t)
	resetConvForTest()
	a := convEnsureActive()
	addUserMsg("fil A")
	conv.persist()
	b := convNew()
	if err := convSwitch(a); err != nil {
		t.Fatal(err)
	}

	entered, release := gatePersist(t)
	addUserMsg("fil A, suite")
	conv.persistAsync()
	waitEntered(t, entered) // un lot visant A est en vol
	addUserMsg("fil A, encore")
	conv.persistAsync() // un autre attend derrière

	done := make(chan error, 1)
	go func() { done <- convDelete(a) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("convDelete bloqué")
	}
	persistQ.flush()
	if n, _ := diskMessages(t, a); n != -1 {
		t.Fatalf("la discussion supprimée a été réécrite (%d messages)", n)
	}
	for _, m := range convIndex() {
		if m.ID == a {
			t.Fatal("la discussion supprimée est revenue dans l'index")
		}
	}
	if got := convEnsureActive(); got != b {
		t.Fatalf("active = %q, attendu %q", got, b)
	}
}

// Basculer pendant qu'une écriture est en vol : l'ancien fil est écrit sous SON
// identifiant, jamais sous celui de la discussion qu'on ouvre.
func TestBasculePendantEcritureGardeChaqueFilChezLui(t *testing.T) {
	testHome(t)
	resetConvForTest()
	a := convEnsureActive()
	addUserMsg("seulement dans A")
	conv.persist()
	b := convNew()
	addUserMsg("seulement dans B")
	conv.persist()
	if err := convSwitch(a); err != nil {
		t.Fatal(err)
	}

	entered, release := gatePersist(t)
	addUserMsg("A encore")
	conv.persistAsync()
	waitEntered(t, entered)
	done := make(chan error, 1)
	go func() { done <- convSwitch(b) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	persistQ.flush()
	if _, body := diskMessages(t, b); strings.Contains(body, "dans A") {
		t.Fatal("le fil A a été écrit sous l'identifiant de B")
	}
	if n, body := diskMessages(t, a); n != 2 || !strings.Contains(body, "A encore") {
		t.Fatalf("le fil A n'a pas gardé son dernier état (%d messages)", n)
	}
	conv.mu.Lock()
	got := len(conv.Messages)
	conv.mu.Unlock()
	if got != 1 {
		t.Fatalf("la bascule doit charger B (1 message), got %d", got)
	}
}

// Base neuve : l'instantané de StartTurn crée la discussion active TOUT DE
// SUITE, avant l'écriture — generate, qui démarre juste après, voit la même
// discussion (mode Code, critères, espace de travail) que la persistance.
func TestPersistAsyncCreeLaDiscussionActiveAvantGenerate(t *testing.T) {
	testHome(t)
	resetConvForTest()
	entered, release := gatePersist(t)
	defer close(release)
	addUserMsg("premier message")
	conv.persistAsync()
	snaps := waitEntered(t, entered)
	id := getStr(bkChat, ckActive)
	if id == "" {
		t.Fatal("la discussion active doit exister dès le retour de persistAsync")
	}
	if got := convEnsureActive(); got != id {
		t.Fatalf("generate verrait %q, la persistance %q", got, id)
	}
	if len(snaps) != 1 || snaps[0].id != id {
		t.Fatalf("l'instantané vise %v, attendu %q", snaps, id)
	}
}

// Base neuve, appels simultanés : un seul identifiant est forgé.
func TestConvEnsureActiveConcurrentForgeUnSeulID(t *testing.T) {
	testHome(t)
	var wg sync.WaitGroup
	ids := make([]string, 8)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i] = convEnsureActive()
		}(i)
	}
	wg.Wait()
	for _, id := range ids[1:] {
		if id != ids[0] {
			t.Fatalf("identifiants divergents : %v", ids)
		}
	}
	if n := len(convIndex()); n != 1 {
		t.Fatalf("%d entrées d'index, attendu 1", n)
	}
}
