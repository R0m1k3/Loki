package loki

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

const helpSpec = helpRecent + `--spec-draft-n-max N                    number of tokens to draft for speculative decoding (default: 3)
--spec-draft-sampling {greedy,probabilistic}
                                        how the draft is sampled (default: greedy)
--spec-type [none,ngram-cache,ngram-simple,ngram-map-k,ngram-map-k4v,ngram-mod,draft-simple,draft-eagle3,draft-mtp,draft-dflash,draft-dspark]
                                        comma-separated list of types of speculative decoding to use (default: none)
`

// helpSpecOld : un moteur qui connaît --spec-type mais pas encore le MTP.
const helpSpecOld = helpRecent + `--spec-type [none,ngram-cache,ngram-simple,draft-simple]
`

// mtpReady : un Qwen3.6 27B avec sa tête MTP dans le fichier, placé par --fit,
// sur un moteur officiel récent. Le cas où SPEC=auto a le droit d'agir.
func mtpReady() serveSysInfo {
	return serveSysInfo{
		Help:        helpSpec,
		Model:       "/models/Qwen3.6-27B-MTP.gguf",
		GGUF:        &GGUFInfo{Arch: "qwen35", BlockCount: 65, NextN: 1, HasNextNTensor: true, Hybrid: true},
		ArgEnv:      map[string]string{},
		EngineBuild: 11351,
	}
}

