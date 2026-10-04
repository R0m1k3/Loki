package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Isolation des travaux annexes dans le cache de prompts.
//
// llama-server tourne en --parallel 1 : un vérificateur, un sous-agent, une
// tâche planifiée ou un bench prennent LE slot de la conversation. Le moteur
// range alors l'état de la conversation dans son cache RAM (exact, octet pour
// octet) et le recharge quand elle revient — à condition qu'il y soit encore.
// Or, à la requête suivante, il sauve d'abord l'état du travail annexe, et pour
// lui faire de la place il évince les entrées les plus anciennes : souvent
// justement la conversation, qui est alors recalculée en entier.
//
// Effacer le slot une fois le travail annexe TERMINÉ (pas entre ses étapes)
// règle ça : un slot vide n'est pas sauvé, et la conversation est rechargée
// depuis la RAM. On ne jette qu'un état qui ne resservirait pas — rien de ce
// que voit le modèle ne change.
//
// Mais un effacement est différé par le moteur tant que le slot travaille, et
// s'exécute ensuite sur ce qui s'y trouve alors : il faut donc être SÛR que le
// slot est à nous. D'où les garde-fous, tous nécessaires :
//   - moteur local (jamais une API externe) ;
//   - build vanilla ≥ 8660 : avant (PR #20993), un slot vide ne rechargeait pas
//     depuis le cache, et effacer aurait forcé un recalcul complet. Build
//     inconnu ou fork : on s'abstient ;
//   - un seul slot (/props total_slots, pas la clé PARALLEL qu'EXTRA_ARGS peut
//     contredire) ;
//   - le travail annexe a bien été servi par le moteur : sinon le slot porte
//     encore la conversation (erreur avant l'envoi, refus, arrêt immédiat) ;
//   - aucune requête de Loki en vol vers le moteur (compteur ci-dessous, tenu
//     par le chat, les résumés, le bench et les proxys /v1), et le slot 0 au
//     repos d'après /slots ;
//   - appel synchrone, borné à 2 s, sous le verrou du compteur : aucune
//     requête de Loki ne peut partir entre la vérification et l'effacement.
//
// Avec SIDE_SLOT (deux slots, llm_sideslot.go), rien n'est jamais effacé : le
// garde-fou du slot unique s'abstient, et les travaux annexes ne passent de
// toute façon plus par le slot de la discussion. Pour la même raison, leurs
// requêtes n'avancent pas le numéro qui dit si le slot 0 porte encore le tour
// (engineSlotHolds) : COMPACT_CONTINUATION reste possible après eux.

// slotEraseMinBuild : premier build de llama.cpp qui recharge un slot vide
// depuis le cache de prompts (PR #20993, 50e0ad08fb).
const slotEraseMinBuild = 8660

// engineGate compte les requêtes de Loki en vol vers le moteur local.
// L'effacement prend le verrou pendant sa vérification et son appel : une
// nouvelle requête attend au plus ces deux secondes.
var engineGate struct {
	mu       sync.Mutex
	inflight int
	served   uint64 // réponses 200 du moteur obtenues par runChat ou le bench
	// seq numérote les requêtes de Loki vers le moteur local, préchauffage
	// compris. main : la dernière, quand c'était une étape d'un tour de
	// discussion acceptée par le moteur — le slot porte alors, selon toute
	// vraisemblance, le prompt de cette discussion (COMPACT_CONTINUATION).
	seq  uint64
	main engineMainStamp
}

// engineMainStamp : la requête d'un tour de discussion que le slot porte.
// Modèle et fenêtre en font partie : un moteur relancé sur un autre modèle ou
// une autre fenêtre a un slot vide.
type engineMainStamp struct {
	seq         uint64
	conv, model string
	window      int
}

// engineMainNow : modèle et fenêtre du moteur, pour le tampon.
func engineMainNow() (model string, window int) {
	return filepath.Base(strings.TrimSpace(ReadConfig()["MODEL"])), ctxWindow()
}

