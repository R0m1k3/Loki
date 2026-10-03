package loki

import (
	"reflect"
	"testing"
)

// helpFit : un moteur récent qui connaît --fit et sa marge.
const helpFit = helpRecent + `-fit,   --fit [on|off]                  whether to adjust unset arguments to fit in device memory
-fitt,  --fit-target MiB0,MiB1,...      target margin per device for --fit (default: 1024)
`

// TestFitTargetArgs : --fit-target n'est passé que validé, sur un moteur qui le
// connaît, avec un contexte fixé, et quand --fit tournera vraiment.
func TestFitTargetArgs(t *testing.T) {
	fit := serveSysInfo{Help: helpFit}
	cases := []struct {
		name      string
		cfg       map[string]string
		si        serveSysInfo
		want      []string
		wantNotes int
	}{
		{name: "clé absente : rien, sans un mot",
			cfg: map[string]string{}, si: fit},
		{name: "deux marges : passées, et on le dit",
			cfg: map[string]string{"FIT_TARGET": "1024,3072"}, si: fit,
			want: []string{"--fit-target", "1024,3072"}, wantNotes: 1},
		{name: "séparateur / : normalisé en virgules",
			cfg: map[string]string{"FIT_TARGET": "2048/1536"}, si: fit,
			want: []string{"--fit-target", "2048,1536"}, wantNotes: 1},
		{name: "une seule valeur : vaut pour toutes les cartes",
			cfg: map[string]string{"FIT_TARGET": " 2048 "}, si: fit,
			want: []string{"--fit-target", "2048"}, wantNotes: 1},
		{name: "marge sous le défaut : remontée à 1024",
			cfg: map[string]string{"FIT_TARGET": "256,3072"}, si: fit,
			want: []string{"--fit-target", "1024,3072"}, wantNotes: 1},
		{name: "valeur non numérique : jamais transmise (stoull ferait mourir le moteur)",
			cfg: map[string]string{"FIT_TARGET": "1G,2G"}, si: fit, wantNotes: 1},
		{name: "valeur négative : refusée",
			cfg: map[string]string{"FIT_TARGET": "-1024"}, si: fit, wantNotes: 1},
		{name: "séparateur doublé : refusé",
			cfg: map[string]string{"FIT_TARGET": "1024,,2048"}, si: fit, wantNotes: 1},
		{name: "trop de valeurs : refusé",
			cfg: map[string]string{"FIT_TARGET": "1,2,3,4,5,6,7,8,9"}, si: fit, wantNotes: 1},
		{name: "moteur sans --fit-target : rien, le drapeau le tuerait",
			cfg: map[string]string{"FIT_TARGET": "1024,3072"}, si: serveSysInfo{Help: helpRecent}, wantNotes: 1},
		{name: "-fitt déjà dans EXTRA_ARGS : l'utilisateur a tranché",
			cfg: map[string]string{"FIT_TARGET": "1024", "EXTRA_ARGS": "-fitt 2048"}, si: fit, wantNotes: 1},
		{name: "LLAMA_ARG_FIT_TARGET dans l'environnement : intouché",
			cfg: map[string]string{"FIT_TARGET": "1024"},
			si:  serveSysInfo{Help: helpFit, ArgEnv: map[string]string{"LLAMA_ARG_FIT_TARGET": "4096"}}, wantNotes: 1},
		{name: "CTX=0 : fit pourrait réduire le contexte, refusé",
			cfg: map[string]string{"FIT_TARGET": "1024,3072", "CTX": "0"}, si: fit, wantNotes: 1},
		{name: "-c 0 dans EXTRA_ARGS l'emporte sur CTX : refusé",
			cfg: map[string]string{"FIT_TARGET": "1024", "CTX": "65536", "EXTRA_ARGS": "-c 0"}, si: fit, wantNotes: 1},
		{name: "CTX absent : Loki passe -c 32768, accepté",
			cfg: map[string]string{"FIT_TARGET": "1024"}, si: fit,
			want: []string{"--fit-target", "1024"}, wantNotes: 1},
		{name: "NGL=999 : devient -ngl auto, fit tourne",
			cfg: map[string]string{"FIT_TARGET": "1024", "NGL": "999"}, si: fit,
			want: []string{"--fit-target", "1024"}, wantNotes: 1},
		{name: "NGL chiffré : fit abandonne, rien",
			cfg: map[string]string{"FIT_TARGET": "1024", "NGL": "40"}, si: fit, wantNotes: 1},
		{name: "NGL=all : fit abandonne, rien",
			cfg: map[string]string{"FIT_TARGET": "1024", "NGL": "all"}, si: fit, wantNotes: 1},
		{name: "-ngl auto dans EXTRA_ARGS : fit tourne",
			cfg: map[string]string{"FIT_TARGET": "1024", "NGL": "40", "EXTRA_ARGS": "-ngl auto"}, si: fit,
			want: []string{"--fit-target", "1024"}, wantNotes: 1},
		{name: "NGL=auto et LLAMA_ARG_N_GPU_LAYERS=30 : rien",
			cfg: map[string]string{"FIT_TARGET": "1024", "NGL": "auto"},
			si:  serveSysInfo{Help: helpFit, ArgEnv: map[string]string{"LLAMA_ARG_N_GPU_LAYERS": "30"}}, wantNotes: 1},
		{name: "--tensor-split : rien",
			cfg: map[string]string{"FIT_TARGET": "1024", "EXTRA_ARGS": "--tensor-split 0.965,0.035"}, si: fit, wantNotes: 1},
		{name: "preset MoE (-ot, --n-cpu-moe) : rien",
			cfg: map[string]string{"FIT_TARGET": "1024", "EXTRA_ARGS": `-ot "per_layer_token_embd=CPU" --n-cpu-moe 40`},
			si:  fit, wantNotes: 1},
		{name: "--cpu-moe : rien",
			cfg: map[string]string{"FIT_TARGET": "1024", "EXTRA_ARGS": "-cmoe"}, si: fit, wantNotes: 1},
		{name: "--fit off : rien",
			cfg: map[string]string{"FIT_TARGET": "1024", "EXTRA_ARGS": "--fit off"}, si: fit, wantNotes: 1},
		{name: "LLAMA_ARG_FIT=off : rien",
			cfg: map[string]string{"FIT_TARGET": "1024"},
			si:  serveSysInfo{Help: helpFit, ArgEnv: map[string]string{"LLAMA_ARG_FIT": "off"}}, wantNotes: 1},
		{name: "-sm row : rien",
			cfg: map[string]string{"FIT_TARGET": "1024", "EXTRA_ARGS": "-sm row"}, si: fit, wantNotes: 1},
		{name: "--device choisi : le placement reste à fit, accepté",
			cfg: map[string]string{"FIT_TARGET": "1024,3072", "EXTRA_ARGS": "--device CUDA1,CUDA0"}, si: fit,
			want: []string{"--fit-target", "1024,3072"}, wantNotes: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, notes := fitTargetArgs(c.cfg, splitArgs(c.cfg["EXTRA_ARGS"]), c.si)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("args = %q, attendu %q", got, c.want)
			}
			if len(notes) != c.wantNotes {
				t.Fatalf("notes = %q, en attendait %d", notes, c.wantNotes)
			}
		})
	}
}

