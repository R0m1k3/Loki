package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Sonde de gabarit : que fait le gabarit de chat du modèle chargé de ce qu'on
// lui envoie ? Deux questions, celles dont dépendent les pistes de réutilisation
// du cache :
//
//   - preservesHistory : le raisonnement d'un message assistant PASSÉ (avant le
//     dernier message utilisateur) est-il encore rendu ? Les gabarits Qwen3 le
//     retirent, d'autres le gardent.
//   - prefixStable : quand un message utilisateur s'ajoute après un tour
//     d'outil, le rendu précédent reste-t-il un préfixe exact du nouveau ? Sinon
//     le moteur recalcule depuis l'endroit où les deux divergent, quoi que fasse
//     son cache.
//
// Plus une troisième, déduite des mêmes rendus : rendersReasoning, le gabarit
// rend-il reasoning_content quelque part ?
//
// DIAGNOSTIQUE d'abord. Rien ici ne construit un prompt : preservesHistory
// décrit le gabarit, pas ce que Loki fait. Seul consommateur : REASONING_ECHO
// (llm_reasoning_echo.go, opt-in), qui s'abstient de renvoyer un raisonnement
// que le gabarit ne rend jamais, et ne compte celui d'avant la dernière
// question que si le gabarit le garde. Jamais pour AJOUTER quoi que ce soit :
// « unknown » y laisse la décision à la clé, avec ses filets.
//
// Règles de conduite, toutes là pour qu'une sonde ne coûte jamais rien au vrai
// travail :
//
//   - moteur LOCAL seulement : un preset externe n'a pas de /apply-template, et
//     sonder une API tierce n'apprendrait rien de son gabarit ;
//   - POST /apply-template et rien d'autre. Ce rendu est sans état (ni file de
//     tâches ni slot), donc il ne peut pas évincer le cache de la conversation.
//     Jamais de repli sur une complétion : avec un seul slot, elle effacerait
//     précisément le KV qu'on cherche à préserver ;
//   - lancée APRÈS une complétion terminée, en tâche de fond, une fois /health à
//     200 : jamais sur le chemin du premier jeton ;
//   - les mêmes outils et chat_template_kwargs que la complétion qui l'a
//     déclenchée : les gabarits Qwen bifurquent sur enable_thinking, sur
//     reasoning_effort et sur la présence d'outils ;
//   - add_generation_prompt=false demandé au moteur, plutôt que de deviner à la
//     main où commence l'amorce de réponse ;
//   - tout échec (404 d'un moteur ancien ou d'un fork, 401, exception du
//     gabarit, délai) donne « unknown », jamais un oui ou un non.

type tplTri string

const (
	tplYes     tplTri = "yes"
	tplNo      tplTri = "no"
	tplUnknown tplTri = "unknown"
)

// tplProbeResult : ce que la sonde a conclu pour un moteur, un modèle, un
// gabarit et une forme de requête donnés. Aucun texte de conversation : les
// messages rendus sont synthétiques, et seuls des verdicts sont gardés.
type tplProbeResult struct {
	PreservesHistory tplTri `json:"preserves_history"`
	PrefixStable     tplTri `json:"prefix_stable"`
	// RendersReasoning : reasoning_content apparaît-il dans un rendu, au moins
	// juste après la dernière question ? « no » = le gabarit (ou le moteur)
	// l'ignore toujours.
	RendersReasoning tplTri `json:"renders_reasoning"`
	Build            string `json:"build,omitempty"`
	Model            string `json:"model,omitempty"` // nom du fichier, pas le chemin
	TemplateHash     string `json:"template_hash,omitempty"`
	Tools            int    `json:"tools"`
	// Kwargs : chat_template_kwargs avec lesquels les rendus ont été faits.
	Kwargs map[string]any `json:"chat_template_kwargs,omitempty"`
	Effort string         `json:"reasoning_effort,omitempty"`
	Note   string         `json:"note,omitempty"`
	At     time.Time      `json:"at"`
	// cacheable : la réponse ne changera pas tant que le moteur, le modèle et le
	// gabarit restent les mêmes (rendu obtenu, 404, exception du gabarit). Un
	// délai, un 503 ou un 401 se retentent plus tard.
	cacheable bool
}

