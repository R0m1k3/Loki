package loki

// engine_cmdline.go — ce que l'interface montre du dernier lancement du moteur
// (repris d'AJEAN, #108 et #43) :
//
//	/api/engine/cmdline  la commande exacte lancée par `loki serve` (variables
//	                     d'environnement posées par Loki comprises), copiable
//	                     pour la vérifier ou la partager ; clé API masquée
//	/api/model/layers    le nombre de couches du modèle, lu dans l'en-tête
//	                     GGUF : la valeur de NGL qui met tout sur le GPU

import (
	"net/http"
	"sort"
	"strings"
)

// engineCmdlineKey : dernière commande du moteur (bkState), lue par l'UI.
const engineCmdlineKey = "engine_cmdline"

// engineCmdline met les arguments sous forme copiable dans un terminal :
// variables d'environnement en tête (KEY=VAL, triées), guillemets autour de ce
// qui contient un espace, clé API masquée.
func engineCmdline(env map[string]string, args []string) string {
	quote := func(a string) string {
		if a == "" || strings.ContainsAny(a, " \t\"'") {
			return `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
		}
		return a
	}
	var out []string
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+quote(env[k]))
	}
	for i, a := range args {
		if i > 0 && (args[i-1] == "--api-key" || args[i-1] == "--api-key-file") {
			a = "<clé masquée>"
		}
		out = append(out, quote(a))
	}
	return strings.Join(out, " ")
}

// recordEngineCmdline garde la commande du lancement en cours. Jamais pour un
// essai de l'optimiseur : il écraserait celle du vrai moteur.
func recordEngineCmdline(env map[string]string, args []string) {
	_ = putBytes(bkState, engineCmdlineKey, []byte(engineCmdline(env, args)))
}

// handleEngineCmdline (GET) : la commande du dernier lancement du moteur local.
func handleEngineCmdline(w http.ResponseWriter, r *http.Request) {
	sendJSON(w, 200, map[string]any{"ok": true, "cmdline": string(getBytes(bkState, engineCmdlineKey))})
}

// handleModelLayers (GET ?model=…, sinon MODEL de la config) : nombre de blocs
// du modèle. llama.cpp en offloade block_count + 1 (la couche de sortie en
// plus) : c'est la valeur de -ngl qui met tout le modèle sur le GPU.
func handleModelLayers(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		model = strings.TrimSpace(ReadConfig()["MODEL"])
	}
	if model == "" {
		sendJSON(w, 200, map[string]any{"ok": false, "error": "aucun modèle"})
		return
	}
	path, err := resolveServeModelPath(model)
	if err != nil {
		sendJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	info, err := ggufMeta(path)
	if err != nil || info.BlockCount <= 0 {
		msg := "nombre de couches absent de l'en-tête"
		if err != nil {
			msg = err.Error()
		}
		sendJSON(w, 200, map[string]any{"ok": false, "error": msg})
		return
	}
	sendJSON(w, 200, map[string]any{"ok": true, "layers": info.BlockCount, "ngl_max": info.BlockCount + 1})
}
