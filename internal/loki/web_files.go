package loki

// web_files.go — parcourir et supprimer les fichiers produits par l'agent.
//
// L'agent écrit dans son dossier de travail (chat_workspace.go) : rapports,
// scripts, exports, captures d'écran. Jusqu'ici on ne pouvait récupérer un
// fichier que si le modèle en avait mis le lien dans sa réponse — tout ce qu'il
// produisait sans le dire restait invisible, et rien ne permettait de faire le
// ménage sans passer par un shell.
//
// Le téléchargement existait déjà (handleChatFile, web_upload.go). Ce fichier
// ajoute les deux verbes qui manquaient : LISTER et SUPPRIMER.
//
// Le panneau s'ouvre sur le dossier de la DISCUSSION active (chat_convfiles.go)
// et n'en sort pas : les fichiers appartiennent à la discussion où ils sont nés,
// changer de discussion change ce qu'on voit. `scope=legacy` ouvre la racine du
// dossier de travail — les fichiers d'avant ce rangement, que la migration n'a
// pas su rattacher à une discussion.
//
// Tout est borné par relWithin (chat_convfiles.go), qui résout les liens
// symboliques des DEUX côtés avant de comparer : un lien posé dans le dossier
// de travail ne peut pas servir d'échappatoire vers le reste du disque.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// wsEntry — une ligne du panneau. Pour un dossier, Size est la somme récursive
// de son contenu : c'est ce qu'on libère en le supprimant, donc c'est ce chiffre
// qui aide à décider.
type wsEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"` // relatif à la racine du panneau, séparateurs /
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	Mod   int64  `json:"mod"`   // date de modification, epoch en secondes
	Image bool   `json:"image"` // affichable via /api/chat/image
	Items int    `json:"items,omitempty"`
}

// wsImageExt : extensions présentées comme images dans le panneau. Simple
// indice d'affichage — /api/chat/image renifle le CONTENU avant de servir quoi
// que ce soit, donc un .png qui contient du HTML est refusé là-bas, pas ici.
var wsImageExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".webp": true, ".bmp": true, ".svg": true, ".avif": true,
}

// wsWalkLimit borne le parcours récursif qui calcule la taille d'un dossier.
// Un workspace part de zéro et grossit lentement, mais rien n'empêche l'agent
// d'y dézipper 200 000 fichiers : la liste doit rester instantanée, quitte à
// annoncer une taille approchée (Items le dit).
const wsWalkLimit = 20000

// wsResolve traduit un chemin RELATIF venu du client en chemin absolu vérifié,
// sous la racine du panneau. La chaîne vide et "." désignent cette racine — cas
// que relWithin rejette (il renvoie false sur "."), d'où ce sas.
func wsResolve(root, rel string) (string, bool) {
	rel = strings.TrimSpace(rel)
	rel = strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "/")
	if rel == "" || rel == "." {
		return root, true
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if _, ok := relWithin(root, abs); !ok {
		return "", false
	}
	return abs, true
}

// dirSize additionne le contenu d'un dossier, en s'arrêtant à wsWalkLimit
// entrées. Renvoie aussi le nombre de fichiers rencontrés.
func dirSize(root string) (total int64, count int) {
	var walk func(string)
	walk = func(dir string) {
		if count >= wsWalkLimit {
			return
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			if count >= wsWalkLimit {
				return
			}
			// Un lien symbolique n'est pas suivi : sa cible ne nous appartient
			// pas, et une boucle de liens ferait tourner ce parcours sans fin.
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			if e.IsDir() {
				walk(filepath.Join(dir, e.Name()))
				continue
			}
			if info, err := e.Info(); err == nil {
				total += info.Size()
				count++
			}
		}
	}
	walk(root)
	return total, count
}

