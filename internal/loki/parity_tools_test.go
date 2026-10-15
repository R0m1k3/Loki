package loki

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// parity_tools_test.go — journal de référence des outils fichiers et du shell
// (lot 7 de la migration Rust).
//
// Une arborescence de test, une suite d'appels d'outils passés à execTool tels
// que la boucle les passe (libellé compris), dans l'ordre : leurs résultats,
// diffs et questions, puis l'arborescence finale, sont rangés dans
// rust/loki-rs/tests/parity_tools/outils.json. Le test Rust (tests/tools.rs)
// rejoue les mêmes appels dans tools::exec_local et doit retrouver ce journal.
// Chemins réécrits en {{ROOT}}, durées du shell en {{DUR}}.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteOutils

const (
	parityToolsFile    = "../../rust/loki-rs/tests/parity_tools/outils.json"
	parityToolsGitFile = "../../rust/loki-rs/tests/parity_tools/git.json"
)

type parityFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	// Âge en secondes (glob trie par date de modification).
	Age int `json:"age"`
}

type parityToolCase struct {
	Name string         `json:"name"`
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
	Code bool           `json:"code,omitempty"`
	// Unix : commande shell propre à bash.
	Unix bool `json:"unix,omitempty"`

	Label   string     `json:"label"`
	Result  string     `json:"result"`
	Diff    []DiffLine `json:"diff,omitempty"`
	Added   int        `json:"added,omitempty"`
	Removed int        `json:"removed,omitempty"`
	Ask     *AskEvent  `json:"ask,omitempty"`
}

// parityGitCmd : une commande git de préparation, lancée dans Dir (relatif
// à {{ROOT}}) avant les appels.
type parityGitCmd struct {
	Dir  string   `json:"dir"`
	Args []string `json:"args"`
}

type parityToolsFixture struct {
	// Dossier de travail et dossier mémoire, relatifs à {{ROOT}}.
	Cwd       string         `json:"cwd"`
	MemoryDir string         `json:"memory_dir"`
	Files     []parityFile   `json:"files"`
	GitSetup  []parityGitCmd `json:"git_setup,omitempty"`
	// Edits : fichiers écrits APRÈS la préparation git (ce que montreront
	// status et diff).
	Edits []parityFile      `json:"edits,omitempty"`
	Cases []parityToolCase  `json:"cases"`
	After map[string]string `json:"after"`
}

func parityToolTree() []parityFile {
	long := strings.Repeat("é", 600)
	many := strings.Repeat("aiguille\n", 70)
	return []parityFile{
		{Path: "ws/a.txt", Content: "ligne 1\nligne 2\nligne 3\n", Age: 50},
		{Path: "ws/b.txt", Content: "alpha\nbeta\n", Age: 40},
		{Path: "ws/vide.txt", Content: "", Age: 30},
		{Path: "ws/long.txt", Content: long + "\nfin\n", Age: 20},
		{Path: "ws/crlf.txt", Content: "un\r\ndeux\r\ntrois\r\n", Age: 10},
		{Path: "ws/bin.dat", Content: "PK\x00\x01binaire", Age: 60},
		{Path: "ws/src/main.go", Content: "package main\n\nfunc main() { x := 1 }\n", Age: 5},
		{Path: "ws/src/lib/util.go", Content: "package lib\n// x marque\n", Age: 3},
		{Path: "ws/src/lib/notes.md", Content: "x dans les notes\n", Age: 4},
		{Path: "ws/node_modules/dep/index.js", Content: "ligne cachée\n", Age: 70},
		{Path: "ws/beaucoup.txt", Content: many, Age: 80},
		{Path: "dehors.txt", Content: "hors du dossier\n", Age: 90},
	}
}

