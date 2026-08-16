// backend_engine_oci.go — mise à jour du moteur llama.cpp SANS reconstruire
// l'image et sans compiler : on va chercher le moteur là où l'équipe llama.cpp
// le publie déjà tout compilé — dans son image de conteneur officielle — et on
// en extrait le seul dossier qui nous intéresse, /app.
//
// Pourquoi ce détour plutôt que les releases GitHub (backend_prebuilt.go) :
// llama.cpp ne publie AUCUN binaire CUDA pour Linux dans ses releases. La seule
// distribution CUDA/Linux officielle et précompilée, ce sont ces images
// (ghcr.io/ggml-org/llama.cpp:server-cuda-b#####). C'est aussi exactement ce
// dont l'image de Loki hérite : mettre à jour par ce chemin, c'est obtenir le
// même moteur qu'une reconstruction complète de l'image, en ~170 Mo au lieu de
// 2,6 Go et sans passer par un rebuild.
//
// Ce qu'on télécharge : PAS l'image. Un manifeste OCI liste ses couches ; le
// runtime CUDA (2 Go) et les paquets système sont déjà présents dans l'image de
// Loki, donc inutiles. Seules les couches du haut portent /app (llama-server et
// ses .so, ~170 Mo). On les prend en partant du sommet et on s'arrête dès qu'on
// tient un moteur complet.
//
// Ce que ça NE remplace PAS : le runtime CUDA, lui, vient de l'image. Un
// llama.cpp compilé pour un CUDA majeur plus récent que celui de l'image ne
// démarrera pas — d'où le test à blanc (--list-devices) avant toute bascule, et
// la possibilité de revenir au moteur d'origine en un clic.
package loki

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ociBase est une variable et non une constante pour que les tests puissent la
// pointer sur un faux registre : tout ce qui suit se joue dans le dialogue avec
// le registre, et le vérifier en frappant ghcr.io rendrait la CI dépendante du
// réseau.
// LOKI_OCI_REGISTRY le remplace : c'est la porte de sortie pour un réseau qui
// n'atteint pas ghcr.io directement — miroir d'entreprise, cache local — sans
// quoi la mise à jour du moteur y serait tout simplement impossible.
var ociBase = "https://ghcr.io"

func init() {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("LOKI_OCI_REGISTRY")), "/"); v != "" {
		if !strings.Contains(v, "://") {
			v = "https://" + v
		}
		ociBase = v
	}
}

// ociHost : l'hôte seul, tel que l'attend le paramètre `service` du jeton.
func ociHost() string {
	h := strings.TrimPrefix(strings.TrimPrefix(ociBase, "https://"), "http://")
	return strings.TrimSuffix(h, "/")
}

const (
	ociEngineRepo = "ggml-org/llama.cpp"

	// Au-delà de cette taille, une couche n'est plus une couche « applicative » :
	// c'est le runtime CUDA (2 Go) ou une base système, qu'on ne veut à aucun
	// prix télécharger. On descend les couches du sommet vers la base ; croiser
	// une couche de cette taille signifie qu'on a dépassé /app.
	ociLayerCap = 600 << 20
)

// ociLayer : une couche du manifeste (on n'a besoin que de ça).
type ociLayer struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// ociDoc couvre à la fois un index multi-architecture et un manifeste d'image :
// les deux arrivent sur la même URL, distingués par le champ qui est rempli.
type ociDoc struct {
	MediaType string `json:"mediaType"`
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
		} `json:"platform"`
	} `json:"manifests"`
	Layers      []ociLayer        `json:"layers"`
	Annotations map[string]string `json:"annotations"`
}

// ociAccept : tous les types de manifeste qu'on sait lire. Sans cet en-tête, le
// registre répond en schéma 1 (obsolète) et la lecture échoue sans expliquer.
const ociAccept = "application/vnd.oci.image.index.v1+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.oci.image.manifest.v1+json," +
	"application/vnd.docker.distribution.manifest.v2+json"