func TestSpecArgs(t *testing.T) {
	mtp := []string{"--spec-type", "draft-mtp", "--spec-draft-sampling", "greedy"}
	auto := map[string]string{"SPEC": "auto"}
	forced := map[string]string{"SPEC": "mtp"}
	with := func(base map[string]string, kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	cases := []struct {
		name     string
		cfg      map[string]string
		extra    []string
		si       func(*serveSysInfo)
		want     []string
		wantAuto bool
		notes    int
	}{
		{name: "SPEC absent : rien, même sur un modèle MTP (opt-in)", cfg: map[string]string{}},
		{name: "SPEC=off : rien", cfg: map[string]string{"SPEC": "off"}},
		{name: "SPEC=off avec MODEL_DRAFT : rien, et on le dit",
			cfg: map[string]string{"SPEC": "off", "MODEL_DRAFT": "mtp-Qwen3.6-27B.gguf"}, notes: 1},
		{name: "SPEC illisible : rien, et on le dit", cfg: map[string]string{"SPEC": "turbo"}, notes: 1},
		{name: "auto, tête MTP dans le fichier, --fit actif : draft-mtp", cfg: auto, want: mtp, wantAuto: true, notes: 1},
		{name: "auto, clé nextn sans tenseur (tête publiée à part) : rien",
			cfg: auto, notes: 1, si: func(s *serveSysInfo) { s.GGUF.HasNextNTensor = false }},
		{name: "auto, modèle sans MTP : rien, sans bruit",
			cfg: auto, si: func(s *serveSysInfo) { s.GGUF = &GGUFInfo{Arch: "llama", BlockCount: 32} }},
		{name: "auto, métadonnées illisibles : rien", cfg: auto, notes: 1, si: func(s *serveSysInfo) { s.GGUF = nil }},
		{name: "--spec-type dans EXTRA_ARGS : Loki se tait", cfg: auto, extra: []string{"--spec-type", "ngram-mod"}, notes: 1},
		{name: "--model-draft=… dans EXTRA_ARGS : Loki se tait", cfg: auto, extra: []string{"--model-draft=d.gguf"}, notes: 1},
		{name: "-hfd dans EXTRA_ARGS : Loki se tait", cfg: auto, extra: []string{"-hfd", "u/r"}, notes: 1},
		{name: "--spec-default dans EXTRA_ARGS : Loki se tait", cfg: auto, extra: []string{"--spec-default"}, notes: 1},
		{name: "LLAMA_ARG_SPEC_TYPE posée : Loki se tait", cfg: auto, notes: 1,
			si: func(s *serveSysInfo) { s.ArgEnv["LLAMA_ARG_SPEC_TYPE"] = "draft-mtp" }},
		{name: "auto, --tensor-split : rien", cfg: auto, extra: []string{"--tensor-split", "0.965,0.035"}, notes: 1},
		{name: "auto, -ts=… : rien", cfg: auto, extra: []string{"-ts=1,1"}, notes: 1},
		{name: "auto, NGL chiffré : rien", cfg: with(auto, "NGL", "40"), notes: 1},
		{name: "auto, -ngl 99 dans EXTRA_ARGS : rien", cfg: auto, extra: []string{"-ngl", "99"}, notes: 1},
		{name: "auto, -ot : rien", cfg: auto, extra: []string{"-ot", "exps=CPU"}, notes: 1},
		{name: "auto, --n-cpu-moe : rien", cfg: auto, extra: []string{"--n-cpu-moe", "20"}, notes: 1},
		{name: "auto, -cmoe : rien", cfg: auto, extra: []string{"-cmoe"}, notes: 1},
		{name: "auto, -sm row : rien", cfg: auto, extra: []string{"-sm", "row"}, notes: 1},
		{name: "auto, -fit off : rien", cfg: auto, extra: []string{"-fit", "off"}, notes: 1},
		{name: "auto, -dev : rien", cfg: auto, extra: []string{"-dev", "CUDA0"}, notes: 1},
		{name: "auto, LLAMA_ARG_DEVICE : rien", cfg: auto, notes: 1,
			si: func(s *serveSysInfo) { s.ArgEnv["LLAMA_ARG_DEVICE"] = "CUDA0" }},
		{name: "auto, CTX=0 (fit pourrait réduire le contexte) : rien", cfg: with(auto, "CTX", "0"), notes: 1},
		{name: "auto, projecteur vision : rien", cfg: auto, notes: 1,
			si: func(s *serveSysInfo) { s.MMProj = "/models/mmproj-F16.gguf" }},
		{name: "auto, build inconnu : rien", cfg: auto, notes: 1, si: func(s *serveSysInfo) { s.EngineBuild = 0 }},
		{name: "auto, moteur trop ancien : rien", cfg: auto, notes: 1, si: func(s *serveSysInfo) { s.EngineBuild = 10900 }},
		{name: "auto, qwen4exp : jamais d'office", cfg: auto, notes: 1, si: func(s *serveSysInfo) { s.GGUF.Arch = "qwen4exp" }},
		{name: "auto, essai précédent sans réponse : rien", cfg: auto, notes: 1,
			si: func(s *serveSysInfo) {
				s.SpecAutoBlocked = "le dernier lancement avec cette configuration n'a jamais répondu"
			}},
		{name: "auto, moteur sans draft-mtp : rien", cfg: auto, notes: 1, si: func(s *serveSysInfo) { s.Help = helpSpecOld }},
		{name: "auto, aide illisible : rien", cfg: auto, notes: 1, si: func(s *serveSysInfo) { s.Help = "" }},
		{name: "mtp imposé malgré -ts : drapeaux, avec l'avertissement", cfg: forced,
			extra: []string{"-ts", "0.965,0.035"}, want: mtp, notes: 1},
		{name: "mtp imposé sur qwen4exp, build inconnu", cfg: forced, want: mtp, notes: 1,
			si: func(s *serveSysInfo) { s.GGUF.Arch = "qwen4exp"; s.EngineBuild = 0 }},
		{name: "mtp imposé, moteur sans draft-mtp : rien", cfg: forced, notes: 1, si: func(s *serveSysInfo) { s.Help = helpSpecOld }},
		{name: "MODEL_DRAFT tête MTP à part : -md chemin absolu + draft-mtp",
			cfg: with(auto, "MODEL_DRAFT", "mtp-Qwen3.6-27B-Q8_0.gguf"), wantAuto: true, notes: 1,
			want: []string{"-md", "/models/mtp-Qwen3.6-27B-Q8_0.gguf", "--spec-type", "draft-mtp", "--spec-draft-sampling", "greedy"},
			si: func(s *serveSysInfo) {
				s.GGUF.HasNextNTensor = false
				s.Draft = "/models/mtp-Qwen3.6-27B-Q8_0.gguf"
				s.DraftGGUF = &GGUFInfo{Arch: "qwen35", BlockCount: 65, HasNextNTensor: true}
			}},
		{name: "MODEL_DRAFT petit modèle : -md + draft-simple, sinon chargé pour rien",
			cfg: with(forced, "MODEL_DRAFT", "Qwen3-0.6B.gguf"), notes: 1,
			want: []string{"-md", "/models/Qwen3-0.6B.gguf", "--spec-type", "draft-simple", "--spec-draft-sampling", "greedy"},
			si: func(s *serveSysInfo) {
				s.Draft = "/models/Qwen3-0.6B.gguf"
				s.DraftGGUF = &GGUFInfo{Arch: "qwen3", BlockCount: 28}
			}},
		{name: "MODEL_DRAFT tête dFlash : pas de draft-simple, réglage à la main",
			cfg: with(forced, "MODEL_DRAFT", "dflash-Qwen3.6.gguf"), notes: 1,
			si: func(s *serveSysInfo) {
				s.Draft = "/models/dflash-Qwen3.6.gguf"
				s.DraftGGUF = &GGUFInfo{Arch: "dflash", BlockCount: 5}
			}},
		{name: "MODEL_DRAFT tête EAGLE-3 : idem",
			cfg: with(auto, "MODEL_DRAFT", "eagle3.gguf"), notes: 1,
			si: func(s *serveSysInfo) {
				s.Draft = "/models/eagle3.gguf"
				s.DraftGGUF = &GGUFInfo{Arch: "eagle3", BlockCount: 1}
			}},
		{name: "MODEL_DRAFT introuvable : moteur lancé sans, pas d'erreur",
			cfg: with(auto, "MODEL_DRAFT", "absent.gguf"), notes: 1,
			si: func(s *serveSysInfo) { s.DraftErr = "fichier introuvable" }},
		{name: "MODEL_DRAFT illisible : rien", cfg: with(auto, "MODEL_DRAFT", "mtp-x.gguf"), notes: 1,
			si: func(s *serveSysInfo) { s.Draft = "/models/mtp-x.gguf" }},
		{name: "SPEC_N_MAX=2", cfg: with(auto, "SPEC_N_MAX", "2"), wantAuto: true, notes: 1,
			want: []string{"--spec-type", "draft-mtp", "--spec-draft-n-max", "2", "--spec-draft-sampling", "greedy"}},
		{name: "SPEC_N_MAX illisible : défaut du moteur", cfg: with(auto, "SPEC_N_MAX", "beaucoup"), want: mtp, wantAuto: true, notes: 2},
		{name: "--spec-draft-n-max dans EXTRA_ARGS : SPEC_N_MAX se tait, MTP reste",
			cfg: with(auto, "SPEC_N_MAX", "2"), extra: []string{"--spec-draft-n-max", "4"}, want: mtp, wantAuto: true, notes: 1},
		{name: "probabilistic jamais avec auto",
			cfg: with(auto, "SPEC_SAMPLING", "probabilistic"), want: mtp, wantAuto: true, notes: 2},
		{name: "probabilistic avec mtp imposé", cfg: with(forced, "SPEC_SAMPLING", "probabilistic"), notes: 1,
			want: []string{"--spec-type", "draft-mtp", "--spec-draft-sampling", "probabilistic"}},
		{name: "probabilistic refusé avec mirostat", cfg: with(forced, "SPEC_SAMPLING", "probabilistic"),
			extra: []string{"--mirostat", "2"}, want: mtp, notes: 2},
		{name: "probabilistic refusé avec adaptive-p", cfg: with(forced, "SPEC_SAMPLING", "probabilistic"),
			extra: []string{"--samplers", "top_k;adaptive_p"}, want: mtp, notes: 2},
		{name: "--spec-draft-sampling dans EXTRA_ARGS : on ne le double pas", cfg: auto,
			extra: []string{"--spec-draft-sampling", "greedy"}, want: []string{"--spec-type", "draft-mtp"}, wantAuto: true, notes: 1},
		{name: "moteur sans --spec-draft-sampling : pas de drapeau", cfg: auto, wantAuto: true, notes: 1,
			want: []string{"--spec-type", "draft-mtp"},
			si: func(s *serveSysInfo) {
				s.Help = strings.Replace(s.Help, "--spec-draft-sampling {greedy,probabilistic}", "", 1)
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			si := mtpReady()
			if c.si != nil {
				c.si(&si)
			}
			got, notes, isAuto := specArgs(c.cfg, c.extra, si)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("args = %q, attendu %q", got, c.want)
			}
			if isAuto != c.wantAuto {
				t.Errorf("auto = %v, attendu %v", isAuto, c.wantAuto)
			}
			if len(notes) != c.notes {
				t.Errorf("%d notes, attendu %d : %q", len(notes), c.notes, notes)
			}
			if specMode(c.cfg) == "auto" && slices.Contains(got, "probabilistic") {
				t.Errorf("SPEC=auto ne doit jamais choisir le tirage probabiliste : %q", got)
			}
			for _, a := range got {
				if a == "--draft" || a == "--draft-max" || a == "--draft-n" || a == "-ctkd" || a == "-ctvd" {
					t.Errorf("drapeau interdit %q", a)
				}
			}
		})
	}
}

