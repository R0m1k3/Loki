// web_engine.go — panneau « Moteur de l'image » : voir la version de llama.cpp
// qui tourne, savoir s'il en existe une plus récente, l'installer, et revenir
// en arrière si elle ne convient pas.
//
//	GET  /api/engine         → version courante, versions installées, variante
//	POST /api/engine/check   → dernier build publié vs celui qui tourne
//	POST /api/engine/update  → job : téléchargement + test à blanc + bascule
//	POST /api/engine/use     → bascule vers une version déjà installée
//	POST /api/engine/remove  → supprime une version installée
//
// Jusqu'ici, mettre à jour llama.cpp imposait de reconstruire l'image de Loki —
// donc un rebuild complet et un redéploiement pour un composant qui publie
// plusieurs versions par jour. Le panneau ne le disait même pas : il annonçait
// « rien à mettre à jour ici ». Ces quatre routes suppriment ce détour.
package loki

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// engineUseImage est la valeur de `tag` qui désigne le moteur d'origine, celui
// que l'image Docker fournit dans /app. C'est le retour arrière garanti : il ne
// dépend d'aucun téléchargement et ne peut pas manquer.
const engineUseImage = "image"

// handleEngineStatus décrit le moteur courant et ce qui est installé à côté.
func handleEngineStatus(w http.ResponseWriter, r *http.Request) {
	bin := currentEngineBin()
	build, commit := engineBuildOf(bin)
	imageBin := providedEngineBin()

	source := "custom"
	switch {
	case imageBin != "" && samePath(bin, imageBin):
		source = "image"
	case engineOwns(bin):
		source = "downloaded"
	case prebuiltOwns(bin):
		source = "prebuilt"
	}

	var installed []map[string]any
	for _, tag := range engineInstalled() {
		p := engineBinFor(tag)
		installed = append(installed, map[string]any{
			"tag":    tag,
			"bin":    p,
			"build":  engineTagBuild(tag),
			"in_use": samePath(p, bin),
		})
	}

	sendJSON(w, 200, map[string]any{
		"supported": engineOCISupported(),
		"variant":   engineVariant(),
		"repo":      ociHost() + "/" + ociEngineRepo,
		"bin":       bin,
		"build":     build,
		"commit":    commit,
		"source":    source,
		"image_bin": imageBin,
		"dir":       engineDir(),
		"installed": installed,
		"job":       lcJobSnapshot(0, false),
	})
}

