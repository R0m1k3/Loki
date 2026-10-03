package loki

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const helpCkpt = helpRecent + `-ctxcp, --ctx-checkpoints, --swa-checkpoints N
                                        max number of context checkpoints to create per slot (default: 32)
-cms,   --checkpoint-min-step N         minimum spacing between context checkpoints in tokens (default: 8192, 0 = no minimum)
--reasoning-preserve, --no-reasoning-preserve
                                        preserve reasoning trace in the full history, not just the last assistant message (default: enabled)
`

// hybridOld : un Qwen3.6 27B hybride de 17 Go sur un moteur officiel b10678,
// machine de 64 Go. État récurrent : 48 couches × (3 × 10240 + 128 × 6144) × 4
// octets ≈ 150 Mio par point de reprise.
func hybridOld() serveSysInfo {
	return serveSysInfo{
		Help:  helpCkpt,
		Model: "/models/Qwen3.6-27B.gguf",
		GGUF: &GGUFInfo{Arch: "qwen35", Hybrid: true, BlockCount: 64, FullAttnInterval: 4, HeadCountKV: 4,
			KeyLen: 256, ValLen: 256, SSMConv: 4, SSMInner: 6144, SSMState: 128, SSMGroups: 16},
		ModelBytes:  17 << 30,
		RAMMiB:      65536,
		RAMAvailMiB: 50000,
		ArgEnv:      map[string]string{},
		EngineBuild: 10678,
	}
}

func TestCkptArgs(t *testing.T) {
	cms := []string{"--checkpoint-min-step", "2048"}
	cases := []struct {
		name  string
		cfg   map[string]string
		extra []string
		si    func(*serveSysInfo)
		want  []string
		notes int
	}{
		{name: "hybride, moteur b10678 : espacement resserré d'office", want: cms, notes: 1},
		{name: "hybride, moteur b11351 : rien", si: func(s *serveSysInfo) { s.EngineBuild = 11351 }},
		{name: "hybride, build pile au correctif : rien", si: func(s *serveSysInfo) { s.EngineBuild = engineMinRecommended }},
		{name: "build inconnu (compilé, clone superficiel, fork) : ni avis ni drapeau",
			si: func(s *serveSysInfo) { s.EngineBuild = 0 }},
		{name: "modèle dense sur moteur ancien : rien",
			si: func(s *serveSysInfo) { s.GGUF = &GGUFInfo{Arch: "qwen3", BlockCount: 64} }},
		{name: "métadonnées illisibles : jamais deviné au nom du fichier",
			si: func(s *serveSysInfo) { s.GGUF = nil }},
		{name: "arch hybride connue sans clé ssm.*", want: cms, notes: 1,
			si: func(s *serveSysInfo) { s.GGUF = &GGUFInfo{Arch: "qwen3next"} }},
		{name: "CKPT_MIN_STEP=4096 passé tel quel, quel que soit le build",
			cfg: map[string]string{"CKPT_MIN_STEP": "4096"}, want: []string{"--checkpoint-min-step", "4096"},
			si: func(s *serveSysInfo) { s.EngineBuild = 0 }},
		{name: "CKPT_MIN_STEP=0 refusé, défaut du moteur, et on le dit",
			cfg: map[string]string{"CKPT_MIN_STEP": "0"}, notes: 1},
		{name: "-cms d'EXTRA_ARGS : Loki se tait sur l'espacement",
			extra: []string{"-cms", "1024"}, notes: 1},
		{name: "--checkpoint-min-step=1024 d'EXTRA_ARGS", cfg: map[string]string{"CKPT_MIN_STEP": "4096"},
			extra: []string{"--checkpoint-min-step=1024"}},
		{name: "LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT posée : pas d'espacement d'office", notes: 1,
			si: func(s *serveSysInfo) { s.ArgEnv["LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT"] = "4096" }},
		{name: "--ctx-checkpoints d'EXTRA_ARGS ne coupe pas l'espacement", extra: []string{"--ctx-checkpoints", "8"},
			want: cms, notes: 1},
		{name: "aide sans -cms : pas de drapeau, l'avis reste", notes: 1,
			si: func(s *serveSysInfo) { s.Help = helpRecent }},
		{name: "points de reprise coupés : l'espacement ne sert à rien", cfg: map[string]string{"CTX_CHECKPOINTS": "0"},
			want: []string{"--ctx-checkpoints", "0"}, notes: 1},
		{name: "MoE mmappé de 82 Go sur 64 Go : conseil, pas d'espacement d'office", notes: 2,
			si: func(s *serveSysInfo) { s.ModelBytes = 82 << 30 }},
		{name: "RAM inconnue : pas d'espacement d'office", notes: 1,
			si: func(s *serveSysInfo) { s.RAMMiB = 0 }},
		{name: "RAM libre trop juste pour 32 points", notes: 1,
			si: func(s *serveSysInfo) { s.RAMAvailMiB = 8000 }},
		{name: "CTX_CHECKPOINTS=16 passé", cfg: map[string]string{"CTX_CHECKPOINTS": "16"},
			want: []string{"--ctx-checkpoints", "16", "--checkpoint-min-step", "2048"}, notes: 1},
		{name: "CTX_CHECKPOINTS illisible", cfg: map[string]string{"CTX_CHECKPOINTS": "beaucoup"}, want: cms, notes: 2},
		{name: "CTX_CHECKPOINTS et LLAMA_ARG_CTX_CHECKPOINTS", cfg: map[string]string{"CTX_CHECKPOINTS": "16"},
			want: cms, notes: 2,
			si: func(s *serveSysInfo) { s.ArgEnv["LLAMA_ARG_CTX_CHECKPOINTS"] = "8" }},
		{name: "CTX_CHECKPOINTS et -ctxcp d'EXTRA_ARGS", cfg: map[string]string{"CTX_CHECKPOINTS": "16"},
			extra: []string{"-ctxcp", "8"}, want: cms, notes: 1},
		{name: "moteur sans --ctx-checkpoints : CTX_CHECKPOINTS ignoré, dit", cfg: map[string]string{"CTX_CHECKPOINTS": "16"},
			notes: 2, si: func(s *serveSysInfo) { s.Help = helpRecent }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			si := hybridOld()
			if c.si != nil {
				c.si(&si)
			}
			got, notes := ckptArgs(c.cfg, c.extra, si)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("ckptArgs = %v, attendu %v", got, c.want)
			}
			if len(notes) != c.notes {
				t.Fatalf("notes = %q, attendu %d", notes, c.notes)
			}
		})
	}
}

