//go:build linux

package loki

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// parity_perf_test.go — journal de référence de l'anneau de télémétrie (lot 39
// de la migration Rust) : une suite de complétions rangées (chacune avec les
// empreintes des messages envoyés), ce que l'anneau en a tiré (numéro, perte
// de cache, ce qui s'est intercalé), la ligne [perf], le résumé de
// /api/perf/summary, puis les seuils d'alerte d'une série de presets. Rangé
// dans rust/loki-rs/tests/parity_perf/perf.json ; src/perfring/tests.rs le
// rejoue.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteAnneauPerf

const parityPerfFile = "../../rust/loki-rs/tests/parity_perf/perf.json"

type parityPerfStep struct {
	Rec    json.RawMessage `json:"rec"`
	Msgs   []string        `json:"msgs,omitempty"` // contenus des messages envoyés (rôle user)
	Prefix []string        `json:"prefix,omitempty"`
	Out    json.RawMessage `json:"out"`
	Line   string          `json:"line"`
	Alert  int             `json:"alert_1000"` // perfAlert au seuil 1000 : 1 vrai, 0 faux
}

type parityPerfAlert struct {
	Cfg  map[string]string `json:"cfg"`
	Env  string            `json:"env,omitempty"` // LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT
	Want int               `json:"want"`
}

type parityPerfJournal struct {
	Steps   []parityPerfStep  `json:"steps"`
	Summary json.RawMessage   `json:"summary"`
	Empty   json.RawMessage   `json:"empty"`
	Alerts  []parityPerfAlert `json:"alerts"`
}

func TestPariteAnneauPerf(t *testing.T) {
	testHome(t)
	freshPerf(t)
	at := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	var j parityPerfJournal
	rec := func(r perfRec, msgs ...string) {
		var prefix []uint64
		if msgs != nil {
			ms := make([]Message, len(msgs))
			for i, c := range msgs {
				ms[i] = Message{Role: "user", Content: c}
			}
			prefix = perfPrefix(ms)
		}
		in, _ := json.Marshal(r)
		r.At = at
		at = at.Add(time.Second)
		got := perfLog.record(r, prefix)
		out, _ := json.Marshal(got)
		st := parityPerfStep{Rec: in, Msgs: msgs, Out: out, Line: perfLine(got)}
		for _, p := range prefix {
			st.Prefix = append(st.Prefix, strconv.FormatUint(p, 10))
		}
		if perfAlert(got, 1000) {
			st.Alert = 1
		}
		j.Steps = append(j.Steps, st)
	}
	main := func(conv string, iter, total int, cached *int) perfRec {
		return perfRec{Kind: perfMain, Conv: conv, Iter: iter, Complete: true, Total: total, Cached: cached}
	}
	full := perfRec{Kind: perfMain, Conv: "c1", Iter: 0, Complete: true, Total: 4000, Cached: iptr(3000),
		New: 1000, PPms: 2000.4, PPtps: 499.9, Gen: 50, TGtps: 40.25, DraftN: iptr(40), DraftAcc: iptr(30), TTFTms: ptr64(2100)}
	rec(full, "a")
	rec(main("c1", 1, 4600, iptr(4000)), "a", "b", "c") // prolonge : rien de perdu
	rec(perfRec{Kind: perfSubagent, Conv: "c1", Complete: true, Total: 300, Cached: iptr(0), New: 300, PPms: 600, PPtps: 500}, "z")
	rec(perfRec{Kind: perfForeign})                                              // client externe : vu, pas mesuré
	rec(main("c1", 2, 5200, iptr(200)), "a", "b", "c", "d")                      // perte après le sous-agent et le client
	rec(main("c1", 3, 5200, nil), "a", "b", "c", "d", "e")                       // cache inconnu
	rec(main("c1", 4, 5300, iptr(10)), "x", "b")                                 // diverge
	rec(main("c2", 0, 20000, iptr(0)), "q")                                      // autre discussion, gros prompt sans cache
	rec(perfRec{Kind: perfMain, Conv: "c2", Iter: 1, Cached: iptr(0)}, "q", "r") // complétion coupée
	rec(main("c2", 1, 21000, iptr(19000)), "q", "r")                             // prolonge malgré la coupure
	rec(perfRec{Kind: perfCompact, Conv: "c2", Complete: true, Total: 9000, Cached: iptr(8800), New: 200, PPms: 100, PPtps: 2000, Gen: 300, TGtps: 30, TTFTms: ptr64(900)})
	rec(perfRec{Kind: perfBench, Complete: true, Total: 33000, New: 33000, PPms: 30000, PPtps: 1100, Gen: 64, TGtps: 20.5})
	rec(perfRec{Kind: perfVerify, Conv: "c1", Iter: 0, Complete: true, Total: 12000, Cached: iptr(0), New: 12000, PPms: 9000.5, PPtps: 1333.3, Gen: 2, TGtps: 9.75, DraftN: iptr(0)})
	rec(main("c1", 0, 6000, iptr(5900)), "a", "b", "c", "d", "e", "f") // prolonge la dernière terminée de c1

	sum, _ := json.Marshal(perfSummarize(perfLog.snapshot()))
	j.Summary = sum
	empty, _ := json.Marshal(perfSummarize(nil))
	j.Empty = empty

	for _, c := range []parityPerfAlert{
		{Cfg: map[string]string{}},
		{Cfg: map[string]string{"CKPT_MIN_STEP": "2048"}},
		{Cfg: map[string]string{"CKPT_MIN_STEP": "4096", "UBATCH": "1024"}},
		{Cfg: map[string]string{"CKPT_MIN_STEP": "beaucoup"}},
		{Cfg: map[string]string{"CKPT_MIN_STEP": " 300 ", "UBATCH": "100"}},
		{Cfg: map[string]string{"CKPT_MIN_STEP": "2048", "EXTRA_ARGS": "--checkpoint-min-step 8192 -ub 256"}},
		{Cfg: map[string]string{"CKPT_MIN_STEP": "2048", "UBATCH": "1024", "EXTRA_ARGS": "-ub=2048"}},
		{Cfg: map[string]string{"CKPT_MIN_STEP": "-5", "UBATCH": "0"}},
		{Cfg: map[string]string{"CKPT_MIN_STEP": "2048"}, Env: "3000"},
		{Cfg: map[string]string{"CKPT_MIN_STEP": "2048"}, Env: "rien"},
		{Cfg: map[string]string{"MODEL": "absent.gguf"}},
	} {
		t.Setenv("LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT", c.Env)
		c.Want = perfLostAlertAt(c.Cfg)
		j.Alerts = append(j.Alerts, c)
	}

	got, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityPerfFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityPerfFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityPerfFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("l'anneau de télémétrie ne produit plus le journal rangé dans %s.\n"+
			"LOKI_PARITY_UPDATE=1 pour le régénérer si le changement est voulu.", parityPerfFile)
	}
}
