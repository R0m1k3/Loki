package loki

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

const helpTensor = helpRecent + `-sm,   --split-mode {none,layer,row,tensor}
                                        how to split the model across multiple GPUs
-ts,   --tensor-split N0,N1,N2,...      fraction of the model to offload to each GPU
`

// tensorReady : un hybride 64 couches de 16 Gio sur 16 + 12 Gio de VRAM, sur
// un moteur qui propose -sm tensor. Le cas où SPLIT_MODE=tensor est accepté.
func tensorReady() serveSysInfo {
	return serveSysInfo{
		Help:  helpTensor,
		Model: "/models/Qwen3.6-27B.gguf",
		GGUF: &GGUFInfo{Arch: "qwen35", BlockCount: 64, KeyLen: 256, ValLen: 256, HeadCountKV: 4,
			Hybrid: true, FullAttnInterval: 4},
		ModelBytes: 16 << 30,
		ArgEnv:     map[string]string{},
		UserEnv:    map[string]string{},
		SplitDevs:  []splitDev{{ID: "CUDA0", TotalMiB: 16311}, {ID: "CUDA1", TotalMiB: 12288}},
	}
}

func TestSplitTensorPlan(t *testing.T) {
	tensor := map[string]string{"SPLIT_MODE": "tensor", "CTX": "65536"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range tensor {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	cases := []struct {
		name  string
		cfg   map[string]string
		extra []string
		si    func(*serveSysInfo)
		on    bool
		why   string // fragment attendu de la raison du refus
	}{
		{name: "clé absente : rien, en silence", cfg: map[string]string{}},
		{name: "SPLIT_MODE=layer : le défaut, rien", cfg: map[string]string{"SPLIT_MODE": "layer"}},
		{name: "accepté", cfg: tensor, on: true},
		{name: "illisible", cfg: with("SPLIT_MODE", "rows"), why: "illisible"},
		{name: "aide qui ne cite que --tensor-split : refusé",
			cfg: tensor, why: "-sm tensor", si: func(s *serveSysInfo) { s.Help = helpRecent + "-ts, --tensor-split N0,N1\n" }},
		{name: "preset externe", cfg: with(extKeyFlag, "1"), why: "externe"},
		{name: "-sm dans EXTRA_ARGS : l'utilisateur garde la main", cfg: tensor, extra: []string{"-sm", "layer"}, why: "-sm layer"},
		{name: "LLAMA_ARG_SPLIT_MODE", cfg: tensor, why: "LLAMA_ARG_SPLIT_MODE",
			si: func(s *serveSysInfo) { s.ArgEnv["LLAMA_ARG_SPLIT_MODE"] = "row" }},
		{name: "-fa off : refusé", cfg: tensor, extra: []string{"-fa", "off"}, why: "Flash Attention"},
		{name: "-fa on : accepté", cfg: tensor, extra: []string{"-fa", "on"}, on: true},
		{name: "--backend-sampling : refusé", cfg: tensor, extra: []string{"--backend-sampling"}, why: "backend-sampling"},
		{name: "LLAMA_ARG_BACKEND_SAMPLING : refusé", cfg: tensor, why: "backend-sampling",
			si: func(s *serveSysInfo) { s.ArgEnv["LLAMA_ARG_BACKEND_SAMPLING"] = "1" }},
		{name: "KV_TYPE=q8_0 : refusé, jamais réécrit", cfg: with("KV_TYPE", "q8_0"), why: "q8_0"},
		{name: "-ctk q8_0 dans EXTRA_ARGS (preset MoE) : refusé", cfg: tensor,
			extra: []string{"--cache-type-k", "q8_0", "--cache-type-v", "q8_0"}, why: "q8_0"},
		{name: "KV_TYPE_V=q4_0 : refusé", cfg: with("KV_TYPE_V", "q4_0"), why: "q4_0"},
		{name: "KV bf16 : accepté", cfg: with("KV_TYPE", "bf16"), on: true},
		{name: "--n-cpu-moe : refusé", cfg: tensor, extra: []string{"--n-cpu-moe", "40"}, why: "n-cpu-moe"},
		{name: "-ot : refusé", cfg: tensor, extra: []string{"-ot", "exps=CPU"}, why: "surcharge"},
		{name: "cache KV sur CPU : refusé", cfg: tensor, extra: []string{"-nkvo"}, why: "KV sur CPU"},
		{name: "NGL=40 : refusé", cfg: with("NGL", "40"), why: "NGL=40"},
		{name: "NGL=999 : accepté", cfg: with("NGL", "999"), on: true},
		{name: "-ngl 99 dans EXTRA_ARGS (≥ toutes les couches) : accepté", cfg: tensor, extra: []string{"-ngl", "99"}, on: true},
		{name: "-ngl 30 dans EXTRA_ARGS : refusé", cfg: tensor, extra: []string{"-ngl", "30"}, why: "-ngl 30"},
		{name: "CTX=0 : refusé", cfg: with("CTX", "0"), why: "non chiffré"},
		{name: "VRAM insuffisante : refusé, CTX jamais réduit", cfg: with("CTX", "262144"), why: "VRAM insuffisante"},
		{name: "qwen4exp : refusé", cfg: tensor, why: "qwen4exp", si: func(s *serveSysInfo) { s.GGUF.Arch = "qwen4exp" }},
		{name: "mamba (refusé par llama.cpp) : refusé proprement", cfg: tensor, why: "mamba",
			si: func(s *serveSysInfo) { s.GGUF.Arch = "mamba" }},
		{name: "GGUF illisible", cfg: tensor, why: "illisibles", si: func(s *serveSysInfo) { s.GGUF = nil }},
		{name: "SPEC=mtp : refusé", cfg: with("SPEC", "mtp"), why: "spéculatif"},
		{name: "SIDE_SLOT=on : refusé", cfg: with("SIDE_SLOT", "on"), why: "SIDE_SLOT"},
		{name: "une seule carte", cfg: tensor, why: "une seule carte",
			si: func(s *serveSysInfo) { s.SplitDevs = s.SplitDevs[:1] }},
		{name: "cartes inconnues", cfg: tensor, why: "inconnues", si: func(s *serveSysInfo) { s.SplitDevs = nil }},
		{name: "moteur Vulkan : refusé", cfg: tensor, why: "CUDA",
			si: func(s *serveSysInfo) { s.SplitDevs = []splitDev{{"Vulkan0", 16311}, {"Vulkan1", 12288}} }},
		{name: "--device d'une seule carte : refusé", cfg: tensor, extra: []string{"--device", "CUDA0"}, why: "une seule carte"},
		{name: "--device inconnu : refusé", cfg: tensor, extra: []string{"-dev", "CUDA0,CUDA7"}, why: "CUDA7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			si := tensorReady()
			if c.si != nil {
				c.si(&si)
			}
			on, why, _, _ := splitPlan(c.cfg, c.extra, si)
			if on != c.on {
				t.Fatalf("on = %v, attendu %v (%q)", on, c.on, why)
			}
			if !strings.Contains(why, c.why) || (c.why == "" && why != "") {
				t.Errorf("raison %q, attendu %q", why, c.why)
			}
		})
	}
}

func TestSplitTensorArgs(t *testing.T) {
	const bin = "/opt/llama/llama-server"
	cfg := map[string]string{"SPLIT_MODE": "tensor", "CTX": "65536", "NGL": "999"}

	// Clé absente : la ligne et l'environnement d'avant, à l'octet près, même
	// sur un moteur et une machine où le mode tensor serait possible.
	plain := map[string]string{"CTX": "65536", "NGL": "999"}
	got, env, _ := buildServeArgs(plain, nil, bin, tensorReady())
	si := tensorReady()
	si.Help, si.SplitDevs = helpRecent, nil
	want, wantEnv, _ := buildServeArgs(plain, nil, bin, si)
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(env, wantEnv) {
		t.Fatalf("sans SPLIT_MODE : %q %v, attendu %q %v", got, env, want, wantEnv)
	}

	got, env, notes := buildServeArgs(cfg, nil, bin, tensorReady())
	tail := []string{"-ngl", "all", "-sm", "tensor", "-ts", "57,43", "-fa", "on"}
	if i := slices.Index(got, "-ngl"); i < 0 || !reflect.DeepEqual(got[i:i+len(tail)], tail) {
		t.Fatalf("args = %q, attendu …%q…", got, tail)
	}
	if env["GGML_CUDA_ALLREDUCE"] != "internal" {
		t.Errorf("GGML_CUDA_ALLREDUCE = %q, attendu internal (NCCL compresse en BF16)", env["GGML_CUDA_ALLREDUCE"])
	}
	if len(notes) == 0 || !strings.Contains(strings.Join(notes, "\n"), "NCCL") {
		t.Errorf("la note doit parler de NCCL : %q", notes)
	}

	// Les choix de l'utilisateur gagnent et ne sont jamais doublés.
	extra := []string{"-ts", "60,40", "-fa", "on", "-ngl", "all"}
	got, _, _ = buildServeArgs(cfg, extra, bin, tensorReady())
	for _, f := range []string{"-ts", "-fa", "-ngl", "-sm"} {
		if n := countFlag(got, f); n != 1 {
			t.Errorf("%s présent %d fois : %q", f, n, got)
		}
	}

	// --device dans l'ordre inverse : la répartition suit.
	got, _, _ = buildServeArgs(cfg, []string{"--device", "CUDA1,CUDA0"}, bin, tensorReady())
	if argValue(got, "-ts") != "43,57" {
		t.Errorf("-ts = %q, attendu 43,57", argValue(got, "-ts"))
	}

	// GGML_CUDA_ALLREDUCE déjà posé : gardé, et NCCL signalé comme avec perte.
	si = tensorReady()
	si.UserEnv["GGML_CUDA_ALLREDUCE"] = "nccl"
	_, env, notes = buildServeArgs(cfg, nil, bin, si)
	if _, set := env["GGML_CUDA_ALLREDUCE"]; set {
		t.Errorf("choix de l'utilisateur écrasé : %v", env)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "BF16") {
		t.Errorf("NCCL doit être signalé : %q", notes)
	}

	// Les réglages qui lisent le placement voient le mode tensor : pas de
	// graphes Q/K/V en parallèle, pas de --fit-target, pas de file CUDA.
	withOpts := map[string]string{"CUDA_GRAPH_OPT": "on", "FIT_TARGET": "1024"}
	for k, v := range cfg {
		withOpts[k] = v
	}
	got, env, _ = buildServeArgs(withOpts, nil, bin, tensorReady())
	if env["GGML_CUDA_GRAPH_OPT"] != "" || slices.Contains(got, "--fit-target") {
		t.Errorf("mode tensor : ni GGML_CUDA_GRAPH_OPT ni --fit-target, %q %v", got, env)
	}

	// Moteur sans -sm tensor : aucun compagnon (-ngl all, -ts, -fa), une note.
	si = tensorReady()
	si.Help = helpRecent
	got, env, notes = buildServeArgs(cfg, nil, bin, si)
	if slices.Contains(got, "-sm") || slices.Contains(got, "-ts") || slices.Contains(got, "-fa") ||
		argValue(got, "-ngl") != "auto" || env["GGML_CUDA_ALLREDUCE"] != "" {
		t.Errorf("refusé : ligne d'avant attendue, %q %v", got, env)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "SPLIT_MODE=tensor refusé") {
		t.Errorf("le refus doit se dire : %q", notes)
	}
}

func countFlag(args []string, f string) int {
	n := 0
	for _, a := range args {
		if a == f {
			n++
		}
	}
	return n
}

func TestSplitTensorSlotPersist(t *testing.T) {
	si := tensorReady()
	si.Help += "--slot-save-path PATH\n"
	si.SlotDir = "/data/slots"
	cfg := map[string]string{"SLOT_PERSIST": "on", "HOST": "127.0.0.1", "SPLIT_MODE": "tensor"}
	if ok, why := slotPersistPlan(cfg, nil, si); ok || !strings.Contains(why, "SPLIT_MODE") {
		t.Errorf("SLOT_PERSIST avec SPLIT_MODE : refusé attendu, got %v %q", ok, why)
	}
}

func TestHelpSupportsSplitTensor(t *testing.T) {
	if helpSupportsSplitTensor(helpRecent + "-ts, --tensor-split N0,N1  fraction\n--override-tensor\n") {
		t.Error("« tensor » ailleurs dans l'aide ne suffit pas")
	}
	if !helpSupportsSplitTensor(helpTensor) {
		t.Error("liste littérale de -sm : oui")
	}
}
