package loki

// code_tracker.go — suivi « lu avant d'écrire » (file-tracker d'OpenFox, réécrit
// en Go — voir NOTICE.md). Le modèle doit avoir LU un fichier (outil read) avant
// de le modifier (edit) ou de l'écraser (write sur un fichier existant), et sa
// lecture doit être plus récente que la dernière modification du fichier sur le
// disque. Ça bloque les deux corruptions silencieuses classiques :
//   - réécrire un fichier jamais ouvert (le modèle « devine » son contenu) ;
//   - éditer d'après une lecture périmée (le fichier a changé entre-temps —
//     autre outil, job d'arrière-plan, utilisateur).
//
// L'état est en mémoire, par discussion : un redémarrage oublie tout, et le
// modèle relit — c'est le comportement voulu, pas un bug.

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	trackerMu    sync.Mutex
	trackerReads = map[string]time.Time{} // conv + chemin → instant de la lecture
)

func trackerKey(path string) string {
	return convEnsureActive() + "\x00" + filepath.Clean(path)
}

// trackerNoteRead enregistre qu'un fichier vient d'être lu (outil read).
func trackerNoteRead(path string) {
	trackerMu.Lock()
	trackerReads[trackerKey(path)] = time.Now()
	trackerMu.Unlock()
}

// trackerNoteWrite : après une écriture réussie, la version sur disque est celle
// que le modèle vient de produire — il la « connaît ». Sans cette note, éditer
// un fichier qu'on vient soi-même d'écrire serait refusé.
func trackerNoteWrite(path string) {
	trackerNoteRead(path)
}

// trackerCheck vérifie qu'une modification est sûre. forWrite distingue write
// (autorisé sur un fichier INEXISTANT sans lecture préalable — création) de
// edit (le fichier doit exister ET avoir été lu).
// Renvoie "" si c'est bon, sinon le message de refus pour le modèle.
func trackerCheck(path string, forWrite bool) string {
	fi, statErr := os.Stat(path)
	if statErr != nil {
		if forWrite {
			return "" // création d'un fichier neuf : rien à relire
		}
		return "" // edit sur fichier absent : fileEdit renverra sa propre erreur claire
	}
	trackerMu.Lock()
	readAt, seen := trackerReads[trackerKey(path)]
	trackerMu.Unlock()
	if !seen {
		return "[refusé] " + path + " existe mais tu ne l'as pas lu dans cette discussion. Lis-le d'abord (outil read), puis modifie-le."
	}
	if fi.ModTime().After(readAt) {
		return "[refusé] " + path + " a changé sur le disque depuis ta lecture. Relis-le (outil read) avant de le modifier."
	}
	return ""
}
