package loki

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// facade_chat.go — conversation tenue par la façade Rust (LOKI_CHAT=rust, lot 6).
//
// La façade sert elle-même les routes du chat sur le port public. Deux chemins
// arrivent pourtant ICI sans passer par elle : le tunnel du relais, qui sert ce
// mux nu (/api/e2e/*), et sa redistribution /api/e2e/req dans le mux local. Ils
// liraient la conversation du Go — périmée — et pourraient même l'écrire en
// base par-dessus celle de la façade. Quand la façade tient la conversation :
//
//   - les routes du chat lui sont remises, authentifiées par le jeton interne
//     (la requête l'a déjà été ici) ;
//   - /api/e2e/chat lit son flux et le rechiffre événement par événement ;
//   - les événements d'affichage nés ici (bascule du mode Code, critères) vont
//     dans SON journal ;
//   - l'export lit la discussion enregistrée ;
//   - le déverrouillage de la mémoire lui fait recharger la discussion ;
//   - changer de projet, ou en supprimer un, passe par elle : la discussion
//     de ce process est périmée, l'enregistrer écraserait la sienne.

const (
	facadeChatEnv = "LOKI_CHAT"
	facadeAddrEnv = "LOKI_FACADE_ADDR"
)

// facadeChat : la façade tient-elle la conversation ? Il faut aussi son
// adresse et le jeton interne, sans lesquels rien ne peut lui être remis — et
// aucun réglage qu'elle ne sait pas honorer (facadeChatBlocker).
func facadeChat() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeChatEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != "" && facadeChatDecision() == ""
}

// facadeChatBlocker : le réglage qui garde la conversation ici, "" s'il n'y
// en a pas. Ces clés opt-in demandent une machinerie restée au Go (bloc projet
// figé, images gardées, slots du moteur, préchauffage, motifs de relance
// personnalisés).
func facadeChatBlocker(cfg map[string]string) string {
	switch {
	case projSnapEnabled(cfg):
		return "PROJ_SNAPSHOT"
	case keepTurnImagesOn(cfg):
		return "KEEP_TURN_IMAGES"
	case cfgOn(cfg, "NUDGE_IN_TOOL"):
		return "NUDGE_IN_TOOL"
	case sideSlotOn(cfg):
		return "SIDE_SLOT"
	case slotPersistOn(cfg):
		return "SLOT_PERSIST"
	case prewarmMode(cfg) != "":
		return "PREWARM"
	case strings.TrimSpace(getStr(bkState, "retry_patterns")) != "":
		return "retry_patterns"
	}
	return ""
}

// cfgOn : une clé booléenne posée (mêmes valeurs que les autres clés opt-in).
func cfgOn(cfg map[string]string, k string) bool {
	switch strings.ToLower(strings.TrimSpace(cfg[k])) {
	case "on", "1", "true", "yes", "oui":
		return true
	}
	return false
}

// Décision prise UNE fois par process : passer la conversation d'un détenteur
// à l'autre en cours de route laisserait chacun avec sa copie — un réglage
// changé vaut au redémarrage suivant. La façade la lit au démarrage
// (/internal/v1/chat/holder) et s'y range.
var (
	facadeDecisionMu   sync.Mutex
	facadeDecisionDone bool
	facadeDecisionWhy  string
)

func facadeChatDecision() string {
	facadeDecisionMu.Lock()
	defer facadeDecisionMu.Unlock()
	if !facadeDecisionDone {
		facadeDecisionWhy = facadeChatBlocker(ReadConfig())
		facadeDecisionDone = true
		if facadeDecisionWhy != "" {
			fmt.Fprintf(os.Stderr, "[chat] %s posé : la conversation reste tenue par le Go (LOKI_CHAT=rust ignoré)\n", facadeDecisionWhy)
		}
	}
	return facadeDecisionWhy
}

// handleInternalChatHolder : qui tient la conversation ({"holder":"rust"} ou
// {"holder":"go","reason":…}). La façade s'y range au démarrage.
func handleInternalChatHolder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET attendu"})
		return
	}
	if facadeChat() {
		sendJSON(w, http.StatusOK, map[string]any{"holder": "rust"})
		return
	}
	why := facadeChatDecision()
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(facadeChatEnv)), "rust") {
		why = facadeChatEnv + " absent"
	}
	sendJSON(w, http.StatusOK, map[string]any{"holder": "go", "reason": why})
}

func facadeAddr() string { return strings.TrimSpace(os.Getenv(facadeAddrEnv)) }

// facadeChatRoutes : les routes que la façade sert quand elle tient la
// conversation (chatapi.rs). Le reste (mode Code, critères, pièces jointes,
// fichiers, export) reste ici.
var facadeChatRoutes = map[string]bool{
	"/api/chat":                 true,
	"/api/chat/send":            true,
	"/api/chat/stop":            true,
	"/api/chat/reset":           true,
	"/api/chat/state":           true,
	"/api/chat/compact":         true,
	"/api/chat/tool-result":     true,
	"/api/conversations":        true,
	"/api/conversations/new":    true,
	"/api/conversations/switch": true,
	"/api/conversations/rename": true,
	"/api/conversations/delete": true,
}

