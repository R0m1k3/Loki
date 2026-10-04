package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Routage des requêtes entre les deux slots de SIDE_SLOT (backend_serve_side.go).
//
// Sans id_slot, llama-server choisit le slot par similarité de prompt ou, à
// défaut, le moins récemment servi : un vérificateur peut alors tomber sur le
// slot 0 et écraser la conversation — exactement ce que la clé doit empêcher.
// Tant que le second slot est en service, CHAQUE requête de Loki porte donc son
// id_slot : 0 pour le fil de la discussion (tour, étapes d'outils,
// préchauffage, résumé en continuation, qui prolonge le prompt du slot 0), 1
// pour tout le reste (vérification, sous-agent, tâche, bench, résumé sur
// transcription, clients /v1).
//
// « En service » se lit sur le moteur, pas dans la configuration : /props doit
// annoncer 2 slots d'au moins CTX jetons chacun, ce que seul un lancement
// SIDE_SLOT accepté produit. Un moteur relancé sans la clé, refusé ou pas encore
// prêt : aucun id_slot, la requête est celle d'avant. Et sur un seul slot, un
// id_slot explicite ferait même sauter le rechargement depuis le cache RAM
// (f_keep indéfini sur un slot vide) : il ne doit jamais y partir.

// sideSlotProbeTTL : durée pendant laquelle la lecture de /props vaut. Un
// moteur relancé entre-temps sur un seul slot recevrait au pire un id_slot 1,
// que llama.cpp ramène au slot 0 : le comportement d'avant la clé.
const sideSlotProbeTTL = 10 * time.Second

var sideSlotProbe struct {
	mu   sync.Mutex
	key  string
	at   time.Time
	live bool
}

// sideSlotProps lit /props du moteur local : nombre de slots et fenêtre d'un
// slot. Remplaçable par les tests.
var sideSlotProps = func(ep chatEndpoint) (slots, nCtx int, ok bool) {
	var props struct {
		TotalSlots int `json:"total_slots"`
		Default    struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	iso := slotIsolator{base: fmt.Sprintf("http://localhost:%d", LLMPort()), auth: ep.auth, client: http.DefaultClient}
	if err := iso.get(context.Background(), "/props", &props); err != nil {
		return 0, 0, false
	}
	return props.TotalSlots, props.Default.NCtx, true
}

// sideSlotLive : le second slot est-il en service sur le moteur local ? Sans la
// clé, ni lecture ni requête : faux tout de suite.
func sideSlotLive(ep chatEndpoint) bool {
	if ep.External {
		return false
	}
	cfg := ReadConfig()
	if !sideSlotOn(cfg) || isExternalConfig(cfg) {
		return false
	}
	win := ctxWindow()
	key := fmt.Sprintf("%d|%d", LLMPort(), win)
	sideSlotProbe.mu.Lock()
	defer sideSlotProbe.mu.Unlock()
	if sideSlotProbe.key == key && time.Since(sideSlotProbe.at) < sideSlotProbeTTL {
		return sideSlotProbe.live
	}
	slots, nCtx, ok := sideSlotProps(ep)
	if !ok {
		// Moteur injoignable (relance en cours) : rien de retenu, la prochaine
		// requête redemande.
		return false
	}
	// llama.cpp arrondit la fenêtre d'un slot au multiple de 256 supérieur.
	live := slots == 2 && nCtx >= win
	sideSlotProbe.key, sideSlotProbe.at, sideSlotProbe.live = key, time.Now(), live
	return live
}

// engineSlotFor : l'id_slot d'une requête vers ep, -1 = aucun (la requête part
// telle qu'avant la clé). main : fil de la discussion.
func engineSlotFor(ep chatEndpoint, main bool) int {
	if !sideSlotLive(ep) {
		return -1
	}
	if main {
		return 0
	}
	return 1
}

// slotIsMain : les complétions de cette nature restent sur le slot de la
// discussion. Toutes les autres (vérification, sous-agent, tâche, bench,
// résumé) passent par le second.
func slotIsMain(kind string) bool {
	return kind == perfMain || kind == perfPrewarm
}

// setEngineSlot pose id_slot dans payload ; slot < 0 : rien.
func setEngineSlot(payload map[string]any, slot int) {
	if slot >= 0 {
		payload["id_slot"] = slot
	}
}

// payloadOnSideSlot : la requête part-elle sur le second slot ? Elle ne touche
// alors pas au slot de la discussion (engineRequestBeginSide).
func payloadOnSideSlot(payload map[string]any) bool {
	s, ok := payload["id_slot"].(int)
	return ok && s == 1
}

// sideSlotProxyMaxBody : au-delà, un corps de client /v1 n'est pas réécrit mais
// refusé — le laisser passer sans id_slot, c'est risquer le slot 0.
const sideSlotProxyMaxBody = 128 << 20

// sideSlotProxyRewrite pose id_slot 1 dans le corps JSON d'une requête POST
// /v1/* d'un client externe, quel que soit l'id_slot qu'il demandait : seul
// Loki parle au slot de la discussion. Sans second slot en service, rien n'est
// lu ni touché (side=false). Un corps qui n'est pas un objet JSON passe tel
// quel : le moteur le refusera de lui-même. status != 0 : refuser la requête
// avec ce code et ce message.
func sideSlotProxyRewrite(r *http.Request) (side bool, status int, msg string) {
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/") || r.Body == nil {
		return false, 0, ""
	}
	if !sideSlotLive(resolveChatEndpoint()) {
		return false, 0, ""
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, sideSlotProxyMaxBody+1))
	r.Body.Close()
	if err != nil {
		return false, http.StatusBadRequest, "corps illisible : " + err.Error()
	}
	if len(raw) > sideSlotProxyMaxBody {
		return false, http.StatusRequestEntityTooLarge, "corps trop gros pour être routé vers le slot des clients"
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil && obj != nil {
		obj["id_slot"] = json.RawMessage("1")
		if b, err := json.Marshal(obj); err == nil {
			raw, side = b, true
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	r.Header.Set("Content-Length", strconv.Itoa(len(raw)))
	return side, 0, ""
}

// reqExceedsCtx : le libellé de llama.cpp pour un prompt plus long que la
// fenêtre d'un slot.
var reqExceedsCtx = regexp.MustCompile(`(?i)exceeds the available context size|request \(\d+ tokens\)`)

// sideSlotPoolError : « Context size has been exceeded. » sans le nombre de
// jetons de la requête n'est pas un prompt trop long, c'est le cache KV qui a
// manqué de place pendant le calcul. Avec deux slots, ce serait la faute du
// voisin, pas de la conversation : la compacter ou la tronquer perdrait de
// l'information pour rien. Avec des flux séparés (--no-kv-unified) ça ne doit
// pas arriver ; le filet ne joue que si le second slot est en service.
func sideSlotPoolError(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "context size has been exceeded") && !reqExceedsCtx.MatchString(msg)
}
