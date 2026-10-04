package loki

// chat_prewarm.go — PREWARM : préparer le prochain tour pendant que
// l'utilisateur lit.
//
// Pourquoi. Certains recalculs sont inévitables : le prompt réécrit par une
// compaction (12 à 20k jetons), le dernier message assistant rendu autrement
// au tour suivant (réflexion retirée par le gabarit, raisonnement jamais
// renvoyé), la discussion reprise après une tâche planifiée qui a pris le slot.
// Sans la clé, ils tombent sur le premier jeton du message suivant : des
// secondes sur un 27B, des dizaines sur un MoE aux experts en RAM.
//
// Avec la clé (off par défaut), Loki envoie au moteur local, dès qu'il est
// libre, la requête du prochain tour telle qu'elle partira — assemblée par
// les MÊMES fonctions (turnViewDry, wireMessages, buildChatPayload), contenu
// vivant, rien de figé ni de mémorisé pour gagner du cache — suivie d'un
// message utilisateur sentinelle « . », avec max_tokens 1. Le moteur calcule
// tout le préfixe et pose un point de reprise au début de ce dernier message :
// le vrai tour ne calcule plus que son propre message. Le jeton produit et la
// sentinelle sont jetés ; rien n'est persisté, ni dans la discussion, ni dans
// le journal, ni dans les compteurs (CtxUsed, stats, effort appris).
//
// Ce que le modèle voit ne change pas : llama-server ne reprend un état que sur
// un préfixe de jetons identique, et la vraie requête est construite à l'envoi,
// comme toujours. Au pire, le préchauffage ne sert à rien.
//
// Il ne passe jamais devant un vrai travail :
//   - moteur local seulement, un seul slot (/props total_slots = 1 : ni
//     PARALLEL ni -np d'EXTRA_ARGS ne peuvent le contredire) — ou les deux de
//     SIDE_SLOT en service, et il vise alors le slot de la discussion
//     (id_slot 0, llm_sideslot.go) ;
//   - jamais pendant un tour, une tâche, une compaction, un bench, un travail
//     annexe, ni s'il reste une requête de Loki en vol (compteur de
//     llm_slots.go, qu'il tient lui-même : l'isolation des slots n'efface rien
//     pendant qu'il tourne) ;
//   - toute requête de Loki vers le moteur l'annule à l'instant de partir —
//     sauf un tour de chat dont la requête PROLONGE exactement le préfixe
//     préchauffé (empreintes des messages sérialisés, des outils et des
//     arguments du gabarit) : elle suit alors dans le même slot et réutilise
//     ce qui vient d'être calculé, sans rien attendre d'inutile ;
//   - un seul en vol ; un nouveau déclencheur annule l'ancien ;
//   - aucune erreur ne remonte : 400 (contexte plein), 503 (chargement), 500
//     (gabarit) sont lâchés, sans compaction, sans relance, sans rien apprendre.
//
// Déclencheurs : fin d'un tour (compaction de fin de tour comprise) quand rien
// n'attend dans la file, fin d'une tâche planifiée (slot déjà effacé, projet
// forcé déjà levé). PREWARM=full ajoute le changement de discussion, après 3 s
// sans nouveau changement. Les capacités sont celles du dernier tour de la
// discussion, gardées en mémoire ; en mode Code, le rôle est celui d'un message
// ordinaire (builder) — une demande de plan diverge et annule.
//
// Limites, sans effet sur la fidélité : un moteur sans point de reprise au
// début des messages utilisateur, ou des images juste avant (le moteur n'en
// pose pas après un bloc d'image), retombe sur un recalcul complet et exact ;
// changer agent, web ou mémoire avant d'envoyer rend le préchauffage inutile.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// prewarmMode : « on », « full » ou "" (off). Jamais pour un preset externe.
func prewarmMode(cfg map[string]string) string {
	if isExternalConfig(cfg) {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(cfg["PREWARM"])) {
	case "on", "1", "true", "yes", "oui":
		return "on"
	case "full":
		return "full"
	}
	return ""
}

// Déclencheurs (pour le journal).
const (
	prewarmTurnEnd   = "fin-tour"
	prewarmAfterTask = "après-tâche"
	prewarmSwitch    = "discussion"
)

// prewarmSentinel : le message utilisateur qui tient la place du vrai.
const prewarmSentinel = "."

