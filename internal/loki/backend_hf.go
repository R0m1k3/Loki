package loki

// backend_hf.go — chercher un modèle GGUF sur Hugging Face depuis Loki.
//
// Avant ce fichier, installer un modèle voulait dire aller sur huggingface.co,
// naviguer dans l'arborescence d'un dépôt, copier le lien d'un .gguf et le
// coller dans Loki. Rien ne disait si le fichier tenait en mémoire, et surtout
// rien ne reliait un modèle à SON projecteur vision : coller un mmproj pris
// dans un autre dépôt donne un moteur qui démarre et ne voit rien.
//
// Ce fichier ne télécharge rien. Il PRODUIT des URL directes, que le chemin
// existant consomme tel quel — normalizeHFURL, shardURLSet, la sonde d'espace
// disque et la reprise de téléchargement (backend_models.go) n'ont pas bougé.
//
// Trois familles de .gguf cohabitent dans un même dépôt et ne veulent pas dire
// la même chose :
//
//	mmproj-*.gguf  le projecteur vision, à passer en --mmproj
//	mtp-*.gguf     les poids de multi-token prediction (décodage spéculatif)
//	le reste       le modèle lui-même
//
// Les deux premiers ressemblent à s'y méprendre à un modèle : les proposer en
// vrac, c'est offrir de lancer llama-server sur un projecteur.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	hfHost = "https://huggingface.co"
	// Les deux durées de cache disent la même chose : ces routes tapent un hôte
	// externe à chaque appel, et l'UI en déclenche une par recherche. Une
	// arborescence de dépôt ne bouge quasiment jamais, une liste de résultats un
	// peu plus.
	hfSearchTTL = 60 * time.Second
	hfFilesTTL  = 10 * time.Minute
	hfMaxRepos  = 25
	hfMaxQuery  = 100
	hfTimeout   = 10 * time.Second
)

// hfRepoRe borne ce qu'on accepte comme identifiant de dépôt. La valeur vient
// du navigateur et part dans un chemin d'URL : sans ce filtre, un « ../.. »
// permettrait d'atteindre n'importe quelle route de l'API Hugging Face.
var hfRepoRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// hfRepo — un dépôt trouvé par la recherche.
//
// Volontairement pauvre. On a essayé d'y porter un indicateur « vision » tiré
// des tags Hugging Face : il ment. Sur les deux dépôts GGUF de Qwen3.8-27B qui
// publient TOUS DEUX un mmproj, ggml-org est taggé image-text-to-text et
// unsloth ne l'est pas. Une pastille « vision » sur l'un et pas sur l'autre
// aurait été pire que rien. La seule source qui ne se trompe pas, c'est la
// présence d'un mmproj-*.gguf dans l'arborescence — donc hfFiles, pas ici.
type hfRepo struct {
	ID        string `json:"id"`
	Downloads int    `json:"downloads"`
	Likes     int    `json:"likes"`
	// Gated : le dépôt exige d'avoir accepté ses conditions ET un jeton. Il se
	// LIT pourtant sans rien (arborescence en 200), et ne refuse qu'au moment du
	// transfert : sans cette pastille, l'interface propose des quants avec leur
	// verdict mémoire et le téléchargement échoue en 401 sans prévenir.
	Gated bool `json:"gated"`
}

// hfEntry — un .gguf installable. Shards > 1 signale une famille de tranches :
// Size est alors le total de la famille, pas le poids du fichier pointé.
type hfEntry struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	Quant  string `json:"quant"`
	Shards int    `json:"shards"`
	// Renseignés par le handler, qui seul connaît le matériel et le contexte.
	Verdict string `json:"verdict,omitempty"`
	Why     string `json:"why,omitempty"`
}

// hfListing — le contenu utile d'un dépôt, trié.
type hfListing struct {
	Repo       string    `json:"repo"`
	Gated      bool      `json:"gated"`
	Models     []hfEntry `json:"models"`
	Projectors []hfEntry `json:"projectors"`
	Drafts     []hfEntry `json:"drafts"`
}

// hfAuth pose les en-têtes communs à tous les appels Hugging Face. Le jeton
// compte aussi pour la RECHERCHE et l'arborescence, pas seulement pour le
// transfert : sans lui un dépôt gated répond 401 et Loki afficherait « aucun
// résultat » là où il faudrait dire « dépôt à accès restreint ».
func hfAuth(req *http.Request) {
	if k := os.Getenv("HF_TOKEN"); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	req.Header.Set("User-Agent", "loki/"+Version)
	req.Header.Set("Accept", "application/json")
}

// hfCache : mémo commun à la recherche et aux arborescences, clé = l'URL
// appelée. Sans lui, chaque frappe au clavier dans le champ de recherche
// produit une requête sortante.
var (
	hfCacheMu sync.Mutex
	hfCache   = map[string]hfCacheItem{}
)

type hfCacheItem struct {
	at  time.Time
	raw []byte
}

