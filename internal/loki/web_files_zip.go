package loki

// web_files_zip.go — téléchargement de TOUS les fichiers d'une discussion en
// une archive .zip (bouton du panneau Fichiers).
//
//	GET /api/chat/files/zip?meta=1[&scope=legacy]  → {ok, name, size, e2e}
//	GET /api/chat/files/zip[&scope=legacy]          → le zip, brut
//	GET /api/chat/files/zip?b64=1&offset=&len=      → une tranche base64
//
// Même danse en trois temps que /api/chat/file (web_upload.go), et pour la
// même raison : à travers le tunnel chiffré, le binaire ne survit pas — seules
// les tranches base64 passent. L'archive est construite dans un fichier
// TEMPORAIRE à l'appel `meta` (le client commence toujours par lui), puis les
// appels suivants la servent telle quelle : la construire à chaque tranche
// donnerait des offsets incohérents si un fichier bouge entre deux tranches.
//
// Les liens symboliques ne sont PAS suivis (même règle que dirSize) et le
// parcours s'arrête à wsWalkLimit entrées : le zip d'une discussion est celui
// de ce que le panneau affiche, pas un aspirateur à disque.

import (
	"archive/zip"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// zipPathFor : l'archive temporaire d'une racine donnée. Un nom stable par
// discussion — l'appel meta l'écrase, les tranches la relisent.
func zipPathFor(scope string) string {
	id := "legacy"
	if strings.TrimSpace(scope) != "legacy" {
		id = safeConvID(convEnsureActive())
		if id == "" {
			id = "conv"
		}
	}
	return filepath.Join(os.TempDir(), "loki-files-"+id+".zip")
}

// zipName : le nom proposé au navigateur.
func zipName(scope string) string {
	stamp := time.Now().Format("2006-01-02")
	if strings.TrimSpace(scope) == "legacy" {
		return "loki-hors-discussion-" + stamp + ".zip"
	}
	// Le titre de la discussion, s'il existe, fait un nom parlant ; l'ID sinon.
	id := convEnsureActive()
	title := ""
	for _, m := range convIndex() {
		if m.ID == id {
			title = m.Title
			break
		}
	}
	slug := slugify(title)
	if slug == "" {
		slug = id
	}
	return "loki-" + slug + "-" + stamp + ".zip"
}

// slugify réduit un titre à un nom de fichier sûr : minuscules, ascii, tirets.
func slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '.':
			// Pas de tirets en rafale : « Déjà   vu » → dj-vu, pas dj---vu.
			if out := b.String(); out != "" && !strings.HasSuffix(out, "-") {
				b.WriteByte('-')
			}
		}
		if b.Len() >= 40 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// buildZip archive le contenu de root dans dest. Liens symboliques ignorés,
// parcours borné, chemins en avant-slash relatifs à root. En scope
// hors-discussion, le dossier des discussions est exclu (il a son propre zip,
// discussion par discussion).
func buildZip(root, dest string, excludeConvs bool) (files int, err error) {
	out, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	defer zw.Close()

	var walk func(dir, rel string) error
	walk = func(dir, rel string) error {
		ents, rerr := os.ReadDir(dir)
		if rerr != nil {
			return nil // dossier illisible : on archive le reste
		}
		for _, e := range ents {
			if files >= wsWalkLimit {
				return nil
			}
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			childRel := e.Name()
			if rel != "" {
				childRel = rel + "/" + e.Name()
			}
			if e.IsDir() {
				if excludeConvs && rel == "" && e.Name() == convFilesRoot {
					continue
				}
				if werr := walk(filepath.Join(dir, e.Name()), childRel); werr != nil {
					return werr
				}
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			hdr, herr := zip.FileInfoHeader(info)
			if herr != nil {
				continue
			}
			hdr.Name = childRel
			hdr.Method = zip.Deflate
			w, werr := zw.CreateHeader(hdr)
			if werr != nil {
				return werr
			}
			f, ferr := os.Open(filepath.Join(dir, e.Name()))
			if ferr != nil {
				continue
			}
			_, cerr := io.Copy(w, f)
			f.Close()
			if cerr != nil {
				return cerr
			}
			files++
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return files, err
	}
	return files, nil
}

// handleChatFilesZip — voir l'en-tête du fichier.
func handleChatFilesZip(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	scope := q.Get("scope")
	root, _ := filesRoot(scope)
	tmp := zipPathFor(scope)

	if q.Get("meta") != "" {
		files, err := buildZip(root, tmp, scope == "legacy")
		if err != nil {
			sendJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if files == 0 {
			os.Remove(tmp)
			sendJSON(w, 404, map[string]any{"ok": false, "error": "aucun fichier à archiver"})
			return
		}
		st, err := os.Stat(tmp)
		if err != nil {
			sendJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		sendJSON(w, 200, map[string]any{
			"ok": true, "name": zipName(scope), "size": st.Size(), "files": files,
			"e2e": r.Header.Get(e2eInnerHeader) != "",
		})
		return
	}

	st, err := os.Stat(tmp)
	if err != nil {
		sendJSON(w, 404, map[string]any{"ok": false, "error": "archive absente — redemande la fiche (meta=1) d'abord"})
		return
	}
	if q.Get("b64") != "" {
		off, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
		length, _ := strconv.ParseInt(q.Get("len"), 10, 64)
		if length <= 0 || length > downloadChunkMax {
			length = downloadChunkMax
		}
		if off < 0 || off > st.Size() {
			sendJSON(w, 400, map[string]any{"ok": false, "error": "position hors du fichier"})
			return
		}
		if off+length > st.Size() {
			length = st.Size() - off
		}
		f, ferr := os.Open(tmp)
		if ferr != nil {
			sendJSON(w, 500, map[string]any{"ok": false, "error": ferr.Error()})
			return
		}
		defer f.Close()
		buf := make([]byte, length)
		// Même lecture que /api/chat/file : ReadAt remplit tout le tampon, et
		// io.EOF sur la dernière tranche est normal.
		n, rerr := f.ReadAt(buf, off)
		if rerr != nil && rerr != io.EOF {
			sendJSON(w, 500, map[string]any{"ok": false, "error": rerr.Error()})
			return
		}
		sendJSON(w, 200, map[string]any{
			"ok": true, "name": zipName(scope), "size": st.Size(), "offset": off,
			"data": base64.StdEncoding.EncodeToString(buf[:n]),
			"eof":  off+int64(n) >= st.Size(),
		})
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", zipName(scope)))
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	f, err := os.Open(tmp)
	if err != nil {
		sendJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer f.Close()
	io.Copy(w, f)
}
