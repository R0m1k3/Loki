package loki

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// parity_web_tools_test.go — journal de référence des outils web de l'agent
// (lot 42 de la migration Rust) : web_search, web_images, web_open, web_read
// et web_grep, sur les deux moteurs.
//
// Un faux Crawl4AI rend des réponses rangées dans le journal, URL par URL ;
// un faux DuckDuckGo sert la recherche d'images. Le moteur intégré (Go) lit
// de vraies pages HTML d'un faux site et de faux résultats DuckDuckGo : ce
// qu'il en tire (goFetchMarkdown, goSearch) est rangé tel quel, la façade le
// demandant au Go. Chaque appel porte son résultat et les requêtes reçues
// par les faux services. Rangé dans rust/loki-rs/tests/parity_web/outils.json ;
// tests/webtools.rs le rejoue contre le port.
//
//	LOKI_PARITY_UPDATE=1 go test ./internal/loki -run TestPariteOutilsWeb

const parityWebFile = "../../rust/loki-rs/tests/parity_web/outils.json"

type parityCrawlResp struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

type parityImgResp struct {
	PageStatus int    `json:"page_status"`
	Page       string `json:"page"`
	Cookie     string `json:"cookie,omitempty"`
	APIStatus  int    `json:"api_status"`
	API        string `json:"api"`
}

type parityWebReq struct {
	Path  string `json:"path"`
	Auth  string `json:"auth,omitempty"`
	CType string `json:"ctype,omitempty"`
	Body  string `json:"body,omitempty"`
	// DuckDuckGo
	Referer string `json:"referer,omitempty"`
	Accept  string `json:"accept,omitempty"`
	Lang    string `json:"lang,omitempty"`
	UA      string `json:"ua,omitempty"`
	Cookie  string `json:"cookie,omitempty"`
}

type parityWebCall struct {
	Name     string            `json:"name"`
	Setup    map[string]string `json:"setup,omitempty"` // WEB_ENGINE, CRAWL4AI_URL, crawl_key ("-" efface)
	Advance  int               `json:"advance_s,omitempty"`
	Tool     string            `json:"tool"`
	Args     map[string]any    `json:"args"`
	Result   string            `json:"result"`
	Crawls   []parityWebReq    `json:"crawls"`
	Images   []parityWebReq    `json:"images"`
	GoFetch  []string          `json:"go_fetch"`
	GoSearch []string          `json:"go_search"`
}

type parityGoOut struct {
	Markdown string              `json:"markdown,omitempty"`
	Results  []map[string]string `json:"results,omitempty"`
	Error    string              `json:"error,omitempty"`
}

type parityWebJournal struct {
	Crawl    map[string][]parityCrawlResp `json:"crawl"`
	Images   map[string]parityImgResp     `json:"images"`
	GoFetch  map[string]parityGoOut       `json:"go_fetch"`
	GoSearch map[string]parityGoOut       `json:"go_search"`
	Calls    []parityWebCall              `json:"calls"`
}

