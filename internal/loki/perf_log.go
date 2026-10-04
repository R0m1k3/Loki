package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Télémétrie par complétion : combien du prompt venait du cache, combien a été
// recalculé, à quelle vitesse, et ce que le décodage spéculatif a gardé.
//
// Rien de tout ça ne retourne au moteur : aucun champ ajouté aux requêtes, le
// comptage du contexte (usage.prompt_tokens → CtxUsed → compaction) reste
// celui d'avant. On LIT ce que llama.cpp envoie déjà sur son chunk final, on le
// garde en mémoire (anneau borné, aucun texte de message) et on l'expose en
// agrégats sur /api/perf/summary. Sans ces chiffres, impossible de savoir si une
// ligne volatile s'est glissée dans le prompt, si une mise à jour du moteur a
// cassé les points de reprise, ou si l'acceptation du MTP s'est effondrée.

// Nature d'une complétion. Elle voyage dans le contexte (withPerf) et jamais
// dans Caps : Caps est une liste de capacités, recopiée par valeur partout.
const (
	perfMain     = "main"     // tour de la conversation (et ses étapes d'outils)
	perfSubagent = "subagent" // sous-agent du mode Code
	perfVerify   = "verify"   // passe de vérification du mode Code
	perfTask     = "task"     // tâche planifiée
	perfCompact  = "compact"  // résumé de compaction
	perfBench    = "bench"    // mesure du moteur (cache_prompt:false)
	perfForeign  = "foreign"  // client externe passé par /v1 : vu, pas mesuré
	perfPrewarm  = "prewarm"  // préchauffage du cache (PREWARM) : réponse jetée
)

type perfCtxKey struct{}

type perfTag struct{ kind, conv string }

// withPerf étiquette les complétions lancées sous ctx.
func withPerf(ctx context.Context, kind, conv string) context.Context {
	return context.WithValue(ctx, perfCtxKey{}, perfTag{kind: kind, conv: conv})
}

// withPerfKind change la nature en gardant la conversation : un sous-agent ou
// une vérification reste rattaché à la discussion qui l'a lancé.
func withPerfKind(ctx context.Context, kind string) context.Context {
	t := perfTagOf(ctx)
	t.kind = kind
	return context.WithValue(ctx, perfCtxKey{}, t)
}

func perfTagOf(ctx context.Context) perfTag {
	t, _ := ctx.Value(perfCtxKey{}).(perfTag)
	if t.kind == "" {
		t.kind = perfMain
	}
	return t
}

// perfNum : un nombre de télémétrie, décodé sans JAMAIS échouer. Un fork ou une
// API tierce qui l'envoie en flottant, en chaîne ou pas du tout le laisse
// simplement « inconnu » : une valeur d'affichage ne doit pas pouvoir jeter le
// chunk final (texte, finish_reason, usage) ni faire échouer une compaction.
type perfNum struct {
	f  float64
	ok bool
}

func (n *perfNum) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 && f < 1e12 {
		*n = perfNum{f: f, ok: true}
	}
	return nil
}

func (n perfNum) int() int { return int(n.f) }

func (n perfNum) ptr() *int {
	if !n.ok {
		return nil
	}
	v := int(n.f)
	return &v
}

