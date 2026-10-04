package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Le benchmark mesure le moteur LOCAL tel que le chat s'en sert. Deux modes :
//
//   - « rapide » (le bouton par défaut) : un préchauffage jeté, puis la ligne
//     courte historique — ~2000 jetons de prompt, ~300 générés — sur
//     /v1/chat/completions, avec le gabarit, le raisonnement et l'échantillonnage
//     du preset. Une seule mesure à froid, à faible profondeur ;
//   - « complet » : la ligne courte, puis une discussion à PROFONDEUR réelle —
//     un prefill à froid de D jetons, et trois tours qui ajoutent chacun ~2048
//     jetons neufs à la même discussion. On y lit le prefill à froid à D, le
//     prefill des tours suivants (cache repris) et le decode à D : ce que la
//     ligne courte ne voit pas, et ce qui décide vraiment du confort à 30k.
//
// Tout passe par l'endpoint du chat, avec une vraie structure de messages
// (utilisateur → assistant, contenu seul, comme le gabarit le reconstruit →
// utilisateur) : la reprise du cache est LUE dans la réponse (cache_n), jamais
// supposée. Un modèle hybride ne reprend qu'aux points de contrôle, un gabarit
// qui retire le raisonnement de l'historique réécrit la fin du préfixe : la
// mesure le montre au lieu de promettre le meilleur cas.
//
// Honnêteté de la mesure : une réponse non-200 ou sans timings fait échouer la
// phase — rien n'est inventé, rien n'est enregistré. Un compteur absent
// (cache_n, draft_n : selon le build) reste « n/a ». Rien ici ne touche au
// modèle : seed fixe et cache coupé ne valent que pour ces requêtes-là.

// benchProtocol : version du protocole, enregistrée avec chaque mesure. Deux
// protocoles ne se comparent pas (la v1 tuilait un petit corpus et inventait un
// partage 15/85 du temps quand les timings manquaient).
const benchProtocol = 2

const (
	benchModeQuick = "quick"
	benchModeFull  = "full"

	benchTurns        = 3     // tours ajoutés à la discussion en profondeur
	benchAppendTokens = 2048  // jetons NEUFS par tour
	benchGenTokens    = 256   // jetons générés par tour (arrêt sur EOS permis)
	benchDepthMax     = 32768 // profondeur plafond
	benchDepthCPU     = 16384 // plafond quand des poids tournent sur CPU (MoE déporté)
	benchDepthMin     = 4096  // en dessous, la profondeur n'apprend rien : phase sautée
	benchMargin       = 512   // gabarit, questions, consignes : la marge du contexte
	benchWarmGen      = 32    // jetons générés au préchauffage
	benchSeed         = 1234
	// benchReuseSlack : jetons de fin de préfixe que le gabarit peut légitimement
	// réécrire d'un tour à l'autre (invite de génération, balise de réflexion).
	benchReuseSlack = 16
	// benchSampleBytes : échantillon tokenisé pour estimer les octets par jeton.
	benchSampleBytes = 160 << 10
	// benchFallbackRatio : octets par jeton sans /tokenize. Plus bas que la
	// réalité du code comme de la prose : des morceaux plus COURTS que visé,
	// la mesure tient toujours dans le contexte.
	benchFallbackRatio = 2.5
	// benchThrashBytes : relu du disque par 256 jetons générés au-delà duquel
	// on évoque un modèle qui ne tient pas en RAM. Seuil non calibré : simple
	// indication, jamais enregistrée.
	benchThrashBytes = 512 << 20
)

// benchReqTimeout : budget d'une requête. Un prefill à froid de 16k jetons
// avec les experts sur CPU prend une à deux minutes ; au-delà de ce budget la
// phase est déclarée partielle, le reste du bench garde ses mesures.
var benchReqTimeout = 6 * time.Minute

var errBenchNoTimings = errors.New("le moteur n'a pas renvoyé de mesures (timings) : rien à enregistrer")

// benchDraft : acceptation du brouillon (MTP, n-gram, modèle d'ébauche), lue
// dans timings.draft_n / draft_n_accepted. nil = le moteur ne le dit pas.
type benchDraft struct {
	N        int `json:"n"`
	Accepted int `json:"accepted"`
}

func (d *benchDraft) add(o *benchDraft) *benchDraft {
	if o == nil {
		return d
	}
	if d == nil {
		d = &benchDraft{}
	}
	d.N += o.N
	d.Accepted += o.Accepted
	return d
}

// benchTurn : une requête de la phase en profondeur.
type benchTurn struct {
	New             int         `json:"new"`      // jetons du prompt réellement calculés (prompt_n)
	Cached          int         `json:"cached"`   // repris du cache ; -1 = le moteur ne le dit pas
	Expected        int         `json:"expected"` // prompt du tour précédent : ce qui POUVAIT être repris
	PromptMs        float64     `json:"prompt_ms"`
	PromptPerSecond float64     `json:"prompt_per_second"`
	PredictedN      int         `json:"predicted_n"`
	PredictedMs     float64     `json:"predicted_ms"`
	PredictedPerSec float64     `json:"predicted_per_second"`
	Draft           *benchDraft `json:"draft,omitempty"`
}

// benchDepth : la phase en profondeur du mode complet.
type benchDepth struct {
	Ctx     int         `json:"ctx"`
	Target  int         `json:"target"`            // profondeur visée (jetons)
	Skipped string      `json:"skipped,omitempty"` // phase sautée, et pourquoi
	Cold    *benchTurn  `json:"cold,omitempty"`
	Turns   []benchTurn `json:"turns,omitempty"`
	// Agrégats des tours : prefill des jetons neufs (cache repris) et decode à D.
	CachedPerSec float64 `json:"cached_per_second,omitempty"`
	DecodePerSec float64 `json:"decode_per_second,omitempty"`
	// Reuse : part du prompt précédent reprise du cache ; -1 = inconnue.
	Reuse     float64     `json:"reuse"`
	ReuseNote string      `json:"reuse_note,omitempty"`
	Draft     *benchDraft `json:"draft,omitempty"` // sortie en code
	Partial   string      `json:"partial,omitempty"`
	// Hint : indication « possible thrash » (Linux), jamais enregistrée.
	Hint string `json:"hint,omitempty"`
}

