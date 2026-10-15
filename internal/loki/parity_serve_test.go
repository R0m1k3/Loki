package loki

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/fstest"
)

// parity_serve_test.go — journal de référence de « loki serve » (lot 14 de la
// migration Rust) : la ligne de commande de llama-server, son environnement et
// ses notes, pour des centaines de lancements (machines × presets × aides de
// moteur × variables), plus les sondes pures (cgroup, cpuset, nvidia-smi,
// --list-devices, build du moteur). Rangé dans
// rust/loki-rs/tests/parity_serve/serve.json ; tests/serve.rs le rejoue contre
// serve::build_serve_args.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteServe

const parityServeFile = "../../rust/loki-rs/tests/parity_serve/serve.json"

type parityServeCase struct {
	M     int      `json:"m"` // machine
	C     int      `json:"c"` // preset
	H     int      `json:"h"` // aide du moteur
	E     int      `json:"e"` // variables du moteur
	Args  []string `json:"args"`
	Env   any      `json:"env"`
	Notes []string `json:"notes"`
}

const (
	parityHelpModern = "-ngl, --gpu-layers, --n-gpu-layers N   max. number of layers to store in VRAM, either an exact number, 'auto', or 'all' (default: auto)\n" +
		"--reasoning [on|off|auto]  --reasoning-budget N  --load-mode {auto,mmap,none,mlock,mmap+mlock,dio}\n" +
		"-fit, --fit [on|off]  -fitt, --fit-target MiB0,MiB1  --no-kv-unified  -cram, --cache-ram N  --slot-save-path PATH\n" +
		"-ctxcp, --ctx-checkpoints N  -cms, --checkpoint-min-step N  --reasoning-preserve, --no-reasoning-preserve\n" +
		"--spec-type {none,draft-simple,draft-mtp,ngram-mod}  --spec-draft-n-max N  --spec-draft-sampling {greedy,probabilistic}\n" +
		"--spec-ngram-mod-n-match N  -sm, --split-mode {none,layer,row,tensor}  --context-shift, --no-context-shift\n"
	parityHelpOld = "-ngl, --gpu-layers, --n-gpu-layers N   number of layers to store in VRAM\n" +
		"--mlock  --no-mmap  --no-context-shift  --cache-ram N\n"
)

