package loki

import (
	"net/http"
	"path"
	"strconv"
	"strings"
)

// web_katex.go — KaTeX 0.16.11 (formules LaTeX des réponses), repris d'AJEAN.
//
// Embarqué sous ui/katex/ et servi sous /katex/ : l'UI ne le charge qu'à la
// première formule d'une réponse (00-md.js), et il doit marcher hors ligne,
// sans appeler un CDN. Seuls le script, la feuille de style et les polices
// woff2 sont servis — pas d'index de dossier — avec un Content-Type fixe : le
// registre MIME de l'OS (Windows notamment) ne doit pas le décider, et la
// façade Rust (rust/loki-rs/src/ui.rs) rend exactement les mêmes en-têtes.

// katexTypes : extensions servies et leur Content-Type.
var katexTypes = map[string]string{
	".js":    "application/javascript",
	".css":   "text/css; charset=utf-8",
	".woff2": "font/woff2",
}

// katexFile : chemin relatif à ui/katex/ d'une ressource servable, ou "" si la
// demande n'en est pas une (racine, dossier, autre extension, remontée).
func katexFile(urlPath string) (rel, ctype string) {
	rel = strings.TrimPrefix(urlPath, "/katex/")
	if rel == urlPath || rel == "" || strings.Contains(rel, "..") || path.Clean(rel) != rel {
		return "", ""
	}
	ctype, ok := katexTypes[path.Ext(rel)]
	if !ok {
		return "", ""
	}
	return rel, ctype
}

func handleKatex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	rel, ctype := katexFile(r.URL.Path)
	if rel == "" {
		http.NotFound(w, r)
		return
	}
	b, err := uiFS.ReadFile("ui/katex/" + rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Write(b)
}
