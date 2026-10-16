//go:build linux

package loki

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// parity_mcp_test.go — journal de référence des serveurs MCP (lot 20 de la
// migration Rust) : fonctions pures du client (noms exposés, schémas,
// libellés, ligne du préambule, délais, aplatissement des résultats), lecture
// et mutations de la configuration, puis un vrai serveur du SDK MCP Go servi
// en Streamable HTTP : ce qu'il annonce et renvoie (tel que sur le fil), et ce
// que le client Go en tire (état, outils annoncés au modèle, appels). Rangé
// dans rust/loki-rs/tests/parity_mcp/mcp.json ; tests/mcp.rs le rejoue contre
// un faux serveur qui sert les mêmes réponses.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteMCP

const parityMCPFile = "../../rust/loki-rs/tests/parity_mcp/mcp.json"

type parityMCPMutation struct {
	Op    string `json:"op"`
	Name  string `json:"name"`
	Tool  string `json:"tool,omitempty"`
	On    bool   `json:"on,omitempty"`
	Cfg   string `json:"cfg,omitempty"` // MCPServerConfig en JSON
	Error string `json:"error"`
	State string `json:"state"` // state/mcp après l'opération
}

func TestPariteMCP(t *testing.T) {
	testHome(t)
	t.Cleanup(mcpCloseAll)
	t.Setenv(facadeMCPEnv, "")
	fx := map[string]any{}

	// --- fonctions pures ---
	san := map[string]string{}
	for _, s := range []string{"alpha", "mon serveur!", "__x__", "a.b/c", "é-t_é", "", "...", "A-B_c9"} {
		san[s] = mcpSanitize(s)
	}
	fx["sanitize"] = san
	var schemas []any
	for _, raw := range []string{`null`, `{"type":"object","properties":{"a":{"type":"string"}}}`, `true`, `[1]`, `{}`, `"x"`} {
		var v any
		_ = json.Unmarshal([]byte(raw), &v)
		schemas = append(schemas, map[string]any{"in": raw, "out": mcpNormalizeSchema(v)})
	}
	fx["schemas"] = schemas
	var labels []any
	for _, raw := range []string{`{}`, `{"path":"/a","query":"q"}`, `{"query":"","url":"http://x"}`, `{"name":"n","command":"c"}`,
		`{"x":1,"y":"<b>"}`, `{"long":"` + strings.Repeat("é", 200) + `"}`, `{"path":3,"file":"f"}`,
		`{"file":"f","path":"p"}`, `{"moyen":"` + strings.Repeat("a", 150) + `"}`} {
		var m map[string]any
		_ = json.Unmarshal([]byte(raw), &m)
		labels = append(labels, map[string]any{"in": raw, "out": mcpArgLabel(m)})
	}
	fx["labels"] = labels
	var servers []any
	for _, c := range [][2]string{
		{"mcp__alpha__x", "[MCP: alpha] desc"}, {"mcp__a_b__x", "[MCP: a]b] desc"}, {"mcp__a_b_c__x", "[MCP: a] b] c] d"},
		{"mcp__x__y", "pas de préfixe"}, {"autre", "[MCP: autre] d"}, {"mcp__b__x", "[MCP: a] d"},
	} {
		s, ok := mcpToolServer(Tool{Function: ToolFunction{Name: c[0], Description: c[1]}})
		servers = append(servers, map[string]any{"name": c[0], "desc": c[1], "server": s, "ok": ok})
	}
	fx["tool_server"] = servers
	var lines []any
	for _, set := range [][][2]string{
		{},
		{{"bash", "shell"}},
		{{"mcp__b__x", "[MCP: b] d"}, {"mcp__a__y", "[MCP: a] d"}, {"mcp__a__z", "[MCP: a] e"}},
		{{"mcp__a_b__x", "[MCP: a]b] d"}},
	} {
		var tools []Tool
		for _, p := range set {
			tools = append(tools, Tool{Function: ToolFunction{Name: p[0], Description: p[1]}})
		}
		lines = append(lines, map[string]any{"tools": set, "out": mcpPromptLine(tools)})
	}
	fx["prompt_line"] = lines
	timeouts := map[string]any{}
	for _, cmd := range []string{"npx", "/usr/local/bin/NPX", "uvx.exe", "npx.cmd", "node", "", "uvx-x", "C:/bin/uvx.EXE"} {
		cfg := MCPServerConfig{Command: cmd}
		timeouts[cmd] = map[string]any{"secs": int(mcpTimeoutFor(cfg).Seconds()),
			"err": mcpFriendlyErr(context.DeadlineExceeded, cfg).Error()}
	}
	fx["timeouts"] = timeouts
	fx["friendly_other"] = mcpFriendlyErr(errors.New("autre"), MCPServerConfig{Command: "npx"}).Error()
	var flat []any
	for _, raw := range []string{
		`{"content":[{"type":"text","text":"  un "},{"type":"text","text":"deux\n"}]}`,
		`{"content":[{"type":"image","data":"cG5n","mimeType":"image/png"},{"type":"audio","data":"d2F2","mimeType":"audio/wav"}]}`,
		`{"content":[{"type":"resource_link","uri":"file:///a.txt","name":"a.txt","mimeType":"text/plain","size":12,"title":"A <b>"}]}`,
		`{"content":[{"type":"resource","resource":{"uri":"file:///b","mimeType":"text/plain","text":"corps"}}]}`,
		`{"content":[],"structuredContent":{"b":[1,2],"a":"<x> & y"}}`,
		`{"content":[{"type":"text","text":"  "}],"structuredContent":{"k":1}}`,
		`{"content":[{"type":"text","text":"t"}],"structuredContent":{"k":1}}`,
		`{"content":[]}`,
		`{"content":[{"type":"text","text":"boum"}],"isError":true}`,
	} {
		var res mcpsdk.CallToolResult
		if err := json.Unmarshal([]byte(raw), &res); err != nil {
			t.Fatalf("%s : %v", raw, err)
		}
		flat = append(flat, map[string]any{"in": raw, "out": flattenMCPContent(&res)})
	}
	fx["flatten"] = flat

	// --- configuration ---
	var decoded []any
	for _, raw := range []string{
		`{"a":{"command":"npx","args":["-y","x"],"env":{"K":"v"}}}`,
		`{"b":{"url":"http://h/mcp","headers":{"Authorization":"Bearer t"},"enabled":false,"disabledTools":["t1"]}}`,
		`{"c":{"command":"x","enabled":null}}`,
		`null`, `{}`,
	} {
		var m map[string]MCPServerConfig
		_ = json.Unmarshal([]byte(raw), &m)
		out, _ := json.Marshal(m)
		tr := map[string]string{}
		for n, c := range m {
			tr[n] = c.Transport() + "|" + errString(c.Validate())
		}
		decoded = append(decoded, map[string]any{"in": raw, "out": string(out), "transports": tr})
	}
	fx["decode"] = decoded
	var muts []parityMCPMutation
	cfgOf := func(raw string) MCPServerConfig {
		var c MCPServerConfig
		_ = json.Unmarshal([]byte(raw), &c)
		return c
	}
	for _, m := range []parityMCPMutation{
		{Op: "set", Name: "  fs ", Cfg: `{"command":"npx","args":["-y","@x/fs","<w>"],"env":{"A":"&"}}`},
		{Op: "set", Name: "", Cfg: `{"command":"x"}`},
		{Op: "set", Name: "a__b", Cfg: `{"command":"x"}`},
		{Op: "set", Name: "both", Cfg: `{"command":"x","url":"http://u"}`},
		{Op: "set", Name: "none", Cfg: `{"enabled":true}`},
		{Op: "set", Name: "web", Cfg: `{"url":"http://u/mcp","headers":{"X":"1"},"enabled":false}`},
		{Op: "enable", Name: "web", On: true},
		{Op: "enable", Name: "nope", On: true},
		{Op: "tool", Name: "fs", Tool: "write", On: false},
		{Op: "tool", Name: "fs", Tool: "read", On: false},
		{Op: "tool", Name: "fs", Tool: "write", On: true},
		{Op: "tool", Name: "fs", Tool: "read", On: false},
		{Op: "tool", Name: "nope", Tool: "x", On: false},
		{Op: "delete", Name: "web"},
		{Op: "delete", Name: "web"},
		{Op: "delete", Name: " fs"},
	} {
		var err error
		switch m.Op {
		case "set":
			err = SetMCPServer(m.Name, cfgOf(m.Cfg))
		case "enable":
			err = SetMCPServerEnabled(m.Name, m.On)
		case "tool":
			err = SetMCPToolEnabled(m.Name, m.Tool, m.On)
		case "delete":
			err = DeleteMCPServer(m.Name)
		}
		m.Error = errString(err)
		m.State = getStr(bkState, "mcp")
		muts = append(muts, m)
	}
	fx["mutations"] = muts

	// --- un vrai serveur MCP (SDK Go), en Streamable HTTP ---
	srvMCP := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "parite", Version: "1"}, nil)
	schema := map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}
	text := func(s string) []mcpsdk.Content { return []mcpsdk.Content{&mcpsdk.TextContent{Text: s}} }
	tools := []struct {
		tool *mcpsdk.Tool
		res  func(args map[string]any) *mcpsdk.CallToolResult
	}{
		{&mcpsdk.Tool{Name: "echo", Description: "Répète le texte <tel quel>", InputSchema: schema},
			func(a map[string]any) *mcpsdk.CallToolResult {
				s, _ := a["text"].(string)
				return &mcpsdk.CallToolResult{Content: text(s)}
			}},
		{&mcpsdk.Tool{Name: "multi", Description: "Deux blocs", InputSchema: map[string]any{"type": "object"}},
			func(map[string]any) *mcpsdk.CallToolResult {
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "un"}, &mcpsdk.TextContent{Text: "deux"}}}
			}},
		{&mcpsdk.Tool{Name: "media", Description: "Image et son", InputSchema: map[string]any{"type": "object"}},
			func(map[string]any) *mcpsdk.CallToolResult {
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.ImageContent{Data: []byte("png"), MIMEType: "image/png"},
					&mcpsdk.AudioContent{Data: []byte("wav"), MIMEType: "audio/wav"}}}
			}},
		{&mcpsdk.Tool{Name: "link", Description: "Un lien", InputSchema: map[string]any{"type": "object"}},
			func(map[string]any) *mcpsdk.CallToolResult {
				sz := int64(12)
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.ResourceLink{URI: "file:///a.txt", Name: "a.txt", Title: "A", MIMEType: "text/plain", Size: &sz}}}
			}},
		{&mcpsdk.Tool{Name: "struct", Description: "Structuré", InputSchema: map[string]any{"type": "object"}},
			func(map[string]any) *mcpsdk.CallToolResult {
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{}, StructuredContent: map[string]any{"b": []any{1, 2}, "a": "<x> & y"}}
			}},
		{&mcpsdk.Tool{Name: "fail", Description: "Échoue", InputSchema: map[string]any{"type": "object"}},
			func(map[string]any) *mcpsdk.CallToolResult {
				return &mcpsdk.CallToolResult{Content: text("boum"), IsError: true}
			}},
		{&mcpsdk.Tool{Name: "empty", Description: "Rien", InputSchema: map[string]any{"type": "object"}},
			func(map[string]any) *mcpsdk.CallToolResult {
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{}}
			}},
		{&mcpsdk.Tool{Name: "big", Description: "Long", InputSchema: map[string]any{"type": "object"}},
			func(map[string]any) *mcpsdk.CallToolResult {
				return &mcpsdk.CallToolResult{Content: text(strings.Repeat("é", 40000))}
			}},
		{&mcpsdk.Tool{Name: "titled", Title: "Titre seul", InputSchema: map[string]any{"type": "object"}},
			func(map[string]any) *mcpsdk.CallToolResult { return &mcpsdk.CallToolResult{Content: text("ok")} }},
		{&mcpsdk.Tool{Name: "secret", Description: "Masqué", InputSchema: map[string]any{"type": "object"}},
			func(map[string]any) *mcpsdk.CallToolResult { return &mcpsdk.CallToolResult{Content: text("chut")} }},
		{&mcpsdk.Tool{Name: "dot.tool", Description: "Nom pointé", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
			func(map[string]any) *mcpsdk.CallToolResult { return &mcpsdk.CallToolResult{Content: text("point")} }},
	}
	for _, tl := range tools {
		res := tl.res
		srvMCP.AddTool(tl.tool, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			var args map[string]any
			_ = json.Unmarshal(req.Params.Arguments, &args)
			return res(args), nil
		})
	}
	ts := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srvMCP }, nil))
	// Les sessions du pool et du serveur tiennent des connexions ouvertes :
	// fermées avant le serveur, sans quoi Close attend indéfiniment.
	defer func() {
		mcpCloseAll()
		for ss := range srvMCP.Sessions() {
			_ = ss.Close()
		}
		ts.CloseClientConnections()
		ts.Close()
	}()

	// Ce que le serveur met sur le fil (le faux serveur Rust le resservira).
	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "t", Version: "1"}, nil)
	sess, err := cl.Connect(context.Background(), &mcpsdk.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	lt, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	wire := map[string]any{}
	b, _ := json.Marshal(lt)
	var ltv any
	_ = json.Unmarshal(b, &ltv)
	wire["tools/list"] = ltv
	calls := map[string]any{}
	callArgs := map[string]map[string]any{"echo": {"text": "salut <toi>"}}
	for _, tl := range tools {
		res, err := sess.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: tl.tool.Name, Arguments: callArgs[tl.tool.Name]})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(res)
		var v any
		_ = json.Unmarshal(b, &v)
		calls[tl.tool.Name] = v
	}
	wire["tools/call"] = calls
	_ = sess.Close()
	fx["wire"] = wire

	// Ce que le client de Loki en tire.
	_ = putStr(bkState, "mcp", "")
	if err := SetMCPServer("alpha", MCPServerConfig{URL: ts.URL, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	_ = SetMCPToolEnabled("alpha", "secret", false)
	_ = SetMCPServer("beta", MCPServerConfig{Command: "loki-mcp-absent-pour-test", Enabled: true})
	_ = SetMCPServer("gamma", MCPServerConfig{URL: ts.URL, Enabled: false})
	raw := getStr(bkState, "mcp")
	raw = strings.Replace(raw, `}}`, `},"delta":{"enabled":true}}`, 1)
	_ = putStr(bkState, "mcp", raw)
	fx["store"] = strings.ReplaceAll(getStr(bkState, "mcp"), ts.URL, "{{URL}}")
	st, err := MCPStatus()
	if err != nil {
		t.Fatal(err)
	}
	fx["status"] = st
	fx["tools"] = mcpTools()
	results := map[string]string{}
	for _, n := range []string{"echo", "multi", "media", "link", "struct", "fail", "empty", "big", "titled", "secret", "dot_tool", "nope"} {
		name := mcpExposedName("alpha", n)
		results[name] = mcpCall(name, callArgs[n])
	}
	results["mcp__zeta__x"] = mcpCall("mcp__zeta__x", nil)
	fx["calls"] = results
	fx["prompt_line_live"] = mcpPromptLine(mcpTools())

	got, err := parityJSON(fx)
	if err != nil {
		t.Fatal(err)
	}
	got = []byte(strings.ReplaceAll(string(got), ts.URL, "{{URL}}"))
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityMCPFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityMCPFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityMCPFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("le client MCP ne produit plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteMCP,\n"+
			"puis cargo test --test mcp dit si le port Rust suit.\n--- obtenu ---\n%s", parityMCPFile, got)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
