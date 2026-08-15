package loki

// chat_screenshot.go — outil web_screenshot : capture d'une page web RENDUE
// (JavaScript exécuté) via le navigateur Chromium piloté par Playwright.
//
// Indépendant de la vision : la capture est un fichier JPEG écrit dans le dossier
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
	"sort"
	"strings"
	"time"
)

// captureDir : sous-dossier du workspace où atterrissent les captures. Elles
// sont rangées PAR DISCUSSION (captures/<id>/…) pour que supprimer une
// discussion supprime aussi ses images — sinon elles s'accumulaient sur le
// disque sans qu'aucun écran ne les mentionne plus.
const captureDir = "captures"

// captureDirFor renvoie le dossier de captures d'une discussion (chemin absolu)
// et son préfixe relatif, celui qui sert dans les URLs d'affichage.
func captureDirFor(convID string) (abs, rel string) {
	rel = captureDir + "/" + convID
	return filepath.Join(agentWorkspace(), captureDir, convID), rel
}

// dropConvCaptures supprime les captures d'une discussion. Appelé quand on la
// supprime ou qu'on la vide. Best-effort : un échec ne doit rien interrompre.
func dropConvCaptures(convID string) {
	if convID == "" {
		return
	}
	abs, _ := captureDirFor(convID)
	_ = os.RemoveAll(abs)
}

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
		Description: "Photographie une page web (JS exécuté) pour la MONTRER. " +
			"La réponse donne la ligne markdown à recopier. Tu ne vois pas l'image.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":       map[string]any{"type": "string", "description": "URL complète"},
				"full_page": map[string]any{"type": "boolean", "description": "Page entière (lourd). Défaut false."},
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
	// Pleine page NON par défaut : un article long capturé en entier fait
	// plusieurs milliers de pixels de haut, donc plusieurs Mo, alors que « montre-moi
	// cette page » veut presque toujours dire le premier écran. Le modèle peut
	// demander la page entière quand c'est vraiment le sujet.
	fullPage := false
	if v, ok := args["full_page"].(bool); ok {
		fullPage = v
	}

	dir, relDir := captureDirFor(convEnsureActive())
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
	// .jpg et non .png : Playwright déduit le format de l'extension, et sur une
	// vraie page web (photos, dégradés) le JPEG pèse 3 à 10 fois moins. Le PNG ne
	// gagne que sur les aplats — pas le cas courant ici.
	name := fmt.Sprintf("%s-%s.jpg", safeSlug(host), time.Now().Format("20060102-150405"))
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

	pruneCaptures(dir, out)

	rel := relDir + "/" + name
	// On rend au modèle la ligne EXACTE à recopier : lui laisser composer l'URL
	// d'affichage revient à lui faire inventer un chemin, donc une image cassée.
	return fmt.Sprintf("Capture enregistrée (%s, %d Ko).\n"+
		"Pour la montrer à l'utilisateur, recopie TELLE QUELLE cette ligne markdown dans ta réponse :\n"+
		"![capture de %s](/api/chat/image?path=%s)",
		rel, st.Size()/1024, host, rel)
}

// Plafonds du dossier de captures. Sans ménage, chaque capture s'ajoute pour
// toujours dans /data — un volume que l'utilisateur n'inspecte jamais et qui
// finirait par saturer son cache SSD.
const (
	maxCaptureFiles = 20
	maxCaptureBytes = 40 << 20 // 40 Mo
)

// pruneCaptures supprime les captures les plus ANCIENNES tant que le dossier
// dépasse l'un des deux plafonds. `keep` est la capture qui vient d'être prise :
// elle n'est JAMAIS supprimée, sinon une capture plus lourde que le plafond
// s'effacerait elle-même et le modèle renverrait un lien vers un fichier absent.
// Best-effort : une erreur d'E/S ne doit pas faire échouer une capture réussie.
func pruneCaptures(dir, keep string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type shot struct {
		path string
		mod  time.Time
		size int64
	}
	var shots []shot
	var total int64
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(dir, e.Name())
		total += fi.Size()
		if p == keep {
			continue // comptée dans le total, mais jamais candidate à la suppression
		}
		shots = append(shots, shot{p, fi.ModTime(), fi.Size()})
	}
	// Plus ancienne en tête : c'est l'ordre de suppression. `nb` compte TOUS les
	// fichiers (keep compris) pour que le plafond porte sur le dossier entier.
	sort.Slice(shots, func(i, j int) bool { return shots[i].mod.Before(shots[j].mod) })
	nb := len(shots)
	if keep != "" {
		nb++
	}
	for i := 0; i < len(shots) && (nb > maxCaptureFiles || total > maxCaptureBytes); i++ {
		if os.Remove(shots[i].path) == nil {
			total -= shots[i].size
			nb--
		}
	}
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