// perfWire : les champs de télémétrie du chunk final, lus dans un DEUXIÈME
// décodage, séparé de streamChunk et dont l'erreur est ignorée. Les compteurs
// cache_n/draft_n n'existent pas sur tous les moteurs (draft_n n'apparaît
// qu'avec un brouillon actif) : absents, ils restent inconnus, jamais 0.
type perfWire struct {
	Timings *struct {
		PromptN         perfNum `json:"prompt_n"`
		PromptMs        perfNum `json:"prompt_ms"`
		PromptPerSecond perfNum `json:"prompt_per_second"`
		PredictedN      perfNum `json:"predicted_n"`
		PredictedMs     perfNum `json:"predicted_ms"`
		PredictedPerSec perfNum `json:"predicted_per_second"`
		CacheN          perfNum `json:"cache_n"`
		DraftN          perfNum `json:"draft_n"`
		DraftNAccepted  perfNum `json:"draft_n_accepted"`
	} `json:"timings"`
	Usage *struct {
		PromptTokens        perfNum `json:"prompt_tokens"`
		CompletionTokens    perfNum `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens perfNum `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

func decodePerfWire(data []byte) perfWire {
	// Decoder plutôt qu'Unmarshal : un octet parasite après la réponse ne doit
	// pas tout rendre inconnu. Ce qui a pu être lu l'est ; le reste reste inconnu.
	var w perfWire
	_ = json.NewDecoder(bytes.NewReader(data)).Decode(&w)
	return w
}

// cachedTokens : usage.prompt_tokens_details.cached_tokens (format OpenAI).
func (w perfWire) cachedTokens() perfNum {
	if w.Usage == nil || w.Usage.PromptTokensDetails == nil {
		return perfNum{}
	}
	return w.Usage.PromptTokensDetails.CachedTokens
}

// applyPerf reporte la télémétrie d'un chunk sur les stats. timings.cache_n a
// la priorité ; cached_tokens ne fait que combler un cache encore inconnu (les
// deux viennent du même compteur côté llama.cpp, mais seul le second existe
// sur une API tierce).
func (s *StatsEvent) applyPerf(w perfWire) {
	if t := w.Timings; t != nil {
		if p := t.CacheN.ptr(); p != nil {
			s.CacheTokens = p
		}
		if p := t.DraftN.ptr(); p != nil {
			s.DraftN = p
		}
		if p := t.DraftNAccepted.ptr(); p != nil {
			s.DraftAccepted = p
		}
	}
	if s.CacheTokens == nil {
		s.CacheTokens = w.cachedTokens().ptr()
	}
}

// perfRec : une complétion. Des identifiants et des compteurs, jamais de texte.
type perfRec struct {
	At       time.Time `json:"at"`
	Seq      uint64    `json:"seq"`
	Kind     string    `json:"kind"`
	Conv     string    `json:"conv,omitempty"`
	Iter     int       `json:"iter"`
	Complete bool      `json:"complete"`
	// Total : usage.prompt_tokens, la taille du prompt entier. New : prompt_n,
	// les jetons réellement recalculés. Cached : cache_n, absent si inconnu.
	Total     int     `json:"total,omitempty"`
	Cached    *int    `json:"cached,omitempty"`
	New       int     `json:"new,omitempty"`
	PPms      float64 `json:"pp_ms,omitempty"`
	PPtps     float64 `json:"pp_tps,omitempty"`
	Gen       int     `json:"gen,omitempty"`
	TGtps     float64 `json:"tg_tps,omitempty"`
	DraftN    *int    `json:"draft_n,omitempty"`
	DraftAcc  *int    `json:"draft_accepted,omitempty"`
	TTFTms    *int64  `json:"ttft_ms,omitempty"`
	Lost      *int    `json:"lost,omitempty"`
	LostAfter string  `json:"lost_after,omitempty"`
}

// perfRecFromStats : la complétion telle que runChat l'a vue.
func perfRecFromStats(t perfTag, iter int, complete bool, s StatsEvent) perfRec {
	return perfRec{
		Kind: t.kind, Conv: t.conv, Iter: iter, Complete: complete,
		Total: s.PromptTokensTotal, Cached: s.CacheTokens, New: s.PromptTokens,
		PPms: s.PromptMs, PPtps: s.PromptPerSecond, Gen: s.GenTokens, TGtps: s.GenPerSecond,
		DraftN: s.DraftN, DraftAcc: s.DraftAccepted, TTFTms: s.TTFTms,
	}
}

// perfRecFromWire : une réponse NON streamée (compaction), lue d'un bloc.
func perfRecFromWire(t perfTag, w perfWire) perfRec {
	r := perfRec{Kind: t.kind, Conv: t.conv}
	if tm := w.Timings; tm != nil {
		r.New, r.PPms, r.PPtps = tm.PromptN.int(), tm.PromptMs.f, tm.PromptPerSecond.f
		r.Gen, r.TGtps = tm.PredictedN.int(), tm.PredictedPerSec.f
		r.Cached, r.DraftN, r.DraftAcc = tm.CacheN.ptr(), tm.DraftN.ptr(), tm.DraftNAccepted.ptr()
	}
	if u := w.Usage; u != nil {
		r.Total = u.PromptTokens.int()
		if r.Gen == 0 {
			r.Gen = u.CompletionTokens.int()
		}
	}
	if r.Cached == nil {
		r.Cached = w.cachedTokens().ptr()
	}
	r.Complete = r.Total > 0 || w.Timings != nil
	return r
}

// perfPrefix : empreintes cumulées des messages envoyés (l'empreinte i couvre
// les messages 0..i). C'est ce qui dit qu'une requête PROLONGE la précédente
// de la même conversation — l'identifiant seul ne suffit pas : une compaction,
// une édition ou une autre tâche dans la même discussion repartent d'ailleurs.
func perfPrefix(msgs []Message) []uint64 {
	h := fnv.New64a()
	out := make([]uint64, len(msgs))
	for i, m := range msgs {
		b, _ := json.Marshal(m)
		h.Write(b)
		h.Write([]byte{0})
		out[i] = h.Sum64()
	}
	return out
}

const (
	perfRingMax = 5000 // ~1 Mio en mémoire au pire
	perfConvMax = 128  // conversations suivies pour le calcul de la perte
	perfBetween = 500  // entrées remontées au plus pour nommer ce qui s'est intercalé
)

// perfConvState : la dernière complétion terminée d'une conversation (et d'une
// nature : le fil d'un sous-agent n'est pas celui de son builder).
type perfConvState struct {
	seq   uint64
	n     int
	hash  uint64
	total int
}

type perfStore struct {
	mu    sync.Mutex
	seq   uint64
	ring  []perfRec
	head  int // prochaine case à écraser quand l'anneau est plein
	convs map[string]perfConvState
}

var perfLog = &perfStore{}

// record range la complétion et calcule sa perte de cache :
//
//	lost = max(0, total précédent − cache_n)
//
// seulement quand le cache est connu, que la complétion est allée au bout, et
// que ses messages PROLONGENT strictement ceux de la précédente complétion de
// la même conversation et de la même nature. Ailleurs (nouvelle discussion,
// compaction, flux coupé) la perte reste inconnue plutôt que fausse.
func (p *perfStore) record(r perfRec, prefix []uint64) perfRec {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	r.Seq = p.seq
	if r.At.IsZero() {
		r.At = time.Now()
	}
	key := r.Kind + "\x00" + r.Conv
	if p.convs == nil {
		p.convs = map[string]perfConvState{}
	}
	if r.Complete && len(prefix) > 0 {
		prev, had := p.convs[key]
		if had && r.Cached != nil && prev.total > 0 && prev.n > 0 && len(prefix) > prev.n && prefix[prev.n-1] == prev.hash {
			lost := max(0, prev.total-*r.Cached)
			r.Lost = &lost
			if lost > 0 {
				r.LostAfter = p.between(prev.seq, key)
			}
		}
		if r.Total > 0 {
			p.convs[key] = perfConvState{seq: r.Seq, n: len(prefix), hash: prefix[len(prefix)-1], total: r.Total}
			p.evictConvs()
		}
	}
	if len(p.ring) < perfRingMax {
		p.ring = append(p.ring, r)
	} else {
		p.ring[p.head] = r
		p.head = (p.head + 1) % perfRingMax
	}
	return r
}

// between nomme les natures des complétions passées par le moteur depuis seq
// (un sous-agent, un client externe…) : la cause probable d'une perte. Vide =
// rien vu d'autre, perte non attribuée.
func (p *perfStore) between(seq uint64, key string) string {
	seen := map[string]bool{}
	n := len(p.ring)
	// De la plus récente à la plus ancienne. Tant que l'anneau n'est pas plein,
	// head vaut 0 et la plus récente est la dernière case : même formule.
	for i := 0; i < n && i < perfBetween; i++ {
		e := p.ring[((p.head-1-i)%n+n)%n]
		if e.Seq <= seq {
			break
		}
		if e.Kind+"\x00"+e.Conv != key {
			seen[e.Kind] = true
		}
	}
	kinds := make([]string, 0, len(seen))
	for k := range seen {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return strings.Join(kinds, ",")
}

// evictConvs borne l'état par conversation : on oublie la plus ancienne.
func (p *perfStore) evictConvs() {
	for len(p.convs) > perfConvMax {
		oldK, oldS := "", uint64(math.MaxUint64)
		for k, s := range p.convs {
			if s.seq < oldS {
				oldK, oldS = k, s.seq
			}
		}
		delete(p.convs, oldK)
	}
}

// snapshot : copie de l'anneau, de la plus ancienne à la plus récente.
func (p *perfStore) snapshot() []perfRec {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]perfRec, 0, len(p.ring))
	out = append(out, p.ring[p.head:]...)
	return append(out, p.ring[:p.head]...)
}

// perfRecord range une complétion et, si LOKI_PERF_LOG est posée, l'écrit sur
// stderr. Une ligne par complétion, c'est des dizaines par tour d'agent : pas
// par défaut, les journaux Docker ont déjà celles de llama-server.
func perfRecord(r perfRec, prefix []uint64) perfRec {
	r = perfLog.record(r, prefix)
	if perfLogOn() {
		fmt.Fprintln(os.Stderr, perfLine(r))
	}
	return r
}

// perfNoteForeign : une requête d'un client externe vient de passer par /v1.
// Ses chiffres ne sont pas lus (le flux est relayé tel quel), mais sa trace
// suffit à expliquer la perte de cache du tour suivant.
func perfNoteForeign() {
	perfRecord(perfRec{Kind: perfForeign}, nil)
}

func perfLogOn() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LOKI_PERF_LOG")))
	return v != "" && v != "0" && v != "false" && v != "off" && v != "no"
}

