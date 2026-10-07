package loki

// mem_store.go — chiffrement transparent de VALEURS de la base (bbolt), pour
// étendre le chiffrement au-delà des pages mémoire : conversations et archives
// contiennent aussi des infos sensibles.
//
// Même enveloppe que les pages (encPage/decPage + magic) : une valeur est
// chiffrée quand le chiffrement est actif ET la mémoire déverrouillée, en clair
// sinon. La détection se fait sur le magic, donc clair et chiffré cohabitent
// (utile pendant une migration ou un état verrouillé).

import (
	"encoding/json"
	"errors"
	"strings"
)

// errStoreLocked : écriture chiffrée demandée alors que la DEK n'est pas en RAM.
var errStoreLocked = errors.New("valeur chiffrée, mémoire verrouillée")

// putStoreBytes écrit une valeur, chiffrée si le chiffrement est actif ET
// déverrouillé. Si actif mais verrouillé, renvoie errStoreLocked SANS écrire
// (on ne remplace jamais une valeur chiffrée par du clair).
func putStoreBytes(bucket, key string, plain []byte) error {
	out, err := encodeMemContent(plain)
	if err != nil {
		return errStoreLocked
	}
	return putBytes(bucket, key, out)
}

// getStoreBytes lit une valeur et la déchiffre au besoin. Renvoie (nil, false)
// si absente, ou si chiffrée alors que la mémoire est verrouillée.
func getStoreBytes(bucket, key string) ([]byte, bool) {
	raw := getBytes(bucket, key)
	if len(raw) == 0 {
		return nil, false
	}
	dec, err := decodeMemContent(raw)
	if err != nil {
		return nil, false
	}
	return dec, true
}

// storeBytes : getStoreBytes réduit à sa valeur, pour les appelants qui
// traitent déjà « absent » et « illisible » de la même façon (un fil vide).
func storeBytes(bucket, key string) []byte {
	b, _ := getStoreBytes(bucket, key)
	return b
}

// getStoreBytesErr fait la même lecture que getStoreBytes mais SANS avaler
// l'erreur d'accès (getBytes, lui, la jette — c'est le piège documenté sur
// getBytesErr : « je n'ai pas pu lire » et « il n'y a rien » ne veulent pas dire
// la même chose). Une valeur absente reste (nil, nil) — err n'est non-nil QUE
// si la base elle-même n'a pas pu être ouverte/lue (verrou bbolt disputé, disque
// indisponible…), pas pour une clé qui n'existe simplement pas encore.
func getStoreBytesErr(bucket, key string) ([]byte, error) {
	raw, err := getBytesErr(bucket, key)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	dec, err := decodeMemContent(raw)
	if err != nil {
		// Chiffré mais verrouillé (ou payload corrompu) : légitime, pas un échec
		// d'ACCÈS — même repli que getStoreBytes.
		return nil, nil
	}
	return dec, nil
}

// putStoreJSON sérialise puis écrit (chiffré si actif+déverrouillé).
func putStoreJSON(bucket, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return putStoreBytes(bucket, key, b)
}

// getStoreJSON lit puis décode (déchiffre au besoin). false si absent/verrouillé.
func getStoreJSON(bucket, key string, dst any) bool {
	b, ok := getStoreBytes(bucket, key)
	if !ok {
		return false
	}
	return json.Unmarshal(b, dst) == nil
}

// plainStoreKey : clés d'un bucket chiffré qui restent EN CLAIR, parce que le
// code les lit et les écrit avec getStr/putStr — des pointeurs et drapeaux, pas
// des données : la discussion active (son id), le mode Chat|Code d'une
// discussion, la puce « passer en mode Code » déjà montrée. Chiffrées par
// reencryptBucket, elles revenaient à la lecture comme du charabia : la
// discussion active devenait introuvable, le mode Code et ses critères
// disparaissaient dès l'activation du chiffrement.
func plainStoreKey(bucket, key string) bool {
	if bucket != bkChat {
		return false
	}
	return key == ckActive || strings.HasPrefix(key, "mode:") || strings.HasPrefix(key, "hinted:")
}

// healPlainStoreKeys remet en clair les clés-pointeurs qu'une version
// précédente avait chiffrées (voir plainStoreKey). À appeler dès que la DEK est
// en RAM, AVANT de recharger la discussion active. Sans DEK, ne fait rien.
func healPlainStoreKeys() {
	if !memUnlocked() {
		return
	}
	for _, bucket := range encryptedBuckets {
		for k, v := range allKV(bucket) {
			raw := []byte(v)
			if !plainStoreKey(bucket, k) || !looksEncrypted(raw) {
				continue
			}
			if plain, err := decodeMemContent(raw); err == nil {
				_ = putBytes(bucket, k, plain)
			}
		}
	}
}

