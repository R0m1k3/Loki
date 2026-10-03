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
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestEffectiveKVTypes(t *testing.T) {
	const ea, kv = "défini par EXTRA_ARGS", "KV_TYPE"
	const env = "défini par LLAMA_ARG_CACHE_TYPE_K/V"
	for _, c := range []struct {
		name   string
		cfg    map[string]string
		extra  string
		argEnv map[string]string
		k, v   string
		src    string
	}{
		{"rien : défaut moteur", map[string]string{}, "", nil, "", "", ""},
		{"KV_TYPE seul", map[string]string{"KV_TYPE": "q8_0"}, "", nil, "q8_0", "q8_0", kv},
		{"EXTRA_ARGS l'emporte sur KV_TYPE", map[string]string{"KV_TYPE": "f16"},
			"--cache-type-k q8_0 --cache-type-v q8_0", nil, "q8_0", "q8_0", ea},
		{"formes courtes, la dernière gagne", map[string]string{},
			"-ctk q4_0 -ctv q4_0 -ctk q8_0", nil, "q8_0", "q4_0", ea},
		{"forme --flag=valeur", map[string]string{}, "--cache-type-v=q8_0", nil, "", "q8_0", ea},
		{"K seul dans EXTRA_ARGS, V du preset", map[string]string{"KV_TYPE_V": "q4_0"},
			"-ctk q8_0", nil, "q8_0", "q4_0", ea},
		// Les variables passent AVANT la ligne de commande : seules, elles
		// décident ; un -ctk venu de KV_TYPE les écrase.
		{"variables seules", map[string]string{}, "",
			map[string]string{"LLAMA_ARG_CACHE_TYPE_K": "q8_0", "LLAMA_ARG_CACHE_TYPE_V": "q8_0"}, "q8_0", "q8_0", env},
		{"KV_TYPE_K écrase la variable K, la variable V reste", map[string]string{"KV_TYPE_K": "f16"}, "",
			map[string]string{"LLAMA_ARG_CACHE_TYPE_K": "q4_0", "LLAMA_ARG_CACHE_TYPE_V": "q8_0"}, "f16", "q8_0", kv},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, v, src := effectiveKVTypes(c.cfg, splitArgs(c.extra), c.argEnv)
			if k != c.k || v != c.v || src != c.src {
				t.Fatalf("got %q/%q src=%q, want %q/%q src=%q", k, v, src, c.k, c.v, c.src)
			}
		})
	}
}

func TestKVFidelityNote(t *testing.T) {
	for _, c := range []struct {
		name   string
		k, v   string
		src    string
		extra  string
		argEnv map[string]string
		want   []string // fragments attendus ; nil = aucune note
		fit    bool     // la note doit parler de --fit (placement figé)
	}{
		{"défaut moteur : rien à dire", "", "", "", "", nil, nil, false},
		{"f16 explicite : rien à dire", "f16", "f16", "KV_TYPE", "", nil, nil, false},
		{"f32 ne perd rien", "f32", "f32", "KV_TYPE", "", nil, nil, false},
		{"q8_0 : léger écart", "q8_0", "q8_0", "KV_TYPE", "", nil,
			[]string{"q8_0/q8_0", "KV_TYPE", "modifie légèrement"}, false},
		{"q4_0 : perte mesurable", "q4_0", "q4_0", "KV_TYPE", "", nil, []string{"perte mesurable"}, false},
		{"mixte : le pire des deux", "q8_0", "q4_0", "KV_TYPE", "", nil, []string{"perte mesurable"}, false},
		{"bf16 : numérique différente", "bf16", "bf16", "KV_TYPE", "", nil, []string{"mantisse"}, false},
		{"V seul quantifié", "", "q8_0", "KV_TYPE", "", nil, []string{"f16/q8_0"}, false},
		{"type inconnu : supposé altérer", "q3_k", "q3_k", "KV_TYPE", "", nil, []string{"modifie les sorties"}, false},
		{"venu d'EXTRA_ARGS, placement figé : --fit ne compense pas", "q8_0", "q8_0", "défini par EXTRA_ARGS",
			"-ot per_layer_token_embd.weight=CPU --n-cpu-moe 40 --cache-type-k q8_0", nil,
			[]string{"défini par EXTRA_ARGS", "relever --n-cpu-moe"}, true},
		// --n-cpu-moe 0 ne place rien : --fit tourne, rien à en dire.
		{"--n-cpu-moe 0 : placement libre", "q8_0", "q8_0", "défini par EXTRA_ARGS",
			"--n-cpu-moe 0 -ctk q8_0 -ctv q8_0", nil, []string{"q8_0/q8_0"}, false},
		{"experts sur CPU par variable : placement figé aussi", "q8_0", "q8_0", "KV_TYPE", "",
			map[string]string{"LLAMA_ARG_N_CPU_MOE": "30"}, []string{"relever --n-cpu-moe"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := kvFidelityNote(c.k, c.v, c.src, splitArgs(c.extra), c.argEnv)
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
			if strings.Contains(got, "--fit") != c.fit {
				t.Fatalf("note %q : mention de --fit attendue = %v", got, c.fit)
			}
		})
	}
}