// La spéculation passe avant EXTRA_ARGS, qui garde le dernier mot.
func TestBuildServeArgsSpec(t *testing.T) {
	si := mtpReady()
	args, _, _ := buildServeArgs(map[string]string{"SPEC": "auto"}, []string{"--jinja"}, "/bin/llama-server", si)
	i := slices.Index(args, "--spec-type")
	if i < 0 || args[i+1] != "draft-mtp" {
		t.Fatalf("--spec-type draft-mtp absent : %q", args)
	}
	if j := slices.Index(args, "--jinja"); j < i {
		t.Errorf("EXTRA_ARGS doit fermer la marche : %q", args)
	}
	args, _, _ = buildServeArgs(map[string]string{}, nil, "/bin/llama-server", si)
	if slices.Contains(args, "--spec-type") {
		t.Errorf("SPEC absent : aucun drapeau attendu, %q", args)
	}
}

func TestStatefulSampler(t *testing.T) {
	cases := map[string][]string{
		"":                               {"--mirostat", "0"},
		"--mirostat 1":                   {"--mirostat=1"},
		"--samplers adaptive-p":          {"--samplers", "penalties;adaptive-p"},
		"--sampling-seq avec adaptive-p": {"--sampling-seq", "kpa"},
	}
	for want, extra := range cases {
		if got := statefulSampler(extra); got != want {
			t.Errorf("statefulSampler(%q) = %q, attendu %q", extra, got, want)
		}
	}
	if got := statefulSampler([]string{"--samplers", "top_k;top_p;temperature"}); got != "" {
		t.Errorf("chaîne sans état : %q", got)
	}
}

