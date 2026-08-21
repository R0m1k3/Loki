// Catalogue des modèles de dictée. Aucun n'est embarqué dans l'image : ils
// pèsent de 190 Mo à 1,1 Go et vivent dans <LOKI_HOME>/whisper/, donc dans le
// volume /data — ils survivent aux recréations du conteneur.
package loki

import (
	"os"
	"path/filepath"
	"sort"
)

// Les modèles ggml officiels de whisper.cpp.
const whisperModelBase = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/"

type whisperModel struct {
	Nom     string // libellé affiché
	Fichier string // nom du .bin, identique en local et chez Hugging Face
	Octets  int64  // taille du téléchargement, pour prévenir avant de le lancer
	VRAMMo  int    // ordre de grandeur occupé sur le GPU, pour choisir en connaissance de cause
}

// Les tailles sont celles publiées par le dépôt amont. Elles servent à
// informer avant un téléchargement, pas à vérifier le fichier : une taille qui
// dérive d'une release à l'autre ne doit pas casser la dictée.
var whisperCatalogue = map[string]whisperModel{
	"small-q5_1":          {Nom: "small — rapide, correct", Fichier: "ggml-small-q5_1.bin", Octets: 190_000_000, VRAMMo: 600},
	"medium-q5_0":         {Nom: "medium — bon compromis", Fichier: "ggml-medium-q5_0.bin", Octets: 539_000_000, VRAMMo: 1400},
	"large-v3-turbo-q5_0": {Nom: "large-v3-turbo — recommandé", Fichier: "ggml-large-v3-turbo-q5_0.bin", Octets: 574_000_000, VRAMMo: 1600},
	"large-v3-q5_0":       {Nom: "large-v3 — le plus précis, le plus lent", Fichier: "ggml-large-v3-q5_0.bin", Octets: 1_080_000_000, VRAMMo: 3600},
}

// whisperModelPathFor : chemin local. Vide si l'identifiant est inconnu —
// l'appelant doit traiter ce cas plutôt que de bâtir un chemin sur une chaîne
// arbitraire venue d'une requête HTTP.
func whisperModelPathFor(id string) string {
	m, ok := whisperCatalogue[id]
	if !ok {
		return ""
	}
	return filepath.Join(LokiHome(), "whisper", m.Fichier)
}

func whisperModelURLFor(id string) string {
	m, ok := whisperCatalogue[id]
	if !ok {
		return ""
	}
	return whisperModelBase + m.Fichier
}

func whisperModelPresent(id string) bool {
	p := whisperModelPathFor(id)
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.Size() > 0
}

// whisperCatalogueTrie : le catalogue pour l'UI, du plus léger au plus lourd —
// l'ordre dans lequel se pose la question du compromis.
func whisperCatalogueTrie() []map[string]any {
	out := make([]map[string]any, 0, len(whisperCatalogue))
	for id, m := range whisperCatalogue {
		out = append(out, map[string]any{
			"id": id, "nom": m.Nom, "octets": m.Octets,
			"vram_mo": m.VRAMMo, "present": whisperModelPresent(id),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["octets"].(int64) < out[j]["octets"].(int64)
	})
	return out
}
