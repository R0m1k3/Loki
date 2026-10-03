package loki

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestEffectiveKVTypes(t *testing.T) {
	for _, c := range []struct {
		name      string
		cfg       map[string]string
		extra     string
		k, v      string
		fromExtra bool
	}{
		{"rien : défaut moteur", map[string]string{}, "", "", "", false},
		{"KV_TYPE seul", map[string]string{"KV_TYPE": "q8_0"}, "", "q8_0", "q8_0", false},
		{"EXTRA_ARGS l'emporte sur KV_TYPE", map[string]string{"KV_TYPE": "f16"},
			"--cache-type-k q8_0 --cache-type-v q8_0", "q8_0", "q8_0", true},
		{"formes courtes, la dernière gagne", map[string]string{},
			"-ctk q4_0 -ctv q4_0 -ctk q8_0", "q8_0", "q4_0", true},
		{"forme --flag=valeur", map[string]string{}, "--cache-type-v=q8_0", "", "q8_0", true},
		{"K seul dans EXTRA_ARGS, V du preset", map[string]string{"KV_TYPE_V": "q4_0"},
			"-ctk q8_0", "q8_0", "q4_0", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, v, fe := effectiveKVTypes(c.cfg, splitArgs(c.extra))
			if k != c.k || v != c.v || fe != c.fromExtra {
				t.Fatalf("got %q/%q extra=%v, want %q/%q extra=%v", k, v, fe, c.k, c.v, c.fromExtra)
			}
		})
	}
}

func TestKVFidelityNote(t *testing.T) {
	for _, c := range []struct {
		name      string
		k, v      string
		fromExtra bool
		extra     string
		want      []string // fragments attendus ; nil = aucune note
	}{
		{"défaut moteur : rien à dire", "", "", false, "", nil},
		{"f16 explicite : rien à dire", "f16", "f16", false, "", nil},
		{"f32 ne perd rien", "f32", "f32", false, "", nil},
		{"q8_0 : léger écart", "q8_0", "q8_0", false, "", []string{"q8_0/q8_0", "KV_TYPE", "modifie légèrement"}},
		{"q4_0 : perte mesurable", "q4_0", "q4_0", false, "", []string{"perte mesurable"}},
		{"mixte : le pire des deux", "q8_0", "q4_0", false, "", []string{"perte mesurable"}},
		{"bf16 : numérique différente", "bf16", "bf16", false, "", []string{"mantisse"}},
		{"V seul quantifié", "", "q8_0", false, "", []string{"f16/q8_0"}},
		{"type inconnu : supposé altérer", "q3_k", "q3_k", false, "", []string{"modifie les sorties"}},
		{"venu d'EXTRA_ARGS, placement figé : --fit ne compense pas", "q8_0", "q8_0", true,
			"-ot per_layer_token_embd.weight=CPU --n-cpu-moe 40 --cache-type-k q8_0",
			[]string{"défini par EXTRA_ARGS", "relever --n-cpu-moe"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := kvFidelityNote(c.k, c.v, c.fromExtra, splitArgs(c.extra))
			if c.want == nil {
				if got != "" {
					t.Fatalf("note inattendue : %q", got)
				}
				return
			}
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Fatalf("note %q sans %q", got, w)
				}
			}
			if !strings.Contains(c.extra, "-ot") && strings.Contains(got, "--fit") {
				t.Fatalf("placement libre, mais la note parle de --fit : %q", got)
			}
		})
	}
}

func TestLossyCacheNotes(t *testing.T) {
	for _, c := range []struct {
		name   string
		extra  string
		mmproj bool
		want   []string // un fragment par note attendue, dans l'ordre
	}{
		{"rien", "", false, nil},
		{"--context-shift", "--context-shift", false, []string{"jette des jetons"}},
		{"--no-context-shift ne déclenche rien", "--no-context-shift", false, nil},
		{"la dernière occurrence décide", "--context-shift --no-context-shift", false, nil},
		{"--cache-reuse 256", "--cache-reuse 256", false, []string{"--cache-reuse 256"}},
		{"--cache-reuse=256", "--cache-reuse=256", false, []string{"--cache-reuse 256"}},
		{"--cache-reuse 0 : désactivé", "--cache-reuse 0", false, nil},
		{"--swa-full est sans perte", "--swa-full", false, nil},
		{"les deux", "--context-shift --cache-reuse 64", false, []string{"jette", "--cache-reuse 64"}},
		{"vision : llama.cpp les ignore, on le dit", "--context-shift --cache-reuse 64", true,
			[]string{"l'ignore", "l'ignore"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := lossyCacheNotes(splitArgs(c.extra), c.mmproj)
			if len(got) != len(c.want) {
				t.Fatalf("notes = %q, en attendait %d", got, len(c.want))
			}
			for i, w := range c.want {
				if !strings.Contains(got[i], w) {
					t.Fatalf("note %q sans %q", got[i], w)
				}
			}
		})
	}
}

