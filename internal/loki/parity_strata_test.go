//go:build linux

package loki

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// parity_strata_test.go — journal de référence de Strata (lot 25 de la
// migration Rust) : la machine vue par l'écran d'installation (cartes,
// Python, RAM, disque) et ce qui y tient, les réglages d'un modèle installé
// (et la bascule qui les applique), l'installation (paquet figé vérifié,
// installeur lancé, preset écrit puis activé) et ses échecs.
// Rangé dans rust/loki-rs/tests/parity_strata/strata.json ; tests/strata.rs
// le rejoue par la façade.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteStrata

const parityStrataFile = "../../rust/loki-rs/tests/parity_strata/strata.json"

// Ce que sert la fausse release : les sources (archive), le moteur (des
// octets), un statut imposé, des empreintes attendues fausses.
type parityStrataRel struct {
	Status int               `json:"status,omitempty"`
	Src    []parityArchEntry `json:"src"`
	Engine string            `json:"engine"`
	BadSHA bool              `json:"bad_sha,omitempty"`
}

type parityStrataStep struct {
	Name     string            `json:"name"`
	Machine  string            `json:"machine,omitempty"`
	Meminfo  string            `json:"meminfo,omitempty"`
	DiskFree int64             `json:"disk_free,omitempty"`
	Files    map[string]string `json:"files,omitempty"`
	Config   map[string]string `json:"config,omitempty"`
	State    map[string]string `json:"state,omitempty"`
	Release  *parityStrataRel  `json:"release,omitempty"`
	Tune     bool              `json:"tune,omitempty"`
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Body     string            `json:"body,omitempty"`
	Wait     bool              `json:"wait,omitempty"`
	Status   int               `json:"status"`
	Out      any               `json:"out"`
	Job      any               `json:"job,omitempty"`
	Actions  []string          `json:"actions"`
	After    map[string]string `json:"after"`
	Tree     []string          `json:"tree"`
}

// Les machines : nvidia-smi et python3 dans le PATH (bash et mkdir toujours).
func parityStrataMachines() map[string]map[string]string {
	py := func(version string, venv bool) string {
		s := "#!/bin/sh\ncase \"$1\" in\n--version) echo '" + version + "';;\n-c) "
		if venv {
			s += "exit 0;;\n"
		} else {
			s += "echo 'ModuleNotFoundError: No module named ensurepip'; exit 1;;\n"
		}
		return s + "esac\n"
	}
	ok := py("Python 3.12.3", true)
	smi := func(rows string) string {
		return "#!/bin/sh\ncase \"$1\" in\n--query-gpu=*) printf '" + rows + "';;\nesac\n"
	}
	deux := smi(`0, NVIDIA GeForce RTX 3070, 8192, 100, 8.6, 00000000:01:00.0, 4, 4, 16, 16, 7001\n1, NVIDIA GeForce RTX 5060 Ti, 16311, 5, 12.0, 00000000:02:00.0, 5, 5, 16, 16, 14001\n`)
	return map[string]map[string]string{
		"sans-carte":    {"python3": ok},
		"smi-en-panne":  {"nvidia-smi": "#!/bin/sh\nexit 9\n", "python3": ok},
		"smi-vide":      {"nvidia-smi": smi(``), "python3": ok},
		"vieille-carte": {"nvidia-smi": smi(`0, NVIDIA GeForce GTX 1080 Ti, 11264, 0, 6.1, 00000000:01:00.0, 3, 3, 16, 16, 5505\n`), "python3": ok},
		"petite-carte":  {"nvidia-smi": smi(`0, NVIDIA GeForce RTX 3050, 6144, 0, 8.6, 00000000:01:00.0, 4, 4, 8, 8, 7001\n`), "python3": ok},
		"sans-python":   {"nvidia-smi": deux},
		"python-casse":  {"nvidia-smi": deux, "python3": "#!/bin/sh\nexit 1\n"},
		"python-muet":   {"nvidia-smi": deux, "python3": py("Python inconnu", true)},
		"python-ancien": {"nvidia-smi": deux, "python3": py("Python 3.8.10", true)},
		"sans-venv":     {"nvidia-smi": deux, "python3": py("Python 3.11.2", false)},
		"deux-cartes":   {"nvidia-smi": deux, "python3": ok},
		"jumelles": {"nvidia-smi": smi(`0, NVIDIA GeForce RTX 4060 Ti, 16311, 0, 8.9, 00000000:01:00.0, 4, 4, 8, 8, 9001\n1, NVIDIA GeForce RTX 5060 Ti, 16311, 0, 12.0, 00000000:02:00.0, 5, 5, 8, 8, 14001\n2, NVIDIA GeForce GTX 1650, 4096, 0, 7.46, 00000000:03:00.0, 3, 3, 16, 16, 4001\n`),
			"python3": ok},
		"aide-ancienne": {"nvidia-smi": smi(`0, NVIDIA GeForce GTX 1070, 8192, 0, 6.1, 00000000:01:00.0, 3, 3, 16, 16, 4004\n1, NVIDIA GeForce RTX 4090, 24564, 0, 8.9, 00000000:02:00.0, 4, 4, 16, 16, 10501\n`),
			"python3": ok},
	}
}