func parityToolCases() []parityToolCase {
	c := func(name, tool string, args map[string]any) parityToolCase {
		return parityToolCase{Name: name, Tool: tool, Args: args}
	}
	code := func(p parityToolCase) parityToolCase { p.Code = true; return p }
	unix := func(p parityToolCase) parityToolCase { p.Unix = true; return p }
	return []parityToolCase{
		c("lecture", "read", map[string]any{"file": "a.txt"}),
		c("lecture_fenetre", "read", map[string]any{"file": "a.txt", "offset": 2, "limit": 1}),
		c("lecture_fenetre_decimale", "read", map[string]any{"file": "a.txt", "offset": 2.7, "limit": "3"}),
		c("lecture_chemin_vide", "read", map[string]any{"file": "  "}),
		c("lecture_absent", "read", map[string]any{"file": "absent.txt"}),
		c("lecture_dossier", "read", map[string]any{"file": "src"}),
		c("lecture_binaire", "read", map[string]any{"file": "bin.dat"}),
		c("lecture_au_dela", "read", map[string]any{"file": "a.txt", "offset": 99}),
		c("lecture_ligne_longue", "read", map[string]any{"file": "long.txt"}),
		c("lecture_fichier_vide", "read", map[string]any{"file": "vide.txt"}),
		c("lecture_absolue", "read", map[string]any{"file": "{{ROOT}}/dehors.txt"}),
		c("lecture_relative_sortante", "read", map[string]any{"file": "src/../../dehors.txt"}),
		code(c("lecture_code_hors_dossier", "read", map[string]any{"file": "../dehors.txt"})),
		code(c("lecture_code", "read", map[string]any{"file": "b.txt"})),

		c("grep", "grep", map[string]any{"pattern": "ligne [12]"}),
		c("grep_filtre", "grep", map[string]any{"pattern": "x", "glob": "*.go"}),
		c("grep_filtre_classe", "grep", map[string]any{"pattern": "x", "glob": "[mn]*"}),
		c("grep_fichier", "grep", map[string]any{"pattern": "beta", "path": "b.txt"}),
		c("grep_sous_dossier", "grep", map[string]any{"pattern": "package", "path": "src/lib"}),
		c("grep_point", "grep", map[string]any{"pattern": "alpha", "path": "."}),
		c("grep_aucun", "grep", map[string]any{"pattern": "zzz"}),
		c("grep_motif_vide", "grep", map[string]any{"pattern": " "}),
		c("grep_plafond", "grep", map[string]any{"pattern": "aiguille"}),
		c("grep_absent", "grep", map[string]any{"pattern": "a", "path": "nulle-part"}),
		code(c("grep_code_hors_dossier", "grep", map[string]any{"pattern": "a", "path": "{{ROOT}}"})),

		c("glob_go", "glob", map[string]any{"pattern": "**/*.go"}),
		c("glob_racine", "glob", map[string]any{"pattern": "*.txt"}),
		c("glob_point", "glob", map[string]any{"pattern": "./src/*"}),
		c("glob_aucun", "glob", map[string]any{"pattern": "*.rs"}),
		c("glob_vide", "glob", map[string]any{"pattern": ""}),

		c("question", "ask", map[string]any{"question": "Lequel ?", "options": []any{"a", " ", "b", "c", "d", "e"}}),
		c("question_vide", "ask", map[string]any{"question": ""}),

		c("ecriture_neuve", "write", map[string]any{"file": "neuf/n.txt", "content": "un\ndeux\n"}),
		c("ecriture_existant", "write", map[string]any{"file": "b.txt", "content": "remplacé\n"}),
		c("ecriture_memoire", "write", map[string]any{"file": "{{MEM}}/x.md", "content": "x\n"}),
		c("ecriture_chemin_vide", "write", map[string]any{"file": "", "content": "x"}),
		code(c("ecriture_code_non_lu", "write", map[string]any{"file": "src/lib/notes.md", "content": "x\n"})),
		code(c("ecriture_code_hors_dossier", "write", map[string]any{"file": "../evade.txt", "content": "x\n"})),
		code(c("ecriture_code_neuve", "write", map[string]any{"file": "code.txt", "content": "c\n"})),

		c("edition", "edit", map[string]any{"file": "a.txt", "old": "ligne 2", "new": "ligne deux"}),
		c("edition_deja_faite", "edit", map[string]any{"file": "a.txt", "old": "ligne 2", "new": "ligne deux"}),
		c("edition_introuvable", "edit", map[string]any{"file": "a.txt", "old": "zzz", "new": "y"}),
		c("edition_ambigue", "edit", map[string]any{"file": "a.txt", "old": "ligne", "new": "l"}),
		c("edition_crlf", "edit", map[string]any{"file": "crlf.txt", "old": "deux\ntrois", "new": "2\n3"}),
		c("edition_old_vide", "edit", map[string]any{"file": "a.txt", "old": "", "new": "y"}),
		c("edition_fichier_absent", "edit", map[string]any{"file": "absent.txt", "old": "a", "new": "b"}),
		code(c("edition_code_non_lu", "edit", map[string]any{"file": "src/main.go", "old": "x := 1", "new": "x := 2"})),
		code(c("edition_code_apres_lecture", "edit", map[string]any{"file": "b.txt", "old": "remplacé", "new": "rem"})),

		unix(c("shell", "bash", map[string]any{"command": "echo bonjour"})),
		unix(c("shell_sortie_et_erreurs", "bash", map[string]any{"command": "echo out; echo err >&2; exit 3"})),
		unix(c("shell_tail", "bash", map[string]any{"command": "printf 'a\\nb\\nc\\n' | tail -2"})),
		unix(c("shell_ansi", "bash", map[string]any{"command": "printf '\\033[31mrouge\\033[0m\\n'"})),
		unix(c("shell_dossier", "bash", map[string]any{"command": "ls src"})),
		unix(c("shell_longue_sortie", "bash", map[string]any{"command": "seq 1 3000"})),
		unix(c("shell_delai", "bash", map[string]any{"command": "echo avant; sleep 3", "timeout": 1})),
		c("shell_dangereux", "bash", map[string]any{"command": "rm -rf /"}),
		unix(c("shell_memoire", "bash", map[string]any{"command": "cat {{MEM}}/x.md"})),
		unix(code(c("shell_code_arriere_plan", "bash", map[string]any{"command": "sleep 1 &"}))),
	}
}

