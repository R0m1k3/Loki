package loki

// code_tools.go — outils fichiers du MODE CODE : read, grep, glob, ask.
// Inspirés des outils d'OpenFox (read/grep/ask — voir NOTICE.md), réécrits en
// Go. Sans eux, le modèle lit les fichiers au `cat` via bash : sortie non
// bornée, pas de numéros de ligne, et aucun moyen de savoir ce qu'il a lu
// (voir code_tracker.go).

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	readMaxLines   = 1000 // lignes par appel (offset/limit pour la suite)
	readDefLines   = 400
	readMaxLineLen = 500 // une ligne minifiée ne doit pas manger le contexte
	grepMaxHits    = 60
	globMaxHits    = 200
)

// skipDirs : dossiers ignorés par grep/glob — dépendances et artefacts, jamais
// du code à lire. `.git` compris : son contenu binaire pollue toute recherche.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, "target": true, "__pycache__": true, ".venv": true,
	"venv": true, ".next": true, ".cache": true,
}

func readTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "read",
			Description: "Read a file with 1-indexed line numbers (offset/limit for long files). ALWAYS use this instead of cat/type: output is bounded and reading a file is REQUIRED before you may edit or overwrite it.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file":   map[string]any{"type": "string", "description": "Path (relative to the working folder)"},
					"offset": map[string]any{"type": "integer", "description": "Start line (default 1)"},
					"limit":  map[string]any{"type": "integer", "description": fmt.Sprintf("Lines (default %d, max %d)", readDefLines, readMaxLines)},
				},
				"required": []string{"file"},
			},
		},
	}
}

func grepTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "grep",
			Description: "Search file contents with a Go regular expression. Returns path:line: text matches (bounded). path may be a file or a directory (default: whole working folder).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string", "description": "Go regexp"},
					"path":    map[string]any{"type": "string", "description": "File or directory to search (default .)"},
					"glob":    map[string]any{"type": "string", "description": "Only files matching this glob (e.g. *.go)"},
				},
				"required": []string{"pattern"},
			},
		},
	}
}

func globTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "glob",
			Description: "List files matching a glob pattern (** supported, e.g. **/*.go), most recently modified first.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string", "description": "Glob, e.g. src/**/*.ts"},
				},
				"required": []string{"pattern"},
			},
		},
	}
}

func askTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "ask",
			Description: "Ask the user a clarifying question with 2-4 short options, then END your turn. Their answer arrives as the next user message. Use when a decision is genuinely theirs (scope, destructive change, ambiguous requirement) — not for things you can check yourself.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question": map[string]any{"type": "string", "description": "The question"},
					"options":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "2-4 short answer options"},
				},
				"required": []string{"question"},
			},
		},
	}
}

// toolRead lit un fichier avec numéros de ligne. Enregistre la lecture dans le
// tracker (code_tracker.go) — c'est ce qui autorise edit/write ensuite.
func toolRead(args map[string]any, codeMode bool) string {
	file, _ := args["file"].(string)
	if strings.TrimSpace(file) == "" {
		return "[erreur] chemin vide"
	}
	path := resolveAgentPath(file)
	if codeMode && !codePathAllowed(path) {
		return refusedPathResult(file)
	}
	offset, limit := 1, readDefLines
	if v, ok := args["offset"].(float64); ok && int(v) > 0 {
		offset = int(v)
	}
	if v, ok := args["limit"].(float64); ok && int(v) > 0 {
		limit = int(v)
	}
	if limit > readMaxLines {
		limit = readMaxLines
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "[erreur] " + err.Error()
	}
	if isBinary(b) {
		fi, _ := os.Stat(path)
		size := int64(len(b))
		if fi != nil {
			size = fi.Size()
		}
		return fmt.Sprintf("[binaire] %s (%d octets) — pas un fichier texte", file, size)
	}
	lines := strings.Split(string(b), "\n")
	total := len(lines)
	// Fichier fini par \n : Split laisse une dernière entrée vide, pas une ligne.
	if total > 0 && lines[total-1] == "" {
		total--
	}
	if offset > total {
		return fmt.Sprintf("[erreur] offset %d > %d lignes", offset, total)
	}
	end := offset - 1 + limit
	if end > total {
		end = total
	}
	var out strings.Builder
	for i := offset - 1; i < end; i++ {
		l := lines[i]
		if utf8.RuneCountInString(l) > readMaxLineLen {
			r := []rune(l)
			l = string(r[:readMaxLineLen]) + "…"
		}
		fmt.Fprintf(&out, "%d: %s\n", i+1, l)
	}
	if end < total {
		fmt.Fprintf(&out, "… (%d lignes restantes — relis avec offset=%d)\n", total-end, end+1)
	}
	trackerNoteRead(path)
	return strings.TrimRight(out.String(), "\n")
}