// reencryptBucket (re)chiffre toutes les valeurs d'un bucket encore en clair,
// avec vérification par relecture. Sûr à rejouer. Exige la DEK en RAM.
func reencryptBucket(bucket string) error {
	for k, v := range allKV(bucket) {
		raw := []byte(v)
		if looksEncrypted(raw) || plainStoreKey(bucket, k) {
			continue
		}
		if err := putStoreBytes(bucket, k, raw); err != nil {
			return err
		}
		back, ok := getStoreBytes(bucket, k)
		if !ok || string(back) != v {
			return errors.New("vérification post-chiffrement échouée pour " + bucket + "/" + k)
		}
	}
	return nil
}

// bucketFullyEncrypted indique qu'AUCUNE valeur du bucket n'est en clair (toutes
// portent le magic). Lisible sans la DEK. Utilisé pour savoir si le chiffrement
// est réellement complet.
func bucketFullyEncrypted(bucket string) bool {
	for k, v := range allKV(bucket) {
		if plainStoreKey(bucket, k) {
			continue
		}
		if v != "" && !looksEncrypted([]byte(v)) {
			return false
		}
	}
	return true
}

// encryptedBuckets : les buckets dont les valeurs sont chiffrées quand le
// chiffrement est actif. Ici : les discussions (bkChat porte l'index, la
// discussion active et le journal de chacune), les blocs archivés au compactage
// (bkRecall — du verbatim de conversation, donc aussi sensible que le fil) et
// les trackers (bkTracker, 3ᵉ type de mémoire), et les résultats complets des
// outils gardés pour « voir plus » (bkToolRes — des extraits du fil).
//
// Les autres buckets (config, prefs, state, tasks) restent EN CLAIR : ils
// portent des réglages, pas des données personnelles — et surtout le coffre
// lui-même vit dans bkState. Le chiffrer avec la DEK qu'il protège ferait une
// boucle : plus moyen de déverrouiller quoi que ce soit.
var encryptedBuckets = []string{bkChat, bkRecall, bkTracker, bkToolRes}

// reencryptChatStores (re)chiffre les buckets de conversation. Exige la DEK.
func reencryptChatStores() error {
	// Écritures différées (chat_persist.go) d'abord : écrites pendant le
	// passage, elles pourraient être écrasées par la relecture d'avant.
	persistQ.flush()
	healPlainStoreKeys()
	for _, b := range encryptedBuckets {
		if err := reencryptBucket(b); err != nil {
			return err
		}
	}
	return nil
}

// decryptChatStores remet en clair les buckets de conversation. Exige la DEK.
func decryptChatStores() error {
	_, err := decryptChatStoresSkipping()
	return err
}

// unreadableValue : une valeur chiffrée qui ne se déchiffre pas avec la clé
// actuelle (écrite avec une autre clé, ou abîmée). Gardée telle quelle.
type unreadableValue struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	Blob   []byte `json:"blob"` // base64 en JSON
	Error  string `json:"error"`
}

// decryptChatStoresSkipping déchiffre tout ce qui peut l'être et RENVOIE ce qui
// ne le peut pas, au lieu de s'arrêter au premier échec : une seule valeur
// illisible bloquait toute la désactivation du chiffrement, sans dire laquelle
// (amont, 2026-10-03).
func decryptChatStoresSkipping() ([]unreadableValue, error) {
	persistQ.flush() // même raison que reencryptChatStores
	var bad []unreadableValue
	for _, b := range encryptedBuckets {
		u, err := decryptBucket(b)
		if err != nil {
			return bad, err
		}
		bad = append(bad, u...)
	}
	return bad, nil
}

// decryptBucket remet en clair toutes les valeurs chiffrées d'un bucket. Exige
// la DEK en RAM. Sûr à rejouer. Une valeur indéchiffrable est laissée telle
// quelle et renvoyée ; seule une erreur d'écriture est fatale.
func decryptBucket(bucket string) ([]unreadableValue, error) {
	var bad []unreadableValue
	for k, v := range allKV(bucket) {
		raw := []byte(v)
		if !looksEncrypted(raw) {
			continue
		}
		plain, err := decodeMemContent(raw)
		if err != nil {
			bad = append(bad, unreadableValue{Bucket: bucket, Key: k, Blob: raw, Error: err.Error()})
			continue
		}
		if err := putBytes(bucket, k, plain); err != nil {
			return bad, err
		}
	}
	return bad, nil
}
