//! Outils web de l'agent (lot 42 de la migration) — port de
//! `chat_internet_tools.go` (`web_search`, `web_open`, `web_read`,
//! `web_grep`), de `chat_internet.go` (client Crawl4AI, cache des pages,
//! lecture des résultats DuckDuckGo) et de `chat_web_images.go`
//! (`web_images`).
//!
//! Le cache des pages (10 min) est ici : `web_open` puis `web_read` ou
//! `web_grep` dans un tour de la façade ne passent plus par le Go. Le
//! moteur intégré lit le HTML avec go-readability, html-to-markdown et le
//! parseur HTML5 de x/net, que rien en Rust ne reproduit au caractère près :
//! la conversion d'une page et la lecture des résultats DuckDuckGo restent
//! au Go (`/internal/v1/web/fetch`, `/internal/v1/web/search`), sans état.
//! `web_screenshot` reste au Go (navigateur piloté).

use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant};

use hyper::Method;
use serde_json::{Map, Value, json};

use crate::hf::go_query_escape;
use crate::store::{BK_STATE, Store};
use crate::system::{ENGINE_GO, crawl4ai_url, go_lossy, web_engine};

/// `webMaxOutput` : plafond d'une réponse d'outil web (en caractères).
pub const MAX_OUTPUT: usize = 8000;
/// `pageCacheTTL`.
pub const PAGE_TTL: Duration = Duration::from_secs(600);
/// `goFetchUA`.
pub const UA: &str = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36";
/// `goFetchMaxBytes`.
const MAX_BYTES: usize = 8 << 20;
/// `goFetchTimeout`.
const FETCH_TIMEOUT: Duration = Duration::from_secs(30);
/// Délai d'un appel à Crawl4AI.
const CRAWL_TIMEOUT: Duration = Duration::from_secs(60);

/// `autoDismissJS` : bandeaux cookies et consentement fermés dans la page.
pub const AUTO_DISMISS_JS: &str = r#"(() => { try {
  const TEXT = /^(accept all|accept|i accept|agree|i agree|got it|i understand|j'accepte|tout accepter|accepter|d'accord|allow all|allow|consent|continue|ok)$/i;
  const isOverlayish = (el) => {
    try {
      let cur = el;
      for (let i = 0; i < 6 && cur; i++) {
        const s = getComputedStyle(cur);
        if (s.position === 'fixed' || s.position === 'sticky') return true;
        const idcls = ((cur.id || '') + ' ' + (cur.className || '')).toLowerCase();
        if (/cookie|consent|gdpr|cmp|privacy/.test(idcls)) return true;
        cur = cur.parentElement;
      }
    } catch {}
    return false;
  };
  const candidates = document.querySelectorAll('button, [role="button"], input[type="button"], input[type="submit"]');
  let clicked = 0;
  for (const b of candidates) {
    if (clicked >= 2) break;
    const t = (b.innerText || b.value || b.getAttribute('aria-label') || '').trim();
    if (!t || !TEXT.test(t)) continue;
    if (b.offsetParent === null) continue;
    if (!isOverlayish(b)) continue;
    try { b.click(); clicked++; } catch {}
  }
  document.querySelectorAll('[id*="cookie" i],[id*="consent" i],[class*="cookie" i],[class*="consent" i],[id*="gdpr" i],[class*="gdpr" i],[id*="cmp" i],[class*="cmp" i]')
    .forEach(el => { try {
      const s = getComputedStyle(el);
      if (s.position === 'fixed' || s.position === 'sticky') el.remove();
    } catch {} });
  document.documentElement.style.overflow = 'auto';
  if (document.body) document.body.style.overflow = 'auto';
} catch {} })();"#;

/// Les outils servis ici.
pub fn is_tool(name: &str) -> bool {
    matches!(
        name,
        "web_search" | "web_images" | "web_open" | "web_read" | "web_grep"
    )
}

/// Une page ouverte (`cacheEntry`).
#[derive(Debug)]
pub struct Entry {
    pub url: String,
    pub lines: Vec<String>,
    at: Instant,
}

/// `fetchOptions`.
#[derive(Clone, Debug, Default)]
pub struct FetchOpts {
    pub force: bool,
    pub actions: Vec<String>,
    pub dismiss_popups: bool,
    pub wait_for: String,
}

/// `crwlOptions`.
#[derive(Default)]
struct CrwlOpts {
    js_code: Vec<String>,
    wait_for: String,
    page_timeout_ms: i64,
    raw_markdown: bool,
}

/// `searchResult`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SearchResult {
    pub title: String,
    pub url: String,
    pub snippet: String,
}

/// `imageResult`.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct ImageResult {
    pub image: String,
    pub title: String,
    pub url: String,
    pub width: i64,
    pub height: i64,
}

/// Les outils web d'une façade : cache des pages, clients HTTP.
pub struct Web {
    cache: std::sync::Mutex<HashMap<String, Arc<Entry>>>,
    crawl: reqwest::Client,
    http: reqwest::Client,
    /// Cookies posés par les sites joints par `http` (le jar en mémoire du
    /// client du moteur intégré) : (hôte, nom, valeur), dans l'ordre.
    jar: std::sync::Mutex<Vec<(String, String, String)>>,
    /// `ddgImagesBase`.
    pub ddg_base: String,
    /// Avance de l'horloge du cache (tests).
    skew: std::sync::Mutex<Duration>,
}

impl Default for Web {
    fn default() -> Self {
        Self::new()
    }
}

impl Web {
    pub fn new() -> Self {
        Self::with_ddg_base("https://duckduckgo.com")
    }

    /// Pour les tests : un faux DuckDuckGo.
    pub fn with_ddg_base(base: &str) -> Self {
        crate::chat::engine::install_crypto();
        Web {
            cache: Default::default(),
            crawl: reqwest::Client::builder().build().unwrap_or_default(),
            http: reqwest::Client::builder()
                .redirect(reqwest::redirect::Policy::limited(10))
                .timeout(FETCH_TIMEOUT)
                .build()
                .unwrap_or_default(),
            jar: Default::default(),
            ddg_base: base.trim_end_matches('/').to_string(),
            skew: Default::default(),
        }
    }

    /// Avance l'horloge du cache (tests).
    pub fn advance(&self, d: Duration) {
        *self.skew.lock().unwrap_or_else(|e| e.into_inner()) += d;
    }