// Ce que buildServeArgs fait des garde-fous : JAMAIS de drapeau ajouté ou
// retiré, seulement des notes. Sans KV_TYPE, aucun -ctk/-ctv : le moteur garde
// son f16.
func TestBuildServeArgsFidelite(t *testing.T) {
	const bin = "/opt/llama/llama-server"
	si := serveSysInfo{Help: helpRecent, Model: "/models/m.gguf"}
	for _, c := range []struct {
		name  string
		cfg   map[string]string
		notes []string // fragments attendus parmi les notes ; nil = aucune note de fidélité
	}{
		{"KV_TYPE absent : ni -ctk ni -ctv, rien à dire", map[string]string{"NGL": "28"}, nil},
		{"--swa-full : sans perte, silence", map[string]string{"NGL": "28", "EXTRA_ARGS": "--swa-full"}, nil},
		{"cache quantifié par EXTRA_ARGS : vu", map[string]string{"NGL": "28",
			"EXTRA_ARGS": "--cache-type-k q8_0 --cache-type-v q8_0"}, []string{"défini par EXTRA_ARGS"}},
		{"--cache-reuse 256 : averti", map[string]string{"NGL": "28", "EXTRA_ARGS": "--cache-reuse 256"},
			[]string{"--cache-reuse 256"}},
		{"--context-shift : averti", map[string]string{"NGL": "28", "EXTRA_ARGS": "--context-shift"},
			[]string{"--context-shift"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			extra := splitArgs(c.cfg["EXTRA_ARGS"])
			args, _, notes := buildServeArgs(c.cfg, extra, bin, si)
			if c.cfg["KV_TYPE"] == "" && !hasAnyFlag(extra, "-ctk", "-ctv", "--cache-type-k", "--cache-type-v") &&
				hasAnyFlag(args, "-ctk", "-ctv", "--cache-type-k", "--cache-type-v") {
				t.Fatalf("Loki a posé un type de cache de lui-même : %q", args)
			}
			// La ligne de commande se termine exactement par EXTRA_ARGS, intouché.
			if tail := args[len(args)-len(extra):]; strings.Join(tail, " ") != strings.Join(extra, " ") {
				t.Fatalf("EXTRA_ARGS modifié : %q", tail)
			}
			all := strings.Join(notes, "\n")
			if c.notes == nil {
				if strings.Contains(all, "cache KV") || strings.Contains(all, "avertissement") {
					t.Fatalf("note de fidélité inattendue : %q", notes)
				}
				return
			}
			for _, w := range c.notes {
				if !strings.Contains(all, w) {
					t.Fatalf("notes %q sans %q", notes, w)
				}
			}
		})
	}
}

// Aucune requête de Loki vers le moteur ne coupe le cache de préfixe
// (cache_prompt:false) ni n'active la réutilisation approximative
// (n_cache_reuse). On vérifie les corps RÉELLEMENT envoyés par le chat et par
// la compaction, contre un faux llama-server local.
func TestRequetesSansCacheApproximatif(t *testing.T) {
	testHome(t)
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(sseChunk("bonjour")))
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
			return
		}
		sendJSON(w, 200, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "- résumé"}}}})
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}
	if _, err := runChat(context.Background(), []Message{{Role: "user", Content: "salut"}}, 0.7, Caps{},
		func(StreamEvent) bool { return true }); err != nil {
		t.Fatalf("runChat : %v", err)
	}
	if _, err := summarizeTranscript(context.Background(), "user: bonjour\nassistant: salut"); err != nil {
		t.Fatalf("compaction : %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 2 {
		t.Fatalf("%d requête(s) reçue(s), attendu chat + compaction", len(bodies))
	}
	for _, b := range bodies {
		if v, ok := b["cache_prompt"]; ok && v != true {
			t.Errorf("cache_prompt=%v envoyé au moteur : le préfixe serait recalculé ou faussé", v)
		}
		if _, ok := b["n_cache_reuse"]; ok {
			t.Errorf("n_cache_reuse envoyé au moteur : réutilisation approximative du cache")
		}
	}
}

// Filet sur tout le paquet : les clés cache_prompt et n_cache_reuse n'existent
// dans le code QUE pour le benchmark, qui mesure le prefill et doit donc couper
// le cache. Lecture de l'arbre syntaxique (pas du texte) : un commentaire qui
// les mentionne ne compte pas, un nouveau fichier est couvert d'office.
func TestCleCacheReserveeAuBench(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"llm_bench.go": true}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || allowed[f] {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if s, err := strconv.Unquote(lit.Value); err == nil && (s == "cache_prompt" || s == "n_cache_reuse") {
				t.Errorf("%s : clé %q hors du benchmark", fset.Position(lit.Pos()), s)
			}
			return true
		})
	}
}
