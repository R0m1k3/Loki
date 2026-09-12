package loki

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// flakyServer répond `code` aux `fails` premières requêtes, puis sert un flux
// SSE normal. Renvoie le port (à mettre dans PORT) et le compteur d'appels.
func flakyServer(t *testing.T, fails int, code int, body string) (string, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if int(calls.Add(1)) <= fails {
			http.Error(w, "moteur occupé", code)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(body))
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port(), &calls
}

// Le cas qui motive tout le fichier : le moteur redémarre, la connexion est
// refusée quelques secondes, et le tour mourait pour de bon — une tâche
// planifiée tombée pile là échouait sans personne pour recliquer.
func TestConnexionRefuseeEstRejouee(t *testing.T) {
	testHome(t)
	// Un port fermé : on ouvre puis on referme aussitôt pour en obtenir un dont on
	// est sûr que personne n'écoute.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	if err := SetConfigKey("PORT", itoa(port)); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = runChat(context.Background(), []Message{{Role: "user", Content: "bonjour"}}, 0.7, Caps{}, func(ev StreamEvent) bool { return true })
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("moteur injoignable : aucune erreur remontée")
	}
	// 3 reprises espacées de 0,8 / 1,6 / 3,2 s : le tour ne peut pas avoir rendu
	// la main tout de suite. Sans ce délai, aucune reprise n'a eu lieu.
	if min := llmNetWait + 2*llmNetWait; elapsed < min {
		t.Fatalf("rendu la main en %v : aucune reprise n'a eu lieu (attendu au moins %v)", elapsed, min)
	}
}

// Un statut « pas maintenant » (503) est rejoué, et le tour aboutit.
func TestStatutTransitoireEstRejoueEtAboutit(t *testing.T) {
	testHome(t)
	port, calls := flakyServer(t, 2, http.StatusServiceUnavailable,
		sseChunk("ça a fini par passer")+"data: [DONE]\n\n")
	if err := SetConfigKey("PORT", port); err != nil {
		t.Fatal(err)
	}
	var content strings.Builder
	_, err := runChat(context.Background(), []Message{{Role: "user", Content: "bonjour"}}, 0.7, Caps{}, func(ev StreamEvent) bool {
		if ev.Content != "" {
			content.WriteString(ev.Content)
		}
		return true
	})
	if err != nil {
		t.Fatalf("503 transitoire non rattrapé : %v", err)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("%d appel(s) au moteur, attendu 3 (deux échecs puis la bonne)", n)
	}
	if !strings.Contains(content.String(), "ça a fini par passer") {
		t.Fatalf("réponse perdue : %q", content.String())
	}
}

// Contre-épreuve : un 500 n'est PAS un statut de reprise RÉSEAU. C'est ce que
// llama.cpp rend pour un appel d'outil malformé ou un prompt trop long — deux
// échecs déterministes ; attendre puis rejouer à l'identique ne ferait que
// retarder l'erreur de plusieurs secondes.
//
// Le tour peut malgré tout repartir UNE fois : le filet de compaction en vol,
// antérieur à ce fichier, retente le tour avec un historique résumé (le 500 est
// souvent un dépassement de contexte). C'est un rejeu SÉMANTIQUE — il change la
// requête et n'attend pas. La preuve qu'aucune reprise réseau n'a eu lieu est
// donc le CHRONO : trois reprises coûteraient au bas mot 0,8 + 1,6 s d'attente.
func TestErreur500NestPasRejoueeParLeReseau(t *testing.T) {
	testHome(t)
	port, calls := flakyServer(t, 99, http.StatusInternalServerError, "")
	if err := SetConfigKey("PORT", port); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := runChat(context.Background(), []Message{{Role: "user", Content: "bonjour"}}, 0.7, Caps{}, func(ev StreamEvent) bool { return true })
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("500 : aucune erreur remontée")
	}
	if elapsed >= llmNetWait {
		t.Fatalf("rendu la main en %v : une attente de reprise réseau a eu lieu sur un 500", elapsed)
	}
	// Borne haute large : ce qui compte est qu'on ne soit pas allé au bout du
	// budget réseau (1 + 3 = 4 appels).
	if n := calls.Load(); n > 2 {
		t.Fatalf("%d appels au moteur sur un 500 : le budget de reprise réseau a été consommé", n)
	}
}

// Un /stop pendant le tour ne doit surtout pas être rattrapé : réessayer, c'est
// passer outre l'annulation de l'utilisateur.
func TestAnnulationNestJamaisRejouee(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if llmRetryableErr(ctx, syscall.ECONNREFUSED) {
		t.Fatal("ctx annulé : la reprise doit être refusée")
	}
	if llmRetryableErr(context.Background(), context.Canceled) {
		t.Fatal("context.Canceled : la reprise doit être refusée")
	}
	if llmRetryableErr(context.Background(), context.DeadlineExceeded) {
		t.Fatal("échéance dépassée : c'est la limite de l'appelant, pas une panne réseau")
	}
	// L'attente elle-même rend la main tout de suite sur un ctx fini.
	if err := llmNetBackoff(ctx, 2); err == nil {
		t.Fatal("l'attente doit être interrompue par le ctx")
	}
}

// Les erreurs de transport qui méritent une reprise, et celles qui n'en méritent
// pas. Une faute de frappe dans l'URL ne se répare pas en attendant.
func TestErreursDeTransportRejouables(t *testing.T) {
	ctx := context.Background()
	for _, e := range []error{syscall.ECONNREFUSED, syscall.ECONNRESET, io.EOF, io.ErrUnexpectedEOF} {
		if !llmRetryableErr(ctx, e) {
			t.Errorf("%v devrait être rejouable (moteur qui redémarre ou connexion perdue)", e)
		}
	}
	if llmRetryableErr(ctx, errors.New("unsupported protocol scheme")) {
		t.Error("une erreur de configuration ne se répare pas en réessayant")
	}
	if llmRetryableErr(ctx, nil) {
		t.Error("pas d'erreur, pas de reprise")
	}
}

// Les codes de reprise, et ceux qui n'en sont pas.
func TestStatutsRejouables(t *testing.T) {
	for _, c := range []int{http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		if !llmRetryableStatus(c) {
			t.Errorf("%d dit « pas maintenant » : rejouable", c)
		}
	}
	for _, c := range []int{200, 400, 401, 404, 500} {
		if llmRetryableStatus(c) {
			t.Errorf("%d ne doit pas déclencher de reprise", c)
		}
	}
}

// L'attente double à chaque tentative, se plafonne, et ne déborde pas : un
// décalage de bits sur un n non borné repasserait à zéro en silence — la reprise
// deviendrait une rafale.
func TestAttenteCroissantePuisPlafonnee(t *testing.T) {
	if got := llmNetDelay(0); got != llmNetWait {
		t.Errorf("1re attente = %v, attendu %v", got, llmNetWait)
	}
	if got := llmNetDelay(1); got != 2*llmNetWait {
		t.Errorf("2e attente = %v, attendu le double", got)
	}
	for _, n := range []int{10, 20, 64, 1000} {
		if got := llmNetDelay(n); got != llmNetMaxWait {
			t.Errorf("llmNetDelay(%d) = %v, attendu le plafond %v", n, got, llmNetMaxWait)
		}
	}
	if got := llmNetDelay(-1); got != llmNetWait {
		t.Errorf("llmNetDelay(-1) = %v, attendu la 1re attente", got)
	}
}