    fn now(&self) -> Instant {
        Instant::now() + *self.skew.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Un outil web : le résultat que le modèle reçoit, `None` pour un autre
    /// outil.
    pub async fn exec(
        &self,
        store: &Store,
        name: &str,
        args: &Map<String, Value>,
    ) -> Option<String> {
        let out = match name {
            "web_search" => self.tool_search(store, args).await,
            "web_images" => self.tool_images(args).await,
            "web_open" => self.tool_open(store, args).await,
            "web_read" => self.tool_read(args),
            "web_grep" => self.tool_grep(args),
            _ => return None,
        };
        Some(cap_output(&out))
    }

    // --- Outils -------------------------------------------------------------

    /// `toolWebSearch`.
    async fn tool_search(&self, store: &Store, args: &Map<String, Value>) -> String {
        let query = arg_str(args, "query");
        let limit = arg_int(args, "limit").unwrap_or(8).clamp(1, 20) as usize;
        let results = match self.search(store, &query, limit).await {
            Ok(r) => r,
            Err(e) => return format!("❌ Recherche échouée : {e}"),
        };
        if results.is_empty() {
            return format!("Aucun résultat pour « {query} »");
        }
        let mut b = format!(
            "# Recherche : {query}\n{} résultat(s) DuckDuckGo\n\n",
            results.len()
        );
        for (i, r) in results.iter().enumerate() {
            b.push_str(&format!(
                "{}. {}\n   {}\n   {}\n\n",
                i + 1,
                r.title,
                r.url,
                r.snippet
            ));
        }
        b.trim_end_matches('\n').to_string()
    }

    /// `toolWebImages`.
    async fn tool_images(&self, args: &Map<String, Value>) -> String {
        let query = arg_str(args, "query");
        let limit = arg_int(args, "limit").unwrap_or(6).clamp(1, 20) as usize;
        let results = match self.images(&query, limit).await {
            Ok(r) => r,
            Err(e) => return format!("❌ Recherche d'images échouée : {e}"),
        };
        if results.is_empty() {
            return format!("Aucune image pour « {query} »");
        }
        let mut b = format!(
            "# Images : {query}\n{} résultat(s) DuckDuckGo\n\n",
            results.len()
        );
        for (i, r) in results.iter().enumerate() {
            b.push_str(&format!(
                "{}. {} ({}x{})\n   image : {}\n   page : {}\n\n",
                i + 1,
                r.title,
                r.width,
                r.height,
                r.image,
                r.url
            ));
        }
        b.push_str("To show an image to the user, write ![title](image URL) in your answer.");
        b
    }

    /// `toolWebOpen`.
    async fn tool_open(&self, store: &Store, args: &Map<String, Value>) -> String {
        let u = arg_str(args, "url");
        let mut opts = FetchOpts {
            dismiss_popups: true,
            ..Default::default()
        };
        if let Some(Value::Bool(b)) = args.get("refresh") {
            opts.force = *b;
        }
        if let Some(Value::Bool(b)) = args.get("dismiss_popups") {
            opts.dismiss_popups = *b;
        }
        if let Some(Value::String(s)) = args.get("wait_for") {
            opts.wait_for = s.clone();
        }
        if let Some(Value::Array(a)) = args.get("actions") {
            opts.actions = a
                .iter()
                .filter_map(|v| v.as_str().map(String::from))
                .collect();
        }
        let entry = match self.get_page(store, &u, &opts).await {
            Ok(e) => e,
            Err(e) => return format!("❌ {e}"),
        };
        let total = entry.lines.len();
        let chars = total + entry.lines.iter().map(String::len).sum::<usize>();
        format!(
            "# Ouvert : {}\nTotal : {total} lignes, {} ({chars} caractères)\nEn cache 10 min. Utilise web_read ou web_grep pour lire.\n\n## Plan (n° de ligne des titres)\n```\n{}\n```",
            entry.url,
            format_bytes(chars),
            extract_outline(&entry.lines)
        )
    }

    /// `toolWebRead`.
    fn tool_read(&self, args: &Map<String, Value>) -> String {
        let u = arg_str(args, "url");
        let Some(entry) = self.find_cached(&u) else {
            return format!("❌ Page absente du cache. Appelle d'abord web_open(\"{u}\").");
        };
        let total = entry.lines.len() as i64;
        let offset = arg_int(args, "offset").unwrap_or(1).max(1);
        let limit = arg_int(args, "limit").unwrap_or(80).clamp(1, 500);
        let start = (offset - 1).min(total);
        let end = start.saturating_add(limit).min(total);
        let slice = &entry.lines[start as usize..end as usize];
        let remaining = total - end;
        let tail = if remaining > 0 {
            format!(" ({remaining} de plus en dessous)")
        } else {
            " (fin de page)".to_string()
        };
        format!(
            "# {}\nLignes {offset}–{end} sur {total}{tail}\n\n```\n{}\n```",
            entry.url,
            format_lines(slice, offset)
        )
    }

    /// `toolWebGrep`.
    fn tool_grep(&self, args: &Map<String, Value>) -> String {
        let u = arg_str(args, "url");
        let pattern = arg_str(args, "pattern");
        let Some(entry) = self.find_cached(&u) else {
            return format!("❌ Page absente du cache. Appelle d'abord web_open(\"{u}\").");
        };
        let expr = format!("(?i){pattern}");
        let Some(re) = crate::chat::stream::compile_go_regex(&expr) else {
            return format!("❌ Regex invalide : {}", go_regexp_error(&expr));
        };
        let ctx = arg_int(args, "context").unwrap_or(2).max(0);
        let max_matches = arg_int(args, "max_matches").unwrap_or(30).max(1);
        let lines = &entry.lines;
        let mut idx: Vec<usize> = Vec::new();
        for (i, l) in lines.iter().enumerate() {
            if idx.len() as i64 >= max_matches {
                break;
            }
            if re.is_match(l) {
                idx.push(i);
            }
        }
        if idx.is_empty() {
            return format!("# {}\nAucun match pour /{pattern}/i", entry.url);
        }
        // Fenêtres de contexte fusionnées quand elles se chevauchent.
        let last = lines.len() as i64 - 1;
        let mut ranges: Vec<(i64, i64)> = Vec::new();
        for &i in &idx {
            let i = i as i64;
            let s = i.saturating_sub(ctx).max(0);
            let e = i.saturating_add(ctx).min(last);
            match ranges.last_mut() {
                Some(r) if s <= r.1 + 1 => r.1 = r.1.max(e),
                _ => ranges.push((s, e)),
            }
        }
        let blocks: Vec<String> = ranges
            .iter()
            .map(|&(s, e)| {
                format!(
                    "```\n{}\n```",
                    format_lines(&lines[s as usize..=e as usize], s + 1)
                )
            })
            .collect();
        let capped = if idx.len() as i64 == max_matches {
            format!(" (plafonné à {max_matches})")
        } else {
            String::new()
        };
        format!(
            "# {}\n{} match(es) pour /{pattern}/i{capped}\n\n{}",
            entry.url,
            idx.len(),
            blocks.join("\n\n---\n\n")
        )
    }

    // --- Cache --------------------------------------------------------------

    /// `findCached` : l'ouverture la plus récente d'une URL, toutes options.
    pub fn find_cached(&self, raw: &str) -> Option<Arc<Entry>> {
        let u = normalize_crawl_url(raw);
        let now = self.now();
        let cache = self.cache.lock().unwrap_or_else(|e| e.into_inner());
        let mut best: Option<&Arc<Entry>> = None;
        for (k, v) in cache.iter() {
            if k == &u || k.starts_with(&format!("{u}#")) {
                if now.saturating_duration_since(v.at) > PAGE_TTL {
                    continue;
                }
                if best.is_none_or(|b| v.at > b.at) {
                    best = Some(v);
                }
            }
        }
        best.cloned()
    }

    /// `getPage` : la page, du cache ou récupérée.
    pub async fn get_page(
        &self,
        store: &Store,
        raw: &str,
        opts: &FetchOpts,
    ) -> Result<Arc<Entry>, String> {
        let u = normalize_crawl_url(raw);
        let key = cache_key_for(&u, opts);
        if !opts.force {
            let cache = self.cache.lock().unwrap_or_else(|e| e.into_inner());
            if let Some(c) = cache.get(&key)
                && self.now().saturating_duration_since(c.at) < PAGE_TTL
            {
                return Ok(c.clone());
            }
        }
        let mut js_code = Vec::new();
        if opts.dismiss_popups {
            js_code.push(AUTO_DISMISS_JS.to_string());
        }
        js_code.extend(opts.actions.iter().cloned());
        let page_timeout_ms = if !opts.wait_for.is_empty() || !opts.actions.is_empty() {
            45000
        } else {
            0
        };
        let cfg = store.config().await;
        let md = if web_engine(&cfg) == ENGINE_GO {
            go_fetch(store, &u).await?
        } else {
            self.run_crwl(
                store,
                &cfg,
                &u,
                &CrwlOpts {
                    js_code,
                    wait_for: opts.wait_for.clone(),
                    page_timeout_ms,
                    raw_markdown: false,
                },
            )
            .await?
        };
        if md.trim().is_empty() {
            return Err("page vide".into());
        }
        let entry = Arc::new(Entry {
            url: u,
            lines: normalize_lines(&md),
            at: self.now(),
        });
        self.cache
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .insert(key, entry.clone());
        Ok(entry)
    }

    // --- Crawl4AI -----------------------------------------------------------

    /// `runCrwl` : POST {base}/crawl, le markdown du premier résultat.
    async fn run_crwl(
        &self,
        store: &Store,
        cfg: &HashMap<String, String>,
        target: &str,
        opts: &CrwlOpts,
    ) -> Result<String, String> {
        let base = crawl4ai_url(cfg);
        if base.is_empty() {
            return Err("aucun serveur Crawl4AI configuré (CRAWL4AI_URL)".into());
        }
        // Clés dans l'ordre où `json.Marshal` écrit une map : triées.
        let mut params = Map::new();
        if !opts.js_code.is_empty() {
            params.insert("js_code".into(), json!(opts.js_code));
        }
        if opts.page_timeout_ms > 0 {
            params.insert("page_timeout".into(), json!(opts.page_timeout_ms));
        }
        if !opts.wait_for.is_empty() {
            params.insert("wait_for".into(), json!(opts.wait_for));
        }
        let body = crate::gojson::marshal(&json!({
            "browser_config": { "params": { "headless": true }, "type": "BrowserConfig" },
            "crawler_config": { "params": params, "type": "CrawlerRunConfig" },
            "urls": [target],
        }));
        let url = format!("{base}/crawl");
        let key = store.get_str(BK_STATE, "crawl_key").await;
        let mut rq = self
            .crawl
            .post(&url)
            .header("Content-Type", "application/json")
            .body(body);
        if !key.is_empty() {
            rq = rq.header("Authorization", format!("Bearer {key}"));
        }
        let call = async {
            let resp = rq.send().await.map_err(|e| {
                format!(
                    "Crawl4AI injoignable ({base}): {}",
                    go_net_err("Post", &url, &e)
                )
            })?;
            let status = resp.status().as_u16();
            let raw = resp.bytes().await.map(|b| b.to_vec()).unwrap_or_default();
            Ok::<_, String>((status, raw))
        };
        let (status, raw) = tokio::time::timeout(CRAWL_TIMEOUT, call)
            .await
            .map_err(|_| {
                format!("Crawl4AI injoignable ({base}): Post \"{url}\": context deadline exceeded")
            })??;
        if status != 200 {
            return Err(format!(
                "Crawl4AI HTTP {status}: {}",
                tail_runes(&go_lossy(&raw), 300)
            ));
        }
        crawl_markdown(&raw, opts.raw_markdown)
    }

    // --- Recherche ----------------------------------------------------------

    /// `duckduckgoSearch`.
    async fn search(
        &self,
        store: &Store,
        query: &str,
        limit: usize,
    ) -> Result<Vec<SearchResult>, String> {
        let cfg = store.config().await;
        if web_engine(&cfg) == ENGINE_GO {
            return go_search(store, query, limit).await;
        }
        let url = format!(
            "https://html.duckduckgo.com/html/?q={}",
            go_query_escape(query.as_bytes())
        );
        let md = self
            .run_crwl(
                store,
                &cfg,
                &url,
                &CrwlOpts {
                    raw_markdown: true,
                    page_timeout_ms: 30000,
                    ..Default::default()
                },
            )
            .await?;
        parse_ddg_markdown(&md, limit)
    }

    /// `duckduckgoImages` : le jeton vqd de la page de recherche, puis i.js.
    async fn images(&self, query: &str, limit: usize) -> Result<Vec<ImageResult>, String> {
        let deadline = tokio::time::Instant::now() + FETCH_TIMEOUT;
        let q = go_query_escape(query.as_bytes());
        let page = format!("{}/?q={q}&iax=images&ia=images", self.ddg_base);
        let body = self.ddg_get(&page, "", deadline).await?;
        let vqd = regex::bytes::Regex::new(r#"vqd=["']?([0-9-]+)"#).expect("regex");
        let Some(m) = vqd.captures(&body) else {
            return Err("jeton DuckDuckGo introuvable (défi anti-bot ?)".into());
        };
        let token = String::from_utf8_lossy(&m[1]).into_owned();
        let api = format!(
            "{}/i.js?l=fr-fr&o=json&f=,,,,,&p=1&q={q}&vqd={token}",
            self.ddg_base
        );
        let body = self
            .ddg_get(&api, "https://duckduckgo.com/", deadline)
            .await?;
        let all = decode_images(&body).map_err(|e| format!("réponse illisible : {e}"))?;
        let mut out: Vec<ImageResult> = Vec::new();
        let mut seen = std::collections::HashSet::new();
        for it in all {
            if it.image.is_empty() || seen.contains(&it.image) || !it.image.starts_with("http") {
                continue;
            }
            seen.insert(it.image.clone());
            out.push(it);
            if out.len() >= limit {
                break;
            }
        }
        Ok(out)
    }

    /// `ddgGet`.
    async fn ddg_get(
        &self,
        target: &str,
        referer: &str,
        deadline: tokio::time::Instant,
    ) -> Result<Vec<u8>, String> {
        let host = url::Url::parse(target)
            .ok()
            .and_then(|u| u.host_str().map(String::from))
            .unwrap_or_default();
        let mut rq = self
            .http
            .get(target)
            .header("User-Agent", UA)
            .header("Accept-Language", "fr-FR,fr;q=0.9,en;q=0.8");
        if !referer.is_empty() {
            rq = rq
                .header("Referer", referer)
                .header("Accept", "application/json, text/javascript, */*; q=0.01");
        }
        let cookies: Vec<String> = self
            .jar
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .iter()
            .filter(|(h, _, _)| *h == host)
            .map(|(_, n, v)| format!("{n}={v}"))
            .collect();
        if !cookies.is_empty() {
            rq = rq.header("Cookie", cookies.join("; "));
        }
        let call = async {
            let resp = rq
                .send()
                .await
                .map_err(|e| format!("échec de la requête : {}", go_net_err("Get", target, &e)))?;
            self.keep_cookies(&host, resp.headers());
            let status = resp.status().as_u16();
            let mut body = resp.bytes().await.map_err(|e| e.to_string())?.to_vec();
            body.truncate(MAX_BYTES);
            Ok::<_, String>((status, body))
        };
        let (status, body) = tokio::time::timeout_at(deadline, call)
            .await
            .map_err(|_| {
                format!("échec de la requête : Get \"{target}\": context deadline exceeded")
            })??;
        if status >= 400 {
            return Err(format!("HTTP {status}"));
        }
        Ok(body)
    }

    /// Les `Set-Cookie` d'une réponse, gardés pour l'hôte (nom=valeur ; le
    /// suivant du même nom remplace, à sa place).
    fn keep_cookies(&self, host: &str, headers: &reqwest::header::HeaderMap) {
        let mut jar = self.jar.lock().unwrap_or_else(|e| e.into_inner());
        for v in headers.get_all("set-cookie") {
            let Ok(v) = v.to_str() else { continue };
            let pair = v.split(';').next().unwrap_or("").trim();
            let Some((name, value)) = pair.split_once('=') else {
                continue;
            };
            let (name, value) = (name.trim().to_string(), value.trim().to_string());
            if name.is_empty() {
                continue;
            }
            match jar.iter_mut().find(|(h, n, _)| h == host && *n == name) {
                Some(c) => c.2 = value,
                None => jar.push((host.to_string(), name, value)),
            }
        }
    }
}

// --- Moteur intégré, au Go ---------------------------------------------------

/// `goFetchMarkdown`, par l'API interne du Go.
async fn go_fetch(store: &Store, u: &str) -> Result<String, String> {
    let v = store
        .internal_json(
            Method::POST,
            "/internal/v1/web/fetch",
            Some(&json!({ "url": u })),
        )
        .await
        .map_err(|e| format!("moteur web intégré injoignable : {e}"))?;
    if let Some(e) = v
        .get("error")
        .and_then(Value::as_str)
        .filter(|e| !e.is_empty())
    {
        return Err(e.to_string());
    }
    Ok(v.get("markdown")
        .and_then(Value::as_str)
        .unwrap_or("")
        .to_string())
}

/// `goSearch`, par l'API interne du Go.
async fn go_search(store: &Store, query: &str, limit: usize) -> Result<Vec<SearchResult>, String> {
    let v = store
        .internal_json(
            Method::POST,
            "/internal/v1/web/search",
            Some(&json!({ "query": query, "limit": limit })),
        )
        .await
        .map_err(|e| format!("moteur web intégré injoignable : {e}"))?;
    if let Some(e) = v
        .get("error")
        .and_then(Value::as_str)
        .filter(|e| !e.is_empty())
    {
        return Err(e.to_string());
    }
    let s = |r: &Value, k: &str| r.get(k).and_then(Value::as_str).unwrap_or("").to_string();
    Ok(v.get("results")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .map(|r| SearchResult {
            title: s(r, "title"),
            url: s(r, "url"),
            snippet: s(r, "snippet"),
        })
        .collect())
}

// --- Fonctions pures ---------------------------------------------------------

/// Texte d'un argument (`args[k].(string)`).
fn arg_str(args: &Map<String, Value>, k: &str) -> String {
    args.get(k)
        .and_then(Value::as_str)
        .unwrap_or("")
        .to_string()
}

/// Entier d'un argument nombre (`int(args[k].(float64))`).
fn arg_int(args: &Map<String, Value>, k: &str) -> Option<i64> {
    args.get(k).and_then(Value::as_f64).map(go_int)
}

/// `int(f)` du Go : tronqué ; hors bornes, l'entier indéfini d'amd64 (ou la
/// saturation d'arm64).
pub fn go_int(f: f64) -> i64 {
    const TWO63: f64 = 9_223_372_036_854_775_808.0;
    if f.is_nan() || !(-TWO63..TWO63).contains(&f) {
        if cfg!(target_arch = "x86_64") {
            return i64::MIN;
        }
        return f as i64;
    }
    f as i64
}

/// `capWebOutput` : le début gardé, la suite à relire par tranches.
pub fn cap_output(s: &str) -> String {
    match s.char_indices().nth(MAX_OUTPUT) {
        None => s.to_string(),
        Some((i, _)) => format!(
            "{}\n…[tronqué : réponse trop longue. Relis par tranches avec web_read(offset, limit) ou cible avec web_grep.]",
            &s[..i]
        ),
    }
}

/// `tailRunes`.
pub fn tail_runes(s: &str, n: usize) -> String {
    let count = s.chars().count();
    if count <= n {
        return s.to_string();
    }
    s.chars().skip(count - n).collect()
}

/// `formatBytes`.
pub fn format_bytes(n: usize) -> String {
    if n < 1024 {
        format!("{n} B")
    } else if n < 1024 * 1024 {
        format!("{:.1} KB", n as f64 / 1024.0)
    } else {
        format!("{:.2} MB", n as f64 / 1024.0 / 1024.0)
    }
}

/// `formatLines` : lignes numérotées à partir de `start`.
pub fn format_lines(lines: &[String], start: i64) -> String {
    let mut b = String::new();
    for (i, l) in lines.iter().enumerate() {
        b.push_str(&format!("{:5} | {l}\n", start + i as i64));
    }
    b.trim_end_matches('\n').to_string()
}

/// `extractOutline` : les titres markdown et leur n° de ligne.
pub fn extract_outline(lines: &[String]) -> String {
    let re = go_re(r"^(#{1,6})\s+(.+?)\s*$");
    let mut out = Vec::new();
    for (i, l) in lines.iter().enumerate() {
        if let Some(m) = re.captures(l) {
            let indent = "  ".repeat(m[1].len() - 1);
            out.push(format!("{:5} | {indent}{}", i + 1, &m[2]));
        }
    }
    if out.is_empty() {
        return "(aucun titre markdown trouvé)".into();
    }
    out.join("\n")
}

/// Une regex écrite pour le Go, traduite.
fn go_re(p: &str) -> regex::Regex {
    crate::chat::stream::compile_go_regex(p).expect("regex du Go")
}

/// `normalizeLines` : \r\n → \n, fins de ligne nettoyées, vides et
/// doublons consécutifs réduits, vides du bord retirés.
pub fn normalize_lines(raw: &str) -> Vec<String> {
    let src = raw.replace("\r\n", "\n");
    let mut out: Vec<String> = Vec::new();
    let mut prev = String::new();
    let mut blank_run = 0;
    for l in src.split('\n') {
        let l = l.trim_end_matches([' ', '\t']);
        if l.trim().is_empty() {
            blank_run += 1;
            if blank_run > 1 {
                continue;
            }
            out.push(String::new());
            prev.clear();
            continue;
        }
        blank_run = 0;
        if l == prev {
            continue;
        }
        out.push(l.to_string());
        prev = l.to_string();
    }
    let start = out.iter().position(|l| !l.is_empty()).unwrap_or(out.len());
    let end = out
        .iter()
        .rposition(|l| !l.is_empty())
        .map_or(start, |e| e + 1);
    out[start..end.max(start)].to_vec()
}

/// `cacheKeyFor` : l'URL, suivie de l'empreinte des options qui changent la
/// page.
pub fn cache_key_for(u: &str, opts: &FetchOpts) -> String {
    let a = if opts.actions.is_empty() {
        Value::Null
    } else {
        json!(opts.actions)
    };
    let fp =
        crate::gojson::marshal(&json!({ "a": a, "d": opts.dismiss_popups, "w": opts.wait_for }));
    if fp == r#"{"a":null,"d":true,"w":""}"# {
        return u.to_string();
    }
    format!("{u}#{}", &hex::encode(md5(fp.as_bytes()))[..8])
}

/// Les morceaux d'une URL lue comme `url.Parse` du Go : (hôte, chemin
/// décodé, requête brute) ; `None` si le Go la refuse.
fn go_url_parts(raw: &str) -> Option<(String, String, String)> {
    if raw.bytes().any(|b| b < 0x20 || b == 0x7f) {
        return None;
    }
    let s = raw.split('#').next().unwrap_or("");
    // Schéma : une lettre, puis lettres, chiffres, « + - . », avant « : ».
    let mut rest = s;
    for (i, c) in s.char_indices() {
        match c {
            'a'..='z' | 'A'..='Z' => {}
            '0'..='9' | '+' | '-' | '.' if i > 0 => {}
            ':' => {
                if i == 0 {
                    return None;
                }
                rest = &s[i + 1..];
                break;
            }
            _ => break,
        }
    }
    let (rest, query) = match rest.split_once('?') {
        Some((r, q)) => (r, q.to_string()),
        None => (rest, String::new()),
    };
    let (host, path) = match rest.strip_prefix("//") {
        Some(a) if !rest.starts_with("///") || s.len() != rest.len() => {
            let end = a.find('/').unwrap_or(a.len());
            let auth = &a[..end];
            let host = auth.rsplit_once('@').map_or(auth, |(_, h)| h);
            (host.to_string(), &a[end..])
        }
        _ => (String::new(), rest),
    };
    let path = percent_decode(path, false)?;
    Some((host, go_lossy(&path), query))
}

/// `%XX` décodés (`+` en espace pour une requête) ; `None` sur un `%` mal
/// formé, comme le Go.
fn percent_decode(s: &str, plus: bool) -> Option<Vec<u8>> {
    let b = s.as_bytes();
    let mut out = Vec::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        match b[i] {
            b'%' => {
                let h = |c: u8| (c as char).to_digit(16);
                let (Some(hi), Some(lo)) = (
                    b.get(i + 1).and_then(|&c| h(c)),
                    b.get(i + 2).and_then(|&c| h(c)),
                ) else {
                    return None;
                };
                out.push((hi * 16 + lo) as u8);
                i += 3;
            }
            b'+' if plus => {
                out.push(b' ');
                i += 1;
            }
            c => {
                out.push(c);
                i += 1;
            }
        }
    }
    Some(out)
}

