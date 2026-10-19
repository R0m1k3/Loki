package loki

// chat_search.go — recherche PLEIN-TEXTE dans les discussions (reprise d'AJEAN,
// commit 558d3b4d, issue amont #98 / PR #99).
//
// Avant, la barre latérale ne filtrait que les TITRES, côté navigateur. Relire
// le corps de chaque discussion à chaque frappe est exclu : une discussion pèse
// vite des centaines de Ko, chiffrée de surcroît. On tient donc un INDEX INVERSÉ
// dans bkChatSearch, à trois familles de clés :
//
//	« d|<id> »     → histSearchDoc : la fiche d'une discussion (version, date
//	                 d'indexation, aperçu). Petite : la couverture et l'aperçu se
//	                 lisent sans charger la liste des mots.
//	« t|<id> »     → []string : les mots DISTINCTS de la discussion, repliés et
//	                 triés. Sert seulement à la mise à jour (différence avec
//	                 l'état précédent) et à la suppression.
//	« p|<token> »  → []string d'identifiants triés (sémantique d'ENSEMBLE).
//
// Écart avec l'amont : AJEAN range la liste des mots dans la fiche « d| » et
// lit TOUTES les fiches à chaque requête pour la couverture. Ici la fiche reste
// petite, et une mise à jour ne touche que les mots ajoutés ou retirés depuis la
// fois précédente — la discussion ouverte est réindexée à chaque écriture, pas
// seulement à l'archivage comme chez l'amont.
//
// QUI ÉCRIT L'INDEX. Ce process, toujours, même quand la façade Rust tient la
// conversation (LOKI_CHAT=rust) : elle n'a pas accès à la base et écrit ses
// discussions par /internal/v1/store (web_internal.go), qui indexe au passage
// (histIndexStoreWrite). L'index suit donc la même discussion quel que soit le
// process qui la tient, sans second écrivain ni test de parité à tenir : la
// route de recherche (/api/conversations/search) est servie ICI, la façade la
// relaie comme toute route qu'elle ne connaît pas (proxy.rs, forward).
//
// ACCENTS : l'index stocke des tokens REPLIÉS (« résumé » → « resume »). C'est
// délibérément différent de la sortie de recallTokens, que l'outil recall_search
// du modèle consomme telle quelle — ne pas « corriger » cette apparente
// incohérence : replier l'index (et la requête, symétriquement) est ce qui fait
// qu'une recherche « resume » trouve « résumé » et inversement.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	bolt "go.etcd.io/bbolt"
)

const (
	// histSearchIndexVersion : une fiche dont la version diffère est traitée
	// comme non indexée et reconstruite en entier.
	histSearchIndexVersion = 1
	// histSearchMaxTerms borne le nombre de postings lus par requête.
	histSearchMaxTerms = 16
	// histSearchDefaultLimit / histSearchMaxLimit : taille d'une page de
	// résultats. L'extrait en contexte relit le corps de CHAQUE résultat de la
	// page : la borne protège la requête d'un « limit=100000 ».
	histSearchDefaultLimit = 30
	histSearchMaxLimit     = 100
)

// histSearchDoc = la fiche d'index d'une discussion.
type histSearchDoc struct {
	V      int    `json:"v"`
	At     int64  `json:"at"`               // indexée à (Unix, s) — comparée à convMeta.Updated
	Apercu string `json:"apercu,omitempty"` // début du texte visible, borné (recallSnippet)
}

// histSearchHit = une discussion trouvée, telle que la reçoit l'UI.
type histSearchHit struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Created int64  `json:"created"`
	Updated int64  `json:"updated"`
	Turns   int    `json:"turns"`
	Score   int    `json:"score"`
	InTitle bool   `json:"in_title"`
	Snippet string `json:"snippet"`
}

// histSearchResult = le résultat complet d'une recherche.
type histSearchResult struct {
	State        string          `json:"state"` // ok | locked | no_terms
	Hits         []histSearchHit `json:"conversations"`
	Total        int             `json:"total"`         // correspondances AVANT pagination
	Indexed      int             `json:"indexed"`       // discussions du projet réellement indexées
	IndexedTotal int             `json:"indexed_total"` // discussions indexables du projet
	Terms        []string        `json:"terms"`         // termes cherchés (repliés, triés)
}