// prewarmRun : un préchauffage en vol. prefix couvre ses n premiers messages
// (sans la sentinelle), opts les champs qui façonnent le rendu.
type prewarmRun struct {
	n      int
	prefix uint64
	opts   uint64
	cancel context.CancelFunc
	done   chan struct{}
}

// prewarmCur : le préchauffage en vol, protégé par engineGate.mu (llm_slots.go).
// prewarmLive le reflète sans verrou : sans préchauffage, une requête ne
// calcule aucune empreinte.
var (
	prewarmCur  *prewarmRun
	prewarmLive atomic.Bool
)

// prewarmDropLocked annule p, engineGate.mu tenu. Le client ferme la
// connexion, et llama-server arrête la tâche (au plus un micro-lot plus tard).
func prewarmDropLocked(p *prewarmRun) {
	if prewarmCur == p {
		prewarmCur = nil
		prewarmLive.Store(false)
	}
	p.cancel()
}

// prewarmBegin inscrit p comme LA requête en vol, si le moteur est libre de
// toute autre requête de Loki. end le retire (une seule fois).
func prewarmBegin(p *prewarmRun) (end func(), ok bool) {
	engineGate.mu.Lock()
	defer engineGate.mu.Unlock()
	if engineGate.inflight > 0 || prewarmCur != nil {
		return nil, false
	}
	prewarmCur = p
	prewarmLive.Store(true)
	engineGate.inflight++
	engineGate.seq++ // le slot ne porte plus le tour d'avant (engineSlotHolds)
	var once sync.Once
	return func() {
		once.Do(func() {
			engineGate.mu.Lock()
			engineGate.inflight--
			if prewarmCur == p {
				prewarmCur = nil
				prewarmLive.Store(false)
			}
			engineGate.mu.Unlock()
		})
	}, true
}

// prewarmStop annule le préchauffage en vol et rend celui-ci (nil sinon), pour
// qu'on puisse attendre sa fin.
func prewarmStop() *prewarmRun {
	// Cas de toujours : rien en vol, et pas de verrou à prendre (l'effacement
	// d'un slot peut le tenir deux secondes).
	if !prewarmLive.Load() {
		return nil
	}
	engineGate.mu.Lock()
	defer engineGate.mu.Unlock()
	p := prewarmCur
	if p != nil {
		prewarmDropLocked(p)
	}
	return p
}

// matches : la requête dont prefix est l'empreinte cumulée des messages
// (perfPrefix) prolonge-t-elle exactement ce préchauffage ?
func (p *prewarmRun) matches(prefix []uint64, opts uint64) bool {
	return p.n > 0 && len(prefix) > p.n && prefix[p.n-1] == p.prefix && opts == p.opts
}

// prewarmOptsKeys : les champs du corps, hors messages, qui changent le prompt
// rendu par le gabarit. L'échantillonnage n'y est pas : il ne touche pas au
// prompt.
var prewarmOptsKeys = []string{"tools", "parallel_tool_calls", "tool_choice", "chat_template_kwargs", "reasoning_effort"}