// ociToken obtient un jeton de lecture ANONYME. Les images publiques de ghcr.io
// sont lisibles sans compte, mais le registre exige quand même un jeton porteur.
func ociToken(repo string) (string, error) {
	u := fmt.Sprintf("%s/token?scope=repository:%s:pull&service=%s", ociBase, repo, ociHost())
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get(u)
	if err != nil {
		return "", fmt.Errorf("registre %s injoignable : %w", ociHost(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("%s/token : HTTP %d", ociHost(), resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Token == "" {
		return "", fmt.Errorf("jeton de lecture illisible")
	}
	return out.Token, nil
}

// ociFetch fait un GET authentifié sur le registre.
func ociFetch(url, token, accept string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return (&http.Client{Timeout: 0}).Do(req)
}

// ociArch traduit GOARCH vers le nom de plateforme OCI.
func ociArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "amd64"
}

// ociResolve lit le manifeste d'un tag et renvoie les couches de l'image pour
// CETTE architecture, plus le numéro de build annoncé par l'image
// (org.opencontainers.image.version, ex. « b10450 »).
func ociResolve(repo, tag string) (layers []ociLayer, version string, err error) {
	token, err := ociToken(repo)
	if err != nil {
		return nil, "", err
	}
	get := func(ref string) (*ociDoc, error) {
		resp, err := ociFetch(fmt.Sprintf("%s/v2/%s/manifests/%s", ociBase, repo, ref), token, ociAccept)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode == 404 {
			return nil, fmt.Errorf("version inconnue dans le registre : %s", ref)
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("manifeste %s : HTTP %d", ref, resp.StatusCode)
		}
		var d ociDoc
		if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
			return nil, fmt.Errorf("manifeste %s illisible : %w", ref, err)
		}
		return &d, nil
	}

	doc, err := get(tag)
	if err != nil {
		return nil, "", err
	}
	version = doc.Annotations["org.opencontainers.image.version"]
	if len(doc.Manifests) > 0 {
		want := ociArch()
		digest := ""
		for _, m := range doc.Manifests {
			if m.Platform.OS == "linux" && m.Platform.Architecture == want {
				digest = m.Digest
				break
			}
		}
		if digest == "" {
			return nil, "", fmt.Errorf("aucune image linux/%s pour %s", want, tag)
		}
		if doc, err = get(digest); err != nil {
			return nil, "", err
		}
		if v := doc.Annotations["org.opencontainers.image.version"]; v != "" {
			version = v
		}
	}
	if len(doc.Layers) == 0 {
		return nil, "", fmt.Errorf("manifeste %s sans couche", tag)
	}
	return doc.Layers, version, nil
}

// ---------------------------------------------------------------------------
// Variante, version et emplacement du moteur
// ---------------------------------------------------------------------------

// engineOCISupported : ce mécanisme n'a de sens que là où ces images tournent.
// Les images llama.cpp sont linux/amd64 et linux/arm64, rien d'autre.
func engineOCISupported() bool {
	return runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64")
}

// engineVariant déduit la variante d'image correspondant au moteur COURANT, en
// regardant les backends ggml posés à côté du binaire. C'est la seule source
// fiable : le conteneur ne sait pas de quelle image il a été bâti, et demander
// à l'utilisateur de retenir « server-cuda » serait un piège à erreur.
//
// Renvoie un préfixe de tag : "server-cuda", "server-vulkan", "server-intel",
// "server-musa", ou "server" (CPU) par défaut.
func engineVariant() string {
	dir := ""
	if bin := currentEngineBin(); bin != "" {
		dir = filepath.Dir(bin)
	}
	if dir == "" {
		return "server"
	}
	has := func(prefix string) bool {
		m, _ := filepath.Glob(filepath.Join(dir, prefix+"*"))
		return len(m) > 0
	}
	switch {
	case has("libggml-cuda.so"):
		return "server-cuda"
	case has("libggml-musa.so"):
		return "server-musa"
	case has("libggml-sycl.so"):
		return "server-intel"
	case has("libggml-vulkan.so"):
		return "server-vulkan"
	}
	return "server"
}

// currentEngineBin : le binaire réellement utilisé (BIN de la configuration,
// avec le repli sur le moteur de l'image quand BIN n'est pas encore posé).
func currentEngineBin() string {
	bin := prebuiltResolveBin(ReadConfig()["BIN"])
	if bin != "" && isFile(bin) {
		return bin
	}
	return providedEngineBin()
}

// La bannière de version de llama.cpp a changé de forme plusieurs fois, et un
// binaire ancien reste parfaitement utilisable : on lit les trois formes.
//
//	version: 0.1.0-dev (build 10450, commit ece963f41)   ← actuelle
//	version: 10450 (ece963f)                             ← précédente
//	build: 4589 (a1b2c3d)                                ← plus ancienne
//
// C'est le NUMÉRO DE BUILD qui compte : c'est lui qui figure dans les tags
// d'image (server-cuda-b10450) et donc la seule chose comparable entre ce qui
// tourne et ce qui est publié.
var reEngineBuildLong = regexp.MustCompile(`build\s+(\d+),\s*commit\s+([0-9a-fA-F]+)`)
var reEngineBuildShort = regexp.MustCompile(`(?m)^\s*(?:version|build)\s*:\s*(\d+)\s*\(([0-9a-fA-F]+)\)`)

func parseEngineBuild(out string) (build int, commit string) {
	for _, re := range []*regexp.Regexp{reEngineBuildLong, reEngineBuildShort} {
		if m := re.FindStringSubmatch(out); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n, m[2]
		}
	}
	return 0, ""
}

// engineBuildOf interroge un llama-server pour son numéro de build. Renvoie
// (0, "") si le binaire ne répond pas — un moteur cassé ne doit pas empêcher
// d'en installer un autre.
func engineBuildOf(bin string) (int, string) {
	if bin == "" || !isFile(bin) {
		return 0, ""
	}
	cmd := hideCmd(exec.Command(bin, "--version"))
	cmd.Env = libraryPathEnv(filepath.Dir(bin))
	out, _ := cmd.CombinedOutput() // --version sort parfois en code non nul
	return parseEngineBuild(string(out))
}

// engineDir : racine des moteurs installés par ce mécanisme, sous LOKI_HOME —
// c'est-à-dire sur le volume /data en conteneur, donc conservés d'un
// redémarrage (et d'une mise à jour d'image) à l'autre.
func engineDir() string { return filepath.Join(LokiHome(), "engine") }

// engineOwns dit si un chemin désigne un moteur installé ici.
func engineOwns(p string) bool {
	if p == "" {
		return false
	}
	norm := func(s string) string { return strings.ToLower(filepath.ToSlash(filepath.Clean(s))) }
	return strings.HasPrefix(norm(p), norm(engineDir())+"/")
}

// engineInstalled liste les moteurs présents sous engineDir(), du plus ancien
// au plus récent (ordre des numéros de build).
func engineInstalled() []string {
	entries, err := os.ReadDir(engineDir())
	if err != nil {
		return nil
	}
	var tags []string
	for _, e := range entries {
		if e.IsDir() && isFile(filepath.Join(engineDir(), e.Name(), "llama-server")) {
			tags = append(tags, e.Name())
		}
	}
	sort.Slice(tags, func(i, j int) bool { return engineTagBuild(tags[i]) < engineTagBuild(tags[j]) })
	return tags
}

var reTagBuild = regexp.MustCompile(`-b(\d+)$`)

// engineTagBuild extrait le numéro de build d'un tag (« server-cuda-b10450 » →
// 10450), 0 si le tag n'en porte pas (tag mouvant « server-cuda »).
func engineTagBuild(tag string) int {
	m := reTagBuild.FindStringSubmatch(tag)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// engineTagOf : le tag d'un moteur téléchargé ici (« server-cuda-b10450 »), ""
// pour tout autre chemin.
func engineTagOf(bin string) string {
	if !engineOwns(bin) {
		return ""
	}
	return filepath.Base(filepath.Dir(bin))
}

// engineBinFor : chemin du llama-server d'une version installée.
func engineBinFor(tag string) string {
	p := filepath.Join(engineDir(), filepath.Base(tag), "llama-server")
	if isFile(p) {
		return p
	}
	return ""
}

// enginePrune supprime les versions installées autres que celles à garder.
// Chaque version pèse ~450 Mo une fois décompressée : en laisser traîner cinq
// remplit le volume /data sans rien apporter.
func enginePrune(keep map[string]bool, logf func(string)) {
	for _, tag := range engineInstalled() {
		if keep[tag] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(engineDir(), tag)); err == nil && logf != nil {
			logf("ancienne version supprimée : " + tag)
		}
	}
}

// ---------------------------------------------------------------------------
// Installation
// ---------------------------------------------------------------------------

// engineOCIInstall télécharge le moteur du tag demandé et l'installe dans
// engineDir()/<tag>. Renvoie le chemin du llama-server installé.
//
// Réentrant : une version déjà installée est renvoyée telle quelle, et une
// installation interrompue laisse un dossier « .part » qui est repris de zéro.
func engineOCIInstall(tag string, logf, phasef func(string)) (string, error) {
	tag = filepath.Base(strings.TrimSpace(tag)) // jamais de séparateur dans un nom de dossier
	if tag == "" || tag == "." {
		return "", fmt.Errorf("version du moteur non précisée")
	}
	if bin := engineBinFor(tag); bin != "" {
		logf("déjà installé : " + tag)
		return bin, nil
	}

	phasef("lecture du manifeste " + tag + "…")
	layers, version, err := ociResolve(ociEngineRepo, tag)
	if err != nil {
		return "", err
	}
	if version != "" {
		logf(fmt.Sprintf("image %s:%s — llama.cpp %s, %d couches", ociEngineRepo, tag, version, len(layers)))
	}

	dest := filepath.Join(engineDir(), tag)
	tmp := dest + ".part"
	if err := os.RemoveAll(tmp); err != nil {
		return "", err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp) // no-op après le rename final

	token, err := ociToken(ociEngineRepo)
	if err != nil {
		return "", err
	}

	// On descend du SOMMET vers la base : /app est ajouté en dernier dans le
	// Dockerfile amont, et les couches du bas sont le runtime CUDA et le système,
	// déjà présents ici. Une couche du haut peut masquer un fichier d'une couche
	// plus basse : le premier extrait gagne, comme le ferait un runtime OCI.
	seen := map[string]bool{}
	var links []archiveLink
	for i := len(layers) - 1; i >= 0; i-- {
		l := layers[i]
		if l.Size > ociLayerCap {
			logf(fmt.Sprintf("couche %d ignorée (%d Mo) : c'est le runtime de l'image, pas le moteur", i, l.Size/1_000_000))
			break
		}
		part := filepath.Join(tmp, "layer.tgz")
		if l.Size > 4<<20 {
			phasef(fmt.Sprintf("téléchargement du moteur (%d Mo)…", l.Size/1_000_000))
		}
		if err := ociDownloadBlob(ociEngineRepo, l.Digest, token, part, l.Size, logf); err != nil {
			return "", fmt.Errorf("téléchargement de la couche %d : %w", i, err)
		}
		n, err := extractAppLayer(part, tmp, seen, &links)
		_ = os.Remove(part)
		if err != nil {
			return "", fmt.Errorf("extraction de la couche %d : %w", i, err)
		}
		if n > 0 {
			logf(fmt.Sprintf("couche %d : %d fichiers du moteur", i, n))
		}
		if engineSetComplete(seen) {
			break
		}
	}
	if err := applyLinks(links); err != nil {
		return "", err
	}
	if !seen["llama-server"] {
		return "", fmt.Errorf("llama-server absent de l'image %s — variante inattendue ?", tag)
	}

	if err := os.RemoveAll(dest); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	bin := filepath.Join(dest, "llama-server")
	if err := os.Chmod(bin, 0o755); err != nil {
		return "", err
	}
	logf("moteur installé : " + bin)
	return bin, nil
}

// engineSetComplete : a-t-on de quoi lancer le moteur ? Le binaire seul ne
// suffit pas — depuis que llama.cpp éclate son code en bibliothèques, l'exécutable
// tient dans une couche et ses .so dans la précédente. S'arrêter au binaire
// donnait un moteur qui meurt sur « libllama.so: cannot open shared object ».
func engineSetComplete(seen map[string]bool) bool {
	if !seen["llama-server"] {
		return false
	}
	base, llama := false, false
	for name := range seen {
		switch {
		case strings.HasPrefix(name, "libggml-base.so"):
			base = true
		case strings.HasPrefix(name, "libllama.so"):
			llama = true
		}
	}
	return base && llama
}

// ociDownloadBlob télécharge une couche vers dest, avec la progression du
// téléchargement dans le journal du job.
func ociDownloadBlob(repo, digest, token, dest string, size int64, logf func(string)) error {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/v2/%s/blobs/%s", ociBase, repo, digest), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return downloadRequest(req, dest, size, logf)
}

// extractAppLayer extrait les entrées « app/… » d'une couche (tar.gz) à PLAT
// dans dir, en ignorant celles déjà vues (une couche supérieure fait foi) et
// les marqueurs de suppression OCI (.wh.*). Renvoie le nombre de fichiers
// extraits. Les liens sont accumulés dans links : leur cible n'est pas
// forcément déjà extraite au moment où on croise l'entrée.
func extractAppLayer(archive, dir string, seen map[string]bool, links *[]archiveLink) (int, error) {
	f, err := os.Open(archive)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	defer gz.Close()

	count := 0
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return count, err
		}
		name, ok := appEntryName(h.Name)
		if !ok || seen[name] {
			continue
		}
		p := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeReg:
			w, err := os.Create(p)
			if err != nil {
				return count, err
			}
			if _, err := io.Copy(w, tr); err != nil {
				w.Close()
				return count, err
			}
			w.Close()
			_ = os.Chmod(p, os.FileMode(h.Mode)&0o777)
			seen[name] = true
			count++
		case tar.TypeSymlink, tar.TypeLink:
			// Indispensable : les .so sont livrés sous leur nom versionné
			// (libllama.so.0.1.0) plus deux liens (libllama.so.0, libllama.so) —
			// c'est le nom court que l'éditeur de liens cherche au lancement.
			target, ok := appEntryName(h.Linkname)
			if !ok {
				target = filepath.Base(h.Linkname) // lien relatif au sein de /app
			}
			*links = append(*links, archiveLink{path: p, target: target})
			seen[name] = true
			count++
		}
	}
}