// engineMarkMain : la requête seq, étape d'un tour de la discussion conv, a été
// acceptée par le moteur. Sans effet si une autre requête est partie depuis.
func engineMarkMain(seq uint64, conv string) {
	if conv == "" {
		return
	}
	model, window := engineMainNow()
	engineGate.mu.Lock()
	defer engineGate.mu.Unlock()
	if engineGate.seq == seq {
		engineGate.main = engineMainStamp{seq: seq, conv: conv, model: model, window: window}
	}
}

// engineSlotHolds : le slot porte-t-il plausiblement le prompt de conv ? Oui si
// la dernière requête de Loki vers le moteur était une étape d'un de ses tours,
// acceptée, sur le même modèle et la même fenêtre. Un vérificateur, un
// sous-agent, une tâche, un préchauffage, un résumé, un bench, un client /v1 ou
// une autre discussion passés depuis : non. Un client qui parlerait au moteur
// sans passer par Loki échappe à ce compte — au pire, un recalcul.
func engineSlotHolds(conv string) bool {
	if conv == "" {
		return false
	}
	model, window := engineMainNow()
	engineGate.mu.Lock()
	defer engineGate.mu.Unlock()
	m := engineGate.main
	return m.seq != 0 && m.seq == engineGate.seq && m.conv == conv && m.model == model && m.window == window
}

// engineServed note qu'une requête de Loki a été acceptée par le moteur local :
// le slot porte désormais SON état. Un travail annexe qui n'en a obtenu aucune
// (erreur avant l'envoi, moteur qui refuse, arrêt immédiat) n'a pas pris le
// slot — il porte encore la conversation, et l'effacer la ferait recalculer.
func engineServed() {
	engineGate.mu.Lock()
	engineGate.served++
	engineGate.mu.Unlock()
}

func engineServedCount() uint64 {
	engineGate.mu.Lock()
	defer engineGate.mu.Unlock()
	return engineGate.served
}

// sideJobs : travaux annexes en cours. Leurs prompts ne sont pas ceux de la
// conversation : noteEnginePrompt ne les retient ni ne les compare.
var sideJobs atomic.Int32

// engineRequestStart marque une requête vers le moteur local ; la fonction
// rendue la clôt (une seule fois, quel que soit le nombre d'appels), une fois
// le corps de la réponse lu. Un préchauffage en vol (PREWARM, chat_prewarm.go)
// est annulé d'abord : il ne passe jamais devant un vrai travail.
func engineRequestStart() func() { return engineRequestStartKeep(nil) }

// engineRequestStartKeep : engineRequestStart, sauf pour le préchauffage en vol
// que keep reconnaît comme le début exact de cette requête — il continue, et la
// requête le suit dans le même slot sur le préfixe qu'il vient de calculer.
// Décidé sous le verrou du compteur : aucun préchauffage ne peut démarrer entre
// la décision et le compte.
func engineRequestStartKeep(keep func(*prewarmRun) bool) func() {
	end, _ := engineRequestBegin(keep)
	return end
}

// engineRequestBegin : engineRequestStartKeep, avec le numéro de la requête
// (engineMarkMain).
func engineRequestBegin(keep func(*prewarmRun) bool) (func(), uint64) {
	return engineRequestBeginSide(keep, false)
}

// engineRequestBeginSide : engineRequestBegin pour une requête qui part, si
// side, sur le second slot de SIDE_SLOT (id_slot 1). Elle ne touche pas au slot
// de la discussion : le numéro n'avance pas, et engineSlotHolds dit toujours
// vrai après elle. Comptée en vol comme les autres.
func engineRequestBeginSide(keep func(*prewarmRun) bool, side bool) (func(), uint64) {
	engineGate.mu.Lock()
	if p := prewarmCur; p != nil && (keep == nil || !keep(p)) {
		prewarmDropLocked(p)
	}
	engineGate.inflight++
	if !side {
		engineGate.seq++
	}
	seq := engineGate.seq
	engineGate.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			engineGate.mu.Lock()
			engineGate.inflight--
			engineGate.mu.Unlock()
		})
	}, seq
}

