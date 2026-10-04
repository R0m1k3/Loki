package loki

// chat_projsnap.go — PROJ_SNAPSHOT : le bloc projet FIGÉ par discussion, et ses
// changements livrés en <context_update> dans le message suivant.
//
// Pourquoi. Le contexte du projet (description, index mémoire, trackers,
// consignes du dépôt) part en tête du PREMIER message utilisateur
// (normalizeSystemMessages). Reconstruit à chaque tour, il change dès qu'une page
// est créée ou qu'un tracker reçoit un point — et tout ce qui le suit dans le
// prompt est recalculé : 10 à 45k tokens, des dizaines de secondes sur un 27B,
// des minutes sur un MoE déchargé. Un modèle qui note une valeur à chaque tour
// paie ce prix à chaque tour.
//
// Avec la clé (off par défaut) :
//   - la discussion garde une copie figée du bloc (ProjSnap), envoyée à l'octet
//     près d'un tour à l'autre. Ses en-têtes disent « à jour au <date> » et que
//     les <context_update> de Loki les complètent ;
//   - un changement arrive en tête du NOUVEAU message utilisateur, rangé tel quel
//     dans l'historique (le tour suivant renvoie les mêmes octets). Le moteur
//     pose un point de reprise au début de chaque message utilisateur : seul ce
//     petit bloc est à calculer ;
//   - rien n'est perdu : lignes ajoutées, modifiées, retirées pour les données
//     faites de lignes (pages, trackers), texte COMPLET pour la prose
//     (description, AGENTS.md). Une ligne fixe du système dit que ces blocs
//     viennent de Loki et que le plus récent l'emporte.
//
// Le bloc est rafraîchi — copie neuve, blocs <context_update> retirés de tout
// l'historique — seulement quand le début du prompt change de toute façon :
// compaction (début, fin de tour, manuelle, en cours de tour, réduction forcée),
// système ou outils modifiés (date du jour, réglages, mode), cache froid
// (redémarrage de Loki, changement de modèle, de preset ou de réglage moteur),
// projet, nom, mode mémoire, mode Code ou dépôt changés, textes des en-têtes
// changés. Et quand les mises à jour accumulées dépassent un seuil : au-delà, le
// modèle aurait trop à recoller.
//
// Hors discussion : une tâche planifiée garde le bloc vivant (elle repart de zéro
// à chaque fois), et sans agent le modèle brut ne reçoit rien du projet — ni bloc,
// ni mise à jour.

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// projSnapEnabled : la clé PROJ_SNAPSHOT demande-t-elle le bloc figé ? Jamais
// pour un preset externe (API OpenAI-compatible) : ses requêtes restent celles
// d'avant, quel que soit le réglage.
func projSnapEnabled(cfg map[string]string) bool {
	if isExternalConfig(cfg) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(cfg["PROJ_SNAPSHOT"])) {
	case "on", "1", "true", "yes", "oui":
		return true
	}
	return false
}

// Textes propres au bloc figé. Leur empreinte entre dans la clé de validité :
// une mise à jour de Loki qui les reformule rafraîchit les instantanés persistés.
const (
	projSnapFormat     = "1"
	memIndexFrozenTail = " Later <context_update> blocks from Loki update it."
	trackerFrozenHead  = " — your trackers, each with its latest point at that time; later <context_update> blocks from Loki replace these lines. " +
		"To say which trackers exist or give a latest value, use this list with those updates — do NOT call tracker with no arguments to list them. " +
		"Call tracker(name[, when]) only to look further back in history, or with action \"add\"/\"edit\"/\"delete\" to change data.\n"
	ctxUpdOpen  = `<context_update from="loki"`
	ctxUpdClose = "</context_update>"
	// projSnapSystemLine : la ligne fixe du prompt système, clé posée seulement.
	projSnapSystemLine = "A <context_update from=\"loki\"> block at the start of a user message is written by Loki, not by the user: it updates <project_context>, and the most recent one wins.\n"
)