// Les réponses du faux Crawl4AI, par URL demandée (consommées dans l'ordre,
// la dernière reste).
func parityCrawlFixtures() map[string][]parityCrawlResp {
	ddg := func(q string) string {
		return "https://html.duckduckgo.com/html/?q=" + q
	}
	obj := func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	long := strings.Repeat("une ligne assez longue pour remplir la fenêtre de lecture, encore et encore. ", 3)
	var longLines []string
	for i := 0; i < 60; i++ {
		longLines = append(longLines, fmt.Sprintf("%d %s", i, long))
	}
	searchMD := strings.Join([]string{
		"# DuckDuckGo",
		"",
		"## [Loki &amp; **Rust**](//duckduckgo.com/l/?uddg=https%3A%2F%2Floki.example%2Frust%3Fa%3D1%26b%3D2&rut=abc)",
		"",
		"[loki.example/rust](//duckduckgo.com/l/?uddg=x) [![](icon.png)](x) [Un port en **Rust** &#233;tape par &#233;tape](//duckduckgo.com/l/?uddg=y)",
		"## [Publicité](https://duckduckgo.com/y.js?ad=1)",
		"## [Deuxième &quot;résultat&quot;](https://deux.example/page)",
		"",
		"",
		"",
		"",
		"",
		"[trop loin pour le résumé](x)",
		"## [Loki encore](//duckduckgo.com/l/?uddg=https%3A%2F%2Floki.example%2Frust%3Fa%3D1%26b%3D2)",
		"## [Troisième](https://trois.example/)   ",
		"[!bang ignoré](x) [vraie description](y)",
		"## [Quatrième](//duckduckgo.com/l/?uddg=%ZZ)",
		"## [](https://vide.example/)",
	}, "\n")
	docMD := strings.Join([]string{
		"",
		"# Documentation   ",
		"",
		"Intro ligne 1",
		"Intro ligne 1",
		"",
		"",
		"",
		"## Installation",
		"Lancer `loki install`.\t",
		"Erreur : moteur absent",
		"### Détails",
		"ligne a",
		"ligne b",
		"Erreur : port pris",
		"ligne c",
		"ligne d",
		"ligne e",
		"ligne f",
		"Erreur : disque plein",
		"####### pas un titre",
		"## Fin\r",
		"",
	}, "\n")
	return map[string][]parityCrawlResp{
		ddg("loki+rust"): {{200, obj(map[string]any{"results": []any{map[string]any{"success": true,
			"markdown": map[string]any{"raw_markdown": searchMD, "fit_markdown": "ignoré"}}}})}},
		ddg("anti-bot"): {{200, obj(map[string]any{"results": []any{map[string]any{"success": true,
			"markdown": "<div class=\"anomaly-modal\">"}}})}},
		ddg("rien"):  {{200, obj(map[string]any{"results": []any{map[string]any{"success": true, "markdown": "# Rien\n\npas de résultat"}}})}},
		ddg("panne"): {{500, "Internal Server Error: " + strings.Repeat("é", 320)}},
		"https://exemple.test/doc": {
			{200, obj(map[string]any{"results": []any{map[string]any{"success": true,
				"markdown": map[string]any{"raw_markdown": "brut", "fit_markdown": docMD}}}})},
			{200, obj(map[string]any{"results": []any{map[string]any{"success": true,
				"markdown": "# Version 2\n\nnouveau contenu"}}})},
			{200, obj(map[string]any{"results": []any{map[string]any{"success": true,
				"markdown": "# Avec actions\n\nbouton cliqué"}}})},
		},
		"https://raw.githubusercontent.com/r0m1k3/loki/HEAD/README.md": {{200, obj(map[string]any{"results": []any{map[string]any{"success": true, "markdown": "# Loki\n\nLe README."}}})}},
		"https://exemple.test/echec": {{200, obj(map[string]any{"results": []any{map[string]any{"success": false,
			"error_message": "net::ERR_NAME_NOT_RESOLVED " + strings.Repeat("x", 310)}}})}},
		"https://exemple.test/echec-muet":  {{200, `{"results":[{"success":false}]}`}},
		"https://exemple.test/vide":        {{200, `{"results":[]}`}},
		"https://exemple.test/tableau":     {{200, `[]`}},
		"https://exemple.test/illisible":   {{200, `pas du json`}},
		"https://exemple.test/seul":        {{200, `{"success":true,"markdown":"# Seul\n\nun résultat seul"}`}},
		"https://exemple.test/mal-type":    {{200, `{"results":[{"success":"oui","markdown":"x"}]}`}},
		"https://exemple.test/blanc":       {{200, `{"results":[{"success":true,"markdown":"  \n\t\n"}]}`}},
		"https://exemple.test/objet-faux":  {{200, `{"results":[{"success":true,"markdown":{"raw_markdown":5}}]}`}},
		"https://exemple.test/longue":      {{200, obj(map[string]any{"results": []any{map[string]any{"success": true, "markdown": strings.Join(longLines, "\n")}}})}},
		"https://exemple.test/brut-absent": {{200, `{"results":[{"success":true,"markdown":{"fit_markdown":"# Fit seul\n\nok"}}]}`}},
	}
}