// benchResult : les premiers champs sont ceux de la ligne courte, inchangés
// depuis la v1 — l'interface et les pastilles des presets les lisent.
type benchResult struct {
	PromptN         int     `json:"prompt_n"`
	PromptMs        float64 `json:"prompt_ms"`
	PromptPerSecond float64 `json:"prompt_per_second"`
	PredictedN      int     `json:"predicted_n"`
	PredictedMs     float64 `json:"predicted_ms"`
	PredictedPerSec float64 `json:"predicted_per_second"`
	Elapsed         float64 `json:"elapsed_sec"`

	Mode     string      `json:"mode,omitempty"`
	Protocol int         `json:"protocol,omitempty"`
	Draft    *benchDraft `json:"draft,omitempty"` // ligne courte, sortie en prose
	// KV : types de cache effectifs. Quantifié = zone grise : la mesure ne se
	// compare pas à une référence sans perte.
	KV          string      `json:"kv,omitempty"`
	KVQuantized bool        `json:"kv_quantized,omitempty"`
	Engine      string      `json:"engine,omitempty"` // build_info du moteur
	GPUs        string      `json:"gpus,omitempty"`   // CUDA_VISIBLE_DEVICES effectif
	Depth       *benchDepth `json:"depth,omitempty"`
}

// benchOpts : ce que demande l'appelant.
type benchOpts struct {
	Mode    string
	Prompt  int // jetons de la ligne courte
	Predict int // jetons générés par la ligne courte
}

func (o benchOpts) full() bool { return o.Mode == benchModeFull }

// benchProgress annonce la phase en cours (step sur steps). nil : silence.
type benchProgress func(phase string, step, steps int)

// benchCorpora : prose pour la ligne courte, code pour la profondeur. Le code
// est l'interface web embarquée — 650 Ko de HTML/CSS/JS réels, sans répétition :
// une discussion profonde ne relit jamais deux fois le même passage, un
// brouillon n-gram n'y trouve pas de copie toute faite.
type benchCorpora struct {
	prose, code string
}

func defaultBenchCorpora() benchCorpora {
	b, _ := uiFS.ReadFile("ui/index.html")
	return benchCorpora{prose: benchCorpus, code: string(b)}
}

// benchCorpus : un passage varié (prose française et anglaise, sujets sans
// lien) pour la ligne courte. Il n'est plus tuilé : un texte répété gonfle le
// decode, chaque jeton proposé par un brouillon (MTP, n-gram) étant accepté —
// rien à voir avec un vrai chat. Au-delà de sa taille, le prompt se complète
// avec du code inédit (benchRun).
const benchCorpus = `In the early hours of an October morning, Camille walked along the canal, watching the cargo barges slip past the iron bridge that spanned the water. She thought about the meeting she had skipped, the unanswered messages on her phone, the way the city always seemed to forget her name after summer ended. Three streets away, a pâtisserie opened its shutters and the smell of warm butter mixed with diesel exhaust from the waiting bus.
Pendant ce temps, à Marseille, un chercheur en biologie marine prépare son matériel pour une plongée. Il étudie les herbiers de posidonie, ces prairies sous-marines vieilles de plusieurs milliers d'années qui stockent autant de carbone qu'une forêt amazonienne. Le bateau quitte le port à six heures vingt-trois.
Quantum computers, properly engineered, can solve certain classes of problems exponentially faster than classical machines. The catch is that decoherence ruins everything. Engineers use dilution refrigerators to drop superconducting qubits to fifteen millikelvin, colder than deep space. The wires connecting the chip to room-temperature electronics must dissipate almost no heat, or the qubit state collapses before any useful computation finishes.
Le boulanger lève la pâte à quatre heures. Il regarde la balance numérique en plissant les yeux : six cent vingt-trois grammes, presque le compte. Son chien dort sur le tapis de farine près du four. Dehors, deux chats se disputent un poisson abandonné par le pêcheur de nuit.
Consider a recursive descent parser written in Go. The lexer emits tokens; the parser consumes them and produces an abstract syntax tree. Error recovery is hard: after a syntax error, the parser must resynchronize at a known boundary—a semicolon, a closing brace—without losing track of subsequent diagnostics. Tree-sitter solves this with incremental parsing and a glr-like algorithm.
Le philosophe stoïcien disait : "Ce qui nous trouble, ce n'est pas ce qui nous arrive, mais l'opinion que nous nous en faisons." Vingt siècles plus tard, la phrase apparaît dans un livre de poche au rayon développement personnel d'une librairie d'aéroport, à côté d'un roman policier suédois.
Mitochondria descended from ancient bacteria engulfed by archaeal cells roughly two billion years ago. They still keep their own ring of DNA, separate from the nuclear genome. Mutations in mitochondrial DNA accumulate with age and have been implicated in everything from Parkinson's disease to ordinary muscle fatigue. Yet they remain stubbornly difficult to repair therapeutically because each cell contains hundreds.
La marée descend lentement, exposant des rochers couverts d'huîtres et d'algues vertes. Un héron immobile surveille les flaques laissées par l'eau. Plus loin, deux enfants courent avec un cerf-volant rouge qui refuse de monter à cause de l'humidité dans la voile.
Compilers translate high-level languages into machine code through several intermediate representations. LLVM IR sits in the middle: typed, mostly static-single-assignment, suitable for both aggressive optimization and direct lowering to x86 or ARM. The optimizer runs dozens of passes—dead code elimination, loop-invariant code motion, induction variable simplification—each touching the IR in carefully ordered ways.
Le cuisinier ferme les yeux pour goûter la sauce. Trop salée. Il ajoute une pomme de terre crue coupée en quartiers, sachant qu'elle absorbera l'excès en mijotant vingt minutes. Sa grand-mère lui a appris ce geste un dimanche de novembre il y a très longtemps.
`