/// `normalizeCrawlURL` : github.com/owner/repo → son README brut.
pub fn normalize_crawl_url(raw: &str) -> String {
    let Some((host, path, _)) = go_url_parts(raw) else {
        return raw.to_string();
    };
    if host == "github.com" {
        let parts: Vec<&str> = path.trim_matches('/').split('/').collect();
        if parts.len() == 2 {
            return format!(
                "https://raw.githubusercontent.com/{}/{}/HEAD/README.md",
                parts[0], parts[1]
            );
        }
    }
    raw.to_string()
}

/// `decodeHTMLEntities`.
pub fn decode_html_entities(s: &str) -> String {
    const REPL: [(&str, &str); 7] = [
        ("&amp;", "&"),
        ("&lt;", "<"),
        ("&gt;", ">"),
        ("&quot;", "\""),
        ("&#39;", "'"),
        ("&#x2F;", "/"),
        ("&nbsp;", " "),
    ];
    let mut out = String::with_capacity(s.len());
    let mut i = 0;
    'outer: while i < s.len() {
        for (old, new) in REPL {
            if s[i..].starts_with(old) {
                out.push_str(new);
                i += old.len();
                continue 'outer;
            }
        }
        let c = s[i..].chars().next().expect("caractère");
        out.push(c);
        i += c.len_utf8();
    }
    let re = go_re(r"&#(\d+);");
    re.replace_all(&out, |m: &regex::Captures| {
        let n: i64 = m[1].parse().unwrap_or(0);
        if n > 0 {
            char::from_u32(n as i32 as u32)
                .filter(|_| (n as i32) >= 0)
                .unwrap_or('\u{fffd}')
                .to_string()
        } else {
            m[0].to_string()
        }
    })
    .into_owned()
}

