//go:build linux

package loki

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// parity_backends_test.go — journal de référence des backends llama.cpp
// (lot 22 de la migration Rust) : statut (dépôt git, binaire, moteur de
// l'image, précompilé, moteur téléchargé, plan de build et conseil selon la
// machine), vérification git, backends custom et leur désinstallation,
// bascule fast/opt, vérification de la dernière release, job des binaires
// précompilés (téléchargement, extraction avec liens, ménage, marqueur
// VERSION, échecs), GPU vus par un moteur (caches mémoire et disque), puis le
// job lui-même pas à pas et sa reprise au démarrage. Les machines sont de faux
// outils dans le PATH (nvidia-smi, nvcc, hipcc, glslc, vulkaninfo, ldconfig) ;
// GitHub est un faux serveur. Rangé dans
// rust/loki-rs/tests/parity_backends/backends.json ; tests/backends.rs le
// rejoue par la façade.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteBackends

const parityBackendsFile = "../../rust/loki-rs/tests/parity_backends/backends.json"

// Une entrée d'archive servie par le faux GitHub.
type parityArchEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // file | dir | symlink | hardlink
	Body string `json:"body,omitempty"`
	Link string `json:"link,omitempty"`
	Mode int64  `json:"mode,omitempty"`
}

// Ce que sert le faux GitHub : la release (« {{GH}} » = son adresse) et les
// archives par nom.
type parityRelease struct {
	Status int                          `json:"status"`
	JSON   string                       `json:"json"`
	Assets map[string][]parityArchEntry `json:"assets,omitempty"`
}

