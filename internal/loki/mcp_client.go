package loki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Pool de sessions MCP + exposition des outils au moteur de chat.
//
// Une session est gardée ouverte entre les tours de chat : un serveur stdio garde
// son process vivant d'un message à l'autre — c'est voulu, desktop-commander & co
// ont un coût de démarrage non négligeable.
//
// Les connexions sont établies EN PARALLÈLE (mcpEnsureAll) : en séquence, quatre
// serveurs `npx` faisaient attendre la somme de leurs démarrages (~20 s), là où
// les autres clients MCP paient le plus lent. Elles sont aussi pré-chauffées au
// démarrage du service web/link (MCPPrewarm) pour que le premier message ne paie
// pas le handshake.
//
// Le namespacing des outils suit la convention de l'écosystème :
// mcp__<serveur>__<outil>. On maintient un registre nom-exposé → (serveur, outil)
// pour router les appels sans avoir à re-parser des noms potentiellement
// ambigus.

// Les délais de connexion et d'appel sont réglables (mcp_timeouts.go) :
// 20 s et 120 s par défaut.
const (
	// mcpConnectTimeoutCold : npx et uvx TÉLÉCHARGENT le paquet du serveur au
	// premier lancement (le catalogue les utilise tous les deux). Sur un réseau
	// ou un disque lents, ça dépasse largement 20 s, et l'utilisateur voyait
	// « enregistré mais connexion échouée : context deadline exceeded » pour un
	// serveur parfaitement sain. Les lancements suivants profitent du cache et
	// retombent dans le régime normal.
	// C'est un minimum : un délai de connexion réglé plus long l'emporte.
	mcpConnectTimeoutCold = 3 * time.Minute
)

// mcpOutputCap borne la sortie d'un outil MCP renvoyée au modèle. L'amont
// (AJEAN 0.15.2) la transmet désormais EN ENTIER, comme une page mémoire : une
// réponse MCP (un ticket, un document, une requête) coupée à 12 000 caractères
// laissait le modèle travailler sur un début. On s'en rapproche sans renoncer
// à tout garde-fou — sur 65k de contexte, une seule réponse géante noierait la
// fenêtre et déclencherait un compactage en plein raisonnement : le plafond
// suit la fenêtre, ~1 caractère par token de contexte, soit environ un quart
// de celle-ci (≈4 caractères par token). Jamais sous l'ancien plafond.
func mcpOutputCap() int {
	return max(12000, ctxWindow())
}

// mcpSession est une connexion vivante à un serveur MCP.
//
// L'entrée est publiée dans le pool AVANT que la connexion aboutisse, pour qu'un
// second appelant se mette en attente au lieu de lancer un deuxième process. Les
// champs sess/tools/err ne doivent donc être lus qu'une fois `ready` fermé.
type mcpSession struct {
	name  string
	cfg   MCPServerConfig
	sess  *mcpsdk.ClientSession
	tools []*mcpsdk.Tool
	err   error         // dernière erreur de connexion/liste, pour l'UI
	ready chan struct{} // fermé quand la tentative de connexion est terminée
}

// mcpManager détient le pool de sessions.
type mcpManager struct {
	mu       sync.Mutex
	sessions map[string]*mcpSession
	// registry mappe un nom d'outil exposé au modèle → (serveur, outil réel).
	registry map[string]mcpToolRef
}

type mcpToolRef struct {
	server string
	tool   string
}

var mcpMgr = &mcpManager{
	sessions: map[string]*mcpSession{},
	registry: map[string]mcpToolRef{},
}

// mcpInvalidate ferme et oublie la session d'un serveur (après un changement de
// config), pour qu'elle soit reconstruite avec la nouvelle config au prochain
// usage.
func mcpInvalidate(name string) {
	mcpMgr.mu.Lock()
	s := mcpMgr.sessions[name]
	delete(mcpMgr.sessions, name)
	// Purge le registre des outils de ce serveur.
	for exposed, ref := range mcpMgr.registry {
		if ref.server == name {
			delete(mcpMgr.registry, exposed)
		}
	}
	mcpMgr.mu.Unlock()
	// Une session encore en cours de connexion a sess == nil : c'est sa goroutine
	// qui refermera ce qu'elle vient d'ouvrir, en constatant qu'elle n'est plus
	// dans le pool.
	if s != nil && s.sess != nil {
		_ = s.sess.Close()
	}
}

