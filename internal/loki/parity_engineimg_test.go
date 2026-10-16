//go:build linux

package loki

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// parity_engineimg_test.go — journal de référence du panneau « moteur de
// l'image » (lot 24 de la migration Rust) : statut (variante, build, avis,
// encart de version recommandée, versions installées, version précédente,
// vérification du rendu), dernier build publié, plan (dernière ou recommandée,
// risque de rendu du raisonnement), mise à jour depuis un faux registre OCI
// (couches du sommet, liens, couche du runtime ignorée, essai à blanc,
// REASONING_PRESERVE=off, ménage des versions, rendu comparé avant/après),
// bascules, suppression, retour à la version précédente, et les échecs.
// Rangé dans rust/loki-rs/tests/parity_engineimg/engineimg.json ;
// tests/engineimg.rs le rejoue par la façade.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteMoteurImage

const parityEngineImgFile = "../../rust/loki-rs/tests/parity_engineimg/engineimg.json"

// Ce que sert le faux registre : le jeton, la version annoncée par les tags,
// les tags figés qui existent, et les couches (de la base au sommet).
type parityRegistry struct {
	Token int `json:"token,omitempty"` // statut de /token (0 = 200)
	Blob  int `json:"blob,omitempty"`  // statut imposé aux couches (0 = servies)
	// Corps de /token servi à la place du jeton ; statut et corps imposés
	// aux manifestes ; index sans image pour cette architecture.
	TokenBody    string              `json:"token_body,omitempty"`
	Manifest     int                 `json:"manifest,omitempty"`
	ManifestBody string              `json:"manifest_body,omitempty"`
	NoArch       bool                `json:"no_arch,omitempty"`
	Version      string              `json:"version"`
	Fixed        []string            `json:"fixed,omitempty"`
	Layers       []parityRegistryLay `json:"layers"`
}

type parityRegistryLay struct {
	Size    int64             `json:"size,omitempty"` // taille annoncée (0 = réelle)
	Entries []parityArchEntry `json:"entries"`
}

// Le moteur local (sonde de gabarit) : gabarit, build, ou arrêté.
type parityLocal struct {
	Mode     string `json:"mode"`
	Build    string `json:"build"`
	Template string `json:"template,omitempty"`
	Down     bool   `json:"down,omitempty"`
	Apply    int    `json:"apply,omitempty"` // statut imposé à /apply-template (0 = rendu)
}

type parityEngineImgStep struct {
	Name       string            `json:"name"`
	Files      map[string]string `json:"files,omitempty"`
	Config     map[string]string `json:"config,omitempty"`
	Registry   *parityRegistry   `json:"registry,omitempty"`
	Local      *parityLocal      `json:"local,omitempty"`
	Tune       bool              `json:"tune,omitempty"`
	EngineEnv  *string           `json:"engine_env,omitempty"` // LOKI_ENGINE_BIN
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	Body       string            `json:"body,omitempty"`
	Wait       bool              `json:"wait,omitempty"`
	AfterJob   *parityLocal      `json:"after_job,omitempty"` // le moteur local une fois le job fini
	WaitRender bool              `json:"wait_render,omitempty"`
	Status     int               `json:"status"`
	Out        any               `json:"out"`
	Job        any               `json:"job,omitempty"`
	Render     any               `json:"render,omitempty"`
	After      map[string]string `json:"after"`
	Tree       []string          `json:"tree"`
}

// Les scripts des moteurs.
func parityEngineScript(version, devices, help string, failList bool) string {
	s := "#!/bin/sh\ncase \"$1\" in\n--version) echo '" + version + "';;\n--list-devices)"
	if failList {
		s += " i=0; while [ $i -lt 13 ]; do echo \"trace $i\"; i=$((i+1)); done; echo 'erreur: libcudart.so.13 introuvable'; exit 1;;\n"
	} else {
		s += " echo 'Available devices:'" + devices + ";;\n"
	}
	return s + "--help) echo '" + help + "';;\nesac\n"
}