// subst remplace {{ROOT}} et {{MEM}} dans les chaînes des arguments.
func paritySubst(v any, root, mem string) any {
	switch x := v.(type) {
	case string:
		return strings.NewReplacer("{{MEM}}", mem, "{{ROOT}}", root).Replace(x)
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = paritySubst(e, root, mem)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = paritySubst(e, root, mem)
		}
		return out
	}
	return v
}

var parityDurRe = regexp.MustCompile(`(exit: -?\d+) · [0-9.]+[a-zµ]+(?:[0-9.]+[a-zµ]+)*`)

func parityNorm(s, root string) string {
	s = strings.ReplaceAll(s, root, "{{ROOT}}")
	return parityDurRe.ReplaceAllString(s, "$1 · {{DUR}}")
}

func TestPariteOutils(t *testing.T) {
	runParityTools(t, parityToolsFile, parityToolsFixture{Files: parityToolTree(), Cases: parityToolCases()})
}

// Outils git (mode Code) : deux dépôts, l'un avec un commit et des
// modifications, l'autre vide ; aucun appel ne sort sur le réseau.
func TestPariteOutilsGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git absent")
	}
	// Ni configuration globale ni système : sorties de git identiques partout.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	id := []string{"-c", "user.name=Loki", "-c", "user.email=loki@test"}
	c := func(name, tool string, args map[string]any) parityToolCase {
		return parityToolCase{Name: name, Tool: tool, Args: args}
	}
	fx := parityToolsFixture{
		Files: []parityFile{
			{Path: "ws/depot/a.txt", Content: "un\ndeux\ntrois\n", Age: 30},
			{Path: "ws/depot/b.txt", Content: "bêta\n", Age: 30},
			{Path: "ws/vide/x.txt", Content: "x\n", Age: 30},
		},
		GitSetup: []parityGitCmd{
			{Dir: "ws/depot", Args: []string{"init", "-q", "-b", "main"}},
			{Dir: "ws/depot", Args: []string{"add", "a.txt", "b.txt"}},
			{Dir: "ws/depot", Args: append(append([]string{}, id...), "commit", "-q", "-m", "départ")},
			{Dir: "ws/vide", Args: []string{"init", "-q", "-b", "main"}},
		},
		Edits: []parityFile{
			{Path: "ws/depot/a.txt", Content: "un\n2\ntrois\n", Age: 10},
			{Path: "ws/depot/neuf.txt", Content: "neuf\n", Age: 10},
		},
		Cases: []parityToolCase{
			c("statut_ambigu", "git_status", map[string]any{}),
			c("statut", "git_status", map[string]any{"dir": "depot"}),
			c("diff", "git_diff", map[string]any{"dir": "depot"}),
			c("diff_fichier", "git_diff", map[string]any{"dir": "depot", "file": "b.txt"}),
			c("diff_sans_commit", "git_diff", map[string]any{"dir": "vide"}),
			c("statut_sans_commit", "git_status", map[string]any{"dir": "vide"}),
			c("dossier_sortant", "git_status", map[string]any{"dir": "../x"}),
			c("dossier_absent", "git_status", map[string]any{"dir": "nulle-part"}),
			c("clone_url_locale", "git_clone", map[string]any{"url": "file:///etc"}),
			c("clone_commande", "git_clone", map[string]any{"url": "ext::sh -c id"}),
			c("clone_existant", "git_clone", map[string]any{"url": "https://exemple.invalid/u/depot.git"}),
			c("clone_dossier_sortant", "git_clone", map[string]any{"url": "git@exemple.invalid:u/r", "dir": "../r"}),
		},
	}
	runParityTools(t, parityToolsGitFile, fx)
}

