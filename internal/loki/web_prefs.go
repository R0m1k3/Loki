package loki

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// Préférences d'apparence de l'UI web (thème, mode d'affichage). Stockées côté
// serveur — sur la machine Loki — pour être partagées entre tous les appareils
// qui pilotent la même instance (téléphone, laptop, accès distant ajean.link).
// Avant ça, elles ne vivaient qu'en localStorage, donc par navigateur/appareil.

var webPrefsMu sync.Mutex

// webPrefsAllowed liste les clés de préférence acceptées et, pour chacune, les
// valeurs valides. On ne stocke que ce qui est connu (pas de champ libre).
var webPrefsAllowed = map[string]map[string]bool{
	"theme":          {"light": true, "dark": true},
	"hide_reasoning": {"0": true, "1": true},
	"hide_tools":     {"0": true, "1": true},
	"fold_tools":     {"0": true, "1": true},
	"hide_side":      {"0": true, "1": true},
	"hide_stats":     {"0": true, "1": true},
}

// avatarChoices : les emojis proposés pour toi et pour Loki. Liste FERMÉE —
// n'accepter qu'un jeu connu évite de stocker une chaîne arbitraire qui finirait
// affichée dans chaque bulle du chat.
var avatarChoices = []string{
	"🙂", "😎", "🧑", "👤", "🦊", "🐱", "🐼", "🦉",
	"🚀", "⚡", "🌙", "🔥", "🌿", "🎯", "🧠", "🤖",
}

// webPrefsText : préférences à valeur libre, avec un nettoyage propre à chacune.
// Renvoie la valeur retenue et si elle est acceptable.
var webPrefsText = map[string]func(string) (string, bool){
	// Prénom affiché dans le chat : une ligne, sans caractères de contrôle, et
	// borné en RUNES (24 « é » ne doivent pas compter double).
	"user_name": func(v string) (string, bool) {
		v = strings.TrimSpace(strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, v))
		if r := []rune(v); len(r) > 24 {
			v = strings.TrimSpace(string(r[:24]))
		}
		return v, true // vide = revenir au libellé par défaut
	},
	"avatar_user": avatarPref,
	"avatar_ai":   avatarPref,
}

// avatarPref n'accepte qu'un emoji de la liste fermée (ou le vide, qui rétablit
// le défaut).
func avatarPref(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", true
	}
	for _, a := range avatarChoices {
		if a == v {
			return v, true
		}
	}
	return "", false
}

// loadWebPrefs lit les préférences enregistrées (map vide si aucune).
func loadWebPrefs() map[string]string {
	out := map[string]string{}
	for k, v := range allKV(bkPrefs) {
		out[k] = v
	}
	return out
}

// saveWebPrefs fusionne les valeurs valides de `in` puis enregistre le tout.
func saveWebPrefs(in map[string]string) (map[string]string, error) {
	webPrefsMu.Lock()
	defer webPrefsMu.Unlock()
	prefs := loadWebPrefs()
	for k, v := range in {
		if clean, ok := webPrefsText[k]; ok {
			if val, good := clean(v); good {
				prefs[k] = val
			}
			continue
		}
		allowed, ok := webPrefsAllowed[k]
		if !ok {
			continue
		}
		v = strings.ToLower(strings.TrimSpace(v))
		if !allowed[v] {
			continue
		}
		prefs[k] = v
	}
	return prefs, replaceKV(bkPrefs, prefs)
}

// handleWebPrefs expose les préférences d'apparence partagées entre appareils.
//
//	GET  → {ok, prefs:{theme, hide_*, fold_*}}
//	POST {theme?, hide_*?, fold_*?} → fusionne puis renvoie {ok, prefs}
func handleWebPrefs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		prefs, err := saveWebPrefs(req)
		if err != nil {
			sendJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		sendJSON(w, 200, map[string]any{"ok": true, "prefs": prefs})
		return
	}
	// avatars : la liste fermée est servie avec les préférences, pour que l'UI
	// n'ait pas sa propre copie (deux listes divergent tôt ou tard).
	sendJSON(w, 200, map[string]any{"ok": true, "prefs": loadWebPrefs(), "avatars": avatarChoices})
}
