package loki

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// parity_chatx_test.go — journal de référence de l'export, du mode Code et
// des critères de la discussion (lot 19 de la migration Rust) : l'export
// Markdown et JSON d'une conversation riche (pièces jointes, raisonnement en
// deltas, outils avec et sans sortie, compaction, images en base64, rappels
// de Loki, mise à jour de contexte) sous une vingtaine de jeux d'options lus
// dans la requête, puis les routes du mode et des critères avec la base après
// chaque appel. Rangé dans rust/loki-rs/tests/parity_chatx/chatx.json ;
// tests/chatx.rs le rejoue par la façade (conversation tenue par elle).
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteChatX

const parityChatXFile = "../../rust/loki-rs/tests/parity_chatx/chatx.json"

var (
	parityExportDateRe = regexp.MustCompile(`Exportée le \d{2}/\d{2}/\d{4} à \d{2}:\d{2}`)
	parityExportedAtRe = regexp.MustCompile(`"exported_at": "[^"]+"`)
)

// parityExportMask : la date de l'export et la version, propres à chaque
// exécution et à chaque binaire.
func parityExportMask(s string) string {
	s = parityExportDateRe.ReplaceAllString(s, "Exportée le {{DATE}}")
	s = parityExportedAtRe.ReplaceAllString(s, `"exported_at": "{{DATE}}"`)
	return strings.ReplaceAll(s, Version, "{{VERSION}}")
}

type parityChatXStep struct {
	Name   string `json:"name"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"body,omitempty"`
	Status int    `json:"status"`
	Out    any    `json:"out"`
	Mode   string `json:"mode"`     // chat/mode:<id> après l'appel
	Crit   any    `json:"criteria"` // chat/crit:<id> après l'appel
	Deltas []any  `json:"deltas"`   // événements ajoutés au fil
}

