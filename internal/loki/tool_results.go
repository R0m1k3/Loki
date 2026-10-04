package loki

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync/atomic"

	bolt "go.etcd.io/bbolt"
)

// tool_results.go — résultats COMPLETS des outils, pour le bouton « voir plus ».
// Repris d'AJEAN 0.15.5 / 0.15.7. Chiffré comme les discussions quand le
// chiffrement mémoire est actif (bkToolRes figure dans encryptedBuckets).
//
// Le flux n'envoie à l'UI qu'un aperçu (toolPreviewChars) ; le reste se charge
// au clic. Il ne peut pas être cherché dans conv.Messages : pendant un tour
// d'agent, les messages tool n'y sont versés qu'à la FIN du tour, et après un
// compactage les anciens ont disparu de la vue modèle. On garde donc chaque
// résultat coupé dans un bucket dédié, sous un id PROPRE (pas le tool_call_id,
// que certains parseurs réutilisent d'un tour à l'autre).
//
// Rangement PAR DISCUSSION : la clé est « <id de discussion>.<aléa> ». Les
// résultats vivent aussi longtemps que leur discussion (supprimés avec elle,
// voir deleteToolResultsFor) ; les orphelins sont nettoyés de temps en temps.

const (
	bkToolRes       = "toolres"
	toolResPruneGap = 200 // nettoyage tenté toutes les N écritures
)

var toolResWrites atomic.Int64

func toolResID(sid string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	if sid = strings.TrimSpace(sid); sid == "" {
		sid = "nosession"
	}
	return sid + "." + hex.EncodeToString(b[:])
}

// saveToolResult enregistre un résultat complet de la discussion ACTIVE et
// renvoie son id ("" en cas d'échec : l'appelant envoie alors le résultat
// entier dans le flux).
//
// L'écriture est confiée à l'écrivain de la discussion (chat_persist.go) : un
// tour d'agent qui coupe vingt résultats payait vingt fsync entre l'outil et
// l'étape suivante. Le résultat est servi depuis la mémoire jusqu'à ce qu'il
// soit sur disque (loadToolResult).
func saveToolResult(result string) string {
	id := toolResID(convActiveID())
	// Chiffrement actif mais mémoire verrouillée : on refuse d'écrire en clair,
	// et l'appelant envoie alors le résultat entier dans le flux. Vérifié ICI,
	// pas à l'écriture : un id rendu doit désigner un résultat qu'on écrira.
	if id == "" {
		return ""
	}
	if _, err := memEncoderNow(); err != nil {
		return ""
	}
	persistQ.enqueueToolRes(toolResJob{path: dbPath(), key: id, plain: result})
	if toolResWrites.Add(1)%toolResPruneGap == 0 {
		go pruneToolResults()
	}
	return id
}

// loadToolResult relit un résultat enregistré.
func loadToolResult(id string) (string, bool) {
	if strings.ContainsAny(id, "/\\") {
		return "", false
	}
	if s, ok := persistQ.toolResPending(id); ok {
		return s, true
	}
	b, err := getStoreBytesErr(bkToolRes, id)
	if err != nil || b == nil {
		return "", false
	}
	return string(b), true
}

// deleteToolResultsFor supprime les résultats d'une discussion supprimée.
func deleteToolResultsFor(sid string) {
	if sid == "" {
		return
	}
	prefix := sid + "."
	// D'abord l'écrivain (chat_persist.go) : ce qui l'attend est jeté, et ce
	// qu'il a déjà pris dans son lot est annulé — sans ça, il l'écrirait après
	// l'effacement ci-dessous et le résultat renaîtrait. Annulé AVANT notre
	// transaction : l'écrivain vérifie dans la sienne, et bbolt les sérialise.
	persistQ.dropToolRes(prefix)
	_ = update(bkToolRes, func(b *bolt.Bucket) error {
		c := b.Cursor()
		var keys [][]byte
		for k, _ := c.Seek([]byte(prefix)); k != nil && strings.HasPrefix(string(k), prefix); k, _ = c.Next() {
			keys = append(keys, append([]byte(nil), k...))
		}
		for _, k := range keys {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

// pruneToolResults supprime les résultats de discussions qui n'existent plus.
func pruneToolResults() {
	// Mémoire verrouillée : l'index des discussions est illisible et reviendrait
	// vide — tout passerait pour orphelin. On attend le déverrouillage.
	if memEncActive() && !memUnlocked() {
		return
	}
	alive := map[string]bool{getStr(bkChat, ckActive): true}
	for _, m := range convIndex() {
		alive[m.ID] = true
	}
	_ = update(bkToolRes, func(b *bolt.Bucket) error {
		var orphans []string
		_ = b.ForEach(func(k, _ []byte) error {
			key := string(k)
			if i := strings.LastIndexByte(key, '.'); i > 0 && key[:i] != "nosession" && !alive[key[:i]] {
				orphans = append(orphans, key)
			}
			return nil
		})
		for _, k := range orphans {
			if err := b.Delete([]byte(k)); err != nil {
				return err
			}
		}
		return nil
	})
}