// appEntryName ramène « app/libllama.so » à « libllama.so ». Renvoie ok=false
// pour tout ce qui n'est pas un fichier direct de /app : autres dossiers,
// sous-dossiers, et marqueurs de suppression OCI.
func appEntryName(raw string) (string, bool) {
	p := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(raw)), "./")
	rest, ok := strings.CutPrefix(p, "app/")
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	if strings.HasPrefix(rest, ".wh.") {
		return "", false
	}
	return rest, true
}

// ---------------------------------------------------------------------------
// Vérification et bascule
// ---------------------------------------------------------------------------

// engineOCILatest renvoie le dernier build publié pour la variante (numéro et
// tag figé correspondant). On lit le tag MOUVANT (« server-cuda ») et son
// annotation de version plutôt que de lister les 1000 tags du dépôt : une seule
// requête, et c'est l'amont qui dit ce qu'est « la dernière ».
func engineOCILatest(variant string) (build int, tag string, err error) {
	_, version, err := ociResolve(ociEngineRepo, variant)
	if err != nil {
		return 0, "", err
	}
	if version == "" {
		// Pas d'annotation : on retombe sur le tag mouvant lui-même, qui pointe
		// bien sur la dernière image — on ne saura juste pas dire son numéro.
		return 0, variant, nil
	}
	// L'annotation vaut « b10450 » : le tag figé est « server-cuda-b10450 ».
	n, _ := strconv.Atoi(strings.TrimPrefix(version, "b"))
	return n, variant + "-" + version, nil
}