func TestPariteChatX(t *testing.T) {
	testHome(t)
	t.Setenv(facadeChatEnv, "")
	img := "data:image/png;base64," + strings.Repeat("QUJD", 600)
	msgs := []Message{
		{Role: "user", CtxUpd: true, Content: ctxUpdOpen + ` v="1">projet</context_update>` + "\n\nBonjour <b> & Cie"},
		{Role: "user", Content: ctxUpdOpen + ` v="2">tapé</context_update> par l'utilisateur`},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "c1", Type: "function",
			Function: ToolCallFunc{Name: "bash", Arguments: `{"cmd":"ls"}`}}}},
		{Role: "tool", Content: "a.txt\nb.txt", ToolCallID: "c1"},
		{Role: "assistant", Content: "Voilà.", ReasoningContent: "je liste", ReasoningModel: "m"},
		{Role: "user", Content: thinkNudgeFirst},
		{Role: "user", Content: []any{
			map[string]any{"type": "text", "text": "Regarde"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": img, "detail": "low"}},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x/y.png"}},
			"brut",
		}},
		{Role: "assistant", Content: "Une image."},
		{Role: "user", Content: "Dernier tour"},
		{Role: "assistant", Content: "Fin."},
	}
	ev := func(seq int, d map[string]any) LogEvent { return LogEvent{Seq: seq, TS: int64(1000 + seq), Delta: d} }
	log := []LogEvent{
		ev(1, map[string]any{"user": "Bonjour <b> & Cie", "files": []any{map[string]any{"name": "rapport.pdf", "size": 10}, map[string]any{"name": ""}, "x"}}),
		ev(2, map[string]any{"reasoning_content": "je "}),
		ev(3, map[string]any{"reasoning_content": "liste\n"}),
		ev(4, map[string]any{"tool_used": map[string]any{"name": "bash", "label": "ls *.txt\n[a]", "result": "a.txt\nb.txt\n\n", "done": true}}),
		ev(5, map[string]any{"tool_used": map[string]any{"name": "mem_read", "result": "   ", "done": true}}),
		ev(5, map[string]any{"tool_used": map[string]any{"name": "git_`log`", "label": "  `--oneline` [main]  ", "done": true}}),
		ev(5, map[string]any{"tool_used": map[string]any{"name": "en_cours", "label": "pas fini"}}),
		ev(6, map[string]any{"content": "Voi"}),
		ev(7, map[string]any{"content": "là.\n"}),
		ev(8, map[string]any{"stats": map[string]any{"tok_s": 12.5}}),
		ev(9, map[string]any{"compacted": true}),
		ev(10, map[string]any{"user": "", "files": []any{map[string]any{"name": "photo.png"}}}),
		ev(11, map[string]any{"content": "Une image."}),
		ev(12, map[string]any{"user": "Dernier tour"}),
		ev(13, map[string]any{"tool_used": map[string]any{"name": "web_open", "label": "https://ex.com/a_b*c", "result": "titre"}}),
		ev(14, map[string]any{"content": "Fin."}),
	}
	c := &Conversation{Messages: msgs, Log: log, Seq: 14, CtxUsed: 4321}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}

	type exportCase struct {
		Query    string `json:"query"`
		Opts     any    `json:"opts"`
		Body     string `json:"body"`
		Filename string `json:"filename_ext"`
	}
	var exports []exportCase
	for _, q := range []string{
		"", "format=json", "format=md", "format=JSON", "reasoning=0", "reasoning=true&tools=off",
		"tools=0", "tools=0&format=json", "results=0", "results=0&format=json", "reasoning=0&format=json",
		"turns=1", "turns=2&format=json", "turns=9", "turns=0", "turns=-2", "turns=abc", "turns=1&results=0&reasoning=0",
		"tools=on&results=yes", "format=json&format=md", "turns=3&format=json", "turns=4&format=json",
	} {
		vals, _ := url.ParseQuery(q)
		o := exportOptsFromQuery(vals)
		var body []byte
		var ext string
		if o.Format == "json" {
			body, err = c.ExportJSON(o)
			if err != nil {
				t.Fatal(err)
			}
			ext = "json"
		} else {
			body = []byte(c.ExportMarkdown(o))
			ext = "md"
		}
		exports = append(exports, exportCase{Query: q, Opts: map[string]any{"format": o.Format, "reasoning": o.Reasoning,
			"tools": o.Tools, "results": o.Results, "turns": o.Turns}, Body: parityExportMask(string(body)), Filename: ext})
	}
	empty := &Conversation{}
	emptyMD := parityExportMask(empty.ExportMarkdown(defaultExportOpts()))
	emptyJSON, _ := empty.ExportJSON(defaultExportOpts())

	// --- mode et critères, conversation tenue ici ---
	id := convEnsureActive()
	conv.mu.Lock()
	conv.Log = nil
	conv.mu.Unlock()
	run := func(st parityChatXStep) parityChatXStep {
		conv.mu.Lock()
		before := len(conv.Log)
		conv.mu.Unlock()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(st.Method, st.Path, strings.NewReader(st.Body))
		if st.Path == "/api/chat/mode" {
			handleChatMode(rec, req)
		} else {
			handleChatCriteria(rec, req)
		}
		st.Status = rec.Code
		_ = json.Unmarshal(rec.Body.Bytes(), &st.Out)
		st.Mode = getStr(bkChat, modeKey(id))
		var crit any
		if b := getBytes(bkChat, critKey(id)); len(b) > 0 {
			_ = json.Unmarshal(b, &crit)
		}
		st.Crit = crit
		conv.mu.Lock()
		for _, e := range conv.Log[before:] {
			d := map[string]any{}
			for k, v := range e.Delta {
				if k != "ts" {
					d[k] = v
				}
			}
			b, _ := json.Marshal(d)
			var x any
			_ = json.Unmarshal(b, &x)
			st.Deltas = append(st.Deltas, x)
		}
		conv.mu.Unlock()
		return st
	}
	long := make([]string, 0, 25)
	for i := 0; i < 25; i++ {
		long = append(long, `{"text":"c`+string(rune('a'+i))+`"}`)
	}
	var steps []parityChatXStep
	for _, st := range []parityChatXStep{
		{Name: "mode par défaut", Method: "GET", Path: "/api/chat/mode"},
		{Name: "mode code", Method: "POST", Path: "/api/chat/mode", Body: `{"mode":"code"}`},
		{Name: "mode relu", Method: "GET", Path: "/api/chat/mode"},
		{Name: "mode inconnu", Method: "POST", Path: "/api/chat/mode", Body: `{"mode":"Code"}`},
		{Name: "mode corps illisible", Method: "POST", Path: "/api/chat/mode", Body: `{"mode":`},
		{Name: "mode type faux", Method: "POST", Path: "/api/chat/mode", Body: `{"mode":true}`},
		{Name: "mode en PUT", Method: "PUT", Path: "/api/chat/mode", Body: `{"mode":"chat"}`},
		{Name: "critères en GET", Method: "GET", Path: "/api/chat/criteria"},
		{Name: "critères", Method: "POST", Path: "/api/chat/criteria",
			Body: `{"criteria":[{"text":"compile"},{"id":5,"text":"tests <ok> & verts","status":"passed","note":"vu"},{"text":"  "},{"id":0,"text":"doc","status":"bizarre"},{"id":2,"text":"lint","status":"failed"},{"text":"perf","status":"completed"}]}`},
		{Name: "mode relu avec critères", Method: "GET", Path: "/api/chat/mode"},
		{Name: "critères au-delà de vingt", Method: "POST", Path: "/api/chat/criteria",
			Body: `{"criteria":[` + strings.Join(long, ",") + `]}`},
		{Name: "critères vidés", Method: "POST", Path: "/api/chat/criteria", Body: `{"criteria":[]}`},
		{Name: "critères absents", Method: "POST", Path: "/api/chat/criteria", Body: `{}`},
		{Name: "critères null", Method: "POST", Path: "/api/chat/criteria", Body: `{"criteria":null}`},
		{Name: "critères corps vide", Method: "POST", Path: "/api/chat/criteria", Body: ``},
		{Name: "critères illisibles", Method: "POST", Path: "/api/chat/criteria", Body: `{"criteria":[`},
		{Name: "critère id texte", Method: "POST", Path: "/api/chat/criteria", Body: `{"criteria":[{"id":"2","text":"x"}]}`},
		{Name: "critère texte nombre", Method: "POST", Path: "/api/chat/criteria", Body: `{"criteria":[{"text":3}]}`},
		{Name: "critères pas une liste", Method: "POST", Path: "/api/chat/criteria", Body: `{"criteria":"a"}`},
		{Name: "critère pas un objet", Method: "POST", Path: "/api/chat/criteria", Body: `{"criteria":["a",{"text":"b"}]}`},
		{Name: "critère id décimal", Method: "POST", Path: "/api/chat/criteria", Body: `{"criteria":[{"id":1.5,"text":"x"}]}`},
		{Name: "critère clés en majuscules", Method: "POST", Path: "/api/chat/criteria", Body: `{"criteria":[{"ID":3,"TEXT":"maj","Status":"passed"},null]}`},
		{Name: "mode chat", Method: "POST", Path: "/api/chat/mode", Body: `{"mode":"chat"}`},
	} {
		steps = append(steps, run(st))
	}

	got, err := parityJSON(map[string]any{
		"conversation": json.RawMessage(raw), "exports": exports,
		"empty_md": emptyMD, "empty_json": parityExportMask(string(emptyJSON)), "steps": steps,
	})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityChatXFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityChatXFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityChatXFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("l'export, le mode ou les critères ne produisent plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteChatX,\n"+
			"puis cargo test --test chatx dit si le port Rust suit.\n--- obtenu ---\n%s", parityChatXFile, got)
	}
}
