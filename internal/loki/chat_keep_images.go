package loki

// chat_keep_images.go — KEEP_TURN_IMAGES : garder dans l'historique les images
// que montrent les outils (web_screenshot, browser_screenshot, see_image), au
// lieu de les jeter à la fin du tour. Opt-in, off par défaut, ZONE GRISE.
//
// Sans la clé, ces images sont ÉPHÉMÈRES (voir runChat) : le modèle les voit
// pendant le tour, puis elles disparaissent de l'historique. Le tour suivant
// renvoie donc un fil où elles manquent : le préfixe que le moteur a en cache
// diverge à la première, et toute la boucle d'outils qui suivait — souvent des
// dizaines d'étapes de navigation — est recalculée.
//
// Avec la clé, une image est gardée telle qu'envoyée, sous conditions :
//
//   - vision réellement active (visionEnabled et engineSeesImages, revérifiés à
//     chaque tour : un preset sans vision n'en reçoit jamais) ; un preset
//     externe n'est concerné que si sa vision est déclarée, et chaque image
//     gardée y est alors refacturée à chaque tour ;
//   - rangée par référence (chat_images.go, chiffrée si la mémoire l'est) et
//     relue à l'octet près avant d'être gardée : le modèle reçoit les mêmes
//     octets à chaque envoi ;
//   - coût MESURÉ par le moteur (écart de usage.prompt_tokens entre deux
//     requêtes, moins l'estimation du texte ajouté) et non estimé : une image
//     dont le coût ne se mesure pas (moteur sans usage, requête relancée,
//     historique réécrit entre-temps) reste éphémère ;
//   - toutes les images gardées tiennent dans keepImgPct % de la fenêtre. La
//     première qui ne tient pas reste éphémère, et toutes celles qui la suivent
//     dans le tour aussi : le préfixe divergera de toute façon à elle ;
//   - retirées D'ABORD par toute compaction ou réduction forcée (légende et
//     imageLostMarker gardés), avant tout résumé ; un rechargement du fil les
//     retire aussi (stripImageParts), comme les pièces jointes.
//
// Ce que ça coûte : plus d'information gardée, donc une compaction (avec
// perte) qui arrive plus tôt — d'où la borne et le retrait prioritaire.

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// keepImgPct : part maximale de la fenêtre que les images gardées occupent.
const keepImgPct = 10

// keepImgMinTokens : en dessous, la mesure ne vaut rien (écart noyé dans
// l'erreur d'estimation du texte) — l'image reste éphémère.
const keepImgMinTokens = 16

func keepTurnImagesOn(cfg map[string]string) bool {
	switch strings.ToLower(strings.TrimSpace(cfg["KEEP_TURN_IMAGES"])) {
	case "on", "1", "true", "yes", "oui":
		return true
	}
	return false
}

func keepImgBudget() int { return ctxWindow() * keepImgPct / 100 }

// keptImageTokens : coût total des images gardées de ces messages.
func keptImageTokens(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += m.imgTokens
	}
	return n
}

func hasKeptImages(msgs []Message) bool {
	for _, m := range msgs {
		if m.imgTokens > 0 {
			return true
		}
	}
	return false
}

// dropKeptImages renvoie une COPIE des messages où chaque image gardée devient
// sa légende suivie d'imageLostMarker (la forme de stripImageParts), et le coût
// libéré. Rien de gardé : msgs tel quel, 0. Le message reste un relais
// (ImgRelay) : ni une demande, ni la preuve qu'une demande a été servie.
func dropKeptImages(msgs []Message) ([]Message, int) {
	var out []Message
	freed := 0
	for i, m := range msgs {
		if m.imgTokens <= 0 {
			continue
		}
		if out == nil {
			out = append([]Message(nil), msgs...)
		}
		var texts []string
		for _, p := range contentParts(m.Content) {
			if t, ok := p["text"].(string); ok && p["type"] == "text" {
				texts = append(texts, t)
			}
		}
		freed += m.imgTokens
		m.Content = strings.Join(texts, "\n") + imageLostMarker
		m.imgTokens = 0
		out[i] = m
	}
	if out == nil {
		return msgs, 0
	}
	return out, freed
}

