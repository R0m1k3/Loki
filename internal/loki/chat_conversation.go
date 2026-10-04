package loki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// État de conversation CÔTÉ SERVEUR — une seule conversation partagée par tous
// les appareils. Avant, l'historique vivait dans le localStorage de chaque
// navigateur : refresh = perte des détails (outils/vitesses/raisonnement),
// contexte différent par appareil, et fermer l'onglet coupait la génération
// (liée à r.Context()). Ici l'état est possédé par le serveur, persisté sur
// disque, et la génération tourne dans une goroutine détachée : fermer le
// navigateur ne l'arrête plus, et se reconnecter rejoue tout le fil.
//
// Idée clé : l'UI reconstruit déjà tout l'affichage à partir d'une suite
// d'événements SSE `delta` (content, reasoning_content, tool_used, stats…). On
// JOURNALISE ces événements (Log) horodatés par Seq. Se reconnecter = rejouer
// Log[from:] puis suivre les événements en direct — aucun code de rendu nouveau.

// maxLogEvents plafonne le journal d'AFFICHAGE (pas la vue modèle). Depuis la
// coalescence en fin de tour (compactLogLocked), un tour terminé ne pèse plus
// qu'une poignée d'événements au lieu d'un par token → 20000 couvre des
// centaines de tours. La marge sert surtout à absorber UN tour en cours (streamé
// token par token) avant sa coalescence : un très gros tour (long raisonnement +
// réponse) ne doit pas se faire tronquer le début avant d'être compacté. 20000
// n'y suffisait pas — un seul tour à très long raisonnement tronquait déjà le
// journal de rejeu, et l'utilisateur perdait le DÉBUT de sa conversation à
// l'écran. Le journal ne pèse que de la mémoire d'affichage, jamais du contexte
// modèle : la marge est bon marché.
const maxLogEvents = 200000

// LogEvent = un événement d'affichage rejouable (un delta SSE + son numéro de
// séquence monotone + un horodatage serveur en ms). Le TS permet au client de
// calculer la vitesse (tok/s) à partir du temps RÉEL de génération — correct
// aussi bien en direct qu'au replay (où tout arrive d'un bloc côté client).
type LogEvent struct {
	Seq   int            `json:"seq"`
	TS    int64          `json:"ts"`
	Delta map[string]any `json:"delta"`
}

// Conversation est le fil unique partagé. Protégé par mu ; cond réveille les
// abonnés (aucun canal par abonné : les abonnés lisent Log au-delà de leur
// dernier Seq puis attendent cond — replay et direct sont le même chemin).
type Conversation struct {
	mu   sync.Mutex
	cond *sync.Cond

	Messages []Message  `json:"messages"` // vue « modèle » (nourrit runChat)
	Log      []LogEvent `json:"log"`      // vue « UI » rejouable
	Seq      int        `json:"seq"`
	CtxUsed  int        `json:"ctx_used"` // taille réelle du contexte au dernier tour
	// ctxUsedLen : nombre de messages de Messages que CtxUsed couvre déjà (0 =
	// inconnu). Ce qui arrive ensuite — le message utilisateur du tour suivant,
	// la file — est estimé en plus par ctxPending : le moteur ne l'a pas encore
	// compté. genPeak : génération la plus longue du dernier tour (llama-server
	// local), pour la garde de marge de compactNeeded. Ni l'un ni l'autre n'est
	// persisté : un rechargement repart du seul CtxUsed, comme avant.
	ctxUsedLen int
	genPeak    int

	Generating bool               `json:"-"`
	cancel     context.CancelFunc // annule la génération en cours (/stop)
	epoch      int                // incrémenté à chaque reset → invalide les abonnés
	// Tâche planifiée en cours (tasks_run.go). Le verrou de génération est le
	// SEUL point partagé entre une tâche de fond et le chat : ces deux champs
	// disent à l'interface POURQUOI elle est occupée, au lieu de laisser croire
	// à une génération fantôme dans la discussion ouverte.
	runningTaskID   string
	runningTaskName string
	// benching : le verrou est tenu par un benchmark (llm_bench_job.go). Même
	// logique que la tâche : l'interface et les refus disent qui occupe le moteur.
	benching bool

	// File d'attente des messages envoyés PENDANT une génération (AJEAN
	// 0.14.0). Ils sont injectés dans le tour en cours à la prochaine frontière
	// d'étape (après un appel d'outil), ou — si le tour se termine avant —
	// traités comme tours suivants, dans l'ordre. Protégée par mu.
	queued []queuedMsg
	// recentCIDs : identifiants d'envoi déjà acceptés. L'UI réessaie un envoi
	// dont la réponse s'est perdue (tunnel) : avant la file, le 409 « déjà en
	// cours » faisait office de dédoublonnage ; sans lui, le réessai mettrait
	// le même message deux fois en file.
	recentCIDs []string
}

// queuedMsg = un message mis en file pendant la génération. Caps et
// température sont ceux choisis à l'envoi : un tour démarré depuis la file doit
// s'exécuter avec ces réglages-là.
type queuedMsg struct {
	text  string
	files []attachInfo
	caps  Caps
	temp  float64
}

var conv = func() *Conversation {
	c := &Conversation{}
	c.cond = sync.NewCond(&c.mu)
	return c
}()

// La conversation est persistée en base, sur la machine qui fait tourner le
// modèle. En clair : cette machine déchiffre déjà pour lancer le modèle, le
// relais reste aveugle — la persister ici ne change rien à la posture E2E.

// loadConvAttempts / loadConvRetryWait : au redémarrage du conteneur, l'ancien
// process peut encore tenir le verrou bbolt quelques centaines de ms pendant que
// le nouveau démarre. Quatre essais espacés de 250 ms couvrent ce chevauchement
// sans retarder perceptiblement un démarrage normal (où le premier essai passe).
const (
	loadConvAttempts  = 4
	loadConvRetryWait = 250 * time.Millisecond
)

// loadConvRetries compte les ATTENTES de reprise du dernier LoadConversation
// (0 = le premier essai a suffi). Il n'existe que pour être observable.
//
// Le test de non-régression veut vérifier qu'une absence légitime — première
// installation, rien d'enregistré — ne déclenche AUCUNE reprise. Il le mesurait
// au chronomètre (« moins de 250 ms »), ce qui confond « aucune reprise » avec
// « une seule tentative, mais lente » : sur un runner CI chargé, créer et
// ouvrir la base bbolt a pris 435 ms et le test a échoué alors qu'aucune reprise
// n'avait eu lieu. On compte donc les reprises au lieu de les déduire d'un
// temps de mur, qui ne dépend pas que de nous.
var loadConvRetries atomic.Int32

// LoadConversation recharge l'état persisté au démarrage du process. Sans état
// enregistré (première fois) on part d'une conversation vide.
//
// Une lecture RATÉE n'est PAS une absence, et les confondre coûte cher ici :
// getStr rend "" aussi bien pour « clé absente » que pour « base inaccessible »,
// donc sur un verrou transitoire convEnsureActive croyait la discussion active
// inexistante, forgeait un NOUVEL identifiant et l'écrasait — le fil en cours
// devenait orphelin, en silence. On sonde donc la base avec son erreur AVANT
// toute écriture, on réessaie, et en cas d'échec durable on le dit et on ne
// touche à rien.
func LoadConversation() {
	var b []byte
	var err error
	loadConvRetries.Store(0)
	retry := func() {
		loadConvRetries.Add(1)
		time.Sleep(loadConvRetryWait)
	}
	for attempt := 0; attempt < loadConvAttempts; attempt++ {
		// Sonde : lire la clé de la discussion active en gardant l'erreur. Tant
		// qu'elle échoue, convEnsureActive ne doit surtout pas être appelée.
		if _, err = getBytesErr(bkChat, ckActive); err != nil {
			retry()
			continue
		}
		// convEnsureActive reprend au passage le fil unique des versions
		// précédentes (clé « conversation ») comme première discussion.
		// getStoreBytesErr : même lecture, en DÉCHIFFRANT au besoin (mémoire
		// chiffrée). Verrouillé, elle rend (nil, nil) — pas une erreur d'accès :
		// on démarre alors sur un fil vide sans rien écraser, et tout revient au
		// déverrouillage.
		if b, err = getStoreBytesErr(bkChat, convKey(convEnsureActive())); err == nil {
			break
		}
		retry()
	}
	if err != nil {
		// Toujours en échec : on le DIT au lieu de repartir à vide en silence, et
		// on abandonne le chargement sans rien écrire — un serveur qui refuse de
		// démarrer serait pire, et l'historique sur disque reste intact.
		fmt.Fprintf(os.Stderr, "[conv] base illisible au démarrage (%v) — aucune discussion chargée ; rien n'a été écrasé, l'historique est intact\n", err)
		return
	}
	if len(b) == 0 {
		return
	}
	conv.mu.Lock()
	defer conv.mu.Unlock()
	_ = json.Unmarshal(b, conv)
	// Guérit les conversations d'avant le passage des captures en éphémère : un
	// base64 d'image persisté était rejoué à chaque tour et dépassait le contexte.
	conv.Messages = stripImageParts(conv.Messages)
	go pruneChatImages()
	// Une génération n'a pas pu survivre à l'arrêt du process : on repart propre.
	conv.Generating = false
	conv.cancel = nil
}

