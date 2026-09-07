package loki

// mem_index.go — maintien AUTOMATIQUE de l'index MEMORY.md du projet, par le CODE
// (pas par le modèle). À chaque création / suppression / renommage d'une page, on
// ajoute ou retire sa ligne `- [titre](fichier.md)` dans MEMORY.md. L'index
// reflète donc TOUJOURS l'état réel des pages, quel que soit le modèle — un petit
// modèle qui « oublie » de tenir son index ne peut plus le désynchroniser. Le
// modèle garde une seule liberté : enrichir l'accroche après le titre, qu'on ne
// touche jamais.
//
// L'index est ensuite INJECTÉ comme un message système au début de la
// conversation (et après un compactage) plutôt que recopié dans le prompt à
// chaque tour : le modèle sait d'emblée quelles pages existent, sans payer la
// liste à chaque échange ni avoir à lancer une recherche pour la découvrir.
//
// Best-effort partout : si l'index est illisible, on n'échoue pas — il sera
// re-synchronisé au prochain passage (reconcileMemIndex).

import (
	"os"
	"path/filepath"
	"strings"
)

const memIndexFile = "MEMORY.md"

// memIndexPrefix marque le message d'index injecté dans l'historique, pour le
// détecter et éviter les doublons.
const memIndexPrefix = "Project memory index"

// memIndexMessage construit le message système d'index à injecter UNE FOIS au
// début de la conversation (et après un compactage). ok=false hors mode mémoire
// proactif ou si l'index est vide. On n'injecte que l'INDEX (titres + accroches),
// jamais le contenu des pages — l'IA fait mem_read pour lire une page.
func memIndexMessage() (Message, bool) {
	if memMode() != MemAlways {
		return Message{}, false
	}
	idx := strings.TrimSpace(MemContent(memIndexFile))
	// Un index qui n'a que son en-tête ne dit rien au modèle et coûte du contexte
	// à chaque tour : on attend au moins une VRAIE ligne de page. Le test est fait
	// sur le début de ligne, parce que l'en-tête cite le format `- [titre](…)` en
	// exemple — une simple recherche du motif le prendrait pour une page.
	if idx == "" || !hasIndexedPage(idx) {
		return Message{}, false
	}
	content := memIndexPrefix + " for project \"" + projectName(activeProjectSlug()) +
		"\" — the pages you can open with mem_read (only titles/hooks here, not their content). Auto-maintained.\n\n" + idx
	return Message{Role: "system", Content: content}, true
}

// projectContextPrefix marque le message de contexte projet injecté (description),
// pour le détecter et éviter les doublons.
const projectContextPrefix = "Project context"

// projectContextMessage construit le message système décrivant le projet actif à
// partir de sa description, à injecter UNE FOIS au début de la conversation (et
// après un compactage). ok=false si aucune description : l'IA sait alors juste
// dans quel projet elle est via l'index mémoire, sans laïus. Indépendant du mode
// mémoire — une description reste utile même mémoire coupée.
func projectContextMessage() (Message, bool) {
	slug := activeProjectSlug()
	desc := strings.TrimSpace(projectDesc(slug))
	if desc == "" {
		return Message{}, false
	}
	content := projectContextPrefix + " — you are working within the project \"" +
		projectName(slug) + "\". What this project is about (set by the user):\n\n" + desc
	return Message{Role: "system", Content: content}, true
}

// projectSystemMessages renvoie, dans l'ordre, tout ce qui situe l'IA dans son
// projet : description du projet, index mémoire, index des trackers.
//
// Ces messages sont injectés dans la vue ENVOYÉE au modèle, jamais persistés dans
// c.Messages — même traitement que le prompt système personnalisé (voir
// Conversation.generate). Deux conséquences voulues :
//   - l'index est TOUJOURS à jour : une page créée au milieu d'une conversation y
//     apparaît au tour suivant, sans attendre un compactage ;
//   - le compactage ne peut pas les avaler, puisqu'ils ne font pas partie de
//     l'historique qu'il résume — donc rien à réinjecter après coup.
//
// Le prix est une invalidation du cache de prompt quand l'index change, c'est-à-
// dire quand une page est créée ou supprimée : exactement les moments où le
// modèle DOIT voir la nouvelle liste.
func projectSystemMessages() []Message {
	var out []Message
	if m, ok := projectContextMessage(); ok {
		out = append(out, m)
	}
	if m, ok := memIndexMessage(); ok {
		out = append(out, m)
	}
	if m, ok := trackerIndexMessage(); ok {
		out = append(out, m)
	}
	return out
}