// mcpCloseAll ferme toutes les sessions (arrêt du service).
func mcpCloseAll() {
	mcpMgr.mu.Lock()
	sessions := mcpMgr.sessions
	mcpMgr.sessions = map[string]*mcpSession{}
	mcpMgr.registry = map[string]mcpToolRef{}
	mcpMgr.mu.Unlock()
	for _, s := range sessions {
		if s.sess != nil {
			_ = s.sess.Close()
		}
	}
}

// headerRoundTripper injecte des en-têtes statiques (auth) sur chaque requête
// HTTP vers un serveur MCP distant.
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.base.RoundTrip(req)
}

// connect établit une session pour la config donnée.
func mcpConnect(ctx context.Context, name string, cfg MCPServerConfig) (*mcpsdk.ClientSession, error) {
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "loki", Version: Version}, nil)

	var transport mcpsdk.Transport
	switch cfg.Transport() {
	case "stdio":
		cmd := hideCmd(exec.Command(cfg.Command, cfg.Args...)) // pas de flash console (mode app Windows)
		// Hérite de l'environnement du service + surcharges déclarées.
		cmd.Env = os.Environ()
		for k, v := range cfg.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		transport = &mcpsdk.CommandTransport{Command: cmd}
	case "http":
		httpClient := &http.Client{}
		if len(cfg.Headers) > 0 {
			httpClient.Transport = headerRoundTripper{base: http.DefaultTransport, headers: cfg.Headers}
		}
		transport = &mcpsdk.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: httpClient}
	default:
		return nil, fmt.Errorf("serveur MCP '%s' mal configuré (ni command ni url)", name)
	}

	sess, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, err
	}
	return sess, nil
}

// ensure renvoie une session vivante pour un serveur activé, en la (re)créant si
// besoin. Doit être appelé sans détenir mgr.mu (il gère le lock lui-même).
//
// L'entrée est réservée dans le pool avant la connexion : deux appelants
// simultanés (le pré-chauffage et un tour de chat, par exemple) partagent la même
// tentative au lieu de lancer deux process stdio.
func (m *mcpManager) ensure(name string, cfg MCPServerConfig) *mcpSession {
	m.mu.Lock()
	if s, ok := m.sessions[name]; ok {
		m.mu.Unlock()
		<-s.ready // connexion peut-être encore en cours : on attend son issue
		return s
	}
	s := &mcpSession{name: name, cfg: cfg, ready: make(chan struct{})}
	m.sessions[name] = s
	m.mu.Unlock()

	// Connexion hors-lock (peut être lente).
	timeouts := LoadMCPTimeouts()
	ctx, cancel := context.WithTimeout(context.Background(), mcpTimeoutFor(cfg, timeouts))
	defer cancel()
	sess, err := mcpConnect(ctx, name, cfg)
	if err != nil {
		s.err = mcpFriendlyErr(err, cfg, timeouts)
	} else {
		s.sess = sess
		lt, lerr := sess.ListTools(ctx, nil)
		if lerr != nil {
			s.err = lerr
		} else {
			s.tools = lt.Tools
		}
	}

	m.mu.Lock()
	// La session a pu être invalidée pendant la connexion (changement de config,
	// arrêt du service) : dans ce cas on ne la republie pas, on referme.
	stale := m.sessions[name] != s
	if !stale {
		// (Re)peuple le registre pour ce serveur.
		for exposed, ref := range m.registry {
			if ref.server == name {
				delete(m.registry, exposed)
			}
		}
		for _, t := range s.tools {
			m.registry[mcpExposedName(name, t.Name)] = mcpToolRef{server: name, tool: t.Name}
		}
	}
	m.mu.Unlock()
	close(s.ready)
	if stale && s.sess != nil {
		_ = s.sess.Close()
		s.sess = nil
	}
	return s
}

// mcpEnsureAll connecte en parallèle tous les serveurs activés et attend qu'ils
// soient tous fixés (connectés ou en erreur). C'est ce qui évite d'additionner
// les temps de démarrage : le coût total est celui du serveur le plus lent.
func mcpEnsureAll(servers map[string]MCPServerConfig) {
	var wg sync.WaitGroup
	for _, name := range sortedServerNames(servers) {
		cfg := servers[name]
		if !cfg.Enabled || cfg.Transport() == "" {
			continue
		}
		wg.Add(1)
		go func(name string, cfg MCPServerConfig) {
			defer wg.Done()
			mcpMgr.ensure(name, cfg)
		}(name, cfg)
	}
	wg.Wait()
}