func TestLossyCacheNotes(t *testing.T) {
	// Aides des deux générations de llama-server : l'ancienne ne connaît que
	// --no-context-shift et glisse par défaut, la récente ne glisse pas.
	const helpShiftOld = "--no-context-shift   disables context shift on infinite text generation (default: disabled)"
	const helpShiftNew = "--context-shift, --no-context-shift   whether to use context shift on infinite text generation (default: disabled)"
	envOf := func(kv ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for _, c := range []struct {
		name  string
		extra string
		si    serveSysInfo
		want  []string // un fragment par note attendue, dans l'ordre
	}{
		{"rien", "", serveSysInfo{}, nil},
		{"--context-shift", "--context-shift", serveSysInfo{}, []string{"--context-shift (EXTRA_ARGS)"}},
		{"--no-context-shift ne déclenche rien", "--no-context-shift", serveSysInfo{}, nil},
		{"la dernière occurrence décide", "--context-shift --no-context-shift", serveSysInfo{}, nil},
		{"--cache-reuse 256", "--cache-reuse 256", serveSysInfo{}, []string{"--cache-reuse 256 (EXTRA_ARGS)"}},
		{"--cache-reuse=256", "--cache-reuse=256", serveSysInfo{}, []string{"--cache-reuse 256"}},
		{"--cache-reuse 0 : désactivé", "--cache-reuse 0", serveSysInfo{}, nil},
		{"--swa-full est sans perte", "--swa-full", serveSysInfo{}, nil},
		{"les deux", "--context-shift --cache-reuse 64", serveSysInfo{}, []string{"jette", "--cache-reuse 64"}},
		{"vision (MMPROJ) : llama.cpp les ignore, on le dit", "--context-shift --cache-reuse 64",
			serveSysInfo{MMProj: "/m/mmproj.gguf"}, []string{"l'ignore", "l'ignore"}},
		{"vision par --mmproj d'EXTRA_ARGS : pareil", "--mmproj mmproj-F16.gguf --cache-reuse 64",
			serveSysInfo{}, []string{"l'ignore"}},
		// Les variables du moteur comptent quand la ligne de commande se tait.
		{"LLAMA_ARG_CACHE_REUSE", "", serveSysInfo{ArgEnv: envOf("LLAMA_ARG_CACHE_REUSE", "128")},
			[]string{"--cache-reuse 128 (LLAMA_ARG_CACHE_REUSE)"}},
		{"--cache-reuse 0 d'EXTRA_ARGS écrase la variable", "--cache-reuse 0",
			serveSysInfo{ArgEnv: envOf("LLAMA_ARG_CACHE_REUSE", "128")}, nil},
		{"LLAMA_ARG_CONTEXT_SHIFT=1", "", serveSysInfo{Help: helpShiftNew, ArgEnv: envOf("LLAMA_ARG_CONTEXT_SHIFT", "1")},
			[]string{"(LLAMA_ARG_CONTEXT_SHIFT)"}},
		{"moteur récent sans drapeau : rien", "", serveSysInfo{Help: helpShiftNew}, nil},
		// Défaut d'un moteur ancien : il jette des jetons sans qu'on ait rien écrit.
		{"moteur ancien : glisse par défaut, on le dit", "", serveSysInfo{Help: helpShiftOld},
			[]string{"glisse le contexte par défaut"}},
		{"moteur ancien, --no-context-shift : rien", "--no-context-shift", serveSysInfo{Help: helpShiftOld}, nil},
		{"moteur ancien, LLAMA_ARG_NO_CONTEXT_SHIFT=1 : rien", "",
			serveSysInfo{Help: helpShiftOld, ArgEnv: envOf("LLAMA_ARG_NO_CONTEXT_SHIFT", "1")}, nil},
		{"moteur inconnu (aide vide) : on ne suppose rien", "", serveSysInfo{}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := lossyCacheNotes(splitArgs(c.extra), c.si)
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
		banned := func(s string) bool { return s == "cache_prompt" || s == "n_cache_reuse" }
		ast.Inspect(file, func(n ast.Node) bool {
			// Un champ de structure sérialisé (étiquette json:"cache_prompt")
			// enverrait la clé aussi sûrement qu'une map : on lit les étiquettes.
			if f, ok := n.(*ast.Field); ok && f.Tag != nil {
				if tag, err := strconv.Unquote(f.Tag.Value); err == nil {
					if name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ","); banned(name) {
						t.Errorf("%s : champ %q hors du benchmark", fset.Position(f.Pos()), name)
					}
				}
				return true
			}
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if s, err := strconv.Unquote(lit.Value); err == nil && banned(s) {
				t.Errorf("%s : clé %q hors du benchmark", fset.Position(lit.Pos()), s)
			}
			return true
		})
	}
}