func tplUnknownResult(note string) tplProbeResult {
	return tplProbeResult{PreservesHistory: tplUnknown, PrefixStable: tplUnknown, RendersReasoning: tplUnknown, Note: note, At: time.Now()}
}

// tplShape : la forme de la requête à reproduire. Les outils sont figés en JSON
// dès la prise de forme : la goroutine de sonde ne partage rien avec runChat.
type tplShape struct {
	tools  json.RawMessage // nil = aucun outil envoyé
	nTools int
	first  string // nom du premier outil, pour un appel vraisemblable
	kwargs map[string]any
	effort string
}

func newTplShape(tools []Tool, kwargs map[string]any, effort string) tplShape {
	s := tplShape{nTools: len(tools), effort: effort}
	if len(tools) > 0 {
		s.tools, _ = json.Marshal(tools)
		s.first = tools[0].Function.Name
	}
	if kwargs != nil {
		s.kwargs = maps.Clone(kwargs)
	}
	return s
}

func (s tplShape) hash() string {
	h := fnv.New64a()
	h.Write(s.tools)
	kw, _ := json.Marshal(s.kwargs) // clés triées : stable
	h.Write([]byte{0})
	h.Write(kw)
	h.Write([]byte{0})
	h.Write([]byte(s.effort))
	return fmt.Sprintf("%016x", h.Sum64())
}

// Messages synthétiques. Des marqueurs qu'aucun gabarit ne produit de lui-même,
// cherchés tels quels dans le rendu.
const (
	tplMarkSys = "loki-sonde-systeme"
	tplMarkA   = "loki-sonde-question-a"
	tplMarkR1  = "loki-sonde-raisonnement-r1"
	tplMarkC1  = "loki-sonde-reponse-c1"
	tplMarkT1  = "loki-sonde-resultat-t1"
	tplMarkB   = "loki-sonde-question-b"
	// Identifiant d'appel : 9 caractères alphanumériques, la forme qu'exigent les
	// gabarits stricts (Mistral) sous peine de raise_exception.
	tplCallID = "a1B2c3D4e"
)

type tplMsg struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

const (
	tplProbeTimeout  = 3 * time.Second // par requête
	tplProbeEvery    = time.Minute     // au plus une vérification par minute
	tplProbeCacheMax = 16
	tplProbe503Tries = 3
)

var (
	// tplProbeAuto : déclenchement automatique après les complétions. Coupé par
	// les tests du paquet (TestMain), dont les faux moteurs ne répondent pas
	// tous en JSON : la sonde y est appelée explicitement.
	tplProbeAuto = true
	// Attente entre deux essais sur un 503 (modèle en chargement).
	tplProbeBackoff = time.Second

	tplProbeMu    sync.Mutex
	tplProbeCache = map[string]tplProbeResult{}
	tplProbeLast  *tplProbeResult
	// tplProbeByShape : le dernier verdict de chaque forme de requête
	// (tplShape.hash). tplProbeLast est celui de la dernière forme sondée,
	// quelle qu'elle soit : bon pour l'affichage, pas pour décider d'une
	// requête d'une autre forme (tplCapsFor).
	tplProbeByShape = map[string]tplProbeResult{}
	tplProbeRunning bool
	tplProbeTried   time.Time
	tplProbeLogged  string
)