// MCPPrewarm établit les sessions MCP en tâche de fond, au démarrage du service
// web/link. Sans ça le handshake était payé par le premier message de chat, ce
// qui donnait l'impression d'une IA très lente à démarrer. Sans mode agent les
// outils MCP ne sont de toute façon pas exposés : inutile de lancer les process.
func MCPPrewarm() {
	if facadeMCP() { // le pool est celui de la façade (facade_mcp.go)
		facadeMCPPrewarm()
		return
	}
	if !agentEnabled() {
		return
	}
	servers, err := LoadMCPConfig()
	if err != nil || len(servers) == 0 {
		return
	}
	go mcpEnsureAll(servers)
}

var mcpSanitizeRe = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// mcpSanitize rend une chaîne compatible avec les noms d'outils (^[A-Za-z0-9_-]+$).
func mcpSanitize(s string) string {
	s = mcpSanitizeRe.ReplaceAllString(s, "_")
	return strings.Trim(s, "_")
}

// mcpExposedName construit le nom d'outil vu par le modèle.
func mcpExposedName(server, tool string) string {
	return "mcp__" + mcpSanitize(server) + "__" + mcpSanitize(tool)
}

// mcpTools connecte les serveurs activés et renvoie leurs outils, prêts à être
// annoncés au modèle. Réservé au mode agent (appelé par EnabledTools).
func mcpTools() []Tool {
	if facadeMCP() { // le pool est celui de la façade (facade_mcp.go)
		return facadeMCPTools()
	}
	servers, err := LoadMCPConfig()
	if err != nil || len(servers) == 0 {
		return nil
	}
	mcpEnsureAll(servers) // connexions en parallèle, puis lecture du pool (instantanée)
	var out []Tool
	for _, name := range sortedServerNames(servers) {
		cfg := servers[name]
		if !cfg.Enabled || cfg.Transport() == "" {
			continue
		}
		s := mcpMgr.ensure(name, cfg)
		if s.sess == nil {
			continue // connexion échouée : signalé dans l'UI, pas au modèle
		}
		for _, t := range s.tools {
			if cfg.ToolDisabled(t.Name) {
				continue // outil masqué par l'utilisateur : pas annoncé au modèle
			}
			desc := t.Description
			if desc == "" {
				desc = t.Title
			}
			// Préfixe le serveur d'origine dans la description : aide le modèle à
			// choisir entre outils similaires de serveurs différents.
			desc = mcpTagDescription(name, desc)
			out = append(out, Tool{
				Type: "function",
				Function: ToolFunction{
					Name:        mcpExposedName(name, t.Name),
					Description: desc,
					Parameters:  mcpNormalizeSchema(t.InputSchema),
				},
			})
		}
	}
	return out
}

// mcpNormalizeSchema convertit le InputSchema du SDK (any → map) en map[string]any
// pour notre type Tool. Un schéma vide devient un objet sans propriété.
func mcpNormalizeSchema(schema any) map[string]any {
	if m, ok := schema.(map[string]any); ok && m != nil {
		return m
	}
	// Le SDK peut renvoyer une struct/RawMessage : passe par un round-trip JSON.
	if schema != nil {
		if b, err := json.Marshal(schema); err == nil {
			var m map[string]any
			if json.Unmarshal(b, &m) == nil && m != nil {
				return m
			}
		}
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// mcpArgLabel produit un libellé court à partir des arguments d'un appel MCP,
// pour l'affichage « outil utilisé » dans l'UI (les outils MCP n'ont pas de
// champ canonique connu à l'avance).
func mcpArgLabel(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	// Privilégie les clés parlantes courantes.
	for _, k := range []string{"path", "file", "query", "command", "url", "name"} {
		if v, ok := args[k].(string); ok && v != "" {
			return v
		}
	}
	b, _ := json.Marshal(args)
	s := string(b)
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120]) + "…"
	}
	return s
}