func logKeptImagesDropped(phase string, freed int) {
	fmt.Fprintf(os.Stderr, "[images] %s : images gardées retirées (%d jetons)\n", phase, freed)
}

// stripRelayTags retire la marque ImgRelay avant l'envoi : propre à Loki, elle
// n'a rien à faire dans la requête. Aucune marque : msgs tel quel, sans copie.
func stripRelayTags(msgs []Message) []Message {
	var out []Message
	for i, m := range msgs {
		// La marque des blocs <context_update> (PROJ_SNAPSHOT) non plus ne
		// quitte jamais Loki.
		if !m.ImgRelay && !m.CtxUpd {
			continue
		}
		if out == nil {
			out = append([]Message(nil), msgs...)
		}
		m.ImgRelay, m.CtxUpd = false, false
		out[i] = m
	}
	if out == nil {
		return msgs
	}
	return out
}

// refRelayImage range l'image d'un relais par référence et renvoie le message
// qui la porte ainsi, avec le poids de l'image (octets, pour répartir une
// mesure entre plusieurs images d'une même étape). ok=false — format sans
// extension connue, disque plein, mémoire chiffrée verrouillée, relecture qui
// ne redonne pas exactement la même data-URL — : l'image reste éphémère.
func refRelayImage(m Message) (Message, int, bool) {
	parts := contentParts(m.Content)
	np := make([]map[string]any, len(parts))
	weight := 0
	for i, p := range parts {
		np[i] = p
		_, u := partImageURL(p)
		if u == "" {
			continue
		}
		if weight > 0 || !strings.HasPrefix(u, "data:image/") {
			return m, 0, false
		}
		k := strings.Index(u, ";base64,")
		if k < 0 {
			return m, 0, false
		}
		mime := u[5:k]
		if imgExtByMime[mime] == "" {
			return m, 0, false
		}
		b, err := base64.StdEncoding.DecodeString(u[k+8:])
		if err != nil || len(b) == 0 {
			return m, 0, false
		}
		ref, err := storeChatImage(b, mime)
		if err != nil {
			return m, 0, false
		}
		// Gardée en vie : storeChatImage ne réécrit pas un fichier déjà là (mêmes
		// octets), et pruneChatImages efface au bout de chatImgKeep d'après la
		// date du fichier — une image revue aujourd'hui ne doit pas tomber demain
		// en « [image indisponible] » au milieu d'un fil.
		now := time.Now()
		_ = os.Chtimes(filepath.Join(chatImgDir(), strings.TrimPrefix(ref, imgRefScheme)), now, now)
		if back, ok := chatImageDataURL(ref); !ok || back != u {
			return m, 0, false
		}
		np[i] = map[string]any{"type": "image_url", "image_url": map[string]any{"url": ref}}
		weight = len(b)
	}
	if weight == 0 {
		return m, 0, false
	}
	m.Content = np
	return m, weight, true
}

// keepImages : l'état de KEEP_TURN_IMAGES pendant UN tour (runChat). nil = clé
// absente, tâche, sous-agent, vérification ou vision inactive : toutes ses
// méthodes rendent alors le comportement d'avant.
type keepImages struct {
	// stopped : une image de ce tour n'a pas été gardée ; les suivantes ne le
	// sont pas non plus (le préfixe du tour suivant diverge à elle).
	stopped bool
	// Images ajoutées à l'étape courante, gardées à l'essai jusqu'à la mesure
	// de la requête qui les montre la première.
	pend []keepPend
	// pendReq : numéro de cette requête. pendBase : contexte que la complétion
	// précédente laisse (ctxAfter). pendFrom : premier message ajouté depuis.
	pendReq, pendBase, pendFrom int
	// reqN : requêtes envoyées ; sentLen : messages de la dernière.
	reqN, sentLen int
	// lastAfter : ctxAfter de la dernière complétion complète (0 = inconnu).
	lastAfter int
}