func histDocKey(id string) string   { return "d|" + id }
func histToksKey(id string) string  { return "t|" + id }
func histPostKey(tok string) string { return "p|" + tok }

// foldHistAccents replie les diacritiques d'un token. Appliqué SYMÉTRIQUEMENT
// à l'indexation et à la requête (voir le commentaire de tête).
func foldHistAccents(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case 'à', 'â', 'ä', 'á', 'ã', 'å':
			b.WriteByte('a')
		case 'é', 'è', 'ê', 'ë':
			b.WriteByte('e')
		case 'î', 'ï', 'í', 'ì':
			b.WriteByte('i')
		case 'ô', 'ö', 'ó', 'ò', 'õ':
			b.WriteByte('o')
		case 'ù', 'û', 'ü', 'ú':
			b.WriteByte('u')
		case 'ç':
			b.WriteByte('c')
		case 'ÿ':
			b.WriteByte('y')
		case 'œ':
			b.WriteString("oe")
		case 'æ':
			b.WriteString("ae")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// histTokens dérive les mots d'index d'une chaîne : recallTokens (réutilisé,
// jamais modifié), puis repli des accents et dédoublonnage, triés. Deux tokens
// distincts peuvent se replier sur le même (« cote » et « côté »).
func histTokens(s string) []string {
	set := recallTokens(s)
	seen := make(map[string]bool, len(set))
	out := make([]string, 0, len(set))
	for t := range set {
		f := foldHistAccents(t)
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// histTokenSet : variante « ensemble » de histTokens (titre, discussion ouverte).
func histTokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range histTokens(s) {
		out[t] = true
	}
	return out
}

// histTermMatches est LA règle de correspondance : un token correspond à un
// terme quand il COMMENCE par lui, jamais au milieu du mot (« eepseek » ne
// trouve pas « deepseek »). Servie au titre, à la discussion ouverte et à
// l'extrait ; le scan des postings applique la même règle au niveau de la clé
// (préfixe « p|<terme> »).
func histTermMatches(token, term string) bool {
	return strings.HasPrefix(token, term)
}

// histSetMatches : un terme correspond-il à l'un des tokens d'un ensemble ?
// Vrai au plus UNE fois par terme : le score compte les TERMES, pas les tokens.
func histSetMatches(set map[string]bool, term string) bool {
	for tok := range set {
		if histTermMatches(tok, term) {
			return true
		}
	}
	return false
}

// histQueryTerms replie, trie et plafonne les termes de la requête.
func histQueryTerms(q string) []string {
	terms := histTokens(q)
	if len(terms) > histSearchMaxTerms {
		terms = terms[:histSearchMaxTerms]
	}
	return terms
}

// histVisibleText concatène le texte VISIBLE d'un journal d'affichage, seule
// source indexée. On part du journal (vue UI) et NON des Messages : leur
// contenu peut être multimodal et porte le CORPS des sorties d'outil, qu'on ne
// veut pas indexer. On retient le message de l'utilisateur (« user »), la
// réponse (« content ») et le libellé d'outil (« tool_used.label »). Exclus :
// raisonnement, résultat d'outil, fichiers, deltas de contrôle.
func histVisibleText(log []LogEvent) string {
	var b strings.Builder
	for _, ev := range log {
		if s, ok := ev.Delta["user"].(string); ok {
			b.WriteString(s)
			b.WriteByte('\n')
		}
		if s, ok := ev.Delta["content"].(string); ok {
			b.WriteString(s)
			b.WriteByte('\n')
		}
		if tu, ok := ev.Delta["tool_used"].(map[string]any); ok {
			if l, ok := tu["label"].(string); ok {
				b.WriteString(l)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

// histFirstVisible : premier texte visible d'un journal (aperçu de la fiche).
func histFirstVisible(log []LogEvent) string {
	for _, ev := range log {
		if s, ok := ev.Delta["user"].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
		if s, ok := ev.Delta["content"].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
		if tu, ok := ev.Delta["tool_used"].(map[string]any); ok {
			if l, ok := tu["label"].(string); ok && strings.TrimSpace(l) != "" {
				return l
			}
		}
	}
	return ""
}

// histLogOf extrait le journal d'affichage du JSON d'une Conversation, sans
// rien décoder d'autre.
func histLogOf(body []byte) ([]LogEvent, bool) {
	var c struct {
		Log []LogEvent `json:"log"`
	}
	if json.Unmarshal(body, &c) != nil {
		return nil, false
	}
	return c.Log, true
}

// histIsWordRune : même découpage que recallTokens (sur la chaîne en minuscules).
func histIsWordRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= 'à' && r <= 'ÿ'
}

// histContextSnippet renvoie un extrait du texte autour du PREMIER mot qui
// correspond à l'un des termes (même règle de préfixe), ou "" sans
// correspondance. L'aperçu de la fiche ne montre que le début d'une
// discussion — c'est-à-dire, le plus souvent, son titre.
func histContextSnippet(text string, terms []string) string {
	const before, after = 60, 140
	runes := []rune(text)
	lower := make([]rune, len(runes)) // rune par rune : les positions restent alignées
	for i, r := range runes {
		lower[i] = unicode.ToLower(r)
	}
	for i := 0; i < len(lower); {
		if !histIsWordRune(lower[i]) {
			i++
			continue
		}
		j := i
		for j < len(lower) && histIsWordRune(lower[j]) {
			j++
		}
		if j-i >= 2 {
			w := foldHistAccents(string(lower[i:j]))
			for _, t := range terms {
				if !histTermMatches(w, t) {
					continue
				}
				from, to := max(0, i-before), min(len(runes), j+after)
				// Couper sur une espace pour ne pas commencer ni finir au milieu d'un mot.
				for from > 0 && from < i && runes[from-1] != ' ' && runes[from-1] != '\n' {
					from++
				}
				for to < len(runes) && to > j && runes[to] != ' ' && runes[to] != '\n' {
					to--
				}
				s := strings.Join(strings.Fields(string(runes[from:to])), " ")
				if from > 0 {
					s = "…" + s
				}
				if to < len(runes) {
					s += "…"
				}
				return s
			}
		}
		i = j
	}
	return ""
}

// --- accès bas niveau au bucket (chiffré comme les autres) --------------------

// histTxGetJSON lit et déchiffre une valeur dans une transaction ouverte.
// decodeMemContent est sûr ici : sur du clair il ne lit rien, sur du chiffré
// il n'appelle que currentDEK (mutex en RAM, jamais la base).
func histTxGetJSON(b *bolt.Bucket, key string, dst any) bool {
	raw := b.Get([]byte(key))
	if raw == nil {
		return false
	}
	plain, err := decodeMemContent(raw)
	if err != nil {
		return false // chiffré + verrouillé : illisible, pas « absent »
	}
	return json.Unmarshal(plain, dst) == nil
}

// histTxPutJSON sérialise, chiffre (encodeur résolu HORS transaction : bbolt
// n'est pas rentrant, voir memEncoderNow) et écrit.
func histTxPutJSON(b *bolt.Bucket, key string, v any, encode func([]byte) ([]byte, error)) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	enc, err := encode(raw)
	if err != nil {
		return err
	}
	return b.Put([]byte(key), enc)
}

// histPost ajoute sid à la liste du token (ensemble trié, idempotent).
func histPost(b *bolt.Bucket, tok, sid string, encode func([]byte) ([]byte, error)) error {
	var ids []string
	_ = histTxGetJSON(b, histPostKey(tok), &ids)
	i := sort.SearchStrings(ids, sid)
	if i < len(ids) && ids[i] == sid {
		return nil
	}
	ids = append(ids, "")
	copy(ids[i+1:], ids[i:])
	ids[i] = sid
	return histTxPutJSON(b, histPostKey(tok), ids, encode)
}

// histUnpost retire sid de la liste du token ; la clé disparaît si elle se vide.
func histUnpost(b *bolt.Bucket, tok, sid string, encode func([]byte) ([]byte, error)) error {
	var ids []string
	if !histTxGetJSON(b, histPostKey(tok), &ids) {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, x := range ids {
		if x != sid {
			out = append(out, x)
		}
	}
	if len(out) == len(ids) {
		return nil
	}
	if len(out) == 0 {
		return b.Delete([]byte(histPostKey(tok)))
	}
	return histTxPutJSON(b, histPostKey(tok), out, encode)
}

// --- maintenance : indexation / désindexation ---------------------------------

// histIndexAt (ré)indexe une discussion dans la base path, en UNE transaction :
// mots retirés depuis la dernière fois, fiche, mots ajoutés. Une fiche d'une
// autre version (ou absente) fait tout reposer. Les sémantiques d'ensemble
// rendent l'opération idempotente. Mémoire chiffrée mais verrouillée : erreur,
// rien n'est écrit en clair (le rattrapage reprendra au déverrouillage).
func histIndexAt(path, id string, log []LogEvent) error {
	if id == "" {
		return nil
	}
	encode, err := memEncoderNow() // HORS transaction (voir memEncoderNow)
	if err != nil {
		return err
	}
	toks := histTokens(histVisibleText(log))
	doc := histSearchDoc{V: histSearchIndexVersion, At: time.Now().Unix(),
		Apercu: recallSnippet(histFirstVisible(log), 200)}
	cacheBust(bkChatSearch)
	return withDBAt(path, func(d *bolt.DB) error {
		return d.Update(func(tx *bolt.Tx) error {
			b, err := tx.CreateBucketIfNotExists([]byte(bkChatSearch))
			if err != nil {
				return err
			}
			var prevDoc histSearchDoc
			var prev []string
			full := !histTxGetJSON(b, histDocKey(id), &prevDoc) || prevDoc.V != histSearchIndexVersion
			_ = histTxGetJSON(b, histToksKey(id), &prev)
			had := make(map[string]bool, len(prev))
			for _, t := range prev {
				had[t] = true
			}
			now := make(map[string]bool, len(toks))
			for _, t := range toks {
				now[t] = true
			}
			for _, t := range prev {
				if !now[t] {
					if err := histUnpost(b, t, id, encode); err != nil {
						return err
					}
				}
			}
			for _, t := range toks {
				if full || !had[t] {
					if err := histPost(b, t, id, encode); err != nil {
						return err
					}
				}
			}
			if err := histTxPutJSON(b, histToksKey(id), toks, encode); err != nil {
				return err
			}
			return histTxPutJSON(b, histDocKey(id), doc, encode)
		})
	})
}

// histUnindexAt retire une discussion de l'index. Si sa liste de mots est
// lisible, ses postings partent avec ; sinon (verrouillé) seules ses clés
// « d| » et « t| » sont supprimées, en laissant des identifiants ORPHELINS
// dans les postings — tolérés : la recherche écarte tout identifiant absent de
// l'index des discussions.
func histUnindexAt(path, id string) {
	if id == "" {
		return
	}
	encode, err := memEncoderNow()
	cacheBust(bkChatSearch)
	_ = withDBAt(path, func(d *bolt.DB) error {
		return d.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(bkChatSearch))
			if b == nil {
				return nil
			}
			var prev []string
			if err == nil && histTxGetJSON(b, histToksKey(id), &prev) {
				for _, t := range prev {
					if err := histUnpost(b, t, id, encode); err != nil {
						return err
					}
				}
			}
			if err := b.Delete([]byte(histToksKey(id))); err != nil {
				return err
			}
			return b.Delete([]byte(histDocKey(id)))
		})
	})
}

// histIndexWritten indexe les instantanés que l'écrivain vient de poser
// (chat_persist.go). Appelé DANS la boucle de l'écrivain, avant qu'elle ne
// marque le lot comme traité : un flush attend donc aussi l'index — c'est ce
// qui garantit qu'une suppression (forget, flush, puis histUnindexAt) passe
// toujours après la dernière indexation de la discussion.
func histIndexWritten(snaps []*convSnap) {
	for _, s := range snaps {
		if log, ok := histLogOf(s.body); ok {
			_ = histIndexAt(s.path, s.id, log) // erreur avalée : le rattrapage reprendra
		}
	}
}

// histIndexStoreWrite suit les écritures de la façade dans /internal/v1/store :
// une discussion écrite (bkChat/conv:<id>) est réindexée, une discussion
// effacée (DELETE, ou valeur vide) est retirée. Le contenu arrive scellé :
// decodeMemContent l'ouvre, la clé ne quitte pas ce process. Synchrone : la
// façade n'envoie la suppression d'une discussion qu'après la réponse à son
// écriture, l'ordre est donc tenu sans file.
func histIndexStoreWrite(bucket, key string, val []byte, deleted bool) {
	if bucket != bkChat || !strings.HasPrefix(key, "conv:") {
		return
	}
	id := strings.TrimPrefix(key, "conv:")
	if deleted || len(val) == 0 {
		histUnindexAt(dbPath(), id)
		return
	}
	plain, err := decodeMemContent(val)
	if err != nil {
		return
	}
	if log, ok := histLogOf(plain); ok {
		_ = histIndexAt(dbPath(), id, log)
	}
}

// --- rattrapage ---------------------------------------------------------------

var histBackfillRunning atomic.Bool

// startHistSearchBackfill lance le rattrapage en tâche de fond, au démarrage
// (après un délai : laisser moteur et lien démarrer d'abord) et au
// déverrouillage de la mémoire. Un seul à la fois.
func startHistSearchBackfill() { startHistSearchBackfillAfter(20 * time.Second) }

func startHistSearchBackfillAfter(d time.Duration) {
	if !histBackfillRunning.CompareAndSwap(false, true) {
		return
	}
	path := dbPath()
	go func() {
		defer histBackfillRunning.Store(false)
		time.Sleep(d)
		// La base visée au lancement, et elle seule : sous les tests, LOKI_HOME
		// change d'un test à l'autre — on ne rattrape pas dans la base d'un autre,
		// ni ne recrée celle d'un test terminé.
		if _, err := os.Stat(path); err != nil || dbPath() != path {
			return
		}
		histSearchIndexBackfill()
	}()
}

// histNeedsIndex : la discussion manque-t-elle à l'index, ou y est-elle plus
// ancienne que sa dernière écriture (une indexation ratée après l'écriture) ?
func histNeedsIndex(docs map[string]histSearchDoc, id string, updated int64) bool {
	d, ok := docs[id]
	return !ok || d.V != histSearchIndexVersion || d.At < updated
}

// histReadDocs lit toutes les fiches « d| » lisibles (petites : version, date,
// aperçu).
func histReadDocs() map[string]histSearchDoc {
	out := map[string]histSearchDoc{}
	_ = view(bkChatSearch, func(b *bolt.Bucket) error {
		c := b.Cursor()
		pfx := []byte("d|")
		for k, v := c.Seek(pfx); k != nil && bytes.HasPrefix(k, pfx); k, v = c.Next() {
			plain, err := decodeMemContent(v)
			if err != nil {
				continue
			}
			var d histSearchDoc
			if json.Unmarshal(plain, &d) == nil {
				out[string(k[len(pfx):])] = d
			}
		}
		return nil
	})
	return out
}

// histConvIDs : identifiants des discussions qui ont un CORPS en base (clés
// « conv:<id> » de bkChat), sans lire les valeurs.
func histConvIDs() map[string]bool {
	out := map[string]bool{}
	_ = view(bkChat, func(b *bolt.Bucket) error {
		c := b.Cursor()
		pfx := []byte("conv:")
		for k, _ := c.Seek(pfx); k != nil && bytes.HasPrefix(k, pfx); k, _ = c.Next() {
			out[string(k[len(pfx):])] = true
		}
		return nil
	})
	return out
}

// histSearchIndexBackfill indexe les discussions absentes de l'index, ou dont
// la fiche est plus ancienne que leur dernière écriture. S'arrête net
// verrouillé (corps illisibles) : il reprendra au déverrouillage.
func histSearchIndexBackfill() {
	if memLocked() {
		return
	}
	updated := map[string]int64{}
	for _, m := range convIndex() {
		updated[m.ID] = m.Updated
	}
	docs := histReadDocs()
	ids := make([]string, 0)
	for id := range histConvIDs() {
		if histNeedsIndex(docs, id, updated[id]) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		b, err := getStoreBytesErr(bkChat, convKey(id))
		if err != nil {
			if memLocked() {
				return
			}
			continue // illisible pour une autre raison (abîmée) : on saute
		}
		log, ok := histLogOf(b)
		if !ok {
			continue
		}
		if err := histIndexAt(dbPath(), id, log); err != nil {
			return
		}
		time.Sleep(30 * time.Millisecond) // ne pas monopoliser la base
	}
}

// --- recherche ----------------------------------------------------------------

// histSearchProject : le projet dont la barre latérale montre les discussions.
// Lu en base et non dans le cache de ce process : quand la façade tient la
// conversation, c'est elle qui change de projet.
func histSearchProject() string {
	if s := strings.TrimSpace(getStr(bkState, stActiveProjectKey)); s != "" {
		return s
	}
	return defaultProjectSlug
}

// histSearch est le cœur de la recherche, sur les discussions du projet
// `project` ("" = projet actif). Pertinence d'abord (score = 2 × termes du titre
// + termes du texte, même ratio que recall_search), puis plus récente, puis
// identifiant. offset/limit découpent APRÈS le tri ; limit <= 0 = tout.
func histSearch(q, project string, offset, limit int) histSearchResult {
	res := histSearchResult{State: "ok", Hits: make([]histSearchHit, 0), Terms: make([]string, 0)}
	terms := histQueryTerms(q)
	if len(terms) == 0 {
		res.State = "no_terms"
		return res
	}
	// Verrouillé décidé AVANT toute lecture : « illisible » et « absent » se
	// confondent au fond. Pour une recherche, un vide serait un mensonge.
	if memLocked() {
		res.State = "locked"
		return res
	}
	res.Terms = terms
	if project == "" {
		project = histSearchProject()
	}
	metaByID := map[string]convMeta{}
	for _, m := range convIndex() {
		if convProjectOf(m) == project {
			metaByID[m.ID] = m
		}
	}

	// Discussion ouverte : quand CE process la tient, son journal en RAM est
	// plus frais que sa dernière écriture (un tour en cours). Copié sous c.mu
	// puis relâché : on tokenise hors verrou, jamais en bloquant une génération.
	// Quand la façade la tient, le journal d'ici est périmé : l'index (alimenté
	// par ses écritures) fait foi.
	var liveID string
	var liveLog []LogEvent
	if !facadeChat() {
		liveID = convActiveCached()
		conv.mu.Lock()
		liveLog = append([]LogEvent(nil), conv.Log...)
		conv.mu.Unlock()
		if _, ok := metaByID[liveID]; !ok || len(liveLog) == 0 {
			liveID, liveLog = "", nil
		}
	}

	titleHits := map[string]int{}
	for id, m := range metaByID {
		set := histTokenSet(m.Title)
		n := 0
		for _, t := range terms {
			if histSetMatches(set, t) {
				n++
			}
		}
		if n > 0 {
			titleHits[id] = n
		}
	}

	// UNE vue sur l'index : postings des termes (scan PRÉFIXE : les clés sont
	// triées, toutes celles qui commencent par « p|<terme> » se suivent) et
	// fiches des discussions du projet (couverture, aperçu). Aucun corps lu.
	bodyHits := map[string]int{}
	docs := map[string]histSearchDoc{}
	_ = view(bkChatSearch, func(b *bolt.Bucket) error {
		for _, t := range terms {
			prefix := []byte(histPostKey(t))
			matched := map[string]bool{} // un terme ne compte qu'UNE fois par discussion
			c := b.Cursor()
			for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
				plain, err := decodeMemContent(v)
				if err != nil {
					continue
				}
				var ids []string
				if json.Unmarshal(plain, &ids) != nil {
					continue
				}
				for _, sid := range ids {
					matched[sid] = true
				}
			}
			for sid := range matched {
				bodyHits[sid]++
			}
		}
		for id := range metaByID {
			var d histSearchDoc
			if histTxGetJSON(b, histDocKey(id), &d) && d.V == histSearchIndexVersion {
				docs[id] = d
			}
		}
		return nil
	})

	// Discussion ouverte : son journal en RAM REMPLACE sa copie indexée (et non
	// +=) — une seule entrée par identifiant, jamais de double compte.
	if liveID != "" {
		set := histTokenSet(histVisibleText(liveLog))
		n := 0
		for _, t := range terms {
			if histSetMatches(set, t) {
				n++
			}
		}
		bodyHits[liveID] = n
	}

	score := func(id string) int { return 2*titleHits[id] + bodyHits[id] }
	hits := make([]histSearchHit, 0)
	for id, m := range metaByID {
		if sc := score(id); sc > 0 {
			hits = append(hits, histSearchHit{ID: id, Title: m.Title, Created: m.Created,
				Updated: m.Updated, Turns: m.Turns, Score: sc, InTitle: titleHits[id] > 0})
		}
	}
	// Les identifiants des postings absents de l'index des discussions (supprimées
	// verrouillé, ou d'un autre projet) ne sont jamais parcourus : seul metaByID l'est.
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if hits[i].Updated != hits[j].Updated {
			return hits[i].Updated > hits[j].Updated
		}
		return hits[i].ID < hits[j].ID
	})
	res.Total = len(hits)
	if limit > 0 {
		off := max(0, min(offset, len(hits)))
		hits = hits[off:min(off+limit, len(hits))]
	}
	histFillSnippets(hits, terms, docs, liveID, liveLog)
	res.Hits = hits

	// Couverture : le dénominateur est l'ensemble des discussions RÉELLEMENT
	// indexables du projet — une entrée d'index sans corps en base (discussion
	// toute neuve, jamais écrite) ne le sera jamais, la compter laisserait
	// l'UI annoncer « indexation en cours » pour toujours (constat amont).
	// Invariant : Indexed <= IndexedTotal.
	bodies := histConvIDs()
	for id, m := range metaByID {
		if !bodies[id] && id != liveID {
			continue
		}
		res.IndexedTotal++
		if id == liveID {
			res.Indexed++
			continue
		}
		if !histNeedsIndex(docs, id, m.Updated) {
			res.Indexed++
		}
	}
	return res
}