// isMCPTool indique si un nom d'outil est routé vers MCP.
func isMCPTool(name string) bool {
	return strings.HasPrefix(name, "mcp__")
}

// mcpCall exécute un outil MCP à partir de son nom exposé et renvoie une chaîne
// résultat (texte aplati), cappée. Reconnecte une fois si la session est morte.
//
// parent est le contexte DU TOUR, comme pour runShell : l'appel naissait d'un
// context.Background(), donc le bouton stop n'arrêtait rien et le tour restait
// bloqué jusqu'au bout des 120 s (OpenFox 2ef584b8).
func mcpCall(parent context.Context, name string, args map[string]any) string {
	if facadeMCP() { // le pool est celui de la façade (facade_mcp.go)
		return facadeMCPCall(name, args)
	}
	mcpMgr.mu.Lock()
	ref, ok := mcpMgr.registry[name]
	mcpMgr.mu.Unlock()
	if !ok {
		return "[erreur] outil MCP inconnu ou serveur non connecté: " + name
	}
	servers, _ := LoadMCPConfig()
	cfg, exists := servers[ref.server]
	timeout := mcpCallTimeoutFor(cfg, LoadMCPTimeouts(), args)

	result, err := mcpCallOnce(parent, ref, args, timeout)
	if err != nil {
		if parent.Err() != nil {
			return "[appel MCP interrompu]"
		}
		if errors.Is(err, context.DeadlineExceeded) {
			// Le serveur est vivant mais l'outil est lent : le relancer
			// doublerait l'attente (et l'effet de l'outil).
			return fmt.Sprintf("[erreur MCP] pas de réponse après %s (délai de l'appel ; un argument timeout plus grand l'allonge)", timeout.Round(time.Second))
		}
		// Session peut-être morte (process stdio tombé) : on l'invalide et on
		// retente une fois avec une reconnexion fraîche.
		if exists && cfg.Enabled {
			mcpInvalidate(ref.server)
			mcpMgr.ensure(ref.server, cfg)
			if result2, err2 := mcpCallOnce(parent, ref, args, timeout); err2 == nil {
				return result2
			} else {
				err = err2
			}
			if parent.Err() != nil {
				return "[appel MCP interrompu]"
			}
		}
		return "[erreur MCP] " + err.Error()
	}
	return result
}

// mcpCallTimeoutMax plafonne le délai d'un appel, quoi que demandent l'outil
// ou la config : au-delà, c'est un job d'arrière-plan, pas un appel d'outil.
// Il suit la borne des réglages (1 h) : un délai réglé à 3600 s n'est pas
// rogné en silence.
const mcpCallTimeoutMax = mcpTimeoutMaxSec * time.Second

// mcpCallTimeoutFor : délai d'un appel. Le réglage global (t.Call), remplacé
// par CallTimeoutSec du serveur ; un argument « timeout » de l'outil lui-même
// (beaucoup en ont un : navigateur, build, requête) l'allonge de 30 s pour que
// l'outil ait le temps de rendre SA propre erreur de délai.
func mcpCallTimeoutFor(cfg MCPServerConfig, t MCPTimeouts, args map[string]any) time.Duration {
	d := t.call()
	if cfg.CallTimeoutSec > 0 {
		d = time.Duration(cfg.CallTimeoutSec) * time.Second
	}
	if t := mcpArgTimeout(args); t > 0 && t+30*time.Second > d {
		d = t + 30*time.Second
	}
	return min(d, mcpCallTimeoutMax)
}

// mcpArgTimeout lit l'argument « timeout » d'un appel. Les serveurs MCP ne
// s'accordent pas sur l'unité : au-delà de 3600 on le lit en millisecondes
// (5000 = 5 s), en deçà en secondes (600 = 10 min).
func mcpArgTimeout(args map[string]any) time.Duration {
	var v float64
	switch x := args["timeout"].(type) {
	case float64:
		v = x
	case int:
		v = float64(x)
	case int64:
		v = float64(x)
	case json.Number:
		v, _ = x.Float64()
	case string:
		v, _ = strconv.ParseFloat(strings.TrimSpace(x), 64)
	}
	if v <= 0 {
		return 0
	}
	if v > 3600 {
		return time.Duration(v) * time.Millisecond
	}
	return time.Duration(v * float64(time.Second))
}

