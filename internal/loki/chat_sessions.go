package loki

// chat_sessions.go — plusieurs discussions au lieu d'un fil unique.
//
// L'amont ne connaît QU'UNE conversation, persistée sous la clé
// bkChat/"conversation" : « clear chat » l'effaçait définitivement. Ici on garde
// exactement la même machinerie (un seul `conv` en mémoire, mêmes flux SSE,
// même compactage) mais on la range dans un tiroir nommé :
//
//	bkChat/index      → liste des discussions (métadonnées, sans les messages)
//	bkChat/active     → identifiant de la discussion ouverte
//	bkChat/conv:<id>  → l'état complet d'une discussion (JSON de Conversation)
//
// Les FICHIERS suivent le même découpage, sur le disque cette fois :
// <workspace>/discussions/<id>/ (chat_convfiles.go). Supprimer une discussion
// supprime donc aussi ses fichiers.
//
// Basculer = enregistrer la discussion courante, charger l'autre en mémoire et
// incrémenter l'epoch : les abonnés SSE reçoivent alors {reset:true} et
// rejouent le nouveau fil depuis zéro. C'est le mécanisme déjà utilisé par
// Reset(), donc aucun code de rendu ni de flux à toucher.
//
// La discussion active est PARTAGÉE par tous les appareils, comme le fil unique
// de l'amont : changer de discussion sur le téléphone la change aussi sur le PC.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ckIndex  = "index"
	ckActive = "active"
	ckLegacy = "conversation" // fil unique d'avant le multi-discussions
)

// convOpMu sérialise les opérations de haut niveau sur les discussions
// (création, bascule, renommage, suppression). Chacune fait plusieurs
// lectures-écritures de l'index et de la clé active : deux requêtes HTTP
// simultanées (deux appareils, double-clic) pouvaient entrelacer ces étapes et
// perdre une entrée d'index ou basculer sur une discussion supprimée. Le verrou
// de Conversation protège l'état en mémoire, pas cette séquence-là.
var convOpMu sync.Mutex

// convMeta décrit une discussion SANS ses messages : c'est ce que liste l'UI.
type convMeta struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Created int64  `json:"created"`
	Updated int64  `json:"updated"`
	Turns   int    `json:"turns"`
}

func convKey(id string) string { return "conv:" + id }

func convIndex() []convMeta {
	var idx []convMeta
	if b := getBytes(bkChat, ckIndex); len(b) > 0 {
		_ = json.Unmarshal(b, &idx)
	}
	// Plus récemment modifiée en tête : c'est l'ordre attendu d'une liste de
	// discussions, et l'UI n'a rien à trier.
	sort.SliceStable(idx, func(i, j int) bool { return idx[i].Updated > idx[j].Updated })
	return idx
}

func convIndexSave(idx []convMeta) {
	b, err := json.Marshal(idx)
	if err != nil {
		return
	}
	_ = putBytes(bkChat, ckIndex, b)
}

// convEnsureActive garantit qu'une discussion active existe, et reprend le fil
// unique de l'amont s'il y en a un (migration silencieuse, une seule fois).
func convEnsureActive() string {
	if id := getStr(bkChat, ckActive); id != "" {
		return id
	}
	id := newConvID()
	if legacy := getBytes(bkChat, ckLegacy); len(legacy) > 0 {
		// Migration : le fil unique devient la première discussion. On garde la
		// clé d'origine intacte — en cas de retour arrière, rien n'est perdu.
		_ = putBytes(bkChat, convKey(id), legacy)
	}
	_ = putStr(bkChat, ckActive, id)
	now := time.Now().Unix()
	convIndexSave(append(convIndex(), convMeta{ID: id, Created: now, Updated: now}))
	return id
}

// newConvID : horodatage en nanosecondes — monotone, lisible au tri, et sans
// dépendance à un générateur aléatoire.
func newConvID() string { return fmt.Sprintf("c%d", time.Now().UnixNano()) }

// convTouchMeta rafraîchit les métadonnées de la discussion active après un
// enregistrement : titre (déduit du premier message si l'utilisateur n'en a pas
// choisi), date de modification et nombre d'échanges.
func convTouchMeta(id string, title string, turns int) {
	idx := convIndex()
	now := time.Now().Unix()
	for i := range idx {
		if idx[i].ID != id {
			continue
		}
		idx[i].Updated = now
		idx[i].Turns = turns
		if idx[i].Title == "" {
			idx[i].Title = title
		}
		convIndexSave(idx)
		return
	}
	convIndexSave(append(idx, convMeta{ID: id, Title: title, Created: now, Updated: now, Turns: turns}))
}