func perfLine(r perfRec) string {
	opt := func(p *int) string {
		if p == nil {
			return "?"
		}
		return strconv.Itoa(*p)
	}
	draft := "?"
	if r.DraftN != nil {
		draft = opt(r.DraftAcc) + "/" + strconv.Itoa(*r.DraftN)
	}
	ttft := "?"
	if r.TTFTms != nil {
		ttft = strconv.FormatInt(*r.TTFTms, 10)
	}
	lost := opt(r.Lost)
	if r.LostAfter != "" {
		lost += "(" + r.LostAfter + ")"
	}
	conv := r.Conv
	if conv == "" {
		conv = "-"
	}
	return fmt.Sprintf("[perf] kind=%s conv=%s iter=%d total=%d cached=%s new=%d pp_ms=%.0f pp_tps=%.1f gen=%d tg_tps=%.1f draft=%s ttft_ms=%s lost=%s complete=%t",
		r.Kind, conv, r.Iter, r.Total, opt(r.Cached), r.New, r.PPms, r.PPtps, r.Gen, r.TGtps, draft, ttft, lost, r.Complete)
}

// perfLostAlertAt : la perte au-delà de laquelle l'interface la signale. Sur un
// modèle hybride (couches récurrentes), le cache ne reprend qu'à un point de
// reprise : perdre jusqu'à un espacement de points (--checkpoint-min-step,
// 2048 posé par Loki) plus un micro-batch est l'état NORMAL, pas un incident.
// Le signaler à chaque étape noierait les vraies pertes.
//
// Même ordre de priorité que le lancement (ckptArgs) : un -cms d'EXTRA_ARGS ou
// LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT l'emporte sur CKPT_MIN_STEP, un -ub
// d'EXTRA_ARGS sur UBATCH.
func perfLostAlertAt(cfg map[string]string) int {
	const base = 1000
	extra := splitArgs(cfg["EXTRA_ARGS"])
	num := func(vals ...string) int {
		for _, v := range vals {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				return n
			}
		}
		return 0
	}
	step := num(flagValue(extra, "-cms", "--checkpoint-min-step"),
		os.Getenv("LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT"), cfg["CKPT_MIN_STEP"])
	if m := strings.TrimSpace(cfg["MODEL"]); step == 0 && m != "" {
		if p, err := resolveServeModelPath(m); err == nil {
			if g, err := ggufMeta(p); err == nil && ggufHybrid(&g) {
				step = 2048
			}
		}
	}
	if step == 0 {
		return base
	}
	ub := num(flagValue(extra, "-ub", "--ubatch-size"), cfg["UBATCH"])
	if ub == 0 {
		ub = 512
	}
	return max(base, step+ub)
}