// Place dans la ligne complète : avant EXTRA_ARGS, qui garde le dernier mot.
func TestBuildServeArgsCkpt(t *testing.T) {
	args, _, notes := buildServeArgs(map[string]string{"NGL": "999"}, []string{"--jinja"}, "/bin/llama-server", hybridOld())
	if got := strings.Join(args, " "); !strings.HasSuffix(got, "--checkpoint-min-step 2048 --jinja") {
		t.Fatalf("ligne = %s", got)
	}
	found := false
	for _, n := range notes {
		found = found || strings.Contains(n, "b10678")
	}
	if !found {
		t.Fatalf("l'avis du moteur ancien manque : %q", notes)
	}
}

func TestEngineBuildTrust(t *testing.T) {
	cases := []struct {
		src   string
		build int
		want  int
	}{
		{"downloaded", 10678, 10678},
		{"image", 11351, 11351},
		{"prebuilt", 10864, 10864},
		{"custom", 10678, 0},  // compilé ou fork : numérotation inconnue
		{"downloaded", 0, 0},  // --version illisible
		{"image", 1, 0},       // clone superficiel
		{"prebuilt", 4999, 0}, // sous le plancher
		{"downloaded", 5000, 5000},
	}
	for _, c := range cases {
		if got := engineBuildTrust(c.src, c.build); got != c.want {
			t.Errorf("engineBuildTrust(%s, %d) = %d, attendu %d", c.src, c.build, got, c.want)
		}
	}
	if engineBuildNotice(10678) == "" || engineBuildNotice(engineMinRecommended) != "" || engineBuildNotice(0) != "" {
		t.Fatal("avis : seulement pour un build connu et antérieur au correctif")
	}
}