// tplProbeKick : appelé par runChat après une complétion COMPLÈTE du fil
// principal, sur le moteur local, avec ce qu'elle a envoyé. Ne bloque jamais :
// au plus une sonde en vol, au plus une vérification par minute (la plupart se
// résument à un GET /props qui retrouve la réponse en cache).
func tplProbeKick(tools []Tool, kwargs map[string]any, effort string) {
	if !tplProbeAuto {
		return
	}
	tplProbeMu.Lock()
	if tplProbeRunning || time.Since(tplProbeTried) < tplProbeEvery {
		tplProbeMu.Unlock()
		return
	}
	tplProbeRunning, tplProbeTried = true, time.Now()
	tplProbeMu.Unlock()
	shape := newTplShape(tools, kwargs, effort)
	go func() {
		defer func() {
			tplProbeMu.Lock()
			tplProbeRunning = false
			tplProbeMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		tplProbeEnsure(ctx, shape)
	}()
}

// tplCapsCurrent : le dernier verdict connu, pour l'affichage. Rien pour un
// preset externe : un verdict du moteur local n'y dirait rien de vrai.
func tplCapsCurrent() (tplProbeResult, bool) {
	if externalActive() {
		return tplProbeResult{}, false
	}
	tplProbeMu.Lock()
	defer tplProbeMu.Unlock()
	if tplProbeLast == nil {
		return tplProbeResult{}, false
	}
	return *tplProbeLast, true
}

// tplCapsFor : le dernier verdict pour CETTE forme de requête (mêmes outils,
// mêmes chat_template_kwargs, même reasoning_effort). Pas de verdict pour elle
// — jamais sondée, ou sortie du cache — : rien, et l'appelant se replie comme
// sur « inconnu ». Rien non plus pour un preset externe.
func tplCapsFor(shape tplShape) (tplProbeResult, bool) {
	if externalActive() {
		return tplProbeResult{}, false
	}
	tplProbeMu.Lock()
	defer tplProbeMu.Unlock()
	r, ok := tplProbeByShape[shape.hash()]
	return r, ok
}

// tplProbeEnsure : verdict pour la forme donnée, depuis le cache ou une sonde.
func tplProbeEnsure(ctx context.Context, shape tplShape) tplProbeResult {
	if resolveChatEndpoint().External {
		return tplUnknownResult("preset externe : pas de sonde")
	}
	p := tplProber{
		base:   fmt.Sprintf("http://localhost:%d", LLMPort()),
		auth:   authHeader,
		client: &http.Client{Timeout: tplProbeTimeout},
	}
	if !p.healthy(ctx) {
		// Moteur absent ou en chargement : rien à conclure, rien à ranger.
		return tplUnknownResult("moteur pas prêt (/health)")
	}
	props, err := p.props(ctx)
	if err != nil {
		r := tplUnknownResult("/props illisible : " + err.Error())
		tplProbeStore("", shape.hash(), r)
		return r
	}
	h := fnv.New64a()
	h.Write([]byte(props.ChatTemplate))
	tmplHash := fmt.Sprintf("%016x", h.Sum64())
	// Clé : le binaire, le modèle ET le gabarit effectivement chargé — un
	// --chat-template d'EXTRA_ARGS change le dernier sans toucher aux deux
	// autres. Plus la forme de la requête, dont le rendu dépend.
	key := props.BuildInfo + "\x00" + props.ModelPath + "\x00" + tmplHash + "\x00" + shape.hash()
	tplProbeMu.Lock()
	if r, ok := tplProbeCache[key]; ok {
		tplProbeLast = &r
		tplProbeKeepShape(shape.hash(), r)
		tplProbeMu.Unlock()
		return r
	}
	tplProbeMu.Unlock()

	r := p.probe(ctx, shape)
	r.Build, r.TemplateHash, r.Tools, r.Kwargs, r.Effort = props.BuildInfo, tmplHash, shape.nTools, shape.kwargs, shape.effort
	if props.ModelPath != "" {
		r.Model = filepath.Base(props.ModelPath)
	}
	// Une ligne par verdict nouveau : un échec passager qui se répète chaque
	// minute à l'identique n'inonde pas le journal.
	line := tplProbeLine(r)
	tplProbeMu.Lock()
	fresh := line != tplProbeLogged
	tplProbeLogged = line
	tplProbeMu.Unlock()
	if fresh {
		fmt.Fprintln(os.Stderr, line)
	}
	tplProbeStore(key, shape.hash(), r)
	return r
}

// tplProbeKeepShape note le verdict d'une forme (tplProbeMu tenu). Au plus
// tplProbeCacheMax formes : la plus ancienne cède sa place.
func tplProbeKeepShape(shape string, r tplProbeResult) {
	if _, ok := tplProbeByShape[shape]; !ok && len(tplProbeByShape) >= tplProbeCacheMax {
		oldest := ""
		for k, v := range tplProbeByShape {
			if oldest == "" || v.At.Before(tplProbeByShape[oldest].At) {
				oldest = k
			}
		}
		delete(tplProbeByShape, oldest)
	}
	tplProbeByShape[shape] = r
}

func tplProbeStore(key, shape string, r tplProbeResult) {
	tplProbeMu.Lock()
	defer tplProbeMu.Unlock()
	tplProbeLast = &r
	tplProbeKeepShape(shape, r)
	if key == "" || !r.cacheable {
		return
	}
	if len(tplProbeCache) >= tplProbeCacheMax {
		oldest := ""
		for k, v := range tplProbeCache {
			if oldest == "" || v.At.Before(tplProbeCache[oldest].At) {
				oldest = k
			}
		}
		delete(tplProbeCache, oldest)
	}
	tplProbeCache[key] = r
}

func tplProbeLine(r tplProbeResult) string {
	s := fmt.Sprintf("[tplprobe] build=%s modèle=%s gabarit=%s outils=%d historique_raisonnement=%s préfixe_stable=%s raisonnement_rendu=%s",
		orDash(r.Build), orDash(r.Model), orDash(r.TemplateHash), r.Tools, r.PreservesHistory, r.PrefixStable, r.RendersReasoning)
	if r.Note != "" {
		s += " (" + r.Note + ")"
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

type tplProber struct {
	base   string
	auth   func(*http.Request)
	client *http.Client
}

// tplHTTPError : réponse non-200 du moteur.
type tplHTTPError struct {
	status int
	body   string
}

func (e *tplHTTPError) Error() string {
	if e.body != "" {
		return fmt.Sprintf("HTTP %d : %s", e.status, e.body)
	}
	return fmt.Sprintf("HTTP %d", e.status)
}

// tplDefinitive : l'échec se reproduira à l'identique tant que moteur, modèle
// et gabarit ne changent pas (route absente, gabarit qui lève une exception,
// requête refusée). Un 401/403 dépend de la clé, un 503 du chargement, un
// délai de la charge : ceux-là se retentent.
func tplDefinitive(err error) bool {
	var he *tplHTTPError
	if !errors.As(err, &he) {
		return false
	}
	switch he.status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusServiceUnavailable, http.StatusTooManyRequests:
		return false
	}
	return he.status >= 400 && he.status < 600
}

func (p tplProber) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, tplProbeTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Un moteur protégé par API_KEY répond 401 sans la clé (le piège déjà
	// payé par la sonde de vision, chat_screenshot.go).
	if p.auth != nil {
		p.auth(req)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(data))
		if rs := []rune(msg); len(rs) > 160 {
			msg = string(rs[:160]) + "…"
		}
		return nil, &tplHTTPError{status: resp.StatusCode, body: msg}
	}
	return data, nil
}

func (p tplProber) healthy(ctx context.Context) bool {
	_, err := p.do(ctx, http.MethodGet, "/health", nil)
	return err == nil
}

type tplProps struct {
	BuildInfo    string `json:"build_info"`
	ModelPath    string `json:"model_path"`
	ChatTemplate string `json:"chat_template"`
}

func (p tplProber) props(ctx context.Context) (tplProps, error) {
	var out tplProps
	data, err := p.do(ctx, http.MethodGet, "/props", nil)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("réponse non JSON")
	}
	return out, nil
}