/// `decodeUddg` : DuckDuckGo enrobe ses résultats en
/// `//duckduckgo.com/l/?uddg=URL_ENCODÉE`.
pub fn decode_uddg(raw: &str) -> String {
    let s = if raw.starts_with("//") {
        format!("https:{raw}")
    } else {
        raw.to_string()
    };
    let Some((_, _, query)) = go_url_parts(&s) else {
        return s;
    };
    // `u.Query().Get("uddg")` : la première paire lisible.
    let mut q: Option<String> = None;
    for pair in query.split('&') {
        if pair.is_empty() || pair.contains(';') {
            continue;
        }
        let (k, v) = pair.split_once('=').unwrap_or((pair, ""));
        let (Some(k), Some(v)) = (percent_decode(k, true), percent_decode(v, true)) else {
            continue;
        };
        if k == b"uddg" {
            q = Some(go_lossy(&v));
            break;
        }
    }
    match q.filter(|q| !q.is_empty()) {
        Some(q) => match percent_decode(&q, true) {
            Some(dec) => go_lossy(&dec),
            None => s,
        },
        None => s,
    }
}

/// La lecture des résultats DuckDuckGo dans le markdown de Crawl4AI
/// (`duckduckgoSearch`).
pub fn parse_ddg_markdown(md: &str, limit: usize) -> Result<Vec<SearchResult>, String> {
    if md.contains("anomaly-modal") || md.contains("anomaly.js") {
        return Err("DuckDuckGo a renvoyé un défi anti-bot".into());
    }
    let head = go_re(r"^##\s+\[([^\]]+)\]\(([^)]+)\)\s*$");
    let link = go_re(r"\[([^\]]+)\]\([^)]+\)");
    let urlish = go_re(r"^[\w.-]+\.[a-z]{2,}");
    let lines: Vec<&str> = md.split('\n').collect();
    let mut results: Vec<SearchResult> = Vec::new();
    let mut i = 0;
    while i < lines.len() && results.len() < limit {
        let Some(h) = head.captures(lines[i]) else {
            i += 1;
            continue;
        };
        let title = decode_html_entities(&h[1])
            .replace("**", "")
            .trim()
            .to_string();
        let u = decode_uddg(&h[2]);
        if title.is_empty() || u.is_empty() || u.contains("duckduckgo.com") {
            i += 1;
            continue;
        }
        let mut snippet = String::new();
        let mut j = i + 1;
        while j < i + 6 && j < lines.len() {
            let ln = lines[j].trim();
            j += 1;
            if ln.is_empty() {
                continue;
            }
            for lm in link.captures_iter(ln) {
                let text = lm[1].trim();
                if text.is_empty() || text.starts_with('!') || urlish.is_match(text) {
                    continue;
                }
                snippet = decode_html_entities(text)
                    .replace("**", "")
                    .trim()
                    .to_string();
                break;
            }
            if !snippet.is_empty() {
                break;
            }
        }
        if !results.iter().any(|r| r.url == u) {
            results.push(SearchResult {
                title,
                url: u,
                snippet,
            });
        }
        i += 1;
    }
    Ok(results)
}