// withFacadeChat remet la requête à la façade quand elle tient la
// conversation. Posé APRÈS requireWebAuth : seule une requête authentifiée
// ici part, avec le jeton interne qui en tient lieu là-bas.
func withFacadeChat(path string, h http.HandlerFunc) http.HandlerFunc {
	if !facadeChatRoutes[path] {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !facadeChat() {
			h(w, r)
			return
		}
		facadeRelay(w, r)
	}
}

// facadeRelay : relais transparent (flux compris) vers la façade.
func facadeRelay(w http.ResponseWriter, r *http.Request) {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: facadeAddr()})
			pr.Out.Header.Set(internalTokenHeader, internalToken())
		},
		FlushInterval: -1, // SSE : chaque trame part aussitôt
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			sendJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "conversation indisponible : " + err.Error()})
		},
	}
	rp.ServeHTTP(w, r)
}

// facadeClient : appels courts vers l'API interne de la façade.
var facadeClient = &http.Client{Timeout: 5 * time.Second}

// facadePost appelle l'API interne de la façade.
func facadePost(path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+facadeAddr()+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(internalTokenHeader, internalToken())
	resp, err := facadeClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("façade : %s", resp.Status)
	}
	return nil
}

// facadeError : refus de la façade, avec son statut (409 : un tour tourne).
type facadeError struct {
	status int
	msg    string
}

func (e *facadeError) Error() string { return e.msg }

// facadeCall appelle l'API interne de la façade et rend son refus éventuel
// tel quel (statut et message), pour les actions dont l'utilisateur attend
// la réponse.
func facadeCall(path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+facadeAddr()+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(internalTokenHeader, internalToken())
	// Délai large : enregistrer une discussion et en ouvrir une autre passe
	// par le disque (et par le chiffrement).
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("conversation indisponible : %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 {
		return nil
	}
	var out struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Error == "" {
		out.Error = "façade : " + resp.Status
	}
	return &facadeError{status: resp.StatusCode, msg: out.Error}
}

// facadeProjectSwitch : la façade enregistre la discussion du projet quitté
// (avec son projet), fait changer le projet actif ici
// (/internal/v1/project/activate), puis ouvre une discussion du nouveau.
func facadeProjectSwitch(slug string) error {
	return facadeCall("/internal/facade/v1/project/switch", map[string]any{"slug": slug})
}

// facadeDropProject : la façade supprime les discussions d'un projet (index,
// contenu, et ce qui les entoure ici par /internal/v1/conv/dropped).
func facadeDropProject(slug string) error {
	return facadeCall("/internal/facade/v1/project/drop", map[string]any{"slug": slug})
}

// facadeDelta : un événement d'affichage né ici, journalisé par la façade
// (tous les appareils le reçoivent). Rend false si la façade ne tient pas la
// conversation : l'appelant journalise alors comme avant.
func facadeDelta(delta map[string]any) bool {
	if !facadeChat() {
		return false
	}
	if err := facadePost("/internal/facade/v1/delta", delta); err != nil {
		fmt.Fprintf(os.Stderr, "[chat] événement non transmis à la façade : %v\n", err)
	}
	return true
}

// facadeReload : la mémoire vient d'être déverrouillée ; la façade recharge la
// discussion si elle l'avait chargée vide.
func facadeReload() {
	if !facadeChat() {
		return
	}
	go func() {
		if err := facadePost("/internal/facade/v1/reload", map[string]any{}); err != nil {
			fmt.Fprintf(os.Stderr, "[chat] rechargement de la discussion par la façade : %v\n", err)
		}
	}()
}

// facadeChatStream : /api/e2e/chat quand la façade tient la conversation. Son
// flux est lu ligne par ligne et chaque événement repart par out (scellé, un
// sceau par événement). Les battements de la façade sont ignorés : ce flux a
// les siens (sseHeartbeat).
func facadeChatStream(ctx context.Context, body []byte, out *sseStream) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+facadeAddr()+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(internalTokenHeader, internalToken())
	resp, err := http.DefaultClient.Do(req) // sans délai : un flux dure ce qu'il dure
	if err != nil {
		if ctx.Err() == nil {
			out.emit(map[string]any{"error": "conversation indisponible : " + err.Error()})
		}
		return
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Choices []struct {
				Delta map[string]any `json:"delta"`
			} `json:"choices"`
		}
		dec := json.NewDecoder(strings.NewReader(data))
		dec.UseNumber() // les nombres repartent tels quels
		if dec.Decode(&ev) != nil || len(ev.Choices) == 0 {
			continue
		}
		if !out.emit(ev.Choices[0].Delta) {
			return
		}
	}
}

// exportSource : la conversation à exporter. Tenue par la façade, celle du Go
// est périmée : on lit la discussion enregistrée (à jour à chaque fin de tour).
func exportSource() *Conversation {
	if !facadeChat() {
		return conv
	}
	c := &Conversation{}
	c.cond = sync.NewCond(&c.mu)
	if b := storeBytes(bkChat, convKey(convEnsureActive())); len(b) > 0 {
		_ = json.Unmarshal(b, c)
	}
	return c
}