// --- le moteur, vu du bench ---------------------------------------------------

// benchEngine parle au moteur local ; séparé pour les tests (faux moteur).
type benchEngine struct {
	base   string // http://localhost:<port>
	auth   func(set func(k, v string))
	client *http.Client
}

// do envoie une requête et rend le corps d'une réponse 200. Toute autre réponse
// est une erreur : la v1 décodait un 500 comme une mesure.
func (e benchEngine) do(ctx context.Context, method, path string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.base+path, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if e.auth != nil {
		e.auth(req.Header.Set)
	}
	// Requête en vol jusqu'à la lecture complète du corps : l'isolation des
	// travaux annexes n'efface jamais le slot pendant ce temps (llm_slots.go).
	defer engineRequestStart()()
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, &benchHTTPError{method: method, path: path, status: resp.Status, body: string(data)}
	}
	return data, err
}

// benchHTTPError : une réponse non-200 du moteur. Le corps est gardé en entier :
// le refus d'un niveau de raisonnement cite les niveaux acceptés en FIN de
// message (llm_effort.go), bien au-delà de l'extrait affiché.
type benchHTTPError struct {
	method, path, status, body string
}

func (e *benchHTTPError) Error() string {
	snip := strings.TrimSpace(e.body)
	if len(snip) > 300 {
		cut := 300
		for cut > 0 && !utf8.RuneStart(snip[cut]) {
			cut-- // jamais au milieu d'un caractère
		}
		snip = snip[:cut]
	}
	return fmt.Sprintf("%s %s : %s %s", e.method, e.path, e.status, snip)
}

// idle : le moteur est-il libre ? Un slot qui travaille, c'est une requête
// d'un autre (client /v1, autre processus loki) : le bench passerait derrière
// elle et mesurerait l'attente. /slots coupé (501) ou absent : on ne sait pas,
// on n'empêche rien.
func (e benchEngine) idle(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	data, err := e.do(ctx, http.MethodGet, "/slots", nil)
	if err != nil {
		return nil
	}
	var slots []struct {
		IsProcessing bool `json:"is_processing"`
	}
	if json.Unmarshal(data, &slots) != nil {
		return nil
	}
	for _, s := range slots {
		if s.IsProcessing {
			return errors.New("le moteur traite déjà une requête : relance le bench une fois libre")
		}
	}
	return nil
}

