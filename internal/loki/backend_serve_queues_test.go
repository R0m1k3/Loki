package loki

import (
	"reflect"
	"testing"
)

// TestLaunchQueuesEnv : CUDA_SCALE_LAUNCH_QUEUES n'est posé que là où le
// pipeline entre cartes peut exister, et jamais par-dessus un choix de
// l'utilisateur.
func TestLaunchQueuesEnv(t *testing.T) {
	two := serveSysInfo{GPUs: 2}
	cases := []struct {
		name      string
		cfg       map[string]string
		si        serveSysInfo
		want      string
		wantNotes int
	}{
		{name: "2 GPU, dense tout sur GPU : 4x, et on le dit",
			cfg: map[string]string{"NGL": "999"}, si: two, want: "4x", wantNotes: 1},
		{name: "NGL=all ou auto : pipeline possible",
			cfg: map[string]string{"NGL": "all"}, si: two, want: "4x", wantNotes: 1},
		{name: "1 GPU : rien",
			cfg: map[string]string{"NGL": "999"}, si: serveSysInfo{GPUs: 1}},
		{name: "nombre de GPU inconnu : rien",
			cfg: map[string]string{}},
		{name: "variable déjà posée par l'utilisateur : intouchée",
			cfg: map[string]string{"CUDA_LAUNCH_QUEUES": "2x"}, si: serveSysInfo{GPUs: 2, LaunchQueues: "1x"}},
		{name: "CUDA_LAUNCH_QUEUES=off : rien",
			cfg: map[string]string{"CUDA_LAUNCH_QUEUES": "off"}, si: two},
		{name: "valeur explicite valide : posée telle quelle, même sur 1 GPU",
			cfg: map[string]string{"CUDA_LAUNCH_QUEUES": "2X"}, si: serveSysInfo{GPUs: 1}, want: "2x", wantNotes: 1},
		{name: "valeur invalide : ignorée, dite, puis automatique",
			cfg: map[string]string{"CUDA_LAUNCH_QUEUES": "8x"}, si: two, want: "4x", wantNotes: 2},
		{name: "valeur invalide sur 1 GPU : seulement la note",
			cfg: map[string]string{"CUDA_LAUNCH_QUEUES": "beaucoup"}, si: serveSysInfo{GPUs: 1}, wantNotes: 1},
		{name: "preset MoE (-ot, --n-cpu-moe) sur 2 GPU : rien",
			cfg: map[string]string{"EXTRA_ARGS": `-ot "per_layer_token_embd=CPU" --n-cpu-moe 40`}, si: two},
		{name: "--n-cpu-moe seul : rien",
			cfg: map[string]string{"EXTRA_ARGS": "--n-cpu-moe=12"}, si: two},
		{name: "--n-cpu-moe 0 : aucun expert sur CPU, pipeline possible",
			cfg: map[string]string{"EXTRA_ARGS": "--n-cpu-moe 0"}, si: two, want: "4x", wantNotes: 1},
		{name: "--cpu-moe : rien",
			cfg: map[string]string{"EXTRA_ARGS": "--cpu-moe"}, si: two},
		{name: "-sm row : rien",
			cfg: map[string]string{"EXTRA_ARGS": "-sm row"}, si: two},
		{name: "--split-mode=layer explicite : 4x",
			cfg: map[string]string{"EXTRA_ARGS": "--split-mode=layer"}, si: two, want: "4x", wantNotes: 1},
		{name: "NGL=28 : des couches restent au CPU, rien",
			cfg: map[string]string{"NGL": "28"}, si: two},
		{name: "-ngl 40 dans EXTRA_ARGS l'emporte sur NGL=999 : rien",
			cfg: map[string]string{"NGL": "999", "EXTRA_ARGS": "-ngl 40"}, si: two},
		{name: "cache KV sur CPU : rien",
			cfg: map[string]string{"EXTRA_ARGS": "-nkvo"}, si: two},
		{name: "--device d'une seule carte : rien",
			cfg: map[string]string{"EXTRA_ARGS": "--device CUDA0"}, si: two},
		{name: "--device de deux cartes CUDA, nvidia-smi non sondé : 4x",
			cfg: map[string]string{"EXTRA_ARGS": "--device CUDA1,CUDA0 --tensor-split 0.6,0.4"}, want: "4x", wantNotes: 1},
		{name: "--device Vulkan : la variable ne concerne pas ce moteur",
			cfg: map[string]string{"EXTRA_ARGS": "-dev Vulkan0,Vulkan1"}, si: two},
		{name: "--device none : rien",
			cfg: map[string]string{"EXTRA_ARGS": "-dev none"}, si: two},
		{name: "LLAMA_ARG_N_CPU_MOE dans l'environnement du moteur : rien",
			cfg: map[string]string{}, si: serveSysInfo{GPUs: 2, ArgEnv: map[string]string{"LLAMA_ARG_N_CPU_MOE": "30"}}},
		{name: "LLAMA_ARG_SPLIT_MODE=none : rien",
			cfg: map[string]string{}, si: serveSysInfo{GPUs: 2, ArgEnv: map[string]string{"LLAMA_ARG_SPLIT_MODE": "none"}}},
		{name: "NGL=auto et LLAMA_ARG_N_GPU_LAYERS=20 : rien",
			cfg: map[string]string{"NGL": "auto"}, si: serveSysInfo{GPUs: 2, ArgEnv: map[string]string{"LLAMA_ARG_N_GPU_LAYERS": "20"}}},
		{name: "LLAMA_ARG_CPU_MOE=0 : désactivé, pipeline possible",
			cfg: map[string]string{}, si: serveSysInfo{GPUs: 2, ArgEnv: map[string]string{"LLAMA_ARG_CPU_MOE": "0"}}, want: "4x", wantNotes: 1},
		{name: "LLAMA_ARG_DEVICE d'une seule carte : rien",
			cfg: map[string]string{}, si: serveSysInfo{GPUs: 2, ArgEnv: map[string]string{"LLAMA_ARG_DEVICE": "CUDA1"}}},
		// Relecture : ce que llama.cpp fait vraiment de ses variables.
		{name: "--n-cpu-moe 0 n'annule pas LLAMA_ARG_N_CPU_MOE (surcharges cumulées) : rien",
			cfg: map[string]string{"EXTRA_ARGS": "--n-cpu-moe 0"},
			si:  serveSysInfo{GPUs: 2, ArgEnv: map[string]string{"LLAMA_ARG_N_CPU_MOE": "30"}}},
		{name: "deux --n-cpu-moe, le dernier à 0 : le premier compte quand même, rien",
			cfg: map[string]string{"EXTRA_ARGS": "--n-cpu-moe 20 --n-cpu-moe 0"}, si: two},
		{name: "--n-cpu-ffn (FFN dense sur CPU) : rien",
			cfg: map[string]string{"EXTRA_ARGS": "-ncffn 8"}, si: two},
		{name: "LLAMA_ARG_OVERRIDE_TENSOR : rien",
			cfg: map[string]string{}, si: serveSysInfo{GPUs: 2, ArgEnv: map[string]string{"LLAMA_ARG_OVERRIDE_TENSOR": "exps=CPU"}}},
		{name: "LLAMA_ARG_CPU_MOE=yes : llama.cpp ne l'active pas, 4x",
			cfg: map[string]string{}, si: serveSysInfo{GPUs: 2, ArgEnv: map[string]string{"LLAMA_ARG_CPU_MOE": "yes"}}, want: "4x", wantNotes: 1},
		{name: "LLAMA_ARG_NO_KV_OFFLOAD=0 : sa seule présence coupe l'offload, rien",
			cfg: map[string]string{}, si: serveSysInfo{GPUs: 2, ArgEnv: map[string]string{"LLAMA_ARG_NO_KV_OFFLOAD": "0"}}},
		{name: "LLAMA_ARG_KV_OFFLOAD=false : rien",
			cfg: map[string]string{}, si: serveSysInfo{GPUs: 2, ArgEnv: map[string]string{"LLAMA_ARG_KV_OFFLOAD": "false"}}},
		{name: "-kvo sur la ligne l'emporte sur LLAMA_ARG_NO_KV_OFFLOAD : 4x",
			cfg: map[string]string{"EXTRA_ARGS": "-kvo"},
			si:  serveSysInfo{GPUs: 2, ArgEnv: map[string]string{"LLAMA_ARG_NO_KV_OFFLOAD": "1"}}, want: "4x", wantNotes: 1},
		{name: "-nkvo puis -kvo : le dernier gagne, 4x",
			cfg: map[string]string{"EXTRA_ARGS": "-nkvo -kvo"}, si: two, want: "4x", wantNotes: 1},
		{name: "NGL absent : Loki passe -ngl auto, LLAMA_ARG_N_GPU_LAYERS ne compte pas, 4x",
			cfg: map[string]string{}, si: serveSysInfo{GPUs: 2, ArgEnv: map[string]string{"LLAMA_ARG_N_GPU_LAYERS": "20"}}, want: "4x", wantNotes: 1},
		{name: "NGL=Auto (casse libre, comme nglArgs) : 4x",
			cfg: map[string]string{"NGL": "Auto"}, si: two, want: "4x", wantNotes: 1},
		{name: "-sm tensor : pas de pipeline par couches, rien",
			cfg: map[string]string{"EXTRA_ARGS": "-sm tensor"}, si: two},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, notes := launchQueuesEnv(c.cfg, splitArgs(c.cfg["EXTRA_ARGS"]), c.si)
			if got != c.want {
				t.Fatalf("valeur = %q, attendu %q", got, c.want)
			}
			if len(notes) != c.wantNotes {
				t.Fatalf("notes = %q, en attendait %d", notes, c.wantNotes)
			}
		})
	}
}

// La variable passe par l'environnement : la ligne de commande ne bouge pas
// d'un octet, et la sélection GPU l'accompagne.
func TestBuildServeArgsLaunchQueues(t *testing.T) {
	const bin, model = "/opt/llama/llama-server", "/models/Qwen3.6-27B-Q4_K_M.gguf"
	cfg := map[string]string{"NGL": "999", "CUDA_VISIBLE_DEVICES": "1,0"}
	si := serveSysInfo{Help: helpRecent, Model: model}
	without, _, _ := buildServeArgs(cfg, nil, bin, si)
	si.GPUs = 2
	args, env, notes := buildServeArgs(cfg, nil, bin, si)
	if !reflect.DeepEqual(args, without) {
		t.Fatalf("la ligne de commande a changé\n got %q\nwant %q", args, without)
	}
	want := map[string]string{"CUDA_VISIBLE_DEVICES": "1,0", "CUDA_DEVICE_ORDER": "PCI_BUS_ID",
		"CUDA_SCALE_LAUNCH_QUEUES": "4x"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("env = %v, want %v", env, want)
	}
	// Une note pour 999 → auto, une pour les files de lancement.
	if len(notes) != 2 {
		t.Fatalf("notes = %q", notes)
	}
}
