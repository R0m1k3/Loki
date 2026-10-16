package loki

import (
	"context"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeMCPSlow branche un serveur MCP en mémoire dont l'outil « slow » ne rend
// la main qu'à l'annulation de sa requête (ou au bout de 30 s).
func fakeMCPSlow(t *testing.T) string {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "lent", Version: "1"}, nil)
	srv.AddTool(&mcpsdk.Tool{Name: "slow", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(30 * time.Second):
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "fini"}}}, nil
			}
		})
	st, ct := mcpsdk.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "loki-test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	close(ready)
	name := mcpExposedName("lent", "slow")
	mcpMgr.mu.Lock()
	if mcpMgr.sessions == nil {
		mcpMgr.sessions = map[string]*mcpSession{}
	}
	if mcpMgr.registry == nil {
		mcpMgr.registry = map[string]mcpToolRef{}
	}
	mcpMgr.sessions["lent"] = &mcpSession{name: "lent", sess: cs, ready: ready}
	mcpMgr.registry[name] = mcpToolRef{server: "lent", tool: "slow"}
	mcpMgr.mu.Unlock()
	t.Cleanup(func() {
		mcpMgr.mu.Lock()
		delete(mcpMgr.sessions, "lent")
		delete(mcpMgr.registry, name)
		mcpMgr.mu.Unlock()
		cs.Close()
		ss.Close()
	})
	return name
}

// Le bouton stop annule l'appel MCP en cours au lieu d'attendre ses 120 s.
func TestMCPCallHonorsTurnCancel(t *testing.T) {
	testHome(t)
	name := fakeMCPSlow(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	start := time.Now()
	out := mcpCall(ctx, name, map[string]any{})
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("l'appel a ignoré l'annulation du tour (%s)", d)
	}
	if !strings.Contains(out, "interrompu") {
		t.Fatalf("sortie = %q ; attendu un appel interrompu", out)
	}
}

func TestMCPCallTimeoutFor(t *testing.T) {
	def := MCPTimeouts{}.normalized()
	cases := []struct {
		cfg  MCPServerConfig
		t    MCPTimeouts
		args map[string]any
		want time.Duration
	}{
		{MCPServerConfig{}, def, nil, 120 * time.Second},
		{MCPServerConfig{CallTimeoutSec: 600}, def, nil, 10 * time.Minute},
		{MCPServerConfig{}, def, map[string]any{"timeout": 300.0}, 330 * time.Second},          // secondes
		{MCPServerConfig{}, def, map[string]any{"timeout": 200000.0}, 230 * time.Second},       // millisecondes
		{MCPServerConfig{}, def, map[string]any{"timeout": 5000.0}, 120 * time.Second},         // 5 s : le défaut reste
		{MCPServerConfig{}, def, map[string]any{"timeout": "400"}, 430 * time.Second},          // chaîne
		{MCPServerConfig{}, def, map[string]any{"timeout": 86400.0 * 1000}, mcpCallTimeoutMax}, // plafond
		{MCPServerConfig{CallTimeoutSec: 30}, def, nil, 30 * time.Second},                      // plus court que le défaut
		{MCPServerConfig{}, MCPTimeouts{Connect: 20, Call: 45}, nil, 45 * time.Second},         // réglage global
		{MCPServerConfig{CallTimeoutSec: 90}, MCPTimeouts{Connect: 20, Call: 45}, nil, 90 * time.Second},
		{MCPServerConfig{}, MCPTimeouts{Connect: 20, Call: 3600}, nil, time.Hour}, // pas rogné par le plafond
	}
	for i, c := range cases {
		if got := mcpCallTimeoutFor(c.cfg, c.t, c.args); got != c.want {
			t.Errorf("cas %d : %s, attendu %s", i, got, c.want)
		}
	}
}
