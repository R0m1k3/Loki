package loki

// chat_persist.go — l'écriture de la discussion SORT du chemin du premier token.
//
// persist() faisait tout en ligne : marshal, puis putStoreBytes (ouverture de la
// base, commit, fsync), puis convTouchMeta (relecture de l'index, second commit,
// second fsync). Dans StartTurn, c'était AVANT `go c.generate` : 26-30 ms
// mesurés sur un NVMe Windows, bien davantage sur le /mnt/user d'Unraid (FUSE)
// ou un disque de parité — payés à chaque message avant que la requête ne parte
// vers llama-server. Même facture au milieu d'un tour (vérification du mode
// Code) et pour chaque résultat d'outil coupé (« voir plus »).
//
// Désormais :
//   - l'INSTANTANÉ reste synchrone et pris sous c.mu (images → références,
//     marshal, identifiant de la discussion) : rien ne peut le muter pendant
//     qu'on le prend, et il dit toujours à quelle discussion il appartient ;
//   - seule l'E/S bbolt part à un écrivain UNIQUE, qui fusionne ce qui s'est
//     accumulé (dernier instantané de chaque discussion + résultats d'outils) en
//     UNE transaction — un fsync au lieu de deux, ou de vingt sur un tour d'agent ;
//   - seuls les chemins chauds sont asynchrones (StartTurn, persistance en cours
//     de tour, résultats d'outils). Fin de tour, reset, bascules et suppression
//     restent synchrones : ils attendent que l'écrivain ait tout vidé, eux
//     compris. Aucun n'est sur le chemin du premier token, et un redémarrage
//     juste après eux ne doit rien perdre.
//
// Ce qui atteint le modèle ne change pas d'un octet : il lit c.Messages en
// mémoire, et les résultats complets des outils ne servent qu'au « voir plus »
// de l'UI. Le risque accepté : un crash dans les millisecondes qui suivent un
// envoi peut perdre le message tout juste envoyé — le même que celui d'un crash
// en pleine génération, avant ce changement, pour la réponse.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

// convSnap est un instantané complet d'une discussion, prêt à écrire.
type convSnap struct {
	path    string // base visée, figée à la prise (voir withDBAt)
	id      string // discussion à laquelle appartient le contenu
	seq     uint64 // ordre de prise : le plus grand l'emporte toujours
	body    []byte // JSON de Conversation
	title   string // titre déduit du premier message (convSummary)
	turns   int
	project string // projet d'une entrée d'index à créer (voir convTouchMetaIn)
}

// toolResJob est un résultat d'outil complet à ranger dans bkToolRes.
type toolResJob struct {
	path  string
	key   string
	plain string
}

// convPersister sérialise TOUTES les écritures de discussion. Un seul écrivain à
// la fois : l'ordre des instantanés ne dépend donc que de seq, jamais d'une
// course entre deux goroutines d'écriture.
type convPersister struct {
	mu      sync.Mutex
	cond    *sync.Cond
	snaps   map[string]*convSnap // dernier instantané en attente, par discussion
	res     []toolResJob         // résultats d'outils en attente, dans l'ordre
	mem     map[string]string    // résultats pas encore écrits : « voir plus » les sert d'ici
	written map[string]uint64    // seq du dernier instantané écrit, par discussion
	deleted map[string]bool      // discussions supprimées : plus rien ne doit les réécrire
	queued  uint64               // tickets délivrés
	done    uint64               // tickets traités (écrits, abandonnés ou en échec)
	running bool
}

var (
	persistQ = newConvPersister()
	// persistSeq numérote les instantanés. Incrémenté SOUS c.mu : l'ordre des
	// numéros est donc celui des états de la discussion.
	persistSeq atomic.Uint64
	// persistCommit écrit un lot ; remplaçable par les tests pour observer
	// l'ordre des écritures ou les ralentir.
	persistCommit = commitPersistBatch
)

func newConvPersister() *convPersister {
	p := &convPersister{
		snaps:   map[string]*convSnap{},
		mem:     map[string]string{},
		written: map[string]uint64{},
		deleted: map[string]bool{},
	}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// enqueueSnap confie un instantané à l'écrivain et renvoie son ticket. Un
// instantané en attente pour la même discussion est remplacé par le plus récent :
// écrire l'ancien ne servirait qu'à l'écraser aussitôt.
func (p *convPersister) enqueueSnap(s *convSnap) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.deleted[s.id] {
		if cur := p.snaps[s.id]; cur == nil || s.seq > cur.seq {
			p.snaps[s.id] = s
		}
	}
	return p.kickLocked()
}

// enqueueToolRes confie un résultat d'outil à l'écrivain. Il est servi depuis
// la mémoire tant qu'il n'est pas sur disque.
func (p *convPersister) enqueueToolRes(j toolResJob) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mem[j.key] = j.plain
	p.res = append(p.res, j)
	return p.kickLocked()
}

