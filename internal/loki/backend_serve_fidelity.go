package loki

import (
	"fmt"
	"strconv"
	"strings"
)

// Garde-fous de fidélité. Loki ne choisit JAMAIS à la place de l'utilisateur un
// réglage qui change ce que le modèle calcule : pas de cache KV quantifié posé
// d'office, pas de réutilisation approximative du cache, pas de contexte qui
// glisse en jetant des jetons. Ces réglages restent possibles — ce sont des
// compromis légitimes quand la VRAM manque — mais ils doivent se VOIR : un
// preset qui tourne en q8_0 depuis EXTRA_ARGS pendant que l'interface affiche
// « f16 » est exactement l'altération silencieuse qu'on refuse.
//
// Tout ici est pur : buildServeArgs en tire des notes, cmdServe les écrit.

// effectiveKVTypes donne les types de cache K et V que le moteur utilisera
// VRAIMENT : ceux du preset (serveKVTypes), puis ceux d'EXTRA_ARGS par-dessus.
// EXTRA_ARGS ferme la ligne de commande et llama-server retient la dernière
// occurrence d'un drapeau : un -ctk q8_0 écrit à la main l'emporte sur KV_TYPE.
// fromExtra dit qu'EXTRA_ARGS a tranché au moins l'un des deux. Vide = défaut
// du moteur (f16).
func effectiveKVTypes(cfg map[string]string, extra []string) (k, v string, fromExtra bool) {
	k, v = serveKVTypes(cfg)
	if s := flagValue(extra, "-ctk", "--cache-type-k"); s != "" {
		k, fromExtra = s, true
	}
	if s := flagValue(extra, "-ctv", "--cache-type-v"); s != "" {
		v, fromExtra = s, true
	}
	return k, v, fromExtra
}

// kvFidelity classe un type de cache par rapport au f16 de référence. Rang 0 =
// mêmes calculs (f16, ou f32 qui ne perd rien) ; plus le rang monte, plus les
// sorties s'en écartent. Un type inconnu (sorti après cette version) est
// supposé altérer : on préfère un avertissement de trop à un silence.
func kvFidelity(t string) (rank int, label string) {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "", "f16", "f32":
		return 0, ""
	case "bf16":
		return 1, "numérique différente de f16 (mantisse plus courte)"
	case "q8_0", "q5_0", "q5_1":
		return 2, "modifie légèrement les sorties par rapport à f16"
	case "q4_0", "q4_1", "iq4_nl":
		return 3, "perte mesurable par rapport à f16"
	default:
		return 2, "modifie les sorties par rapport à f16"
	}
}

// kvFidelityNote dit, quand le cache n'est pas en f16, ce que ce choix coûte en
// fidélité et d'où il vient. Le choix reste celui du preset : Loki ne le change
// jamais, ni dans un sens ni dans l'autre. Pas d'estimation de la VRAM qu'un
// retour en f16 demanderait : sans les métadonnées du GGUF, ce serait un chiffre
// inventé. En revanche, si le placement est figé à la main (-ot, --n-cpu-moe…),
// --fit ne tourne pas et ne rattrapera pas ce surplus — mieux vaut le savoir
// avant de basculer et de finir en « out of memory ».
func kvFidelityNote(k, v string, fromExtra bool, extra []string) string {
	rk, lk := kvFidelity(k)
	rv, lv := kvFidelity(v)
	if rk == 0 && rv == 0 {
		return ""
	}
	label := lk
	if rv > rk {
		label = lv
	}
	src := "KV_TYPE"
	if fromExtra {
		src = "défini par EXTRA_ARGS"
	}
	note := fmt.Sprintf("cache KV %s/%s (%s) : %s — choix du preset, laissé tel quel.",
		orF16(k), orF16(v), src, label)
	if placementFixed(extra) {
		note += " Repasser en f16 demande plus de VRAM, et le placement fixé par -ot/--n-cpu-moe " +
			"empêche --fit de compenser : il faudrait sans doute relever --n-cpu-moe."
	}
	return note
}

func orF16(t string) string {
	if t == "" {
		return "f16"
	}
	return t
}

// placementFixed : l'utilisateur a placé lui-même des tenseurs. llama.cpp
// abandonne alors son placement automatique (« tensor_buft_overrides already
// set by user, abort ») — rien ne viendra absorber un cache plus gros.
func placementFixed(extra []string) bool {
	return hasAnyFlag(extra, "-ot", "--override-tensor", "--n-cpu-moe", "-ncmoe", "--cpu-moe", "-cmoe")
}

// lossyCacheNotes avertit des drapeaux de cache qui changent ce que voit le
// modèle. Seuls deux le font :
//
//   - --context-shift : contexte plein → le moteur JETTE des jetons anciens et
//     continue, le modèle perd une partie de la conversation sans le savoir ;
//   - --cache-reuse N (N > 0) : recolle des morceaux de cache calculés sous un
//     AUTRE préfixe, simplement décalés — le résultat n'est plus celui d'un
//     calcul complet. --cache-reuse 0 le désactive : rien à dire.
//
// --swa-full n'en fait PAS partie : il garde le cache complet des couches à
// fenêtre glissante, ce qui rend la réutilisation du préfixe exacte (au prix de
// VRAM), et ne fait rien sur un modèle sans SWA. Correspondance exacte des noms
// (hasAnyFlag/flagValue) : --no-context-shift ne déclenche rien, et la dernière
// occurrence gagne comme dans llama-server.
//
// Avec la vision chargée, llama.cpp désactive lui-même les deux (non pris en
// charge en multimodal) : on le dit plutôt que de crier au loup. Les modèles
// hybrides ou récurrents les ignorent aussi, mais sans métadonnées du GGUF on
// ne sait pas les reconnaître ici : on se tait sur ce point plutôt que deviner.
func lossyCacheNotes(extra []string, mmproj bool) []string {
	var notes []string
	ignored := ""
	if mmproj {
		ignored = " (vision chargée : llama.cpp l'ignore de toute façon)"
	}
	if contextShiftOn(extra) {
		notes = append(notes, "avertissement : --context-shift (EXTRA_ARGS) — contexte plein, le moteur jette "+
			"des jetons anciens et le modèle perd une partie de la conversation sans le savoir"+ignored+".")
	}
	if s := flagValue(extra, "--cache-reuse"); s != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
			notes = append(notes, fmt.Sprintf("avertissement : --cache-reuse %d (EXTRA_ARGS) — réutilise des morceaux "+
				"de cache calculés sous un autre préfixe : les sorties ne sont plus exactement celles d'un calcul "+
				"complet%s.", n, ignored))
		}
	}
	return notes
}

// contextShiftOn : --context-shift et --no-context-shift peuvent cohabiter
// (preset copié, puis corrigé) ; comme llama-server, la dernière occurrence
// décide.
func contextShiftOn(extra []string) bool {
	on := false
	for _, a := range extra {
		name, _, _ := strings.Cut(a, "=")
		switch name {
		case "--context-shift":
			on = true
		case "--no-context-shift":
			on = false
		}
	}
	return on
}
