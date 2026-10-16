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
// adresse et le jeton interne, sans lesquels rien ne peut lui être remis.
// Plus aucun réglage ne garde la conversation ici : la façade honore
// NUDGE_IN_TOOL (lot 23), les motifs de relance personnalisés (lot 30),
// SIDE_SLOT et SLOT_PERSIST (lot 31), KEEP_TURN_IMAGES (lot 32), PREWARM
// (lot 33) et PROJ_SNAPSHOT (lot 34, facade_projsnap.go).
func facadeChat() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeChatEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
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
	why := "adresse de la façade ou jeton interne manquant"
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(facadeChatEnv)), "rust") {
		why = facadeChatEnv + " absent"
	}
	sendJSON(w, http.StatusOK, map[string]any{"holder": "go", "reason": why})
}

func facadeAddr() string { return strings.TrimSpace(os.Getenv(facadeAddrEnv)) }

// facadeChatRoutes : les routes que la façade sert quand elle tient la
// conversation (chatapi.rs, chatx.rs). Le reste du mode Code (indice, travaux
// de fond, outil criteria) reste ici et lit les mêmes clés.
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
	// Lot 19 : export, mode Code et critères de la discussion ouverte.
	"/api/chat/export":   true,
	"/api/chat/mode":     true,
	"/api/chat/criteria": true,
	// Lot 39 : l'anneau de télémétrie est celui de la façade.
	"/api/perf/summary": true,
	// Lot 44 : les aperçus du navigateur piloté, tenu par la façade.
	"/api/chat/cushot": true,
}

// withFacadeChat remet la requête à la façade quand elle tient la
// conversation — ou les routes des modèles (facade_models.go), la dictée
// (facade_dictate.go), les réglages partagés (facade_settings.go), les fichiers
// de la discussion (facade_files.go), les notifications (facade_push.go), la
// mémoire (facade_mem.go), les presets (facade_presets.go), le pilotage du
// moteur (facade_engine.go), les interrupteurs système (facade_system.go), les
// projets (facade_projects.go), les serveurs MCP (facade_mcp.go), la mise à
// jour (facade_update.go), les backends llama.cpp (facade_backends.go), le
// moteur de l'image (facade_engineimg.go), Strata (facade_strata.go).
// Posé APRÈS requireWebAuth : seule une requête authentifiée ici part, avec le
// jeton interne qui en tient lieu là-bas.
func withFacadeChat(path string, h http.HandlerFunc) http.HandlerFunc {
	held, unavailable := facadeChat, "conversation indisponible : "
	switch {
	case facadeChatRoutes[path]:
	case facadeModelRoutes[path]:
		held, unavailable = facadeModels, "modèles indisponibles : "
	case facadeDictateRoutes[path]:
		held, unavailable = facadeDictate, "dictée indisponible : "
	case facadeSettingsRoutes[path]:
		held, unavailable = facadeSettings, "réglages indisponibles : "
	case facadeFilesRoutes[path]:
		held, unavailable = facadeFiles, "fichiers indisponibles : "
	case facadePushRoutes[path]:
		held, unavailable = facadePush, "notifications indisponibles : "
	case facadeMemRoutes[path]:
		held, unavailable = facadeMem, "mémoire indisponible : "
	case facadePresetsRoutes[path]:
		held, unavailable = facadePresets, "presets indisponibles : "
	case facadeEngineRoutes[path]:
		held, unavailable = facadeEngine, "pilotage du moteur indisponible : "
	case facadeSystemRoutes[path]:
		held, unavailable = facadeSystem, "réglages système indisponibles : "
	case facadeProjectsRoutes[path]:
		held, unavailable = facadeProjects, "projets indisponibles : "
	case facadeMCPRoutes[path]:
		held, unavailable = facadeMCP, "serveurs MCP indisponibles : "
	case facadeUpdateRoutes[path]:
		held, unavailable = facadeUpdate, "mise à jour indisponible : "
	case facadeBackendsRoutes[path]:
		held, unavailable = facadeBackends, "moteurs indisponibles : "
	case facadeEngineImageRoutes[path]:
		held, unavailable = facadeEngineImage, "moteur de l'image indisponible : "
	case facadeStrataRoutes[path]:
		held, unavailable = facadeStrata, "Strata indisponible : "
	case facadeTasksRoutes[path]:
		held, unavailable = facadeTasks, "tâches indisponibles : "
	case facadeBenchRoutes[path]:
		held, unavailable = facadeBench, "banc d'essai indisponible : "
	case facadeTuneRoutes[path]:
		held, unavailable = facadeTune, "optimiseur indisponible : "
	case facadeCoreRoutes[path]:
		held, unavailable = facadeCore, "façade indisponible : "
	default:
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !held() {
			h(w, r)
			return
		}
		facadeRelayAs(w, r, unavailable)
	}
}

// facadeRelayAs : relais transparent (flux compris) vers la façade ;
// unavailable préfixe l'erreur si elle ne répond pas.
func facadeRelayAs(w http.ResponseWriter, r *http.Request, unavailable string) {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: facadeAddr()})
			// L'hôte et le schéma du client, pas ceux du relais : la clé de
			// complétion affiche l'adresse à coller dans un client tiers.
			pr.Out.Host = pr.In.Host
			if p := pr.In.Header.Get("X-Forwarded-Proto"); p != "" {
				pr.Out.Header.Set("X-Forwarded-Proto", p)
			}
			pr.Out.Header.Set(internalTokenHeader, internalToken())
		},
		FlushInterval: -1, // SSE : chaque trame part aussitôt
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			sendJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": unavailable + err.Error()})
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

// facadeTplCaps : le dernier verdict de la sonde de gabarit de la façade,
// quand elle tient la conversation (c'est elle qui sonde après ses
// complétions).
func facadeTplCaps() (tplProbeResult, bool) {
	req, err := http.NewRequest(http.MethodPost, "http://"+facadeAddr()+"/internal/facade/v1/chat/tpl", strings.NewReader("{}"))
	if err != nil {
		return tplProbeResult{}, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(internalTokenHeader, internalToken())
	resp, err := facadeClient.Do(req)
	if err != nil {
		return tplProbeResult{}, false
	}
	defer resp.Body.Close()
	var out struct {
		OK     bool           `json:"ok"`
		Result tplProbeResult `json:"result"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil || !out.OK {
		return tplProbeResult{}, false
	}
	return out.Result, true
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
