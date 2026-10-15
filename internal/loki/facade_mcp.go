package loki

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// facade_mcp.go — serveurs MCP tenus par la façade Rust (LOKI_MCP=rust, lot 20
// de la migration ; expérimental, le Go reste le défaut) : configuration,
// catalogue, état et pool de sessions (rust/loki-rs/src/mcp).
//
// Un seul pool : celui de la façade. Le Go, qui exécute encore les outils de
// l'agent, lui demande les outils MCP à annoncer au modèle et leurs appels
// (/internal/facade/v1/mcp/*), quelle que soit la conversation qui tourne ;
// son propre pool reste vide.

const facadeMCPEnv = "LOKI_MCP"

// facadeMCP : la façade tient-elle les serveurs MCP ?
func facadeMCP() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadeMCPEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// facadeMCPRoutes : les routes que la façade sert alors (mcp/mod.rs).
var facadeMCPRoutes = map[string]bool{
	"/api/mcp":         true,
	"/api/mcp/catalog": true,
	"/api/mcp/save":    true,
	"/api/mcp/delete":  true,
	"/api/mcp/toggle":  true,
	"/api/mcp/tool":    true,
	"/api/mcp/test":    true,
}

// facadeMCPJSON appelle l'API interne MCP de la façade et décode sa réponse.
// Délai large : un appel d'outil MCP peut durer (mcpCallTimeout), une
// première connexion npx/uvx aussi (mcpConnectTimeoutCold).
func facadeMCPJSON(path string, body any, out any) error {
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
	resp, err := (&http.Client{Timeout: mcpConnectTimeoutCold + mcpCallTimeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("façade : %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// facadeMCPTools : les outils MCP à annoncer, tenus par la façade.
func facadeMCPTools() []Tool {
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := facadeMCPJSON("/internal/facade/v1/mcp/tools", map[string]any{}, &out); err != nil {
		fmt.Fprintf(os.Stderr, "[mcp] outils demandés à la façade : %v\n", err)
		return nil
	}
	return out.Tools
}

// facadeMCPCall : un outil MCP exécuté par la façade.
func facadeMCPCall(name string, args map[string]any) string {
	if args == nil {
		args = map[string]any{}
	}
	var out struct {
		Result string `json:"result"`
	}
	if err := facadeMCPJSON("/internal/facade/v1/mcp/call", map[string]any{"name": name, "args": args}, &out); err != nil {
		return "[erreur MCP] façade injoignable : " + err.Error()
	}
	return out.Result
}

// facadeMCPPrewarm : la façade ouvre ses sessions en tâche de fond.
func facadeMCPPrewarm() {
	go func() {
		var out map[string]any
		// La façade démarre le Go puis attend qu'il réponde : elle peut ne pas
		// écouter encore au tout premier instant.
		for i := 0; i < 10; i++ {
			if facadeMCPJSON("/internal/facade/v1/mcp/prewarm", map[string]any{}, &out) == nil {
				return
			}
			time.Sleep(time.Second)
		}
	}()
}