// L'installeur factice : ses arguments, une barre de progression, puis ce
// que le modèle demandé lui fait faire.
const parityStrataSetup = `#!/bin/bash
echo "installeur : $*"
echo "cache : $XDG_CACHE_HOME"
printf 'modèle 10%%\r'
printf 'modèle 20%%\r'
echo "modèle téléchargé"
mkdir -p engine-cuda12 .venv/bin
fam=""; model=""; data=""
while [ $# -gt 0 ]; do
  case "$1" in
  --family) fam="$2"; shift;;
  --model) model="$2"; shift;;
  --data-dir) data="$2"; shift;;
  esac
  shift
done
case "$model" in
Q2_0) echo "erreur : téléchargement interrompu" >&2; exit 3;;
IQ2_XS) echo "rien d'écrit"; exit 0;;
esac
low=$(echo "$model" | tr 'A-Z' 'a-z')
if [ "$fam" = "swift" ]; then tag="swift-$low"; else tag="$low"; fi
mkdir -p "$data/packs/$tag"
printf '{"exe":"engine-cuda12/strata","args":["--pack","%s","--kv","int8","--spec","4","--max-context","131072","--vision","--vram-reserve-mib","700"],"vision":{"exe":"v","gpu":true},"gpu":0,"port":8080}\n' "$data/packs/$tag" > "strata-$tag.json"
echo "prêt : strata-$tag.json"
`