func TestSpecAutoVerdict(t *testing.T) {
	cur := specAutoMark{FP: "abc", Bin: "/e/llama-server", Build: 11351}
	other := specAutoMark{FP: "abc", Bin: "/e/llama-server", Build: 11400}
	if why, rec := specAutoVerdict(cur, nil, nil); why != "" || rec {
		t.Errorf("aucun essai : permis, got %q %v", why, rec)
	}
	if why, rec := specAutoVerdict(cur, &cur, map[string]string{}); why == "" || !rec {
		t.Errorf("jeton resté pour la même combinaison : coupé et inscrit, got %q %v", why, rec)
	}
	if why, rec := specAutoVerdict(cur, &other, map[string]string{}); why != "" || rec {
		t.Errorf("jeton d'un autre build : permis (mise à jour = nouvel essai), got %q %v", why, rec)
	}
	if why, rec := specAutoVerdict(cur, nil, map[string]string{cur.key(): "OOM"}); why != "OOM" || rec {
		t.Errorf("échec inscrit : coupé, got %q %v", why, rec)
	}
}

// Le cycle complet sur une vraie base : jeton posé, jamais effacé, puis coupé
// pour cette combinaison seulement.
func TestSpecAutoCheckStore(t *testing.T) {
	testHome(t)
	// Ce que fait cmdServe : lire, puis — port libre — ranger et poser le jeton.
	launch := func(cur specAutoMark, wantAttempt bool) string {
		why, record := specAutoPeek(cur)
		specAutoSettle(cur, why, record, wantAttempt && why == "")
		return why
	}
	cur := specAutoMark{FP: "abc", Bin: "/e/llama-server", Build: 11351}
	if why := launch(cur, true); why != "" {
		t.Fatalf("base vide : permis, got %q", why)
	}
	// Second « loki serve » refusé (port pris) : il a lu, sans rien ranger. Le
	// jeton du premier, qui charge encore, doit rester intact.
	if why, _ := specAutoPeek(cur); why == "" {
		t.Fatal("jeton présent : le lecteur doit le voir")
	}
	var a specAutoMark
	if !getJSON(bkState, specAttemptKey, &a) || a != cur {
		t.Fatal("specAutoPeek ne doit rien consommer")
	}
	if why := launch(cur, true); why == "" {
		t.Fatal("jeton resté : l'auto doit être coupé")
	}
	if getJSON(bkState, specAttemptKey, &a) {
		t.Error("auto coupé : aucun nouveau jeton")
	}
	if why := launch(cur, true); why == "" {
		t.Error("l'échec doit rester inscrit au lancement suivant")
	}
	updated := cur
	updated.Build = 11400
	if why := launch(updated, true); why != "" {
		t.Errorf("moteur mis à jour : nouvel essai permis, got %q", why)
	}
	// Le process web efface SON jeton ; pas celui d'un lancement plus récent.
	specAttemptClear(cur, "")
	if !getJSON(bkState, specAttemptKey, &a) || a != updated {
		t.Fatal("le jeton d'un autre lancement doit rester")
	}
	specAttemptClear(updated, "")
	if getJSON(bkState, specAttemptKey, &a) {
		t.Fatal("le moteur a répondu : jeton effacé")
	}
	if why := launch(updated, true); why != "" {
		t.Errorf("lancement réussi : permis, got %q", why)
	}
	// Répondu mais couches en RAM : échec inscrit pour cette combinaison.
	specAttemptClear(updated, "couches en RAM")
	if why, _ := specAutoPeek(updated); why != "couches en RAM" {
		t.Errorf("échec après chargement : coupé, got %q", why)
	}
	// Un jeton d'une autre configuration est jeté sans rien conclure.
	other := specAutoMark{FP: "zzz", Bin: "/e/llama-server", Build: 11351}
	specAutoSettle(other, "", false, true)
	if why := launch(cur, false); why == "" {
		t.Error("échec de cur toujours inscrit")
	}
	if getJSON(bkState, specAttemptKey, &a) {
		t.Error("le jeton étranger doit être consommé")
	}
}

