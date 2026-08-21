// Réglages de la dictée vocale — modèle, langue, matériel, réactivité.
//
// Ils vivent dans le magasin clé/valeur (bkState), comme le jeton Hugging
// Face : ce sont des réglages de SERVEUR, pas de navigateur. Le modèle chargé
// et le GPU occupé sont des propriétés de la machine, identiques pour tous les
// onglets ouverts.
package loki

import (
	"fmt"
	"strconv"
	"time"
)

const dictateCfgKey = "dictate_cfg"

// DictateCfg : Device vaut "cpu" ou un indice de GPU en décimal ("0", "1"),
// dans l'ordre de nvidia-smi — le même que celui affiché par /api/vram.
type DictateCfg struct {
	Model      string `json:"model"`
	Lang       string `json:"lang"`
	Device     string `json:"device"`
	Reactivity string `json:"reactivity"`
}

// withDefauts comble les champs vides. Appliqué à la LECTURE plutôt qu'à
// l'écriture : une version future qui ajoute un champ trouvera les réglages
// déjà enregistrés sans champ correspondant, et doit pouvoir le combler.
func (c DictateCfg) withDefauts() DictateCfg {
	if c.Model == "" {
		// large-v3-turbo : meilleur rapport qualité/vitesse en français à ce
		// jour. Le défaut penche vers la qualité.
		c.Model = "large-v3-turbo-q5_0"
	}
	if c.Lang == "" {
		// Pas "auto" : sur des tranches de quelques secondes la détection se
		// trompe, et une langue mal détectée produit du charabia — des suites
		// de caractères géorgiens ont été observées en production.
		c.Lang = "fr"
	}
	if c.Device == "" {
		c.Device = "cpu"
	}
	if c.Reactivity == "" {
		c.Reactivity = "moyen"
	}
	return c
}

func dictateCfgLoad() DictateCfg {
	var c DictateCfg
	getJSON(bkState, dictateCfgKey, &c)
	return c.withDefauts()
}

func dictateCfgSave(c DictateCfg) error {
	c = c.withDefauts()
	// Valider ICI plutôt qu'au démarrage du serveur : un modèle hors catalogue
	// accepté en silence ne se manifesterait qu'au premier clic sur le micro,
	// loin du geste qui l'a causé.
	if _, ok := whisperCatalogue[c.Model]; !ok {
		return fmt.Errorf("modèle de dictée inconnu : %s", c.Model)
	}
	return putJSON(bkState, dictateCfgKey, c)
}

// cudaVisibleDevices traduit le réglage en valeur de CUDA_VISIBLE_DEVICES.
// Vide = aucun GPU visible = whisper tourne sur le CPU. Toute valeur qui n'est
// pas un indice positif retombe sur le CPU : mieux vaut une dictée lente qu'un
// GPU choisi au hasard sur une machine dont on ne sait rien.
func (c DictateCfg) cudaVisibleDevices() string {
	n, err := strconv.Atoi(c.Device)
	if err != nil || n < 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// chunkBounds : réserve minimale avant de couper, et coupure forcée quand
// aucun silence n'arrive. Consommé par le découpeur du plan « temps réel ».
func (c DictateCfg) chunkBounds() (time.Duration, time.Duration) {
	switch c.Reactivity {
	case "court":
		return 1000 * time.Millisecond, 5 * time.Second
	case "long":
		return 3 * time.Second, 15 * time.Second
	default:
		return 1500 * time.Millisecond, 8 * time.Second
	}
}