// props lit la taille de contexte et le build du moteur. Absents : 0 et "".
func (e benchEngine) props(ctx context.Context) (nCtx int, build string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	data, err := e.do(ctx, http.MethodGet, "/props", nil)
	if err != nil {
		return 0, ""
	}
	var p struct {
		BuildInfo string `json:"build_info"`
		NCtx      int    `json:"n_ctx"`
		Default   struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if json.Unmarshal(data, &p) != nil {
		return 0, ""
	}
	nCtx = p.Default.NCtx
	if nCtx <= 0 {
		nCtx = p.NCtx
	}
	return nCtx, p.BuildInfo
}

// bytesPerToken : octets par jeton d'un échantillon, mesurés par /tokenize.
func (e benchEngine) bytesPerToken(ctx context.Context, sample string) float64 {
	if sample == "" {
		return benchFallbackRatio
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := e.do(ctx, http.MethodPost, "/tokenize", map[string]any{"content": sample})
	if err != nil {
		return benchFallbackRatio
	}
	var out struct {
		Tokens []json.RawMessage `json:"tokens"`
	}
	if json.Unmarshal(data, &out) != nil || len(out.Tokens) == 0 {
		return benchFallbackRatio
	}
	return float64(len(sample)) / float64(len(out.Tokens))
}

// benchReply : une réponse du chat. total : usage.prompt_tokens (0 = inconnu).
type benchReply struct {
	turn    benchTurn
	total   int
	content string
}

// chat envoie une complétion et en lit les timings. Sans timings, pas de
// mesure : la v1 inventait un partage 15/85 du temps écoulé, enregistré ensuite
// comme s'il avait été mesuré.
func (e benchEngine) chat(ctx context.Context, payload map[string]any) (benchReply, error) {
	ctx, cancel := context.WithTimeout(ctx, benchReqTimeout)
	defer cancel()
	data, err := e.do(ctx, http.MethodPost, "/v1/chat/completions", payload)
	if err != nil {
		return benchReply{}, err
	}
	engineServed() // le slot porte désormais le bench : l'effacer a un sens
	w := decodePerfWire(data)
	t := w.Timings
	if t == nil || !t.PromptN.ok || !t.PromptMs.ok || !t.PredictedN.ok || !t.PredictedMs.ok {
		return benchReply{}, errBenchNoTimings
	}
	rate := func(n, ms, given perfNum) float64 {
		if given.ok && given.f > 0 {
			return given.f
		}
		if ms.f > 0 {
			return n.f / (ms.f / 1000)
		}
		return 0
	}
	r := benchReply{turn: benchTurn{
		New: t.PromptN.int(), Cached: -1,
		PromptMs: t.PromptMs.f, PromptPerSecond: rate(t.PromptN, t.PromptMs, t.PromptPerSecond),
		PredictedN: t.PredictedN.int(), PredictedMs: t.PredictedMs.f,
		PredictedPerSec: rate(t.PredictedN, t.PredictedMs, t.PredictedPerSec),
	}}
	if p := t.CacheN.ptr(); p != nil {
		r.turn.Cached = *p
	} else if p := w.cachedTokens().ptr(); p != nil {
		r.turn.Cached = *p
	}
	if n, a := t.DraftN.ptr(), t.DraftNAccepted.ptr(); n != nil && a != nil && *n > 0 {
		r.turn.Draft = &benchDraft{N: *n, Accepted: *a}
	}
	if w.Usage != nil && w.Usage.PromptTokens.ok {
		r.total = w.Usage.PromptTokens.int()
	}
	var msg struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &msg) == nil && len(msg.Choices) > 0 {
		r.content = msg.Choices[0].Message.Content
	}
	return r, nil
}

// promptTotal : taille du prompt entier de ce tour.
func (r benchReply) promptTotal() int {
	if r.total > 0 {
		return r.total
	}
	return r.turn.New + max(r.turn.Cached, 0)
}

// --- réglages du preset --------------------------------------------------------

// benchArgEnv : les LLAMA_ARG_* qui décident, sans drapeau, des types de cache
// et du placement des poids.
var benchArgEnv = []string{"LLAMA_ARG_CACHE_TYPE_K", "LLAMA_ARG_CACHE_TYPE_V",
	"LLAMA_ARG_OVERRIDE_TENSOR", "LLAMA_ARG_CPU_MOE", "LLAMA_ARG_N_CPU_MOE", "LLAMA_ARG_N_CPU_FFN"}

// benchSetup : ce que le bench tire du preset, une fois.
type benchSetup struct {
	cfg       map[string]string
	want      string // niveau de raisonnement demandé par le preset
	off       bool   // raisonnement interdit
	effort    string // niveau envoyé (want, ou la traduction apprise)
	kwargs    map[string]any
	retried   bool                   // refus du niveau déjà rejoué une fois
	learn     func(want, got string) // retient la traduction (nil : tests)
	ub        int
	cpuPlaced bool // poids sur CPU (-ot, --n-cpu-moe…) : profondeur plafonnée
	hybrid    bool // modèle hybride : reprise du cache aux points de contrôle
	kv        string
	kvQuant   bool
	gpus      string
}

func benchSetupFrom(cfg map[string]string, argEnv map[string]string) benchSetup {
	extra := splitArgs(cfg["EXTRA_ARGS"])
	// Le raisonnement tel que le chat l'envoie (runChatTools) : mesurer sans lui
	// serait mesurer un autre usage que le sien.
	want := reasoningEffortValue(cfg["REASONING_EFFORT"])
	effort := effortResolve(want)
	off := reasoningExplicitlyOff(cfg["REASONING"]) || want == "none"
	s := benchSetup{cfg: cfg, want: want, off: off, effort: effort, kwargs: reasoningTemplateKwargs(off, effort), ub: 512,
		gpus: strings.TrimSpace(cfg["CUDA_VISIBLE_DEVICES"])}
	for _, v := range []string{flagValue(extra, "-ub", "--ubatch-size"), cfg["UBATCH"]} {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			s.ub = n
			break
		}
	}
	s.cpuPlaced = tensorOverride(extra, argEnv) != ""
	k, v, _ := effectiveKVTypes(cfg, extra, argEnv)
	s.kv, s.kvQuant = benchKVLabel(k, v)
	if m := strings.TrimSpace(cfg["MODEL"]); m != "" {
		if p, err := resolveServeModelPath(m); err == nil {
			if g, err := ggufMeta(p); err == nil {
				s.hybrid = ggufHybrid(&g)
			}
		}
	}
	return s
}

// benchKVLabel nomme les types de cache (« f16 », « q8_0/f16 »…) et dit s'ils
// sont quantifiés — rang 2 et plus de kvFidelity : bf16 n'en est pas.
func benchKVLabel(k, v string) (string, bool) {
	norm := func(s string) string {
		if s = strings.ToLower(strings.TrimSpace(s)); s == "" {
			return "f16"
		}
		return s
	}
	k, v = norm(k), norm(v)
	rk, _ := kvFidelity(k)
	rv, _ := kvFidelity(v)
	label := k
	if k != v {
		label = k + "/" + v
	}
	return label, max(rk, rv) >= 2
}

// payload : une requête de chat du bench. Échantillonnage et raisonnement du
// preset, comme le chat ; seed fixe pour que deux passages se comparent.
// cache : seuls les tours qui PROLONGENT la discussion en profondeur reprennent
// le cache — le reste mesure un prefill réellement calculé.
func (s benchSetup) payload(msgs []Message, maxTokens int, cache bool) map[string]any {
	p := map[string]any{
		"model":        "loki",
		"messages":     msgs,
		"max_tokens":   maxTokens,
		"stream":       false,
		"temperature":  0.7,
		"seed":         benchSeed,
		"cache_prompt": cache,
	}
	applySamplingFrom(p, s.cfg)
	if s.effort != "" {
		p["reasoning_effort"] = s.effort
	}
	if s.kwargs != nil {
		p["chat_template_kwargs"] = s.kwargs
	}
	return p
}

// ask envoie une requête de chat du bench. Un gabarit qui REFUSE le niveau de
// raisonnement du preset (Qwen3.8 ne connaît pas « high ») est traité comme
// dans le chat (runChatTools) : on rejoue une fois avec le niveau qu'il accepte,
// ou sans le champ, et la traduction est retenue pour ce modèle. Sans ça, le
// bench d'un processus neuf échouait sur un 500 là où le chat, lui, répond.
func (s *benchSetup) ask(ctx context.Context, e benchEngine, msgs []Message, maxTokens int, cache bool) (benchReply, error) {
	r, err := e.chat(ctx, s.payload(msgs, maxTokens, cache))
	var he *benchHTTPError
	if err == nil || s.retried || s.want == "" || !errors.As(err, &he) {
		return r, err
	}
	fixed, ok := effortFromRejection(he.body, s.want)
	if !ok {
		return r, err
	}
	s.retried = true
	if s.learn != nil {
		s.learn(s.want, fixed)
	}
	s.effort, s.kwargs = fixed, reasoningTemplateKwargs(s.off, fixed)
	return e.chat(ctx, s.payload(msgs, maxTokens, cache))
}