// TestGraphOptEnv : GGML_CUDA_GRAPH_OPT=1 seulement sur demande, jamais
// par-dessus l'environnement, jamais quand Q/K/V pourraient changer de carte.
func TestGraphOptEnv(t *testing.T) {
	cases := []struct {
		name      string
		cfg       map[string]string
		si        serveSysInfo
		want      string
		wantNotes int
	}{
		{name: "clé absente : rien", cfg: map[string]string{}},
		{name: "off : rien", cfg: map[string]string{"CUDA_GRAPH_OPT": "off"}},
		{name: "on : posé, et on le dit", cfg: map[string]string{"CUDA_GRAPH_OPT": "on"}, want: "1", wantNotes: 1},
		{name: "ON (casse libre) : posé", cfg: map[string]string{"CUDA_GRAPH_OPT": "ON"}, want: "1", wantNotes: 1},
		{name: "valeur inconnue : ignorée, dite", cfg: map[string]string{"CUDA_GRAPH_OPT": "2"}, wantNotes: 1},
		{name: "variable déjà posée (même à 0) : intouchée",
			cfg: map[string]string{"CUDA_GRAPH_OPT": "on"},
			si:  serveSysInfo{UserEnv: map[string]string{"GGML_CUDA_GRAPH_OPT": "0"}}},
		{name: "découpe par couches + --tensor-split : couches entières, posé",
			cfg:  map[string]string{"CUDA_GRAPH_OPT": "on", "EXTRA_ARGS": "--device CUDA1,CUDA0 -ts 0.6,0.4"},
			want: "1", wantNotes: 1},
		{name: "-sm row : refusé",
			cfg: map[string]string{"CUDA_GRAPH_OPT": "on", "EXTRA_ARGS": "-sm row"}, wantNotes: 1},
		{name: "LLAMA_ARG_SPLIT_MODE=tensor : refusé",
			cfg: map[string]string{"CUDA_GRAPH_OPT": "on"},
			si:  serveSysInfo{ArgEnv: map[string]string{"LLAMA_ARG_SPLIT_MODE": "tensor"}}, wantNotes: 1},
		{name: "-ot sur l'attention : refusé",
			cfg: map[string]string{"CUDA_GRAPH_OPT": "on", "EXTRA_ARGS": `-ot "blk\.1\.attn_k.*=CUDA1"`}, wantNotes: 1},
		{name: "-ot des experts seulement : posé",
			cfg:  map[string]string{"CUDA_GRAPH_OPT": "on", "EXTRA_ARGS": `-ot "ffn_.*_exps=CPU" --n-cpu-moe 40`},
			want: "1", wantNotes: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, notes := graphOptEnv(c.cfg, splitArgs(c.cfg["EXTRA_ARGS"]), c.si)
			if got != c.want {
				t.Fatalf("valeur = %q, attendu %q", got, c.want)
			}
			if len(notes) != c.wantNotes {
				t.Fatalf("notes = %q, en attendait %d", notes, c.wantNotes)
			}
		})
	}
}