// render : POST /apply-template, relancé sur 503 (modèle en chargement).
func (p tplProber) render(ctx context.Context, shape tplShape, msgs []tplMsg, genPrompt bool) (string, error) {
	body := map[string]any{"messages": msgs, "add_generation_prompt": genPrompt}
	// Mêmes clés que runChat, et seulement celles qui atteignent le gabarit.
	if shape.tools != nil {
		body["tools"] = shape.tools
		body["parallel_tool_calls"] = false
	}
	if shape.kwargs != nil {
		body["chat_template_kwargs"] = shape.kwargs
	}
	if shape.effort != "" {
		body["reasoning_effort"] = shape.effort
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	for try := 1; ; try++ {
		data, err := p.do(ctx, http.MethodPost, "/apply-template", raw)
		var he *tplHTTPError
		if err != nil && errors.As(err, &he) && he.status == http.StatusServiceUnavailable && try < tplProbe503Tries {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(try) * tplProbeBackoff):
			}
			continue
		}
		if err != nil {
			return "", err
		}
		var out struct {
			Prompt *string `json:"prompt"`
		}
		if json.Unmarshal(data, &out) != nil || out.Prompt == nil {
			return "", &tplHTTPError{status: http.StatusOK, body: "réponse sans prompt"}
		}
		return *out.Prompt, nil
	}
}