func mcpCallOnce(parent context.Context, ref mcpToolRef, args map[string]any, timeout time.Duration) (string, error) {
	mcpMgr.mu.Lock()
	s := mcpMgr.sessions[ref.server]
	mcpMgr.mu.Unlock()
	if s == nil || s.sess == nil {
		return "", fmt.Errorf("serveur %s non connecté", ref.server)
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	res, err := s.sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: ref.tool, Arguments: args})
	if err != nil {
		return "", err
	}
	out := flattenMCPContent(res)
	if r, lim := []rune(out), mcpOutputCap(); len(r) > lim {
		out = string(r[:lim]) + fmt.Sprintf("\n…[tronqué : %d caractères au total, %d gardés]", len(r), lim)
	}
	if res.IsError {
		return "[l'outil a renvoyé une erreur]\n" + out, nil
	}
	if strings.TrimSpace(out) == "" {
		return "[ok] (aucune sortie)", nil
	}
	return out, nil
}

// flattenMCPContent aplatit le contenu d'un CallToolResult en texte. Les blocs
// texte sont concaténés ; le contenu structuré est rendu en JSON s'il n'y a pas
// de texte ; les autres types (image/audio) sont résumés.
func flattenMCPContent(res *mcpsdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcpsdk.TextContent:
			parts = append(parts, v.Text)
		case *mcpsdk.ImageContent:
			parts = append(parts, "[image "+v.MIMEType+"]")
		case *mcpsdk.AudioContent:
			parts = append(parts, "[audio "+v.MIMEType+"]")
		default:
			if b, err := json.Marshal(c); err == nil {
				parts = append(parts, string(b))
			}
		}
	}
	text := strings.TrimSpace(strings.Join(parts, "\n"))
	if text == "" && res.StructuredContent != nil {
		if b, err := json.MarshalIndent(res.StructuredContent, "", "  "); err == nil {
			return string(b)
		}
	}
	return text
}

// mcpTagDescription préfixe la description d'un outil MCP par le nom d'origine
// de son serveur, que le nom d'outil (assaini) ne garde pas. mcpToolServer le
// relit pour la ligne du préambule : ce format n'existe qu'ici, pour que les
// deux ne puissent pas diverger en silence.
func mcpTagDescription(server, desc string) string {
	return "[MCP: " + server + "] " + desc
}

// mcpToolServer retrouve le serveur d'origine d'un outil MCP annoncé, d'après
// le préfixe posé par mcpTagDescription. Le nom brut d'un serveur peut lui-même
// contenir « ] » : on retient la première coupure dont la forme assainie est
// bien celle qui figure dans le nom de l'outil.
func mcpToolServer(t Tool) (string, bool) {
	name := t.Function.Name
	if !strings.HasPrefix(name, "mcp__") {
		return "", false
	}
	rest, ok := strings.CutPrefix(t.Function.Description, "[MCP: ")
	if !ok {
		return "", false
	}
	for i := 0; ; {
		j := strings.Index(rest[i:], "] ")
		if j < 0 {
			return "", false
		}
		if server := rest[:i+j]; strings.HasPrefix(name, "mcp__"+mcpSanitize(server)+"__") {
			return server, true
		}
		i += j + 1
	}
}

// mcpPromptLine renvoie une ligne système listant les serveurs MCP annoncés et
// leur nombre d'outils, pour situer le modèle. Elle se DÉDUIT des outils
// réellement envoyés ce tour-ci (EnabledTools), pas de l'état du pool.
//
// Lue dans le pool, elle arrivait en retard d'un tour : le préambule est bâti
// avant que mcpTools n'ouvre les connexions, si bien que le premier tour après
// un démarrage partait sans la ligne et le second avec — tout le prompt à
// recalculer pour une ligne. Elle comptait aussi les outils masqués par
// l'utilisateur, et s'affichait pour un rôle (planner…) qui ne reçoit aucun
// outil MCP. Tirée de la même tranche que les schémas, elle ne peut plus les
// contredire.
func mcpPromptLine(tools []Tool) string {
	counts := map[string]int{}
	for _, t := range tools {
		if server, ok := mcpToolServer(t); ok {
			counts[server]++
		}
	}
	if len(counts) == 0 {
		return ""
	}
	names := make([]string, 0, len(counts))
	for name, n := range counts {
		names = append(names, fmt.Sprintf("%s (%d)", name, n))
	}
	sort.Strings(names)
	return "\n\nMCP servers connected (their tools are prefixed mcp__<server>__<tool>): " + strings.Join(names, ", ") + "."
}