// benchDepthFor choisit la profondeur : la moitié du contexte, plafonnée, et
// assez basse pour que les tours ajoutés y tiennent encore (préchauffage compris,
// par prudence). Trop petit : la phase est sautée, pas ratée.
func benchDepthFor(nCtx int, cpuPlaced bool, warm int) (int, string) {
	if nCtx <= 0 {
		return 0, "taille de contexte inconnue"
	}
	d := min(nCtx/2, benchDepthMax)
	if cpuPlaced {
		d = min(d, benchDepthCPU)
	}
	d = min(d, nCtx-benchTurns*(benchAppendTokens+benchGenTokens)-benchGenTokens-warm-benchMargin)
	if d < benchDepthMin {
		return 0, fmt.Sprintf("contexte de %d jetons trop petit pour la mesure en profondeur", nCtx)
	}
	return d, ""
}

// benchSlice coupe dans text, à partir de off, environ n octets arrêtés à une
// fin de ligne — jamais au milieu d'un caractère. "" : corpus épuisé.
func benchSlice(text string, off, n int) (string, int) {
	if off >= len(text) || n <= 0 {
		return "", off
	}
	end := off + n
	if end >= len(text) {
		return text[off:], len(text)
	}
	if i := strings.LastIndexByte(text[off:end], '\n'); i > n/2 {
		end = off + i + 1
	}
	for end > off && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[off:end], end
}

// benchLineStart : le début de la ligne qui suit off (off s'il en commence une).
func benchLineStart(text string, off int) int {
	if off <= 0 {
		return 0
	}
	if off >= len(text) {
		return len(text)
	}
	if text[off-1] == '\n' {
		return off
	}
	if i := strings.IndexByte(text[off:], '\n'); i >= 0 {
		return off + i + 1
	}
	return len(text)
}

// Consignes au modèle. La ligne courte demande de la prose, la profondeur du
// code : l'acceptation du brouillon diffère de l'un à l'autre, on mesure les deux.
const (
	benchAskProse = "\n\nContinue this passage with another 1000+ words of original varied prose, mixing French and English narrative paragraphs on different topics."
	benchAskFirst = "Résume en quelques phrases le rôle de ce fichier."
	benchAskNext  = "Écris en JavaScript une fonction courte inspirée de cette partie, sans explication."
)

// --- le protocole ----------------------------------------------------------------

// benchRun déroule le protocole contre un moteur. Erreur = une phase de mesure
// a échoué : rien n'est à enregistrer. Une phase en profondeur interrompue par
// son budget ou par la taille du contexte rend un résultat PARTIEL, dit tel.
func benchRun(ctx context.Context, e benchEngine, opts benchOpts, s benchSetup, corp benchCorpora, progress benchProgress) (*benchResult, error) {
	if opts.Prompt <= 0 {
		opts.Prompt = 2000
	}
	if opts.Predict <= 0 {
		opts.Predict = 300
	}
	if err := e.idle(ctx); err != nil {
		return nil, err
	}
	steps, step := 2, 0
	if opts.full() {
		steps += 1 + benchTurns
	}
	next := func(phase string) {
		step++
		if progress != nil {
			progress(phase, step, steps)
		}
	}
	t0 := time.Now()
	nCtx, build := e.props(ctx)
	if nCtx <= 0 {
		nCtx, _ = strconv.Atoi(strings.TrimSpace(s.cfg["CTX"]))
	}
	mode := benchModeQuick
	if opts.full() {
		mode = benchModeFull
	}
	res := &benchResult{Mode: mode, Protocol: benchProtocol, KV: s.kv, KVQuantized: s.kvQuant, Engine: build, GPUs: s.gpus}

	sample, _ := benchSlice(corp.code, 0, benchSampleBytes)
	codeRatio := e.bytesPerToken(ctx, sample)
	proseRatio := e.bytesPerToken(ctx, corp.prose)

	// 1. Préchauffage, jeté : premières allocations, graphes CUDA, pages du
	// modèle — sans lui, la première mesure paie l'installation. Pris en fin de
	// corpus, loin de ce que relira la profondeur. Deux ubatchs, mais jamais plus
	// du quart du contexte : un preset à -ub 4096 sur 8k de contexte enverrait
	// sinon un prompt plus grand que la fenêtre, et le bench échouerait là.
	next("préchauffage")
	warmTok := 2 * s.ub
	if nCtx > 0 {
		warmTok = max(min(warmTok, nCtx/4), 64)
	}
	warm := warmTok + benchWarmGen
	warmText, _ := benchSlice(corp.code, benchLineStart(corp.code, len(corp.code)-int(float64(warmTok)*codeRatio)), len(corp.code))
	if _, err := s.ask(ctx, e, []Message{{Role: "user", Content: warmText + "\n\n" + benchAskFirst}}, benchWarmGen, false); err != nil {
		return nil, fmt.Errorf("préchauffage : %w", err)
	}

	// 2. La ligne courte, celle des versions précédentes : de la prose, puis du
	// code inédit pour atteindre la taille visée — plus de corpus tuilé, qu'un
	// brouillon n-gram recopiait.
	next(fmt.Sprintf("ligne courte (%d + %d jetons)", opts.Prompt, opts.Predict))
	prompt := corp.prose
	if want := int(float64(opts.Prompt) * proseRatio); want < len(prompt) {
		prompt, _ = benchSlice(prompt, 0, want)
	} else if rest := opts.Prompt - int(float64(len(prompt))/proseRatio); rest > 0 {
		chunk, _ := benchSlice(corp.code, benchLineStart(corp.code, len(corp.code)/2), int(float64(rest)*codeRatio))
		prompt += "\n\n" + chunk
	}
	quick, err := s.ask(ctx, e, []Message{{Role: "user", Content: strings.TrimSpace(prompt) + benchAskProse}}, opts.Predict, false)
	if err != nil {
		return nil, fmt.Errorf("ligne courte : %w", err)
	}
	q := quick.turn
	res.PromptN, res.PromptMs, res.PromptPerSecond = q.New, q.PromptMs, q.PromptPerSecond
	res.PredictedN, res.PredictedMs, res.PredictedPerSec = q.PredictedN, q.PredictedMs, q.PredictedPerSec
	res.Draft = q.Draft
	if !opts.full() {
		res.Elapsed = time.Since(t0).Seconds()
		return res, nil
	}

	// 3. La profondeur.
	d, why := benchDepthFor(nCtx, s.cpuPlaced, warm)
	depth := &benchDepth{Ctx: nCtx, Target: d, Reuse: -1}
	res.Depth = depth
	if why != "" {
		depth.Skipped = why
		res.Elapsed = time.Since(t0).Seconds()
		return res, nil
	}
	if err := benchDepthRun(ctx, e, &s, corp.code, codeRatio, depth, next); err != nil {
		return nil, err
	}
	res.Elapsed = time.Since(t0).Seconds()
	return res, nil
}

