package loki

// backend_catalog.go — ce que la machine peut avaler, et si un modèle y tient.
//
// Ce fichier servait autrefois un catalogue de modèles téléchargé depuis
// ajean.link/models.json — le serveur de l'auteur du projet dont Loki est un
// fork. Deux raisons de l'avoir retiré : sa route n'avait AUCUN consommateur
// (l'« écran d'accueil » qu'elle attendait n'a jamais existé), et un fork qui
// laisse un tiers décider de ce qu'il propose n'est pas vraiment un fork. Les
// modèles se cherchent maintenant directement sur Hugging Face
// (backend_hf.go).
//
// Reste ici la seule question qui vaille avant de lancer un téléchargement de
// 20 Go : est-ce que ça tiendra ?

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

type gpuBrief struct {
	Name   string  `json:"name"`
	VRAMGB float64 `json:"vram_gb"`
}

type hardwareInfo struct {
	OS    string  `json:"os"`
	Arch  string  `json:"arch"`
	RAMGB float64 `json:"ram_gb"`
	// VRAMGB : somme de la mémoire des GPU détectés. 0 = pas de GPU visible
	// (ni nvidia-smi, ni carte) — le verdict retombe alors sur la RAM système,
	// ce qui est le bon repère pour une inférence CPU.
	VRAMGB float64    `json:"vram_gb"`
	GPUs   []gpuBrief `json:"gpus"`
}

// detectHardware décrit la machine. La VRAM vient de detectGPUs
// (backend_gpu.go, via nvidia-smi) : sans elle, le verdict d'un serveur à GPU
// se prononçait sur la RAM système, donc à côté de la plaque — c'est le GPU
// qui porte le modèle quand NGL l'y envoie.
func detectHardware() hardwareInfo {
	h := hardwareInfo{OS: runtime.GOOS, Arch: runtime.GOARCH, RAMGB: totalRAMGB()}
	gpus, err := detectGPUs()
	if err != nil {
		return h // pas de GPU visible : VRAMGB reste à 0, et c'est une info
	}
	for _, g := range gpus {
		// nvidia-smi rend des MiB en --format=nounits.
		mib, convErr := strconv.ParseFloat(strings.TrimSpace(g.MemTotal), 64)
		if convErr != nil {
			continue
		}
		gb := mib / 1024
		h.VRAMGB += gb
		h.GPUs = append(h.GPUs, gpuBrief{Name: g.Name, VRAMGB: gb})
	}
	return h
}

// Verdicts rendus par fitVerdict.
const (
	fitOK    = "ok"    // confortable
	fitTight = "juste" // ça passe, sans marge
	fitOver  = "trop"  // ça ne rentre pas
)

// ctxOverheadGB estime ce que le cache KV va prendre en plus des poids.
//
// ⚠️ C'est une RÈGLE GROSSIÈRE, et l'interface doit le dire. Le coût exact d'un
// cache KV dépend de l'architecture du modèle (nombre de couches, têtes KV,
// dimension des têtes), qu'une simple liste de fichiers ne révèle pas. Le
// connaître demanderait de lire l'en-tête GGUF par requête Range et d'en parser
// les métadonnées — faisable, mais ce n'est pas ce que fait ce code.
//
// L'ordre de grandeur retenu : ~1 Go par tranche de 32k de contexte pour un
// modèle de 20 Go, proportionnel à la taille des poids. Mieux vaut une
// fourchette annoncée comme telle qu'un chiffre faussement précis.
func ctxOverheadGB(weightsGB float64, ctxTokens int) float64 {
	if ctxTokens <= 0 {
		ctxTokens = 32768
	}
	return weightsGB / 20 * (float64(ctxTokens) / 32768)
}

// fitVerdict dit si un modèle tient, et POURQUOI. La phrase compte autant que
// le verdict : « trop » sans explication laisse l'utilisateur deviner s'il doit
// changer de quantification, baisser le contexte ou renoncer au projecteur.
func fitVerdict(h hardwareInfo, weights, mmproj int64, ctxTokens int) (string, string) {
	const gb = float64(1 << 30)
	wGB := float64(weights) / gb
	mGB := float64(mmproj) / gb
	kvGB := ctxOverheadGB(wGB, ctxTokens)
	need := wGB + mGB + kvGB

	budget, where := h.VRAMGB, "VRAM"
	if budget <= 0 {
		budget, where = h.RAMGB, "RAM"
	}
	if budget <= 0 {
		return "", "" // machine non mesurable : pas de verdict inventé
	}

	detail := fmt.Sprintf("%.1f Go de poids", wGB)
	if mGB > 0 {
		detail += fmt.Sprintf(" + %.1f Go de projecteur", mGB)
	}
	detail += fmt.Sprintf(" + ~%.1f Go de contexte (estimation) = ~%.1f Go pour %.1f Go de %s",
		kvGB, need, budget, where)

	switch {
	case need > budget:
		return fitOver, "ne tient pas : " + detail
	case need > budget*0.9:
		return fitTight, "ça passe de justesse : " + detail
	default:
		return fitOK, detail
	}
}