// handleChatFiles liste un dossier de la discussion active.
//
//	GET /api/chat/files?dir=<relatif>[&scope=legacy]
//	→ {ok, dir, parent, entries:[…], total, count, scope, legacy}
//
// `total` et `count` portent sur le dossier ENTIER de la discussion, pas sur le
// dossier affiché : c'est ce que cette discussion occupe sur le disque, donc ce
// qu'on libère en la supprimant.
//
// `legacy` compte ce qui reste à la racine du dossier de travail, hors
// discussions : l'UI n'offre le détour « hors discussion » que si ce compte est
// non nul, et le bouton disparaît une fois le ménage fait.
func handleChatFiles(w http.ResponseWriter, r *http.Request) {
	root, legacy := filesRoot(r.URL.Query().Get("scope"))
	abs, ok := wsResolve(root, r.URL.Query().Get("dir"))
	if !ok {
		sendJSON(w, 403, map[string]any{"ok": false, "error": "hors du dossier de travail"})
		return
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		sendJSON(w, 404, map[string]any{"ok": false, "error": "dossier introuvable"})
		return
	}
	ents, err := os.ReadDir(abs)
	if err != nil {
		sendJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	cur, _ := relWithin(root, abs) // "" à la racine
	entries := []wsEntry{}
	for _, e := range ents {
		name := e.Name()
		// Vue « hors discussion » : le dossier des discussions n'y a pas sa place,
		// il est déjà accessible discussion par discussion.
		if legacy && cur == "" && name == convFilesRoot {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		p := name
		if cur != "" {
			p = cur + "/" + name
		}
		it := wsEntry{Name: name, Path: p, Dir: e.IsDir(), Mod: info.ModTime().Unix()}
		if e.IsDir() {
			it.Size, it.Items = dirSize(filepath.Join(abs, name))
		} else {
			it.Size = info.Size()
			it.Image = wsImageExt[strings.ToLower(filepath.Ext(name))]
		}
		entries = append(entries, it)
	}
	// Dossiers d'abord, puis du plus récent au plus ancien : ce qu'on cherche
	// après un tour d'agent, c'est ce qu'il vient d'écrire.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return entries[i].Mod > entries[j].Mod
	})

	parent := ""
	if cur != "" {
		if i := strings.LastIndexByte(cur, '/'); i >= 0 {
			parent = cur[:i]
		}
	}
	total, count := dirSize(root)
	scope := ""
	if legacy {
		scope = "legacy"
	}
	sendJSON(w, 200, map[string]any{
		"ok": true, "dir": cur, "parent": parent, "at_root": cur == "",
		"entries": entries, "total": total, "count": count,
		"root": root, "scope": scope, "legacy": legacyCount(),
	})
}

// handleChatFileDelete supprime un fichier ou un dossier de la discussion.
//
//	POST /api/chat/file/delete {"path":"rapport.md"[,"scope":"legacy"]}
//	→ {ok, freed}
//
// La racine est refusée : « tout supprimer » n'est pas un geste qu'on veut à un
// clic de distance, et l'agent perdrait aussi le dossier dans lequel il écrit.
// Le dossier des discussions l'est aussi — depuis la vue « hors discussion », il
// emporterait les fichiers de TOUTES les discussions d'un seul clic.
func handleChatFileDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path  string `json:"path"`
		Scope string `json:"scope"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if strings.TrimSpace(req.Path) == "" {
		sendJSON(w, 400, map[string]any{"ok": false, "error": "chemin manquant"})
		return
	}
	root, legacy := filesRoot(req.Scope)
	abs, ok := wsResolve(root, req.Path)
	if !ok {
		sendJSON(w, 403, map[string]any{"ok": false, "error": "hors du dossier de travail"})
		return
	}
	if abs == root {
		sendJSON(w, 403, map[string]any{"ok": false, "error": "le dossier de travail lui-même ne se supprime pas"})
		return
	}
	if legacy && abs == filepath.Join(agentWorkspace(), convFilesRoot) {
		sendJSON(w, 403, map[string]any{"ok": false, "error": "les dossiers des discussions se suppriment discussion par discussion"})
		return
	}
	st, err := os.Lstat(abs)
	if err != nil {
		sendJSON(w, 404, map[string]any{"ok": false, "error": "introuvable"})
		return
	}
	var freed int64
	if st.IsDir() {
		freed, _ = dirSize(abs)
		err = os.RemoveAll(abs)
	} else {
		freed = st.Size()
		err = os.Remove(abs)
	}
	if err != nil {
		sendJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sendJSON(w, 200, map[string]any{"ok": true, "freed": freed})
}