// loadFrom remplace l'état en mémoire par une autre discussion (b vide = fil
// neuf) et invalide les abonnés : l'epoch incrémenté leur fait vider l'écran et
// rejouer depuis zéro, exactement comme un reset. id = discussion chargée : le
// cache de la discussion active change sous mu, avec le contenu (voir
// convActivate). L'appelant NE doit PAS détenir mu.
func (c *Conversation) loadFrom(id string, b []byte) {
	// Un benchmark n'appartient à aucune discussion : changer de fil ne l'arrête
	// pas, et le verrou reste à lui jusqu'à sa fin (llm_bench_job.go).
	c.mu.Lock()
	bench := c.benching
	c.mu.Unlock()
	if !bench {
		c.Stop()
	}
	c.mu.Lock()
	if c == conv {
		convActiveRemember(id)
	}
	c.Messages, c.Log, c.Seq, c.CtxUsed = nil, nil, 0, 0
	c.ctxUsedLen, c.genPeak = 0, 0
	c.queued = nil // file de l'ancienne discussion : elle ne suit pas la bascule
	if len(b) > 0 {
		_ = json.Unmarshal(b, c)
		c.Messages = stripImageParts(c.Messages) // même guérison qu'au chargement
	}
	go pruneChatImages() // plus rien ne référence les images de l'ancien fil
	if !c.benching {
		c.Generating = false
		c.cancel = nil
	}
	c.epoch++
	c.cond.Broadcast()
	c.mu.Unlock()
}

// reloadEncryptedStores recharge la discussion depuis le disque après un
// déverrouillage : verrouillée, la lecture au démarrage a rendu un fil vide.
// On ne recharge QUE dans ce cas — écraser un fil déjà rempli ferait perdre le
// tour en cours, qui n'a pas pu être persisté.
func reloadEncryptedStores() {
	conv.mu.Lock()
	empty := len(conv.Log) == 0
	conv.mu.Unlock()
	if empty {
		LoadConversation()
	}
}

// appendDelta journalise un événement d'affichage et réveille les abonnés.
// epoch est celui capturé au début du tour : si un Reset est passé entre-temps,
// l'événement appartient à l'ancienne conversation et est jeté (sinon il
// polluerait le journal tout neuf avec des Seq repartis de zéro).
func (c *Conversation) appendDelta(epoch int, delta map[string]any) {
	c.mu.Lock()
	if c.epoch != epoch {
		c.mu.Unlock()
		return
	}
	c.Seq++
	c.Log = append(c.Log, LogEvent{Seq: c.Seq, TS: time.Now().UnixMilli(), Delta: delta})
	if len(c.Log) > maxLogEvents {
		c.Log = c.Log[len(c.Log)-maxLogEvents:]
	}
	c.cond.Broadcast()
	c.mu.Unlock()
}

// evToks lit le nombre de tokens porté par un événement texte : 1 pour un delta
// brut (streaming), ou la valeur `toks` accumulée pour un événement déjà coalescé.
func evToks(d map[string]any) int {
	switch v := d["toks"].(type) {
	case int:
		return v
	case float64: // relu depuis le JSON persisté
		return int(v)
	}
	return 1
}

// evTS0 lit l'horodatage de DÉBUT d'un événement texte (présent seulement sur les
// événements coalescés) ; sinon on retombe sur `fallback` (le TS de l'événement).
func evTS0(d map[string]any, fallback int64) int64 {
	switch v := d["ts0"].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return fallback
}

// compactLogLocked coalesce EN PLACE les suites d'événements content /
// reasoning_content du journal d'affichage en un seul événement chacun (même
// logique que coalesceReplay). Sans ça, le journal grossit token par token et
// atteint maxLogEvents en quelques réponses → on perd le DÉBUT de la conversation
// à l'affichage. On l'appelle en fin de tour (les événements sont alors figés).
// On préserve `toks` (somme) et `ts0` (premier) pour que le compteur de vitesse
// (tok/s) reste correct au replay. Verrou détenu par l'appelant.
func (c *Conversation) compactLogLocked() {
	c.Log = compactLog(c.Log)
}

// compactLog est la compaction de compactLogLocked sous forme PURE : elle rend
// un nouveau journal sans toucher à celui qu'on lui passe (les événements non
// fusionnés sont partagés, pas recopiés). snapshot s'en sert pour écrire, en
// plein tour, la forme qu'aura le journal à la fin du tour.
func compactLog(log []LogEvent) []LogEvent {
	if len(log) < 2 {
		return log
	}
	textKey := func(d map[string]any) string {
		if _, ok := d["content"].(string); ok {
			return "content"
		}
		if _, ok := d["reasoning_content"].(string); ok {
			return "reasoning_content"
		}
		return ""
	}
	out := make([]LogEvent, 0, len(log))
	var buf strings.Builder
	bufKey := ""
	var cur LogEvent
	var toks int
	var ts0 int64
	var seq0 int
	flush := func() {
		if bufKey == "" {
			return
		}
		// seq0 = seq du PREMIER delta fusionné (cur.Seq, lui, vaut celui du dernier).
		cur.Delta = map[string]any{bufKey: buf.String(), "toks": toks, "ts0": ts0, "seq0": seq0}
		out = append(out, cur)
		buf.Reset()
		bufKey, toks, ts0, seq0 = "", 0, 0, 0
	}
	for _, ev := range log {
		// Outils : un appel s'écrit en de nombreux événements (annonce done=false,
		// frappe du corps, streaming des arguments) jusqu'au done=true, qui porte
		// déjà l'état FINAL (résultat + diff). Les intermédiaires ne servent qu'à
		// l'affichage EN DIRECT ; les garder dans le journal le faisait enfler sur
		// une conversation agentique (gros write, appels en chaîne) jusqu'à
		// maxLogEvents → troncature des plus VIEUX événements, donc perte des
		// premiers messages à l'affichage. On ne conserve donc que le done=true
		// (même politique que coalesceReplay au replay/export).
		if tu, ok := ev.Delta["tool_used"].(map[string]any); ok {
			flush()
			if done, _ := tu["done"].(bool); done {
				out = append(out, ev)
			}
			continue
		}
		key := textKey(ev.Delta)
		if key == "" {
			flush()
			out = append(out, ev)
			continue
		}
		if bufKey != "" && bufKey != key {
			flush()
		}
		if bufKey == "" {
			bufKey = key
			cur = LogEvent{Seq: ev.Seq, TS: ev.TS}
			ts0 = evTS0(ev.Delta, ev.TS)
			seq0 = evSeq0(ev.Delta, ev.Seq)
		}
		buf.WriteString(ev.Delta[key].(string))
		cur.Seq, cur.TS = ev.Seq, ev.TS
		toks += evToks(ev.Delta)
	}
	flush()
	return out
}

