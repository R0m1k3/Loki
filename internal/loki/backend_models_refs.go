package loki

// backend_models_refs.go — QUI référence un .gguf, et peut-on l'effacer ?
//
// Vécu qui a produit ce fichier : un modèle téléchargé depuis l'éditeur d'un
// preset, le preset supprimé (sans cocher « supprimer aussi le .gguf »), et
// 40 Go de fichier plus rien pour les reprendre — la route /api/models/delete
// existait mais AUCUN bouton ne l'appelait. Supprimer un modèle depuis
// l'interface demande deux choses : savoir qui s'en sert (le moteur en service,
// d'autres presets), et le dire AVANT d'effacer.

import (
	"os"
	"path/filepath"
	"strings"
)

// modelRefIndex — l'index « ce fichier est référencé par… », construit en une
// seule passe. Demander « qui utilise ce modèle ? » fichier par fichier
// relisait tous les presets à chaque ligne de la liste des modèles.
//
// Les clés sont des chemins normalisés (normDir) : un preset peut nommer son
// modèle en chemin absolu et un autre en simple nom de fichier, c'est le même
// fichier sur le disque.
type modelRefIndex struct {
	active  map[string]bool     // référencé par config.env — donc par le moteur qui tourne
	presets map[string][]string // chemin -> noms d'affichage des presets
}

// modelRefKey normalise une valeur MODEL=/MMPROJ= en clé d'index, ou "" si elle
// ne désigne pas un .gguf. resolveModelPath accepte le nom simple comme le
// chemin absolu et les résout dans les dossiers déclarés — c'est exactement la
// règle qu'appliquera le moteur.
func modelRefKey(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	p, err := resolveServeModelPath(v)
	if err != nil {
		// Introuvable : on garde quand même une clé comparable, sinon deux
		// presets qui nomment le même fichier absent paraîtraient distincts.
		p = filepath.Join(modelsDir(), baseName(v))
	}
	return normDir(p)
}

// modelKeysOf liste les .gguf référencés par une configuration : le modèle, le
// projecteur vision, et un --mmproj posé à la main dans EXTRA_ARGS (les presets
// d'avant la clé MMPROJ l'écrivaient là).
func modelKeysOf(env map[string]string) []string {
	vals := []string{env["MODEL"], env["MMPROJ"]}
	args := splitArgs(env["EXTRA_ARGS"])
	for i, a := range args {
		if a == "--mmproj" && i+1 < len(args) {
			vals = append(vals, args[i+1])
		}
	}
	out := []string{}
	for _, v := range vals {
		if k := modelRefKey(v); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// buildModelRefIndex lit la configuration active et tous les presets.
func buildModelRefIndex() modelRefIndex {
	ix := modelRefIndex{active: map[string]bool{}, presets: map[string][]string{}}
	for _, k := range modelKeysOf(ReadConfig()) {
		ix.active[k] = true
	}
	list, err := ListPresets()
	if err != nil {
		return ix
	}
	for _, p := range list {
		content, err := ReadPreset(p.ID)
		if err != nil {
			continue
		}
		seen := map[string]bool{}
		for _, k := range modelKeysOf(parseEnv(content)) {
			if seen[k] {
				continue // MODEL et MMPROJ identiques : un seul « utilisé par »
			}
			seen[k] = true
			ix.presets[k] = append(ix.presets[k], p.Name)
		}
	}
	return ix
}

// lookup renvoie les presets qui référencent path et s'il est en service.
func (ix modelRefIndex) lookup(path string) (presets []string, active bool) {
	k := normDir(path)
	return ix.presets[k], ix.active[k]
}

// modelUsers répond pour UN modèle (nom de fichier ou chemin).
func modelUsers(name string) (presets []string, active bool) {
	k := modelRefKey(name)
	if k == "" {
		return nil, false
	}
	return buildModelRefIndex().lookup(k)
}

// modelInServiceErr est le refus opposé à la suppression d'un modèle que le
// moteur a ouvert. Effacer le .gguf sous un llama-server qui tourne ne libère
// même pas la place (le fichier reste mappé) et rend le prochain démarrage
// impossible sans rien dire.
const modelInServiceMsg = "modèle en service — bascule sur un autre preset avant de le supprimer"

// modelFamilySize additionne ce que la suppression libérera réellement
// (toutes les tranches présentes).
func modelFamilySize(p string) int64 {
	return shardFamilySize(filepath.Dir(p), filepath.Base(p))
}

// modelPresence dit si un modèle est DÉJÀ sur le disque, et sous quelle valeur
// l'écrire dans MODEL=. dir restreint la recherche à ce dossier (celui visé par
// un téléchargement) ; vide = tous les dossiers déclarés.
//
// C'est ce qui transforme « le modèle existe déjà » — un cul-de-sac rouge au
// bout d'un clic sur un quant — en « déjà là, je le sélectionne ».
func modelPresence(name, dir string) (value, path string, ok bool) {
	base := baseName(strings.TrimSpace(name))
	if base == "" || !strings.HasSuffix(strings.ToLower(base), ".gguf") {
		return "", "", false
	}
	dirs := modelDirs()
	if strings.TrimSpace(dir) != "" {
		if d, err := resolveDownloadDir(dir); err == nil {
			dirs = []string{d}
		}
	}
	for _, d := range dirs {
		p := filepath.Join(d, base)
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			continue
		}
		// Famille incomplète = modèle inutilisable : on ne prétend pas qu'il est
		// là, l'appelant doit pouvoir relancer le téléchargement des tranches
		// manquantes (handleModelDownload les reprend une par une).
		if len(shardFamilyMissing(d, base)) > 0 {
			continue
		}
		return modelPickerValue(d, base), p, true
	}
	return "", "", false
}

// modelPickerValue est la valeur à écrire dans MODEL= pour un fichier donné :
// le simple nom quand il vit dans le dossier de téléchargement de Loki (le
// preset reste alors portable d'une machine à l'autre), le chemin complet
// ailleurs. Même règle que la liste /api/models.
func modelPickerValue(dir, base string) string {
	if normDir(dir) == normDir(modelsDir()) {
		return base
	}
	return filepath.Join(dir, base)
}
