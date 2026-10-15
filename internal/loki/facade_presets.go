package loki

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// facade_presets.go — presets tenus par la façade Rust (LOKI_PRESETS=rust,
// lot 15 de la migration ; expérimental, le Go reste le défaut) : liste,
// ordre, lecture, éditeur, suppression, aperçus de l'éditeur, copie en
// placement auto, presets externes, bascule.
//
// Les presets restent des fichiers presets/*.env et leur prompt système une
// clé de la base. Ce qui touche au moteur reste ici jusqu'au lot qui le
// portera : verrou de l'optimiseur (/internal/v1/tune/guard) et application
// d'un preset à la configuration vivante, avec l'état du slot gardé et le
// moteur relancé ou arrêté (/internal/v1/preset/apply).

const facadePresetsEnv = "LOKI_PRESETS"

// facadePresets : la façade tient-elle les presets ?
func facadePresets() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(facadePresetsEnv)), "rust") &&
		facadeAddr() != "" && internalToken() != ""
}

// facadePresetsRoutes : les routes que la façade sert alors (presets_api.rs).
var facadePresetsRoutes = map[string]bool{
	"/api/presets":              true,
	"/api/presets/order":        true,
	"/api/preset":               true,
	"/api/preset/save":          true,
	"/api/preset/cacheram":      true,
	"/api/preset/moe":           true,
	"/api/preset/autoplace":     true,
	"/api/preset/delete":        true,
	"/api/preset/external":      true,
	"/api/preset/external/save": true,
	"/api/preset/external/test": true,
	"/api/switch":               true,
}

// presetServiceAction : le pilotage du moteur après une application de
// preset. Variable pour les tests (aucun moteur n'y tourne).
var presetServiceAction = serviceAction

// presetApply applique un preset (chemin dans presets/) à la configuration
// vivante, comme les handlers d'origine :
//
//	switch : bascule (handleSwitch) — état du slot relevé avant, moteur arrêté
//	         pour un preset externe, relancé sinon, en arrière-plan ;
//	save   : preset en service modifié (handlePresetSave) — relancé ;
//	apply  : preset externe en service modifié (handlePresetExternalSave) —
//	         configuration seule.
func presetApply(path, mode string) error {
	root, err := filepath.Abs(presetsDir())
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil || !strings.HasPrefix(abs, root+string(filepath.Separator)) || !strings.HasSuffix(abs, ".env") {
		return fmt.Errorf("preset hors du dossier des presets")
	}
	switch mode {
	case "switch":
		persist := slotPersistPrepare()
		if err := applyPresetFile(abs); err != nil {
			return err
		}
		fmt.Printf("%s config.env <- %s\n", green("[ok]"), filepath.Base(abs))
		if isExternalConfig(ReadConfig()) {
			fmt.Println(dim("[info] preset externe — arrêt du moteur local"))
			go func() {
				if serviceIsActive() {
					persist.save()
					if err := presetServiceAction("stop"); err != nil {
						fmt.Printf("%s arrêt du moteur après bascule externe : %v\n", red("[ERREUR]"), err)
					}
				}
			}()
			return nil
		}
		fmt.Println(dim("[info] redémarrage du service..."))
		go func() {
			persist.save()
			if err := presetServiceAction("restart"); err != nil {
				fmt.Printf("%s redémarrage après bascule: %v\n", red("[ERREUR]"), err)
			}
		}()
		return nil
	case "save":
		if err := applyPresetFile(abs); err != nil {
			return err
		}
		fmt.Printf("%s config.env <- %s (preset actif modifié)\n", green("[ok]"), filepath.Base(abs))
		fmt.Println(dim("[info] redémarrage du service..."))
		go func() {
			if err := presetServiceAction("restart"); err != nil {
				fmt.Printf("%s redémarrage après modification du preset: %v\n", red("[ERREUR]"), err)
			}
		}()
		return nil
	case "apply":
		return applyPresetFile(abs)
	}
	return fmt.Errorf("mode inconnu : %q", mode)
}

// handleInternalPresetApply : POST {path, mode} → 204, ou 400/500 + {error}.
func handleInternalPresetApply(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	if err := presetApply(in.Path, in.Mode); err != nil {
		sendJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleInternalTuneGuard : GET → {busy, error} — une optimisation tient-elle
// le moteur (tuneGuard) ?
func handleInternalTuneGuard(w http.ResponseWriter, r *http.Request) {
	if err := tuneGuard(); err != nil {
		sendJSON(w, http.StatusOK, map[string]any{"busy": true, "error": err.Error()})
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{"busy": false})
}