/// Le markdown du premier résultat d'une réponse de Crawl4AI : `{results:
/// [...]}` ou un résultat seul, lus comme `json.Unmarshal` du Go.
pub fn crawl_markdown(raw: &[u8], prefer_raw: bool) -> Result<String, String> {
    let v = go_unmarshal_value(raw).ok();
    // `{results:[…]}` d'abord.
    let mut results: Option<Vec<CrwlResult>> = None;
    let mut results_ok = false;
    if let Some(v) = &v {
        match v {
            Value::Object(o) => match go_field(o, "results") {
                None | Some(Value::Null) => results_ok = true,
                Some(Value::Array(a)) => {
                    let mut ok = true;
                    let list: Vec<CrwlResult> = a
                        .iter()
                        .map(|e| {
                            let (r, good) = CrwlResult::from(e);
                            ok &= good;
                            r
                        })
                        .collect();
                    results_ok = ok;
                    results = Some(list);
                }
                Some(_) => {}
            },
            Value::Null => results_ok = true,
            _ => {}
        }
    }
    let mut list = results.unwrap_or_default();
    if !results_ok || list.is_empty() {
        // Un résultat seul.
        let single = match &v {
            Some(Value::Null) => Some(CrwlResult::default()),
            Some(o @ Value::Object(_)) => {
                let (r, good) = CrwlResult::from(o);
                good.then_some(r)
            }
            _ => None,
        };
        if let Some(s) = single {
            list = vec![s];
        }
    }
    let Some(r) = list.into_iter().next() else {
        return Err("Crawl4AI : réponse vide".into());
    };
    if !r.success {
        let msg = if r.error_message.is_empty() {
            "échec du crawl".to_string()
        } else {
            r.error_message
        };
        return Err(format!("Crawl4AI : {}", tail_runes(&msg, 300)));
    }
    Ok(r.markdown(prefer_raw))
}