// compactAndPublish exécute UNE compaction et en publie tout le cycle de vie :
// bannière de progression, journal, installation du résultat, jauge de contexte.
// Renvoie l'historique (compacté ou inchangé) et s'il a changé.
//
// Les trois compactions (début de tour, fin de tour, bouton manuel) déroulaient
// la même douzaine de lignes recopiées, y compris le calcul du surcoût fixe :
// une correction dans l'une ne suivait pas dans les deux autres.
//
// Le surcoût, justement : le contexte RÉEL mesuré moins l'estimation des
// messages donne ce que l'estimation ne voit pas (prompt système injecté,
// schémas d'outils, gabarit de chat). On le rajoute à l'estimation d'après
// compaction, sinon la jauge s'effondre puis resaute au tour suivant.
func (c *Conversation) compactAndPublish(ctx context.Context, epoch int, phase string, msgs []Message, ctxUsed int, caps Caps) ([]Message, bool) {
	c.appendDelta(epoch, map[string]any{"compacting": true})
	compacted, changed := compactMessages(ctx, msgs, caps)
	c.appendDelta(epoch, map[string]any{"compacting": false})
	logCompact(phase, ctxUsed, msgs, compacted, changed)
	if !changed {
		// Seuil franchi mais compaction sans effet (torse vide, ou réduction sous
		// le minimum exigé) : on le DIT. Retirer la bannière sans un mot donnait,
		// vu de l'UI, « le compactage automatique ne fait rien ».
		c.appendDelta(epoch, map[string]any{"compact_noop": true})
		return msgs, false
	}
	// Les pages mémoire lues ont été résumées avec le torse : un rappel liste
	// leurs noms pour que le modèle les relise au besoin (AJEAN 0.14.0). Agent
	// seulement — sans lui, pas d'outil mem_read pour y donner suite.
	if caps.Agent {
		compacted = remindReadMemPages(compacted, msgs)
	}
	overhead := ctxUsed - estimateTokens(msgs)
	if overhead < 0 {
		overhead = 0
	}
	est := estimateTokens(compacted) + overhead
	c.mu.Lock()
	if c.epoch == epoch {
		c.Messages = compacted
		c.CtxUsed = est // le vrai compte reviendra avec les stats du prochain tour
		c.ctxUsedLen = len(compacted)
	}
	c.mu.Unlock()
	c.appendDelta(epoch, map[string]any{"compacted": true})
	c.appendDelta(epoch, map[string]any{"ctx_used": est}) // fait chuter la jauge tout de suite
	return compacted, true
}

// convState renvoie un instantané léger (pour /api/chat/state).
func (c *Conversation) state() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	// `turns` = nombre d'échanges, borne du curseur de portée de l'export (voir
	// countTurns). Compté ici plutôt que par un appel dédié : c'est un balayage
	// du journal déjà en main, et l'état est de toute façon relu à l'ouverture.
	turns := 0
	for _, ev := range c.Log {
		if _, ok := ev.Delta["user"]; ok {
			turns++
		}
	}
	st := map[string]any{"seq": c.Seq, "generating": c.Generating, "ctx_used": c.CtxUsed, "turns": turns}
	// Occupé PAR UNE TÂCHE : l'interface l'annonce (« tâche en cours ») plutôt que
	// d'afficher un tour qui n'apparaîtra jamais dans le fil.
	if c.runningTaskID != "" {
		st["task_id"] = c.runningTaskID
		st["task_name"] = c.runningTaskName
	}
	return st
}

// isGenerating dit si un tour est en cours. Sert à la LISTE des discussions :
// elle doit pouvoir montrer laquelle travaille dès le chargement de la page,
// sans attendre que le rejeu du flux SSE ait rattrapé son retard.
func (c *Conversation) isGenerating() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Generating
}

// ErrBusy : une génération est déjà en cours (un seul tour à la fois).
var ErrBusy = fmt.Errorf("génération en cours")

// busyReason explique le refus à l'utilisateur. Sans ça, un message envoyé
// pendant qu'une TÂCHE planifiée tourne renvoyait « génération en cours » alors
// que le fil est vide et que rien ne bouge à l'écran — incompréhensible. On
// nomme la tâche et on dit comment reprendre la main.
func (c *Conversation) busyReason() error {
	c.mu.Lock()
	name, bench := c.runningTaskName, c.benching
	c.mu.Unlock()
	if bench {
		return errBenchBusy
	}
	if name == "" {
		return ErrBusy
	}
	return fmt.Errorf("la tâche planifiée « %s » occupe le modèle — attends la fin ou arrête-la (bouton stop)", name)
}

// errModelLoading est renvoyée telle quelle à l'utilisateur, dans le chat : ce
// n'est pas un défaut mais une attente, et le message doit le dire.
var errModelLoading = fmt.Errorf("⏳ Le modèle est encore en train de charger — réessaie dans quelques secondes.")

// StartTurn ajoute le message utilisateur et lance la génération EN ARRIÈRE-PLAN
// (context.Background, détaché de toute connexion HTTP). Renvoie ErrBusy si un
// tour est déjà en cours, ou une erreur si le modèle n'est pas prêt.
// files = pièces jointes déjà déposées (web_upload.go). Elles sont annoncées au
// MODÈLE en tête du message, mais rendues comme pastilles dans la bulle : le fil
// doit montrer ce que l'utilisateur a écrit, pas la consigne qu'on ajoute pour lui.
func (c *Conversation) StartTurn(text string, files []attachInfo, caps Caps, temperature float64) error {
	if !healthCheck() {
		return errModelLoading
	}
	c.mu.Lock()
	if c.Generating {
		c.mu.Unlock()
		return ErrBusy
	}
	c.Generating = true
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	// Envoi sans un mot, juste un fichier : la bulle reste vide (les pastilles
	// disent tout), mais le modèle a besoin d'une demande — sans elle il reçoit
	// une liste de fichiers et rien à en faire.
	prompt := text
	if strings.TrimSpace(prompt) == "" {
		prompt = "Prends-en connaissance."
	}
	// Mode code : fige le rôle du tour (plan demandé → planner, sinon builder).
	// La détection de bascule (puce « passer en mode Code ? ») est émise après
	// la bulle utilisateur, plus bas.
	if caps.Code && caps.Role == "" {
		caps.Role = "builder"
		if wantsPlan(text) {
			caps.Role = "planner"
		}
	}
	// Content = simple texte d'ordinaire ; format multimodal (texte + images) quand
	// la vision est active et qu'une pièce jointe est une image (userMessageContent).
	c.Messages = append(c.Messages, Message{Role: "user", Content: userMessageContent(files, prompt)})
	epoch := c.epoch
	c.mu.Unlock()

	// Borne de tour + bulle utilisateur (rejouables). Persistée tout de suite
	// (sur disque quelques ms plus tard, voir persistAsync) : si le process
	// meurt en pleine génération (crash, restart après MAJ), le message de
	// l'utilisateur survit au lieu de disparaître avec le tour.
	delta := map[string]any{"user": text}
	// Nom du modèle qui va produire la réponse : journalisé avec la borne de
	// tour, donc rejoué au chargement — la carte de réponse garde son modèle
	// après un rafraîchissement, et un vieux tour garde le modèle de l'époque,
	// pas celui chargé aujourd'hui.
	if m := chatModelName(); m != "" {
		delta["model"] = m
	}
	if len(files) > 0 {
		delta["files"] = files
	}
	c.appendDelta(epoch, delta)
	// En mode chat, un message qui ressemble à une tâche de code fait
	// apparaître la puce « passer en mode Code ? » (une fois par discussion).
	if !caps.Code {
		maybeCodeHint(c, epoch, text)
	}
	// Instantané pris ici, écriture laissée à l'écrivain (chat_persist.go) : le
	// fsync ne retarde plus le départ de la requête vers le modèle.
	c.persistAsync()
	if temperature == 0 {
		temperature = 0.7
	}
	go c.generate(ctx, caps, temperature, epoch)
	return nil
}

// ErrDupSend : cet envoi (même cid) a déjà été accepté — réessai d'un client
// qui n'a pas reçu la réponse. Ce n'est pas une erreur pour l'utilisateur.
var ErrDupSend = fmt.Errorf("envoi déjà reçu")

// seenCID enregistre cid et dit s'il avait déjà été vu. Appelant : mu tenu.
func (c *Conversation) seenCIDLocked(cid string) bool {
	if cid == "" {
		return false
	}
	for _, v := range c.recentCIDs {
		if v == cid {
			return true
		}
	}
	c.recentCIDs = append(c.recentCIDs, cid)
	if len(c.recentCIDs) > 64 {
		c.recentCIDs = c.recentCIDs[len(c.recentCIDs)-64:]
	}
	return false
}