func TestLastOffload(t *testing.T) {
	log := strings.Join([]string{
		"srv load_model: loading model '/models/old.gguf'",
		"load_tensors: offloaded 40/65 layers to GPU",
		"srv load_model: loading model '/models/new.gguf'",
		"load_tensors: offloaded 65/65 layers to GPU",
		"main: server is listening",
	}, "\n")
	if n, m := lastOffload(log); n != 65 || m != 65 {
		t.Errorf("dernier chargement seulement : got %d/%d", n, m)
	}
	if n, m := lastOffload("load_tensors: offloaded 10/65 layers to GPU"); n != 0 || m != 0 {
		t.Errorf("sans ligne de chargement, on ne conclut rien : got %d/%d", n, m)
	}
	if n, m := lastOffload("srv load_model: loading model 'x'\nload_tensors: offloaded 50/65 layers to GPU"); n != 50 || m != 65 {
		t.Errorf("chargement partiel : got %d/%d", n, m)
	}
	// Le journal réel de llama-server : « load_model: initializing » APRÈS le
	// chargement, et un brouillon chargé à part qui a ses propres lignes.
	srvLog := strings.Join([]string{
		"srv    load_model: loading model '/models/Qwen3.6-27B.gguf'",
		"load_tensors: loading model tensors, this can take a while... (load_mode = mmap)",
		"load_tensors: offloaded 52/65 layers to GPU",
		"load_tensors: loading model tensors, this can take a while... (load_mode = mmap)",
		"load_tensors: offloaded 2/2 layers to GPU",
		"srv    load_model: initializing, n_slots = 1, n_ctx_slot = 65536, kv_unified = 'false'",
		"main: server is listening on http://127.0.0.1:8080",
	}, "\n")
	if n, m := lastOffload(srvLog); n != 52 || m != 65 {
		t.Errorf("modèle partiel, brouillon complet : le pire compte, got %d/%d", n, m)
	}
	full := strings.Replace(srvLog, "52/65", "65/65", 1)
	if n, m := lastOffload(full); n != m || m == 0 {
		t.Errorf("tout sur GPU : got %d/%d", n, m)
	}
}