// benchTimedOut : la requête a dépassé SON budget, le bench n'a pas été annulé.
func benchTimedOut(ctx context.Context, err error) bool {
	return errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil
}

// benchDepthRun : prefill à froid à D, puis benchTurns tours qui prolongent la
// même discussion avec du code jamais vu.
func benchDepthRun(ctx context.Context, e benchEngine, s *benchSetup, code string, ratio float64, depth *benchDepth, next func(string)) error {
	next(fmt.Sprintf("prefill à froid (%d jetons)", depth.Target))
	prefix, off := benchSlice(code, 0, int(float64(depth.Target)*ratio))
	msgs := []Message{{Role: "user", Content: "Voici un fichier source de l'interface web de Loki :\n\n```html\n" + prefix + "\n```\n\n" + benchAskFirst}}
	cold, err := s.ask(ctx, e, msgs, benchGenTokens, false)
	if benchTimedOut(ctx, err) {
		depth.Partial = fmt.Sprintf("prefill à froid interrompu après %s", benchReqTimeout)
		return nil
	}
	if err != nil {
		return fmt.Errorf("prefill à froid : %w", err)
	}
	ct := cold.turn
	depth.Cold = &ct
	prev, answer := cold.promptTotal(), cold.content
	// Relectures du disque comptées sur les TOURS seulement (decode à D) : le
	// prefill à froid lit légitimement des pages du modèle, il fausserait
	// l'indication.
	io0, rss0, ioOK := engineIOSample()
	var newN, gen int
	var promptMs, genMs float64
	for r := 1; r <= benchTurns; r++ {
		// Le tour suivant doit tenir : prompt précédent + sa réponse + les jetons
		// neufs (estimés large) + la génération et la marge. Sinon on s'arrête —
		// une mesure qui déborde déclencherait une erreur, ou pire un décalage.
		if prev+2*benchGenTokens+benchAppendTokens*5/4+benchMargin > depth.Ctx {
			depth.Partial = fmt.Sprintf("contexte plein après %d tour(s)", r-1)
			break
		}
		chunk, nextOff := benchSlice(code, off, int(float64(benchAppendTokens)*ratio))
		if chunk == "" {
			depth.Partial = fmt.Sprintf("corpus épuisé après %d tour(s)", r-1)
			break
		}
		off = nextOff
		next(fmt.Sprintf("tour %d/%d en profondeur", r, benchTurns))
		msgs = append(msgs,
			Message{Role: "assistant", Content: answer},
			Message{Role: "user", Content: "Suite du fichier :\n\n```html\n" + chunk + "\n```\n\n" + benchAskNext})
		rep, err := s.ask(ctx, e, msgs, benchGenTokens, true)
		if benchTimedOut(ctx, err) {
			depth.Partial = fmt.Sprintf("tour %d interrompu après %s", r, benchReqTimeout)
			break
		}
		if err != nil {
			return fmt.Errorf("tour %d en profondeur : %w", r, err)
		}
		t := rep.turn
		t.Expected = prev
		depth.Turns = append(depth.Turns, t)
		depth.Draft = depth.Draft.add(t.Draft)
		newN += t.New
		promptMs += t.PromptMs
		gen += t.PredictedN
		genMs += t.PredictedMs
		prev, answer = rep.promptTotal(), rep.content
	}
	if promptMs > 0 {
		depth.CachedPerSec = float64(newN) / (promptMs / 1000)
	}
	if genMs > 0 {
		depth.DecodePerSec = float64(gen) / (genMs / 1000)
	}
	depth.Reuse, depth.ReuseNote = benchReuse(depth.Turns, s.hybrid)
	if io1, rss1, ok := engineIOSample(); ok && ioOK && gen > 0 {
		depth.Hint = benchThrashHint(io1-io0, gen, max(rss0, rss1))
	}
	return nil
}

// benchReuse : la part du prompt précédent reprise du cache, et ce qu'elle veut
// dire. Un constat, jamais un échec : un hybride reprend légitimement au
// dernier point de contrôle.
func benchReuse(turns []benchTurn, hybrid bool) (float64, string) {
	var cached, expected int
	full := true
	for _, t := range turns {
		if t.Cached < 0 || t.Expected <= 0 {
			continue
		}
		cached += min(t.Cached, t.Expected)
		expected += t.Expected
		if t.Cached < t.Expected-benchReuseSlack {
			full = false
		}
	}
	if expected == 0 {
		return -1, "le moteur ne dit pas combien de jetons il reprend du cache"
	}
	ratio := float64(cached) / float64(expected)
	switch {
	case full:
		return ratio, ""
	case hybrid:
		return ratio, "reprise partielle : modèle hybride, le cache ne reprend qu'aux points de contrôle"
	default:
		return ratio, "reprise partielle : le gabarit réécrit sans doute la fin de l'historique (raisonnement retiré)"
	}
}