// handleEngineCheck interroge le registre : quel est le dernier build publié
// pour cette variante, et sommes-nous en retard ? Synchrone, quelques secondes.
func handleEngineCheck(w http.ResponseWriter, r *http.Request) {
	if !engineOCISupported() {
		sendJSON(w, 200, map[string]any{"ok": false, "error": engineUnsupportedWhy()})
		return
	}
	variant := engineVariant()
	build, tag, err := engineOCILatest(variant)
	if err != nil {
		sendJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cur, _ := engineBuildOf(currentEngineBin())
	sendJSON(w, 200, map[string]any{
		"ok":      true,
		"variant": variant,
		"latest":  build,
		"tag":     tag,
		"current": cur,
		// Un build courant inconnu (0) ne prouve pas qu'on est à jour : on
		// propose alors la mise à jour plutôt que d'affirmer le contraire.
		"update": build == 0 || cur == 0 || build > cur,
	})
}

// handleEngineUpdate lance le job de mise à jour. {tag} permet d'épingler une
// version précise ; sans lui, on prend la dernière publiée pour la variante.
func handleEngineUpdate(w http.ResponseWriter, r *http.Request) {
	if !engineOCISupported() {
		sendJSON(w, 400, map[string]any{"ok": false, "error": engineUnsupportedWhy()})
		return
	}
	var req struct {
		Tag string `json:"tag"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	tag := strings.TrimSpace(req.Tag)
	if err := startLcJob("engine", func() { engineRunUpdate(tag) }); err != nil {
		sendJSON(w, 409, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sendJSON(w, 200, map[string]any{"ok": true})
}

func engineUnsupportedWhy() string {
	return "les images officielles llama.cpp n'existent que pour Linux x86-64 et arm64 ; " +
		"sur cette machine, utilise « llama.cpp précompilé » ou « llama.cpp compilé »."
}

// engineRunUpdate : corps du job. Télécharger, ESSAYER, puis seulement basculer.
// L'ordre compte — le seul vrai risque ici est un moteur plus récent que le
// runtime CUDA de l'image, et on le découvre au test à blanc, moteur courant
// encore intact.
func engineRunUpdate(tag string) {
	variant := engineVariant()
	ancien := currentEngineBin()
	if tag == "" {
		lcPhase("recherche de la dernière version publiée…")
		build, latest, err := engineOCILatest(variant)
		if err != nil {
			lcFail(err)
			return
		}
		tag = latest
		if build > 0 {
			if cur, _ := engineBuildOf(currentEngineBin()); cur == build {
				lcDone(fmt.Sprintf("déjà à jour (build %d)", build))
				return
			}
		}
	}
	lcAppend("variante : " + variant + " — version visée : " + tag)

	bin, err := engineOCIInstall(tag, lcAppend, lcPhase)
	if err != nil {
		lcFail(err)
		return
	}

	lcPhase("essai du nouveau moteur…")
	if err := engineSmokeTest(bin, ancien); err != nil {
		// On ne garde pas un moteur inutilisable : il encombrerait la liste et
		// inviterait à le sélectionner.
		_ = os.RemoveAll(filepath.Dir(bin))
		lcFail(fmt.Errorf("%w\n\nLe moteur courant n'a PAS été touché. Cette version demande sans doute un runtime CUDA plus récent que celui de l'image : reconstruis l'image avec un LLAMACPP_IMAGE récent, ou épingle un build antérieur", err))
		return
	}
	if build, commit := engineBuildOf(bin); build > 0 {
		lcAppend(fmt.Sprintf("moteur testé : build %d (%s)", build, commit))
	}

	if err := engineSwitchTo(bin); err != nil {
		lcFail(err)
		return
	}
	// On garde la version qu'on vient de poser ; les précédentes ne servent plus
	// (le retour arrière se fait sur le moteur de l'image, qui, lui, est intact).
	enginePrune(map[string]bool{filepath.Base(filepath.Dir(bin)): true}, lcAppend)
	lcDone("moteur mis à jour (" + tag + ")")
}

// engineSwitchTo pointe BIN sur un moteur et redémarre le service s'il tournait.
func engineSwitchTo(bin string) error {
	wasUp := serviceIsActive()
	if wasUp {
		lcPhase("arrêt du moteur…")
		if err := serviceAction("stop"); err != nil {
			lcAppend("[warn] arrêt du moteur : " + err.Error())
		}
	}
	if err := SetConfigKey("BIN", bin); err != nil {
		if wasUp {
			_ = serviceAction("start")
		}
		return fmt.Errorf("moteur installé mais échec d'écriture de BIN : %w", err)
	}
	lcAppend("BIN → " + bin)
	if wasUp {
		lcPhase("redémarrage du moteur…")
		if err := serviceAction("start"); err != nil {
			lcAppend("[warn] redémarrage du moteur : " + err.Error())
		}
	}
	return nil
}

// handleEngineUse bascule vers une version DÉJÀ installée, ou revient au moteur
// de l'image ({tag:"image"}). Aucun réseau : c'est la sortie de secours quand une
// mise à jour s'avère décevante.
func handleEngineUse(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag string `json:"tag"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	tag := strings.TrimSpace(req.Tag)

	var bin string
	if tag == engineUseImage {
		if bin = providedEngineBin(); bin == "" {
			sendJSON(w, 400, map[string]any{"ok": false, "error": "cette installation n'a pas de moteur d'image (LOKI_ENGINE_BIN)"})
			return
		}
	} else if bin = engineBinFor(tag); bin == "" {
		sendJSON(w, 400, map[string]any{"ok": false, "error": "version non installée : " + tag})
		return
	}
	if err := SetConfigKey("BIN", bin); err != nil {
		sendJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if serviceIsActive() {
		_ = serviceAction("restart")
	}
	sendJSON(w, 200, map[string]any{"ok": true, "bin": bin})
}

// handleEngineRemove supprime une version téléchargée. On refuse de retirer
// celle qui tourne : ça laisserait une configuration pointant dans le vide, et
// le moteur ne redémarrerait plus.
func handleEngineRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag string `json:"tag"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	tag := filepath.Base(strings.TrimSpace(req.Tag))
	bin := engineBinFor(tag)
	if bin == "" {
		sendJSON(w, 400, map[string]any{"ok": false, "error": "version non installée : " + tag})
		return
	}
	if samePath(bin, currentEngineBin()) {
		sendJSON(w, 409, map[string]any{"ok": false, "error": "c'est le moteur utilisé — bascule d'abord sur une autre version"})
		return
	}
	if err := os.RemoveAll(filepath.Join(engineDir(), tag)); err != nil {
		sendJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sendJSON(w, 200, map[string]any{"ok": true})
}
