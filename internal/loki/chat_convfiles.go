package loki

// chat_convfiles.go — les fichiers rangés PAR DISCUSSION.
//
// Avant, tout ce qui passait par le chat — pièces jointes déposées, rapports
// écrits par l'agent, captures de pages — atterrissait dans UN seul dossier de
// travail partagé par toutes les discussions. Changer de discussion ne changeait
// rien au panneau Fichiers (on revoyait les mêmes), et supprimer une discussion
// laissait ses fichiers derrière elle — seules ses captures, déjà rangées par
// identifiant, partaient avec.
//
// Désormais chaque discussion a son dossier :
//
//	<workspace>/discussions/<id>/           ← dossier courant de l'agent
//	<workspace>/discussions/<id>/uploads/   ← pièces jointes de CETTE discussion
//	<workspace>/discussions/<id>/captures/  ← captures de CETTE discussion
//
// Supprimer une discussion (ou la vider) emporte tout le dossier, et le panneau
// Fichiers n'ouvre que celui de la discussion active.
//
// La racine du dossier de travail reste la BORNE de sécurité : les liens écrits
// dans les anciens messages (`uploads/x.pdf`, `captures/<id>/y.jpg`) pointent
// encore vers elle et doivent continuer de marcher — c'est le rôle de
// workspaceFile, qui cherche d'abord dans la discussion puis retombe sur les
// emplacements d'avant.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
)

const (
	// convFilesRoot : le sous-dossier qui contient un dossier par discussion.
	// Nommé, plutôt que d'éparpiller les identifiants à la racine, pour que le
	// dossier de travail reste lisible quand on l'ouvre depuis l'hôte.
	convFilesRoot = "discussions"

	// uploadsSub : les dépôts de l'utilisateur, à l'intérieur de la discussion.
	uploadsSub = "uploads"
)

// safeConvID refuse tout identifiant qui ne soit pas un simple nom : c'est lui
// qui devient un segment de chemin, et il arrive parfois du client (paramètre de
// requête, ancien lien de message). Renvoie "" si l'identifiant est douteux.
func safeConvID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 64 {
		return ""
	}
	for _, r := range id {
		if r == '-' || r == '_' || unicode.IsDigit(r) || (r < unicode.MaxASCII && unicode.IsLetter(r)) {
			continue
		}
		return ""
	}
	return id
}

// convDirFor renvoie le dossier (absolu) d'une discussion, sans le créer. Un
// identifiant vide ou douteux retombe sur la racine du dossier de travail :
// mieux vaut écrire au mauvais endroit que refuser d'écrire.
func convDirFor(convID string) string {
	id := safeConvID(convID)
	if id == "" {
		return agentWorkspace()
	}
	return filepath.Join(agentWorkspace(), convFilesRoot, id)
}

// convWorkspace est le dossier de travail COURANT : celui de la discussion
// active, créé au besoin. C'est là que résolvent les chemins relatifs du modèle
// (resolveAgentPath), que démarre son shell, et que se déposent les fichiers.
//
// Si la création échoue (disque plein, droits), on retombe sur la racine du
// dossier de travail : l'agent doit continuer d'écrire quelque part.
func convWorkspace() string {
	dir := convDirFor(convEnsureActive())
	if dir == agentWorkspace() {
		return dir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return agentWorkspace()
	}
	return dir
}

// dropConvFiles efface TOUT le dossier d'une discussion (dépôts, captures,
// fichiers écrits par l'agent). Appelé quand la discussion est supprimée ou
// vidée : plus un message ne les mentionne, personne ne les retrouverait.
// Best-effort — un échec ne doit rien interrompre.
func dropConvFiles(convID string) {
	dir := convDirFor(convID)
	if dir == agentWorkspace() {
		return // identifiant douteux : surtout pas la racine
	}
	_ = os.RemoveAll(dir)
}

// relWithin dit si `abs` se trouve DANS `root` et, si oui, renvoie son chemin
// relatif en séparateurs '/'.
//
// EvalSymlinks des deux côtés : sans ça, un lien posé dans le dossier passerait
// le test de préfixe tout en pointant ailleurs sur le disque.
func relWithin(root, abs string) (string, bool) {
	if root == "" {
		return "", false
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	if a, err := filepath.EvalSymlinks(abs); err == nil {
		abs = a
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// workspaceFile résout un chemin RELATIF venu d'un client ou d'un message
// (`uploads/rapport.pdf`, `captures/xxx.jpg`) en fichier du disque.
//
// Ordre de recherche :
//  1. le dossier de la discussion active — le cas normal ;
//  2. les emplacements d'AVANT le rangement par discussion, pour que les liens
//     des anciens messages continuent d'ouvrir leur fichier.
//
// ok=false signale une sortie du périmètre (« .. »), pas une absence : un
// fichier introuvable revient avec le chemin attendu, pour que l'appelant
// réponde « introuvable » et non « hors du dossier de travail ».
func workspaceFile(rel string) (string, bool) {
	rel = strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(rel), "\\", "/"), "/")
	if rel == "" {
		return "", false
	}
	base := convWorkspace()
	p := filepath.Join(base, filepath.FromSlash(rel))
	if _, ok := relWithin(base, p); !ok {
		return "", false
	}
	if _, err := os.Stat(p); err == nil {
		return p, true
	}
	if legacy, ok := legacyWorkspaceFile(rel); ok {
		return legacy, true
	}
	return p, true
}

// legacyWorkspaceFile retrouve un fichier désigné par un chemin d'avant le
// rangement par discussion. Deux formes existaient :
//
//	captures/<id>/x.jpg → discussions/<id>/captures/x.jpg (déplacé par la migration)
//	uploads/x.pdf       → resté à la racine si aucune discussion ne le réclamait
func legacyWorkspaceFile(rel string) (string, bool) {
	if parts := strings.SplitN(rel, "/", 3); len(parts) == 3 && parts[0] == captureDir && safeConvID(parts[1]) != "" {
		p := filepath.Join(convDirFor(parts[1]), captureDir, filepath.FromSlash(parts[2]))
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
	}
	p := filepath.Join(agentWorkspace(), filepath.FromSlash(rel))
	if _, ok := workspaceRel(p); !ok {
		return "", false
	}
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p, true
	}
	return "", false
}