func TestPariteStrata(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("Strata n'est proposé que sous Linux x86_64")
	}
	home := testHome(t)
	resetBackendsState()
	t.Cleanup(resetBackendsState)

	tools := t.TempDir()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash introuvable")
	}
	mkdir, _ := exec.LookPath("mkdir")
	tr, _ := exec.LookPath("tr")
	t.Setenv("PATH", tools)
	machines := parityStrataMachines()
	setMachine := func(name string) {
		entries, _ := os.ReadDir(tools)
		for _, e := range entries {
			_ = os.Remove(filepath.Join(tools, e.Name()))
		}
		for n, p := range map[string]string{"bash": bash, "mkdir": mkdir, "tr": tr} {
			if err := os.Symlink(p, filepath.Join(tools, n)); err != nil {
				t.Fatal(err)
			}
		}
		for n, body := range machines[name] {
			if err := os.WriteFile(filepath.Join(tools, n), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}

	meminfo := filepath.Join(t.TempDir(), "meminfo")
	oldMem, oldDisk, oldBase, oldSHA, oldSvc := meminfoPath, strataDiskFree, strataReleaseBase, strataAssetSHA, serviceActionOSFn
	var mu sync.Mutex
	var disk int64
	var actions []string
	meminfoPath = meminfo
	strataDiskFree = func(string) int64 { mu.Lock(); defer mu.Unlock(); return disk }
	serviceActionOSFn = func(a string) error { mu.Lock(); defer mu.Unlock(); actions = append(actions, a); return nil }
	t.Cleanup(func() {
		meminfoPath, strataDiskFree, strataReleaseBase, strataAssetSHA, serviceActionOSFn = oldMem, oldDisk, oldBase, oldSHA, oldSvc
	})

	// La fausse release.
	var rel parityStrataRel
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cur := rel
		mu.Unlock()
		if cur.Status != 0 {
			w.WriteHeader(cur.Status)
			return
		}
		switch strings.TrimPrefix(r.URL.Path, "/") {
		case strataSrcAsset:
			_, _ = w.Write(parityTarGz(t, cur.Src))
		case strataEngineAsset:
			_, _ = w.Write([]byte(cur.Engine))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	strataReleaseBase = srv.URL + "/"
	setRelease := func(r parityStrataRel) {
		mu.Lock()
		rel = r
		mu.Unlock()
		sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
		strataAssetSHA = map[string]string{
			strataSrcAsset:    sum(parityTarGz(t, r.Src)),
			strataEngineAsset: sum([]byte(r.Engine)),
		}
		if r.BadSHA {
			strataAssetSHA[strataEngineAsset] = strings.Repeat("0", 64)
		}
	}

	mask := func(s string) string {
		s = strings.ReplaceAll(s, srv.URL, "{{REL}}")
		s = strings.ReplaceAll(s, home, "{{HOME}}")
		return parityTSRe.ReplaceAllString(s, `"$1":"{{T}}"`)
	}
	unmask := func(s string) string { return strings.ReplaceAll(s, "{{HOME}}", home) }
	handlers := map[string]http.HandlerFunc{
		"/api/strata":          handleStrata,
		"/api/strata/settings": handleStrataSettings,
		"/api/strata/install":  handleStrataInstall,
	}
	// L'arborescence : presets/ et strata/ (sans le contenu des archives, qui
	// diffère d'une implémentation à l'autre).
	tree := func() []string {
		var out []string
		for _, root := range []string{"presets", "strata"} {
			_ = filepath.WalkDir(filepath.Join(home, root), func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				rel, _ := filepath.Rel(home, p)
				info, err := os.Lstat(p)
				if err != nil {
					return nil
				}
				switch {
				case info.IsDir():
					out = append(out, "d "+rel)
				case strings.HasPrefix(rel, "strata/dl/") || strings.HasPrefix(rel, "strata/prebuilt/"):
					out = append(out, "f "+rel)
				default:
					b, _ := os.ReadFile(p)
					out = append(out, "f "+rel+" "+info.Mode().Perm().String()+" "+mask(string(b)))
				}
				return nil
			})
		}
		sort.Strings(out)
		return out
	}

	srcDir := filepath.Base(strataSrcDir())
	src := []parityArchEntry{
		{Name: srcDir + "/", Type: "dir", Mode: 0o755},
		{Name: srcDir + "/setup.sh", Type: "file", Body: parityStrataSetup, Mode: 0o755},
		{Name: srcDir + "/setup.py", Type: "file", Body: "# installeur\n", Mode: 0o644},
		{Name: srcDir + "/serve/server.py", Type: "file", Body: "# serveur\n", Mode: 0o644},
	}
	goodRel := parityStrataRel{Src: src, Engine: "moteur strata " + strataVersion}
	mem47 := "MemTotal:       49283072 kB\nMemFree:         1000000 kB\nMemAvailable:   41943040 kB\n"
	mem64 := "MemTotal:       67108864 kB\nMemAvailable:   65011712 kB\n"
	mem16 := "MemTotal:       16777216 kB\nMemAvailable:   12582912 kB\n"
	settings := func(preset string, ctx int, extra string) string {
		return `{"preset":"` + preset + `","ctx":` + jsonInt(ctx) + `,"kv":"int8","spec":4,"vision":false,"helper":true` + extra + `}`
	}

	var steps []parityStrataStep
	run := func(st parityStrataStep) {
		if st.Machine != "" {
			setMachine(st.Machine)
		}
		if st.Meminfo != "" {
			if err := os.WriteFile(meminfo, []byte(st.Meminfo), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if st.DiskFree != 0 {
			mu.Lock()
			disk = st.DiskFree
			mu.Unlock()
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
		for k, v := range st.State {
			if err := putStr(bkState, k, v); err != nil {
				t.Fatal(err)
			}
		}
		if st.Release != nil {
			setRelease(*st.Release)
		}
		mu.Lock()
		actions = nil
		mu.Unlock()
		var lock *tuneLock
		if st.Tune {
			l, err := tuneLockAcquire("web", false)
			if err != nil {
				t.Fatal(err)
			}
			lock = l
		}
		rec := httptest.NewRecorder()
		handlers[st.Path](rec, httptest.NewRequest(st.Method, st.Path, strings.NewReader(unmask(st.Body))))
		if lock != nil {
			lock.release()
		}
		st.Status = rec.Code
		if err := json.Unmarshal([]byte(mask(rec.Body.String())), &st.Out); err != nil {
			t.Fatalf("%s : corps illisible %q", st.Name, rec.Body.String())
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
		}
		mu.Lock()
		st.Actions = append([]string{}, actions...)
		mu.Unlock()
		st.After = map[string]string{}
		cfg := ReadConfig()
		for _, k := range []string{"ENGINE", "STRATA_CONFIG", "CTX", "STRATA_KV", "STRATA_HELPER"} {
			if v := cfg[k]; v != "" {
				st.After["config/"+k] = mask(v)
			}
		}
		st.Tree = tree()
		steps = append(steps, st)
	}

	for _, st := range []parityStrataStep{
		{Name: "aucune carte", Machine: "sans-carte", Meminfo: mem47, DiskFree: 400e9, Method: "GET", Path: "/api/strata"},
		{Name: "nvidia-smi en panne", Machine: "smi-en-panne", Method: "GET", Path: "/api/strata"},
		{Name: "nvidia-smi sans carte", Machine: "smi-vide", Method: "GET", Path: "/api/strata"},
		{Name: "carte trop ancienne", Machine: "vieille-carte", Method: "GET", Path: "/api/strata"},
		{Name: "carte trop petite", Machine: "petite-carte", Method: "GET", Path: "/api/strata"},
		{Name: "sans Python", Machine: "sans-python", Method: "GET", Path: "/api/strata"},
		{Name: "Python en panne", Machine: "python-casse", Method: "GET", Path: "/api/strata"},
		{Name: "Python illisible", Machine: "python-muet", Method: "GET", Path: "/api/strata"},
		{Name: "Python trop ancien", Machine: "python-ancien", Method: "GET", Path: "/api/strata"},
		{Name: "sans venv", Machine: "sans-venv", Method: "GET", Path: "/api/strata"},
		{Name: "deux cartes, 47 Go", Machine: "deux-cartes", Method: "GET", Path: "/api/strata"},
		{Name: "trois cartes, la plus récente devant", Machine: "jumelles", Method: "GET", Path: "/api/strata"},
		{Name: "carte d'aide trop ancienne", Machine: "aide-ancienne", Method: "GET", Path: "/api/strata"},
		{Name: "64 Go de RAM", Machine: "deux-cartes", Meminfo: mem64, Method: "GET", Path: "/api/strata"},
		{Name: "16 Go de RAM", Meminfo: mem16, Method: "GET", Path: "/api/strata"},
		{Name: "disque presque plein", Meminfo: mem47, DiskFree: 50e9, Method: "GET", Path: "/api/strata"},
		{Name: "disque juste trop petit", DiskFree: 78e9, Method: "GET", Path: "/api/strata"},
		{Name: "données déjà là", DiskFree: 60e9, Files: map[string]string{
			"strata/data/models/swift-IQ3_XXS/a.gguf": strings.Repeat("x", 2000),
		}, Method: "GET", Path: "/api/strata"},
		{Name: "meminfo sans MemAvailable", DiskFree: 400e9, Meminfo: "MemTotal:       49283072 kB\n", Method: "GET", Path: "/api/strata"},

		// Réglages.
		{Name: "réglages en GET", Meminfo: mem47, Method: "GET", Path: "/api/strata/settings"},
		{Name: "réglages illisibles", Method: "POST", Path: "/api/strata/settings", Body: `{"ctx":"x"}`},
		{Name: "réglages sans preset", Method: "POST", Path: "/api/strata/settings", Body: settings("", 65536, "")},
		{Name: "réglages, preset avec chemin", Method: "POST", Path: "/api/strata/settings", Body: settings("../x", 65536, "")},
		{Name: "réglages, preset absent", Method: "POST", Path: "/api/strata/settings", Body: settings("absent", 65536, "")},
		{Name: "réglages, preset llama.cpp", Files: map[string]string{"presets/llama.env": "MODEL=/m.gguf\n"},
			Method: "POST", Path: "/api/strata/settings", Body: settings("llama", 65536, "")},
		{Name: "réglages hors limites", Files: map[string]string{"presets/S.env": "# NAME=Strata S\nENGINE=strata\nSTRATA_HELPER_GPU=0\nCTX=131072\n"},
			Method: "POST", Path: "/api/strata/settings", Body: settings("S", 1000, "")},
		{Name: "réglages, experts inconnus", Method: "POST", Path: "/api/strata/settings", Body: settings("S", 65536, `,"experts":"nvme"`)},
		{Name: "réglages d'un preset inactif", Method: "POST", Path: "/api/strata/settings", Body: settings("S", 65536, `,"experts":"disk"`)},
		{Name: "réglages du preset actif", Config: map[string]string{"ENGINE": "strata", "STRATA_HELPER_GPU": "0", "CTX": "65536",
			"STRATA_KV": "int8", "STRATA_SPEC": "4", "STRATA_VISION": "0", "STRATA_HELPER": "1", "STRATA_EXPERTS": "disk"},
			Method: "POST", Path: "/api/strata/settings", Body: settings("S", 98304, "")},
		{Name: "réglages du preset actif pendant une optimisation", Tune: true,
			Method: "POST", Path: "/api/strata/settings", Body: settings("S", 98304, "")},

		// Installation.
		{Name: "installation en GET", Method: "GET", Path: "/api/strata/install"},
		{Name: "installation illisible", Method: "POST", Path: "/api/strata/install", Body: `[`},
		{Name: "installation d'un quant inconnu", Method: "POST", Path: "/api/strata/install", Body: `{"family":"swift","quant":"Q2_0"}`},
		{Name: "installation pendant une optimisation", Tune: true, Method: "POST", Path: "/api/strata/install", Body: `{"family":"swift","quant":"IQ3_XXS"}`},
		{Name: "installation sur une machine sans carte", Machine: "sans-carte", Method: "POST", Path: "/api/strata/install",
			Body: `{"family":"swift","quant":"IQ3_XXS"}`, Wait: true},
		{Name: "installation qui ne tient pas", Machine: "deux-cartes", Meminfo: mem16, Method: "POST", Path: "/api/strata/install",
			Body: `{"family":"qwen","quant":"IQ3_S"}`, Wait: true},
		{Name: "release introuvable", Meminfo: mem47, Release: &parityStrataRel{Status: 404, Src: src, Engine: "x"},
			Method: "POST", Path: "/api/strata/install", Body: `{"family":"swift","quant":"IQ3_XXS"}`, Wait: true},
		{Name: "moteur altéré", Release: &parityStrataRel{Src: src, Engine: "moteur strata " + strataVersion, BadSHA: true},
			Method: "POST", Path: "/api/strata/install", Body: `{"family":"swift","quant":"IQ3_XXS"}`, Wait: true},
		{Name: "installation", Release: &goodRel, Method: "POST", Path: "/api/strata/install",
			Body: `{"family":"swift","quant":"IQ3_XXS"}`, Wait: true},
		{Name: "après l'installation", Method: "GET", Path: "/api/strata"},
		{Name: "mode lancé connu, modèles inactifs", State: map[string]string{"strata_experts_mode": "drop"}, Files: map[string]string{
			"presets/STRATA FLASH NEXT CLASSIQUE IQ2_XS.env": "# NAME=Classique IQ2_XS\nENGINE=strata\nSTRATA_CONFIG={{HOME}}/strata/" + srcDir + "/strata-swift-iq3_xxs.json\n" +
				"STRATA_MAIN_GPU=1\nSTRATA_HELPER_GPU=-1\nSTRATA_EXPERTS=disk\n",
			"presets/STRATA FLASH NEXT CLASSIQUE Q2_0.env": "ENGINE=moe\nSTRATA_CONFIG={{HOME}}/strata/" + srcDir + "/strata-swift-iq3_xxs.json\n" +
				"STRATA_MAIN_GPU=0\nSTRATA_HELPER_GPU=1\nSTRATA_HELPER=0\nSTRATA_VISION=0\nSTRATA_EXPERTS=ram\n",
		}, Method: "GET", Path: "/api/strata"},
		{Name: "experts.bin et moteur du paquet", Files: map[string]string{
			"strata/data/packs/swift-iq3_xxs/experts.bin": "x",
		}, State: map[string]string{"strata_experts_mode": ""}, Method: "GET", Path: "/api/strata"},
		{Name: "RAM juste sous le seuil du mode hors RAM", Meminfo: "MemTotal:       49283072 kB\nMemAvailable:   44140625 kB\n",
			Method: "GET", Path: "/api/strata"},
		{Name: "RAM au-dessus du seuil du mode hors RAM", Meminfo: "MemTotal:       49283072 kB\nMemAvailable:   44433594 kB\n",
			Method: "GET", Path: "/api/strata"},
		{Name: "contexte plafonné en mode hors RAM", Files: map[string]string{
			"presets/STRATA FLASH NEXT SWIFT 1.5 IQ3_XXS.env": "# NAME=Qwen3.8 Flash Next Swift 1.5 IQ3_XXS (Strata)\nENGINE=strata\n" +
				"STRATA_CONFIG={{HOME}}/strata/" + srcDir + "/strata-swift-iq3_xxs.json\nSTRATA_MAIN_GPU=1\nSTRATA_HELPER_GPU=0\n" +
				"STRATA_VISION=1\nSTRATA_EXPERTS=auto\nSTRATA_DROP=1\nSTRATA_KV=fp16\nSTRATA_KV_RESIDENT=32768\nSTRATA_SPEC=3\nCTX=196608\nREASONING=off\n",
		}, Method: "GET", Path: "/api/strata"},
		{Name: "mode hors RAM coupé dans le preset", Files: map[string]string{
			"presets/STRATA FLASH NEXT SWIFT 1.5 IQ3_XXS.env": "# NAME=Qwen3.8 Flash Next Swift 1.5 IQ3_XXS (Strata)\nENGINE=strata\n" +
				"STRATA_CONFIG={{HOME}}/strata/" + srcDir + "/strata-swift-iq3_xxs.json\nSTRATA_MAIN_GPU=1\nSTRATA_HELPER_GPU=0\n" +
				"STRATA_VISION=1\nSTRATA_EXPERTS=auto\nSTRATA_DROP=0\nSTRATA_KV=fp16\nSTRATA_KV_RESIDENT=32768\nSTRATA_SPEC=3\nCTX=131072\nREASONING=off\n",
		}, Method: "GET", Path: "/api/strata"},
		{Name: "moteur d'un autre paquet", Meminfo: mem47, Files: map[string]string{
			"strata/" + srcDir + "/engine-cuda12/.loki-engine": "ajean-moe-engine-0.9-linux-x64-cuda12.zip\n",
		}, Method: "GET", Path: "/api/strata"},
		{Name: "réinstallation, moteur téléchargé périmé", Files: map[string]string{
			"strata/dl/" + strataEngineAsset: "ancien moteur",
		}, Method: "POST", Path: "/api/strata/install", Body: `{"family":"swift","quant":"IQ3_XXS"}`, Wait: true},
		{Name: "installeur en échec", Method: "POST", Path: "/api/strata/install", Body: `{"family":"qwen","quant":"Q2_0"}`, Wait: true},
		{Name: "installeur sans configuration", Method: "POST", Path: "/api/strata/install", Body: `{"family":"qwen","quant":"IQ2_XS"}`, Wait: true},
		{Name: "configuration de lancement disparue", Files: map[string]string{
			"strata/" + srcDir + "/strata-swift-iq3_xxs.json": "-",
		}, Method: "GET", Path: "/api/strata"},
		{Name: "configuration de lancement illisible", Files: map[string]string{
			"strata/" + srcDir + "/strata-swift-iq3_xxs.json": "[1]",
		}, Method: "GET", Path: "/api/strata"},
	} {
		run(st)
	}

	got, err := parityJSON(map[string]any{"machines": machines, "setup": parityStrataSetup, "steps": steps})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityStrataFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityStrataFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityStrataFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("Strata ne produit plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteStrata,\n"+
			"puis cargo test --test strata dit si le port Rust suit.\n--- obtenu ---\n%s", parityStrataFile, got)
	}
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