type keepPend struct{ msgIdx, extraIdx, weight int }

// newKeepImages : l'état du tour, ou nil. Fil principal d'une discussion
// seulement : les tâches, sous-agents et la vérification ne persistent pas
// leur historique comme elle.
func newKeepImages(cfg map[string]string, tag perfTag) *keepImages {
	if !keepTurnImagesOn(cfg) || tag.kind != perfMain || tag.conv == "" {
		return nil
	}
	if !visionEnabled() || !engineSeesImages() {
		return nil
	}
	return &keepImages{}
}

// relay ajoute le message qui relaie une image d'outil. Sans état : à la vue
// du modèle seulement, comme avant. Avec : gardé à l'essai dans extra aussi,
// par référence, si le budget n'est pas déjà plein. from = début des messages
// de l'étape (premier résultat d'outil).
func (k *keepImages) relay(messages, extra []Message, m Message, from int) ([]Message, []Message) {
	if k == nil {
		return append(messages, m), extra
	}
	m.ImgRelay = true
	if k.stopped {
		return append(messages, m), extra
	}
	// Budget déjà plein : inutile de ranger le fichier.
	r, w, ok := Message{}, 0, false
	if keptImageTokens(messages) < keepImgBudget() {
		r, w, ok = refRelayImage(m)
	}
	if !ok {
		k.stopped = true
		return append(messages, m), extra
	}
	if len(k.pend) == 0 {
		k.pendReq, k.pendBase, k.pendFrom = k.reqN+1, k.lastAfter, from
	}
	messages = append(messages, r)
	extra = append(extra, r)
	k.pend = append(k.pend, keepPend{msgIdx: len(messages) - 1, extraIdx: len(extra) - 1, weight: w})
	return messages, extra
}

// sending : une requête part avec n messages.
func (k *keepImages) sending(n int) {
	if k == nil {
		return
	}
	k.reqN++
	k.sentLen = n
}

// observe : une complétion est lue. Mesure le coût des images à l'essai si
// c'est la requête qui les a montrées la première, et les garde si le budget
// le permet ; sinon elles redeviennent éphémères (retirées d'extra). Renvoie
// extra.
func (k *keepImages) observe(complete bool, st StatsEvent, messages, extra []Message) []Message {
	if k == nil {
		return extra
	}
	after := 0
	if complete && st.PromptTokensTotal > 0 {
		after = st.ctxAfter()
	}
	if len(k.pend) > 0 {
		if !k.measure(after, st.PromptTokensTotal, messages, extra) {
			extra = k.reject(extra)
		}
		k.pend = nil
	}
	k.lastAfter = after
	return extra
}

// measure répartit le coût mesuré entre les images à l'essai et les garde si
// tout tient dans le budget. false = mesure impossible ou budget dépassé.
func (k *keepImages) measure(after, prompt int, messages, extra []Message) bool {
	if after <= 0 || k.pendBase <= 0 || k.pendReq != k.reqN ||
		k.pendFrom > k.sentLen || k.sentLen > len(messages) {
		return false
	}
	// Ce que la requête a ajouté depuis la précédente, hors images : résultats
	// d'outils, légendes exceptées, ajouts en cours de réponse, rappels.
	text := 0
	for _, m := range messages[k.pendFrom:k.sentLen] {
		if !m.ImgRelay {
			text += msgTokens(m)
		}
	}
	img := prompt - k.pendBase - text
	weight := 0
	for _, p := range k.pend {
		weight += p.weight
	}
	if weight <= 0 || img < keepImgMinTokens*len(k.pend) {
		return false
	}
	if keptImageTokens(messages)+img > keepImgBudget() {
		return false
	}
	shares := make([]int, len(k.pend))
	left := img
	for i, p := range k.pend {
		shares[i] = img * p.weight / weight
		if i == len(k.pend)-1 {
			shares[i] = left
		}
		if shares[i] < keepImgMinTokens {
			return false
		}
		left -= shares[i]
	}
	for _, p := range k.pend {
		if p.msgIdx >= len(messages) || p.extraIdx >= len(extra) || !messages[p.msgIdx].ImgRelay || !extra[p.extraIdx].ImgRelay {
			return false
		}
	}
	for i, p := range k.pend {
		messages[p.msgIdx].imgTokens = shares[i]
		extra[p.extraIdx].imgTokens = shares[i]
	}
	return true
}