// TestOpOffloadMinBatchEnv : la variable n'existe que si la clé est posée et
// valide, et ne remplace jamais celle de l'environnement.
func TestOpOffloadMinBatchEnv(t *testing.T) {
	cases := []struct {
		name      string
		cfg       map[string]string
		si        serveSysInfo
		want      string
		wantNotes int
	}{
		{name: "clé absente : rien", cfg: map[string]string{}},
		{name: "128 : posé", cfg: map[string]string{"OP_OFFLOAD_MIN_BATCH": "128"}, want: "128", wantNotes: 1},
		{name: "espaces autour : posé", cfg: map[string]string{"OP_OFFLOAD_MIN_BATCH": " 64 "}, want: "64", wantNotes: 1},
		{name: "0 : ignoré, dit", cfg: map[string]string{"OP_OFFLOAD_MIN_BATCH": "0"}, wantNotes: 1},
		{name: "texte : ignoré, dit", cfg: map[string]string{"OP_OFFLOAD_MIN_BATCH": "beaucoup"}, wantNotes: 1},
		{name: "variable déjà posée : intouchée",
			cfg: map[string]string{"OP_OFFLOAD_MIN_BATCH": "128"},
			si:  serveSysInfo{UserEnv: map[string]string{"GGML_OP_OFFLOAD_MIN_BATCH": "32"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, notes := opOffloadMinBatchEnv(c.cfg, c.si)
			if got != c.want {
				t.Fatalf("valeur = %q, attendu %q", got, c.want)
			}
			if len(notes) != c.wantNotes {
				t.Fatalf("notes = %q, en attendait %d", notes, c.wantNotes)
			}
		})
	}
}

// Les trois clés absentes, la ligne de commande et l'environnement ne bougent
// pas ; posées, --fit-target arrive avant EXTRA_ARGS et les variables vont
// dans env, nulle part ailleurs.
func TestBuildServeArgsExpertToggles(t *testing.T) {
	const bin, model = "/opt/llama/llama-server", "/models/Qwen3.6-27B-Q4_K_M.gguf"
	si := serveSysInfo{Help: helpFit, Model: model}
	cfg := map[string]string{"CTX": "65536", "EXTRA_ARGS": "--device CUDA0"}
	base, baseEnv, baseNotes := buildServeArgs(cfg, splitArgs(cfg["EXTRA_ARGS"]), bin, si)
	if len(baseEnv) != 0 || len(baseNotes) != 0 {
		t.Fatalf("sans les clés : env = %v, notes = %q", baseEnv, baseNotes)
	}
	on := map[string]string{"CTX": "65536", "EXTRA_ARGS": "--device CUDA0",
		"FIT_TARGET": "1024,3072", "OP_OFFLOAD_MIN_BATCH": "128", "CUDA_GRAPH_OPT": "on"}
	args, env, notes := buildServeArgs(on, splitArgs(on["EXTRA_ARGS"]), bin, si)
	n := len(base)
	want := append(append(append([]string{}, base[:n-2]...), "--fit-target", "1024,3072"), base[n-2:]...)
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args\n got %q\nwant %q", args, want)
	}
	wantEnv := map[string]string{"GGML_CUDA_GRAPH_OPT": "1", "GGML_OP_OFFLOAD_MIN_BATCH": "128"}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Fatalf("env = %v, want %v", env, wantEnv)
	}
	if len(notes) != 3 {
		t.Fatalf("notes = %q", notes)
	}
}