// histFillSnippets pose l'extrait de chaque résultat de la PAGE : le passage
// autour du premier mot trouvé, relu dans le corps (une seule ouverture de la
// base pour toute la page) ; à défaut — corps illisible, mot trouvé seulement
// dans le titre — l'aperçu de la fiche. Le classement, lui, ne dépend jamais
// des corps.
func histFillSnippets(hits []histSearchHit, terms []string, docs map[string]histSearchDoc, liveID string, liveLog []LogEvent) {
	if len(hits) == 0 {
		return
	}
	texts := map[string]string{}
	if liveID != "" {
		texts[liveID] = histVisibleText(liveLog)
	}
	_ = view(bkChat, func(b *bolt.Bucket) error {
		for _, h := range hits {
			if _, ok := texts[h.ID]; ok {
				continue
			}
			raw := b.Get([]byte(convKey(h.ID)))
			if raw == nil {
				continue
			}
			plain, err := decodeMemContent(raw)
			if err != nil {
				continue
			}
			if log, ok := histLogOf(plain); ok {
				texts[h.ID] = histVisibleText(log)
			}
		}
		return nil
	})
	for i := range hits {
		s := ""
		if txt, ok := texts[hits[i].ID]; ok {
			s = histContextSnippet(txt, terms)
		}
		if s == "" {
			s = docs[hits[i].ID].Apercu
		}
		if s == "" && liveID == hits[i].ID {
			s = recallSnippet(histFirstVisible(liveLog), 200)
		}
		hits[i].Snippet = s
	}
}