// perfAlert : la complétion mérite d'être signalée — une perte au-delà du seuil,
// ou un gros prompt repris de zéro alors que le cache est connu.
func perfAlert(r perfRec, threshold int) bool {
	if r.Lost != nil && *r.Lost > threshold {
		return true
	}
	return r.Cached != nil && *r.Cached == 0 && r.Total >= 8*threshold
}

// --- /api/perf/summary -------------------------------------------------------

type perfKindSum struct {
	Completions int `json:"completions"`
	Incomplete  int `json:"incomplete"`
	// Turns : premières complétions (iter 0) — un tour utilisateur, un
	// sous-agent, une passe de vérification…
	Turns             int      `json:"turns"`
	NewTokens         int      `json:"new_tokens"`
	PrefillSec        float64  `json:"prefill_sec"`
	NewTokensPerTurn  float64  `json:"new_tokens_per_turn,omitempty"`
	PrefillSecPerTurn float64  `json:"prefill_sec_per_turn,omitempty"`
	LostEvents        int      `json:"lost_events"`
	LostTokens        int      `json:"lost_tokens"`
	TTFTMedianMs      *float64 `json:"ttft_median_ms,omitempty"`
}

type perfDepthSum struct {
	Bucket      string   `json:"bucket"`
	N           int      `json:"n"`
	PPMedianTPS *float64 `json:"pp_median_tps,omitempty"`
	TGMedianTPS *float64 `json:"tg_median_tps,omitempty"`
}

