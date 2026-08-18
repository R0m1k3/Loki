package loki

// code_policy.go — politique d'exécution des outils de l'agent (repris dans
// l'esprit de tool-policy/path-security d'OpenFox, réécrit en Go — voir
// NOTICE.md).
//
// Deux gardes indépendantes :
//   - dangerousCommand : liste courte de commandes CATASTROPHIQUES (effacement
//     de disque, arrêt machine, fork bomb). Toujours active, mode chat compris :
//     aucune de ces commandes n'a d'usage légitime lancée par un modèle. Le
//     refus explique quoi faire (l'utilisateur la lance lui-même).
//   - codePathAllowed : en MODE CODE, tous les chemins des outils fichiers sont
//     bornés au dossier de la discussion — liens symboliques résolus des deux
//     côtés, comme le panneau Fichiers (relWithin). Le mode chat garde le
//     comportement historique (chemins absolus honorés) : c'est le mode code
//     qui promet un bac à sable, pas l'agent généraliste.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// dangerousPatterns : motifs (insensibles à la casse) de commandes refusées.
// Volontairement ÉTROIT : viser la perte de données irréversible et l'arrêt de
// la machine, pas la moindre écriture. Un motif trop large rend l'agent
// inutilisable et pousse à tout désactiver.
var dangerousPatterns = []struct {
	re     *regexp.Regexp
	reason string
}{
	// rm -rf / (ou ~, ou une racine de lecteur) — pas un rm -rf d'un sous-dossier.
	{regexp.MustCompile(`(?i)\brm\s+(-[a-z]*r[a-z]*f[a-z]*|-[a-z]*f[a-z]*r[a-z]*)\s+("?/"?|~/?|/\*|[a-z]:\\?)\s*$`), "efface la racine du disque ou le home"},
	{regexp.MustCompile(`(?i)\brm\s+(-[a-z]*r[a-z]*f[a-z]*|-[a-z]*f[a-z]*r[a-z]*)\s+("?/"?|~/?|/\*|[a-z]:\\?)\s`), "efface la racine du disque ou le home"},
	// Écriture directe sur un périphérique bloc / formatage.
	{regexp.MustCompile(`(?i)\bmkfs(\.[a-z0-9]+)?\b`), "formate un système de fichiers"},
	{regexp.MustCompile(`(?i)\bdd\b[^\n]*\bof=/dev/`), "écrit directement sur un périphérique disque"},
	{regexp.MustCompile(`(?i)>\s*/dev/sd[a-z]\b`), "écrit directement sur un périphérique disque"},
	// Arrêt / redémarrage de la machine (ici : le conteneur, donc tout Loki).
	// Ancré en position de COMMANDE (début, après ; && | ou sudo) : le mot
	// « reboot » dans un grep ou un texte n'est pas une commande.
	{regexp.MustCompile(`(?i)(^|[;&|]\s*|\bsudo\s+)(shutdown|poweroff|reboot|halt)\b`), "arrête ou redémarre la machine"},
	// Fork bomb classique.
	{regexp.MustCompile(`:\(\)\s*\{\s*:\|:`), "fork bomb"},
	// chmod/chown récursif sur la racine.
	{regexp.MustCompile(`(?i)\bch(mod|own)\b[^\n]*-[a-z]*R[a-z]*\s+[^\s]+\s+/\s*$`), "change récursivement les droits de la racine"},
	// Windows : suppression récursive de la racine d'un lecteur, format.
	{regexp.MustCompile(`(?i)\b(rd|rmdir)\s+/s\b[^\n]*\s[a-z]:\\?\s*$`), "efface la racine d'un lecteur"},
	{regexp.MustCompile(`(?i)\bdel\s+(/[fsq]\s+)*[a-z]:\\\*`), "efface la racine d'un lecteur"},
	{regexp.MustCompile(`(?i)\bformat\s+[a-z]:`), "formate un lecteur"},
}

// dangerousCommand renvoie la raison du refus si la commande correspond à un
// motif interdit, "" sinon.
func dangerousCommand(cmd string) string {
	for _, p := range dangerousPatterns {
		if p.re.MatchString(cmd) {
			return p.reason
		}
	}
	return ""
}

// refusedCommandResult formate le refus renvoyé au modèle : dire POURQUOI et
// QUOI FAIRE, sinon il réessaie en boucle avec des variantes.
func refusedCommandResult(reason string) string {
	return "[refusé] Commande bloquée par la politique de sécurité : " + reason + ". " +
		"Ne la réessaie pas, même reformulée. Si l'utilisateur la veut vraiment, dis-lui de la lancer lui-même dans un terminal."
}

// codePathAllowed dit si un chemin (déjà résolu par resolveAgentPath) est DANS
// le dossier de la discussion courante. Utilisé par les outils fichiers en mode
// code uniquement.
func codePathAllowed(abs string) bool {
	ws := convWorkspace()
	if ws == "" {
		return false
	}
	abs = filepath.Clean(abs)
	// Le fichier peut ne pas exister encore (write) : EvalSymlinks échouerait.
	// On teste alors son ancêtre le plus proche existant — c'est LUI qui doit
	// être dans le workspace, liens symboliques résolus.
	probe := abs
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	if !sameOrWithin(ws, probe) {
		return false
	}
	// L'ancêtre est dans le workspace ; le chemin FINAL ne doit pas en
	// ressortir par des ".." lexicaux non encore résolus.
	rel, err := filepath.Rel(ws, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// sameOrWithin : p est root lui-même, ou dedans — liens symboliques résolus
// des deux côtés (relWithin refuse « . », d'où ce cousin qui l'accepte : la
// racine du workspace est un chemin légitime pour grep/glob).
func sameOrWithin(root, p string) bool {
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	if q, err := filepath.EvalSymlinks(p); err == nil {
		p = q
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// refusedPathResult : refus de chemin hors du bac à sable du mode code.
func refusedPathResult(path string) string {
	return "[refusé] " + path + " est hors du dossier de cette discussion. " +
		"En mode code, tous les fichiers vivent dans le dossier de travail — utilise des chemins relatifs."
}

// codeWriteGuard regroupe les gardes du mode code AVANT une écriture locale
// (write/edit) : borne de chemin, puis tracker « lu avant d'écrire ».
// Renvoie "" si tout est bon, sinon le message de refus pour le modèle.
// En mode chat, aucune garde : comportement historique de l'agent.
func codeWriteGuard(caps Caps, file string, forWrite bool) string {
	if !caps.Code {
		return ""
	}
	path := resolveAgentPath(file)
	if !codePathAllowed(path) {
		return refusedPathResult(file)
	}
	return trackerCheck(path, forWrite)
}

// --- Mutex par fichier -------------------------------------------------------
// Sérialise les écritures concurrentes sur un même fichier (write/edit). Sans
// parallélisme d'outils aujourd'hui le coût est nul, mais le jour où deux tours
// ou un job d'arrière-plan écrivent le même fichier, la corruption est évitée.

var fileMuMap sync.Map // clé : chemin nettoyé → *sync.Mutex

func fileMu(path string) *sync.Mutex {
	key := filepath.Clean(path)
	if m, ok := fileMuMap.Load(key); ok {
		return m.(*sync.Mutex)
	}
	m, _ := fileMuMap.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}
