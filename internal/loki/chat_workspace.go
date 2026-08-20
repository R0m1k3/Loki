package loki

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// Dossier de travail du mode agent.
//
// Sans ça, les outils write/edit/bash héritent du répertoire courant du PROCESSUS :
// le modèle écrit "meteo.json", et le fichier atterrit là d'où l'utilisateur a
// lancé loki — le Bureau quand on double-clique l'installateur, C:\ProgramData\loki\bin
// quand on lance le binaire installé. Personne ne s'attend à voir un chat déposer
// des fichiers sur son Bureau. On donne donc à l'agent un dossier à lui : les
// chemins relatifs y sont résolus, et le shell y démarre. Les chemins ABSOLUS
// restent honorés tels quels — quand l'utilisateur demande d'écrire dans un
// dossier précis, ça doit marcher.
//
// agentWorkspace est la RACINE, commune à tout : le dossier de travail effectif
// est celui de la discussion ouverte (convWorkspace, chat_convfiles.go), pour
// qu'un fichier appartienne à la discussion où il est né et disparaisse avec
// elle — ou celui de la tâche planifiée en cours (agentCwd, plus bas). La racine
// reste la borne de sécurité des téléchargements.

const workspaceEnv = "LOKI_WORKSPACE"

var (
	workspaceOnce sync.Once
	workspacePath string
)

// agentWorkspace renvoie le dossier de travail de l'agent, créé au besoin. Il
// essaie plusieurs emplacements car LokiHome() vaut %ProgramData%\loki sous
// Windows, qui n'est pas inscriptible par un utilisateur non administrateur.
func agentWorkspace() string {
	workspaceOnce.Do(func() {
		for _, dir := range workspaceCandidates() {
			if dir == "" {
				continue
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				continue
			}
			// MkdirAll réussit sur un dossier existant même non inscriptible :
			// on vérifie l'écriture réelle.
			f, err := os.CreateTemp(dir, ".probe-*")
			if err != nil {
				continue
			}
			name := f.Name()
			f.Close()
			os.Remove(name)
			workspacePath = dir
			return
		}
		// Dernier recours : le répertoire courant, comportement historique.
		workspacePath, _ = os.Getwd()
	})
	return workspacePath
}

func workspaceCandidates() []string {
	var c []string
	if v := strings.TrimSpace(os.Getenv(workspaceEnv)); v != "" {
		c = append(c, v)
	}
	c = append(c, workspaceDir())
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		c = append(c, filepath.Join(home, "loki", "workspace"))
	}
	c = append(c, filepath.Join(os.TempDir(), "loki-workspace"))
	return c
}

// Dossier de travail imposé pendant l'exécution d'une TÂCHE planifiée
// (tasks_run.go). Une tâche tourne hors de toute discussion : sans ça, ses
// fichiers atterriraient dans le dossier de la discussion qui se trouve ouverte
// — et changeraient de place en cours de route si l'utilisateur en changeait.
//
// La bascule est un état de processus et non de goroutine, ce que Go ne sait pas
// faire autrement ; c'est sûr ici parce qu'une SEULE génération tourne à la fois
// (le verrou de conv), et parce que seuls les points d'entrée des OUTILS lisent
// agentCwd(). Ce que voit l'utilisateur — panneau Fichiers, dépôts, liens des
// messages — continue de passer par convWorkspace() et reste donc sur sa
// discussion, même pendant qu'une tâche écrit ailleurs.
var taskWS atomic.Value // string

func setTaskWorkspace(dir string) { taskWS.Store(dir) }

// agentCwd = le dossier où l'agent travaille MAINTENANT : celui de la tâche en
// cours s'il y en a une, sinon celui de la discussion ouverte.
func agentCwd() string {
	if d, _ := taskWS.Load().(string); d != "" {
		return d
	}
	return convWorkspace()
}

// resolveAgentPath résout un chemin fourni par le modèle. Absolu → inchangé ;
// "~/x" → dans le home de l'utilisateur ; relatif → dans le dossier de la
// discussion ouverte.
func resolveAgentPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return p
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, filepath.FromSlash(strings.TrimPrefix(p[1:], "/")))
		}
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(agentCwd(), filepath.FromSlash(p))
}