// benchThrashHint : un moteur qui relit beaucoup le disque pendant les tours
// n'a pas le modèle en RAM. Indication seulement (seuil non calibré).
func benchThrashHint(read int64, gen int, rss int64) string {
	if read <= 0 || gen <= 0 {
		return ""
	}
	per := read * 256 / int64(gen)
	if per <= benchThrashBytes {
		return ""
	}
	return fmt.Sprintf("possible thrash : %.1f Go relus du disque par 256 jetons générés (RSS %.1f Go) — le modèle ne tient sans doute pas en RAM",
		float64(per)/(1<<30), float64(rss)/(1<<30))
}

// --- entrée --------------------------------------------------------------------

// runBench mesure le moteur local et enregistre le résultat s'il est complet.
// L'appelant tient le verrou de génération (benchStart) : aucun tour de chat
// ni tâche ne s'intercale.
func runBench(ctx context.Context, opts benchOpts, progress benchProgress) (*benchResult, error) {
	// Un bench mesure le moteur LOCAL : sur un preset externe, healthCheck dit
	// « prêt » sans moteur, et la mesure taperait un port arrêté ou un moteur
	// resté en vie — attribuée à tort à ce preset.
	if externalActive() {
		return nil, fmt.Errorf("benchmark indisponible : le preset actif est une API externe")
	}
	port := LLMPort()
	if !healthCheck() {
		return nil, fmt.Errorf("serveur injoignable sur :%d", port)
	}
	cfg := ReadConfig()
	argEnv := map[string]string{}
	for _, k := range benchArgEnv {
		if v := os.Getenv(k); v != "" {
			argEnv[k] = v
		}
	}
	setup := benchSetupFrom(cfg, argEnv)
	setup.learn = func(want, got string) {
		effortRemember(want, got)
		logEffortFallback(want, got)
	}
	eng := benchEngine{base: fmt.Sprintf("http://localhost:%d", port), auth: resolveChatEndpoint().auth, client: http.DefaultClient}
	// Le bench prend le slot de la conversation : une fois fini (erreur et
	// annulation comprises), on l'efface si c'est sans risque, pour que son état
	// de 30k jetons n'évince pas la conversation du cache RAM (llm_slots.go).
	defer engineSideJob()()
	res, err := benchRun(ctx, eng, opts, setup, defaultBenchCorpora(), progress)
	if err != nil {
		return nil, err
	}
	// Trace dans la télémétrie : le bench a pris le slot, la perte de cache du
	// tour suivant lui revient (perf_log.go). Cache inconnu : cache_prompt:false.
	perfRecord(perfRec{Kind: perfBench, Complete: true, Total: res.PromptN, New: res.PromptN,
		PPms: res.PromptMs, PPtps: res.PromptPerSecond, Gen: res.PredictedN, TGtps: res.PredictedPerSec}, nil)
	if benchSavable(res) {
		saveLastBench(res, cfg)
		saveBenchForActivePreset(res, cfg)
	}
	return res, nil
}

// benchSavable : seul un résultat COMPLET est enregistré — une phase partielle
// se montre, mais ne s'affiche pas plus tard comme la mesure de ce preset.
func benchSavable(res *benchResult) bool {
	return res != nil && (res.Depth == nil || res.Depth.Partial == "")
}

// --- enregistrement --------------------------------------------------------------

// savedBench is a benchResult plus the model it was run against and a timestamp.
// Fingerprint : configuration + cartes visées + protocole (benchFingerprint).
// Vide sur les mesures d'avant la v2, qui retombent sur le nom du modèle.
type savedBench struct {
	Result      benchResult `json:"result"`
	Model       string      `json:"model"`
	At          int64       `json:"at"`
	Fingerprint string      `json:"fingerprint,omitempty"`
}

// benchFingerprint : l'empreinte de configuration (configFingerprint) ignore
// les clés « appareil », dont CUDA_VISIBLE_DEVICES — or l'ensemble et l'ordre
// des cartes pèsent plus que tout sur une machine à deux GPU. On l'y remet :
// celle du preset s'il l'impose, sinon celle de la machine (cur), que la
// bascule lui appliquerait (softPreservedKeys). Le build du moteur est
// enregistré à côté et affiché, sans entrer ici : le lire pour chaque preset
// de la liste lancerait le binaire.
func benchFingerprint(cfg, cur map[string]string) string {
	gpus := strings.TrimSpace(cfg["CUDA_VISIBLE_DEVICES"])
	if gpus == "" {
		gpus = strings.TrimSpace(cur["CUDA_VISIBLE_DEVICES"])
	}
	return configFingerprint(map[string]string{
		"config":   configFingerprint(cfg),
		"gpus":     gpus,
		"protocol": strconv.Itoa(benchProtocol),
	})
}

func newSavedBench(res *benchResult, cfg map[string]string) savedBench {
	r := *res
	if r.Depth != nil {
		d := *r.Depth
		d.Hint = "" // indication du moment, pas une mesure
		r.Depth = &d
	}
	return savedBench{Result: r, Model: filepath.Base(cfg["MODEL"]), At: time.Now().Unix(),
		Fingerprint: benchFingerprint(cfg, cfg)}
}

// saveLastBench enregistre le dernier benchmark (best-effort) pour que l'UI
// puisse l'afficher sans le relancer.
func saveLastBench(res *benchResult, cfg map[string]string) {
	_ = putJSON(bkState, "last_bench", newSavedBench(res, cfg))
}

