package loki

// code_git.go — outils git natifs du mode code : git_status, git_diff,
// git_clone. Le modèle les avait via bash, mais des outils dédiés donnent une
// sortie bornée, un libellé propre dans le fil, et un clone qui atterrit
// TOUJOURS dans le dossier de la discussion (jamais ailleurs sur le disque).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const gitMaxOutput = 8000

func gitStatusTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "git_status",
			Description: "git status of the working folder (branch + changed files, porcelain format).",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

func gitDiffTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "git_diff",
			Description: "git diff of the working folder (unstaged + staged). Optional file to diff a single path.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file": map[string]any{"type": "string", "description": "Limit the diff to this path"},
				},
			},
		},
	}
}

func gitCloneTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "git_clone",
			Description: "Clone a git repository (https:// or git@) INTO the working folder of this discussion.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{"type": "string", "description": "Repository URL"},
					"dir": map[string]any{"type": "string", "description": "Target folder name (default: repo name)"},
				},
				"required": []string{"url"},
			},
		},
	}
}

// runGit exécute git dans le dossier de la discussion, sortie bornée.
func runGit(ctx context.Context, timeout time.Duration, args ...string) string {
	if _, err := exec.LookPath("git"); err != nil {
		return "[erreur] git n'est pas installé sur cette machine"
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", args...)
	cmd.Dir = agentCwd()
	// Jamais d'invite interactive (mot de passe, hôte inconnu) : un process
	// serveur n'a personne pour y répondre, il faut échouer vite et le dire.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -oBatchMode=yes")
	out, err := cmd.CombinedOutput()
	s := tailRunes(strings.TrimSpace(string(out)), gitMaxOutput)
	if cctx.Err() == context.DeadlineExceeded {
		return "[timeout] git " + strings.Join(args, " ")
	}
	if err != nil {
		if s == "" {
			s = err.Error()
		}
		return "[erreur] " + s
	}
	if s == "" {
		s = "(aucune sortie)"
	}
	return s
}

func toolGitStatus(ctx context.Context) string {
	return runGit(ctx, 20*time.Second, "status", "--porcelain=v1", "--branch")
}

func toolGitDiff(ctx context.Context, args map[string]any) string {
	// Un seul appel qui montre TOUT ce qui a changé : index + arbre de travail.
	// HEAD peut ne pas exister (dépôt tout neuf) : on retombe sur le diff simple.
	gitArgs := []string{"diff", "HEAD"}
	if file, _ := args["file"].(string); strings.TrimSpace(file) != "" {
		gitArgs = append(gitArgs, "--", file)
	}
	out := runGit(ctx, 30*time.Second, gitArgs...)
	if strings.HasPrefix(out, "[erreur]") && strings.Contains(out, "HEAD") {
		fallback := []string{"diff"}
		if file, _ := args["file"].(string); strings.TrimSpace(file) != "" {
			fallback = append(fallback, "--", file)
		}
		out = runGit(ctx, 30*time.Second, fallback...)
	}
	return out
}

// gitURLRe : https:// ou git@hôte: — pas de chemins locaux ni de protocoles
// exotiques (file://, ext:: qui exécute une commande arbitraire).
var gitURLRe = regexp.MustCompile(`^(https://[\w.\-]+(:\d+)?/|git@[\w.\-]+:)[\w.\-~/]+$`)

func toolGitClone(ctx context.Context, args map[string]any) string {
	url, _ := args["url"].(string)
	url = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(url), ".git")) + ".git"
	if !gitURLRe.MatchString(strings.TrimSuffix(url, ".git")) && !gitURLRe.MatchString(url) {
		return "[erreur] URL refusée — seuls https:// et git@hôte: sont acceptés"
	}
	dir, _ := args["dir"].(string)
	dir = strings.TrimSpace(dir)
	if dir == "" {
		base := filepath.Base(strings.TrimSuffix(url, ".git"))
		dir = base
	}
	// Le clone atterrit DANS la discussion, quoi qu'il arrive.
	if strings.Contains(dir, "..") || filepath.IsAbs(dir) {
		return "[erreur] dossier cible invalide"
	}
	target := filepath.Join(agentCwd(), dir)
	if _, err := os.Stat(target); err == nil {
		return "[erreur] le dossier " + dir + " existe déjà"
	}
	out := runGit(ctx, 5*time.Minute, "clone", "--", url, target)
	if strings.HasPrefix(out, "[erreur]") || strings.HasPrefix(out, "[timeout]") {
		return out
	}
	return fmt.Sprintf("[ok] cloné dans %s/\n%s", dir, out)
}