// projSnapBoot : identifiant de ce lancement de Loki. Après un redémarrage, le
// cache du moteur est froid : garder un vieil instantané n'y gagnerait rien.
var projSnapBoot = strconv.FormatInt(time.Now().UnixNano(), 36)

// projSnapNow : l'horodatage « à jour au » des en-têtes et des mises à jour.
func projSnapNow() string { return time.Now().Format("2006-01-02 15:04") }

// projState : ce que le modèle sait du projet, section par section — au moment
// de l'instantané, puis après chaque mise à jour annoncée. Structuré, jamais un
// simple hash : c'est contre lui qu'on calcule la mise à jour suivante, y
// compris après un redémarrage.
type projState struct {
	Code     string            `json:"code,omitempty"`      // consignes du dépôt, message complet
	Desc     string            `json:"desc,omitempty"`      // description du projet
	MemOn    bool              `json:"mem_on,omitempty"`    // index mémoire présent
	MemProse string            `json:"mem_prose,omitempty"` // lignes de l'index qui ne sont pas des pages
	Mem      map[string]string `json:"mem,omitempty"`       // fichier → ligne de page
	Trk      map[string]string `json:"trk,omitempty"`       // slug → « nom — valeur (date) »
}

// projSnapshot : le bloc figé d'une discussion. Persisté avec elle (omitempty :
// absent tant que la clé n'a jamais servi, et ignoré d'un Loki plus ancien).
type projSnapshot struct {
	At       int64     `json:"at"`             // instant de l'instantané (ms)
	Msgs     []Message `json:"msgs,omitempty"` // le bloc tel qu'il part, à l'octet près
	State    projState `json:"state"`          // dernier état annoncé au modèle
	Key      string    `json:"key"`            // validité : lancement, réglages, projet, mode…
	SysHash  string    `json:"sys_hash"`       // système + outils du dernier tour
	SnapTok  int       `json:"snap_tok,omitempty"`
	DeltaTok int       `json:"delta_tok,omitempty"` // mises à jour livrées depuis
}

// projLive : le bloc vivant, rendu au format figé, avec son état. memOrder /
// trkOrder : ordre d'affichage, pour des mises à jour stables.
type projLive struct {
	msgs     []Message
	state    projState
	memMsg   Message
	trkMsg   Message
	memOrder []string
	trkOrder []string
}

// projSnapCapture lit le contexte projet UNE fois et en tire le bloc (même
// ordre que l'assemblage d'un tour : consignes du dépôt, description, index
// mémoire, trackers) et son état.
func projSnapCapture(caps Caps, asOf string) projLive {
	var lv projLive
	if m, ok := codeInstructionsMessage(caps); ok {
		lv.msgs = append(lv.msgs, m)
		lv.state.Code, _ = m.Content.(string)
	}
	if desc := strings.TrimSpace(projectDesc(activeProjectSlug())); desc != "" {
		lv.msgs = append(lv.msgs, renderProjectContext(desc))
		lv.state.Desc = desc
	}
	if idx, ok := memIndexText(); ok {
		lv.memMsg = renderMemIndex(idx, asOf)
		lv.msgs = append(lv.msgs, lv.memMsg)
		lv.state.MemOn = true
		lv.state.MemProse, lv.state.Mem, lv.memOrder = splitMemIndex(idx)
	}
	if list, ok := trackerIndexList(); ok {
		lv.trkMsg = renderTrackerIndex(list, asOf)
		lv.msgs = append(lv.msgs, lv.trkMsg)
		lv.state.Trk = map[string]string{}
		for _, m := range list {
			lv.state.Trk[m.Slug] = trackerIndexLine(m)
			lv.trkOrder = append(lv.trkOrder, m.Slug)
		}
	}
	return lv
}

