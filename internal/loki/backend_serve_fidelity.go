package loki

import (
	"fmt"
	"os"
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

// fidelityArgEnv : les LLAMA_ARG_* qui, sans drapeau, posent un cache
// quantifié, un cache approché ou la vision. llama.cpp les applique AVANT la
// ligne de commande : un -ctk de KV_TYPE ou d'EXTRA_ARGS les écrase, mais seuls
// ils décident. LLAMA_ARG_NO_CONTEXT_SHIFT est celle des moteurs anciens.
var fidelityArgEnv = []string{"LLAMA_ARG_CACHE_TYPE_K", "LLAMA_ARG_CACHE_TYPE_V",
	"LLAMA_ARG_CONTEXT_SHIFT", "LLAMA_ARG_NO_CONTEXT_SHIFT", "LLAMA_ARG_CACHE_REUSE",
	"LLAMA_ARG_MMPROJ", "LLAMA_ARG_MMPROJ_URL", "LLAMA_ARG_SPEC_SYNTH_LEN"}

// probeFidelityEnv complète ArgEnv avec fidelityArgEnv. Appelée après
// probeServeGPUs, qui crée ArgEnv.
func probeFidelityEnv(si *serveSysInfo) {
	if si.ArgEnv == nil {
		si.ArgEnv = map[string]string{}
	}
	for _, k := range fidelityArgEnv {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			si.ArgEnv[k] = v
		}
	}
}