/// `crwlResult`.
#[derive(Default)]
struct CrwlResult {
    success: bool,
    error_message: String,
    markdown: Option<Value>,
}

impl CrwlResult {
    /// Le résultat, et faux si un champ a le mauvais type (erreur du Go).
    fn from(v: &Value) -> (Self, bool) {
        let mut r = CrwlResult::default();
        let Value::Object(o) = v else {
            return (r, v.is_null());
        };
        let mut ok = true;
        match go_field(o, "success") {
            None | Some(Value::Null) => {}
            Some(Value::Bool(b)) => r.success = *b,
            Some(_) => ok = false,
        }
        match go_field(o, "error_message") {
            None | Some(Value::Null) => {}
            Some(Value::String(s)) => r.error_message = s.clone(),
            Some(_) => ok = false,
        }
        r.markdown = go_field(o, "markdown").cloned();
        (r, ok)
    }

    /// `crwlResult.markdown` : une chaîne, ou `{raw_markdown, fit_markdown}`.
    fn markdown(&self, prefer_raw: bool) -> String {
        match &self.markdown {
            None | Some(Value::Null) => String::new(),
            Some(Value::String(s)) => s.clone(),
            Some(Value::Object(o)) => {
                let field = |k: &str| match go_field(o, k) {
                    None | Some(Value::Null) => Some(String::new()),
                    Some(Value::String(s)) => Some(s.clone()),
                    Some(_) => None,
                };
                let (Some(raw), Some(fit)) = (field("raw_markdown"), field("fit_markdown")) else {
                    return String::new();
                };
                if prefer_raw {
                    if !raw.is_empty() { raw } else { fit }
                } else if !fit.is_empty() {
                    fit
                } else {
                    raw
                }
            }
            Some(_) => String::new(),
        }
    }
}