// EnqueueOrStart démarre un tour tout de suite si le moteur est libre, sinon
// MET EN FILE le message (AJEAN 0.14.0) — au lieu du 409 d'avant, qui obligeait
// à arrêter la réponse pour ajouter une précision. Seul un tour de CHAT accepte
// une file : une tâche planifiée ou un benchmark qui occupe le modèle garde le
// refus (busyReason), sa fin ne dépile rien. cid = identifiant de l'envoi (voir
// recentCIDs) ; un doublon renvoie ErrDupSend.
func (c *Conversation) EnqueueOrStart(cid, text string, files []attachInfo, caps Caps, temperature float64) (bool, error) {
	c.mu.Lock()
	if c.seenCIDLocked(cid) {
		c.mu.Unlock()
		return false, ErrDupSend
	}
	if c.Generating && c.runningTaskName == "" && !c.benching {
		c.queued = append(c.queued, queuedMsg{text: text, files: files, caps: caps, temp: temperature})
		c.mu.Unlock()
		return true, nil
	}
	c.mu.Unlock()
	err := c.StartTurn(text, files, caps, temperature)
	if errors.Is(err, ErrBusy) {
		// Un autre tour a démarré entre le test ci-dessus et StartTurn (deux
		// appareils qui envoient en même temps) : en file plutôt qu'un refus
		// (AJEAN 0.17.4). Une tâche planifiée garde le refus : sa fin ne dépile rien.
		c.mu.Lock()
		if c.Generating && c.runningTaskName == "" && !c.benching {
			c.queued = append(c.queued, queuedMsg{text: text, files: files, caps: caps, temp: temperature})
			c.mu.Unlock()
			// Ce tour a pu finir entre-temps : sans relance, le message attendrait
			// le prochain envoi.
			c.startQueuedIfAny()
			return true, nil
		}
		c.mu.Unlock()
	}
	if err != nil {
		// Refusé (modèle pas prêt, tâche en cours) : le client pourra renvoyer
		// ce même message, son cid ne doit pas rester marqué comme reçu.
		c.mu.Lock()
		for i, v := range c.recentCIDs {
			if v == cid {
				c.recentCIDs = append(c.recentCIDs[:i], c.recentCIDs[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
	}
	return false, err
}

// drainQueued vide la file et renvoie les messages utilisateur à injecter dans
// le tour EN COURS (vue modèle), en les journalisant pour qu'ils apparaissent
// dans le fil sur tous les appareils. Appelé par runChat entre deux étapes.
// epoch garde contre un reset survenu entre-temps.
func (c *Conversation) drainQueued(epoch int) []Message {
	c.mu.Lock()
	if c.epoch != epoch || len(c.queued) == 0 {
		c.mu.Unlock()
		return nil
	}
	items := c.queued
	c.queued = nil
	c.mu.Unlock()

	var out []Message
	for _, q := range items {
		delta := map[string]any{"user": q.text}
		if m := chatModelName(); m != "" {
			delta["model"] = m
		}
		if len(q.files) > 0 {
			delta["files"] = q.files
		}
		c.appendDelta(epoch, delta)
		prompt := q.text
		if strings.TrimSpace(prompt) == "" {
			prompt = "Prends-en connaissance."
		}
		out = append(out, Message{Role: "user", Content: userMessageContent(q.files, prompt)})
	}
	return out
}

// startQueuedIfAny démarre le prochain tour depuis la file, s'il en reste et
// que le moteur est libre. Appelé à la toute fin d'un tour : les messages
// envoyés après la dernière frontière d'étape sont ainsi traités sans que
// l'utilisateur ait à les renvoyer. Chaque tour lancé ainsi rappelle
// startQueuedIfAny à sa fin → la file se vide dans l'ordre. Renvoie true si un
// tour a démarré.
func (c *Conversation) startQueuedIfAny() bool {
	c.mu.Lock()
	if c.Generating || len(c.queued) == 0 {
		c.mu.Unlock()
		return false
	}
	q := c.queued[0]
	c.queued = c.queued[1:]
	epoch := c.epoch
	c.mu.Unlock()
	if err := c.StartTurn(q.text, q.files, q.caps, q.temp); err != nil {
		// Modèle indisponible au moment de dépiler (rare) : on le dit dans le
		// fil plutôt que de perdre le message en silence, et on tente le suivant.
		c.appendDelta(epoch, map[string]any{"error": "message en attente non envoyé : " + err.Error()})
		return c.startQueuedIfAny()
	}
	return true
}

// dropQueued abandonne la file (bouton stop : l'utilisateur reprend la main) et
// le signale au client, qui retire ses messages « en attente ».
func (c *Conversation) dropQueued(epoch int) {
	c.mu.Lock()
	n := len(c.queued)
	c.queued = nil
	c.mu.Unlock()
	if n > 0 {
		c.appendDelta(epoch, map[string]any{"queue_dropped": n})
	}
}

// chatModelName renvoie le nom lisible du modèle configuré : fichier sans
// dossier ni extension .gguf (même règle que modelLabel côté UI), "" si aucun.
func chatModelName() string {
	cfg := ReadConfig()
	if isExternalConfig(cfg) {
		return strings.TrimSpace(cfg[extKeyModel]) // modèle distant, tel quel
	}
	m := strings.TrimSpace(cfg["MODEL"])
	if m == "" {
		return ""
	}
	if i := strings.LastIndexAny(m, "/\\"); i >= 0 {
		m = m[i+1:]
	}
	if strings.HasSuffix(strings.ToLower(m), ".gguf") {
		m = m[:len(m)-len(".gguf")]
	}
	return m
}

// generate exécute un tour complet et journalise chaque événement. Détaché : la
// fermeture du navigateur n'a aucun effet ici, seul /stop (cancel) l'interrompt.
// epoch est capturé au StartTurn : si un Reset survient pendant la génération,
// tout ce que ce tour produirait ensuite (deltas, messages, persistance) est
// abandonné au lieu de ressusciter des morceaux de l'ancienne conversation.
func (c *Conversation) generate(ctx context.Context, caps Caps, temperature float64, epoch int) {
	turnStart := time.Now()
	defer func() {
		c.mu.Lock()
		stale := c.epoch != epoch
		// ⚠️ Un tour périmé ne touche PAS à l'état courant. Depuis que Reset
		// débloque lui-même la conversation, un tour abandonné peut se terminer
		// APRÈS le démarrage du suivant : remettre Generating à false ici
		// déclarerait « libre » une génération toute neuve, et l'UI afficherait
		// un fil qui se remplit avec un bouton « envoyer » actif.
		if !stale {
			c.Generating = false
			c.cancel = nil
		}
		c.mu.Unlock()
		if stale {
			return // Reset pendant le tour : Reset a déjà persisté l'état vide
		}
		c.appendDelta(epoch, map[string]any{"turn_done": true})
		c.mu.Lock()
		c.compactLogLocked() // le tour est fini : coalesce ses tokens pour garder le journal petit
		c.mu.Unlock()
		c.persist()
		// File d'attente : interrompu par un stop, l'utilisateur reprend la main
		// et les messages en file sont abandonnés. Sinon on démarre le prochain
		// tour depuis la file — ceux qui n'ont pas pu être injectés en cours de
		// route sont traités, dans l'ordre.
		if ctx.Err() != nil {
			c.dropQueued(epoch)
			return
		}
		// Notification Web Push, seulement quand plus rien ne suit : un message
		// en file qui démarre aussitôt n'est pas une « réponse prête », et sa
		// propre fin notifiera. Ce chemin (generate) ne sert QUE les tours
		// utilisateur — les tâches de fond passent par RunAutonomous et ont leur
		// propre notification — donc pas de doublon. Détaché : l'envoi HTTP vers
		// le service de push ne doit pas retenir la fin du tour. Corps générique
		// (pas d'extrait de réponse) : la notif transite par Apple/Google. Pas
		// de notification après un « stop » (retour ci-dessus) : l'utilisateur
		// est là et a coupé volontairement.
		if !c.startQueuedIfAny() && hasPushSubs() {
			go sendPushToAll("Loki", "Réponse prête · "+fmtDurFR(time.Since(turnStart)))
		}
	}()
	// Télémétrie : tout ce que ce tour envoie au moteur (étapes, compaction,
	// sous-agents, vérification) est rattaché à la discussion active.
	ctx = withPerf(ctx, perfMain, convActiveID())

	// llama-server local seulement : le preset externe garde le seul seuil, et
	// ses complétions ne nourrissent pas la garde de marge (compactNeeded).
	local := !externalActive()
	// Snapshot de la vue modèle.
	c.mu.Lock()
	msgs := append([]Message(nil), c.Messages...)
	// Dernier compte du moteur + ce qu'il n'a pas encore vu (ce message-ci, la
	// file). Sans ce complément, seul le raisonnement compté à tort masquait le
	// manque ; maintenant qu'il est retiré, le nouveau message doit compter.
	ctxUsed := c.ctxNowLocked(msgs, local)
	peak := c.genPeak
	c.genPeak = 0 // recompté pendant ce tour
	c.mu.Unlock()

	// Compaction proactive (façon Hermes) sur la vue MODÈLE uniquement ; le journal
	// d'affichage garde le fil complet. Le résumé est un appel modèle non streamé :
	// il bloque plusieurs secondes AVANT que la vraie réponse commence, d'où la
	// bannière de progression émise par compactAndPublish.
	// Estimation de début de tour, comparée au premier compte réel du moteur
	// (journal [ctx], seulement au-delà de 2 % d'écart).
	startEst := ctxUsed
	if compactNeeded(msgs, ctxUsed, peak) {
		if out, changed := c.compactAndPublish(ctx, epoch, "début-tour", msgs, ctxUsed, caps); changed {
			msgs = out
			// Le chiffre d'avant compaction ne dit plus rien de la requête qui
			// part : on compare celui que la compaction vient de poser.
			c.mu.Lock()
			startEst = c.CtxUsed
			c.mu.Unlock()
		}
	}

	// Prompt système personnalisé (UI → /api/sysprompt, fichier côté serveur).
	// Injecté seulement dans la vue envoyée au modèle, jamais persisté dans
	// c.Messages : modifiable à chaud, effet dès le tour suivant.
	// Contexte du projet actif (description, index mémoire, index des trackers),
	// même traitement : injecté dans la vue envoyée, jamais persisté — voir
	// projectSystemMessages.
	//
	// Mode agent OFF = modèle BRUT (AJEAN 0.13.13) : ni l'un ni l'autre. Sans
	// agent, on veut parler au modèle nu, comme à un llama-server direct. Le
	// prompt du preset et l'index mémoire décrivent des outils que ce mode n'a
	// pas : le modèle se mettait à écrire des <tool_call> en clair.
	final := msgs
	if caps.Agent {
		final = append(projectSystemMessages(), msgs...)
		if m, ok := codeInstructionsMessage(caps); ok {
			final = append([]Message{m}, final...)
		}
		if sp := readSysPrompt(); sp != "" {
			final = append([]Message{{Role: "system", Content: sp}}, final...)
		}
	}

	// newBase : vue modèle publiée par une compaction survenue PENDANT le tour.
	// Non-nil = elle remplace l'historique (elle contient déjà le tour en cours).
	var newBase []Message
	var content strings.Builder
	// Le comptage EXACT du contexte vient de `usage.prompt_tokens` (option
	// include_usage). Tous les moteurs ne le renvoient pas — un llama-server
	// récent a cessé de le faire, et la jauge est restée bloquée à zéro sur des
	// discussions de plusieurs dizaines de milliers de tokens. On note donc si
	// l'usage est arrivé ; sinon on retombe sur l'estimation (celle qui pilote
	// déjà la compaction), approximative mais jamais absente.
	sawUsage := false
	turnPeak := 0
	// asked : le tour s'est terminé sur une question à l'utilisateur (outil ask) —
	// pas de vérification du mode Code avant sa réponse.
	asked := false
	sent, tools := prepareTurn(final, caps)
	extra, _ := runChatTools(ctx, sent, tools, temperature, caps, func(ev StreamEvent) bool {
		switch {
		case ev.Err != nil:
			c.appendDelta(epoch, map[string]any{"error": ev.Err.Error()})
		case ev.ToolUsed != nil:
			// Le texte écrit AVANT cet appel d'outil est déjà rangé dans le message
			// assistant porteur des tool_calls (extra). Le garder aussi dans la
			// réponse finale le doublait dans l'historique vu par le modèle — du
			// contexte gaspillé à chaque tour d'outil (AJEAN 0.17.4).
			content.Reset()
			tu := map[string]any{
				"name": ev.ToolUsed.Name, "label": ev.ToolUsed.Label,
				"result": ev.ToolUsed.Result, "done": ev.ToolUsed.Done, "typing": ev.ToolUsed.Typing,
			}
			// Corps en cours de frappe : transitoire (l'état final est le diff), on
			// ne l'ajoute que quand il est là pour ne pas gonfler chaque événement.
			if ev.ToolUsed.Body != "" {
				tu["body"] = ev.ToolUsed.Body
				tu["body_lines"] = ev.ToolUsed.BodyLines
				if ev.ToolUsed.BodyTail {
					tu["body_tail"] = true
				}
			}
			// Lignes +/- d'une écriture (edit / mémoire) : persistées avec le reste
			// pour que le diff soit encore là après un rafraîchissement.
			if len(ev.ToolUsed.Diff) > 0 {
				tu["diff"] = ev.ToolUsed.Diff
				tu["added"] = ev.ToolUsed.Added
				tu["removed"] = ev.ToolUsed.Removed
			}
			if ev.ToolUsed.Image != "" {
				tu["image"] = ev.ToolUsed.Image
			}
			// Aperçu seulement dans le flux : taille réelle + id pour « voir plus ».
			if ev.ToolUsed.ResultChars > 0 {
				tu["result_chars"] = ev.ToolUsed.ResultChars
			}
			if ev.ToolUsed.ResultID != "" {
				tu["result_id"] = ev.ToolUsed.ResultID
			}
			c.appendDelta(epoch, map[string]any{"tool_used": tu})
		case ev.NewHistory != nil:
			// Compaction faite en cours de tour : elle remplace la base au lieu de
			// s'ajouter à l'ancienne (voir StreamEvent.NewHistory). On retire le
			// préfixe système injecté à la volée (prompt perso + skills, fusionnés en
			// UN message system en tête) : il n'appartient pas à l'historique persisté
			// et doit rester modifiable à chaud.
			base := ev.NewHistory
			for len(base) > 0 && base[0].Role == "system" {
				base = base[1:]
			}
			newBase = append([]Message(nil), base...)
		case ev.Compacting != nil:
			// Compaction déclenchée pendant la boucle d'outils : même bannière que la
			// compaction de début de tour.
			c.appendDelta(epoch, map[string]any{"compacting": *ev.Compacting})
		case ev.Stats != nil:
			// Taille réelle du contexte (usage.prompt_tokens + généré, raisonnement
			// non renvoyé retiré) pour le compteur et la décision de compactage au
			// tour suivant.
			if ev.Stats.PromptTokensTotal > 0 {
				if !sawUsage && local && startEst > 0 {
					logCtxEstimate("début-tour", startEst, ev.Stats.PromptTokensTotal)
				}
				sawUsage = true
				c.mu.Lock()
				if c.epoch == epoch {
					c.CtxUsed = ev.Stats.ctxAfter()
				}
				c.mu.Unlock()
			}
			if local {
				turnPeak = max(turnPeak, ev.Stats.GenTokens)
			}
			c.appendDelta(epoch, map[string]any{"stats": ev.Stats})
		case ev.Ask != nil:
			// Question structurée (outil ask) : carte à boutons dans l'UI.
			asked = true
			c.appendDelta(epoch, map[string]any{"ask": ev.Ask})
		case ev.DropReasoning:
			c.appendDelta(epoch, map[string]any{"drop_reasoning": true})
		case ev.Reasoning != "":
			c.appendDelta(epoch, map[string]any{"reasoning_content": ev.Reasoning})
		case ev.Content != "":
			content.WriteString(ev.Content)
			c.appendDelta(epoch, map[string]any{"content": ev.Content})
		}
		return true // génération détachée : on ne s'interrompt jamais sur un abonné
	}, func() []Message { return c.drainQueued(epoch) })

	// Persiste la vue modèle : messages d'outils (assistant tool_calls + résultats)
	// PUIS la réponse finale — même ordre que l'ancien client, pour que le modèle
	// garde la trace de ce qu'il a fait. Sauf si un Reset est passé entre-temps :
	// la nouvelle conversation vide ne doit pas hériter de la fin de l'ancienne.
	c.mu.Lock()
	if c.epoch == epoch {
		if newBase != nil {
			c.Messages = newBase
		}
		c.Messages = append(c.Messages, extra...)
		if s := content.String(); strings.TrimSpace(s) != "" {
			c.Messages = append(c.Messages, Message{Role: "assistant", Content: s})
		}
		// Rappel de loki resté sans réponse (stop, erreur) ou collé à un autre
		// user : retiré, sinon deux `user` d'affilée au tour suivant.
		c.Messages = dropStrayNudges(c.Messages)
		// Le dernier compte couvre tout ce qui vient d'être rangé : la dernière
		// requête portait le tour entier, sa génération en est la réponse.
		if sawUsage {
			c.ctxUsedLen = len(c.Messages)
		}
		c.genPeak = max(c.genPeak, turnPeak)
	}
	msgs = append([]Message(nil), c.Messages...)
	ctxUsed = c.ctxNowLocked(msgs, local)
	peak = c.genPeak
	stale := c.epoch != epoch
	c.mu.Unlock()

	// Moteur muet sur l'usage : on publie l'estimation, sinon la jauge reste à
	// zéro et l'utilisateur ne voit jamais son contexte se remplir — ni la
	// compaction arriver.
	if !stale && !sawUsage {
		est := estimateTokens(msgs)
		c.mu.Lock()
		if c.epoch == epoch {
			c.CtxUsed = est
			c.ctxUsedLen = len(msgs)
		}
		c.mu.Unlock()
		c.appendDelta(epoch, map[string]any{"ctx_used": est})
		ctxUsed = est
	}

	// Boucle du contrat (mode code) : vérification indépendante des critères,
	// corrections, re-vérification — AVANT de rendre la main. Voir
	// code_verify.go. No-op hors mode code ou sans critères.
	if !stale && ctx.Err() == nil {
		c.codeVerifyLoop(ctx, caps, temperature, epoch, asked)
		c.mu.Lock()
		msgs = append([]Message(nil), c.Messages...)
		ctxUsed = c.ctxNowLocked(msgs, local)
		peak = c.genPeak
		stale = c.epoch != epoch
		c.mu.Unlock()
	}

	// Compaction de FIN DE TOUR. C'était LE trou : le seuil n'était testé qu'au
	// DÉBUT d'un tour, avec le contexte du tour précédent. Le tour qui fait
	// franchir le seuil se termine donc à 81% et… rien. La jauge reste haute, seul
	// le bouton manuel s'affiche, et l'utilisateur voit un compactage automatique
	// qui « ne marche pas » — alors qu'il attendait simplement le message suivant.
	// On compacte donc DÈS que le tour qui a franchi le seuil est fini : la jauge
	// retombe tout de suite et le tour suivant démarre avec de la marge.
	// Sauf si un Reset est passé (rien à compacter) ou si le tour a été annulé
	// (bouton stop) : on n'enchaîne pas plusieurs secondes de résumé sur un stop.
	if stale || ctx.Err() != nil || !compactNeeded(msgs, ctxUsed, peak) {
		return
	}
	// context.Background() et non ctx : le tour est terminé, son contexte peut
	// être annulé alors que cette compaction-là doit aller au bout.
	c.compactAndPublish(context.Background(), epoch, "fin-tour", msgs, ctxUsed, caps)
}

// ctxNowLocked : contexte à juger pour compacter, c.mu tenu. llama-server
// local : le dernier compte plus les messages arrivés depuis (ctxPending). Preset
// externe : le dernier compte seul, comme avant — son CtxUsed garde encore le
// raisonnement (ctxAfter n'y retire rien), qui couvre déjà ce complément ; l'y
// ajouter le ferait compacter plus tôt qu'avant.
func (c *Conversation) ctxNowLocked(msgs []Message, local bool) int {
	if !local {
		return c.CtxUsed
	}
	return ctxPending(c.CtxUsed, c.ctxUsedLen, msgs)
}

// CompactNow force une compaction du contexte MAINTENANT, sans attendre le seuil
// (bouton « compacter » de l'UI). Détaché comme la génération : émet la bannière
// de progression, résume les anciens tours, remplace le torse et persiste. Les
// événements passent par le flux d'abonnement, donc tous les appareils voient la
// progression. Renvoie ErrBusy si un tour est déjà en cours.
func (c *Conversation) CompactNow() error {
	if !healthCheck() {
		return errModelLoading
	}
	local := !externalActive()
	c.mu.Lock()
	if c.Generating {
		c.mu.Unlock()
		return ErrBusy
	}
	c.Generating = true
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	msgs := append([]Message(nil), c.Messages...)
	lastReal := c.ctxNowLocked(msgs, local) // dernier contexte réel mesuré, pour estimer le surcoût fixe
	epoch := c.epoch
	c.mu.Unlock()

	go func() {
		defer func() {
			c.mu.Lock()
			// Même précaution que dans generate : une compaction abandonnée par un
			// Reset ne doit pas déclarer « libre » le tour qui a démarré depuis.
			if c.epoch == epoch {
				c.Generating = false
				c.cancel = nil
			}
			c.mu.Unlock()
		}()
		if _, changed := c.compactAndPublish(ctx, epoch, "manuel", msgs, lastReal, Caps{}); changed {
			c.persist()
		}
	}()
	return nil
}

// Stop interrompt la génération en cours (le cas échéant).
func (c *Conversation) Stop() {
	c.mu.Lock()
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Reset démarre une nouvelle conversation (vide) pour TOUS les appareils. On
// interrompt une éventuelle génération, on vide tout et on bump epoch pour que
// les abonnés reçoivent l'ordre de nettoyer leur affichage.
//
// Reset DÉBLOQUE toujours, et c'est sa deuxième raison d'être. Il se contentait
// avant de vider le fil : si un tour restait coincé (moteur redémarré sous ses
// pieds, commande shell accrochée à ses tubes), Generating restait vrai pour
// toujours et tout message suivant se voyait refusé « génération en cours ».
// Vider le fil ne changeait rien, rafraîchir non plus : il fallait redémarrer le
// service. « Nouvelle conversation » est le geste qu'on tente naturellement dans
// ce cas ; il doit donc rendre la main, quoi qu'il arrive au tour abandonné, que
// le bump d'epoch réduit de toute façon au silence.
func (c *Conversation) Reset() {
	c.Stop()
	// Vider la discussion efface aussi ses fichiers (dépôts, captures, ce que
	// l'agent y a écrit) : les messages qui les mentionnaient disparaissent, plus
	// rien ne les rattacherait à quoi que ce soit. L'UI le dit avant de demander
	// confirmation.
	dropConvFiles(convEnsureActive())
	c.mu.Lock()
	c.Messages = nil
	c.Log = nil
	c.Seq = 0
	c.CtxUsed = 0
	c.ctxUsedLen, c.genPeak = 0, 0
	c.epoch++
	c.Generating = false
	c.benching = false // Stop a annulé le bench : le verrou est rendu ici
	c.cancel = nil
	c.queued = nil // les messages en attente visaient le fil qu'on vient de vider
	c.cond.Broadcast()
	c.mu.Unlock()
	c.persist()
}

// evSeq0 lit le seq de DÉBUT d'un bloc texte : sur un événement coalescé (dont le
// Seq vaut celui du DERNIER delta fusionné), c'est le seq du PREMIER delta du bloc ;
// sur un delta brut, c'est son propre Seq.
//
// ⚠️ Sans ça : un client qui a déjà affiché une PARTIE d'un bloc (streaming en
// cours) et qui reçoit ensuite ce bloc coalescé — parce que la compaction de fin de
// tour a remplacé la suite de deltas par un seul événement de Seq plus grand que
// son dernier seq vu — le concatène à ce qu'il affichait déjà : la réponse
// apparaissait DEUX FOIS (la 1re copie tronquée à l'endroit exact où le client en
// était). Cf. `replace` ci-dessous.
func evSeq0(d map[string]any, fallback int) int {
	switch v := d["seq0"].(type) {
	case int:
		return v
	case float64: // relu depuis le JSON persisté
		return int(v)
	}
	return fallback
}

// decorateEvent aplatit un événement du journal pour l'émission SSE et marque
// `replace` quand le client a DÉJÀ vu le début du bloc (from tombe à l'intérieur) :
// le texte envoyé est alors le bloc ENTIER, donc le client doit remplacer sa bulle
// au lieu d'y concaténer.
func decorateEvent(ev LogEvent, from int) map[string]any {
	out := map[string]any{"seq": ev.Seq, "ts": ev.TS}
	for k, v := range ev.Delta {
		out[k] = v
	}
	if isTextDelta(ev.Delta) && evSeq0(ev.Delta, ev.Seq) <= from {
		out["replace"] = true
	}
	return out
}

// isTextDelta : événement porteur de texte (content / reasoning_content).
func isTextDelta(d map[string]any) bool {
	if _, ok := d["content"].(string); ok {
		return true
	}
	_, ok := d["reasoning_content"].(string)
	return ok
}

// coalesceReplay fusionne les deltas texte consécutifs (content / reasoning_content)
// d'un même bloc en UN seul événement, pour que le replay au chargement soit léger
// (quelques événements par tour au lieu de milliers de tokens). On conserve le
// nombre de tokens fusionnés (toks) et les bornes d'horodatage (ts0→ts) pour que le
// client reconstitue le compteur ET la vitesse. Les événements non-texte (user,
// tool_used, stats, turn_done…) passent tels quels.
func coalesceReplay(events []LogEvent, from int) []map[string]any {
	var out []map[string]any
	var buf strings.Builder
	bufKey := ""
	var bufSeq, bufToks, bufSeq0 int
	var bufTs0, bufTs int64
	flush := func() {
		if bufKey == "" {
			return
		}
		m := map[string]any{bufKey: buf.String(), "seq": bufSeq, "ts": bufTs, "ts0": bufTs0, "toks": bufToks, "seq0": bufSeq0}
		// Le client a déjà affiché le début de ce bloc (from tombe dedans, ce qui
		// arrive quand la compaction de fin de tour l'a fusionné) : on renvoie le
		// bloc entier et on lui demande de REMPLACER sa bulle, pas d'y ajouter.
		if bufSeq0 <= from {
			m["replace"] = true
		}
		out = append(out, m)
		buf.Reset()
		bufKey, bufSeq, bufToks, bufTs0, bufTs, bufSeq0 = "", 0, 0, 0, 0, 0
	}
	// Coalescence des outils : un appel d'outil génère plein d'événements tool_used
	// intermédiaires (streaming des arguments) jusqu'à un dernier avec done=true. Au
	// replay, seul l'état FINAL de chaque bulle compte (les intermédiaires ne servent
	// qu'à l'affichage progressif en direct). Sans ça, un long fil rejoue des milliers
	// de tool_used → 2-3 s de rendu inutile sur mobile. On ne garde donc que les
	// done=true, en mémorisant le dernier non-done pour le cas d'un outil interrompu.
	var pendingTool map[string]any
	flushTool := func() {
		if pendingTool != nil {
			out = append(out, pendingTool)
			pendingTool = nil
		}
	}
	for _, ev := range events {
		if ev.Seq <= from {
			continue
		}
		_, isTool := ev.Delta["tool_used"].(map[string]any)
		// ⚠️ On ne « flushe » PAS l'annonce d'outil en attente sur un événement
		// non-outil. Un appel d'outil s'écrit dans le journal en DEUX temps : une
		// annonce (done=false, juste « je vais lancer X ») puis le résultat
		// (done=true). Si un événement non-outil (stats, un bout de raisonnement)
		// se glisse ENTRE les deux, flusher ici émettait l'annonce PUIS le done →
		// le même outil apparaissait DEUX fois (à l'export Markdown et au replay).
		// On garde donc l'annonce en attente : le done la remplace (émis une seule
		// fois), et si aucun done n'arrive jamais (outil interrompu par un stop),
		// le flushTool final l'émet une seule fois, en fin de fil.
		// Delta texte ? (une seule clé content ou reasoning_content, valeur string)
		key := ""
		if s, ok := ev.Delta["content"].(string); ok {
			key, _ = "content", s
		} else if s, ok := ev.Delta["reasoning_content"].(string); ok {
			key, _ = "reasoning_content", s
		}
		if key != "" {
			if bufKey != "" && bufKey != key {
				flush()
			}
			if bufKey == "" {
				bufKey, bufTs0, bufSeq0 = key, evTS0(ev.Delta, ev.TS), evSeq0(ev.Delta, ev.Seq)
			}
			buf.WriteString(ev.Delta[key].(string))
			bufSeq, bufTs = ev.Seq, ev.TS
			bufToks += evToks(ev.Delta) // 1 pour un delta brut, N pour un événement déjà coalescé
			continue
		}
		flush()
		m := map[string]any{"seq": ev.Seq, "ts": ev.TS}
		for k, v := range ev.Delta {
			m[k] = v
		}
		if isTool {
			tu, _ := ev.Delta["tool_used"].(map[string]any)
			done, _ := tu["done"].(bool)
			if done {
				pendingTool = nil // les intermédiaires de cet outil sont superflus
				out = append(out, m)
			} else {
				pendingTool = m // on retient le dernier état non terminé, sans l'émettre
			}
			continue
		}
		out = append(out, m)
	}
	flush()
	flushTool()
	return out
}

// historyTailDefault : échanges rejoués à l'ouverture d'une session (reset) pour
// un client qui gère la pagination de l'historique.
const historyTailDefault = 20

// historyCut calcule où couper le rejeu pour ne garder que les `tail` derniers
// échanges (un échange commence à un événement `user`). Renvoie le Seq à partir
// duquel rejouer (exclu) et le nombre d'échanges masqués ; (0, 0) = tout
// rejouer. Repris d'AJEAN 0.15.7.
func historyCut(log []LogEvent, tail int) (int, int) {
	if tail <= 0 {
		return 0, 0
	}
	seen := 0
	for i := len(log) - 1; i >= 0; i-- {
		if _, ok := log[i].Delta["user"]; !ok {
			continue
		}
		seen++
		if seen == tail {
			hidden := 0
			for _, ev := range log[:i] {
				if _, ok := ev.Delta["user"]; ok {
					hidden++
				}
			}
			if hidden == 0 {
				return 0, 0
			}
			return log[i].Seq - 1, hidden
		}
	}
	return 0, 0
}

// historyHead : événements qui annoncent un historique tronqué. Les états que
// les échanges masqués avaient posés et que le client garde (mode Chat/Code,
// critères du mode Code) sont réémis tels qu'ils étaient à la coupe — sans eux,
// une discussion en mode Code rouverte s'afficherait en mode Chat.
func historyHead(log []LogEvent, cut, hidden int) []map[string]any {
	out := []map[string]any{{"history_more": hidden}}
	var mode, crit map[string]any
	for _, ev := range log {
		if ev.Seq > cut {
			break
		}
		if _, ok := ev.Delta["mode"]; ok {
			mode = ev.Delta
		}
		if _, ok := ev.Delta["criteria"]; ok {
			crit = ev.Delta
		}
	}
	for _, d := range []map[string]any{mode, crit} {
		if d != nil {
			out = append(out, d)
		}
	}
	return out
}

// Subscribe : abonnement sans pagination (rejeu complet).
func (c *Conversation) Subscribe(ctx context.Context, from int, emit func(map[string]any) bool) {
	c.SubscribeTail(ctx, from, -1, "", emit)
}

// SubscribeTail diffuse les événements au client via emit : d'abord un REPLAY coalescé
// de Log[from:] (léger), puis un caught_up, puis le DIRECT événement par événement.
// Bloque jusqu'à ce que ctx (la connexion HTTP) soit annulé — la génération, elle,
// continue indépendamment. emit renvoie false si l'écriture échoue (client parti).
func (c *Conversation) SubscribeTail(ctx context.Context, from int, tail int, convID string, emit func(map[string]any) bool) {
	c.subscribeSink(ctx, from, tail, convID, tailSink{emit: emit})
}

// tailSink : sortie d'un abonné. emit écrit et pousse aussitôt ; queue met en
// tampon (en écrivant d'office un tampon plein) et flush pousse ce qui attend.
// Sans queue/flush, tout passe par emit, un événement à la fois.
type tailSink struct {
	emit  func(map[string]any) bool
	queue func(map[string]any) bool
	flush func() bool
}

// subscribeSink : SubscribeTail, en regroupant le direct. Au réveil, plusieurs
// événements peuvent attendre (un jeton chacun) : une écriture et un flush par
// événement multipliaient les appels système et les trames d'un client en
// retard, ou d'un relais chiffré. Ils partent ensemble, dans l'ordre.
func (c *Conversation) subscribeSink(ctx context.Context, from int, tail int, convID string, sink tailSink) {
	emit := sink.emit
	queue, flush := sink.queue, sink.flush
	if queue == nil {
		queue = emit
	}
	if flush == nil {
		flush = func() bool { return true }
	}
	// Réveille les attentes de cond quand la connexion se ferme.
	go func() {
		<-ctx.Done()
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	}()

	// 0. Amorçage anti-buffering. Le chat E2E d'app.ajean.link traverse Cloudflare
	// (ajean.link, proxy orange) : tant qu'un proxy intermédiaire n'a pas reçu assez
	// d'octets, il bufferise la réponse et ne la relaie qu'en retard (symptôme :
	// derniers messages qui arrivent 20-30 s après le reste, de façon intermittente
	// sur Safari). Envoyer d'emblée un gros événement de padding force le proxy à
	// basculer en mode streaming tout de suite. Le client ignore la clé `pad`.
	if !emit(map[string]any{"pad": strings.Repeat("·", 2048)}) {
		return
	}

	// 1. Replay coalescé (snapshot hors verrou pour ne pas bloquer la génération).
	c.mu.Lock()
	snapshot := append([]LogEvent(nil), c.Log...)
	epoch := c.epoch
	c.mu.Unlock()
	// ⚠️ Garde-fou anti-« premiers messages manquants au changement de session ».
	// Le client se réabonne avec from=lastSeq (le dernier Seq qu'il a vu). Les Seq
	// ne sont PAS globaux : ouvrir une session PLUS ANCIENNE charge un journal dont
	// les Seq sont plus bas. Si le client garde un from élevé (hérité de la session
	// qu'il quittait) et se reconnecte, coalesceReplay saute tout (Seq <= from) →
	// la conversation ouverte apparaît tronquée, voire vide. En session normale, le
	// client ne peut jamais avoir un from SUPÉRIEUR au dernier Seq du journal ; s'il
	// l'est, son curseur vient d'une autre session → on repart du début.
	//
	// ⚠️ Même famille, cas plus sournois (AJEAN 0.14.0) : un AUTRE appareil a
	// changé de discussion pendant que ce client était déconnecté, et son `from`
	// est PLUS BAS que le dernier Seq de la nouvelle. Le rejeu greffait alors la
	// fin de la nouvelle discussion sur le début de l'ancienne restée à l'écran —
	// deux fils fusionnés jusqu'au prochain rafraîchissement. Le client renvoie
	// l'id de la discussion qu'il affiche (convID) : s'il ne correspond plus, on
	// lui ordonne d'abord de vider l'écran (reset), puis on rejoue tout.
	curID := getStr(bkChat, ckActive)
	staleConv := from > 0 && convID != "" && convID != curID
	if n := len(snapshot); n > 0 && from > snapshot[n-1].Seq {
		from = 0
		staleConv = staleConv || convID != ""
	}
	if staleConv {
		from = 0
		if !emit(map[string]any{"reset": true, "id": curID}) {
			return
		}
	}
	// Chargement initial d'un client paginé : on ne rejoue que la fin du fil,
	// précédée de {history_more: N}. Sur un long fil d'agent, tout rejouer
	// envoyait des Mo et bâtissait des milliers de bulles avant d'afficher quoi
	// que ce soit.
	if from == 0 && tail > 0 {
		if cut, hidden := historyCut(snapshot, tail); hidden > 0 {
			from = cut
			for _, ev := range historyHead(snapshot, cut, hidden) {
				if !emit(ev) {
					return
				}
			}
		}
	}
	last := from
	for _, ev := range coalesceReplay(snapshot, from) {
		if ctx.Err() != nil {
			return
		}
		if !emit(ev) {
			return
		}
		if s, ok := ev["seq"].(int); ok {
			last = s
		}
	}
	if !emit(map[string]any{"caught_up": true, "id": curID}) {
		return
	}
	// Taille du contexte connue MAINTENANT : le journal ne rejoue que les
	// événements du fil, et `ctx_used` n'y figure qu'aux tours où il a changé —
	// souvent hors de la fenêtre rejouée. Sans cet envoi, toute page rechargée
	// affichait « 0 / N (0 %) » sur une discussion pourtant bien remplie.
	c.mu.Lock()
	used := c.CtxUsed
	c.mu.Unlock()
	if used > 0 && !emit(map[string]any{"ctx_used": used}) {
		return
	}
	// Le padding doit venir APRÈS caught_up, pas dessus. Un proxy (Cloudflare) garde
	// toujours le DERNIER bout de flux en tampon jusqu'au prochain flush (~2-3 s). En
	// envoyant un gros pad juste après, ce sont ses octets — et non le dernier vrai
	// message — qui deviennent la « queue » qui attend : le dernier message et
	// caught_up, eux, sont poussés dehors immédiatement. Le client ignore `pad`.
	// 16 Ko : largement au-dessus du tampon de coalescence d'un proxy courant.
	if !emit(map[string]any{"pad": strings.Repeat("·", 16384)}) {
		return
	}

	// 2. Direct : événements granulaires au-delà de `last`.
	c.mu.Lock()
	for {
		if ctx.Err() != nil {
			c.mu.Unlock()
			return
		}
		if c.epoch != epoch { // reset → on ordonne au client de nettoyer et on repart
			epoch = c.epoch
			last = 0
			// Session ouverte (fil complet qui suit) chez un client paginé : on
			// saute directement aux derniers échanges.
			var head []map[string]any
			if tail >= 0 {
				if cut, hidden := historyCut(c.Log, historyTailDefault); hidden > 0 {
					last = cut
					// ctx_used vit dans le journal des échanges masqués : on le
					// donne tel qu'il est maintenant, sinon la jauge resterait à 0.
					head = append(historyHead(c.Log, cut, hidden), map[string]any{"ctx_used": c.CtxUsed})
				}
			}
			c.mu.Unlock()
			// id : la discussion désormais affichée (lue hors verrou — base).
			if !emit(map[string]any{"reset": true, "id": getStr(bkChat, ckActive)}) {
				return
			}
			for _, ev := range head {
				if !emit(ev) {
					return
				}
			}
			c.mu.Lock()
			continue
		}
		// Copie des événements en attente SOUS verrou, émission HORS verrou : on
		// n'itère jamais sur c.Log pendant que la génération peut y écrire ou que
		// la troncature (maxLogEvents) peut le déplacer.
		//
		// c.Log est trié par Seq croissant : on trouve le premier événement neuf
		// par dichotomie plutôt qu'en relisant tout. Le balayage complet coûtait
		// la longueur du journal (jusqu'à 20 000) À CHAQUE TOKEN et pour CHAQUE
		// appareil connecté, verrou tenu — donc au détriment de la génération
		// elle-même. C'est quadratique sur un long fil.
		i := sort.Search(len(c.Log), func(i int) bool { return c.Log[i].Seq > last })
		var pending []LogEvent
		if i < len(c.Log) {
			pending = append(pending, c.Log[i:]...)
		}
		if len(pending) == 0 {
			c.cond.Wait()
			continue
		}
		lastEmitted := last
		last = pending[len(pending)-1].Seq
		c.mu.Unlock()
		for _, ev := range pending {
			// `lastEmitted` (avant mise à jour) sert de repère : si la compaction de
			// fin de tour vient de fusionner un bloc que le client suivait en direct,
			// l'événement fusionné arrive avec un Seq supérieur au sien → `replace`.
			if !queue(decorateEvent(ev, lastEmitted)) {
				return
			}
		}
		if !flush() {
			return
		}
		c.mu.Lock()
	}
}