// engineProxyBegin : engineRequestBeginSide pour une requête d'un client passée
// par les proxys de Loki (/v1, /health, /props, /metrics, /slots). Comptée en
// vol dans tous les cas, comme avant. Une lecture (GET, HEAD : /v1/models, une
// sonde /health d'Open WebUI…) ne calcule rien dans aucun slot : elle ne doit
// ni annuler un préchauffage (PREWARM), ni dire que le slot ne porte plus la
// discussion (COMPACT_CONTINUATION, SLOT_PERSIST). Sans ces clés, aucune
// différence.
func engineProxyBegin(r *http.Request, side bool) func() {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		end, _ := engineRequestBeginSide(func(*prewarmRun) bool { return true }, true)
		return end
	}
	end, _ := engineRequestBeginSide(nil, side)
	return end
}

// slotsWrite : une action sur /slots (save, restore, erase) plutôt qu'une
// lecture. Les proxys de Loki la refusent : seul Loki, en local, efface — et
// save ou restore écriraient des Gio sur le disque pour un client distant.
func slotsWrite(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/slots") && r.Method != http.MethodGet && r.Method != http.MethodHead
}

// slotIsolator parle au moteur local ; séparé pour les tests.
type slotIsolator struct {
	base   string // http://localhost:<port>
	auth   func(set func(k, v string))
	client *http.Client
}

var reBuildInfo = regexp.MustCompile(`^b(\d+)`)

// get lit un JSON du moteur, borné à 2 s.
func (s slotIsolator) get(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+path, nil)
	if err != nil {
		return err
	}
	s.auth(req.Header.Set)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s : %s", path, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// eraseIfIdle efface le slot 0 si, et seulement si, tous les garde-fous
// passent. erased=false sans erreur : on s'est abstenu, sans dommage.
func (s slotIsolator) eraseIfIdle(ctx context.Context) (erased bool, err error) {
	var props struct {
		BuildInfo  string `json:"build_info"`
		TotalSlots int    `json:"total_slots"`
	}
	if err := s.get(ctx, "/props", &props); err != nil {
		return false, err
	}
	build := 0
	if m := reBuildInfo.FindStringSubmatch(props.BuildInfo); m != nil {
		build, _ = strconv.Atoi(m[1])
	}
	if build < slotEraseMinBuild || props.TotalSlots != 1 {
		return false, nil
	}
	engineGate.mu.Lock()
	defer engineGate.mu.Unlock()
	if engineGate.inflight > 0 {
		return false, nil
	}
	var slots []struct {
		ID           int  `json:"id"`
		IsProcessing bool `json:"is_processing"`
	}
	if err := s.get(ctx, "/slots", &slots); err != nil {
		return false, err
	}
	for _, sl := range slots {
		if sl.ID == 0 && sl.IsProcessing {
			return false, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/slots/0?action=erase", nil)
	if err != nil {
		return false, err
	}
	s.auth(req.Header.Set)
	resp, err := s.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return false, fmt.Errorf("effacement du slot : %s %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return true, nil
}

var slotEraseLogOnce sync.Once

// engineSideJob encadre un travail annexe (sous-agent, vérification, tâche,
// bench) : à appeler AVANT, et la fonction rendue APRÈS — d'habitude
// « defer engineSideJob()() ». Elle efface le slot si c'est sans risque, et
// arme la vérification du cache sur la requête suivante (noteEnginePrompt).
func engineSideJob() func() {
	before, servedAt := lastEnginePrompt.Load(), engineServedCount()
	sideJobs.Add(1)
	return func() {
		sideJobs.Add(-1)
		finishSideJob(before, servedAt, eraseLocalSlot)
	}
}

// finishSideJob : la fin d'un travail annexe, séparée de ses appels au moteur
// pour les tests. Rien n'est effacé si le moteur n'a servi aucune requête de
// Loki depuis le début du travail : le slot porte alors toujours la
// conversation (ou l'état d'un client qu'on ne connaît pas).
func finishSideJob(before int64, servedAt uint64, erase func() (bool, error)) {
	if engineServedCount() == servedAt {
		return
	}
	erased, err := erase()
	if err != nil {
		// Moteur sans --slot-save-path (501), ou /slots coupé : l'erreur est
		// sans conséquence et se répéterait à chaque travail annexe.
		slotEraseLogOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "[cache] isolation des travaux annexes indisponible : %v\n", err)
		})
	}
	if erased && before > 0 {
		slotHintPending.Store(before)
	}
}