/// Un champ d'objet comme le cherche `encoding/json` : le nom exact, sinon
/// sans casse ; la dernière occurrence l'emporte (déjà vrai d'un objet lu
/// par serde_json).
fn go_field<'a>(o: &'a Map<String, Value>, name: &str) -> Option<&'a Value> {
    o.get(name).or_else(|| {
        o.iter()
            .filter(|(k, _)| k.eq_ignore_ascii_case(name))
            .map(|(_, v)| v)
            .next_back()
    })
}

/// `json.Unmarshal` dans une valeur : erreurs de syntaxe au texte du Go.
pub fn go_unmarshal_value(raw: &[u8]) -> Result<Value, String> {
    let v = match crate::gojson::first_value(raw) {
        Ok(v) => v,
        Err(e) if e == "EOF" || e == "unexpected EOF" => {
            return Err("unexpected end of JSON input".into());
        }
        Err(e) => return Err(e),
    };
    let start = v.as_ptr() as usize - raw.as_ptr() as usize;
    if let Some(&c) = raw[start + v.len()..]
        .iter()
        .find(|c| !matches!(c, b' ' | b'\t' | b'\r' | b'\n'))
    {
        return Err(format!(
            "invalid character {} after top-level value",
            crate::gojson::quote_char(c)
        ));
    }
    serde_json::from_slice(v).map_err(|e| e.to_string())
}

/// La réponse de `i.js`, lue comme `json.Unmarshal` dans
/// `struct{ Results []imageResult }` : les erreurs de type au texte du Go
/// (la première), après avoir tout lu.
pub fn decode_images(raw: &[u8]) -> Result<Vec<ImageResult>, String> {
    let v = go_unmarshal_value(raw)?;
    let mut err: Option<String> = None;
    let mut fail = |e: String| {
        if err.is_none() {
            err = Some(e);
        }
    };
    let mut out = Vec::new();
    match &v {
        Value::Object(o) => match go_field(o, "results") {
            None | Some(Value::Null) => {}
            Some(Value::Array(a)) => {
                for e in a {
                    let mut r = ImageResult::default();
                    match e {
                        Value::Null => {}
                        Value::Object(f) => {
                            for (name, slot) in [("image", 0), ("title", 1), ("url", 2)] {
                                match go_field(f, name) {
                                    None | Some(Value::Null) => {}
                                    Some(Value::String(s)) => match slot {
                                        0 => r.image = s.clone(),
                                        1 => r.title = s.clone(),
                                        _ => r.url = s.clone(),
                                    },
                                    Some(other) => fail(format!(
                                        "json: cannot unmarshal {} into Go struct field imageResult.results.{name} of type string",
                                        go_kind(other)
                                    )),
                                }
                            }
                            for (name, wide) in [("width", true), ("height", false)] {
                                match go_field(f, name) {
                                    None | Some(Value::Null) => {}
                                    Some(Value::Number(n)) => match n.as_i64() {
                                        Some(i) if !n.to_string().contains(['.', 'e', 'E']) => {
                                            if wide {
                                                r.width = i
                                            } else {
                                                r.height = i
                                            }
                                        }
                                        _ => fail(format!(
                                            "json: cannot unmarshal number {n} into Go struct field imageResult.results.{name} of type int"
                                        )),
                                    },
                                    Some(other) => fail(format!(
                                        "json: cannot unmarshal {} into Go struct field imageResult.results.{name} of type int",
                                        go_kind(other)
                                    )),
                                }
                            }
                        }
                        other => fail(format!(
                            "json: cannot unmarshal {} into Go struct field .results of type loki.imageResult",
                            go_kind(other)
                        )),
                    }
                    out.push(r);
                }
            }
            Some(other) => fail(format!(
                "json: cannot unmarshal {} into Go struct field .results of type []loki.imageResult",
                go_kind(other)
            )),
        },
        Value::Null => {}
        other => fail(format!(
            "json: cannot unmarshal {} into Go value of type struct {{ Results []loki.imageResult \"json:\\\"results\\\"\" }}",
            go_kind(other)
        )),
    }
    match err {
        Some(e) => Err(e),
        None => Ok(out),
    }
}

/// Le nom d'une valeur JSON dans les erreurs du Go.
fn go_kind(v: &Value) -> &'static str {
    match v {
        Value::Null => "null",
        Value::Bool(_) => "bool",
        Value::Number(_) => "number",
        Value::String(_) => "string",
        Value::Array(_) => "array",
        Value::Object(_) => "object",
    }
}