// kickLocked délivre un ticket et démarre l'écrivain s'il dort. p.mu détenu.
func (p *convPersister) kickLocked() uint64 {
	p.queued++
	if !p.running {
		p.running = true
		go p.run()
	}
	return p.queued
}

// run vide la file par lots, jusqu'à ce qu'elle soit vide.
func (p *convPersister) run() {
	for {
		p.mu.Lock()
		if len(p.snaps) == 0 && len(p.res) == 0 {
			p.running = false
			p.done = p.queued
			p.cond.Broadcast()
			p.mu.Unlock()
			return
		}
		upto := p.queued
		snaps := make([]*convSnap, 0, len(p.snaps))
		for id, s := range p.snaps {
			if !p.deleted[id] && s.seq > p.written[id] {
				snaps = append(snaps, s)
			}
		}
		res := make([]toolResJob, 0, len(p.res))
		for _, j := range p.res {
			if !p.deleted[toolResConv(j.key)] {
				res = append(res, j)
			}
		}
		p.snaps, p.res = map[string]*convSnap{}, nil
		p.mu.Unlock()

		sort.Slice(snaps, func(i, j int) bool { return snaps[i].seq < snaps[j].seq })
		wrote, err := persistCommitByPath(snaps, res)

		p.mu.Lock()
		for _, s := range wrote {
			if s.seq > p.written[s.id] {
				p.written[s.id] = s.seq
			}
		}
		if err != nil {
			// Les résultats restent servis depuis la mémoire : le « voir plus » de
			// cette session marche encore, seul un redémarrage les perdrait. Un
			// instantané raté sera remplacé par le suivant (fin de tour au plus tard).
			fmt.Fprintf(os.Stderr, "[persist] écriture différée échouée (%d instantané(s), %d résultat(s) d'outil gardé(s) en mémoire) : %v\n", len(snaps)-len(wrote), len(res), err)
		} else {
			for _, j := range res {
				if p.mem[j.key] == j.plain {
					delete(p.mem, j.key)
				}
			}
		}
		p.done = upto
		p.cond.Broadcast()
		p.mu.Unlock()
	}
}

// wait bloque jusqu'à ce que le ticket (et tous ceux d'avant) soit traité.
func (p *convPersister) wait(ticket uint64) {
	p.mu.Lock()
	for p.done < ticket {
		p.cond.Wait()
	}
	p.mu.Unlock()
}

// flush attend que tout ce qui a été confié jusqu'ici soit traité.
func (p *convPersister) flush() {
	p.mu.Lock()
	t := p.queued
	p.mu.Unlock()
	p.wait(t)
}

// forget marque une discussion comme supprimée : ses instantanés et résultats en
// attente sont jetés, et plus rien ne la réécrira — sans ça, un instantané en
// vol la ferait renaître (contenu ET entrée d'index) juste après sa suppression.
func (p *convPersister) forget(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleted[id] = true
	delete(p.snaps, id)
	p.dropToolResLocked(id + ".")
}

// dropToolRes oublie les résultats en attente d'une discussion.
func (p *convPersister) dropToolRes(prefix string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropToolResLocked(prefix)
}

func (p *convPersister) dropToolResLocked(prefix string) {
	for k := range p.mem {
		if strings.HasPrefix(k, prefix) {
			delete(p.mem, k)
		}
	}
	kept := p.res[:0]
	for _, j := range p.res {
		if !strings.HasPrefix(j.key, prefix) {
			kept = append(kept, j)
		}
	}
	p.res = kept
}

// toolResPending relit un résultat pas encore sur disque.
func (p *convPersister) toolResPending(key string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.mem[key]
	return s, ok
}

// toolResConv extrait l'identifiant de discussion d'une clé « <id>.<aléa> ».
func toolResConv(key string) string {
	if i := strings.LastIndexByte(key, '.'); i > 0 {
		return key[:i]
	}
	return key
}

// persistExitWait borne l'attente de l'écrivain avant une sortie : le délai
// d'ouverture de la base (5 s, withDBAt) plus une marge.
const persistExitWait = 6 * time.Second

// flushPersister attend que les écritures différées soient sur disque, au plus
// d pour ne jamais retenir un arrêt sur une base bloquée. À appeler avant toute
// sortie volontaire du process (redémarrage, « Quitter », exec).
func flushPersister(d time.Duration) {
	ok := make(chan struct{})
	go func() {
		persistQ.flush()
		close(ok)
	}()
	select {
	case <-ok:
	case <-time.After(d):
	}
}

