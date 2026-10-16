package loki

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// facade_browser.go — le navigateur piloté tenu par la façade (lot 44 de la
// migration Rust). Quand elle tient la conversation, la session Chrome est
// la sienne : ses tours exécutent les outils browser_* chez elle, et ceux du
// Go (sous-agents, tâches IA) passent par /internal/facade/v1/browser — un
// seul navigateur. L'aperçu après une action et la fermeture aussi.
// Façade injoignable : le navigateur du Go prend le relais.

// facadeBrowserClient : une action du navigateur peut attendre le
// lancement de Chrome, puis le chargement et le calme réseau de la page.
var facadeBrowserClient = &http.Client{Timeout: 3 * time.Minute}

// facadeBrowserCall appelle l'API du navigateur de la façade.
func facadeBrowserCall(body map[string]any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+facadeAddr()+"/internal/facade/v1/browser", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(internalTokenHeader, internalToken())
	resp, err := facadeBrowserClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("façade : %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// facadeBrowserTool exécute un outil browser_* à la façade ; ok faux : elle
// ne tient pas la conversation, ou ne répond pas (le Go le fait lui-même).
func facadeBrowserTool(name string, args map[string]any) (result string, image map[string]any, ok bool) {
	if !facadeChat() {
		return "", nil, false
	}
	var out struct {
		Result string         `json:"result"`
		Image  map[string]any `json:"image"`
	}
	if err := facadeBrowserCall(map[string]any{"op": "tool", "name": name, "args": args}, &out); err != nil {
		fmt.Fprintf(os.Stderr, "[cu] navigateur de la façade injoignable, repli local : %v\n", err)
		return "", nil, false
	}
	return out.Result, out.Image, true
}

// facadeBrowserShot : l'aperçu pris par la façade.
func facadeBrowserShot() (string, bool) {
	if !facadeChat() {
		return "", false
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := facadeBrowserCall(map[string]any{"op": "shot"}, &out); err != nil {
		return "", false
	}
	return out.ID, true
}

// facadeBrowserShutdown ferme le navigateur de la façade.
func facadeBrowserShutdown() {
	if !facadeChat() {
		return
	}
	var out struct{}
	if err := facadeBrowserCall(map[string]any{"op": "shutdown"}, &out); err != nil {
		fmt.Fprintf(os.Stderr, "[cu] fermeture du navigateur de la façade : %v\n", err)
	}
}