func TestReasoningPreserveArgs(t *testing.T) {
	cases := []struct {
		name  string
		cfg   map[string]string
		extra []string
		si    func(*serveSysInfo)
		want  []string
		notes int
	}{
		{name: "clé absente : rien, même sur un moteur qui connaît le drapeau"},
		{name: "on", cfg: map[string]string{"REASONING_PRESERVE": "on"}, want: []string{"--reasoning-preserve"}},
		{name: "OFF", cfg: map[string]string{"REASONING_PRESERVE": "OFF"}, want: []string{"--no-reasoning-preserve"}},
		{name: "illisible", cfg: map[string]string{"REASONING_PRESERVE": "peut-être"}, notes: 1},
		{name: "moteur sans le drapeau", cfg: map[string]string{"REASONING_PRESERVE": "off"}, notes: 1,
			si: func(s *serveSysInfo) { s.Help = helpRecent }},
		{name: "EXTRA_ARGS tranche déjà", cfg: map[string]string{"REASONING_PRESERVE": "off"},
			extra: []string{"--reasoning-preserve"}},
		{name: "--chat-template-kwargs le fixe", cfg: map[string]string{"REASONING_PRESERVE": "off"},
			extra: []string{"--chat-template-kwargs", `{"preserve_reasoning":true}`}, notes: 1},
		{name: "LLAMA_ARG_REASONING_PRESERVE posée", cfg: map[string]string{"REASONING_PRESERVE": "off"}, notes: 1,
			si: func(s *serveSysInfo) { s.ArgEnv["LLAMA_ARG_REASONING_PRESERVE"] = "1" }},
		{name: "LLAMA_ARG_CHAT_TEMPLATE_KWARGS le fixe", cfg: map[string]string{"REASONING_PRESERVE": "on"}, notes: 1,
			si: func(s *serveSysInfo) {
				s.ArgEnv["LLAMA_ARG_CHAT_TEMPLATE_KWARGS"] = `{"preserve_reasoning":false}`
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			si := serveSysInfo{Help: helpCkpt, ArgEnv: map[string]string{}}
			if c.si != nil {
				c.si(&si)
			}
			got, notes := reasoningPreserveArgs(c.cfg, c.extra, si)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("reasoningPreserveArgs = %v, attendu %v", got, c.want)
			}
			if len(notes) != c.notes {
				t.Fatalf("notes = %q, attendu %d", notes, c.notes)
			}
		})
	}
}

// Après une mise à jour, la version téléchargée qui tournait reste installée :
// c'est le retour arrière sans réseau. Le moteur de l'image, lui, n'a rien à
// garder sous engine/.
func TestEngineKeepAfterUpdate(t *testing.T) {
	testHome(t)
	for _, tag := range []string{"server-cuda-b10500", "server-cuda-b10678", "server-cuda-b10900"} {
		d := filepath.Join(engineDir(), tag)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "llama-server"), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	nouveau := filepath.Join(engineDir(), "server-cuda-b10900", "llama-server")
	ancien := filepath.Join(engineDir(), "server-cuda-b10678", "llama-server")
	keep := engineKeepAfterUpdate(nouveau, ancien, nil)
	if !reflect.DeepEqual(keep, map[string]bool{"server-cuda-b10900": true, "server-cuda-b10678": true}) {
		t.Fatalf("gardés = %v", keep)
	}
	enginePrune(keep, nil)
	if got := engineInstalled(); !reflect.DeepEqual(got, []string{"server-cuda-b10678", "server-cuda-b10900"}) {
		t.Fatalf("après ménage : %v", got)
	}
	if keep := engineKeepAfterUpdate(nouveau, "/app/llama-server", nil); len(keep) != 1 {
		t.Fatalf("moteur de l'image : %v", keep)
	}
	if keep := engineKeepAfterUpdate(nouveau, nouveau, nil); len(keep) != 1 {
		t.Fatalf("même version : %v", keep)
	}
}
