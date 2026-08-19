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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// captureDir : sous-dossier où atterrissent les captures, À L'INTÉRIEUR du
// dossier de la discussion (discussions/<id>/captures/…) — comme les dépôts.
// Supprimer une discussion emporte donc ses images, qui sinon s'accumulaient
// sur le disque sans qu'aucun écran ne les mentionne plus.
const captureDir = "captures"

// captureDirFor renvoie le dossier de captures d'une discussion (chemin absolu)
// et son préfixe relatif, celui qui sert dans les URLs d'affichage. Le relatif
// part du dossier de la DISCUSSION : /api/chat/image résout d'abord là
// (workspaceFile), et les liens des anciens messages — en captures/<id>/… —
// continuent d'ouvrir leur image par le chemin de repli.
func captureDirFor(convID string) (abs, rel string) {
	return filepath.Join(convDirFor(convID), captureDir), captureDir
}

// Supprimer les captures d'une discussion n'a plus de fonction à soi : elles
// vivent dans son dossier, et dropConvFiles (chat_convfiles.go) l'emporte en
// entier — dépôts et fichiers écrits par l'agent compris.

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
		// Elle DÉPEND de la vision : annoncer « tu ne vois pas l'image » à un
		// modèle qui la reçoit ensuite le fait se contredire devant l'utilisateur
		// (il refuse de décrire ce qu'il a pourtant sous les yeux).
		Description: "Photographie une page web (JS exécuté) pour la MONTRER. " +
			"La réponse donne la ligne markdown à recopier. " + screenshotVisionNote(),
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

// screenshotVisionNote : ce que le modèle doit savoir de SA propre perception.
// Alignée sur la capacité RÉELLE du moteur, pas seulement sur la clé MMPROJ —
// sinon on promet une image qui n'arrivera pas et le modèle se contredit.
func screenshotVisionNote() string {
	if visionEnabled() && engineSeesImages() {
		return "L'image t'est ensuite montrée : tu peux la décrire."
	}
	return "Tu ne vois pas l'image."
}

// engineSeesImages : le moteur ACCEPTE-t-il réellement des images ? On
// interroge /props de llama-server (modalities.vision), avec un cache court —
// l'appel est local et instantané moteur en marche, mais moteur ARRÊTÉ chaque
// sonde attendrait le timeout, et elle est faite à chaque construction du
// catalogue d'outils.
//
// La clé MMPROJ ne suffit pas : configurée avec le projecteur d'un AUTRE
// modèle, ou avec un modèle sans vision (gpt-oss), le gabarit sérialise le
// base64 de l'image en TEXTE — la requête vue en production pesait 55 000
// tokens pour 32 768 de contexte, et tous les tours suivants échouaient.
//
// La sonde est un appel interne comme les autres : elle DOIT s'authentifier.
// Sans l'en-tête Bearer, un moteur protégé par API_KEY répond 401 et la sonde
// conclut « pas de vision » — le projecteur était bien chargé, les images ne
// partaient jamais et le modèle brodait à partir du seul chemin de fichier.
var (
	visionProbeMu   sync.Mutex
	visionProbeAt   time.Time
	visionProbeSeen bool
)

func engineSeesImages() bool {
	visionProbeMu.Lock()
	defer visionProbeMu.Unlock()
	if time.Since(visionProbeAt) < 10*time.Second {
		return visionProbeSeen
	}
	visionProbeAt = time.Now()
	visionProbeSeen = false
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://localhost:%d/props", LLMPort()), nil)
	if err != nil {
		return false
	}
	authHeader(req)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var props struct {
		Modalities struct {
			Vision bool `json:"vision"`
		} `json:"modalities"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&props) != nil {
		return false
	}
	visionProbeSeen = props.Modalities.Vision
	return visionProbeSeen
}

// stripImageParts retire les parties image_url des messages persistés et
// aplatit ce qui reste en texte simple. Deux raisons :
//   - guérir les conversations créées AVANT le passage de l'image en éphémère,
//     où un base64 de plusieurs dizaines de milliers de tokens était rejoué à
//     chaque tour jusqu'à dépasser définitivement le contexte ;
//   - garantir l'invariant à l'avenir, quel que soit le chemin d'écriture.
func stripImageParts(msgs []Message) []Message {
	for i, m := range msgs {
		parts, ok := m.Content.([]any)
		if !ok {
			continue
		}
		var texts []string
		dropped := false
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if pm["type"] == "image_url" {
				dropped = true
				continue
			}
			if t, ok := pm["text"].(string); ok {
				texts = append(texts, t)
			}
		}
		if dropped {
			msgs[i].Content = strings.Join(texts, "\n")
		}
	}
	return msgs
}

// screenshotImageMessage construit le message utilisateur qui PORTE la capture
// jusqu'au modèle. Le résultat d'un outil est un message `tool`, qui ne
// transporte que du texte : pour qu'un modèle multimodal voie l'image, elle doit
// arriver dans un message `user` au format OpenAI (partie text + partie
// image_url en data URI), le même que celui des pièces jointes.
//
// Renvoie ok=false quand la vision est absente ou le fichier illisible :
// llama-server rejette un contenu image sans --mmproj, donc mieux vaut ne rien
// envoyer que de faire échouer le tour.
func screenshotImageMessage(relPath string) (Message, bool) {
	if !visionEnabled() || !engineSeesImages() {
		return Message{}, false
	}
	abs, ok := workspaceFile(relPath)
	if !ok {
		return Message{}, false
	}
	mime := imageMime(abs)
	if mime == "" {
		return Message{}, false
	}
	b, err := os.ReadFile(abs)
	if err != nil || len(b) == 0 {
		return Message{}, false
	}
	return Message{Role: "user", Content: []map[string]any{
		{"type": "text", "text": "Voici la capture demandée."},
		{"type": "image_url", "image_url": map[string]any{
			"url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b),
		}},
	}}, true
}

// capturedRelPath extrait le chemin de la capture du texte rendu par
// toolWebScreenshot, pour éviter de faire porter deux valeurs de retour à
// l'outil (le protocole n'en accepte qu'une, textuelle).
var capturedRe = regexp.MustCompile(`\(/api/chat/image\?path=([^)]+)\)`)

func capturedRelPath(toolResult string) string {
	m := capturedRe.FindStringSubmatch(toolResult)
	if len(m) != 2 {
		return ""
	}
	return m[1]
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

func toolWebScreenshot(args map[string]any, caps Caps) string {
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
	// Accès internet coupé : la capture reste offerte au MODE CODE, mais bornée
	// aux adresses locales — voir sa propre page de dev, pas le web. Sans cette
	// borne, l'outil rouvrirait par la fenêtre ce que l'interrupteur ferme.
	if !caps.Internet && !isLocalURL(url) {
		return "[refusé] L'accès internet est coupé : la capture ne marche que sur une adresse locale " +
			"(localhost / 127.0.0.1), par exemple ton serveur de dev. Active l'accès internet pour le web."
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
		// 3,5 s : beaucoup de sites chargent leurs polices en webfont et laissent le
		// texte INVISIBLE le temps du téléchargement (font-display: block). Capturer
		// trop tôt donnait une page aux blocs vides, sans un mot.
		"--wait-for-timeout", "3500"}
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

// isLocalURL : l'adresse désigne-t-elle cette machine ? Sert de borne quand
// l'accès internet est coupé mais que le mode code doit pouvoir photographier
// son propre serveur de dev.
func isLocalURL(raw string) bool {
	u, err := neturl.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	switch host {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return strings.HasSuffix(host, ".localhost")
}
