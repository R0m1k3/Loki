package loki

// sys_datavolume.go — garde-fou contre la PERTE DE DONNÉES en conteneur.
//
// Le symptôme vécu : modèles, discussions et fichiers disparus après un
// redémarrage du serveur. La cause, dans tous les cas observés : /data n'est
// PAS un volume monté (template Docker sans mapping, `docker run` sans -v,
// chemin hôte qui change d'un lancement à l'autre). Tout vit alors dans la
// couche d'écriture du conteneur, qui meurt avec lui à la première recréation
// (mise à jour d'image, compose down/up, redémarrage de l'array Unraid).
//
// Loki ne peut pas monter le volume à la place de l'utilisateur, mais il peut
// le VOIR et le DIRE : au démarrage en conteneur, si LOKI_HOME n'est pas un
// point de montage, on le journalise en rouge et l'UI l'affiche en bandeau.

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// dataDirIsMounted dit si LokiHome() est un point de montage (bind mount ou
// volume Docker). Lecture de /proc/self/mountinfo — Linux seulement, or c'est
// le seul endroit où Loki tourne en conteneur. Hors conteneur : sans objet.
//
// Renvoie (monté, connu) : connu=false quand on n'a pas pu lire mountinfo —
// on ne crie pas au loup sur une simple erreur de lecture.
func dataDirIsMounted() (mounted, known bool) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, false
	}
	defer f.Close()
	target := filepath.Clean(LokiHome())
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// Format mountinfo : champs séparés par des espaces, le 5ᵉ (index 4)
		// est le point de montage. Les espaces dans les chemins sont encodés
		// \040 — décodés avant comparaison.
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		mp := strings.ReplaceAll(fields[4], `\040`, " ")
		if filepath.Clean(mp) == target {
			return true, true
		}
	}
	return false, sc.Err() == nil
}

// dataVolumeWarning renvoie le message d'alerte si les données sont en périple
// ("" si tout va bien). Évalué au démarrage du serveur web et servi par
// /api/status pour le bandeau de l'UI.
func dataVolumeWarning() string {
	if os.Getenv("LOKI_CONTAINER") == "" {
		return "" // hors conteneur : l'utilisateur gère ses chemins
	}
	mounted, known := dataDirIsMounted()
	if !known || mounted {
		return ""
	}
	return "⚠️ " + LokiHome() + " n'est PAS un volume monté : modèles, discussions et fichiers seront PERDUS à la recréation du conteneur (mise à jour, redémarrage du serveur). " +
		"Ajoute un mapping de volume — compose : « - /mnt/user/appdata/loki/data:/data » (et « …/models:/models ») ; " +
		"docker run : « -v /chemin/hote/data:/data ». Voir docker-compose.unraid.yml du dépôt."
}