// helpNgram : un moteur qui connaît les drapeaux actuels de ngram-mod.
const helpNgram = helpSpec + `--spec-ngram-mod-n-min N                minimum number of ngram tokens (default: 48)
--spec-ngram-mod-n-max N                maximum number of ngram tokens (default: 64)
--spec-ngram-mod-n-match N              ngram-mod lookup length (default: 24)
`

func TestSpecArgsNgram(t *testing.T) {
	ngram := []string{"--spec-type", "ngram-mod",
		"--spec-ngram-mod-n-match", "24", "--spec-ngram-mod-n-min", "48", "--spec-ngram-mod-n-max", "64"}
	both := []string{"--spec-type", "draft-mtp,ngram-mod",
		"--spec-ngram-mod-n-match", "24", "--spec-ngram-mod-n-min", "48", "--spec-ngram-mod-n-max", "64",
		"--spec-draft-sampling", "greedy"}
	cfgOf := func(kv ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	dense := func(s *serveSysInfo) { s.GGUF = &GGUFInfo{Arch: "llama", BlockCount: 32} }
	cases := []struct {
		name  string
		cfg   map[string]string
		extra []string
		si    func(*serveSysInfo)
		want  []string
		notes int
	}{
		{name: "ngram, modèle dense : forme explicite, jamais --spec-default",
			cfg: cfgOf("SPEC", "ngram"), si: dense, want: ngram, notes: 1},
		{name: "ngram+mtp s'écrit aussi dans l'autre sens",
			cfg: cfgOf("SPEC", "ngram+mtp"), want: both, notes: 2},
		{name: "ngram sur un modèle hybride à tête MTP : tête inutilisée et points de reprise dits",
			cfg: cfgOf("SPEC", "ngram"), want: ngram, notes: 3},
		{name: "ngram, moteur qui n'a que le type dans sa liste : rien",
			cfg: cfgOf("SPEC", "ngram"), notes: 1, si: func(s *serveSysInfo) { s.Help = helpSpec }},
		{name: "mtp+ngram, moteur sans drapeaux ngram-mod : rien",
			cfg: cfgOf("SPEC", "mtp+ngram"), notes: 1, si: func(s *serveSysInfo) { s.Help = helpSpec }},
		{name: "mtp+ngram avec la tête dans le fichier : une seule liste de types",
			cfg: cfgOf("SPEC", "mtp+ngram"), want: both, notes: 2},
		{name: "mtp+ngram sans tête MTP : n-grammes seuls, jamais « failed to create MTP context »",
			cfg: cfgOf("SPEC", "mtp+ngram"), si: dense, want: ngram, notes: 1},
		{name: "mtp+ngram, moteur sans draft-mtp : n-grammes seuls",
			cfg: cfgOf("SPEC", "mtp+ngram"), want: ngram, notes: 2,
			si: func(s *serveSysInfo) {
				s.Help = strings.ReplaceAll(s.Help, "draft-mtp", "draft-xxx")
			}},
		{name: "mtp+ngram avec un petit modèle brouillon : n-grammes seuls, pas de -md",
			cfg: cfgOf("SPEC", "mtp+ngram", "MODEL_DRAFT", "Qwen3-0.6B.gguf"), want: ngram, notes: 2,
			si: func(s *serveSysInfo) {
				s.Draft = "/models/Qwen3-0.6B.gguf"
				s.DraftGGUF = &GGUFInfo{Arch: "qwen3", BlockCount: 28}
			}},
		{name: "mtp+ngram avec la tête MTP publiée à part",
			cfg: cfgOf("SPEC", "mtp+ngram", "MODEL_DRAFT", "mtp-q.gguf"), notes: 2,
			want: append([]string{"-md", "/models/mtp-q.gguf"}, both...),
			si: func(s *serveSysInfo) {
				s.GGUF.HasNextNTensor = false
				s.Draft = "/models/mtp-q.gguf"
				s.DraftGGUF = &GGUFInfo{Arch: "qwen35", BlockCount: 65, HasNextNTensor: true}
			}},
		{name: "ngram + MODEL_DRAFT : brouillon ignoré, et dit",
			cfg: cfgOf("SPEC", "ngram", "MODEL_DRAFT", "mtp-q.gguf"), si: dense, want: ngram, notes: 2},
		{name: "ngram + SPEC_N_MAX : ignoré, et dit",
			cfg: cfgOf("SPEC", "ngram", "SPEC_N_MAX", "4"), si: dense, want: ngram, notes: 2},
		{name: "--spec-type dans EXTRA_ARGS : Loki se tait",
			cfg: cfgOf("SPEC", "ngram"), extra: []string{"--spec-type", "ngram-simple"}, notes: 1},
		{name: "--spec_type=… dans EXTRA_ARGS : Loki se tait",
			cfg: cfgOf("SPEC", "ngram"), extra: []string{"--spec_type=ngram-mod"}, notes: 1},
		{name: "--spec-default dans EXTRA_ARGS : Loki se tait",
			cfg: cfgOf("SPEC", "mtp+ngram"), extra: []string{"--spec-default"}, notes: 1},
		{name: "--spec-ngram-mod-n-max dans EXTRA_ARGS : Loki se tait",
			cfg: cfgOf("SPEC", "ngram"), extra: []string{"--spec-ngram-mod-n-max=16"}, notes: 1},
		{name: "-md dans EXTRA_ARGS : Loki se tait",
			cfg: cfgOf("SPEC", "mtp+ngram"), extra: []string{"-md", "d.gguf"}, notes: 1},
		{name: "LLAMA_ARG_SPEC_TYPE posée : Loki se tait", cfg: cfgOf("SPEC", "ngram"), notes: 1,
			si: func(s *serveSysInfo) { s.ArgEnv["LLAMA_ARG_SPEC_TYPE"] = "ngram-mod" }},
		{name: "ngram + --spec-draft-n-max à la main : rien",
			cfg: cfgOf("SPEC", "ngram"), si: dense, extra: []string{"--spec-draft-n-max", "8"}, notes: 1},
		{name: "mtp+ngram + --spec-draft-n-max à la main : la règle de SPEC=mtp",
			cfg: cfgOf("SPEC", "mtp+ngram"), extra: []string{"--spec-draft-n-max", "8"}, want: both, notes: 2},
		{name: "experts sur CPU, seuil par défaut : refusé, OP_OFFLOAD_MIN_BATCH suggéré",
			cfg: cfgOf("SPEC", "ngram"), si: dense, extra: []string{"--n-cpu-moe", "40"}, notes: 1},
		{name: "experts sur CPU, mtp+ngram : refusé aussi",
			cfg: cfgOf("SPEC", "mtp+ngram"), extra: []string{"-ot", "exps=CPU"}, notes: 1},
		{name: "experts sur CPU, OP_OFFLOAD_MIN_BATCH=64 : encore sous le lot de 65, refusé",
			cfg: cfgOf("SPEC", "ngram", "OP_OFFLOAD_MIN_BATCH", "64"), si: dense, extra: []string{"-cmoe"}, notes: 1},
		{name: "experts sur CPU, OP_OFFLOAD_MIN_BATCH=128 : accepté",
			cfg: cfgOf("SPEC", "ngram", "OP_OFFLOAD_MIN_BATCH", "128"), si: dense, extra: []string{"-cmoe"}, want: ngram, notes: 1},
		{name: "experts sur CPU, GGML_OP_OFFLOAD_MIN_BATCH=256 dans l'environnement : accepté",
			cfg: cfgOf("SPEC", "ngram"), extra: []string{"--n-cpu-moe", "40"}, want: ngram, notes: 1,
			si: func(s *serveSysInfo) {
				dense(s)
				s.UserEnv = map[string]string{"GGML_OP_OFFLOAD_MIN_BATCH": "256"}
			}},
		{name: "experts sur CPU, modèle plus gros que la RAM : accepté avec l'avertissement",
			cfg: cfgOf("SPEC", "ngram", "OP_OFFLOAD_MIN_BATCH", "128"), extra: []string{"--n-cpu-moe", "40"},
			want: ngram, notes: 2,
			si: func(s *serveSysInfo) {
				dense(s)
				s.ModelBytes, s.RAMMiB = 82<<30, 64<<10
			}},
		{name: "NGL imposé : logits en VRAM dits",
			cfg: cfgOf("SPEC", "ngram", "NGL", "40"), si: dense, want: ngram, notes: 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			si := mtpReady()
			si.Help = helpNgram
			if c.si != nil {
				c.si(&si)
			}
			got, notes, isAuto := specArgs(c.cfg, c.extra, si)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("args = %q, attendu %q", got, c.want)
			}
			if isAuto {
				t.Error("ngram n'est jamais automatique")
			}
			if len(notes) != c.notes {
				t.Errorf("%d notes, attendu %d : %q", len(notes), c.notes, notes)
			}
			for _, a := range got {
				if a == "--spec-default" || strings.HasPrefix(a, "--spec-synth") {
					t.Errorf("drapeau interdit %q", a)
				}
			}
		})
	}
}