type parityBackendsStep struct {
	Name    string            `json:"name"`
	Machine string            `json:"machine,omitempty"` // outils du PATH (machines)
	Git     []string          `json:"git,omitempty"`     // avant les fichiers : init <d> | commit <d> <date> <msg> | clone <src> <dst> | pull <d> | detach <d> | remote-bad <d>
	Files   map[string]string `json:"files,omitempty"`   // sous LOKI_HOME : contenu ; « #! » exécutable ; « -> x » lien ; « - » supprimé ; « / » dossier
	Config  map[string]string `json:"config,omitempty"`  // "" = supprimé
	Engine  *string           `json:"engine_env,omitempty"`
	Release *parityRelease    `json:"release,omitempty"`
	Tune    bool              `json:"tune,omitempty"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Body    string            `json:"body,omitempty"`
	Wait    bool              `json:"wait,omitempty"` // job lancé : attendre sa fin, relire /api/llamacpp/job
	Status  int               `json:"status"`
	Out     any               `json:"out"`
	Job     any               `json:"job,omitempty"`
	After   map[string]string `json:"after"`
	Tree    []string          `json:"tree"`
}

// Les machines : nom → outil → script.
var parityMachines = map[string]map[string]string{
	"nu": {},
	"nvidia": {
		"nvidia-smi": "#!/bin/sh\ncase \"$1\" in\n-L) echo 'GPU 0: NVIDIA GeForce RTX 3090 (UUID: GPU-1)';;\n--query-gpu=compute_cap) printf '8.6\\n8.9\\n8.6\\n';;\n'') echo '| NVIDIA-SMI 570.00   Driver Version: 570.00   CUDA Version: 12.8 |';;\n--query-gpu=*) printf '0, NVIDIA GeForce RTX 3090, 24576, 100, 8.6, 00000000:01:00.0, 1, 4, 16, 16, 9751\\n1, Carte B, 8192, 5, 7.5, [N/A], [N/A], 3, 8, 16, 7001\\n';;\nesac\n",
		"nvcc":       "#!/bin/sh\nexit 0\n",
	},
	"nvidia-sans-nvcc": {
		"nvidia-smi": "#!/bin/sh\ncase \"$1\" in\n-L) echo 'GPU 0: NVIDIA (UUID: GPU-1)';;\n--query-gpu=compute_cap) echo '12.0';;\nesac\n",
	},
	"nvidia-muet": {
		"nvidia-smi": "#!/bin/sh\ncase \"$1\" in\n-L) echo 'rien';;\nesac\n",
		"nvcc":       "#!/bin/sh\nexit 0\n",
	},
	"nvidia-en-panne": {
		"nvidia-smi": "#!/bin/sh\nexit 9\n",
		"nvcc":       "#!/bin/sh\nexit 0\n",
	},
	"rocm": {
		"hipcc": "#!/bin/sh\nexit 0\n",
	},
	"glslc-ldconfig": {
		"glslc":    "#!/bin/sh\nexit 0\n",
		"ldconfig": "#!/bin/sh\necho '\tlibvulkan.so.1 (libc6,x86-64) => /usr/lib/libvulkan.so.1'\n",
	},
	"vulkan": {
		"glslc":      "#!/bin/sh\nexit 0\n",
		"vulkaninfo": "#!/bin/sh\nexit 0\n",
	},
	"nvidia-jumelles": {
		"nvidia-smi": "#!/bin/sh\ncase \"$1\" in\n-L) echo 'GPU 0: Carte B';;\n--query-gpu=*) printf '0, Carte B, 8192, 5, 7.5, [N/A], 1, 3, 8, 16, 7001\\n1, Carte B, 8192, 5, 7.5, [N/A], 1, 3, 8, 16, 7001\\n';;\nesac\n",
	},
	"liaison-partielle": {
		"nvidia-smi": "#!/bin/sh\ncase \"$1\" in\n--query-gpu=*) echo '0, Carte P, 4096, 1, 8.6, 00000000:02:00.0, [N/A], [N/A], 4, 8, 5001';;\nesac\n",
	},
	"vulkaninfo-seul": {
		"vulkaninfo": "#!/bin/sh\nexit 0\n",
	},
	"arch-ancienne": {
		"nvidia-smi": "#!/bin/sh\ncase \"$1\" in\n--query-gpu=index*pci.bus_id*) echo 'Field \"pci.bus_id\" is not a valid field to query.'; exit 2;;\n--query-gpu=*) echo '0, Vieille carte, 4096, 10, 6.1';;\n-L) echo 'GPU 0: Vieille carte';;\nesac\n",
	},
}

var (
	parityTSRe   = regexp.MustCompile(`"(started_at|ended_at)":[1-9][0-9]*`)
	parityJobsRe = regexp.MustCompile(`"jobs":[0-9]+`)
)

func TestPariteBackends(t *testing.T) {
	for _, p := range []string{"/usr/local/cuda", "/opt/rocm"} {
		if _, err := os.Stat(p); err == nil {
			t.Skipf("%s présent : la machine fausserait le journal", p)
		}
	}
	if m, _ := filepath.Glob("/usr/local/cuda-*"); len(m) > 0 {
		t.Skip("CUDA installé : la machine fausserait le journal")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git absent")
	}
	home := testHome(t)
	t.Setenv("LOKI_ENGINE_BIN", "")
	tools := t.TempDir()
	t.Setenv("PATH", tools)
	resetBackendsState()
	t.Cleanup(resetBackendsState)

	// Le faux GitHub.
	var mu sync.Mutex
	var rel parityRelease
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cur := rel
		mu.Unlock()
		if r.URL.Path == "/latest" {
			w.WriteHeader(cur.Status)
			_, _ = w.Write([]byte(strings.ReplaceAll(cur.JSON, "{{GH}}", "http://"+r.Host)))
			return
		}
		entries, ok := cur.Assets[strings.TrimPrefix(r.URL.Path, "/dl/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(parityTarGz(t, entries))
	}))
	defer gh.Close()
	old := llamaReleasesAPI
	llamaReleasesAPI = gh.URL + "/latest"
	defer func() { llamaReleasesAPI = old }()

	mask := func(s string) string {
		s = strings.ReplaceAll(s, gh.URL, "{{GH}}")
		s = strings.ReplaceAll(s, home, "{{HOME}}")
		s = parityJobsRe.ReplaceAllString(s, `"jobs":"{{JOBS}}"`)
		return parityTSRe.ReplaceAllString(s, `"$1":"{{T}}"`)
	}
	unmask := func(s string) string { return strings.ReplaceAll(s, "{{HOME}}", home) }
	handlers := map[string]http.HandlerFunc{
		"/api/llamacpp":                  handleLlamacpp,
		"/api/llamacpp/check":            handleLlamacppCheck,
		"/api/llamacpp/prebuilt/check":   handleLlamacppPrebuiltCheck,
		"/api/llamacpp/prebuilt":         handleLlamacppPrebuilt,
		"/api/llamacpp/use":              handleLlamacppUse,
		"/api/llamacpp/job":              handleLlamacppJob,
		"/api/llamacpp/uninstall-custom": handleLlamacppUninstallCustom,
		"/api/backends":                  handleBackends,
		"/api/backends/custom":           handleBackendsCustom,
		"/api/backends/devices":          handleBackendDevices,
	}
	call := func(method, path, body string) (int, any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		handlers[strings.SplitN(path, "?", 2)[0]](rec, req)
		var out any
		if err := json.Unmarshal([]byte(mask(rec.Body.String())), &out); err != nil {
			t.Fatalf("%s : corps illisible %q", path, rec.Body.String())
		}
		return rec.Code, out
	}
	machine := func(name string) {
		entries, _ := os.ReadDir(tools)
		for _, e := range entries {
			_ = os.Remove(filepath.Join(tools, e.Name()))
		}
		if err := os.Symlink(gitBin, filepath.Join(tools, "git")); err != nil {
			t.Fatal(err)
		}
		for tool, script := range parityMachines[name] {
			if err := os.WriteFile(filepath.Join(tools, tool), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	machine("nu")

	run := func(st parityBackendsStep) parityBackendsStep {
		if st.Machine != "" {
			machine(st.Machine)
		}
		for _, g := range st.Git {
			parityGit(t, gitBin, home, g)
		}
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
		if st.Engine != nil {
			t.Setenv("LOKI_ENGINE_BIN", unmask(*st.Engine))
		}
		if st.Release != nil {
			mu.Lock()
			rel = *st.Release
			mu.Unlock()
		}
		var lock *tuneLock
		if st.Tune {
			l, err := tuneLockAcquire("web", false)
			if err != nil {
				t.Fatal(err)
			}
			lock = l
		}
		st.Status, st.Out = call(st.Method, st.Path, unmask(st.Body))
		if lock != nil {
			lock.release()
		}
		if st.Wait {
			deadline := time.Now().Add(30 * time.Second)
			for lcJobSnapshot(0, false)["running"] == true {
				if time.Now().After(deadline) {
					t.Fatalf("%s : le job ne finit pas", st.Name)
				}
				time.Sleep(20 * time.Millisecond)
			}
			_, st.Job = call("GET", "/api/llamacpp/job?from=0", "")
		}
		st.After = map[string]string{}
		if v := ReadConfig()["BIN"]; v != "" {
			st.After["config/BIN"] = mask(v)
		}
		if v := getStr(bkState, "build_job"); v != "" {
			st.After["state/build_job"] = mask(v)
		}
		if b, err := os.ReadFile(filepath.Join(home, "loki-build.log")); err == nil {
			st.After["loki-build.log"] = mask(string(b))
		}
		if b, err := os.ReadFile(filepath.Join(home, "devices.json")); err == nil {
			st.After["devices.json"] = mask(string(b))
		}
		st.Tree = parityTree(t, home)
		for i := range st.Tree {
			st.Tree[i] = mask(st.Tree[i])
		}
		return st
	}

	str := func(s string) *string { return &s }
	tarOK := []parityArchEntry{
		{Name: "llama-b300/", Type: "dir", Mode: 0o755},
		{Name: "llama-b300/llama-server", Type: "file", Body: "#!/bin/sh\necho serveur\n", Mode: 0o750},
		{Name: "llama-b300/libggml.so.1.0", Type: "file", Body: "lib", Mode: 0o644},
		{Name: "llama-b300/libggml.so.1", Type: "symlink", Link: "libggml.so.1.0"},
		{Name: "llama-b300/libggml.so", Type: "symlink", Link: "absente.so"},
		{Name: "llama-b300/llama-cli", Type: "hardlink", Link: "llama-b300/llama-server"},
		{Name: "llama-b300/sous/doc.txt", Type: "file", Body: "doc", Mode: 0o600},
	}
	relJSON := func(tag string, names ...string) string {
		var assets []string
		for _, n := range names {
			assets = append(assets, `{"name":"`+n+`","browser_download_url":"{{GH}}/dl/`+n+`","size":2500000}`)
		}
		return `{"tag_name":"` + tag + `","assets":[` + strings.Join(assets, ",") + `]}`
	}
	cpu := "llama-b300-bin-ubuntu-x64.tar.gz"
	if runtime.GOARCH == "arm64" {
		cpu = "llama-b300-bin-ubuntu-arm64.tar.gz"
	}
	allAssets := []string{"llama-b300-bin-macos-arm64.zip", "cudart-llama-bin-win-cuda-12.4-x64.zip",
		"llama-b300-bin-win-cuda-12.4-x64.zip", "llama-b300-bin-ubuntu-vulkan-x64.tar.gz",
		"llama-b300-bin-ubuntu-rocm-7.0-x64.tar.gz", "llama-b300-bin-ubuntu-vulkan-arm64.tar.gz",
		"llama-b300-bin-ubuntu-arm64.tar.gz", "llama-b300-bin-ubuntu-x64.tar.gz"}
	okRel := &parityRelease{Status: 200, JSON: relJSON("b300", allAssets...),
		Assets: map[string][]parityArchEntry{cpu: tarOK}}
	pb := "backends/llama.cpp-prebuilt/"

	var steps []parityBackendsStep
	for _, st := range []parityBackendsStep{
		// --- statut, rien d'installé ---
		{Name: "statut vide", Method: "GET", Path: "/api/llamacpp"},
		{Name: "backends vides", Method: "GET", Path: "/api/backends"},
		{Name: "custom vides", Method: "GET", Path: "/api/backends/custom"},
		{Name: "job absent", Method: "GET", Path: "/api/llamacpp/job"},

		// --- précompilé déjà là ---
		{Name: "deux versions précompilées", Files: map[string]string{
			pb + "llama-b100/llama-server":           "#!/bin/sh\n",
			pb + "llama-b200/build/bin/llama-server": "#!/bin/sh\n",
			pb + "VERSION":                           "b100 12.4 fmt2\n",
		}, Config: map[string]string{"BIN": "{{HOME}}/" + pb + "llama-b200/build/bin/llama-server"},
			Method: "GET", Path: "/api/llamacpp"},
		{Name: "VERSION absent : la plus récente", Files: map[string]string{pb + "VERSION": "-"},
			Method: "GET", Path: "/api/llamacpp"},
		{Name: "BIN précompilé disparu", Config: map[string]string{"BIN": "{{HOME}}/" + pb + "llama-b050/llama-server"},
			Method: "GET", Path: "/api/llamacpp"},

		// --- backends custom ---
		{Name: "backends custom", Files: map[string]string{
			"backends/fork-a/build/bin/llama-server": "#!/bin/sh\n",
			"backends/fork-a/CMakeLists.txt":         "",
			"backends/fork-b/bin/llama-server":       "#!/bin/sh\n",
			"backends/fork-c/build/bin/llama-server": "/",
			"backends/fork-c/llama-server.exe":       "x",
			"backends/.cache/llama-server":           "x",
			"backends/vide/":                         "/",
			"backends/lien":                          "-> fork-b",
			"backends/fichier":                       "x",
			pb + "bin/llama-server":                  "#!/bin/sh\n", // vu par le balayage, mais pas un backend custom
		}, Config: map[string]string{"BIN": "{{HOME}}/backends/fork-a/build/bin/llama-server"},
			Method: "GET", Path: "/api/backends"},
		{Name: "custom seulement", Method: "GET", Path: "/api/backends/custom"},
		{Name: "statut sur un fork", Method: "GET", Path: "/api/llamacpp"},
		{Name: "désinstaller le canonique", Method: "POST", Path: "/api/llamacpp/uninstall-custom", Body: `{"name":"llama.cpp"}`},
		{Name: "désinstaller le précompilé", Method: "POST", Path: "/api/llamacpp/uninstall-custom", Body: `{"name":" LLAMA.CPP-prebuilt "}`},
		{Name: "désinstaller sans nom", Method: "POST", Path: "/api/llamacpp/uninstall-custom", Body: `{"name":""}`},
		{Name: "désinstaller, corps illisible", Method: "POST", Path: "/api/llamacpp/uninstall-custom", Body: `{`},
		{Name: "désinstaller l'actif", Method: "POST", Path: "/api/llamacpp/uninstall-custom", Body: `{"name":"Fork-A"}`},
		{Name: "désinstaller par un chemin", Method: "POST", Path: "/api/llamacpp/uninstall-custom", Body: `{"name":"../fork-b"}`},
		{Name: "désinstaller un absent", Method: "POST", Path: "/api/llamacpp/uninstall-custom", Body: `{"name":"fork-z"}`},
		{Name: "désinstaller un lien", Method: "POST", Path: "/api/llamacpp/uninstall-custom", Body: `{"name":"lien"}`},
		{Name: "désinstaller un fichier", Method: "POST", Path: "/api/llamacpp/uninstall-custom", Body: `{"name":"fichier"}`},
		{Name: "désinstaller un dossier sans binaire", Method: "POST", Path: "/api/llamacpp/uninstall-custom", Body: `{"name":"vide"}`},

		// --- dépôt git du build local ---
		{Name: "dépôt cloné", Git: []string{
			"init src", "commit src 2024-01-02T03:04:05+00:00 premier état",
			"clone src backends/llama.cpp",
		}, Files: map[string]string{"backends/llama.cpp/build/bin/llama-server": "#!/bin/sh\n"},
			Config: map[string]string{"BIN": "{{HOME}}/backends/llama.cpp/build/bin/llama-server"},
			Method: "GET", Path: "/api/llamacpp"},
		{Name: "amont avancé, pas encore vu", Git: []string{"commit src 2024-02-03T04:05:06+00:00 <second> & état"},
			Method: "GET", Path: "/api/llamacpp"},
		{Name: "vérification git", Method: "POST", Path: "/api/llamacpp/check"},
		{Name: "retard connu", Method: "GET", Path: "/api/llamacpp"},
		{Name: "binaire Release", Files: map[string]string{
			"backends/llama.cpp/build/bin/llama-server":         "-",
			"backends/llama.cpp/build/bin/Release/llama-server": "#!/bin/sh\n",
		}, Method: "POST", Path: "/api/llamacpp/check"},
		{Name: "sans binaire", Files: map[string]string{"backends/llama.cpp/build/bin/Release/llama-server": "-"},
			Method: "POST", Path: "/api/llamacpp/check"},
		{Name: "à jour, sans binaire", Git: []string{"pull backends/llama.cpp"}, Method: "POST", Path: "/api/llamacpp/check"},
		{Name: "tête détachée", Git: []string{"detach backends/llama.cpp"}, Method: "GET", Path: "/api/llamacpp"},
		{Name: "vérification, tête détachée", Method: "POST", Path: "/api/llamacpp/check"},
		{Name: "origine perdue", Git: []string{"remote-bad backends/llama.cpp"}, Method: "POST", Path: "/api/llamacpp/check"},
		{Name: "pas de dépôt", Config: map[string]string{"BIN": "/nulle/part/llama-server"},
			Files: map[string]string{"backends/llama.cpp/.git": "-"}, Method: "POST", Path: "/api/llamacpp/check"},

		// --- bascule fast / opt ---
		{Name: "bascule fast", Method: "POST", Path: "/api/llamacpp/use", Body: `{"mode":"fast"}`},
		{Name: "bascule opt sans build", Method: "POST", Path: "/api/llamacpp/use", Body: `{"mode":"opt"}`},
		{Name: "bascule opt", Files: map[string]string{"backends/llama.cpp/build/llama-server": "#!/bin/sh\n"},
			Method: "POST", Path: "/api/llamacpp/use", Body: `{"mode":"opt"}`},
		{Name: "bascule opt depuis un fork", Config: map[string]string{"BIN": "{{HOME}}/backends/fork-a/build/bin/llama-server"},
			Files:  map[string]string{"backends/fork-a/build/llama-server": "#!/bin/sh\n"},
			Method: "POST", Path: "/api/llamacpp/use", Body: `{"mode":"opt"}`},
		{Name: "mode inconnu", Method: "POST", Path: "/api/llamacpp/use", Body: `{"mode":"turbo"}`},
		{Name: "mode absent", Method: "POST", Path: "/api/llamacpp/use", Body: ``},
		{Name: "mode de travers", Method: "POST", Path: "/api/llamacpp/use", Body: `{"mode":1}`},
		{Name: "bascule pendant une optimisation", Tune: true, Method: "POST", Path: "/api/llamacpp/use", Body: `{"mode":"fast"}`},
		{Name: "fast sans précompilé", Files: map[string]string{pb: "-"},
			Method: "POST", Path: "/api/llamacpp/use", Body: `{"mode":"fast"}`},

		// --- moteur téléchargé, moteur de l'image ---
		{Name: "moteur téléchargé", Files: map[string]string{"engine/server-cuda-b10450/llama-server": "#!/bin/sh\n"},
			Config: map[string]string{"BIN": "{{HOME}}/engine/server-cuda-b10450/llama-server"},
			Method: "GET", Path: "/api/llamacpp"},
		{Name: "moteur de l'image", Engine: str(" {{HOME}}/engine/server-cuda-b10450/llama-server "),
			Method: "GET", Path: "/api/llamacpp"},
		{Name: "moteur de l'image absent", Engine: str("{{HOME}}/image/llama-server"),
			Method: "GET", Path: "/api/llamacpp"},
		{Name: "moteur de l'image : un dossier", Engine: str("{{HOME}}/engine"),
			Method: "GET", Path: "/api/llamacpp"},

		// --- dernière release ---
		{Name: "release : CPU", Release: okRel, Engine: str(""), Method: "POST", Path: "/api/llamacpp/prebuilt/check"},
		{Name: "release refusée", Release: &parityRelease{Status: 403, JSON: `{"message":"rate limit"}`},
			Method: "POST", Path: "/api/llamacpp/prebuilt/check"},
		{Name: "release sans tag", Release: &parityRelease{Status: 200, JSON: `{"tag_name":"","assets":[]}`},
			Method: "POST", Path: "/api/llamacpp/prebuilt/check"},
		{Name: "release illisible", Release: &parityRelease{Status: 200, JSON: `{"tag_name":`},
			Method: "POST", Path: "/api/llamacpp/prebuilt/check"},
		{Name: "release vide", Release: &parityRelease{Status: 200, JSON: ``},
			Method: "POST", Path: "/api/llamacpp/prebuilt/check"},
		{Name: "release, tag de travers", Release: &parityRelease{Status: 200, JSON: `{"tag_name":5}`},
			Method: "POST", Path: "/api/llamacpp/prebuilt/check"},
		{Name: "release sans asset adapté", Release: &parityRelease{Status: 200, JSON: relJSON("b301", "llama-b301-bin-macos-arm64.zip")},
			Method: "POST", Path: "/api/llamacpp/prebuilt/check"},

		// --- job des binaires précompilés ---
		{Name: "installation", Release: okRel, Files: map[string]string{
			pb + "llama-b100/llama-server": "#!/bin/sh\n",
			pb + "llama-b200/llama-server": "#!/bin/sh\n",
			pb + "llama-b30/llama-server":  "#!/bin/sh\n", // préfixe de llama-b300 : supprimé quand même
			pb + "autre/llama-server":      "#!/bin/sh\n",
			pb + "VERSION":                 "b100 12.4 fmt2\n",
		}, Method: "POST", Path: "/api/llamacpp/prebuilt", Wait: true},
		{Name: "statut après installation", Method: "GET", Path: "/api/llamacpp"},
		{Name: "release à jour", Method: "POST", Path: "/api/llamacpp/prebuilt/check"},
		{Name: "lignes à partir de 3", Method: "GET", Path: "/api/llamacpp/job?from=3"},
		{Name: "lignes au-delà", Method: "GET", Path: "/api/llamacpp/job?from=99"},
		{Name: "lignes, from négatif", Method: "GET", Path: "/api/llamacpp/job?from=-4"},
		{Name: "lignes, from illisible", Method: "GET", Path: "/api/llamacpp/job?from=abc"},
		{Name: "lignes, from démesuré", Method: "GET", Path: "/api/llamacpp/job?from=99999999999999999999"},
		{Name: "déjà à jour", Files: map[string]string{pb + "llama-b250/x": ""},
			Method: "POST", Path: "/api/llamacpp/prebuilt", Wait: true},
		{Name: "ancien format : réextrait", Files: map[string]string{pb + "VERSION": "b300 \n"},
			Method: "POST", Path: "/api/llamacpp/prebuilt", Wait: true},
		{Name: "pendant une optimisation", Tune: true, Method: "POST", Path: "/api/llamacpp/prebuilt"},
		{Name: "GitHub refuse", Release: &parityRelease{Status: 403, JSON: `{}`},
			Method: "POST", Path: "/api/llamacpp/prebuilt", Wait: true},
		{Name: "rien d'adapté", Release: &parityRelease{Status: 200, JSON: relJSON("b302", "llama-b302-bin-macos-arm64.zip")},
			Method: "POST", Path: "/api/llamacpp/prebuilt", Wait: true},
		{Name: "archive absente", Release: &parityRelease{Status: 200, JSON: relJSON("b303", allAssets...)},
			Method: "POST", Path: "/api/llamacpp/prebuilt", Wait: true},
		{Name: "archive piégée", Release: &parityRelease{Status: 200, JSON: relJSON("b304", allAssets...),
			Assets: map[string][]parityArchEntry{cpu: {
				{Name: "llama-b304/ok", Type: "file", Body: "x", Mode: 0o644},
				{Name: "../evasion", Type: "file", Body: "x", Mode: 0o644},
			}}}, Method: "POST", Path: "/api/llamacpp/prebuilt", Wait: true},
		{Name: "archive sans serveur", Release: &parityRelease{Status: 200, JSON: relJSON("b305", allAssets...),
			Assets: map[string][]parityArchEntry{cpu: {
				{Name: "llama-b305/llama-cli", Type: "file", Body: "x", Mode: 0o755},
			}}}, Files: map[string]string{pb: "-"}, Method: "POST", Path: "/api/llamacpp/prebuilt", Wait: true},
		{Name: "statut après échecs", Method: "GET", Path: "/api/llamacpp"},

		// --- GPU vus par un moteur ---
		{Name: "GPU sans moteur", Config: map[string]string{"BIN": ""}, Method: "POST", Path: "/api/backends/devices", Body: `{}`},
		{Name: "GPU d'un moteur absent", Method: "POST", Path: "/api/backends/devices", Body: `{"bin":"/nulle/part/llama-server"}`},
		{Name: "GPU : pas un llama-server", Files: map[string]string{"outils/autre": "#!/bin/sh\n"},
			Method: "POST", Path: "/api/backends/devices", Body: `{"bin":"{{HOME}}/outils/autre"}`},
		{Name: "GPU listés", Machine: "nvidia", Files: map[string]string{
			"gpu/LLAMA-SERVER": "#!/bin/sh\n[ \"$1\" = --list-devices ] || exit 7\necho 'Available devices:'\necho \"  CUDA0: NVIDIA GeForce RTX 3090 (0 MiB, 0 MiB free)\"\necho \"  CUDA1: Carte B (8192 MiB, 8000 MiB free)\"\necho \"  Vulkan0: NVIDIA GeForce RTX 3090 (24576 MiB, 20000 MiB free)\"\necho \"  CUDA2: ordre ${CUDA_DEVICE_ORDER:-aucun} cvd ${CUDA_VISIBLE_DEVICES:-aucun} (1 MiB, 1 MiB free)\" >&2\n",
		}, Method: "POST", Path: "/api/backends/devices", Body: `{"bin":" {{HOME}}/gpu/LLAMA-SERVER "}`},
		{Name: "GPU en cache", Files: map[string]string{"gpu/LLAMA-SERVER": "#!/bin/sh\necho '  CUDA0: changé (1 MiB, 1 MiB free)'\n"},
			Method: "POST", Path: "/api/backends/devices", Body: `{"bin":"{{HOME}}/gpu/LLAMA-SERVER"}`},
		{Name: "GPU filtrés", Config: map[string]string{"CUDA_VISIBLE_DEVICES": "1,0", "BIN": "{{HOME}}/gpu/LLAMA-SERVER"},
			Files:  map[string]string{"gpu/LLAMA-SERVER": "#!/bin/sh\necho \"  CUDA0: ordre $CUDA_DEVICE_ORDER cvd $CUDA_VISIBLE_DEVICES (10 MiB, 5 MiB free)\"\n"},
			Method: "POST", Path: "/api/backends/devices", Body: `{"bin":""}`},
		{Name: "GPU : mémoire nulle non figée", Machine: "nu", Config: map[string]string{"CUDA_VISIBLE_DEVICES": "0"},
			Files:  map[string]string{"gpu/LLAMA-SERVER": "#!/bin/sh\necho '  CUDA0: Saturée (0 MiB, 0 MiB free)'\n"},
			Method: "POST", Path: "/api/backends/devices", Body: `{}`},
		{Name: "GPU : relu, pas en cache", Files: map[string]string{"gpu/LLAMA-SERVER": "#!/bin/sh\necho '  CUDA0: Saturée (100 MiB, 0 MiB free)'\n"},
			Method: "POST", Path: "/api/backends/devices", Body: `{}`},
		{Name: "GPU : moteur planté, repli disque", Config: map[string]string{"CUDA_VISIBLE_DEVICES": "1,0"},
			Files:  map[string]string{"gpu/llama-server": "#!/bin/sh\necho '  CUDA0: partielle (1 MiB, 1 MiB free)'\nexit 3\n"},
			Method: "POST", Path: "/api/backends/devices", Body: `{"bin":"{{HOME}}/gpu/llama-server"}`},
		{Name: "GPU : planté, partiel", Config: map[string]string{"CUDA_VISIBLE_DEVICES": "2"},
			Method: "POST", Path: "/api/backends/devices", Body: `{"bin":"{{HOME}}/gpu/llama-server"}`},
		{Name: "GPU : planté, rien", Config: map[string]string{"CUDA_VISIBLE_DEVICES": "3"},
			Files:  map[string]string{"gpu/llama-server": "#!/bin/sh\necho 'CUDA error: out of memory'\nexit 4\n"},
			Method: "POST", Path: "/api/backends/devices", Body: `{"bin":"{{HOME}}/gpu/llama-server"}`},
		{Name: "GPU : tué", Config: map[string]string{"CUDA_VISIBLE_DEVICES": "4"},
			Files:  map[string]string{"gpu/llama-server": "#!/bin/sh\nkill -9 $$\n"},
			Method: "POST", Path: "/api/backends/devices", Body: `{"bin":"{{HOME}}/gpu/llama-server"}`},
		{Name: "GPU : non exécutable", Config: map[string]string{"CUDA_VISIBLE_DEVICES": "5"},
			Files:  map[string]string{"gpu/llama-server": "pas un script"},
			Method: "POST", Path: "/api/backends/devices", Body: `{"bin":"{{HOME}}/gpu/llama-server"}`},
		{Name: "GPU : corps illisible, BIN de la config", Config: map[string]string{"CUDA_VISIBLE_DEVICES": "6"},
			Files:  map[string]string{"gpu/LLAMA-SERVER": "#!/bin/sh\necho '  CUDA0: Lue (2 MiB, 1 MiB free)'\n"},
			Method: "POST", Path: "/api/backends/devices", Body: `{"bin":`},
		{Name: "GPU : pilote ancien", Machine: "arch-ancienne", Config: map[string]string{"CUDA_VISIBLE_DEVICES": "7"},
			Files:  map[string]string{"gpu/LLAMA-SERVER": "#!/bin/sh\necho '  CUDA0: Vieille carte (0 MiB, 0 MiB free)'\necho '  CUDA1: Vieille carte (0 MiB, 0 MiB free)'\n"},
			Method: "POST", Path: "/api/backends/devices", Body: `{}`},
		{Name: "GPU : deux cartes de même nom", Machine: "nvidia", Config: map[string]string{"CUDA_VISIBLE_DEVICES": "8"},
			Files:  map[string]string{"gpu/LLAMA-SERVER": "#!/bin/sh\necho '  CUDA0: Carte B (0 MiB, 0 MiB free)'\n"},
			Method: "POST", Path: "/api/backends/devices", Body: `{}`},
		{Name: "GPU : lu à vide, gardé sur disque", Machine: "nu", Config: map[string]string{"CUDA_VISIBLE_DEVICES": "9"},
			Files:  map[string]string{"gpu/LLAMA-SERVER": "#!/bin/sh\necho '  CUDA0: Pleine (0 MiB, 0 MiB free)'\n"},
			Method: "POST", Path: "/api/backends/devices", Body: `{}`},
		{Name: "GPU : cartes jumelles chez nvidia-smi", Machine: "nvidia-jumelles", Config: map[string]string{"CUDA_VISIBLE_DEVICES": "10"},
			Files:  map[string]string{"gpu/LLAMA-SERVER": "#!/bin/sh\necho '  CUDA0: Carte B (0 MiB, 0 MiB free)'\n"},
			Method: "POST", Path: "/api/backends/devices", Body: `{}`},
		{Name: "GPU : liaison sans génération connue", Machine: "liaison-partielle", Config: map[string]string{"CUDA_VISIBLE_DEVICES": "11"},
			Files:  map[string]string{"gpu/LLAMA-SERVER": "#!/bin/sh\necho '  CUDA0: Carte P (4096 MiB, 4000 MiB free)'\n"},
			Method: "POST", Path: "/api/backends/devices", Body: `{}`},
		{Name: "GPU : liste vide sur le disque, pas de repli", Machine: "nu", Config: map[string]string{"CUDA_VISIBLE_DEVICES": "vide"},
			Files: map[string]string{
				"devices.json":     `{"{{HOME}}/gpu/LLAMA-SERVER\u0000vide": []}`,
				"gpu/LLAMA-SERVER": "#!/bin/sh\nexit 3\n",
			}, Method: "POST", Path: "/api/backends/devices", Body: `{}`},
		{Name: "GPU : planté, repli sur le disque",
			Files:  map[string]string{"gpu/LLAMA-SERVER": "#!/bin/sh\necho '  CUDA0: partielle (1 MiB, 1 MiB free)'\nexit 3\n"},
			Method: "POST", Path: "/api/backends/devices", Body: `{}`},
	} {
		steps = append(steps, run(st))
	}

	// --- plan de build, conseil et release, machine par machine ---
	type planCase struct {
		Machine string `json:"machine"`
		Plan    any    `json:"plan"`
		Reco    any    `json:"reco"`
		Check   any    `json:"check"`
	}
	var plans []planCase
	machineNames := make([]string, 0, len(parityMachines))
	for n := range parityMachines {
		machineNames = append(machineNames, n)
	}
	sort.Strings(machineNames)
	planRel := parityRelease{Status: 200, JSON: relJSON("b400", allAssets...)}
	mu.Lock()
	rel = planRel
	mu.Unlock()
	for _, n := range machineNames {
		machine(n)
		_, out := call("GET", "/api/llamacpp", "")
		m := out.(map[string]any)
		_, check := call("POST", "/api/llamacpp/prebuilt/check", "")
		plans = append(plans, planCase{Machine: n, Plan: m["plan"], Reco: m["reco"], Check: check})
	}
	machine("vulkaninfo-seul")
	sansVulkanRel := parityRelease{Status: 200, JSON: relJSON("b401", "llama-b401-bin-ubuntu-rocm-7.0-x64.tar.gz", "llama-b401-bin-ubuntu-x64.tar.gz", "llama-b401-bin-ubuntu-arm64.tar.gz")}
	mu.Lock()
	rel = sansVulkanRel
	mu.Unlock()
	_, sansVulkan := call("POST", "/api/llamacpp/prebuilt/check", "")
	machine("rocm")
	rocmRel := parityRelease{Status: 200, JSON: relJSON("b402", "llama-b402-bin-ubuntu-vulkan-x64.tar.gz", "llama-b402-bin-ubuntu-x64.tar.gz", "llama-b402-bin-ubuntu-vulkan-arm64.tar.gz", "llama-b402-bin-ubuntu-arm64.tar.gz")}
	mu.Lock()
	rel = rocmRel
	mu.Unlock()
	_, rocmSansAsset := call("POST", "/api/llamacpp/prebuilt/check", "")

	got, err := parityJSON(map[string]any{
		"goarch":          runtime.GOARCH,
		"machines":        parityMachines,
		"steps":           steps,
		"plans":           plans,
		"plan_release":    planRel,
		"sans_vulkan":     map[string]any{"release": sansVulkanRel, "out": sansVulkan},
		"rocm_sans_asset": map[string]any{"release": rocmRel, "out": rocmSansAsset},
		"job":             parityJobScript(t, home),
	})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityBackendsFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityBackendsFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityBackendsFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("les backends ne produisent plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteBackends,\n"+
			"puis cargo test --test backends dit si le port Rust suit.\n--- obtenu ---\n%s", parityBackendsFile, got)
	}
}

// parityApplyFiles pose les fichiers d'une étape, dans l'ordre des noms.
func parityApplyFiles(t *testing.T, home string, files map[string]string) {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v := files[n]
		p := filepath.Join(home, n)
		switch {
		case v == "-":
			if err := os.RemoveAll(p); err != nil {
				t.Fatal(err)
			}
			continue
		case v == "/":
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if target, ok := strings.CutPrefix(v, "-> "); ok {
			_ = os.Remove(p)
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(v, "#!") {
			mode = 0o755
		}
		// Remplacé, pas réécrit en place : un script qui vient de tourner ne
		// gêne pas l'écriture (ETXTBSY).
		_ = os.Remove(p)
		if err := os.WriteFile(p, []byte(v), mode); err != nil {
			t.Fatal(err)
		}
	}
}

// parityGit : les opérations git d'une étape, dates et identité fixées pour
// des commits reproductibles.
func parityGit(t *testing.T, gitBin, home, op string) {
	t.Helper()
	f := strings.SplitN(op, " ", 4)
	gitIn := func(dir, date string, args ...string) {
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=Loki", "GIT_AUTHOR_EMAIL=loki@exemple.fr",
			"GIT_COMMITTER_NAME=Loki", "GIT_COMMITTER_EMAIL=loki@exemple.fr",
			"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v : %v\n%s", args, err, out)
		}
	}
	switch f[0] {
	case "init":
		dir := filepath.Join(home, f[1])
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		gitIn(dir, "", "init", "-q", "-b", "master")
	case "commit":
		dir := filepath.Join(home, f[1])
		if err := os.WriteFile(filepath.Join(dir, "README"), []byte(f[3]), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(dir, f[2], "add", "README")
		gitIn(dir, f[2], "commit", "-q", "-m", f[3])
	case "clone":
		gitIn(home, "", "clone", "-q", filepath.Join(home, f[1]), filepath.Join(home, f[2]))
	case "pull":
		gitIn(filepath.Join(home, f[1]), "", "pull", "-q", "--ff-only")
	case "detach":
		gitIn(filepath.Join(home, f[1]), "", "checkout", "-q", "--detach")
	case "remote-bad":
		gitIn(filepath.Join(home, f[1]), "", "remote", "set-url", "origin", filepath.Join(home, "nulle-part"))
	default:
		t.Fatalf("opération git inconnue : %s", op)
	}
}

// parityTarGz construit une archive .tar.gz.
func parityTarGz(t *testing.T, entries []parityArchEntry) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.Name, Mode: e.Mode, ModTime: time.Unix(1700000000, 0)}
		switch e.Type {
		case "dir":
			h.Typeflag = tar.TypeDir
		case "symlink":
			h.Typeflag, h.Linkname, h.Mode = tar.TypeSymlink, e.Link, 0o777
		case "hardlink":
			h.Typeflag, h.Linkname = tar.TypeLink, e.Link
		default:
			h.Typeflag, h.Size = tar.TypeReg, int64(len(e.Body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Error(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte(e.Body))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// parityTree : ce qu'il y a sous backends/ et engine/ (« d », « f mode », « l
// cible »), trié.
func parityTree(t *testing.T, home string) []string {
	var out []string
	for _, root := range []string{"backends", "engine"} {
		_ = filepath.WalkDir(filepath.Join(home, root), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(home, p)
			if strings.Contains(rel, "/.git/") || strings.HasSuffix(rel, "/.git") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := os.Lstat(p)
			if err != nil {
				return nil
			}
			switch {
			case info.Mode()&os.ModeSymlink != 0:
				target, _ := os.Readlink(p)
				out = append(out, "l "+rel+" -> "+target)
			case info.IsDir():
				out = append(out, "d "+rel)
			default:
				b, _ := os.ReadFile(p)
				out = append(out, "f "+rel+" "+info.Mode().Perm().String()+" "+string(b))
			}
			return nil
		})
	}
	sort.Strings(out)
	return out
}

// parityJobScript rejoue le job pas à pas (sans la façade) : début, lignes
// de build (phases déduites, fichiers compilés), troncature à lcMaxLines,
// phases, commits, échec, fin, refus d'un second job, vues à partir d'un
// décalage, état écrit en base et journal ; puis la reprise au démarrage.
func parityJobScript(t *testing.T, home string) any {
	resetBackendsState()
	_ = os.Remove(lcJobLogPath())
	type op struct {
		Op   string `json:"op"`
		Arg  string `json:"arg,omitempty"`
		From int    `json:"from,omitempty"`
		Out  any    `json:"out,omitempty"`
	}
	tsRe := regexp.MustCompile(`"(started_at|ended_at)":[1-9][0-9]*`)
	norm := func(v any) any {
		b, _ := json.Marshal(v)
		var out any
		_ = json.Unmarshal([]byte(tsRe.ReplaceAllString(string(b), `"$1":"{{T}}"`)), &out)
		return out
	}
	var ops []op
	done := make(chan struct{})
	release := make(chan struct{})
	do := func(o op) {
		switch o.Op {
		case "start":
			err := startLcJob(o.Arg, func() { <-release; close(done) })
			if err != nil {
				o.Out = err.Error()
			} else {
				o.Out = "ok"
			}
		case "line":
			lcAppend(o.Arg)
		case "lines":
			for i := 0; i < 4002; i++ {
				lcAppend("[" + o.Arg + "] bruit")
			}
		case "phase":
			lcPhase(o.Arg)
		case "fail":
			lcFail(errors.New(o.Arg))
		case "done":
			lcDone(o.Arg)
		case "old", "new":
			lcSetCommit(o.Op, o.Arg)
		case "end":
			close(release)
			<-done
			deadline := time.Now().Add(10 * time.Second)
			for lcJobSnapshot(0, false)["running"] == true && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
		case "snapshot":
			o.Out = norm(lcJobSnapshot(o.From, true))
		case "summary": // vue longue résumée : nombre de lignes, première, dernière
			v := lcJobSnapshot(o.From, true)
			lines, _ := v["lines"].([]string)
			v["lines"] = map[string]any{"n": len(lines), "first": lines[0], "last": lines[len(lines)-1]}
			o.Out = norm(v)
		case "head":
			o.Out = norm(lcJobSnapshot(0, false))
		case "state":
			o.Out = tsRe.ReplaceAllString(getStr(bkState, "build_job"), `"$1":"{{T}}"`)
		case "log":
			b, _ := os.ReadFile(lcJobLogPath())
			o.Out = string(b)
		}
		ops = append(ops, o)
	}
	for _, o := range []op{
		{Op: "head"},
		{Op: "line", Arg: "avant tout job"},
		{Op: "start", Arg: "update"},
		{Op: "state"},
		{Op: "start", Arg: "engine"},
		{Op: "line", Arg: "-- The C compiler identification is GNU 13.2.0 avec un très long suffixe é"},
		{Op: "head"},
		{Op: "line", Arg: "[ 12%] Building CXX object ggml/src/CMakeFiles/ggml.dir/ggml.cpp.o"},
		{Op: "line", Arg: "  Compiling CUDA source file ..\\ggml-cuda\\fattn.cu..."},
		{Op: "line", Arg: "softmax.cuh"},
		{Op: "line", Arg: "[100%] Linking CXX executable bin/llama-server"},
		{Op: "line", Arg: "état : é.cpp"},
		{Op: "head"},
		{Op: "old", Arg: "abc1234"},
		{Op: "phase", Arg: "git pull origin/master…"},
		{Op: "new", Arg: ""},
		{Op: "state"},
		{Op: "line", Arg: "-- Configuring done"}, // moins de 2 s après la dernière écriture : pas réécrit
		{Op: "state"},
		{Op: "snapshot", From: 2},
		{Op: "lines", Arg: "bloc"},
		{Op: "snapshot", From: 4005},
		{Op: "summary", From: 0},
		{Op: "summary", From: 3},
		{Op: "line", Arg: "Build files have been written to: /src/build"},
		{Op: "summary", From: 0},
		{Op: "line", Arg: "une de plus"},
		{Op: "summary", From: 0},
		{Op: "fail", Arg: "compilation échouée: exit status 2"},
		{Op: "done", Arg: "tout de même"},
		{Op: "end"},
		{Op: "head"},
		{Op: "state"},
	} {
		do(o)
	}
	logLines := strings.Count(func() string { b, _ := os.ReadFile(lcJobLogPath()); return string(b) }(), "\n")

	// Reprise au démarrage : un job noté « en cours » est mort avec le
	// processus précédent ; un job fini est rendu tel quel.
	type restoreCase struct {
		State string  `json:"state"`
		Log   *string `json:"log"`
		Out   any     `json:"out"`
		After string  `json:"after"`
	}
	str := func(s string) *string { return &s }
	var restores []restoreCase
	for _, rc := range []restoreCase{
		{State: ``},
		{State: `{"action":""}`},
		{State: `{"action":"prebuilt","phase":"fini","running":false,"started_at":5,"ended_at":6}`, Log: str("a\nb\n")},
		{State: `{"action":"update","phase":"compilation… 3 fichiers","compiled":3,"running":true,"started_at":5,"old":"abc"}`, Log: str("x\n")},
		{State: `{"action":"strata","running":true}`},
		{State: `{"action":"engine","running":false}`, Log: str("")},
		{State: `{"action":"engine","running":false}`, Log: str("\n\n")},
		{State: `pas du json`},
		{State: `{"action":"engine","compiled":"3"}`},
	} {
		resetBackendsState()
		if rc.State == "" {
			_ = putStr(bkState, "build_job", "")
		} else if err := putStr(bkState, "build_job", rc.State); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(lcJobLogPath())
		if rc.Log != nil {
			if err := os.WriteFile(lcJobLogPath(), []byte(*rc.Log), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		lcRestore()
		rc.Out = norm(lcJobSnapshot(0, true))
		rc.After = tsRe.ReplaceAllString(getStr(bkState, "build_job"), `"$1":"{{T}}"`)
		restores = append(restores, rc)
	}
	// Journal de plus de lcMaxLines lignes.
	resetBackendsState()
	var long strings.Builder
	for i := 0; i < lcMaxLines+3; i++ {
		long.WriteString("ligne\n")
	}
	_ = putStr(bkState, "build_job", `{"action":"update","running":false}`)
	_ = os.WriteFile(lcJobLogPath(), []byte(long.String()), 0o644)
	lcRestore()
	snap := lcJobSnapshot(lcMaxLines, true)
	resetBackendsState()
	_ = putStr(bkState, "build_job", "")
	_ = os.Remove(lcJobLogPath())
	return map[string]any{"ops": ops, "log_lines": logLines, "restores": restores, "long_restore": norm(snap)}
}