func parityEngineLayers(version, script string, withServer bool, runtimeSize int64) []parityRegistryLay {
	// Le serveur en 0700 : l'installation le rend exécutable par tous.
	server := []parityArchEntry{{Name: "app/llama", Type: "file", Body: "#!/bin/true\n", Mode: 0o755}}
	libs := []parityArchEntry{}
	if withServer {
		server = append(server, parityArchEntry{Name: "app/llama-server", Type: "file", Body: script, Mode: 0o700})
		// Masqué par la couche du dessus : jamais extrait.
		libs = append(libs, parityArchEntry{Name: "app/llama-server", Type: "file", Body: "ancien", Mode: 0o755})
	}
	return []parityRegistryLay{
		{Size: 2 << 30, Entries: []parityArchEntry{{Name: "usr/local/cuda/lib64/libcudart.so", Type: "file", Body: "runtime", Mode: 0o644}}},
		{Entries: []parityArchEntry{{Name: "etc/hosts", Type: "file", Body: "x", Mode: 0o644}}},
		{Entries: append(libs, []parityArchEntry{
			{Name: "app/", Type: "dir", Mode: 0o755},
			{Name: "app/libggml-base.so.0.20.0", Type: "file", Body: "elf " + version, Mode: 0o644},
			{Name: "app/libggml-base.so", Type: "symlink", Link: "libggml-base.so.0.20.0"},
			{Name: "app/libllama.so.0.1.0", Type: "file", Body: "elf", Mode: 0o644},
			{Name: "app/libllama.so", Type: "symlink", Link: "/app/libllama.so.0.1.0"},
			{Name: "app/libggml-cuda.so", Type: "file", Body: "elf", Mode: 0o644},
			{Name: "app/sous/ignoré", Type: "file", Body: "x", Mode: 0o644},
			{Name: "app/.wh.supprimé", Type: "file", Body: "", Mode: 0o644},
			{Name: "etc/passwd", Type: "file", Body: "x", Mode: 0o644},
		}...)},
		{Size: runtimeSize, Entries: server},
	}
}

var parityAtRe = regexp.MustCompile(`"at":"[^"]*"`)