// Sans SPEC, la même ligne qu'avant, quel que soit le moteur.
func TestBuildServeArgsNgramDefault(t *testing.T) {
	si := mtpReady()
	si.Help = helpNgram
	base, _, _ := buildServeArgs(map[string]string{}, nil, "/bin/llama-server", si)
	for _, a := range base {
		if strings.HasPrefix(a, "--spec") {
			t.Fatalf("SPEC absent : aucun drapeau de spéculation, %q", base)
		}
	}
	got, _, _ := buildServeArgs(map[string]string{"SPEC": "ngram"}, []string{"--jinja"}, "/bin/llama-server", si)
	i := slices.Index(got, "--spec-type")
	if i < 0 || got[i+1] != "ngram-mod" || slices.Index(got, "--jinja") < i {
		t.Fatalf("SPEC=ngram : %q", got)
	}
}

func TestSynthAcceptanceWarned(t *testing.T) {
	for _, extra := range [][]string{{"--spec-synth-len", "3"}, {"--spec-synth-rates=0.5,0.5"}} {
		if n := lossyCacheNotes(extra, serveSysInfo{ArgEnv: map[string]string{}}); len(n) != 1 ||
			!strings.Contains(n[0], "AU HASARD") {
			t.Errorf("%q : %q", extra, n)
		}
	}
	si := serveSysInfo{ArgEnv: map[string]string{"LLAMA_ARG_SPEC_SYNTH_LEN": "3"}}
	if n := lossyCacheNotes(nil, si); len(n) != 1 {
		t.Errorf("variable : %q", n)
	}
}