// splitMemIndex sépare les lignes de page (clé = fichier entre « ]( » et « ) »)
// du reste de l'index. Une ligne de page en double part avec la prose : ainsi
// tout changement la concernant fait renvoyer l'index entier, sans perte.
func splitMemIndex(idx string) (prose string, pages map[string]string, order []string) {
	pages = map[string]string{}
	var other []string
	for _, line := range strings.Split(idx, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "- [") {
			if i := strings.Index(l, "]("); i >= 0 {
				if j := strings.Index(l[i+2:], ")"); j > 0 {
					key := l[i+2 : i+2+j]
					if _, dup := pages[key]; !dup {
						pages[key] = l
						order = append(order, key)
						continue
					}
				}
			}
		}
		other = append(other, line)
	}
	return strings.TrimSpace(strings.Join(other, "\n")), pages, order
}

func newProjSnapshot(lv projLive, key, sysHash string) *projSnapshot {
	tok := 0
	for _, m := range lv.msgs {
		tok += len(msgText(m)) / 4
	}
	return &projSnapshot{At: time.Now().UnixMilli(), Msgs: lv.msgs, State: lv.state, Key: key, SysHash: sysHash, SnapTok: tok}
}

// projSnapDeltaLimit : au-delà de ce volume de mises à jour depuis l'instantané
// (en tokens estimés), on rafraîchit plutôt que d'en empiler d'autres : environ
// un quart du bloc, entre 400 et 1500 tokens.
func projSnapDeltaLimit(snapTok int) int {
	return min(1500, max(400, snapTok/4))
}

// projSnapKey : tout ce qui, en changeant, rend l'instantané faux ou inutile.
// Les réglages pris en compte sont ceux qui touchent au moteur (modèle, preset,
// arguments : un changement le relance, le cache est froid) ; les boutons de
// conversation qui ne le relancent pas en sont exclus.
func projSnapKey(caps Caps, cfg map[string]string) string {
	h := fnv.New64a()
	w := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	w(projSnapFormat)
	w(projSnapBoot)
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		if !projSnapIgnoredKeys[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		w(k)
		w(cfg[k])
	}
	slug := activeProjectSlug()
	w(slug)
	w(projectName(slug))
	w(string(memMode()))
	w(strconv.FormatBool(caps.Code))
	if caps.Code {
		d, _ := gitRepoDir("")
		w(d)
		w(agentCwd())
	}
	for _, s := range []string{memIndexFrozenTail, trackerFrozenHead, ctxUpdOpen, ctxUpdClose, projSnapSystemLine} {
		w(s)
	}
	return fmt.Sprintf("%016x", h.Sum64())
}

// projSnapIgnoredKeys : réglages changés en cours de discussion sans relancer le
// moteur ni toucher au bloc. Les compter rafraîchirait pour rien.
var projSnapIgnoredKeys = map[string]bool{
	"PROJ_SNAPSHOT": true, "PREWARM": true, "REASONING_ECHO": true, "REASONING_EFFORT": true, "TEMP": true,
	"COMPACT": true, "CRAWL4AI_URL": true, "CRAWL4AI_KEY": true, "WEB_ENGINE": true,
}