func TestPariteMoteurImage(t *testing.T) {
	home := testHome(t)
	freshTplProbe(t)
	resetBackendsState()
	t.Cleanup(resetBackendsState)
	oldPoll, oldWait := engineRenderPoll, engineRenderWait
	engineRenderPoll, engineRenderWait = time.Millisecond, 10*time.Second
	reset := func() {
		engineRenderMu.Lock()
		engineRenderLast = nil
		engineRenderGen++
		engineRenderMu.Unlock()
		buildMu.Lock()
		buildCache = map[string]int{}
		buildMu.Unlock()
	}
	reset()
	t.Cleanup(func() {
		engineRenderPoll, engineRenderWait = oldPoll, oldWait
		reset()
	})

	// Le faux registre.
	var mu sync.Mutex
	var reg parityRegistry
	type built struct {
		layers   map[string][]byte
		manifest []byte
		manDig   string
		index    []byte
	}
	build := func(r parityRegistry) built {
		b := built{layers: map[string][]byte{}}
		var layers []map[string]any
		for _, l := range r.Layers {
			data := parityTarGz(t, l.Entries)
			sum := sha256.Sum256(data)
			d := "sha256:" + hex.EncodeToString(sum[:])
			b.layers[d] = data
			size := int64(len(data))
			if l.Size > 0 {
				size = l.Size
			}
			layers = append(layers, map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": d, "size": size})
		}
		b.manifest, _ = json.Marshal(map[string]any{"schemaVersion": 2, "layers": layers,
			"annotations": map[string]string{"org.opencontainers.image.version": r.Version}})
		sum := sha256.Sum256(b.manifest)
		b.manDig = "sha256:" + hex.EncodeToString(sum[:])
		arch := ociArch()
		if r.NoArch {
			arch = "s390x"
		}
		b.index, _ = json.Marshal(map[string]any{"schemaVersion": 2,
			"manifests": []map[string]any{
				{"digest": "sha256:autre", "platform": map[string]string{"os": "linux", "architecture": "ppc64le"}},
				{"digest": b.manDig, "platform": map[string]string{"os": "linux", "architecture": arch}},
			}})
		return b
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cur := reg
		mu.Unlock()
		b := build(cur)
		switch {
		case r.URL.Path == "/token":
			if cur.Token != 0 {
				w.WriteHeader(cur.Token)
				return
			}
			if cur.TokenBody != "" {
				_, _ = w.Write([]byte(cur.TokenBody))
				return
			}
			_, _ = w.Write([]byte(`{"token":"jeton"}`))
		case strings.Contains(r.URL.Path, "/manifests/"):
			if r.Header.Get("Authorization") != "Bearer jeton" {
				w.WriteHeader(401)
				return
			}
			if cur.Manifest != 0 {
				w.WriteHeader(cur.Manifest)
				return
			}
			if cur.ManifestBody != "" {
				_, _ = w.Write([]byte(cur.ManifestBody))
				return
			}
			ref := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			switch {
			case ref == b.manDig:
				_, _ = w.Write(b.manifest)
			case ref == "server-cuda":
				_, _ = w.Write(b.index)
			default:
				for _, f := range cur.Fixed {
					if f == ref {
						_, _ = w.Write(b.index)
						return
					}
				}
				w.WriteHeader(404)
			}
		case strings.Contains(r.URL.Path, "/blobs/"):
			if cur.Blob != 0 {
				w.WriteHeader(cur.Blob)
				return
			}
			data, ok := b.layers[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]
			if !ok {
				w.WriteHeader(404)
				return
			}
			_, _ = w.Write(data)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	oldBase := ociBase
	ociBase = srv.URL
	defer func() { ociBase = oldBase }()
	regHost := strings.TrimPrefix(srv.URL, "http://")

	// Le moteur local, pour la sonde de gabarit.
	local := &fakeTpl{mode: "qwen3", build: "b10450-img"}
	down := false
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		local.mu.Lock()
		d := down
		local.mu.Unlock()
		if d {
			w.WriteHeader(503)
			return
		}
		local.handler(w, r)
	}))
	defer eng.Close()
	u, _ := url.Parse(eng.URL)
	if err := SetConfigKey("PORT", u.Port()); err != nil {
		t.Fatal(err)
	}
	setLocal := func(l *parityLocal) {
		local.mu.Lock()
		local.mode, local.build, local.tpl = l.Mode, l.Build, l.Template
		local.applyStatus, local.applyBody = nil, ""
		if l.Apply != 0 {
			for range 1000 {
				local.applyStatus = append(local.applyStatus, l.Apply)
			}
			local.applyBody = `{"error":{"code":500,"message":"Jinja Exception: gabarit cassé"}}`
		}
		down = l.Down
		local.mu.Unlock()
	}

	img := filepath.Join(home, "image")
	t.Setenv("LOKI_ENGINE_BIN", filepath.Join(img, "llama-server"))
	dev1 := "; echo '  CUDA0: Carte (1000 MiB, 900 MiB free)'"
	mask := func(s string) string {
		s = strings.ReplaceAll(s, srv.URL, "{{REG}}")
		s = strings.ReplaceAll(s, regHost, "{{REGHOST}}")
		s = strings.ReplaceAll(s, home, "{{HOME}}")
		s = parityTSRe.ReplaceAllString(s, `"$1":"{{T}}"`)
		return parityAtRe.ReplaceAllString(s, `"at":"{{T}}"`)
	}
	unmask := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, "{{HOME}}", home), "{{PORT}}", u.Port())
	}
	handlers := map[string]http.HandlerFunc{
		"/api/engine":          handleEngineStatus,
		"/api/engine/check":    handleEngineCheck,
		"/api/engine/update":   handleEngineUpdate,
		"/api/engine/use":      handleEngineUse,
		"/api/engine/remove":   handleEngineRemove,
		"/api/engine/plan":     handleEnginePlan,
		"/api/engine/rollback": handleEngineRollback,
	}
	call := func(method, path, body string) (int, any) {
		rec := httptest.NewRecorder()
		handlers[path](rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		var out any
		if err := json.Unmarshal([]byte(mask(rec.Body.String())), &out); err != nil {
			t.Fatalf("%s : corps illisible %q", path, rec.Body.String())
		}
		return rec.Code, out
	}
	run := func(st parityEngineImgStep) parityEngineImgStep {
		files := map[string]string{}
		for k, v := range st.Files {
			files[k] = unmask(v)
		}
		parityApplyFiles(t, home, files)
		for k, v := range st.Config {
			if err := SetConfigKey(k, unmask(v)); err != nil {
				t.Fatal(err)
			}
		}
		if st.Registry != nil {
			mu.Lock()
			reg = *st.Registry
			mu.Unlock()
		}
		if st.Local != nil {
			setLocal(st.Local)
		}
		if st.EngineEnv != nil {
			t.Setenv("LOKI_ENGINE_BIN", unmask(*st.EngineEnv))
		}
		var lock *tuneLock
		if st.Tune {
			l, err := tuneLockAcquire("web", false)
			if err != nil {
				t.Fatal(err)
			}
			lock = l
		}
		st.Status, st.Out = call(st.Method, st.Path, st.Body)
		if lock != nil {
			lock.release()
		}
		if st.Wait {
			deadline := time.Now().Add(30 * time.Second)
			for lcJobSnapshot(0, false)["running"] == true {
				if time.Now().After(deadline) {
					t.Fatalf("%s : le job ne finit pas", st.Name)
				}
				time.Sleep(10 * time.Millisecond)
			}
			b, _ := json.Marshal(lcJobSnapshot(0, true))
			_ = json.Unmarshal([]byte(mask(string(b))), &st.Job)
			if st.AfterJob != nil {
				setLocal(st.AfterJob)
			}
		}
		if st.WaitRender {
			deadline := time.Now().Add(15 * time.Second)
			for {
				c := engineRenderSnapshot()
				if c != nil && c.Status != "pending" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s : la vérification du rendu ne finit pas", st.Name)
				}
				time.Sleep(10 * time.Millisecond)
			}
			_, out := call("GET", "/api/engine", "")
			st.Render = out.(map[string]any)["render_check"]
		}
		st.After = map[string]string{}
		for _, k := range []string{"BIN", "REASONING_PRESERVE"} {
			if v := ReadConfig()[k]; v != "" {
				st.After["config/"+k] = mask(v)
			}
		}
		if b, err := os.ReadFile(filepath.Join(home, "engine", "previous-bin")); err == nil {
			st.After["previous-bin"] = mask(string(b))
		}
		st.Tree = parityTree(t, home)
		for i := range st.Tree {
			st.Tree[i] = mask(st.Tree[i])
		}
		return st
	}

	v10900 := "version: 0.1.0-dev (build 10900, commit def5678)"
	good := parityEngineScript(v10900, dev1, "--no-reasoning-preserve", false)
	reg10900 := &parityRegistry{Version: "b10900", Fixed: []string{"server-cuda-b10900"},
		Layers: parityEngineLayers("b10900", good, true, 0)}
	qwenTpl := "{% if preserve_thinking %}garde{% endif %}"

	noImage := ""
	var steps []parityEngineImgStep
	for _, st := range []parityEngineImgStep{
		{Name: "statut, moteur de l'image", Files: map[string]string{
			"image/llama-server":    parityEngineScript("version: 10450 (abc1234)", dev1, "usage", false),
			"image/libggml-cuda.so": "elf",
		}, Registry: reg10900, Local: &parityLocal{Mode: "qwen3", Build: "b10450-img", Template: qwenTpl},
			Method: "GET", Path: "/api/engine"},
		{Name: "dernière version publiée", Method: "POST", Path: "/api/engine/check"},
		{Name: "dernière version sans numéro", Registry: &parityRegistry{Layers: parityEngineLayers("x", good, true, 0)},
			Method: "POST", Path: "/api/engine/check"},
		{Name: "jeton illisible", Registry: &parityRegistry{TokenBody: `{"token":""}`, Version: "b10900"}, Method: "POST", Path: "/api/engine/check"},
		{Name: "manifeste refusé", Registry: &parityRegistry{Manifest: 403, Version: "b10900"}, Method: "POST", Path: "/api/engine/check"},
		{Name: "manifeste illisible", Registry: &parityRegistry{ManifestBody: `{"mediaType":`, Version: "b10900"}, Method: "POST", Path: "/api/engine/check"},
		{Name: "aucune image pour cette architecture", Registry: &parityRegistry{NoArch: true, Version: "b10900", Layers: parityEngineLayers("b10900", good, true, 0)},
			Method: "POST", Path: "/api/engine/check"},
		{Name: "jeton refusé", Registry: &parityRegistry{Token: 500, Version: "b10900"}, Method: "POST", Path: "/api/engine/check"},
		{Name: "plan : dernière version", Registry: reg10900, Method: "POST", Path: "/api/engine/plan", Body: `{}`},
		{Name: "plan : version recommandée", Method: "POST", Path: "/api/engine/plan", Body: `{"recommended":true}`},
		{Name: "plan recommandé, build trop ancien", Registry: &parityRegistry{Version: "b10800", Fixed: []string{"server-cuda-b10800"},
			Layers: parityEngineLayers("b10800", good, true, 0)}, Method: "POST", Path: "/api/engine/plan", Body: `{"recommended":true}`},
		{Name: "plan recommandé, tag figé absent", Registry: &parityRegistry{Version: "b10900", Layers: parityEngineLayers("b10900", good, true, 0)},
			Method: "POST", Path: "/api/engine/plan", Body: `{"recommended":true}`},
		{Name: "plan recommandé, sans numéro", Registry: &parityRegistry{Version: "", Layers: parityEngineLayers("x", good, true, 0)},
			Method: "POST", Path: "/api/engine/plan", Body: `{"recommended":true}`},
		{Name: "plan, dernière version sans numéro", Method: "POST", Path: "/api/engine/plan", Body: `{}`},
		{Name: "plan en GET", Method: "GET", Path: "/api/engine/plan"},
		{Name: "plan, moteur local arrêté", Registry: reg10900, Local: &parityLocal{Down: true},
			Method: "POST", Path: "/api/engine/plan", Body: `{}`},
		{Name: "plan, REASONING_PRESERVE posée", Local: &parityLocal{Mode: "qwen3", Build: "b10450-img", Template: qwenTpl},
			Config: map[string]string{"REASONING_PRESERVE": "on"}, Method: "POST", Path: "/api/engine/plan", Body: `{}`},
		{Name: "plan, EXTRA_ARGS", Config: map[string]string{"REASONING_PRESERVE": "", "EXTRA_ARGS": "--reasoning-preserve"},
			Method: "POST", Path: "/api/engine/plan", Body: `{}`},
		{Name: "plan, gabarit sans les variables", Config: map[string]string{"EXTRA_ARGS": ""},
			Local: &parityLocal{Mode: "qwen3", Build: "b10450-img", Template: "simple"}, Method: "POST", Path: "/api/engine/plan", Body: `{}`},
		{Name: "plan, gabarit qui garde tout", Local: &parityLocal{Mode: "keep", Build: "b10451-img", Template: qwenTpl},
			Method: "POST", Path: "/api/engine/plan", Body: `{}`},
		{Name: "plan, gabarit vide", Local: &parityLocal{Build: "b10452-img"},
			Method: "POST", Path: "/api/engine/plan", Body: `{}`},
		{Name: "plan, build antérieur au défaut", Registry: &parityRegistry{Version: "b10700", Fixed: []string{"server-cuda-b10700"},
			Layers: parityEngineLayers("b10700", good, true, 0)}, Local: &parityLocal{Mode: "qwen3", Build: "b10450-img", Template: qwenTpl},
			Method: "POST", Path: "/api/engine/plan", Body: `{}`},
		{Name: "plan, preset externe", Registry: reg10900, Config: map[string]string{"EXTERNAL": "1"},
			Method: "POST", Path: "/api/engine/plan", Body: `{}`},
		{Name: "mise à jour pendant une optimisation", Config: map[string]string{"EXTERNAL": ""}, Tune: true, Method: "POST", Path: "/api/engine/update", Body: `{}`},
		{Name: "mise à jour, moteur local arrêté", Registry: &parityRegistry{Version: "b10920", Fixed: []string{"server-cuda-b10920"},
			Layers: parityEngineLayers("b10920", good, true, 0)}, Local: &parityLocal{Down: true},
			Method: "POST", Path: "/api/engine/update", Body: `{}`, Wait: true},
		{Name: "retour au moteur de l'image", Method: "POST", Path: "/api/engine/use", Body: `{"tag":"image"}`},
		{Name: "suppression de la version d'essai", Method: "POST", Path: "/api/engine/remove", Body: `{"tag":"server-cuda-b10920"}`},
		{Name: "mise à jour", Registry: reg10900, Local: &parityLocal{Mode: "qwen3", Build: "b10450-img", Template: qwenTpl},
			Method: "POST", Path: "/api/engine/update", Body: `{}`, Wait: true,
			AfterJob: &parityLocal{Mode: "keep", Build: "b10900-new", Template: qwenTpl}, WaitRender: true},
		{Name: "statut après la mise à jour", Method: "GET", Path: "/api/engine"},
		{Name: "plan, déjà à jour", Method: "POST", Path: "/api/engine/plan", Body: `{}`},
		{Name: "déjà à jour", Method: "POST", Path: "/api/engine/update", Body: `{}`, Wait: true},
		{Name: "retour à la version précédente", Method: "POST", Path: "/api/engine/rollback"},
		{Name: "statut après le retour", Method: "GET", Path: "/api/engine"},
		{Name: "retour en GET", Method: "GET", Path: "/api/engine/rollback"},
		{Name: "retour pendant une optimisation", Tune: true, Method: "POST", Path: "/api/engine/rollback"},
		{Name: "bascule vers la version installée", Method: "POST", Path: "/api/engine/use", Body: `{"tag":" server-cuda-b10900 "}`},
		{Name: "bascule vers l'image", Method: "POST", Path: "/api/engine/use", Body: `{"tag":"image"}`},
		{Name: "bascule vers une version absente", Method: "POST", Path: "/api/engine/use", Body: `{"tag":"server-cuda-b1"}`},
		{Name: "bascule pendant une optimisation", Tune: true, Method: "POST", Path: "/api/engine/use", Body: `{"tag":"image"}`},
		{Name: "suppression d'une version", Method: "POST", Path: "/api/engine/remove", Body: `{"tag":"../server-cuda-b10900"}`},
		{Name: "suppression d'une version absente", Method: "POST", Path: "/api/engine/remove", Body: `{"tag":"server-cuda-b10900"}`},
		{Name: "mise à jour épinglée, REASONING_PRESERVE=off dans le preset actif",
			Files: map[string]string{"presets/actif.env": "PORT={{PORT}}\nBIN={{HOME}}/image/llama-server\n"}, Local: &parityLocal{Mode: "qwen3", Build: "b10450-img", Template: qwenTpl},
			Method: "POST", Path: "/api/engine/update", Body: `{"tag":"server-cuda-b10900","preserve_off":true}`, Wait: true,
			AfterJob: &parityLocal{Mode: "qwen3", Build: "b10900-new", Template: qwenTpl}, WaitRender: true},
		{Name: "suppression du moteur utilisé", Method: "POST", Path: "/api/engine/remove", Body: `{"tag":"server-cuda-b10900"}`},
		{Name: "REASONING_PRESERVE déjà posée", Method: "POST", Path: "/api/engine/update",
			Body: `{"tag":"server-cuda-b10900","preserve_off":true}`, Wait: true,
			AfterJob: &parityLocal{Mode: "drop", Build: "b10900-bis", Template: qwenTpl}, WaitRender: true},
		{Name: "REASONING_PRESERVE=off posée dans la configuration", Files: map[string]string{"presets/actif.env": "-"},
			Config: map[string]string{"REASONING_PRESERVE": ""}, Method: "POST", Path: "/api/engine/update",
			Body: `{"tag":"server-cuda-b10900","preserve_off":true}`, Wait: true,
			AfterJob: &parityLocal{Mode: "qwen3", Build: "b10900-ter", Template: qwenTpl, Apply: 500}, WaitRender: true},
		{Name: "nouvelle version, ménage", Files: map[string]string{"engine/server-cuda-b10000/llama-server": "#!/bin/sh\n",
			"engine/server-cuda-b9999/llama-server": "#!/bin/sh\n", "engine/server-cuda-b10001": "/"},
			Config:   map[string]string{"REASONING_PRESERVE": ""},
			Registry: &parityRegistry{Version: "b10950", Fixed: []string{"server-cuda-b10950"}, Layers: parityEngineLayers("b10950", parityEngineScript("version: 0.1.0-dev (build 10950, commit 0ab1234)", dev1, "", false), true, 10<<20)},
			Local:    &parityLocal{Down: true}, Method: "POST", Path: "/api/engine/update", Body: `{}`, Wait: true},
		{Name: "statut, version précédente téléchargée", Method: "GET", Path: "/api/engine"},
		{Name: "REASONING_PRESERVE=off inconnue du moteur", Method: "POST", Path: "/api/engine/update",
			Body: `{"tag":"server-cuda-b10950","preserve_off":true}`, Wait: true},
		{Name: "version inconnue du registre", Method: "POST", Path: "/api/engine/update", Body: `{"tag":"server-cuda-b1"}`, Wait: true},
		{Name: "image sans serveur", Registry: &parityRegistry{Version: "b10960", Fixed: []string{"server-cuda-b10960"},
			Layers: parityEngineLayers("b10960", good, false, 0)}, Method: "POST", Path: "/api/engine/update", Body: `{"tag":"server-cuda-b10960"}`, Wait: true},
		{Name: "manifeste sans couche", Registry: &parityRegistry{Version: "b10966", Fixed: []string{"server-cuda-b10966"}},
			Method: "POST", Path: "/api/engine/update", Body: `{"tag":"server-cuda-b10966"}`, Wait: true},
		{Name: "image sans version annoncée", Registry: &parityRegistry{Fixed: []string{"server-cuda-b10967"},
			Layers: parityEngineLayers("b10967", good, false, 0)}, Method: "POST", Path: "/api/engine/update", Body: `{"tag":"server-cuda-b10967"}`, Wait: true},
		{Name: "couche du runtime au sommet", Registry: &parityRegistry{Version: "b10961", Fixed: []string{"server-cuda-b10961"},
			Layers: parityEngineLayers("b10961", good, true, 700<<20)}, Method: "POST", Path: "/api/engine/update", Body: `{"tag":"server-cuda-b10961"}`, Wait: true},
		{Name: "moteur qui ne démarre pas", Registry: &parityRegistry{Version: "b10962", Fixed: []string{"server-cuda-b10962"},
			Layers: parityEngineLayers("b10962", parityEngineScript("version: 10962 (aaa)", dev1, "", true), true, 0)},
			Method: "POST", Path: "/api/engine/update", Body: `{"tag":"server-cuda-b10962"}`, Wait: true},
		{Name: "moteur qui ne voit aucune carte", Registry: &parityRegistry{Version: "b10963", Fixed: []string{"server-cuda-b10963"},
			Layers: parityEngineLayers("b10963", parityEngineScript("version: 10963 (bbb)", "", "", false), true, 0)},
			Method: "POST", Path: "/api/engine/update", Body: `{"tag":"server-cuda-b10963"}`, Wait: true},
		{Name: "jeton refusé pendant la mise à jour", Registry: &parityRegistry{Token: 401, Version: "b10964"},
			Method: "POST", Path: "/api/engine/update", Body: `{"tag":"server-cuda-b10964"}`, Wait: true},
		{Name: "couche introuvable", Registry: &parityRegistry{Blob: 404, Version: "b10965", Fixed: []string{"server-cuda-b10965"},
			Layers: parityEngineLayers("b10965", good, true, 0)}, Method: "POST", Path: "/api/engine/update", Body: `{"tag":"server-cuda-b10965"}`, Wait: true},
		{Name: "tag illisible", Method: "POST", Path: "/api/engine/update", Body: `{"tag":"."}`, Wait: true},
		{Name: "corps illisible, preset externe", Registry: reg10900, Config: map[string]string{"EXTERNAL": "1"},
			Local: &parityLocal{Mode: "qwen3", Build: "b10900-ext", Template: qwenTpl}, Method: "POST", Path: "/api/engine/update", Body: `{"tag":1}`, Wait: true},
		{Name: "statut, moteur compilé", Files: map[string]string{
			"custom/llama-server": parityEngineScript("version: 10800 (c0ffee1)", dev1, "", false),
		}, Config: map[string]string{"BIN": "{{HOME}}/custom/llama-server"}, Method: "GET", Path: "/api/engine"},
		{Name: "bascule vers l'image, sans moteur d'image", EngineEnv: &noImage, Method: "POST", Path: "/api/engine/use", Body: `{"tag":"image"}`},
		{Name: "statut final", Method: "GET", Path: "/api/engine"},
		{Name: "statut sans aucun moteur", Config: map[string]string{"BIN": ""}, Method: "GET", Path: "/api/engine"},
		{Name: "statut, version précédente = moteur courant", Files: map[string]string{
			"engine/previous-bin": "{{HOME}}/engine/server-cuda-b10900/llama-server\n",
		}, Config: map[string]string{"BIN": "{{HOME}}/engine/server-cuda-b10900/llama-server"}, Method: "GET", Path: "/api/engine"},
	} {
		steps = append(steps, run(st))
	}

	got, err := parityJSON(map[string]any{"goarch": ociArch(), "steps": steps})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityEngineImgFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityEngineImgFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityEngineImgFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("le panneau du moteur de l'image ne produit plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteMoteurImage,\n"+
			"puis cargo test --test engineimg dit si le port Rust suit.\n--- obtenu ---\n%s", parityEngineImgFile, got)
	}
}