// filesRoot donne la racine du panneau Fichiers pour un `scope` de requête :
// la discussion active par défaut, la racine du dossier de travail pour
// `scope=legacy` — celle où dorment les fichiers d'avant le rangement par
// discussion, que la migration n'a pas su rattacher.
func filesRoot(scope string) (root string, legacy bool) {
	if strings.TrimSpace(scope) == "legacy" {
		return agentWorkspace(), true
	}
	return convWorkspace(), false
}

// legacyCount compte ce qui traîne à la racine du dossier de travail, hors
// dossier des discussions. L'UI n'affiche le raccourci « hors discussion » que
// si ce compte est non nul — il tombe à zéro une fois le ménage fait, et le
// bouton disparaît de lui-même.
func legacyCount() int {
	ents, err := os.ReadDir(agentWorkspace())
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		if e.Name() == convFilesRoot || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		n++
	}
	return n
}

var convFilesMigrateOnce sync.Once

// migrateConvFiles range les fichiers des versions précédentes dans le dossier
// de leur discussion. Une seule fois par process, au démarrage du serveur web.
func migrateConvFiles() { convFilesMigrateOnce.Do(migrateConvFilesNow) }

func migrateConvFilesNow() {
	root := agentWorkspace()
	if root == "" {
		return
	}
	// 1. Les captures étaient DÉJÀ par discussion : captures/<id> → discussions/<id>/captures.
	//    Un simple renommage, donc instantané quel que soit le nombre d'images.
	oldCaps := filepath.Join(root, captureDir)
	if ents, err := os.ReadDir(oldCaps); err == nil {
		for _, e := range ents {
			id := safeConvID(e.Name())
			if !e.IsDir() || id == "" {
				continue
			}
			dst := filepath.Join(convDirFor(id), captureDir)
			if _, err := os.Stat(dst); err == nil {
				continue // déjà rangé (migration interrompue puis reprise)
			}
			if os.MkdirAll(filepath.Dir(dst), 0o755) != nil {
				continue
			}
			_ = os.Rename(filepath.Join(oldCaps, e.Name()), dst)
		}
		_ = os.Remove(oldCaps) // ne part que s'il est vide
	}
	// 2. Les dépôts, eux, étaient dans un pot commun. On rend chaque fichier à la
	//    discussion qui le mentionne — l'historique garde le nom des pièces
	//    jointes de chaque tour. Ce qui n'est réclamé par personne reste à la
	//    racine : toujours téléchargeable (legacyWorkspaceFile) et visible via
	//    « hors discussion », plutôt que rattaché au hasard.
	migrateLegacyUploads(filepath.Join(root, uploadsSub))
}

func migrateLegacyUploads(src string) {
	ents, err := os.ReadDir(src)
	if err != nil {
		return
	}
	left := map[string]bool{}
	for _, e := range ents {
		if !e.IsDir() {
			left[e.Name()] = true
		}
	}
	for _, m := range convIndex() {
		if len(left) == 0 {
			break
		}
		for _, name := range convAttachNames(m.ID) {
			if !left[name] {
				continue
			}
			dst := filepath.Join(convDirFor(m.ID), uploadsSub)
			if os.MkdirAll(dst, 0o755) != nil {
				continue
			}
			if os.Rename(filepath.Join(src, name), filepath.Join(dst, name)) == nil {
				delete(left, name)
			}
		}
	}
	_ = os.Remove(src) // ne part que s'il est vide
}

// convAttachNames relit les pièces jointes annoncées dans le journal d'affichage
// d'une discussion (delta `files`, cf. StartTurn). C'est la seule trace qui
// relie un fichier déposé à sa discussion.
func convAttachNames(id string) []string {
	b := getBytes(bkChat, convKey(id))
	if len(b) == 0 {
		return nil
	}
	var st struct {
		Log []struct {
			Delta struct {
				Files []attachInfo `json:"files"`
			} `json:"delta"`
		} `json:"log"`
	}
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	var out []string
	for _, ev := range st.Log {
		for _, f := range ev.Delta.Files {
			if n := safeUploadName(f.Name); n != "" {
				out = append(out, n)
			}
		}
	}
	return out
}