// hfGetJSON appelle l'API Hugging Face et décode la réponse dans out, avec
// cache. Le corps brut est mémorisé plutôt que la valeur décodée : deux
// appelants attendent des types différents et le partage d'une structure
// décodée les exposerait à se modifier mutuellement.
func hfGetJSON(ctx context.Context, endpoint string, ttl time.Duration, out any) error {
	hfCacheMu.Lock()
	if it, ok := hfCache[endpoint]; ok && time.Since(it.at) < ttl {
		raw := it.raw
		hfCacheMu.Unlock()
		return json.Unmarshal(raw, out)
	}
	hfCacheMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, hfTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return err
	}
	hfAuth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("Hugging Face injoignable : %v", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
	case 401, 403:
		return fmt.Errorf("dépôt à accès restreint — renseigne HF_TOKEN pour y accéder")
	case 404:
		return fmt.Errorf("dépôt introuvable sur Hugging Face")
	default:
		return fmt.Errorf("Hugging Face a répondu %d", resp.StatusCode)
	}
	// Borne de lecture : la réponse vient d'un tiers, on ne lui laisse pas
	// décider de la mémoire qu'on lui consacre.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	hfCacheMu.Lock()
	hfCache[endpoint] = hfCacheItem{at: time.Now(), raw: raw}
	hfCacheMu.Unlock()
	return json.Unmarshal(raw, out)
}

// hfSearch cherche des dépôts GGUF. Le filtre `gguf` est essentiel : sans lui la
// recherche remonte surtout des dépôts de poids safetensors, inutilisables par
// llama-server.
func hfSearch(ctx context.Context, q string) ([]hfRepo, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, fmt.Errorf("recherche vide")
	}
	if len(q) > hfMaxQuery {
		q = q[:hfMaxQuery]
	}
	// `expand[]` remplace les champs par défaut de la réponse : `gated` s'y
	// ajoute, mais downloads et likes doivent alors être redemandés
	// explicitement, sinon ils disparaissent et la liste perd son classement
	// lisible.
	endpoint := hfHost + "/api/models?" + url.Values{
		"search":    {q},
		"filter":    {"gguf"},
		"limit":     {fmt.Sprint(hfMaxRepos)},
		"sort":      {"downloads"},
		"direction": {"-1"},
		"expand[]":  {"gated", "downloads", "likes"},
	}.Encode()

	var raw []struct {
		ID        string `json:"id"`
		Downloads int    `json:"downloads"`
		Likes     int    `json:"likes"`
		Gated     any    `json:"gated"`
	}
	if err := hfGetJSON(ctx, endpoint, hfSearchTTL, &raw); err != nil {
		return nil, err
	}
	out := make([]hfRepo, 0, len(raw))
	for _, r := range raw {
		out = append(out, hfRepo{ID: r.ID, Downloads: r.Downloads, Likes: r.Likes, Gated: hfGatedFlag(r.Gated)})
	}
	return out, nil
}

// hfGatedFlag lit le champ `gated` de l'API, qui n'est PAS un booléen : il vaut
// false, "auto" (accepter les conditions suffit) ou "manual" (l'auteur valide
// chaque demande). Les deux dernières valeurs verrouillent le téléchargement de
// la même façon ; seule la présence d'un verrou nous intéresse ici.
func hfGatedFlag(v any) bool {
	switch g := v.(type) {
	case bool:
		return g
	case string:
		switch strings.ToLower(strings.TrimSpace(g)) {
		case "", "false", "none", "no":
			return false
		}
		return true
	}
	return false
}

// hfRepoGated dit si un dépôt est verrouillé. L'arborescence (hfFiles) ne porte
// pas l'information : elle se lit sur la fiche du dépôt, en un appel séparé et
// mis en cache comme elle. Une erreur ici ne doit RIEN casser — au pire on
// n'affiche pas l'avertissement, et le téléchargement dira lui-même pourquoi il
// a été refusé.
func hfRepoGated(ctx context.Context, repo string) bool {
	var info struct {
		Gated any `json:"gated"`
	}
	endpoint := hfHost + "/api/models/" + repo + "?" + url.Values{"expand[]": {"gated"}}.Encode()
	if err := hfGetJSON(ctx, endpoint, hfFilesTTL, &info); err != nil {
		return false
	}
	return hfGatedFlag(info.Gated)
}

// hfTokenSet dit si un jeton est configuré, pour que l'interface distingue
// « il faut en poser un » de « celui qui est posé ne suffit pas ».
func hfTokenSet() bool {
	return strings.TrimSpace(os.Getenv("HF_TOKEN")) != ""
}

// hfRepoFromURL retrouve « auteur/dépôt » dans un lien de téléchargement
// Hugging Face, pour pouvoir le nommer dans un message d'erreur. Renvoie "" si
// le lien pointe ailleurs.
func hfRepoFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !strings.Contains(u.Host, "huggingface.co") {
		return ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 4 || (segs[2] != "resolve" && segs[2] != "blob") {
		return ""
	}
	repo := segs[0] + "/" + segs[1]
	if !hfRepoRe.MatchString(repo) {
		return ""
	}
	return repo
}

