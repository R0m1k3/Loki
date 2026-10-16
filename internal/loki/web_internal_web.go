package loki

import "net/http"

// API interne du moteur web intégré (lot 42 de la migration Rust).
//
// La façade exécute les outils web de ses tours (web_search, web_images,
// web_open, web_read, web_grep) : cache des pages, client Crawl4AI, lecture
// des résultats, mise en forme. Le moteur intégré lit le HTML avec
// go-readability, html-to-markdown et le parseur HTML5 de x/net : rien en
// Rust ne les reproduit au caractère près, ils restent donc ici, derrière
// deux appels sans état.
//
//	POST /internal/v1/web/fetch  {"url":"…"}
//	→ {"markdown":"…"} | {"error":"…"}
//	POST /internal/v1/web/search {"query":"…","limit":8}
//	→ {"results":[{"title":"…","url":"…","snippet":"…"}]} | {"error":"…"}

func handleInternalWebFetch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	md, err := goFetchMarkdown(in.URL)
	if err != nil {
		sendJSON(w, http.StatusOK, map[string]any{"error": err.Error()})
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{"markdown": md})
}

func handleInternalWebSearch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if !readInternalJSON(w, r, &in) {
		return
	}
	results, err := goSearch(in.Query, in.Limit)
	if err != nil {
		sendJSON(w, http.StatusOK, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]string, 0, len(results))
	for _, r := range results {
		out = append(out, map[string]string{"title": r.Title, "url": r.URL, "snippet": r.Snippet})
	}
	sendJSON(w, http.StatusOK, map[string]any{"results": out})
}