// eraseLocalSlot efface le slot 0 du moteur local si le preset actif s'y prête
// et que tous les garde-fous d'eraseIfIdle passent.
func eraseLocalSlot() (bool, error) {
	ep := resolveChatEndpoint()
	if ep.External {
		return false, nil
	}
	cfg := ReadConfig()
	argEnv := map[string]string{}
	for _, k := range cacheArgEnv {
		argEnv[k] = os.Getenv(k)
	}
	if !cacheIsolationOn(cfg) || cacheRAMOff(cfg, splitArgs(cfg["EXTRA_ARGS"]), argEnv) {
		return false, nil
	}
	iso := slotIsolator{base: fmt.Sprintf("http://localhost:%d", LLMPort()), auth: ep.auth, client: http.DefaultClient}
	return iso.eraseIfIdle(context.Background())
}

// Vérification du cache après un effacement. Le moteur dit, dans l'usage de
// chaque réponse, combien de jetons du prompt il a repris du cache
// (prompt_tokens_details.cached_tokens). Si la conversation revient après un
// travail annexe avec bien moins que ce qu'elle avait, son état n'a pas tenu
// dans le cache RAM : c'est CACHE_RAM qu'il faut relever.
var (
	lastEnginePrompt atomic.Int64 // prompt_tokens de la dernière réponse locale
	slotHintPending  atomic.Int64 // prompt d'avant le travail annexe effacé ; 0 = rien à vérifier
	slotHintShown    atomic.Bool  // le conseil n'apparaît qu'une fois par lancement
)

// slotHintMinTokens : en dessous, un recalcul coûte trop peu pour en parler.
const slotHintMinTokens = 4096

// noteEnginePrompt enregistre l'usage d'une réponse du moteur local et rend le
// conseil à afficher, s'il y a lieu (une seule fois par lancement ; le journal,
// lui, le note à chaque fois).
func noteEnginePrompt(total, cached int, hasCached bool) string {
	// Prompt d'un travail annexe : ni la conversation à comparer, ni la taille
	// à retenir pour le prochain (deux vérifications de suite, par exemple).
	if sideJobs.Load() > 0 {
		return ""
	}
	if total > 0 {
		lastEnginePrompt.Store(int64(total))
	}
	before := slotHintPending.Swap(0)
	// Un prompt plus court que celui d'avant n'est pas la suite de la même
	// conversation (nouvelle discussion, autre tâche) : rien à comparer.
	if !hasCached || before < slotHintMinTokens || int64(total) < before || int64(cached) >= before/2 {
		return ""
	}
	fmt.Fprintf(os.Stderr, "[cache] conversation recalculée après un travail annexe : %d jetons repris du cache sur %d\n",
		cached, before)
	if slotHintShown.Swap(true) {
		return ""
	}
	return fmt.Sprintf("Conversation recalculée après un travail annexe (%d jetons repris du cache sur %d) : "+
		"le cache de prompts en RAM est trop petit — augmente CACHE_RAM dans le preset.", cached, before)
}