// toolGrep cherche un motif regexp dans les fichiers.
func toolGrep(args map[string]any, codeMode bool) string {
	pattern, _ := args["pattern"].(string)
	if strings.TrimSpace(pattern) == "" {
		return "[erreur] motif vide"
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "[erreur] motif invalide : " + err.Error()
	}
	root := convWorkspace()
	if p, _ := args["path"].(string); strings.TrimSpace(p) != "" && p != "." {
		root = resolveAgentPath(p)
	}
	if codeMode && !codePathAllowed(root) {
		return refusedPathResult(root)
	}
	globPat, _ := args["glob"].(string)
	var hits []string
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // dossier illisible : on continue ailleurs
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if globPat != "" {
			if ok, _ := filepath.Match(globPat, d.Name()); !ok {
				return nil
			}
		}
		if len(hits) >= grepMaxHits {
			return filepath.SkipAll
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil || isBinary(b) {
			return nil
		}
		rel, rrr := filepath.Rel(root, path)
		if rrr != nil {
			rel = path
		}
		for i, line := range strings.Split(string(b), "\n") {
			if re.MatchString(line) {
				if utf8.RuneCountInString(line) > readMaxLineLen {
					r := []rune(line)
					line = string(r[:readMaxLineLen]) + "…"
				}
				hits = append(hits, fmt.Sprintf("%s:%d: %s", filepath.ToSlash(rel), i+1, strings.TrimSpace(line)))
				if len(hits) >= grepMaxHits {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if walkErr != nil && len(hits) == 0 {
		return "[erreur] " + walkErr.Error()
	}
	if len(hits) == 0 {
		return "[aucun résultat]"
	}
	out := strings.Join(hits, "\n")
	if len(hits) >= grepMaxHits {
		out += fmt.Sprintf("\n… (plafond de %d résultats atteint — affine le motif)", grepMaxHits)
	}
	return out
}

// toolGlob liste les fichiers correspondant à un motif, plus récents d'abord.
func toolGlob(args map[string]any, codeMode bool) string {
	pattern, _ := args["pattern"].(string)
	if strings.TrimSpace(pattern) == "" {
		return "[erreur] motif vide"
	}
	root := convWorkspace()
	re, err := globToRegexp(pattern)
	if err != nil {
		return "[erreur] motif invalide : " + err.Error()
	}
	type hit struct {
		rel string
		mod int64
	}
	var hits []hit
	filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !re.MatchString(rel) {
			return nil
		}
		info, ierr := d.Info()
		mod := int64(0)
		if ierr == nil {
			mod = info.ModTime().UnixNano()
		}
		hits = append(hits, hit{rel, mod})
		return nil
	})
	if len(hits) == 0 {
		return "[aucun fichier]"
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].mod > hits[j].mod })
	if len(hits) > globMaxHits {
		hits = hits[:globMaxHits]
	}
	var out strings.Builder
	for _, h := range hits {
		out.WriteString(h.rel + "\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

// globToRegexp convertit un glob (avec **) en expression régulière sur des
// chemins en séparateurs '/'. `**` = n'importe quelle profondeur, `*` = tout
// sauf '/', `?` = un caractère sauf '/'.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	pattern = strings.TrimPrefix(filepath.ToSlash(pattern), "./")
	var b strings.Builder
	b.WriteString("^")
	i := 0
	for i < len(pattern) {
		c := pattern[i]
		switch c {
		case '*':
			if strings.HasPrefix(pattern[i:], "**/") {
				b.WriteString(`(?:[^/]+/)*`)
				i += 3
				continue
			}
			if strings.HasPrefix(pattern[i:], "**") {
				b.WriteString(`.*`)
				i += 2
				continue
			}
			b.WriteString(`[^/]*`)
		case '?':
			b.WriteString(`[^/]`)
		case '.', '+', '(', ')', '|', '[', ']', '{', '}', '^', '$', '\\':
			b.WriteString(`\` + string(c))
		default:
			b.WriteByte(c)
		}
		i++
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// isBinary : heuristique simple — un NUL dans les premiers 8 Kio.
func isBinary(b []byte) bool {
	n := len(b)
	if n > 8192 {
		n = 8192
	}
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}

// toolAsk formate la « réponse » de l'outil ask : la question part vers l'UI
// via un événement dédié (voir runChat) ; ici on dit au modèle de clore son
// tour et d'attendre.
func toolAsk(args map[string]any) string {
	q, _ := args["question"].(string)
	if strings.TrimSpace(q) == "" {
		return "[erreur] question vide"
	}
	return "[question posée à l'utilisateur] Termine ton tour MAINTENANT sans appeler d'autre outil : sa réponse arrivera comme prochain message."
}

// askOptions extrait les options (chaînes) des arguments de l'outil ask.
func askOptions(args map[string]any) []string {
	raw, _ := args["options"].([]any)
	var opts []string
	for _, o := range raw {
		if s, ok := o.(string); ok && strings.TrimSpace(s) != "" {
			opts = append(opts, s)
		}
	}
	if len(opts) > 4 {
		opts = opts[:4]
	}
	return opts
}