// projSnapSysHash : empreinte du bloc système commun et des outils. S'ils
// changent, tout le prompt est recalculé de toute façon : rafraîchir est gratuit.
func projSnapSysHash(sent []Message, tools []Tool) string {
	h := fnv.New64a()
	for _, m := range sent {
		if m.Role != "system" {
			break
		}
		if isProjectSystem(m) {
			continue
		}
		s, _ := m.Content.(string)
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	b, _ := json.Marshal(tools)
	h.Write(b)
	return fmt.Sprintf("%016x", h.Sum64())
}

// projDelta : la mise à jour à annoncer entre l'état déjà connu du modèle et le
// bloc vivant. "" = rien de changé. Les sections faites de lignes changent ligne
// à ligne (une ligne de tracker remplace la précédente) ; la prose, ou une
// section qui apparaît, repart en entier.
func projDelta(old projState, lv projLive, asOf string) string {
	cur := lv.state
	var b strings.Builder
	full := func(m Message) {
		b.WriteString("Current version, replaces any earlier one — " + msgText(m) + "\n")
	}
	if old.Code != cur.Code {
		if cur.Code == "" {
			b.WriteString("Repository instructions: removed.\n")
		} else {
			full(Message{Content: cur.Code})
		}
	}
	if old.Desc != cur.Desc {
		if cur.Desc == "" {
			b.WriteString("Project description: removed.\n")
		} else {
			full(renderProjectContext(cur.Desc))
		}
	}
	switch {
	case !cur.MemOn && old.MemOn:
		b.WriteString("Memory index: no pages any more.\n")
	case cur.MemOn && (!old.MemOn || old.MemProse != cur.MemProse):
		full(lv.memMsg)
	case cur.MemOn:
		var lines []string
		for _, k := range lv.memOrder {
			l := strings.TrimPrefix(cur.Mem[k], "- ")
			switch prev, ok := old.Mem[k]; {
			case !ok:
				lines = append(lines, "+ "+l)
			case prev != cur.Mem[k]:
				lines = append(lines, "~ "+l)
			}
		}
		for _, k := range sortedMissing(old.Mem, cur.Mem) {
			lines = append(lines, "- "+k+" (deleted)")
		}
		if len(lines) > 0 {
			b.WriteString("Memory index changes:\n" + strings.Join(lines, "\n") + "\n")
		}
	}
	switch {
	case len(cur.Trk) == 0 && len(old.Trk) > 0:
		b.WriteString("Trackers: none left.\n")
	case len(cur.Trk) > 0 && len(old.Trk) == 0:
		full(lv.trkMsg)
	case len(cur.Trk) > 0:
		var lines []string
		for _, k := range lv.trkOrder {
			switch prev, ok := old.Trk[k]; {
			case !ok:
				lines = append(lines, "+ "+cur.Trk[k])
			case prev != cur.Trk[k]:
				lines = append(lines, "~ "+cur.Trk[k])
			}
		}
		for _, k := range sortedMissing(old.Trk, cur.Trk) {
			lines = append(lines, "- "+old.Trk[k]+" (deleted)")
		}
		if len(lines) > 0 {
			b.WriteString("Tracker changes (latest point):\n" + strings.Join(lines, "\n") + "\n")
		}
	}
	if b.Len() == 0 {
		return ""
	}
	// Un titre de page qui contiendrait la balise fermante couperait le bloc au
	// mauvais endroit au moment de le retirer.
	body := strings.ReplaceAll(b.String(), ctxUpdClose, "</context-update>")
	return ctxUpdOpen + ` at="` + asOf + `">` + "\n" + body + ctxUpdClose + "\n\n"
}

// sortedMissing : clés de old absentes de cur, triées.
func sortedMissing(old, cur map[string]string) []string {
	var out []string
	for k := range old {
		if _, ok := cur[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// --- blocs <context_update> dans l'historique -----------------------------

// stripCtxUpdText retire le(s) bloc(s) posé(s) en TÊTE du texte. Seulement en
// tête : un message qui en parle plus loin reste intact.
func stripCtxUpdText(s string) (string, bool) {
	stripped := false
	for strings.HasPrefix(s, ctxUpdOpen) {
		i := strings.Index(s, ctxUpdClose)
		if i < 0 {
			break
		}
		s = strings.TrimPrefix(s[i+len(ctxUpdClose):], "\n\n")
		stripped = true
	}
	return s, stripped
}

// withoutCtxUpdate : le message sans son bloc de tête, ok=false s'il n'en avait
// pas (message rendu tel quel). Contenu texte ou multimodal ([]any, []map).
func withoutCtxUpdate(m Message) (Message, bool) {
	if m.Role != "user" {
		return m, false
	}
	switch c := m.Content.(type) {
	case string:
		if s, ok := stripCtxUpdText(c); ok {
			m.Content = s
			return m, true
		}
	case []any:
		if len(c) > 0 {
			if p, ok := c[0].(map[string]any); ok && p["type"] == "text" {
				if t, _ := p["text"].(string); t != "" {
					if s, ok := stripCtxUpdText(t); ok {
						out := append([]any(nil), c[1:]...)
						if s != "" {
							q := map[string]any{}
							for k, v := range p {
								q[k] = v
							}
							q["text"] = s
							out = append([]any{q}, out...)
						}
						m.Content = out
						return m, true
					}
				}
			}
		}
	case []map[string]any:
		if len(c) > 0 && c[0]["type"] == "text" {
			if t, _ := c[0]["text"].(string); t != "" {
				if s, ok := stripCtxUpdText(t); ok {
					out := append([]map[string]any(nil), c[1:]...)
					if s != "" {
						q := map[string]any{}
						for k, v := range c[0] {
							q[k] = v
						}
						q["text"] = s
						out = append([]map[string]any{q}, out...)
					}
					m.Content = out
					return m, true
				}
			}
		}
	}
	return m, false
}

// stripContextUpdates rend l'historique sans aucun bloc <context_update>. Sans
// bloc, c'est la MÊME tranche (rien recopié) : le cas de toutes les discussions
// sans la clé.
func stripContextUpdates(msgs []Message) []Message {
	var out []Message
	for i, m := range msgs {
		if s, ok := withoutCtxUpdate(m); ok {
			if out == nil {
				out = append([]Message(nil), msgs...)
			}
			out[i] = s
		}
	}
	if out == nil {
		return msgs
	}
	return out
}

func hasContextUpdates(msgs []Message) bool {
	for _, m := range msgs {
		if _, ok := withoutCtxUpdate(m); ok {
			return true
		}
	}
	return false
}

// prependCtxUpdate pose le bloc en tête d'un message utilisateur, comme
// prependToFirstUser : texte simple, ou une partie texte de plus en multimodal.
func prependCtxUpdate(m Message, block string) (Message, bool) {
	if m.Role != "user" {
		return m, false
	}
	switch c := m.Content.(type) {
	case string:
		m.Content = block + c
	case []any:
		m.Content = append([]any{map[string]any{"type": "text", "text": block}}, c...)
	case []map[string]any:
		m.Content = append([]map[string]any{{"type": "text", "text": block}}, c...)
	default:
		return m, false
	}
	return m, true
}

// replaceProjectHead remplace le bloc projet de tête (hors rappel des pages
// lues, qui appartient à l'historique) par block, à la même place.
func replaceProjectHead(msgs []Message, block []Message) []Message {
	end := 0
	for end < len(msgs) && msgs[end].Role == "system" {
		end++
	}
	out := make([]Message, 0, len(msgs)+len(block))
	inserted := false
	for _, m := range msgs[:end] {
		reminder := false
		if s, ok := m.Content.(string); ok && strings.HasPrefix(s, memReminderPrefix) {
			reminder = true
		}
		if isProjectSystem(m) && !reminder {
			if !inserted {
				out = append(out, block...)
				inserted = true
			}
			continue
		}
		if reminder && !inserted {
			out = append(out, block...)
			inserted = true
		}
		out = append(out, m)
	}
	if !inserted {
		out = append(out, block...)
	}
	return append(out, msgs[end:]...)
}

// --- assemblage d'un tour de la discussion --------------------------------

// turnView assemble la séquence envoyée pour un tour de la discussion (generate,
// runBuilderTurn) : prompt du preset, consignes du dépôt, contexte projet, puis
// l'historique. inject=false : modèle brut (agent off), rien du projet.
//
// Sans PROJ_SNAPSHOT : exactement l'assemblage d'avant, contexte vivant. Seul
// ajout, sans effet tant que la clé n'a jamais servi : une discussion qui porte
// encore un instantané ou des blocs d'une époque où la clé était posée en est
// nettoyée — un vieux <context_update> placé après le bloc vivant se lirait
// comme plus récent que lui.
//
// Avec la clé : instantané pris, gardé ou rafraîchi ; mise à jour éventuelle
// posée en tête du dernier message utilisateur, dans c.Messages ET dans msgs
// (sous c.mu, si l'epoch n'a pas changé). pt sert au rafraîchissement en cours
// de tour (runChatTools) ; nil sans la clé.
func (c *Conversation) turnView(caps Caps, epoch int, msgs []Message, inject bool) (sent []Message, tools []Tool, pt *projSnapTurn) {
	return c.assembleTurn(caps, epoch, msgs, inject, false)
}

// turnViewDry : la séquence que turnView enverrait pour msgs, SANS rien toucher
// à la conversation — ni instantané pris, ni mise à jour rangée, ni nettoyage.
// Pour le préchauffage (chat_prewarm.go) : contenu vivant, mêmes décisions que
// le vrai tour. La mise à jour qu'il poserait en tête du dernier message n'y est
// pas : ce message-là est la sentinelle, hors du préfixe préchauffé.
func (c *Conversation) turnViewDry(caps Caps, msgs []Message, inject bool) ([]Message, []Tool) {
	sent, tools, _ := c.assembleTurn(caps, 0, msgs, inject, true)
	return sent, tools
}

func (c *Conversation) assembleTurn(caps Caps, epoch int, msgs []Message, inject, dry bool) (sent []Message, tools []Tool, pt *projSnapTurn) {
	cfg := ReadConfig()
	on := projSnapEnabled(cfg)
	if !inject {
		// Sans agent : ni bloc, ni mise à jour. Des blocs restés d'un tour avec
		// agent sont retirés de la vue envoyée (l'historique les garde : l'agent
		// revenu, le système change et le bloc est rafraîchi).
		sent, tools = prepareTurn(stripContextUpdates(msgs), caps)
		return sent, tools, nil
	}
	if !on {
		c.mu.Lock()
		if !dry && c.epoch == epoch && (c.ProjSnap != nil || hasContextUpdates(c.Messages)) {
			c.ProjSnap = nil
			c.Messages = stripContextUpdates(c.Messages)
		}
		c.mu.Unlock()
		msgs = stripContextUpdates(msgs)
		final := append(projectSystemMessages(), msgs...)
		if m, ok := codeInstructionsMessage(caps); ok {
			final = append([]Message{m}, final...)
		}
		if sp := readSysPrompt(); sp != "" {
			final = append([]Message{{Role: "system", Content: sp}}, final...)
		}
		sent, tools = prepareTurn(final, caps)
		return sent, tools, nil
	}

	// Lectures (disque, base, git) hors du verrou de la conversation.
	asOf := projSnapNow()
	live := projSnapCapture(caps, asOf)
	key := projSnapKey(caps, cfg)
	// Même calcul que prepareTurn, en deux temps : l'empreinte du système et des
	// outils décide du rafraîchissement avant l'assemblage. Les outils ne sont
	// calculés qu'une fois (connexions MCP).
	tools = EnabledTools(caps)
	var spMsgs []Message
	if sp := readSysPrompt(); sp != "" {
		spMsgs = []Message{{Role: "system", Content: sp}}
	}
	sysHash := projSnapSysHash(InjectSkills(spMsgs, caps, tools), tools)

	block := live.msgs
	c.mu.Lock()
	if dry {
		// Même décision que projSnapApplyLocked, appliquée à la seule copie.
		if refresh, _, _ := projSnapDecide(c.ProjSnap, live, key, sysHash, asOf); refresh {
			msgs = stripContextUpdates(msgs)
		} else {
			block = c.ProjSnap.Msgs
		}
		c.mu.Unlock()
		final := append(append(append([]Message(nil), spMsgs...), block...), msgs...)
		return InjectSkills(final, caps, tools), tools, nil
	}
	if c.epoch == epoch {
		refresh, delta := c.projSnapApplyLocked(live, key, sysHash, asOf)
		if refresh {
			msgs = stripContextUpdates(msgs)
		}
		if delta != "" {
			if n := len(msgs); n > 0 {
				if m, ok := prependCtxUpdate(msgs[n-1], delta); ok {
					msgs = append(msgs[:n-1:n-1], m)
				}
			}
		}
		block = c.ProjSnap.Msgs
	}
	c.mu.Unlock()

	final := append(append(append([]Message(nil), spMsgs...), block...), msgs...)
	sent = InjectSkills(final, caps, tools)
	return sent, tools, &projSnapTurn{caps: caps, key: key, sysHash: sysHash}
}

// projSnapApplyLocked décide pour ce tour, c.mu tenu : rafraîchir (instantané
// neuf, blocs retirés de l'historique) ou annoncer une mise à jour, posée en tête
// du dernier message utilisateur de c.Messages. Renvoie ce qui a été fait, pour
// que l'appelant l'applique à sa propre copie.
func (c *Conversation) projSnapApplyLocked(live projLive, key, sysHash, asOf string) (refresh bool, delta string) {
	snap := c.ProjSnap
	refresh, delta, tok := projSnapDecide(snap, live, key, sysHash, asOf)
	if refresh {
		c.Messages = stripContextUpdates(c.Messages)
		c.ProjSnap = newProjSnapshot(live, key, sysHash)
		return true, ""
	}
	if delta == "" {
		return false, ""
	}
	n := len(c.Messages)
	if n == 0 {
		return false, ""
	}
	m, ok := prependCtxUpdate(c.Messages[n-1], delta)
	if !ok {
		// Pas de message utilisateur où la poser (cas théorique) : l'état
		// annoncé ne bouge pas, elle partira au tour suivant.
		return false, ""
	}
	c.Messages[n-1] = m
	snap.State = live.state
	snap.DeltaTok += tok
	return false, delta
}

// projSnapDecide : rafraîchir ou non, et la mise à jour à annoncer sinon (tok :
// sa taille estimée). Pur : projSnapApplyLocked l'applique, le préchauffage
// (turnViewDry) s'en sert pour prévoir le tour sans rien toucher.
func projSnapDecide(snap *projSnapshot, live projLive, key, sysHash, asOf string) (refresh bool, delta string, tok int) {
	refresh = snap == nil || snap.Key != key || snap.SysHash != sysHash
	if !refresh {
		delta = projDelta(snap.State, live, asOf)
		tok = len(delta) / 4
		if delta != "" && snap.DeltaTok+tok > projSnapDeltaLimit(snap.SnapTok) {
			refresh = true
		}
	}
	return refresh, delta, tok
}

// --- rafraîchissement en cours de tour -------------------------------------

// projSnapTurn accompagne un tour de discussion avec la clé posée. runChatTools
// le récupère dans le contexte et s'en sert quand une compaction ou une
// réduction réécrit l'historique en plein tour : le début du prompt change de
// toute façon, le bloc est donc remplacé par le vivant (et les mises à jour
// retirées), sans perdre celles qu'aurait avalées le résumé. L'appelant range
// ensuite refreshed comme nouvel instantané : le tour suivant renvoie les mêmes
// octets, pas de recalcul de plus.
type projSnapTurn struct {
	caps    Caps
	key     string
	sysHash string

	mu        sync.Mutex
	refreshed *projSnapshot
}

type projSnapCtxKey struct{}

func withProjSnapTurn(ctx context.Context, pt *projSnapTurn) context.Context {
	return context.WithValue(ctx, projSnapCtxKey{}, pt)
}

func projSnapTurnFrom(ctx context.Context) *projSnapTurn {
	pt, _ := ctx.Value(projSnapCtxKey{}).(*projSnapTurn)
	return pt
}

// refresh : historique en cours de tour, bloc vivant à la place du figé, sans
// aucun <context_update>.
func (pt *projSnapTurn) refresh(msgs []Message) []Message {
	live := projSnapCapture(pt.caps, projSnapNow())
	out := replaceProjectHead(stripContextUpdates(msgs), live.msgs)
	pt.mu.Lock()
	pt.refreshed = newProjSnapshot(live, pt.key, pt.sysHash)
	pt.mu.Unlock()
	return out
}

// takeRefreshed : l'instantané posé en cours de tour, nil s'il n'y en a pas eu.
func (pt *projSnapTurn) takeRefreshed() *projSnapshot {
	if pt == nil {
		return nil
	}
	pt.mu.Lock()
	defer pt.mu.Unlock()
	s := pt.refreshed
	pt.refreshed = nil
	return s
}