// persistCommitByPath regroupe le lot par base (une seule en production) et
// l'écrit. Renvoie les instantanés effectivement écrits.
func persistCommitByPath(snaps []*convSnap, res []toolResJob) ([]*convSnap, error) {
	paths := map[string]bool{}
	for _, s := range snaps {
		paths[s.path] = true
	}
	for _, j := range res {
		paths[j.path] = true
	}
	var wrote []*convSnap
	var firstErr error
	for path := range paths {
		var ps []*convSnap
		var pr []toolResJob
		for _, s := range snaps {
			if s.path == path {
				ps = append(ps, s)
			}
		}
		for _, j := range res {
			if j.path == path {
				pr = append(pr, j)
			}
		}
		w, err := persistCommit(path, ps, pr)
		wrote = append(wrote, w...)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return wrote, firstErr
}

// commitPersistBatch écrit un lot en UNE transaction : les résultats d'outils,
// puis chaque instantané avec la mise à jour de son entrée d'index. Avant, le
// contenu et l'index étaient deux commits (deux fsync), et la relecture de
// l'index hors de tout verrou pouvait écraser un renommage concurrent.
//
// Mémoire chiffrée mais verrouillée : rien n'est écrit en clair, comme
// putStoreBytes — les instantanés sont sautés (le fil reste en RAM), les
// résultats d'outils renvoient une erreur et restent servis depuis la mémoire.
func commitPersistBatch(path string, snaps []*convSnap, res []toolResJob) ([]*convSnap, error) {
	// Hors transaction : memEncActive relit la configuration, donc rouvrirait la
	// base — bbolt n'est pas rentrant (voir memEncoderNow).
	encode, encErr := memEncoderNow()
	if len(res) > 0 && encErr != nil {
		return nil, encErr
	}
	now := time.Now().Unix()
	convIndexMu.Lock()
	defer convIndexMu.Unlock()
	cacheBust(bkChat)
	cacheBust(bkToolRes)
	var wrote []*convSnap
	err := withDBAt(path, func(d *bolt.DB) error {
		return d.Update(func(tx *bolt.Tx) error {
			wrote = wrote[:0]
			if len(res) > 0 {
				b, err := tx.CreateBucketIfNotExists([]byte(bkToolRes))
				if err != nil {
					return err
				}
				for _, j := range res {
					enc, err := encode([]byte(j.plain))
					if err != nil {
						return err
					}
					if err := b.Put([]byte(j.key), enc); err != nil {
						return err
					}
				}
			}
			if len(snaps) == 0 || encErr != nil {
				return nil
			}
			b, err := tx.CreateBucketIfNotExists([]byte(bkChat))
			if err != nil {
				return err
			}
			// Index illisible (chiffré par une autre clé, abîmé) : on écrit le
			// contenu mais on ne touche PAS à l'index — le réécrire depuis une
			// lecture vide effacerait toutes les autres discussions de la liste.
			var idx []convMeta
			idxOK := true
			if raw := b.Get([]byte(ckIndex)); len(raw) > 0 {
				dec, err := decodeMemContent(raw)
				if err != nil || json.Unmarshal(dec, &idx) != nil {
					idxOK = false
				}
			}
			for _, s := range snaps {
				enc, err := encode(s.body)
				if err != nil {
					return err
				}
				if err := b.Put([]byte(convKey(s.id)), enc); err != nil {
					return err
				}
				if idxOK {
					idx = convTouchMetaIn(idx, s, now)
				}
				wrote = append(wrote, s)
			}
			if !idxOK {
				return nil
			}
			ib, err := json.Marshal(idx)
			if err != nil {
				return err
			}
			enc, err := encode(ib)
			if err != nil {
				return err
			}
			return b.Put([]byte(ckIndex), enc)
		})
	})
	if err != nil {
		return nil, err
	}
	return wrote, nil
}

// convTouchMetaIn rafraîchit l'entrée d'index d'un instantané : date, nombre
// d'échanges, titre déduit si l'utilisateur n'en a pas choisi. Une discussion
// absente de l'index y est ajoutée — c'est ce qui recolle une discussion créée
// mémoire verrouillée (son entrée n'avait pas pu s'écrire). Une discussion
// SUPPRIMÉE n'arrive jamais ici : l'écrivain l'a écartée (forget).
func convTouchMetaIn(idx []convMeta, s *convSnap, now int64) []convMeta {
	for i := range idx {
		if idx[i].ID != s.id {
			continue
		}
		idx[i].Updated = now
		idx[i].Turns = s.turns
		if idx[i].Title == "" {
			idx[i].Title = s.title
		}
		return idx
	}
	return append(idx, convMeta{ID: s.id, Title: s.title, Created: now, Updated: now, Turns: s.turns, Project: s.project})
}

// --- Discussion active, gardée en RAM -----------------------------------------
//
// convEnsureActive relisait bkChat/active à CHAQUE appel : une ouverture de base
// (3-4 ms sur Windows) pour le mode Code, l'espace de travail, les résultats
// d'outils, la télémétrie… plusieurs fois par tour. Un seul process possède la
// conversation (relay_link.go) et toutes les bascules passent par
// convActivate : le cache ne peut donc pas diverger de la base, exactement
// comme activeProjCache pour le projet actif.
//
// Le cache sert aussi de LIEN entre le contenu en mémoire et son identifiant :
// convActivate le change sous c.mu, en même temps que le contenu (loadFrom). Un
// instantané pris sous c.mu lit donc toujours l'identifiant du contenu qu'il
// sérialise — jamais l'ancien fil sous le nouvel identifiant, même si une
// bascule s'intercale entre deux étapes de persist.

type convActiveRef struct{ path, id string }

var (
	// convActiveMu sérialise la création de l'identifiant et les bascules.
	// Ordre des verrous : convActiveMu, puis c.mu (loadFrom). Jamais l'inverse —
	// d'où la lecture sans verrou du cache chaud.
	convActiveMu    sync.Mutex
	convActiveCache atomic.Pointer[convActiveRef]
	// convIndexMu sérialise les lectures-modifications-écritures de l'index :
	// celles des opérations de discussion et celle de l'écrivain.
	convIndexMu sync.Mutex
)

// convActiveCached renvoie l'identifiant en cache pour la base courante, ou "".
func convActiveCached() string {
	if r := convActiveCache.Load(); r != nil && r.path == dbPath() {
		return r.id
	}
	return ""
}

// convActiveRemember met l'identifiant en cache. Un pointeur encore chiffré par
// une ancienne version (healPlainStoreKeys) n'est pas mis en cache : il sera
// relu en clair après la réparation.
func convActiveRemember(id string) {
	if id == "" || looksEncrypted([]byte(id)) {
		convActiveCache.Store(nil)
		return
	}
	convActiveCache.Store(&convActiveRef{path: dbPath(), id: id})
}

// convActivate fait de id la discussion active et charge b en mémoire, sous
// convActiveMu : le pointeur en base, le cache et le contenu changent ensemble.
func convActivate(id string, b []byte) {
	convActiveMu.Lock()
	defer convActiveMu.Unlock()
	_ = putStr(bkChat, ckActive, id)
	conv.loadFrom(id, b)
}

// snapshot prend l'instantané à écrire. L'appelant NE doit PAS détenir mu.
func (c *Conversation) snapshot() *convSnap {
	// Hors verrou : au premier appel, convEnsureActive lit (voire crée) la clé
	// en base. Ensuite le cache est chaud et relu sous c.mu.
	id0 := convEnsureActive()
	project := activeProjectSlug()
	c.mu.Lock()
	defer c.mu.Unlock()
	id := id0
	if c == conv {
		if cid := convActiveCached(); cid != "" {
			id = cid
		}
	}
	// Images en base64 → références (chat_images.go) avant d'écrire : une photo
	// jointe pesait des Mo, réécrits en entier à CHAQUE fin de tour.
	refImagesInMessages(c.Messages)
	// En plein tour, le journal porte encore un événement par token et chaque
	// frappe d'outil : on écrit la forme que produira la fin de tour
	// (compactLog), sans toucher au journal en mémoire que suivent les abonnés.
	full := c.Log
	if c.Generating {
		c.Log = compactLog(full)
	}
	b, err := json.Marshal(c)
	c.Log = full
	if err != nil {
		return nil
	}
	turns := 0
	for _, ev := range full {
		if _, ok := ev.Delta["user"]; ok {
			turns++
		}
	}
	return &convSnap{
		path:    dbPath(),
		id:      id,
		seq:     persistSeq.Add(1),
		body:    b,
		title:   convSummary(c.Messages),
		turns:   turns,
		project: project,
	}
}

// persist enregistre l'état sous sa discussion et ATTEND l'écriture : fin de
// tour, reset, bascules, compactage manuel. Tout ce qui était en attente avant
// lui est écrit aussi. L'appelant NE doit PAS détenir mu.
func (c *Conversation) persist() {
	if s := c.snapshot(); s != nil {
		persistQ.wait(persistQ.enqueueSnap(s))
		return
	}
	persistQ.flush()
}

// persistAsync prend l'instantané tout de suite mais laisse l'écriture à
// l'écrivain : pour les chemins qui précèdent un appel au modèle (StartTurn,
// persistance en cours de tour). L'identifiant de la discussion est résolu ici,
// de façon synchrone — sur une base neuve, generate trouve donc la discussion
// active déjà créée, comme avant. L'appelant NE doit PAS détenir mu.
func (c *Conversation) persistAsync() {
	if s := c.snapshot(); s != nil {
		persistQ.enqueueSnap(s)
	}
}