func TestPariteServe(t *testing.T) {
	gDense := &GGUFInfo{Version: 3, Arch: "qwen3", BlockCount: 64, ContextLength: 262144, KeyLen: 128, ValLen: 128,
		HeadCountKV: 8, EmbeddingLength: 5120, HeadCount: 40, TensorCount: 707}
	gMoE := &GGUFInfo{Version: 3, Arch: "qwen3moe", BlockCount: 48, ContextLength: 262144, ExpertCount: 128,
		KeyLen: 128, ValLen: 128, HeadCountKV: 4, EmbeddingLength: 2048, HeadCount: 32, TensorCount: 579}
	gHybrid := &GGUFInfo{Version: 3, Arch: "qwen35", BlockCount: 64, NextN: 1, ContextLength: 262144, KeyLen: 256,
		ValLen: 256, HeadCountKV: 4, FullAttnInterval: 4, Hybrid: true, EmbeddingLength: 5120, HeadCount: 24,
		SSMConv: 4, SSMInner: 6144, SSMState: 128, SSMGroups: 16, HasNextNTensor: true, TensorCount: 900}
	gLayers := &GGUFInfo{Version: 3, Arch: "gemma3", BlockCount: 6, KeyLen: 64, ValLen: 64,
		HeadCountKVLayers: []int{2, 2, 0, 2, 2, 0}, HeadCountKV: 2, Hybrid: true, SSMConv: 4, SSMInner: 512,
		SSMState: 16, SSMGroups: 1}
	gDeny := &GGUFInfo{Version: 3, Arch: "deepseek2", BlockCount: 61, ExpertCount: 256, KeyLen: 192, ValLen: 128,
		HeadCountKV: 128}
	draftMTP := &GGUFInfo{Arch: "qwen35", BlockCount: 1, HasNextNTensor: true}
	draftEagle := &GGUFInfo{Arch: "eagle3", BlockCount: 1}
	draftSmall := &GGUFInfo{Arch: "qwen3", BlockCount: 28, KeyLen: 128, ValLen: 128, HeadCountKV: 8}
	gib := int64(1 << 30)
	devs2 := []splitDev{{ID: "CUDA0", TotalMiB: 16311}, {ID: "CUDA1", TotalMiB: 12288}}

	machines := []serveSysInfo{
		// 0 : une carte de 16 Go, 64 Go de RAM, dense de 16 Go.
		{Model: "/m/dense.gguf", GGUF: gDense, ModelBytes: 16 * gib, RAMMiB: 65536, RAMAvailMiB: 49152, VRAMMiB: 16311, GPUs: 1},
		// 1 : deux cartes, 32 Go de RAM, MoE de 22 Go.
		{Model: "/m/moe.gguf", GGUF: gMoE, ModelBytes: 22 * gib, RAMMiB: 32768, RAMAvailMiB: 20000, VRAMMiB: 28599, GPUs: 2,
			SplitDevs: devs2},
		// 2 : rien de connu.
		{Model: "/m/inconnu.gguf"},
		// 3 : hybride avec tête MTP, RAM serrée, build ancien.
		{Model: "/m/hybrid.gguf", GGUF: gHybrid, ModelBytes: 20 * gib, RAMMiB: 24576, RAMAvailMiB: 6000, VRAMMiB: 24576,
			GPUs: 1, EngineBuild: 9000},
		// 4 : hybride, build récent, deux cartes, beaucoup de RAM.
		{Model: "/m/hybrid.gguf", GGUF: gHybrid, ModelBytes: 15 * gib, RAMMiB: 131072, RAMAvailMiB: 100000, VRAMMiB: 28599,
			GPUs: 2, EngineBuild: 12000, SplitDevs: devs2, SlotDir: "/data/slots"},
		// 5 : architecture exclue du mode tensor, VRAM inconnue, clé d'API.
		{Model: "/m/ds.gguf", GGUF: gDeny, ModelBytes: 200 * gib, RAMMiB: 262144, RAMAvailMiB: 200000, APIKey: "sk-loki-x",
			SplitDevs: []splitDev{{ID: "Vulkan0", TotalMiB: 8000}, {ID: "CUDA0", TotalMiB: 8000}}},
		// 6 : couches par tableau, mémoire unifiée, conteneur à l'étroit, dossier des slots.
		{Model: "/m/layers.gguf", GGUF: gLayers, ModelBytes: 3 * gib, RAMMiB: 16384, RAMAvailMiB: 4096, UnifiedMem: true,
			CPU: cpuBudget{N: 4, Engine: 8, Why: "cpuset du conteneur, 4 cœurs physiques permis sur 8"}, SlotDir: "/data/slots",
			MMProj: "/m/mmproj.gguf", EngineVRAMMiB: 0},
		// 7 : brouillon MTP à part, deux cartes, build récent, VRAM du moteur connue.
		{Model: "/m/dense.gguf", GGUF: gDense, ModelBytes: 30 * gib, RAMMiB: 32768, RAMAvailMiB: 16000, VRAMMiB: 24000,
			GPUs: 2, EngineBuild: 11500, Draft: "/m/mtp-head.gguf", DraftGGUF: draftMTP, EngineVRAMMiB: 24000,
			SplitDevs: devs2, LaunchQueues: ""},
	}
	machines = append(machines,
		// 8 : brouillon EAGLE, puis petit modèle brouillon, puis le modèle lui-même.
		serveSysInfo{Model: "/m/dense.gguf", GGUF: gDense, ModelBytes: 16 * gib, RAMMiB: 65536, RAMAvailMiB: 49152,
			VRAMMiB: 16311, GPUs: 1, EngineBuild: 12000, Draft: "/m/eagle.gguf", DraftGGUF: draftEagle},
		serveSysInfo{Model: "/m/dense.gguf", GGUF: gDense, ModelBytes: 16 * gib, RAMMiB: 65536, RAMAvailMiB: 49152,
			VRAMMiB: 16311, GPUs: 1, EngineBuild: 12000, Draft: "/m/small.gguf", DraftGGUF: draftSmall},
		serveSysInfo{Model: "/m/hybrid.gguf", GGUF: gHybrid, ModelBytes: 15 * gib, RAMMiB: 65536, RAMAvailMiB: 49152,
			VRAMMiB: 24576, GPUs: 1, EngineBuild: 12000, Draft: "/m/hybrid.gguf", DraftGGUF: gHybrid, DraftIsModel: true,
			SpecAutoBlocked: "le dernier lancement avec cette configuration n'a jamais répondu"},
		// 11 : brouillon introuvable.
		serveSysInfo{Model: "/m/dense.gguf", GGUF: gDense, ModelBytes: 16 * gib, RAMMiB: 65536, RAMAvailMiB: 49152,
			VRAMMiB: 16311, GPUs: 1, EngineBuild: 12000, DraftErr: "stat /m/absent.gguf: no such file or directory"},
	)

	presets := []map[string]string{
		{},
		{"CTX": "65536", "NGL": "999", "BATCH": "4096", "UBATCH": "2048", "HOST": "127.0.0.1", "PORT": "8081"},
		{"NGL": "auto"}, {"NGL": "40"}, {"NGL": "all"}, {"NGL": " 999 "},
		{"THREADS": "8", "THREADS_BATCH": "4"}, {"THREADS": "abc", "THREADS_BATCH": "-2"}, {"THREADS": "0"},
		{"KV_TYPE": "q8_0"}, {"KV_TYPE_K": "q4_0", "KV_TYPE_V": "f16"}, {"KV_TYPE": "bf16"}, {"KV_TYPE": "q5_1"},
		{"REASONING": "on"}, {"REASONING": "off"}, {"REASONING": "auto", "REASONING_BUDGET": "2048"}, {"REASONING": "deepseek"},
		{"REASONING_PRESERVE": "on"}, {"REASONING_PRESERVE": "off"}, {"REASONING_PRESERVE": "peut-être"},
		{"CACHE_RAM": "auto"}, {"CACHE_RAM": "4096"}, {"CACHE_RAM": "0"}, {"CACHE_RAM": "-1"}, {"CACHE_RAM": "beaucoup"},
		{"CACHE_ISOLATE": "off"}, {"CACHE_RAM": "0", "CACHE_ISOLATE": "on"},
		{"SIDE_SLOT": "on"}, {"SIDE_SLOT": "on", "PARALLEL": "3"}, {"SIDE_SLOT": "on", "CTX": "0"}, {"SIDE_SLOT": "on", "CTX": "8192", "KV_TYPE": "q8_0"},
		{"SPLIT_MODE": "tensor"}, {"SPLIT_MODE": "tensor", "CTX": "16384"}, {"SPLIT_MODE": "bizarre"}, {"SPLIT_MODE": "layer"},
		{"SPLIT_MODE": "tensor", "KV_TYPE": "q8_0"}, {"SPLIT_MODE": "tensor", "NGL": "20"},
		{"SPEC": "auto"}, {"SPEC": "mtp"}, {"SPEC": "ngram"}, {"SPEC": "mtp+ngram"}, {"SPEC": "ngram+mtp"}, {"SPEC": "turbo"},
		{"SPEC": "off", "MODEL_DRAFT": "mtp-head.gguf"}, {"SPEC": "mtp", "MODEL_DRAFT": "mtp-head.gguf", "SPEC_N_MAX": "4"},
		{"SPEC": "mtp", "SPEC_N_MAX": "99", "SPEC_SAMPLING": "probabilistic"}, {"SPEC": "auto", "SPEC_SAMPLING": "probabilistic"},
		{"SPEC": "mtp", "SPEC_SAMPLING": "aléatoire"}, {"SPEC": "ngram", "MODEL_DRAFT": "x.gguf", "SPEC_N_MAX": "3"},
		{"SPEC": "auto", "MODEL_DRAFT": "small.gguf"}, {"SPEC": "mtp+ngram", "MODEL_DRAFT": "small.gguf"},
		{"CUDA_LAUNCH_QUEUES": "off"}, {"CUDA_LAUNCH_QUEUES": "2x"}, {"CUDA_LAUNCH_QUEUES": "8x"}, {"CUDA_LAUNCH_QUEUES": "auto"},
		{"CUDA_VISIBLE_DEVICES": "1,0"},
		{"FIT_TARGET": "1024,3072"}, {"FIT_TARGET": "512"}, {"FIT_TARGET": "1024,,3072"}, {"FIT_TARGET": "1,2,3,4,5,6,7,8,9"},
		{"FIT_TARGET": "2048/4096", "CTX": "0"}, {"FIT_TARGET": "2048", "NGL": "30"},
		{"OP_OFFLOAD_MIN_BATCH": "128"}, {"OP_OFFLOAD_MIN_BATCH": "0"},
		{"CUDA_GRAPH_OPT": "on"}, {"CUDA_GRAPH_OPT": "oui"}, {"CUDA_GRAPH_OPT": "off"},
		{"CTX_CHECKPOINTS": "8"}, {"CTX_CHECKPOINTS": "beaucoup"}, {"CKPT_MIN_STEP": "1024"}, {"CKPT_MIN_STEP": "0"},
		{"SLOT_PERSIST": "on"}, {"SLOT_PERSIST": "on", "HOST": "127.0.0.1"}, {"SLOT_PERSIST": "on", "HOST": "127.0.0.1", "SPEC": "mtp"},
		{"LOAD_GUARD": "off", "EXTRA_ARGS": "--load-mode none"}, {"EXTRA_ARGS": "--mlock --no-mmap"}, {"EXTRA_ARGS": "--mlock"},
		{"EXTRA_ARGS": "--no-mmap"}, {"EXTRA_ARGS": "--mmap"}, {"EXTRA_ARGS": "--load-mode dio"}, {"EXTRA_ARGS": "--load-mode=mlock"},
		{"EXTRA_ARGS": "-ngl 30 --parallel 4"}, {"EXTRA_ARGS": "-t 6 -tb 3"}, {"EXTRA_ARGS": "-Cb 0xff --poll-batch 1"},
		{"THREADS": "6", "EXTRA_ARGS": "--prio-batch 2"}, {"EXTRA_ARGS": "--cache-reuse 256 --context-shift"},
		{"EXTRA_ARGS": "-ctk q8_0 -ctv q8_0"}, {"EXTRA_ARGS": "-ot exps=CPU"}, {"EXTRA_ARGS": "-ot 'blk\\.(1|2)\\.ffn_.*_exps=CPU' -ub 512"},
		{"EXTRA_ARGS": "--n-cpu-moe 20"}, {"EXTRA_ARGS": "--cpu-moe"}, {"EXTRA_ARGS": "-sm row"}, {"EXTRA_ARGS": "-ts 1,1"},
		{"EXTRA_ARGS": "-dev CUDA0,CUDA1"}, {"EXTRA_ARGS": "-dev Vulkan0"}, {"EXTRA_ARGS": "--fit off"}, {"EXTRA_ARGS": "--mmproj /m/x.gguf"},
		{"EXTRA_ARGS": "--spec-synth-len 4"}, {"EXTRA_ARGS": "--slot-save-path /x"}, {"EXTRA_ARGS": "-c 0"},
		{"EXTRA_ARGS": "--host 127.0.0.1 --port 9000"}, {"EXTRA_ARGS": "--mirostat 2", "SPEC": "mtp", "SPEC_SAMPLING": "probabilistic"},
		{"EXTRA_ARGS": "--chat-template-kwargs '{\"preserve_reasoning\":true}'", "REASONING_PRESERVE": "on"},
		{"EXTRA_ARGS": "--spec-type ngram-mod", "SPEC": "auto"}, {"EXTRA_ARGS": "--spec-draft-n-max 8", "SPEC": "ngram"},
		{"EXTRA_ARGS": "-ot attn=CUDA1", "CUDA_GRAPH_OPT": "on"}, {"EXTRA_ARGS": "-fa off", "SPLIT_MODE": "tensor"},
		{"EXTRA_ARGS": "-nkvo"}, {"EXTRA_ARGS": "--no-context-shift"}, {"EXTRA_ARGS": "-c 8192 -np 2", "SIDE_SLOT": "on"},
		{"EXTERNAL": "1", "SIDE_SLOT": "on", "SPLIT_MODE": "tensor"},
		{"MMPROJ": "x", "NGL": "999", "CTX": "40000", "KV_TYPE": "q4_0", "SPEC": "auto", "CACHE_RAM": "auto"},
	}
	helps := []string{parityHelpModern, parityHelpOld, ""}
	argEnvs := []map[string]string{
		{},
		{"LLAMA_ARG_CACHE_TYPE_K": "q8_0"}, {"LLAMA_ARG_NO_KV_OFFLOAD": "1"}, {"LLAMA_ARG_N_CPU_MOE": "10"},
		{"LLAMA_ARG_CACHE_RAM": "1024"}, {"LLAMA_ARG_CONTEXT_SHIFT": "on"}, {"LLAMA_ARG_FIT": "off"},
		{"LLAMA_ARG_SPLIT_MODE": "row"}, {"LLAMA_ARG_DEVICE": "CUDA0"}, {"LLAMA_ARG_MLOCK": "1", "LLAMA_ARG_NO_MMAP": "1"},
		{"LLAMA_ARG_NO_CONTEXT_SHIFT": "true"}, {"LLAMA_ARG_SPEC_SYNTH_LEN": "3"}, {"LLAMA_ARG_TENSOR_SPLIT": "1,1"},
	}
	userEnvs := []map[string]string{{}, {"GGML_CUDA_GRAPH_OPT": "1", "GGML_CUDA_ALLREDUCE": "nccl", "GGML_OP_OFFLOAD_MIN_BATCH": "64"}}

	var fx struct {
		Machines []serveSysInfo      `json:"machines"`
		Presets  []map[string]string `json:"presets"`
		Helps    []string            `json:"helps"`
		ArgEnvs  []map[string]string `json:"arg_envs"`
		UserEnvs []map[string]string `json:"user_envs"`
		Cases    []parityServeCase   `json:"cases"`

		CPUList  map[string]any    `json:"cpu_list"`
		Cpuset   []map[string]any  `json:"cpuset"`
		Cgroup   []map[string]any  `json:"cgroup"`
		SMI      []map[string]any  `json:"smi"`
		Devices  map[string]any    `json:"devices"`
		Builds   map[string]int    `json:"builds"`
		Cmdlines []map[string]any  `json:"cmdlines"`
		Verdicts []map[string]any  `json:"verdicts"`
		Fit      map[string]bool   `json:"help_fit"`
		Gib      map[string]string `json:"gib"`
		Load     []map[string]any  `json:"load_flags"`
		State    []map[string]any  `json:"state_bytes"`
	}
	fx.Machines, fx.Presets, fx.Helps, fx.ArgEnvs, fx.UserEnvs = machines, presets, helps, argEnvs, userEnvs
	add := func(m, c, h, e int) {
		si := machines[m]
		si.Help = helps[h]
		si.ArgEnv = map[string]string{}
		for k, v := range argEnvs[e] {
			si.ArgEnv[k] = v
		}
		si.UserEnv = map[string]string{}
		if (m+c)%3 == 1 {
			for k, v := range userEnvs[1] {
				si.UserEnv[k] = v
			}
		}
		cfg := presets[c]
		args, env, notes := buildServeArgs(cfg, splitArgs(cfg["EXTRA_ARGS"]), "/app/llama-server", si)
		fx.Cases = append(fx.Cases, parityServeCase{M: m, C: c, H: h, E: e, Args: args, Env: env, Notes: notes})
	}
	for m := range machines {
		for c := range presets {
			add(m, c, 0, 0)
		}
	}
	for c := range presets {
		add(0, c, 1, 0)
		add(2, c, 2, 0)
		add(4, c, 1, 0)
	}
	for e := 1; e < len(argEnvs); e++ {
		for _, c := range []int{0, 1, 9, 20, 27, 31, 37, 39, 40, 57, 65, 69, 77, 84, 92, 93} {
			add((e+c)%len(machines), c, 0, e)
		}
	}

	// Sondes pures.
	fx.CPUList = map[string]any{}
	for _, s := range []string{"0-3,8-11", " 5 ", "", "0,0,1", "3-1", "a-b", "0-70000", "2-2,1"} {
		l, err := parseCPUList(s)
		if err != nil {
			fx.CPUList[s] = "erreur"
		} else {
			fx.CPUList[s] = l
		}
	}
	topo := func(fs fstest.MapFS, cpus int, smt bool) {
		for c := 0; c < cpus; c++ {
			sib := c
			if smt {
				sib = c / 2
			}
			fs["sys/devices/system/cpu/cpu"+strconv.Itoa(c)+"/topology/thread_siblings"] = &fstest.MapFile{Data: []byte(strconv.Itoa(sib) + "\n")}
		}
	}
	for _, tc := range []struct {
		name  string
		files map[string]string
		cpus  int
		smt   bool
	}{
		{"sans restriction", map[string]string{"sys/devices/system/cpu/online": "0-7\n", "proc/self/status": "Name:\tx\nCpus_allowed_list:\t0-7\n"}, 8, true},
		{"cpuset", map[string]string{"sys/devices/system/cpu/online": "0-15\n", "proc/self/status": "Cpus_allowed_list:\t0-5\n"}, 16, true},
		{"quota v2", map[string]string{"sys/devices/system/cpu/online": "0-15\n", "proc/self/status": "Cpus_allowed_list:\t0-15\n", "sys/fs/cgroup/cpu.max": "250000 100000\n"}, 16, false},
		{"quota v1", map[string]string{"sys/devices/system/cpu/online": "0-15\n", "proc/self/status": "Cpus_allowed_list:\t0-15\n", "sys/fs/cgroup/cpu/cpu.cfs_quota_us": "150000\n", "sys/fs/cgroup/cpu/cpu.cfs_period_us": "100000\n"}, 16, false},
		{"cœurs E", map[string]string{"sys/devices/system/cpu/online": "0-11\n", "proc/self/status": "Cpus_allowed_list:\t0-3,8-11\n", "sys/devices/cpu_atom/cpus": "8-11\n"}, 12, false},
		{"max", map[string]string{"sys/devices/system/cpu/online": "0-3\n", "proc/self/status": "Cpus_allowed_list:\t0-3\n", "sys/fs/cgroup/cpu.max": "max 100000\n"}, 4, false},
		{"illisible", map[string]string{"sys/devices/system/cpu/online": "x\n"}, 0, false},
	} {
		fs := fstest.MapFS{}
		for p, d := range tc.files {
			fs[p] = &fstest.MapFile{Data: []byte(d)}
		}
		topo(fs, tc.cpus, tc.smt)
		files := map[string]string{}
		for p, f := range fs {
			files[p] = string(f.Data)
		}
		b := cpusetThreads(fs)
		fx.Cpuset = append(fx.Cpuset, map[string]any{"name": tc.name, "files": files, "n": b.N, "engine": b.Engine, "why": b.Why})
	}
	for _, files := range []map[string]string{
		{"sys/fs/cgroup/memory.max": "17179869184\n", "sys/fs/cgroup/memory.current": "4294967296\n"},
		{"sys/fs/cgroup/memory.max": "max\n"},
		{"sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712\n"},
		{"sys/fs/cgroup/memory/memory.limit_in_bytes": "8589934592\n", "sys/fs/cgroup/memory/memory.usage_in_bytes": "1073741824\n"},
		{},
	} {
		fs := fstest.MapFS{}
		for p, d := range files {
			fs[p] = &fstest.MapFile{Data: []byte(d)}
		}
		l, c := cgroupMemMiB(fs)
		fx.Cgroup = append(fx.Cgroup, map[string]any{"files": files, "limit": l, "current": c})
	}
	smiOut := "0, 16311\n1, 12288\n"
	for _, tc := range []struct {
		out, cvd string
		pci      bool
	}{{smiOut, "", true}, {smiOut, "1", true}, {smiOut, "1,0", false}, {smiOut, "2", true}, {smiOut, "GPU-abc", true},
		{"0, 0\n", "", true}, {"", "", true}, {"0, 8192\n", "0", false}} {
		fx.SMI = append(fx.SMI, map[string]any{"out": tc.out, "cvd": tc.cvd, "pci": tc.pci, "mib": vramFromSMI(tc.out, tc.cvd, tc.pci)})
	}
	devOut := "Available devices:\n  CUDA0: NVIDIA GeForce RTX 5060 Ti (16311 MiB, 15579 MiB free)\n  CUDA1: NVIDIA GeForce RTX 3060 (12288 MiB, 11000 MiB free)\r\n  Vulkan0: AMD (8000 MiB, 0 MiB free)\nautre ligne\n"
	fx.Devices = map[string]any{"out": devOut, "devs": splitDevsFrom(parseListDevices(devOut))}
	fx.Builds = map[string]int{}
	for _, out := range []string{"version: 6543 (abcdef0)\nbuilt with gcc", "llama-server build 11009, commit 1a2b3c4", "rien", "build: 12 (ff)"} {
		b, _ := parseEngineBuild(out)
		fx.Builds[out] = b
	}
	for _, tc := range []struct {
		env  map[string]string
		args []string
	}{
		{map[string]string{"CUDA_VISIBLE_DEVICES": "0,1", "GGML_X": "a b"}, []string{"/app/llama-server", "-m", "/m/a b.gguf", "--api-key", "secret", "--chat-template-kwargs", `{"x":"y"}`, ""}},
		{map[string]string{}, []string{"/bin/x", "--api-key-file", "/k"}},
	} {
		fx.Cmdlines = append(fx.Cmdlines, map[string]any{"env": tc.env, "args": tc.args, "out": engineCmdline(tc.env, tc.args)})
	}
	cur := specAutoMark{FP: "abc", Bin: "/app/llama-server", Build: 11500}
	for _, tc := range []struct {
		attempt *specAutoMark
		failed  map[string]string
	}{{nil, nil}, {&cur, nil}, {&specAutoMark{FP: "x"}, nil}, {nil, map[string]string{cur.key(): "déjà échoué"}}} {
		why, rec := specAutoVerdict(cur, tc.attempt, tc.failed)
		fx.Verdicts = append(fx.Verdicts, map[string]any{"attempt": tc.attempt, "failed": tc.failed, "why": why, "record": rec})
	}
	fx.Fit = map[string]bool{}
	for _, h := range []string{"--fit [on|off]", "-fit, --fit X", "--fit-target", "--fit=on", "x--fit", "--fit", "a,--fit,b", "--fitness"} {
		fx.Fit[h] = helpSupportsFit(h)
	}
	fx.Gib = map[string]string{}
	for _, m := range []int64{0, 512, 1280, 1536, 16311, 22528, 65536, 104857} {
		fx.Gib[strconv.Itoa(int(m))] = gibText(m)
	}
	for _, tc := range []struct {
		args   []string
		modern bool
	}{
		{[]string{"--mlock"}, true}, {[]string{"--no-mmap", "--mlock"}, true}, {[]string{"--mmap", "-x"}, true},
		{[]string{"--load-mode", "none", "--mlock"}, true}, {[]string{"--load-mode", "mlock"}, false},
		{[]string{"--load-mode=none", "--no-mmap"}, false}, {[]string{"-lm", "dio"}, false}, {[]string{"-lm", "MMAP+MLOCK"}, false},
		{[]string{"--load-mode"}, false}, {[]string{"-x"}, false},
	} {
		out, note := normalizeLoadFlags(tc.args, tc.modern)
		fx.Load = append(fx.Load, map[string]any{"args": tc.args, "modern": tc.modern, "out": out, "note": note})
	}
	for _, g := range []*GGUFInfo{gDense, gMoE, gHybrid, gLayers, gDeny, {BlockCount: 10}} {
		for _, kv := range [][2]string{{"", ""}, {"q8_0", "q4_0"}, {"f32", "iq4_nl"}} {
			pt, rec := ggufStateBytes(*g, kv[0], kv[1])
			fx.State = append(fx.State, map[string]any{"gguf": g, "k": kv[0], "v": kv[1], "per_token": pt, "recurrent": rec})
		}
	}

	got, err := parityJSON(fx)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityServeFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityServeFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityServeFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("loki serve ne produit plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteServe,\n"+
			"puis cargo test --test serve dit si le port Rust suit.", parityServeFile)
	}
}