/// L'erreur d'un client HTTP du Go (`*url.Error`), au mieux.
fn go_net_err(method: &str, url: &str, e: &reqwest::Error) -> String {
    if e.is_timeout() {
        return format!("{method} \"{url}\": context deadline exceeded");
    }
    if e.is_redirect() {
        return format!("{method} \"{url}\": trop de redirections");
    }
    if e.is_connect() {
        let mut src: Option<&(dyn std::error::Error + 'static)> = Some(e);
        while let Some(s) = src {
            if let Some(io) = s.downcast_ref::<std::io::Error>()
                && io.kind() == std::io::ErrorKind::ConnectionRefused
            {
                let addr = url::Url::parse(url)
                    .ok()
                    .and_then(|u| {
                        let port = u.port_or_known_default()?;
                        Some(match u.host()? {
                            url::Host::Ipv6(a) => format!("[{a}]:{port}"),
                            h => format!("{h}:{port}"),
                        })
                    })
                    .unwrap_or_default();
                return format!("{method} \"{url}\": dial tcp {addr}: connect: connection refused");
            }
            src = s.source();
        }
    }
    format!("{method} \"{url}\": {e}")
}

/// L'erreur de `regexp.Compile` du Go pour les fautes courantes (parenthèse,
/// crochet, répétition sans argument ou imbriquée, échappement inconnu) ;
/// sinon « Perl non pris en charge ».
pub fn go_regexp_error(expr: &str) -> String {
    let fmt = |code: &str, frag: &str| format!("error parsing regexp: {code}: `{frag}`");
    let cs: Vec<char> = expr.chars().collect();
    let mut depth = 0i32;
    // Peut-on répéter ce qui précède ? (début, après « ( » ou « | » : non)
    let mut can_repeat = false;
    // Le précédent était-il déjà une répétition (et son début) ?
    let mut last_repeat: Option<usize> = None;
    let mut i = 0;
    while i < cs.len() {
        let c = cs[i];
        match c {
            '\\' => {
                let Some(&n) = cs.get(i + 1) else {
                    return fmt("trailing backslash at end of expression", "");
                };
                let ok = n.is_ascii_punctuation()
                    || matches!(
                        n,
                        'a' | 'f'
                            | 't'
                            | 'n'
                            | 'r'
                            | 'v'
                            | 'd'
                            | 'D'
                            | 's'
                            | 'S'
                            | 'w'
                            | 'W'
                            | 'b'
                            | 'B'
                            | 'A'
                            | 'z'
                            | 'p'
                            | 'P'
                            | 'Q'
                            | 'E'
                            | 'x'
                            | 'C'
                            | '0'..='7'
                    );
                if !ok || ('1'..='7').contains(&n) {
                    return fmt("invalid escape sequence", &format!("\\{n}"));
                }
                i += 2;
                can_repeat = true;
                last_repeat = None;
                continue;
            }
            '[' => {
                // Jusqu'au « ] » fermant (un « ] » en tête est littéral).
                let start = i;
                let mut j = i + 1;
                if cs.get(j) == Some(&'^') {
                    j += 1;
                }
                if cs.get(j) == Some(&']') {
                    j += 1;
                }
                let mut closed = false;
                while j < cs.len() {
                    match cs[j] {
                        '\\' => j += 2,
                        '[' if cs.get(j + 1) == Some(&':') => {
                            match (j + 2..cs.len().saturating_sub(1))
                                .find(|&k| cs[k] == ':' && cs[k + 1] == ']')
                            {
                                Some(k) => j = k + 2,
                                None => j += 1,
                            }
                        }
                        ']' => {
                            closed = true;
                            break;
                        }
                        _ => j += 1,
                    }
                }
                if !closed {
                    return fmt("missing closing ]", &cs[start..].iter().collect::<String>());
                }
                i = j + 1;
                can_repeat = true;
                last_repeat = None;
                continue;
            }
            '(' => {
                depth += 1;
                if cs.get(i + 1) == Some(&'?') {
                    // Drapeaux seuls « (?i) » : rien à répéter ensuite.
                    let k = cs[i..].iter().position(|&c| c == ')' || c == ':');
                    if let Some(k) = k
                        && cs[i + k] == ')'
                    {
                        depth -= 1;
                        i += k + 1;
                        can_repeat = false;
                        last_repeat = None;
                        continue;
                    }
                    i += k.map_or(2, |k| k + 1);
                } else {
                    i += 1;
                }
                can_repeat = false;
                last_repeat = None;
                continue;
            }
            ')' => {
                if depth == 0 {
                    return fmt("unexpected )", expr);
                }
                depth -= 1;
                can_repeat = true;
                last_repeat = None;
            }
            '|' => {
                can_repeat = false;
                last_repeat = None;
            }
            '*' | '+' | '?' => {
                if let Some(start) = last_repeat {
                    // « ? » après une répétition la rend paresseuse, une fois.
                    let prev = cs[i - 1];
                    if !(c == '?' && prev != '?' && i - start == 1) {
                        return fmt(
                            "invalid nested repetition operator",
                            &cs[start..=i].iter().collect::<String>(),
                        );
                    }
                    i += 1;
                    continue;
                }
                if !can_repeat {
                    return fmt("missing argument to repetition operator", &c.to_string());
                }
                last_repeat = Some(i);
                i += 1;
                continue;
            }
            _ => {
                can_repeat = true;
                last_repeat = None;
            }
        }
        i += 1;
    }
    if depth > 0 {
        return fmt("missing closing )", expr);
    }
    fmt("invalid or unsupported Perl syntax", expr)
}

/// MD5 (RFC 1321), pour l'empreinte des options du cache.
pub fn md5(data: &[u8]) -> [u8; 16] {
    const S: [u32; 64] = [
        7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 5, 9, 14, 20, 5, 9, 14, 20, 5,
        9, 14, 20, 5, 9, 14, 20, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 6, 10,
        15, 21, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21,
    ];
    const K: [u32; 64] = [
        0xd76aa478, 0xe8c7b756, 0x242070db, 0xc1bdceee, 0xf57c0faf, 0x4787c62a, 0xa8304613,
        0xfd469501, 0x698098d8, 0x8b44f7af, 0xffff5bb1, 0x895cd7be, 0x6b901122, 0xfd987193,
        0xa679438e, 0x49b40821, 0xf61e2562, 0xc040b340, 0x265e5a51, 0xe9b6c7aa, 0xd62f105d,
        0x02441453, 0xd8a1e681, 0xe7d3fbc8, 0x21e1cde6, 0xc33707d6, 0xf4d50d87, 0x455a14ed,
        0xa9e3e905, 0xfcefa3f8, 0x676f02d9, 0x8d2a4c8a, 0xfffa3942, 0x8771f681, 0x6d9d6122,
        0xfde5380c, 0xa4beea44, 0x4bdecfa9, 0xf6bb4b60, 0xbebfbc70, 0x289b7ec6, 0xeaa127fa,
        0xd4ef3085, 0x04881d05, 0xd9d4d039, 0xe6db99e5, 0x1fa27cf8, 0xc4ac5665, 0xf4292244,
        0x432aff97, 0xab9423a7, 0xfc93a039, 0x655b59c3, 0x8f0ccc92, 0xffeff47d, 0x85845dd1,
        0x6fa87e4f, 0xfe2ce6e0, 0xa3014314, 0x4e0811a1, 0xf7537e82, 0xbd3af235, 0x2ad7d2bb,
        0xeb86d391,
    ];
    let (mut a0, mut b0, mut c0, mut d0) = (
        0x6745_2301u32,
        0xefcd_ab89u32,
        0x98ba_dcfeu32,
        0x1032_5476u32,
    );
    let mut msg = data.to_vec();
    let bits = (data.len() as u64).wrapping_mul(8);
    msg.push(0x80);
    while msg.len() % 64 != 56 {
        msg.push(0);
    }
    msg.extend_from_slice(&bits.to_le_bytes());
    for chunk in msg.chunks(64) {
        let m: Vec<u32> = chunk
            .chunks(4)
            .map(|w| u32::from_le_bytes([w[0], w[1], w[2], w[3]]))
            .collect();
        let (mut a, mut b, mut c, mut d) = (a0, b0, c0, d0);
        for i in 0..64 {
            let (f, g) = match i / 16 {
                0 => ((b & c) | (!b & d), i),
                1 => ((d & b) | (!d & c), (5 * i + 1) % 16),
                2 => (b ^ c ^ d, (3 * i + 5) % 16),
                _ => (c ^ (b | !d), (7 * i) % 16),
            };
            let f = f.wrapping_add(a).wrapping_add(K[i]).wrapping_add(m[g]);
            a = d;
            d = c;
            c = b;
            b = b.wrapping_add(f.rotate_left(S[i]));
        }
        a0 = a0.wrapping_add(a);
        b0 = b0.wrapping_add(b);
        c0 = c0.wrapping_add(c);
        d0 = d0.wrapping_add(d);
    }
    let mut out = [0u8; 16];
    for (i, w) in [a0, b0, c0, d0].iter().enumerate() {
        out[i * 4..i * 4 + 4].copy_from_slice(&w.to_le_bytes());
    }
    out
}

#[cfg(test)]
mod tests;