// engineSmokeTest lance à blanc le moteur fraîchement installé — c'est là que
// se joue le seul vrai risque de ce mécanisme. La mise à jour apporte llama.cpp,
// PAS le runtime CUDA, qui reste celui de l'image : un llama.cpp compilé pour un
// CUDA plus récent ne chargera pas son backend GPU ici.
//
// Deux échecs distincts, et le second est le sournois :
//
//  1. le binaire ne démarre pas du tout (bibliothèque manquante) — bruyant ;
//  2. il démarre mais ne voit plus AUCUNE carte, alors que le moteur courant en
//     voyait. Rien ne casse : le modèle se charge, tourne sur le processeur, et
//     on ne s'en aperçoit qu'au débit. C'est le pire résultat possible, donc on
//     le traite comme un échec de mise à jour.
//
// Ne voir aucune carte n'est un échec que si le moteur courant en voyait :
// sur une machine sans GPU, « aucune carte » est le résultat normal.
func engineSmokeTest(bin, ancien string) error {
	devs, err := engineListDevices(bin)
	if err != nil {
		return err
	}
	if len(devs) > 0 {
		return nil
	}
	if ancien == "" || ancien == bin {
		return nil
	}
	avant, err := engineListDevices(ancien)
	if err != nil || len(avant) == 0 {
		return nil // le moteur courant n'en voyait pas plus : rien à reprocher au nouveau
	}
	return fmt.Errorf("le moteur téléchargé ne voit aucune carte graphique, alors que le moteur actuel en voit %d — "+
		"il tournerait sur le processeur, beaucoup plus lentement", len(avant))
}

// engineListDevices lance `llama-server --list-devices` sur un binaire donné.
func engineListDevices(bin string) ([]map[string]any, error) {
	cmd := hideCmd(exec.Command(bin, "--list-devices"))
	cmd.Env = libraryPathEnv(filepath.Dir(bin))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("le moteur ne démarre pas ici : %v\n%s", err, tailLines(string(out), 12))
	}
	return parseListDevices(string(out)), nil
}

// tailLines garde les n dernières lignes non vides d'une sortie.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