// hfFiles liste les .gguf d'un dépôt et les range par famille.
//
// `recursive=1` n'est pas un confort : les quants volumineux vivent dans des
// sous-dossiers (UD-IQ4_XS/…), et sans lui l'arborescence ne renvoie que les
// dossiers, donc aucun modèle.
func hfFiles(ctx context.Context, repo string) (hfListing, error) {
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if !hfRepoRe.MatchString(repo) {
		return hfListing{}, fmt.Errorf("nom de dépôt invalide (attendu « auteur/dépôt »)")
	}
	endpoint := hfHost + "/api/models/" + repo + "/tree/main?recursive=1"

	var raw []struct {
		Type string `json:"type"`
		Path string `json:"path"`
		Size int64  `json:"size"`
		LFS  struct {
			Size int64 `json:"size"`
		} `json:"lfs"`
	}
	if err := hfGetJSON(ctx, endpoint, hfFilesTTL, &raw); err != nil {
		return hfListing{}, err
	}
	files := make([]hfFile, 0, len(raw))
	for _, f := range raw {
		if f.Type == "directory" {
			continue
		}
		n := f.LFS.Size
		if n == 0 {
			n = f.Size
		}
		files = append(files, hfFile{Path: f.Path, Size: n})
	}
	out := hfClassify(repo, files)
	out.Gated = hfRepoGated(ctx, repo)
	return out, nil
}

// hfFile — une entrée d'arborescence, réduite à ce dont le classement a besoin.
type hfFile struct {
	Path string
	Size int64
}

// hfClassify range les fichiers d'un dépôt en modèles, projecteurs et poids de
// décodage spéculatif. Séparé de l'appel réseau pour être testable : c'est ici
// que se jouent les erreurs qui coûtent cher (proposer un mmproj comme modèle,
// annoncer 15 Go pour une famille qui en pèse 45).
func hfClassify(repo string, files []hfFile) hfListing {
	// Taille par chemin complet : une famille de tranches partage son dossier,
	// et deux dossiers de quantification différents contiennent des fichiers de
	// même nom de base.
	size := map[string]int64{}
	var paths []string
	for _, f := range files {
		if !strings.HasSuffix(strings.ToLower(f.Path), ".gguf") {
			continue
		}
		size[f.Path] = f.Size
		paths = append(paths, f.Path)
	}

	out := hfListing{Repo: repo}
	for _, p := range paths {
		base := baseName(p)
		// Tranche 2..N : elle appartient à une famille déjà représentée par sa
		// première, et ne démarre pas seule. On ne la propose jamais.
		if isFollowerShard(base) {
			continue
		}
		fam := shardFamily(base)
		dir := ""
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			dir = p[:i+1]
		}
		total := int64(0)
		for _, n := range fam {
			total += size[dir+n]
		}
		e := hfEntry{
			Name: base, URL: hfResolveURL(repo, p), Size: total,
			Quant: quantFromName(base), Shards: len(fam),
		}
		switch {
		case strings.HasPrefix(strings.ToLower(base), "mmproj"):
			out.Projectors = append(out.Projectors, e)
		case strings.HasPrefix(strings.ToLower(base), "mtp-"):
			out.Drafts = append(out.Drafts, e)
		default:
			out.Models = append(out.Models, e)
		}
	}
	bySize := func(s []hfEntry) { sort.Slice(s, func(i, j int) bool { return s[i].Size < s[j].Size }) }
	bySize(out.Models)
	bySize(out.Projectors)
	bySize(out.Drafts)
	return out
}

// hfResolveURL construit le lien de téléchargement direct. Chaque segment est
// échappé séparément : un nom de fichier peut contenir un espace, et les « / »
// du chemin doivent rester des séparateurs.
func hfResolveURL(repo, filePath string) string {
	segs := strings.Split(filePath, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return hfHost + "/" + repo + "/resolve/main/" + strings.Join(segs, "/")
}

// hfPickProjector choisit le projecteur à proposer avec un modèle : le Q8_0
// d'abord (629 Mo contre 931 pour le BF16, sans différence perceptible sur un
// encodeur d'images), sinon le plus léger. Renvoie false si le dépôt n'en
// publie aucun — auquel cas l'UI doit le DIRE, pas aller en chercher un
// ailleurs : un projecteur d'un autre modèle ne correspond jamais.
func hfPickProjector(list []hfEntry) (hfEntry, bool) {
	if len(list) == 0 {
		return hfEntry{}, false
	}
	for _, e := range list {
		if strings.EqualFold(e.Quant, "Q8_0") {
			return e, true
		}
	}
	best := list[0]
	for _, e := range list[1:] {
		if e.Size < best.Size {
			best = e
		}
	}
	return best, true
}