// probe : les rendus et leurs verdicts. Chaque verdict ne vaut que si ses
// propres rendus ont abouti ; l'autre peut rester inconnu.
func (p tplProber) probe(ctx context.Context, shape tplShape) tplProbeResult {
	r := tplUnknownResult("")
	definitive := true
	var notes []string
	fail := func(what string, err error) {
		if !tplDefinitive(err) {
			definitive = false
		}
		notes = append(notes, what+" : "+err.Error())
	}

	sys := tplMsg{Role: "system", Content: tplMarkSys}
	userA := tplMsg{Role: "user", Content: tplMarkA}
	userB := tplMsg{Role: "user", Content: tplMarkB}

	// 1) Raisonnement d'un tour passé : [système, A, assistant{R1, C1}, B].
	hist, err := p.render(ctx, shape, []tplMsg{sys, userA,
		{Role: "assistant", Content: tplMarkC1, ReasoningContent: tplMarkR1}, userB}, false)
	switch {
	case err != nil:
		fail("historique", err)
	case !strings.Contains(hist, tplMarkC1) || !strings.Contains(hist, tplMarkB):
		notes = append(notes, "historique : rendu incohérent")
	case strings.Contains(hist, tplMarkR1):
		r.PreservesHistory = tplYes
	default:
		r.PreservesHistory = tplNo
	}

	// 2) Stabilité du préfixe à travers un tour d'outil bien formé : un appel
	// avec identifiant, puis son résultat, puis un nouveau message utilisateur.
	name := shape.first
	if name == "" {
		name = "lookup"
	}
	call := tplMsg{Role: "assistant", Content: tplMarkC1, ReasoningContent: tplMarkR1,
		ToolCalls: []ToolCall{{ID: tplCallID, Type: "function", Function: ToolCallFunc{Name: name, Arguments: "{}"}}}}
	result := tplMsg{Role: "tool", Content: tplMarkT1, ToolCallID: tplCallID}
	before := []tplMsg{sys, userA, call, result}
	after := append(append([]tplMsg{}, before...), userB)

	r1, err1 := p.render(ctx, shape, before, false)
	var r1g, r2 string
	var err2, err3 error
	if err1 == nil {
		r1g, err2 = p.render(ctx, shape, before, true)
	}
	if err1 == nil && err2 == nil {
		r2, err3 = p.render(ctx, shape, after, false)
	}
	switch {
	case err1 != nil:
		fail("préfixe", err1)
	case err2 != nil:
		fail("préfixe", err2)
	case err3 != nil:
		fail("préfixe", err3)
	case r1 == r1g:
		// Un moteur qui ignore add_generation_prompt rend l'amorce de réponse
		// dans les deux cas : la comparaison conclurait à tort « instable ».
		notes = append(notes, "add_generation_prompt ignoré par le moteur")
	case !strings.Contains(r1, tplMarkT1) || !strings.Contains(r2, tplMarkB):
		notes = append(notes, "préfixe : rendu incohérent")
	case strings.HasPrefix(r2, r1):
		r.PrefixStable = tplYes
	default:
		r.PrefixStable = tplNo
	}
	switch {
	case r.PreservesHistory == tplYes || (err1 == nil && strings.Contains(r1, tplMarkR1)):
		r.RendersReasoning = tplYes
	case err1 == nil && strings.Contains(r1, tplMarkT1) && r.PreservesHistory == tplNo:
		// Même au dernier tour le raisonnement n'apparaît pas : c'est le moteur
		// (ou le gabarit) qui ignore reasoning_content, pas un tri par position.
		r.RendersReasoning = tplNo
		notes = append(notes, "reasoning_content jamais rendu")
	}
	r.Note = strings.Join(notes, " ; ")
	r.cacheable = definitive
	return r
}
