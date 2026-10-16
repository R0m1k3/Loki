//go:build linux

package loki

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// parity_build_test.go — journal de référence de la compilation de llama.cpp
// depuis les sources (lot 41 de la migration Rust) : installation, mise à jour,
// recompilation propre, échec du build, outils manquants, backends custom.
// Les outils (git, cmake, compilateur) sont de faux scripts rangés dans le
// journal, seuls sur le PATH ; le job est rejoué jusqu'au bout et sa vue
// (lignes, phase, erreur, commits), BIN et la présence de build.old sont
// relevés après chaque étape. Rangé dans
// rust/loki-rs/tests/parity_backends/build.json ; tests/backends.rs le
// rejoue par la façade.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteCompilation

const parityBuildFile = "../../rust/loki-rs/tests/parity_backends/build.json"

// Les faux outils : le dépôt « distant » est un fichier à côté des scripts.
var parityBuildTools = map[string]string{
	"git": `#!/bin/sh
d=$(dirname "$0")
case "$1" in
  clone)
    dir="$4"
    if [ -f "$(dirname "$dir")/CLONE_FAIL" ]; then echo "fatal: repository '$3' not found" >&2; exit 128; fi
    mkdir -p "$dir/.git"
    cat "$d/remote-sha" > "$dir/.git/HEAD_SHA"
    echo "Cloning into '$dir'..." >&2 ;;
  fetch)
    exit 0 ;;
  rev-parse)
    case "$2" in
      --short)
        if [ "$3" = HEAD ]; then head -c 7 .git/HEAD_SHA; else head -c 7 "$d/remote-sha"; fi
        echo ;;
      --abbrev-ref) echo master ;;
      HEAD) cat .git/HEAD_SHA ;;
      *) cat "$d/remote-sha" ;;
    esac ;;
  pull)
    cat "$d/remote-sha" > .git/HEAD_SHA
    echo "Fast-forward" ;;
  checkout)
    echo "$2" > .git/REF
    echo "HEAD is now at $2" >&2 ;;
esac
`,
	"cmake": `#!/bin/sh
if [ "$1" = "-B" ]; then
  mkdir -p build
  echo "-- The C compiler identification is GNU 13.2.0"
  echo "-- Configuring done (0.5s)"
  printf 'CMAKE_HOME_DIRECTORY:INTERNAL=%s\n' "$(pwd)" > build/CMakeCache.txt
  echo "-- Build files have been written to: build"
  exit 0
fi
if [ -f FAIL_BUILD ]; then
  echo "[ 10%] Building CXX object common/CMakeFiles/common.dir/arg.cpp.o"
  echo "common/arg.cpp:12:5: error: 'boom' was not declared in this scope"
  exit 2
fi
echo "[ 50%] Building CXX object common/CMakeFiles/common.dir/arg.cpp.o"
echo "[ 90%] Building CUDA object ggml/src/ggml-cuda/CMakeFiles/ggml-cuda.dir/fattn.cu.o"
echo "[100%] Linking CXX executable ../bin/llama-server"
mkdir -p build/bin
printf '#!/bin/sh\necho llama-server\n' > build/bin/llama-server
chmod +x build/bin/llama-server
`,
	"cc":   "#!/bin/sh\n",
	"c++":  "#!/bin/sh\n",
	"make": "#!/bin/sh\n",
}

type parityBuildStep struct {
	Name     string            `json:"name"`
	Remote   string            `json:"remote,omitempty"`   // sha du dépôt distant
	Files    map[string]string `json:"files,omitempty"`    // relatifs à LOKI_HOME ; "-" supprime
	NoTools  []string          `json:"no_tools,omitempty"` // faux outils retirés pour l'étape
	Action   string            `json:"action"`             // install, update, custom
	Force    bool              `json:"force,omitempty"`
	Clean    bool              `json:"clean,omitempty"`
	URL      string            `json:"url,omitempty"`
	BName    string            `json:"bname,omitempty"`
	Ref      string            `json:"ref,omitempty"`
	Job      map[string]any    `json:"job"`
	Bin      string            `json:"bin"`
	BuildOld []string          `json:"build_old"` // dépôts qui ont un build.old
}

type parityBuildJournal struct {
	Tools map[string]string `json:"tools"`
	Steps []parityBuildStep `json:"steps"`
}

var parityBuildJobsRe = regexp.MustCompile(`jobs=[0-9]+`)

