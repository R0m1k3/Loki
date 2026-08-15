package loki

// chat_screenshot.go — outil web_screenshot : capture d'une page web RENDUE
// (JavaScript exécuté) via le navigateur Chromium piloté par Playwright.
//
// Indépendant de la vision : la capture est un fichier PNG écrit dans le dossier
// de travail, que l'UI affiche dans le fil (route /api/chat/image). Le modèle,
// lui, ne la VOIT que si un projecteur multimodal est configuré (MMPROJ) — deux
// mécanismes distincts qu'il ne faut pas confondre. Sans vision, l'agent
// photographie sans regarder : c'est utile pour TOI, pas pour lui.
//
// Playwright est installé dans l'image Docker (voir Dockerfile). Hors conteneur,
// ou si l'image a été bâtie avec PLAYWRIGHT=0, l'outil n'est pas déclaré du tout
// plutôt que d'être annoncé au modèle puis d'échouer — un outil qu'on annonce et
// qui ne marche pas déclenche des boucles de réessai.

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

// captureDir : sous-dossier du workspace où atterrissent les captures.
const captureDir = "captures"

// screenshotTimeout : une page lente ne doit pas bloquer le tour. Playwright a
// son propre délai interne, celui-ci est le garde-fou externe.
const screenshotTimeout = 90 * time.Second

// playwrightBin renvoie le chemin du CLI Playwright, ou "" s'il est absent.
func playwrightBin() string {
	p, err := exec.LookPath("playwright")
	if err != nil {
		return ""
	}
	return p
}

func screenshotAvailable() bool { return playwrightBin() != "" }

func webScreenshotTool() Tool {
	return Tool{Type: "function", Function: ToolFunction{
		Name: "web_screenshot",
		// Description tenue au plus court : les schémas d'outils partent dans
		// CHAQUE requête et le préambule a un budget (TestSystemPromptStaysLean).
		Description: "Photographie une page web (PNG, JS exécuté) pour la MONTRER. " +
			"La réponse donne la ligne markdown à recopier. Tu ne vois pas l'image.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":       map[string]any{"type": "string", "description": "URL complète"},
				"full_page": map[string]any{"type": "boolean", "description": "Page entière. Défaut true."},
			},
			"required": []string{"url"},
		},
	}}
}

// safeSlug réduit un hôte à un nom de fichier sûr.
var slugRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func safeSlug(s string) string {
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-.")
	if r := []rune(s); len(r) > 40 {
		s = string(r[:40])
	}
	if s == "" {
		s = "page"
	}
	return s
}

func toolWebScreenshot(args map[string]any) string {
	bin := playwrightBin()
	if bin == "" {
		return "[erreur] Playwright n'est pas installé dans cette image (bâtie avec PLAYWRIGHT=0)."
	}
	url, _ := args["url"].(string)
	url = strings.TrimSpace(url)
	if url == "" {
		return "[erreur] url manquante"
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "[erreur] url invalide : elle doit commencer par http:// ou https://"
	}
	// width n'est plus déclaré dans le schéma (budget de préambule), mais reste
	// honoré s'il arrive quand même : un modèle qui l'invente obtient le
	// comportement attendu plutôt qu'un paramètre ignoré en silence.
	width := 1280
	if v, ok := args["width"].(float64); ok && v >= 320 && v <= 3840 {
		width = int(v)
	}
	fullPage := true
	if v, ok := args["full_page"].(bool); ok {
		fullPage = v
	}

	dir := filepath.Join(agentWorkspace(), captureDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "[erreur] création du dossier de captures : " + err.Error()
	}
	// Nom lisible et unique : hôte + horodatage. Deux captures de la même page
	// ne s'écrasent donc pas, et le fil garde les deux états.
	host := url
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	name := fmt.Sprintf("%s-%s.png", safeSlug(host), time.Now().Format("20060102-150405"))
	out := filepath.Join(dir, name)

	cmdArgs := []string{"screenshot", "--browser", "chromium",
		"--viewport-size", fmt.Sprintf("%d,800", width),
		"--wait-for-timeout", "2500"}
	if fullPage {
		cmdArgs = append(cmdArgs, "--full-page")
	}
	cmdArgs = append(cmdArgs, url, out)

	ctx, cancel := context.WithTimeout(context.Background(), screenshotTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, cmdArgs...)
	cmd.Dir = dir
	combined, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "[erreur] la page n'a pas fini de charger en 90 s"
		}
		return "[erreur] capture impossible : " + strings.TrimSpace(lastLines(string(combined), 3))
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() == 0 {
		return "[erreur] Playwright n'a produit aucune image"
	}

	rel := captureDir + "/" + name
	// On rend au modèle la ligne EXACTE à recopier : lui laisser composer l'URL
	// d'affichage revient à lui faire inventer un chemin, donc une image cassée.
	return fmt.Sprintf("Capture enregistrée (%s, %d Ko).\n"+
		"Pour la montrer à l'utilisateur, recopie TELLE QUELLE cette ligne markdown dans ta réponse :\n"+
		"![capture de %s](/api/chat/image?path=%s)",
		rel, st.Size()/1024, host, rel)
}

// lastLines garde les n dernières lignes non vides d'une sortie d'erreur —
// Playwright est bavard, seule la fin porte la cause.
func lastLines(s string, n int) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			keep = append(keep, l)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, " · ")
}