// handleConvSearch : GET /api/conversations/search?q=&offset=&limit=.
// Discussions du projet actif dont le titre OU le texte contient les mots
// cherchés (préfixes), avec un extrait. Toujours servie par ce process, même
// quand la façade tient la conversation (voir le commentaire de tête).
func handleConvSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "GET attendu"})
		return
	}
	q := r.URL.Query()
	if utf8.RuneCountInString(q.Get("q")) > 500 {
		sendJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "requête trop longue"})
		return
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit <= 0 {
		limit = histSearchDefaultLimit
	}
	limit = min(limit, histSearchMaxLimit)
	res := histSearch(q.Get("q"), "", max(0, offset), limit)
	// Index incomplet (discussions d'avant la fonctionnalité, indexation ratée) :
	// on relance le rattrapage plutôt que d'attendre le prochain démarrage. L'UI
	// annonce la couverture partielle en attendant.
	if res.State == "ok" && res.Indexed < res.IndexedTotal {
		startHistSearchBackfillAfter(time.Second)
	}
	sendJSON(w, http.StatusOK, map[string]any{"ok": true, "state": res.State, "terms": res.Terms,
		"conversations": res.Hits, "total": res.Total,
		"indexed": res.Indexed, "indexed_total": res.IndexedTotal})
}
