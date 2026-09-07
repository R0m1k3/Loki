// Réglages de la dictée vocale — modèle et réactivité.
//
// Ils vivent dans le magasin clé/valeur (bkState), comme le jeton Hugging
// Face : ce sont des réglages de SERVEUR, pas de navigateur. Le modèle chargé
// est une propriété de la machine, identique pour tous les onglets ouverts.
package loki

import (
	"fmt"
	"time"
)

const dictateCfgKey = "dictate_cfg"

// DictateCfg : ce qui reste réglable depuis le passage à Parakeet.
//
// Deux champs ont disparu, et c'est le moteur qui l'impose, pas un choix de
// simplification :
//   - la LANGUE : Parakeet v3 détecte la langue lui-même et n'a aucun drapeau
//     pour la forcer. Le réglage n'aurait servi qu'à mentir.
//   - le GPU : le binaire sherpa-onnx livré dans l'image est le build statique
//     CPU. Annoncer un sélecteur de carte sans pouvoir l'honorer serait pire que
//     de ne rien annoncer. Un modèle 0,6 B en int8 sur des tranches de quelques
//     secondes n'a pas besoin de la carte, qui reste au moteur de chat.
type DictateCfg struct {
	Model      string `json:"model"`
	Reactivity string `json:"reactivity"`
}

// withDefauts comble les champs vides. Appliqué à la LECTURE plutôt qu'à
// l'écriture : une version future qui ajoute un champ trouvera les réglages
// déjà enregistrés sans champ correspondant, et doit pouvoir le combler. C'est
// aussi ce qui rattrape les réglages enregistrés du temps de whisper, dont le
// Model ne veut plus rien dire.
func (c DictateCfg) withDefauts() DictateCfg {
	if _, connu := asrCatalogue[c.Model]; !connu {
		// v3 : le seul des deux qui parle français.
		c.Model = "parakeet-tdt-0.6b-v3"
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
	// Valider ICI plutôt qu'au démarrage du serveur : un modèle hors catalogue
	// accepté en silence ne se manifesterait qu'au premier clic sur le micro,
	// loin du geste qui l'a causé. On valide AVANT withDefauts, qui corrigerait
	// justement la faute qu'on veut signaler.
	if _, ok := asrCatalogue[c.Model]; !ok {
		return fmt.Errorf("modèle de dictée inconnu : %s", c.Model)
	}
	return putJSON(bkState, dictateCfgKey, c.withDefauts())
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