// MCPServerStatus est l'état d'un serveur pour l'UI web.
type MCPServerStatus struct {
	Name      string   `json:"name"`
	Transport string   `json:"transport"`
	Enabled   bool     `json:"enabled"`
	Connected bool     `json:"connected"`
	Error     string   `json:"error,omitempty"`
	Tools     []string `json:"tools"`    // tous les outils découverts sur le serveur
	Disabled  []string `json:"disabled"` // sous-ensemble masqué (non exposé à l'IA)
	// Détail de config (pour pré-remplir le formulaire d'édition).
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// CallTimeoutSec : délai d'un appel propre au serveur (0 = réglage global).
	CallTimeoutSec int `json:"callTimeoutSec,omitempty"`
}

// MCPStatus renvoie l'état de tous les serveurs configurés, en tentant de
// connecter ceux qui sont activés (pour refléter l'état réel dans l'UI).
func MCPStatus() ([]MCPServerStatus, error) {
	servers, err := LoadMCPConfig()
	if err != nil {
		return nil, err
	}
	mcpEnsureAll(servers) // en parallèle : le panneau MCP de l'UI n'attend plus la somme
	var out []MCPServerStatus
	for _, name := range sortedServerNames(servers) {
		cfg := servers[name]
		st := MCPServerStatus{
			Name:      name,
			Transport: cfg.Transport(),
			Enabled:   cfg.Enabled,
			Command:   cfg.Command,
			Args:      cfg.Args,
			Env:       cfg.Env,
			URL:       cfg.URL,
			Headers:   cfg.Headers,
			Tools:     []string{},

			CallTimeoutSec: cfg.CallTimeoutSec,
			Disabled:       cfg.DisabledTools,
		}
		if st.Disabled == nil {
			st.Disabled = []string{}
		}
		if cfg.Enabled && cfg.Transport() != "" {
			s := mcpMgr.ensure(name, cfg)
			if s.sess != nil {
				st.Connected = true
				for _, t := range s.tools {
					st.Tools = append(st.Tools, t.Name)
				}
			}
			if s.err != nil {
				st.Error = s.err.Error()
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// mcpTimeoutFor choisit le délai de connexion d'un serveur : le réglage
// global, mais au moins mcpConnectTimeoutCold pour un lanceur qui télécharge
// son paquet à la volée (npx, uvx).
func mcpTimeoutFor(cfg MCPServerConfig, t MCPTimeouts) time.Duration {
	if mcpColdLauncher(cfg) {
		return max(t.connect(), mcpConnectTimeoutCold)
	}
	return t.connect()
}

// mcpColdLauncher : npx ou uvx, qui téléchargent le paquet au premier
// lancement.
func mcpColdLauncher(cfg MCPServerConfig) bool {
	base := strings.ToLower(filepath.Base(cfg.Command))
	base = strings.TrimSuffix(strings.TrimSuffix(base, ".exe"), ".cmd")
	return base == "npx" || base == "uvx"
}

// mcpFriendlyErr habille l'erreur de connexion la plus vécue : le délai
// dépassé d'un npx/uvx en plein téléchargement. L'erreur brute (« context
// deadline exceeded ») envoyait l'utilisateur chercher un bug de config alors
// qu'il suffit souvent de réessayer une fois le paquet en cache.
func mcpFriendlyErr(err error, cfg MCPServerConfig, t MCPTimeouts) error {
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if mcpColdLauncher(cfg) {
		base := strings.ToLower(filepath.Base(cfg.Command))
		base = strings.TrimSuffix(strings.TrimSuffix(base, ".exe"), ".cmd")
		return fmt.Errorf("délai dépassé — %s télécharge le paquet du serveur au premier lancement, ce qui peut être long ; clique « tester » pour réessayer (le paquet reste en cache), et vérifie que le conteneur atteint le registre (npm/PyPI)", base)
	}
	return fmt.Errorf("délai dépassé — le serveur n'a pas répondu au handshake en %s (délai de connexion réglable dans les réglages MCP)", t.connect())
}
