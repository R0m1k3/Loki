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
//   - le déverrouillage de la mémoire lui fait recharger la discussion.

const (
	facadeChatEnv = "LOKI_CHAT"
	facadeAddrEnv = "LOKI_FACADE_ADDR"
)

// facadeChat : la façade tient-elle la conversation ? Il faut aussi son
// adresse et le jeton interne, sans lesquels rien ne peut lui être remis.
func facadeChat() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeChatEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
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