// effectiveKVTypes donne les types de cache K et V que le moteur utilisera
// VRAIMENT. Trois sources, de la plus faible à la plus forte : les variables
// LLAMA_ARG_CACHE_TYPE_K/V (lues avant la ligne de commande), le preset
// (serveKVTypes, traduit en -ctk/-ctv), puis EXTRA_ARGS, qui ferme la ligne :
// llama-server retient la dernière occurrence d'un drapeau, un -ctk q8_0 écrit
// à la main l'emporte donc sur KV_TYPE. src nomme la source la plus forte qui a
// tranché l'un des deux (vide : aucune). Vide = défaut du moteur (f16).
func effectiveKVTypes(cfg map[string]string, extra []string, argEnv map[string]string) (k, v, src string) {
	k, v = argEnv["LLAMA_ARG_CACHE_TYPE_K"], argEnv["LLAMA_ARG_CACHE_TYPE_V"]
	if k != "" || v != "" {
		src = "défini par LLAMA_ARG_CACHE_TYPE_K/V"
	}
	if ck, cv := serveKVTypes(cfg); ck != "" || cv != "" {
		src = "KV_TYPE"
		if ck != "" {
			k = ck
		}
		if cv != "" {
			v = cv
		}
	}
	if s := flagValue(extra, "-ctk", "--cache-type-k"); s != "" {
		k, src = s, "défini par EXTRA_ARGS"
	}
	if s := flagValue(extra, "-ctv", "--cache-type-v"); s != "" {
		v, src = s, "défini par EXTRA_ARGS"
	}
	return k, v, src
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
// inventé. En revanche, si le placement est figé à la main (-ot, --n-cpu-moe N>0,
// en drapeau ou en variable : tensorOverride), --fit ne tourne pas et ne
// rattrapera pas ce surplus — mieux vaut le savoir avant de basculer et de
// finir en « out of memory ».
func kvFidelityNote(k, v, src string, extra []string, argEnv map[string]string) string {
	rk, lk := kvFidelity(k)
	rv, lv := kvFidelity(v)
	if rk == 0 && rv == 0 {
		return ""
	}
	label := lk
	if rv > rk {
		label = lv
	}
	note := fmt.Sprintf("cache KV %s/%s (%s) : %s — choix de l'utilisateur, laissé tel quel.",
		orF16(k), orF16(v), src, label)
	if tensorOverride(extra, argEnv) != "" {
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

// lossyCacheNotes avertit des réglages de cache qui changent ce que voit le
// modèle. Seuls deux le font :
//
//   - le glissement de contexte : contexte plein → le moteur JETTE des jetons
//     anciens et continue, le modèle perd une partie de la conversation sans le
//     savoir ;
//   - --cache-reuse N (N > 0) : recolle des morceaux de cache calculés sous un
//     AUTRE préfixe, simplement décalés — le résultat n'est plus celui d'un
//     calcul complet. --cache-reuse 0 le désactive : rien à dire.
//
// --swa-full n'en fait PAS partie : il garde le cache complet des couches à
// fenêtre glissante, ce qui rend la réutilisation du préfixe exacte (au prix de
// VRAM), et ne fait rien sur un modèle sans SWA. Correspondance exacte des noms
// (flagValue) : --no-context-shift ne déclenche rien, et la dernière occurrence
// gagne comme dans llama-server. Les variables LLAMA_ARG_* comptent quand la
// ligne de commande se tait.
//
// Avec la vision chargée (MMPROJ, --mmproj d'EXTRA_ARGS ou LLAMA_ARG_MMPROJ),
// llama.cpp désactive lui-même les deux (non pris en charge en multimodal) : on
// le dit plutôt que de crier au loup. Les modèles hybrides ou récurrents les
// ignorent aussi, mais sans métadonnées du GGUF on ne sait pas les reconnaître
// ici : on se tait sur ce point plutôt que deviner.
func lossyCacheNotes(extra []string, si serveSysInfo) []string {
	var notes []string
	ignored := ""
	if si.MMProj != "" || hasAnyFlag(extra, "-mm", "--mmproj", "-mmu", "--mmproj-url") ||
		si.ArgEnv["LLAMA_ARG_MMPROJ"] != "" || si.ArgEnv["LLAMA_ARG_MMPROJ_URL"] != "" {
		ignored = " (vision chargée : llama.cpp l'ignore de toute façon)"
	}
	switch on, src := contextShift(extra, si.ArgEnv, si.Help); {
	case on && src == "":
		notes = append(notes, "avertissement : ce llama-server ancien glisse le contexte par défaut — contexte "+
			"plein, il jette des jetons anciens et le modèle perd une partie de la conversation sans le savoir"+
			ignored+". --no-context-shift dans EXTRA_ARGS l'en empêche.")
	case on:
		notes = append(notes, "avertissement : --context-shift ("+src+") — contexte plein, le moteur jette "+
			"des jetons anciens et le modèle perd une partie de la conversation sans le savoir"+ignored+".")
	}
	s, src := flagValue(extra, "--cache-reuse"), "EXTRA_ARGS"
	if s == "" {
		s, src = si.ArgEnv["LLAMA_ARG_CACHE_REUSE"], "LLAMA_ARG_CACHE_REUSE"
	}
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
		notes = append(notes, fmt.Sprintf("avertissement : --cache-reuse %d (%s) — réutilise des morceaux "+
			"de cache calculés sous un autre préfixe : les sorties ne sont plus exactement celles d'un calcul "+
			"complet%s.", n, src, ignored))
	}
	// Acceptation SYNTHÉTIQUE (« benchmarking only ») : le moteur garde des
	// jetons du brouillon au hasard, sans les comparer à ce qu'aurait tiré le
	// modèle. Loki ne les pose jamais ; écrits à la main, on le crie.
	if f := hasFlagPrefix(extra, "--spec-synth-"); f != "" || si.ArgEnv["LLAMA_ARG_SPEC_SYNTH_LEN"] != "" {
		if f == "" {
			f = "LLAMA_ARG_SPEC_SYNTH_LEN"
		}
		notes = append(notes, "avertissement : "+f+" — le moteur accepte des jetons du brouillon AU HASARD, sans "+
			"vérification : les réponses ne sont plus celles du modèle. Réservé aux mesures, retire-le.")
	}
	return notes
}

// contextShift dit si le moteur glissera le contexte, et qui l'a décidé (vide :
// son propre défaut). --context-shift et --no-context-shift peuvent cohabiter
// dans EXTRA_ARGS (preset copié, puis corrigé) ; comme llama-server, la
// dernière occurrence décide. Sans drapeau, la variable du moteur, puis son
// défaut — qui a changé : les llama-server d'avant --context-shift (mi-2025) ne
// connaissent que --no-context-shift et GLISSENT par défaut. Aide vide (moteur
// inconnu) : on ne suppose rien.
func contextShift(extra []string, argEnv map[string]string, help string) (on bool, src string) {
	set := false
	for _, a := range extra {
		name, _, _ := strings.Cut(a, "=")
		switch name {
		case "--context-shift":
			on, set = true, true
		case "--no-context-shift":
			on, set = false, true
		}
	}
	if set {
		return on, "EXTRA_ARGS"
	}
	if strings.Contains(help, "--no-context-shift") && !strings.Contains(help, "--context-shift") {
		// Moteur ancien : seul LLAMA_ARG_NO_CONTEXT_SHIFT (lu comme un booléen
		// vrai) l'arrête ; LLAMA_ARG_CONTEXT_SHIFT lui est inconnue.
		if envTruthy(argEnv["LLAMA_ARG_NO_CONTEXT_SHIFT"]) {
			return false, "LLAMA_ARG_NO_CONTEXT_SHIFT"
		}
		return true, ""
	}
	if envTruthy(argEnv["LLAMA_ARG_CONTEXT_SHIFT"]) {
		return true, "LLAMA_ARG_CONTEXT_SHIFT"
	}
	return false, ""
}