// loadLastBench relit le benchmark enregistré, ou nil s'il n'y en a pas.
func loadLastBench() *savedBench {
	var sb savedBench
	if !getJSON(bkState, "last_bench", &sb) {
		return nil
	}
	return &sb
}

// benchMatchesPreset : le bench a-t-il été mesuré sur la configuration ACTUELLE
// du preset ? Les benchs sont rangés par id de preset : sans ce contrôle, un
// preset dont le modèle (ou les cartes, le contexte, EXTRA_ARGS…) a changé
// affichait les mesures d'une autre configuration. Une mesure d'avant
// l'empreinte retombe sur le nom du modèle (repris d'AJEAN 0.16.3), pour que
// les pastilles existantes ne disparaissent pas.
func benchMatchesPreset(sb savedBench, cfg, cur map[string]string) bool {
	if sb.Fingerprint != "" {
		return sb.Fingerprint == benchFingerprint(cfg, cur)
	}
	m := strings.TrimSpace(cfg["MODEL"])
	return m != "" && sb.Model == filepath.Base(m)
}

// deletePresetBench oublie le bench d'un preset supprimé.
func deletePresetBench(id string) {
	m := loadBenchStore()
	if _, ok := m[id]; ok {
		delete(m, id)
		_ = putJSON(bkState, "bench_presets", m)
	}
}

// loadBenchStore renvoie les benchmarks par preset (vide s'il n'y en a pas).
func loadBenchStore() map[string]savedBench {
	m := map[string]savedBench{}
	getJSON(bkState, "bench_presets", &m)
	if m == nil {
		m = map[string]savedBench{}
	}
	return m
}

// saveBenchForActivePreset records res under the name of the currently active
// preset (celui qui correspond à la configuration active). Sans effet si aucun
// preset ne correspond — le benchmark reste enregistré par saveLastBench.
func saveBenchForActivePreset(res *benchResult, cfg map[string]string) {
	list, err := ListPresets()
	if err != nil {
		return
	}
	id := ""
	for _, p := range list {
		if p.Active {
			id = p.ID
			break
		}
	}
	if id == "" {
		return
	}
	m := loadBenchStore()
	m[id] = newSavedBench(res, cfg)
	_ = putJSON(bkState, "bench_presets", m)
}

// --- ligne de commande -------------------------------------------------------------

// cmdBench : « loki bench [N] [PROMPT] [--full] ». Même protocole que
// l'interface. Le verrou de génération est celui de CE processus : face à un
// service web qui tourne, seule la vérification /slots (moteur occupé) protège
// la mesure — à lancer quand rien d'autre ne tourne.
func cmdBench(args []string) error {
	opts := benchOpts{Mode: benchModeQuick, Predict: 300, Prompt: 2000}
	var pos []string
	for _, a := range args {
		if a == "--full" {
			opts.Mode = benchModeFull
			continue
		}
		pos = append(pos, a)
	}
	if len(pos) >= 1 && pos[0] != "" {
		n, err := strconv.Atoi(pos[0])
		if err != nil {
			return fmt.Errorf("argument invalide: %s", pos[0])
		}
		opts.Predict = n
	}
	if len(pos) >= 2 && pos[1] != "" {
		n, err := strconv.Atoi(pos[1])
		if err != nil {
			return fmt.Errorf("argument invalide: %s", pos[1])
		}
		opts.Prompt = n
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release, err := conv.benchLease(cancel)
	if err != nil {
		return err
	}
	defer release()
	fmt.Printf("[bench] %s — prompt ~%d tokens, n_predict=%d…\n", opts.Mode, opts.Prompt, opts.Predict)
	r, err := runBench(ctx, opts, func(phase string, step, steps int) {
		fmt.Printf("  [%d/%d] %s\n", step, steps, phase)
	})
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Printf("  %s  %7.1f tok/s   (%d tokens en %.2fs)\n", cyan("Prefill"), r.PromptPerSecond, r.PromptN, r.PromptMs/1000)
	fmt.Printf("  %s  %7.1f tok/s   (%d tokens en %.2fs)%s\n", cyan("Decode "), r.PredictedPerSec, r.PredictedN, r.PredictedMs/1000, benchDraftText(r.Draft))
	if d := r.Depth; d != nil {
		switch {
		case d.Skipped != "":
			fmt.Printf("  Profondeur                sautée : %s\n", d.Skipped)
		case d.Cold != nil:
			fmt.Printf("  %s  %7.1f tok/s   (%d tokens, à froid)\n", cyan("Prefill@D"), d.Cold.PromptPerSecond, d.Cold.New)
			fmt.Printf("  %s  %7.1f tok/s   (jetons neufs, cache repris)\n", cyan("Prefill+ "), d.CachedPerSec)
			fmt.Printf("  %s  %7.1f tok/s%s\n", cyan("Decode@D "), d.DecodePerSec, benchDraftText(d.Draft))
			if d.Reuse >= 0 {
				fmt.Printf("  Reprise du cache          %.0f %%\n", d.Reuse*100)
			}
		}
		for _, s := range []string{d.ReuseNote, d.Partial, d.Hint} {
			if s != "" {
				fmt.Printf("  ! %s\n", s)
			}
		}
	}
	kv := r.KV
	if r.KVQuantized {
		kv += " (quantifié — zone grise)"
	}
	fmt.Printf("  KV %s · moteur %s · total %.2fs\n", kv, r.Engine, r.Elapsed)
	if !benchSavable(r) {
		fmt.Println("  (résultat partiel : non enregistré)")
	}
	fmt.Println()
	return nil
}

func benchDraftText(d *benchDraft) string {
	if d == nil || d.N == 0 {
		return ""
	}
	return fmt.Sprintf("   brouillon accepté %.0f %% (%d/%d)", float64(d.Accepted)*100/float64(d.N), d.Accepted, d.N)
}