func TestPariteCompilation(t *testing.T) {
	home := testHome(t)
	fake := filepath.Join(t.TempDir(), "outils")
	sys := filepath.Join(t.TempDir(), "sys")
	for _, d := range []string{fake, sys} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []string{"mkdir", "cat", "head", "dirname", "chmod"} {
		p, err := exec.LookPath(c)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p, filepath.Join(sys, c)); err != nil {
			t.Fatal(err)
		}
	}
	writeTool := func(name string) {
		if err := os.WriteFile(filepath.Join(fake, name), []byte(parityBuildTools[name]), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name := range parityBuildTools {
		writeTool(name)
	}
	t.Setenv("PATH", fake+string(os.PathListSeparator)+sys)
	t.Setenv("LOKI_ENGINE_BIN", "")
	mask := func(s string) string {
		s = strings.ReplaceAll(s, home, "{{HOME}}")
		return parityBuildJobsRe.ReplaceAllString(s, "jobs={{N}}")
	}
	var steps []parityBuildStep
	run := func(st parityBuildStep) {
		if st.Remote != "" {
			if err := os.WriteFile(filepath.Join(fake, "remote-sha"), []byte(st.Remote+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		parityApplyFiles(t, home, st.Files)
		for _, n := range st.NoTools {
			_ = os.Remove(filepath.Join(fake, n))
		}
		var body func()
		switch st.Action {
		case "install":
			body = func() { lcRunInstall(st.Force) }
		case "update":
			body = func() { lcRunUpdate(st.Clean) }
		case "custom":
			body = func() { lcRunCustomInstall(st.URL, st.BName, st.Ref) }
		}
		if err := startLcJob(st.Action, body); err != nil {
			t.Fatalf("%s : %v", st.Name, err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for {
			if snap := lcJobSnapshot(0, true); snap["running"] == false {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s : le job ne finit pas", st.Name)
			}
			time.Sleep(10 * time.Millisecond)
		}
		snap := lcJobSnapshot(0, true)
		delete(snap, "started_at")
		delete(snap, "ended_at")
		b, _ := json.Marshal(snap)
		var job map[string]any
		_ = json.Unmarshal([]byte(mask(string(b))), &job)
		st.Job = job
		st.Bin = mask(ReadConfig()["BIN"])
		st.BuildOld = []string{}
		entries, _ := os.ReadDir(filepath.Join(home, "backends"))
		for _, e := range entries {
			if isDir(filepath.Join(home, "backends", e.Name(), "build.old")) {
				st.BuildOld = append(st.BuildOld, e.Name())
			}
		}
		for _, n := range st.NoTools {
			writeTool(n)
		}
		steps = append(steps, st)
	}

	run(parityBuildStep{Name: "mise à jour sans dépôt", Remote: "aaaaaaa1111111111111111111111111111111", Action: "update"})
	run(parityBuildStep{Name: "installation", Action: "install"})
	run(parityBuildStep{Name: "mise à jour : déjà à jour", Action: "update"})
	run(parityBuildStep{Name: "mise à jour : nouveau commit", Remote: "bbbbbbb2222222222222222222222222222222", Action: "update"})
	run(parityBuildStep{Name: "recompilation propre", Action: "update", Clean: true})
	run(parityBuildStep{Name: "installation sur un dépôt présent", Remote: "ccccccc3333333333333333333333333333333", Action: "install"})
	run(parityBuildStep{Name: "build en échec", Remote: "ddddddd4444444444444444444444444444444",
		Files: map[string]string{"backends/llama.cpp/FAIL_BUILD": "1"}, Action: "update"})
	run(parityBuildStep{Name: "installation forcée", Files: map[string]string{"backends/llama.cpp/FAIL_BUILD": "-"},
		Action: "install", Force: true})
	run(parityBuildStep{Name: "cmake absent", Action: "update", NoTools: []string{"cmake"}})
	run(parityBuildStep{Name: "compilateur absent", Action: "update", NoTools: []string{"cc", "make"}})
	run(parityBuildStep{Name: "clone en échec", Files: map[string]string{"backends/llama.cpp": "-", "backends/CLONE_FAIL": "1"},
		Action: "install"})
	run(parityBuildStep{Name: "custom : URL invalide", Files: map[string]string{"backends/CLONE_FAIL": "-"},
		Action: "custom", URL: "ftp://exemple/x"})
	run(parityBuildStep{Name: "custom : nouveau fork", Action: "custom", URL: "https://github.com/prism/llama.cpp.git"})
	run(parityBuildStep{Name: "custom : nom assaini, sur un ref", Action: "custom", URL: "https://github.com/prism/llama.cpp.git", BName: "Mon Fork/../x", Ref: "v2"})
	run(parityBuildStep{Name: "custom : dossier existant sur un ref", Action: "custom", URL: "git@github.com:prism/llama.cpp.git", Ref: "v3"})

	got, err := json.MarshalIndent(parityBuildJournal{Tools: parityBuildTools, Steps: steps}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityBuildFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityBuildFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityBuildFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("la compilation ne produit plus le journal rangé dans %s.\n"+
			"LOKI_PARITY_UPDATE=1 pour le régénérer si le changement est voulu.", parityBuildFile)
	}
}