func parityImageFixtures() map[string]parityImgResp {
	page := func(vqd string) string {
		return `<html><script>var x = 1; vqd="` + vqd + `"; </script></html>`
	}
	res := func(items ...map[string]any) string {
		b, _ := json.Marshal(map[string]any{"results": items})
		return string(b)
	}
	it := func(img, title, u string, w, h int) map[string]any {
		return map[string]any{"image": img, "title": title, "url": u, "width": w, "height": h, "thumbnail": "t"}
	}
	return map[string]parityImgResp{
		"chats": {200, page("4-123-456"), "dcm=7; Path=/; HttpOnly", 200, res(
			it("https://img.example/1.jpg", "Chat roux", "https://page.example/1", 800, 600),
			it("https://img.example/1.jpg", "Doublon", "https://page.example/1b", 1, 1),
			it("", "Sans image", "https://page.example/2", 1, 1),
			it("data:image/png;base64,xx", "Pas http", "https://page.example/3", 1, 1),
			it("http://img.example/4.png", "Chat & chien <3", "https://page.example/4", 1024, 768),
			it("https://img.example/5.jpg", "Cinquième", "https://page.example/5", 10, 20),
		)},
		"sans jeton":     {200, "<html>rien</html>", "", 200, "{}"},
		"api en panne":   {200, page("1"), "", 500, "oops"},
		"page en panne":  {403, "interdit", "", 200, "{}"},
		"illisible":      {200, page("2"), "", 200, "pas du json"},
		"apres":          {200, page("3"), "", 200, `{"results":[]} x`},
		"largeur texte":  {200, page("4"), "", 200, `{"results":[{"image":"https://i/1","width":"800"}]}`},
		"vide":           {200, page("5"), "", 200, `{"results":[]}`},
		"null":           {200, page("6"), "", 200, `{"results":null,"next":"i.js?x"}`},
		"resultats faux": {200, page("7"), "", 200, `{"results":"non"}`},
		"apostrophe":     {200, `vqd='77-88'`, "", 200, `{"results":[{"image":"https://i/a","title":"A","url":"https://p/a","width":1,"height":2}]}`},
	}
}

// Le faux site du moteur intégré.
var parityWebPages = map[string]string{
	"/article.html": `<!doctype html><html><head><title>Guide de Loki</title></head><body>
<nav><a href="https://loki.example/">Accueil</a> · <a href="https://loki.example/blog">Blog</a></nav>
<article><h1>Guide de Loki</h1>
<p>Loki fait tourner un modèle de langage sur votre machine, sans rien envoyer ailleurs. Ce guide explique l'installation, la configuration et les premiers pas avec l'agent.</p>
<h2>Installation</h2>
<p>Téléchargez le binaire depuis la <a href="https://loki.example/releases">page des versions</a>, puis lancez-le. Le moteur llama.cpp est installé au premier démarrage, précompilé pour votre carte graphique.</p>
<h2>Configuration</h2>
<p>Les réglages se font dans l'interface : modèle, contexte, outils. Erreur fréquente : un port déjà pris par un autre service. Changez alors le port dans les réglages avancés.</p>
<p>Erreur moins fréquente : un disque plein empêche le téléchargement du modèle.</p>
</article><footer>© Loki</footer></body></html>`,
	"/texte.txt": "Ligne brute 1\nLigne brute 2\n",
}

// Les faux résultats DuckDuckGo (page HTML) du moteur intégré.
var parityWebDDGHTML = map[string]string{
	"loki": `<html><body><div class="results">
<div class="result results_links"><h2 class="result__title"><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Floki.example%2F&rut=x">Loki   <b>local</b></a></h2>
<a class="result__snippet" href="x">Un assistant <b>local</b> et   privé.</a></div>
<div class="result"><a class="result__a" href="https://duckduckgo.com/ad">Pub</a></div>
<div class="result"><a class="result__a" href="https://deux.example/">Deux</a></div>
<div class="result"><a class="result__a" href="https://loki.example/">Doublon</a></div>
</div></body></html>`,
	"bot": `<html><script src="/anomaly.js"></script></html>`,
}