func prewarmOptsHash(payload map[string]any) uint64 {
	h := fnv.New64a()
	for _, k := range prewarmOptsKeys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		if v, ok := payload[k]; ok {
			b, _ := json.Marshal(v)
			h.Write(b)
		}
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// prewarmKeeper : pour runChat, ce qui reconnaît un préchauffage à garder. nil
// (tout annuler) hors tour de chat, ou sans préchauffage en vol — dans ce cas,
// le cas de toujours, rien n'est calculé.
func prewarmKeeper(kind string, sent []Message, payload map[string]any) func(*prewarmRun) bool {
	if kind != perfMain || !prewarmLive.Load() {
		return nil
	}
	prefix, opts := perfPrefix(sent), prewarmOptsHash(payload)
	return func(p *prewarmRun) bool { return p.matches(prefix, opts) }
}

// Capacités du dernier tour de chaque discussion, en mémoire seulement : le
// client les envoie avec chaque message, le serveur ne les connaît pas sinon.
var prewarmCaps struct {
	mu sync.Mutex
	m  map[string]Caps
}

const prewarmCapsMax = 64

func prewarmNoteCaps(convID string, caps Caps) {
	if convID == "" {
		return
	}
	prewarmCaps.mu.Lock()
	defer prewarmCaps.mu.Unlock()
	if prewarmCaps.m == nil {
		prewarmCaps.m = map[string]Caps{}
	}
	if _, ok := prewarmCaps.m[convID]; !ok && len(prewarmCaps.m) >= prewarmCapsMax {
		for k := range prewarmCaps.m {
			delete(prewarmCaps.m, k)
			break
		}
	}
	prewarmCaps.m[convID] = caps
}

func prewarmCapsFor(convID string) (Caps, bool) {
	prewarmCaps.mu.Lock()
	defer prewarmCaps.mu.Unlock()
	c, ok := prewarmCaps.m[convID]
	return c, ok
}

// prewarmNearMidnight : la date du jour fait partie du prompt système ; à
// moins de 10 min de minuit, le prochain message a toutes les chances de
// partir avec la suivante.
func prewarmNearMidnight(now time.Time) bool {
	y, m, d := now.Date()
	next := time.Date(y, m, d+1, 0, 0, 0, 0, now.Location())
	return next.Sub(now) < 10*time.Minute
}

// Points d'accroche remplaçables par les tests.
var (
	// prewarmAsync : false = le préchauffage tourne dans l'appelant (tests).
	prewarmAsync = true
	// prewarmSlots : nombre de slots du moteur local (/props total_slots).
	prewarmSlots = func(ep chatEndpoint) int {
		var props struct {
			TotalSlots int `json:"total_slots"`
		}
		iso := slotIsolator{base: fmt.Sprintf("http://localhost:%d", LLMPort()), auth: ep.auth, client: http.DefaultClient}
		if err := iso.get(context.Background(), "/props", &props); err != nil {
			return 0
		}
		return props.TotalSlots
	}
	prewarmSwitchDelay = 3 * time.Second
	// prewarmTimeout : borne d'un préchauffage (un MoE aux experts en RAM met
	// des minutes sur 20k jetons). Toute vraie requête l'annule bien avant.
	prewarmTimeout = 15 * time.Minute
)

// prewarmKick : un déclencheur. Sans la clé, rien — ni goroutine, ni requête.
func (c *Conversation) prewarmKick(why string) {
	if prewarmMode(ReadConfig()) == "" {
		return
	}
	if prewarmAsync {
		go c.prewarm(why)
		return
	}
	c.prewarm(why)
}

// prewarmAfterSwitch : changement de discussion. Le préchauffage de l'ancienne
// est annulé ; avec PREWARM=full, la nouvelle est préparée si l'utilisateur y
// reste 3 s (cliquer d'une discussion à l'autre ne lance rien).
func prewarmAfterSwitch() {
	prewarmStop() // sa fin n'est pas attendue : le changement ne doit pas bloquer
	if prewarmMode(ReadConfig()) != "full" {
		return
	}
	id := convActiveID()
	time.AfterFunc(prewarmSwitchDelay, func() {
		if convActiveID() == id {
			conv.prewarmKick(prewarmSwitch)
		}
	})
}

// prewarmBodyFor construit le corps du préchauffage de c, ou "" avec la raison
// de s'abstenir. Lecture seule : rien n'est modifié dans la discussion.
func (c *Conversation) prewarmBodyFor(ep chatEndpoint, cfg map[string]string) (payload map[string]any, run *prewarmRun, skip string) {
	caps, ok := prewarmCapsFor(convActiveID())
	if !ok {
		return nil, nil, "capacités du dernier tour inconnues"
	}
	caps = withTurnRole(caps, prewarmSentinel)

	c.mu.Lock()
	if c.Generating {
		c.mu.Unlock()
		return nil, nil, "moteur occupé"
	}
	msgs := append([]Message(nil), c.Messages...)
	ctxNow := c.ctxNowLocked(msgs, true)
	peak := c.genPeak
	c.mu.Unlock()

	hasUser := false
	for _, m := range msgs {
		if m.Role == "user" {
			hasUser = true
			break
		}
	}
	// Sans message utilisateur dans l'historique, le contexte projet irait en
	// tête de la sentinelle (normalizeSystemMessages) : rien à préparer.
	if !hasUser {
		return nil, nil, "discussion vide"
	}
	msgs = append(msgs, Message{Role: "user", Content: prewarmSentinel})
	// Le prochain tour compactera d'abord : son prompt ne sera pas celui-ci.
	if compactNeeded(msgs, ctxNow, peak) {
		return nil, nil, "compaction attendue au prochain tour"
	}

	sent, tools := c.turnViewDry(caps, msgs, caps.Agent)
	_, effort, _, kwargs := turnReasoning(cfg)
	wire, _ := wireMessages(sent, false, turnEchoPolicy(false))
	n := len(wire)
	if n < 2 || wire[n-1].Role != "user" {
		return nil, nil, "assemblage inattendu"
	}
	if s, _ := wire[n-1].Content.(string); s != prewarmSentinel {
		return nil, nil, "assemblage inattendu"
	}
	payload = buildChatPayload(ep, wire, 0.7, chatPayloadOpts{tools: tools, effort: effort, kwargs: kwargs})
	// Seuls les champs de réponse changent : une réponse d'un seul jeton, non
	// streamée (jetée de toute façon).
	payload["stream"] = false
	delete(payload, "stream_options")
	payload["max_tokens"] = 1
	return payload, &prewarmRun{n: n - 1, prefix: perfPrefix(wire)[n-2], opts: prewarmOptsHash(payload)}, ""
}

var prewarmLogged sync.Map // raison d'échec → déjà journalisée

func prewarmLogOnce(key, format string, args ...any) {
	if _, dup := prewarmLogged.LoadOrStore(key, true); dup {
		return
	}
	fmt.Fprintf(os.Stderr, "[prewarm] "+format+"\n", args...)
}

// prewarm : un préchauffage complet, ou rien. Jamais d'erreur rendue.
func (c *Conversation) prewarm(why string) {
	cfg := ReadConfig()
	if prewarmMode(cfg) == "" {
		return
	}
	ep := resolveChatEndpoint()
	if ep.External {
		return
	}
	// Un seul en vol : le nouveau déclencheur remplace l'ancien.
	if p := prewarmStop(); p != nil {
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			return
		}
	}
	if sideJobs.Load() > 0 || benchRunning() || prewarmNearMidnight(time.Now()) {
		return
	}
	if !healthCheck() {
		return
	}
	// Un seul slot, ou les deux de SIDE_SLOT : le préchauffage vise alors le
	// slot de la discussion (id_slot 0), celui où partira le vrai tour.
	slot := -1
	if n := prewarmSlots(ep); n != 1 {
		if slot = engineSlotFor(ep, true); n != 2 || slot != 0 {
			return
		}
	}
	payload, run, skip := c.prewarmBodyFor(ep, cfg)
	if skip != "" {
		return
	}
	setEngineSlot(payload, slot)
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	// SLOT_PERSIST : l'état gardé de la discussion d'abord, s'il y en a un — le
	// préchauffage le prolonge au lieu de tout recalculer.
	slotPersistRestore(ep, convActiveID())
	ctx, cancel := context.WithTimeout(context.Background(), prewarmTimeout)
	defer cancel()
	run.cancel, run.done = cancel, make(chan struct{})
	defer close(run.done)
	end, ok := prewarmBegin(run)
	if !ok {
		return // une requête de Loki est partie entre-temps
	}
	defer end()
	ctx = withPerf(ctx, perfPrewarm, convActiveID())

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	ep.auth(req.Header.Set)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			prewarmLogOnce("net", "moteur injoignable (%s) : %v", why, err)
		}
		return
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return
	}
	if resp.StatusCode != http.StatusOK {
		prewarmLogOnce(resp.Status, "refusé par le moteur (%s) : %s %s", why, resp.Status, strings.TrimSpace(string(raw[:min(len(raw), 200)])))
		return
	}
	// Télémétrie seulement (kind=prewarm) : la réponse est jetée.
	rec := perfRecord(perfRecFromWire(perfTagOf(ctx), decodePerfWire(raw)), nil)
	// Après une tâche, c'est lui qui retrouve (ou non) la conversation dans le
	// cache RAM : la vérification de llm_slots.go se fait donc ici — le vrai
	// tour, servi depuis le slot préchauffé, n'aurait plus rien à en dire. Le
	// conseil va au journal (aucun fil où l'afficher).
	if rec.Total > 0 {
		cached := 0
		if rec.Cached != nil {
			cached = *rec.Cached
		}
		noteEnginePrompt(rec.Total, cached, rec.Cached != nil)
	}
}