// reject retire d'extra les images à l'essai : éphémères, comme sans la clé.
func (k *keepImages) reject(extra []Message) []Message {
	if len(k.pend) == 0 {
		return extra
	}
	k.stopped = true
	drop := map[int]bool{}
	for _, p := range k.pend {
		drop[p.extraIdx] = true
	}
	out := make([]Message, 0, len(extra))
	for i, m := range extra {
		if !drop[i] {
			out = append(out, m)
		}
	}
	k.pend = nil
	return out
}

// rewritten : l'historique vient d'être réécrit en cours de tour (compaction,
// réduction) et extra repart de zéro. Les images à l'essai ne se mesurent plus
// (ni base ni position) : éphémères, et le tour s'arrête là pour les suivantes.
func (k *keepImages) rewritten() {
	if k == nil {
		return
	}
	if len(k.pend) > 0 {
		k.stopped = true
	}
	k.pend = nil
}

// publishable : l'historique publié par une réécriture en cours de tour, sans
// les relais dont l'image n'est pas gardée (éphémères, ou à l'essai). Sans
// état, tel quel — comportement d'avant.
func (k *keepImages) publishable(msgs []Message) []Message {
	if k == nil {
		return msgs
	}
	out := msgs[:0:0]
	for _, m := range msgs {
		if m.ImgRelay && m.imgTokens == 0 && hasImagePart(m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// finish : fin du tour. Une image encore à l'essai (tour arrêté, erreur, fin
// juste après elle) n'a pas été mesurée : éphémère.
func (k *keepImages) finish(extra []Message) []Message {
	if k == nil {
		return extra
	}
	return k.reject(extra)
}

func hasImagePart(m Message) bool {
	for _, p := range contentParts(m.Content) {
		if p["type"] == "image_url" {
			return true
		}
	}
	return false
}

// keptImagesTurnStart : début d'un tour de discussion. Les images gardées
// sont retirées d'abord — avant toute compaction, qui ne résume alors que si
// c'est encore nécessaire — quand le contexte la réclame, ou quand elles ne
// doivent plus partir : clé retirée, vision absente (un moteur sans projecteur
// refuserait la requête). Renvoie l'historique et le contexte à jour.
func (c *Conversation) keptImagesTurnStart(epoch int, msgs []Message, ctxUsed, peak int) ([]Message, int) {
	if !hasKeptImages(msgs) {
		return msgs, ctxUsed
	}
	if keepTurnImagesOn(ReadConfig()) && visionEnabled() && engineSeesImages() && !compactNeeded(msgs, ctxUsed, peak) {
		return msgs, ctxUsed
	}
	out, freed := dropKeptImages(msgs)
	logKeptImagesDropped("début-tour", freed)
	if ctxUsed > 0 {
		ctxUsed = max(ctxUsed-freed, 1)
	}
	c.mu.Lock()
	cur := 0
	if c.epoch == epoch {
		c.Messages = append([]Message(nil), out...)
		if c.CtxUsed > 0 {
			c.CtxUsed = max(c.CtxUsed-freed, 1)
		}
		cur = c.CtxUsed
	}
	c.mu.Unlock()
	if cur > 0 {
		c.appendDelta(epoch, map[string]any{"ctx_used": cur})
	}
	return out, ctxUsed
}