func TestPariteOutilsWeb(t *testing.T) {
	testHome(t)
	crawlFx := parityCrawlFixtures()
	imgFx := parityImageFixtures()

	var mu sync.Mutex
	var crawls, images []parityWebReq
	served := map[string]int{}
	crawlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in struct {
			URLs []string `json:"urls"`
		}
		_ = json.Unmarshal(body, &in)
		mu.Lock()
		crawls = append(crawls, parityWebReq{Path: r.Method + " " + r.URL.Path, Auth: r.Header.Get("Authorization"),
			CType: r.Header.Get("Content-Type"), Body: string(body)})
		target := ""
		if len(in.URLs) > 0 {
			target = in.URLs[0]
		}
		list := crawlFx[target]
		n := served[target]
		served[target]++
		mu.Unlock()
		if len(list) == 0 {
			w.WriteHeader(404)
			_, _ = io.WriteString(w, "inconnu : "+target)
			return
		}
		if n >= len(list) {
			n = len(list) - 1
		}
		w.WriteHeader(list[n].Status)
		_, _ = io.WriteString(w, list[n].Body)
	}))
	defer crawlSrv.Close()
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		images = append(images, parityWebReq{Path: r.URL.RequestURI(), Referer: r.Header.Get("Referer"),
			Accept: r.Header.Get("Accept"), Lang: r.Header.Get("Accept-Language"),
			UA: r.Header.Get("User-Agent"), Cookie: r.Header.Get("Cookie")})
		mu.Unlock()
		fx := imgFx[r.URL.Query().Get("q")]
		if r.URL.Path == "/i.js" {
			w.WriteHeader(fx.APIStatus)
			_, _ = io.WriteString(w, fx.API)
			return
		}
		if fx.Cookie != "" {
			w.Header().Set("Set-Cookie", fx.Cookie)
		}
		w.WriteHeader(fx.PageStatus)
		_, _ = io.WriteString(w, fx.Page)
	}))
	defer imgSrv.Close()
	var pagesHit, searchHit []string
	pagesSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pagesHit = append(pagesHit, r.URL.Path)
		mu.Unlock()
		p, ok := parityWebPages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".txt") {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		} else {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
		_, _ = io.WriteString(w, p)
	}))
	defer pagesSrv.Close()
	ddgHTMLSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		mu.Lock()
		searchHit = append(searchHit, q)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, parityWebDDGHTML[q])
	}))
	defer ddgHTMLSrv.Close()
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.Addr().String()
	dead.Close()

	oldImg, oldHTML, oldNow, oldJar := ddgImagesBase, ddgHTMLBase, webNow, goHTTPClient.Jar
	ddgImagesBase, ddgHTMLBase = imgSrv.URL, ddgHTMLSrv.URL
	goHTTPClient.Jar = newCookieJar() // les cookies d'un passage précédent ne comptent pas
	base := time.Now()
	var skew time.Duration
	webNow = func() time.Time { return base.Add(skew) }
	pageCacheMu.Lock()
	pageCache = map[string]*cacheEntry{}
	pageCacheMu.Unlock()
	defer func() {
		ddgImagesBase, ddgHTMLBase, webNow, goHTTPClient.Jar = oldImg, oldHTML, oldNow, oldJar
		pageCacheMu.Lock()
		pageCache = map[string]*cacheEntry{}
		pageCacheMu.Unlock()
	}()

	mask := func(s string) string {
		s = strings.ReplaceAll(s, crawlSrv.URL, "{{CRAWL}}")
		s = strings.ReplaceAll(s, imgSrv.URL, "{{DDG}}")
		s = strings.ReplaceAll(s, pagesSrv.URL, "{{PAGES}}")
		s = strings.ReplaceAll(s, "http://"+deadAddr, "{{DEAD}}")
		return strings.ReplaceAll(s, deadAddr, "{{DEADADDR}}")
	}
	unmask := func(s string) string {
		s = strings.ReplaceAll(s, "{{CRAWL}}", crawlSrv.URL)
		s = strings.ReplaceAll(s, "{{PAGES}}", pagesSrv.URL)
		return strings.ReplaceAll(s, "{{DEAD}}", "http://"+deadAddr)
	}
	maskReqs := func(rs []parityWebReq) []parityWebReq {
		out := []parityWebReq{}
		for _, r := range rs {
			r.Path, r.Body, r.Referer = mask(r.Path), mask(r.Body), mask(r.Referer)
			out = append(out, r)
		}
		return out
	}

	// Ce que le moteur intégré tire des pages et des résultats, rangé pour la
	// façade (qui le demande au Go).
	goFetch := map[string]parityGoOut{}
	for _, p := range []string{"/article.html", "/texte.txt", "/absente"} {
		md, err := goFetchMarkdown(pagesSrv.URL + p)
		o := parityGoOut{Markdown: mask(md)}
		if err != nil {
			o = parityGoOut{Error: mask(err.Error())}
		}
		goFetch["{{PAGES}}"+p] = o
	}
	goSearchOut := map[string]parityGoOut{}
	for _, q := range []struct {
		q string
		n int
	}{{"loki", 8}, {"loki", 1}, {"bot", 8}} {
		res, err := goSearch(q.q, q.n)
		o := parityGoOut{Results: []map[string]string{}}
		for _, r := range res {
			o.Results = append(o.Results, map[string]string{"title": r.Title, "url": r.URL, "snippet": r.Snippet})
		}
		if err != nil {
			o = parityGoOut{Error: err.Error()}
		}
		goSearchOut[fmt.Sprintf("%s|%d", q.q, q.n)] = o
	}
	mu.Lock()
	pagesHit, searchHit = nil, nil
	mu.Unlock()

	crawlSetup := map[string]string{"WEB_ENGINE": "crawl4ai", "CRAWL4AI_URL": "{{CRAWL}}", "crawl_key": "sesame"}
	var calls []parityWebCall
	run := func(c parityWebCall) {
		for k, v := range c.Setup {
			switch k {
			case "crawl_key":
				if v == "-" {
					v = ""
				}
				if err := putStr(bkState, "crawl_key", v); err != nil {
					t.Fatal(err)
				}
			default:
				if v == "-" {
					v = ""
				}
				if err := SetConfigKey(k, unmask(v)); err != nil {
					t.Fatal(err)
				}
			}
		}
		skew += time.Duration(c.Advance) * time.Second
		args := map[string]any{}
		raw, _ := json.Marshal(c.Args)
		_ = json.Unmarshal([]byte(unmask(string(raw))), &args)
		res := execTool(t.Context(), c.Tool, args, toolCallLabel(c.Tool, args), Caps{Agent: true})
		mu.Lock()
		c.Result = mask(res.Result)
		c.Crawls, c.Images = maskReqs(crawls), maskReqs(images)
		c.GoFetch, c.GoSearch = []string{}, []string{}
		for _, p := range pagesHit {
			c.GoFetch = append(c.GoFetch, "{{PAGES}}"+p)
		}
		c.GoSearch = append(c.GoSearch, searchHit...)
		crawls, images, pagesHit, searchHit = nil, nil, nil, nil
		mu.Unlock()
		if c.Args == nil {
			c.Args = map[string]any{}
		}
		calls = append(calls, c)
	}
	a := func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}

	// Crawl4AI.
	run(parityWebCall{Name: "recherche", Setup: crawlSetup, Tool: "web_search", Args: a("query", "loki rust")})
	run(parityWebCall{Name: "recherche limitée", Tool: "web_search", Args: a("query", "loki rust", "limit", 1.0)})
	run(parityWebCall{Name: "recherche, limite hors bornes", Tool: "web_search", Args: a("query", "loki rust", "limit", 99.0)})
	run(parityWebCall{Name: "recherche, limite nulle", Tool: "web_search", Args: a("query", "loki rust", "limit", -3.0)})
	run(parityWebCall{Name: "recherche, limite texte", Tool: "web_search", Args: a("query", "loki rust", "limit", "2")})
	run(parityWebCall{Name: "défi anti-bot", Tool: "web_search", Args: a("query", "anti-bot")})
	run(parityWebCall{Name: "aucun résultat", Tool: "web_search", Args: a("query", "rien")})
	run(parityWebCall{Name: "Crawl4AI en panne", Tool: "web_search", Args: a("query", "panne")})
	run(parityWebCall{Name: "ouverture", Tool: "web_open", Args: a("url", "https://exemple.test/doc")})
	run(parityWebCall{Name: "lecture", Tool: "web_read", Args: a("url", "https://exemple.test/doc")})
	run(parityWebCall{Name: "lecture d'une plage", Tool: "web_read", Args: a("url", "https://exemple.test/doc", "offset", 5.0, "limit", 3.0)})
	run(parityWebCall{Name: "lecture au-delà", Tool: "web_read", Args: a("url", "https://exemple.test/doc", "offset", 400.0)})
	run(parityWebCall{Name: "lecture, bornes", Tool: "web_read", Args: a("url", "https://exemple.test/doc", "offset", -2.0, "limit", 0.0)})
	run(parityWebCall{Name: "grep", Tool: "web_grep", Args: a("url", "https://exemple.test/doc", "pattern", "erreur")})
	run(parityWebCall{Name: "grep sans contexte", Tool: "web_grep", Args: a("url", "https://exemple.test/doc", "pattern", "^ligne [ae]$", "context", 0.0)})
	run(parityWebCall{Name: "grep plafonné", Tool: "web_grep", Args: a("url", "https://exemple.test/doc", "pattern", "ligne", "max_matches", 2.0, "context", 1.0)})
	run(parityWebCall{Name: "grep, contexte large", Tool: "web_grep", Args: a("url", "https://exemple.test/doc", "pattern", "Détails", "context", 50.0)})
	run(parityWebCall{Name: "grep sans résultat", Tool: "web_grep", Args: a("url", "https://exemple.test/doc", "pattern", "introuvable")})
	run(parityWebCall{Name: "grep, ascii", Tool: "web_grep", Args: a("url", "https://exemple.test/doc", "pattern", `\bD\w+ils`)})
	for i, p := range []string{"(", "a)", "[abc", "*x", "a**", `\q`, `a\`} {
		run(parityWebCall{Name: fmt.Sprintf("regex invalide %d", i+1), Tool: "web_grep", Args: a("url", "https://exemple.test/doc", "pattern", p)})
	}
	run(parityWebCall{Name: "page jamais ouverte", Tool: "web_read", Args: a("url", "https://exemple.test/autre")})
	run(parityWebCall{Name: "grep, page jamais ouverte", Tool: "web_grep", Args: a("url", "https://exemple.test/autre", "pattern", "x")})
	run(parityWebCall{Name: "réouverture : cache", Tool: "web_open", Args: a("url", "https://exemple.test/doc")})
	run(parityWebCall{Name: "réouverture forcée", Tool: "web_open", Args: a("url", "https://exemple.test/doc", "refresh", true)})
	run(parityWebCall{Name: "ouverture avec actions", Advance: 5, Tool: "web_open", Args: a("url", "https://exemple.test/doc",
		"actions", []any{"document.querySelector('#plus').click();", 3.0, "x && y < z"}, "wait_for", "css:#fin", "dismiss_popups", false)})
	run(parityWebCall{Name: "lecture : la plus récente", Tool: "web_read", Args: a("url", "https://exemple.test/doc")})
	run(parityWebCall{Name: "attente seule", Tool: "web_open", Args: a("url", "https://exemple.test/seul", "wait_for", "js:() => true")})
	run(parityWebCall{Name: "sans fermer les bandeaux", Tool: "web_open", Args: a("url", "https://exemple.test/brut-absent", "dismiss_popups", false)})
	run(parityWebCall{Name: "cache périmé", Advance: 601, Tool: "web_read", Args: a("url", "https://exemple.test/doc")})
	run(parityWebCall{Name: "ouverture après péremption", Tool: "web_open", Args: a("url", "https://exemple.test/doc")})
	run(parityWebCall{Name: "dépôt GitHub", Tool: "web_open", Args: a("url", "https://github.com/r0m1k3/loki/")})
	run(parityWebCall{Name: "lecture du dépôt", Tool: "web_read", Args: a("url", "https://github.com/r0m1k3/loki")})
	run(parityWebCall{Name: "GitHub, pas un dépôt", Tool: "web_open", Args: a("url", "https://github.com/r0m1k3")})
	run(parityWebCall{Name: "crawl en échec", Tool: "web_open", Args: a("url", "https://exemple.test/echec")})
	run(parityWebCall{Name: "crawl en échec, sans message", Tool: "web_open", Args: a("url", "https://exemple.test/echec-muet")})
	run(parityWebCall{Name: "réponse vide", Tool: "web_open", Args: a("url", "https://exemple.test/vide")})
	run(parityWebCall{Name: "réponse tableau", Tool: "web_open", Args: a("url", "https://exemple.test/tableau")})
	run(parityWebCall{Name: "réponse illisible", Tool: "web_open", Args: a("url", "https://exemple.test/illisible")})
	run(parityWebCall{Name: "résultat mal typé", Tool: "web_open", Args: a("url", "https://exemple.test/mal-type")})
	run(parityWebCall{Name: "page blanche", Tool: "web_open", Args: a("url", "https://exemple.test/blanc")})
	run(parityWebCall{Name: "markdown objet faux", Tool: "web_open", Args: a("url", "https://exemple.test/objet-faux")})
	run(parityWebCall{Name: "page longue", Tool: "web_open", Args: a("url", "https://exemple.test/longue")})
	run(parityWebCall{Name: "lecture tronquée", Tool: "web_read", Args: a("url", "https://exemple.test/longue", "limit", 500.0)})
	run(parityWebCall{Name: "URL inconnue du faux", Tool: "web_open", Args: a("url", "https://exemple.test/inconnue")})
	run(parityWebCall{Name: "sans clé", Setup: map[string]string{"crawl_key": "-"}, Tool: "web_open", Args: a("url", "https://exemple.test/seul", "refresh", true)})
	run(parityWebCall{Name: "Crawl4AI injoignable", Setup: map[string]string{"CRAWL4AI_URL": "{{DEAD}}/"}, Tool: "web_open", Args: a("url", "https://exemple.test/x")})
	run(parityWebCall{Name: "sans serveur Crawl4AI", Setup: map[string]string{"CRAWL4AI_URL": "-"}, Tool: "web_open", Args: a("url", "https://exemple.test/x")})
	run(parityWebCall{Name: "lecture d'une page vue par Crawl4AI", Tool: "web_read", Args: a("url", "https://exemple.test/seul", "limit", 1.0)})

	// Moteur intégré.
	goSetup := map[string]string{"WEB_ENGINE": "go"}
	run(parityWebCall{Name: "intégré : ouverture", Setup: goSetup, Tool: "web_open", Args: a("url", "{{PAGES}}/article.html")})
	run(parityWebCall{Name: "intégré : grep", Tool: "web_grep", Args: a("url", "{{PAGES}}/article.html", "pattern", "erreur", "context", 0.0)})
	run(parityWebCall{Name: "intégré : texte brut", Tool: "web_open", Args: a("url", "{{PAGES}}/texte.txt", "actions", []any{"x()"})})
	run(parityWebCall{Name: "intégré : lecture", Tool: "web_read", Args: a("url", "{{PAGES}}/texte.txt")})
	run(parityWebCall{Name: "intégré : page absente", Tool: "web_open", Args: a("url", "{{PAGES}}/absente")})
	run(parityWebCall{Name: "intégré : recherche", Tool: "web_search", Args: a("query", "loki")})
	run(parityWebCall{Name: "intégré : recherche limitée", Tool: "web_search", Args: a("query", "loki", "limit", 1.0)})
	run(parityWebCall{Name: "intégré : anti-bot", Tool: "web_search", Args: a("query", "bot")})

	// Images (même client quel que soit le moteur).
	run(parityWebCall{Name: "images", Tool: "web_images", Args: a("query", "chats")})
	run(parityWebCall{Name: "images, cookie gardé", Tool: "web_images", Args: a("query", "chats", "limit", 1.0)})
	for _, q := range []string{"sans jeton", "api en panne", "page en panne", "illisible", "apres", "largeur texte", "vide", "null", "resultats faux", "apostrophe"} {
		run(parityWebCall{Name: "images : " + q, Tool: "web_images", Args: a("query", q)})
	}

	j := parityWebJournal{Crawl: crawlFx, Images: imgFx, GoFetch: goFetch, GoSearch: goSearchOut, Calls: calls}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(j); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()
	if os.Getenv("LOKI_PARITY_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(parityWebFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityWebFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(parityWebFile)
	if err != nil {
		t.Fatalf("journal de référence absent (%v) : LOKI_PARITY_UPDATE=1 pour le créer", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("les outils web ne produisent plus le journal rangé dans %s.\n"+
			"LOKI_PARITY_UPDATE=1 pour le régénérer si le changement est voulu.", parityWebFile)
	}
}