// hasIndexedPage dit si l'index contient au moins une ligne de page réelle
// (« - [titre](fichier.md) » en début de ligne).
func hasIndexedPage(idx string) bool {
	for _, line := range strings.Split(idx, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "- [") && strings.Contains(l, "](") {
			return true
		}
	}
	return false
}

// isIndexFile indique si `name` est le fichier d'index (à ne jamais indexer
// lui-même : il se listerait en boucle).
func isIndexFile(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), memIndexFile)
}

// indexLineFor construit la ligne d'index d'une page : `- [titre](fichier.md)`.
// Le titre vient de la 1re ligne de la page, à défaut le nom du fichier.
func indexLineFor(name string) string {
	title := name
	if t := titleOf(MemContent(name)); t != "" {
		title = t
	}
	return "- [" + title + "](" + name + ")"
}

// indexRefFor est le motif qui identifie la ligne d'une page dans l'index :
// `](fichier.md)`. Le nom complet ET la parenthèse fermante évitent qu'un nom en
// matche un autre dont il serait le suffixe (`notes.md` vs `mes-notes.md`).
func indexRefFor(name string) string { return "](" + name + ")" }

// writeIndex écrit le contenu de l'index. Passe par le disque directement plutôt
// que par MemSave : MemSave normalise et pourrait un jour se mettre à indexer,
// ce qui bouclerait.
func writeIndex(body string) {
	dir := memoryDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, memIndexFile), []byte(body), 0o644)
}

// memIndexAdd ajoute la ligne d'une page à MEMORY.md si elle n'y est pas déjà. Si
// une ligne pour ce fichier existe (l'IA a pu y écrire une accroche), on la LAISSE
// telle quelle : l'index appartient au code pour sa structure, au modèle pour sa
// prose.
func memIndexAdd(name string) {
	fn, err := memFileName(name)
	if err != nil || isIndexFile(fn) {
		return
	}
	ensureIndexSeed()
	content := MemContent(memIndexFile)
	ref := indexRefFor(fn)
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, ref) {
			return // déjà indexée
		}
	}
	writeIndex(strings.TrimRight(content, "\n") + "\n" + indexLineFor(fn) + "\n")
}

// memIndexRemove retire la (les) ligne(s) d'une page de MEMORY.md.
func memIndexRemove(name string) {
	fn, err := memFileName(name)
	if err != nil || isIndexFile(fn) {
		return
	}
	content := MemContent(memIndexFile)
	if content == "" {
		return
	}
	ref := indexRefFor(fn)
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	changed := false
	for _, line := range lines {
		if strings.Contains(line, ref) {
			changed = true
			continue
		}
		out = append(out, line)
	}
	if !changed {
		return
	}
	writeIndex(strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n")
}

// memIndexRename retire l'ancienne ligne et ajoute la nouvelle.
func memIndexRename(oldName, newName string) {
	if oldName != "" {
		memIndexRemove(oldName)
	}
	memIndexAdd(newName)
}

// reconcileMemIndex complète l'index du projet ACTIF : pour chaque page existante
// non référencée dans MEMORY.md, il ajoute sa ligne. ADDITIF — il ne touche ni aux
// lignes ni aux accroches déjà présentes. Indispensable après la migration vers
// les projets : les pages déplacées à la main sur le disque n'ont jamais été
// indexées, puisque l'index n'est alimenté que par la création d'une page.
func reconcileMemIndex() {
	pages := MemList()
	if len(pages) == 0 {
		return
	}
	ensureIndexSeed()
	content := MemContent(memIndexFile)
	var add []string
	for _, p := range pages {
		if isIndexFile(p.Name) {
			continue
		}
		if strings.Contains(content, indexRefFor(p.Name)) {
			continue // déjà indexée
		}
		add = append(add, indexLineFor(p.Name))
	}
	if len(add) == 0 {
		return
	}
	writeIndex(strings.TrimRight(content, "\n") + "\n" + strings.Join(add, "\n") + "\n")
}

// ensureIndexSeed crée MEMORY.md (avec son en-tête) s'il n'existe pas encore, pour
// que l'ajout d'une première ligne ait un fichier où s'écrire.
func ensureIndexSeed() {
	if strings.TrimSpace(MemContent(memIndexFile)) != "" {
		return
	}
	writeIndex(memorySeed(projectName(activeProjectSlug())))
}