type perfSummary struct {
	Entries int       `json:"entries"`
	Since   time.Time `json:"since,omitzero"`
	// CacheHitRatio : Σ cached / Σ total sur les complétions où les deux sont
	// connus. Absent = moteur qui ne dit rien de son cache.
	CacheHitRatio *float64                `json:"cache_hit_ratio,omitempty"`
	Kinds         map[string]*perfKindSum `json:"kinds"`
	Depth         []perfDepthSum          `json:"depth"`
	DraftN        int                     `json:"draft_n"`
	DraftAccepted int                     `json:"draft_accepted"`
	DraftRate     *float64                `json:"draft_rate,omitempty"`
	// Template : dernier verdict de la sonde de gabarit (chat_tplprobe.go),
	// absent tant qu'aucune n'a tourné ou sur un preset externe.
	Template *tplProbeResult `json:"template,omitempty"`
}

var perfDepthBuckets = []struct {
	name string
	max  int
}{{"0-8k", 8 << 10}, {"8-16k", 16 << 10}, {"16-32k", 32 << 10}, {"32k+", math.MaxInt}}

func perfMedian(v []float64) *float64 {
	if len(v) == 0 {
		return nil
	}
	sort.Float64s(v)
	m := v[len(v)/2]
	if len(v)%2 == 0 {
		m = (v[len(v)/2-1] + v[len(v)/2]) / 2
	}
	return &m
}

func perfSummarize(recs []perfRec) perfSummary {
	s := perfSummary{Entries: len(recs), Kinds: map[string]*perfKindSum{}}
	if len(recs) > 0 {
		s.Since = recs[0].At
	}
	var sumCached, sumTotal int
	pp := make([][]float64, len(perfDepthBuckets))
	tg := make([][]float64, len(perfDepthBuckets))
	ttft := map[string][]float64{}
	for _, r := range recs {
		k := s.Kinds[r.Kind]
		if k == nil {
			k = &perfKindSum{}
			s.Kinds[r.Kind] = k
		}
		if !r.Complete {
			k.Incomplete++
			continue
		}
		k.Completions++
		if r.Iter == 0 {
			k.Turns++
		}
		k.NewTokens += r.New
		k.PrefillSec += r.PPms / 1000
		if r.Lost != nil && *r.Lost > 0 {
			k.LostEvents++
			k.LostTokens += *r.Lost
		}
		if r.TTFTms != nil {
			ttft[r.Kind] = append(ttft[r.Kind], float64(*r.TTFTms))
		}
		if r.Cached != nil && r.Total > 0 {
			sumCached += min(*r.Cached, r.Total)
			sumTotal += r.Total
		}
		depth := r.Total
		if depth == 0 {
			depth = r.New
		}
		for i, b := range perfDepthBuckets {
			if depth < b.max {
				// Débit de lecture : seulement sur un vrai prefill (quelques jetons
				// nouveaux donnent des débits sans signification).
				if r.PPtps > 0 && r.New >= 64 {
					pp[i] = append(pp[i], r.PPtps)
				}
				if r.TGtps > 0 && r.Gen > 1 {
					tg[i] = append(tg[i], r.TGtps)
				}
				break
			}
		}
		if r.DraftN != nil {
			s.DraftN += *r.DraftN
			if r.DraftAcc != nil {
				s.DraftAccepted += *r.DraftAcc
			}
		}
	}
	for kind, k := range s.Kinds {
		if k.Turns > 0 {
			k.NewTokensPerTurn = float64(k.NewTokens) / float64(k.Turns)
			k.PrefillSecPerTurn = k.PrefillSec / float64(k.Turns)
		}
		k.TTFTMedianMs = perfMedian(ttft[kind])
	}
	if sumTotal > 0 {
		r := float64(sumCached) / float64(sumTotal)
		s.CacheHitRatio = &r
	}
	if s.DraftN > 0 {
		r := float64(s.DraftAccepted) / float64(s.DraftN)
		s.DraftRate = &r
	}
	for i, b := range perfDepthBuckets {
		s.Depth = append(s.Depth, perfDepthSum{Bucket: b.name, N: max(len(pp[i]), len(tg[i])),
			PPMedianTPS: perfMedian(pp[i]), TGMedianTPS: perfMedian(tg[i])})
	}
	return s
}

// handlePerfSummary : GET /api/perf/summary (derrière la clé de pilotage).
// Des agrégats seulement : aucun texte de message n'est jamais retenu.
func handlePerfSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET seulement"})
		return
	}
	s := perfSummarize(perfLog.snapshot())
	if r, ok := tplCapsCurrent(); ok {
		s.Template = &r
	}
	sendJSON(w, http.StatusOK, s)
}