// convSummary lit le premier message utilisateur pour en faire un titre. Sans
// message, on laisse vide : l'UI affiche « Nouvelle discussion » et le titre se
// posera tout seul au premier échange.
func convSummary(msgs []Message) string {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		s, _ := m.Content.(string)
		s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
		if s == "" {
			continue
		}
		// Découpe sur les RUNES : tronquer des octets couperait un caractère
		// accentué en deux et produirait un titre invalide.
		if r := []rune(s); len(r) > 60 {
			s = strings.TrimSpace(string(r[:60])) + "…"
		}
		return s
	}
	return ""
}

// ConvList renvoie les discussions et l'identifiant de l'active (pour l'UI).
func ConvList() ([]convMeta, string) { return convIndex(), convEnsureActive() }

// convSwitch bascule sur une autre discussion : la courante est enregistrée,
// la cible chargée en mémoire, et l'epoch incrémenté pour que tous les clients
// rejouent le nouveau fil.
func convSwitch(id string) error {
	convOpMu.Lock()
	defer convOpMu.Unlock()
	if id == "" {
		return fmt.Errorf("identifiant manquant")
	}
	found := false
	for _, m := range convIndex() {
		if m.ID == id {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("discussion introuvable")
	}
	if id == getStr(bkChat, ckActive) {
		return nil
	}
	conv.persist() // fige la discussion qu'on quitte
	_ = putStr(bkChat, ckActive, id)
	conv.loadFrom(getBytes(bkChat, convKey(id)))
	return nil
}

// convCreate crée une discussion vide et l'ouvre, SANS enregistrer la courante.
// Séparé de convNew parce que la suppression de la dernière discussion doit
// repartir d'un fil neuf sans ressusciter celui qu'on vient d'effacer.
func convCreate() string {
	id := newConvID()
	now := time.Now().Unix()
	convIndexSave(append(convIndex(), convMeta{ID: id, Created: now, Updated: now}))
	_ = putStr(bkChat, ckActive, id)
	conv.loadFrom(nil)
	return id
}

// convNew met de côté la discussion courante puis en ouvre une neuve.
func convNew() string {
	convOpMu.Lock()
	defer convOpMu.Unlock()
	conv.persist()
	return convCreate()
}

func convRename(id, title string) error {
	convOpMu.Lock()
	defer convOpMu.Unlock()
	title = strings.TrimSpace(title)
	if id == "" || title == "" {
		return fmt.Errorf("identifiant ou titre manquant")
	}
	idx := convIndex()
	for i := range idx {
		if idx[i].ID == id {
			idx[i].Title = title
			convIndexSave(idx)
			return nil
		}
	}
	return fmt.Errorf("discussion introuvable")
}

// convDelete supprime une discussion. Supprimer celle qui est ouverte bascule
// sur la plus récente restante — ou sur une discussion neuve s'il n'en reste
// aucune : il y a TOUJOURS une discussion active.
func convDelete(id string) error {
	convOpMu.Lock()
	defer convOpMu.Unlock()
	idx := convIndex()
	next := make([]convMeta, 0, len(idx))
	found := false
	for _, m := range idx {
		if m.ID == id {
			found = true
			continue
		}
		next = append(next, m)
	}
	if !found {
		return fmt.Errorf("discussion introuvable")
	}
	convIndexSave(next)
	_ = putBytes(bkChat, convKey(id), nil)
	// Les fichiers de cette discussion — dépôts, captures, ce que l'agent y a
	// écrit — n'ont plus rien qui les référence : les garder occuperait le disque
	// pour toujours, et plus aucun écran ne permettrait de les retrouver.
	dropConvFiles(id)
	if id != getStr(bkChat, ckActive) {
		return nil
	}
	if len(next) == 0 {
		convCreate() // surtout pas convNew : il réenregistrerait la supprimée
		return nil
	}
	_ = putStr(bkChat, ckActive, next[0].ID)
	conv.loadFrom(getBytes(bkChat, convKey(next[0].ID)))
	return nil
}
