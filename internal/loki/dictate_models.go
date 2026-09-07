// Catalogue des modèles de dictée — Parakeet (NVIDIA), servi par sherpa-onnx.
//
// Aucun n'est embarqué dans l'image : ils pèsent près de 500 Mo compressés et
// vivent dans <LOKI_HOME>/asr/, donc dans le volume /data — ils survivent aux
// recréations du conteneur.
//
// Différence de forme avec whisper.cpp, qu'ils remplacent : un modèle n'est plus
// UN fichier .bin mais un DOSSIER (encodeur, décodeur, joiner, table de jetons),
// livré en .tar.bz2. Le téléchargement décompresse donc au passage — d'où
// asrExtract plutôt qu'une simple copie de flux.
package loki

import (
	"archive/tar"
	"compress/bzip2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Les archives sherpa-onnx officielles (conversions ONNX des modèles NeMo).
const asrModelBase = "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/"

type asrModel struct {
	Nom     string // libellé affiché
	Dossier string // nom du dossier, identique en local et dans l'archive
	Octets  int64  // taille du TÉLÉCHARGEMENT (archive compressée), pour prévenir
	Langues string // ce que le modèle sait transcrire — c'est LE critère de choix
}

// asrFichiers : les quatre fichiers qu'un modèle doit avoir pour être utilisable.
// Sert au contrôle de présence : un dossier à moitié extrait (téléchargement
// interrompu) ne doit pas passer pour un modèle installé.
var asrFichiers = []string{"encoder.int8.onnx", "decoder.int8.onnx", "joiner.int8.onnx", "tokens.txt"}

// Les tailles sont celles publiées par le dépôt amont. Elles servent à informer
// avant un téléchargement, pas à vérifier le fichier : une taille qui dérive
// d'une release à l'autre ne doit pas casser la dictée.
var asrCatalogue = map[string]asrModel{
	"parakeet-tdt-0.6b-v3": {
		Nom:     "Parakeet v3 — recommandé",
		Dossier: "sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8",
		Octets:  487_170_055,
		Langues: "25 langues européennes, français inclus — détection automatique",
	},
	"parakeet-tdt-0.6b-v2": {
		Nom:     "Parakeet v2 — anglais seul",
		Dossier: "sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8",
		Octets:  482_468_385,
		Langues: "anglais uniquement",
	},
}

// asrRoot : la racine des modèles de dictée. Volontairement distincte de
// l'ancien dossier `whisper/` : les .bin ggml qui y dorment ne servent plus à
// rien, mais les effacer tout seuls serait détruire des données que
// l'utilisateur n'a pas demandé de perdre. Ils sont à supprimer à la main.
func asrRoot() string { return filepath.Join(LokiHome(), "asr") }

// asrModelDirFor : dossier local d'un modèle. Vide si l'identifiant est inconnu —
// l'appelant doit traiter ce cas plutôt que de bâtir un chemin sur une chaîne
// arbitraire venue d'une requête HTTP.
func asrModelDirFor(id string) string {
	m, ok := asrCatalogue[id]
	if !ok {
		return ""
	}
	return filepath.Join(asrRoot(), m.Dossier)
}

func asrModelURLFor(id string) string {
	m, ok := asrCatalogue[id]
	if !ok {
		return ""
	}
	return asrModelBase + m.Dossier + ".tar.bz2"
}

// asrModelPresent : le modèle est installé ET complet. On vérifie les quatre
// fichiers plutôt que l'existence du dossier : une extraction coupée en deux
// laisse un dossier bien réel dont sherpa-onnx ne saura rien faire, et l'erreur
// tomberait au premier clic sur le micro plutôt qu'ici.
func asrModelPresent(id string) bool {
	dir := asrModelDirFor(id)
	if dir == "" {
		return false
	}
	for _, f := range asrFichiers {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil || st.Size() == 0 {
			return false
		}
	}
	return true
}

// asrModelFile : chemin d'un des fichiers du modèle, pour les arguments passés
// au serveur. Vide si le modèle est inconnu.
func asrModelFile(id, fichier string) string {
	dir := asrModelDirFor(id)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, fichier)
}

// asrCatalogueTrie : le catalogue pour l'UI, du plus léger au plus lourd —
// l'ordre dans lequel se pose la question du compromis.
func asrCatalogueTrie() []map[string]any {
	out := make([]map[string]any, 0, len(asrCatalogue))
	for id, m := range asrCatalogue {
		out = append(out, map[string]any{
			"id": id, "nom": m.Nom, "octets": m.Octets,
			"langues": m.Langues, "present": asrModelPresent(id),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["octets"].(int64) < out[j]["octets"].(int64)
	})
	return out
}

// asrExtract décompresse une archive .tar.bz2 sous `dest`, en ne gardant que le
// contenu du dossier de premier niveau (l'archive amont l'embarque déjà).
//
// Deux précautions qui ne sont pas du zèle :
//   - un chemin d'entrée qui remonte (« ../ ») ou absolu est REFUSÉ : une archive
//     téléchargée est une entrée non fiable, et une entrée piégée écrirait
//     n'importe où sur le disque du serveur ;
//   - on extrait dans un dossier temporaire renommé à la fin, pour qu'une
//     extraction interrompue ne laisse jamais un modèle à moitié installé sous
//     son nom définitif.
func asrExtract(r io.Reader, dest string) error {
	tmp := dest + ".part"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(bzip2.NewReader(r))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
		rel, err := asrSafeRel(h.Name)
		if err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
		if rel == "" {
			continue // le dossier racine de l'archive lui-même
		}
		cible := filepath.Join(tmp, rel)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(cible, 0o755); err != nil {
				_ = os.RemoveAll(tmp)
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(cible), 0o755); err != nil {
				_ = os.RemoveAll(tmp)
				return err
			}
			f, err := os.Create(cible)
			if err != nil {
				_ = os.RemoveAll(tmp)
				return err
			}
			// #nosec G110 — l'archive vient d'une release GitHub épinglée par son
			// nom, pas d'une source arbitraire ; et la borne utile ici est le
			// disque, pas un plafond deviné.
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				_ = os.RemoveAll(tmp)
				return err
			}
			if err := f.Close(); err != nil {
				_ = os.RemoveAll(tmp)
				return err
			}
		default:
			// Liens, périphériques, FIFO : une archive de modèle n'en a aucun
			// besoin, et les suivre serait un autre moyen d'écrire hors du dossier.
			continue
		}
	}
	if err := os.RemoveAll(dest); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// asrSafeRel retire le dossier de premier niveau d'un chemin d'archive et refuse
// tout ce qui sortirait du dossier de destination. Renvoie "" pour l'entrée du
// dossier racine elle-même.
func asrSafeRel(name string) (string, error) {
	n := strings.ReplaceAll(strings.TrimSpace(name), `\`, "/")
	if n == "" || strings.HasPrefix(n, "/") || filepath.IsAbs(n) {
		return "", fmt.Errorf("entrée d'archive au chemin absolu : %q", name)
	}
	parts := strings.Split(strings.Trim(n, "/"), "/")
	for _, p := range parts {
		if p == ".." {
			return "", fmt.Errorf("entrée d'archive qui remonte hors du dossier : %q", name)
		}
	}
	if len(parts) <= 1 {
		return "", nil // le dossier racine de l'archive
	}
	return filepath.Join(parts[1:]...), nil
}