func runParityTools(t *testing.T, file string, fx parityToolsFixture) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("journal tiré sous Unix")
	}
	root := testHome(t)
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r // macOS : /var → /private/var, comme le verraient les outils
	}
	t.Setenv("LOKI_HOME", root)
	ws := filepath.Join(root, "ws")
	now := time.Now()
	for _, f := range fx.Files {
		p := filepath.Join(root, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.Content), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := now.Add(-time.Duration(f.Age) * time.Second)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	for _, g := range fx.GitSetup {
		cmd := exec.Command("git", g.Args...)
		cmd.Dir = filepath.Join(root, filepath.FromSlash(g.Dir))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v : %v\n%s", g.Args, err, out)
		}
	}
	for _, f := range fx.Edits {
		p := filepath.Join(root, filepath.FromSlash(f.Path))
		if err := os.WriteFile(p, []byte(f.Content), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := now.Add(-time.Duration(f.Age) * time.Second)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	setTaskWorkspace(ws)
	t.Cleanup(func() { setTaskWorkspace("") })
	mem := memoryDir()
	relMem, err := filepath.Rel(root, mem)
	if err != nil || strings.HasPrefix(relMem, "..") {
		t.Fatalf("dossier mémoire hors de la racine : %s", mem)
	}

	fx.Cwd, fx.MemoryDir = "ws", filepath.ToSlash(relMem)
	cases := fx.Cases
	fx.Cases = nil
	for _, pc := range cases {
		args := paritySubst(pc.Args, root, mem).(map[string]any)
		// Les nombres passent par JSON, comme ceux d'un vrai appel.
		b, _ := json.Marshal(args)
		args = map[string]any{}
		_ = json.Unmarshal(b, &args)
		label := toolCallLabel(pc.Tool, args)
		res := execTool(context.Background(), pc.Tool, args, label, Caps{Code: pc.Code})
		pc.Label = parityNorm(label, root)
		pc.Result = parityNorm(res.Result, root)
		pc.Diff, pc.Added, pc.Removed, pc.Ask = res.Diff, res.Added, res.Removed, res.Ask
		fx.Cases = append(fx.Cases, pc)
	}
	fx.After = map[string]string{}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if !strings.HasPrefix(rel, "ws/") && rel != "dehors.txt" && rel != "evade.txt" {
			return nil // base, configuration : propres au Go
		}
		if strings.Contains(rel, "/.git/") {
			return nil // index, objets : datés
		}
		b, _ := os.ReadFile(p)
		fx.After[rel] = string(b)
		return nil
	})
	got, err := parityJSON(fx)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if string(got) != string(want) {
		t.Errorf("les outils ne produisent plus le journal rangé dans %s.\n"+
			"Si c'est voulu : LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteOutils,\n"+
			"puis cargo test --test tools dit si le port Rust suit.\n--- obtenu ---\n%s", file, got)
	}
}
