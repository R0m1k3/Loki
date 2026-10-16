//! Interrupteurs système tenus par la façade (lot 17 de la migration,
//! `LOKI_SYSTEM=rust` ; expérimental, le Go reste le défaut) :
//!
//! - `/api/internet` — port de `handleInternet` (`web_api.go`) et de
//!   `chat_internet.go` / `web_fetch_go.go` : moteur web, serveur Crawl4AI et
//!   sa clé (jamais rendue), joignabilité avec son cache de 30 s ;
//! - `/api/computer` — `handleComputer`, `chromePath`, `visionEnabled` ;
//! - `/api/apikey` — `handleAPIKey` et `backend_config.go` : clé de
//!   complétion, repli sur `config.env`, adresse à coller dans un client tiers
//!   (`oaiBaseURL`) ;
//! - `/api/network` — `handleNetwork`, `sys_network.go` (en conteneur Linux,
//!   aucun pare-feu à piloter : « inconnu »).
//!
//! Les réglages restent des clés de la base. Ce qui vit dans le processus Go
//! reste au Go : son cache de joignabilité (vidé par
//! `/internal/v1/web/reach/reset`), le Chromium qu'il pilote quand la façade
//! ne tient pas la conversation (`/internal/v1/cdp/shutdown` ; sinon le
//! navigateur est celui de la façade, lot 44) et le redémarrage du moteur
//! quand la façade ne le pilote pas (`/internal/v1/service/action`).

use std::time::{Duration, Instant};

use http_body_util::{BodyExt, Limited};
use hyper::body::Incoming;
use hyper::header::HOST;
use hyper::{Method, Request, Response, StatusCode};
use serde_json::{Value, json};

use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::gojson::{self, Kind};
use crate::proxy::Body;
use crate::serve::{self, Cfg};
use crate::store::{BK_CONFIG, BK_STATE, Store};

/// Moteur web intégré (`engineGo`).
pub const ENGINE_GO: &str = "go";
/// Serveur Crawl4AI (`engineCrawl`).
pub const ENGINE_CRAWL: &str = "crawl4ai";
/// Durée de vie du cache de joignabilité (`reachTTL`).
pub const REACH_TTL: Duration = Duration::from_secs(30);

/// État tenu par la façade pour ce lot.
pub struct System {
    /// Binaire du navigateur imposé (tests) ; `None` : [`chrome_path`].
    pub chrome: Option<String>,
    reach: tokio::sync::Mutex<Reach>,
    client: reqwest::Client,
}

#[derive(Default)]
struct Reach {
    ok: bool,
    url: String,
    when: Option<Instant>,
}

impl Default for System {
    fn default() -> Self {
        Self::new()
    }
}

impl System {
    pub fn new() -> Self {
        crate::chat::engine::install_crypto();
        System {
            chrome: None,
            reach: tokio::sync::Mutex::new(Reach::default()),
            client: reqwest::Client::builder().build().unwrap_or_default(),
        }
    }

    /// Le cache de joignabilité est périmé : ici, et dans le Go (outils web).
    async fn reach_reset(&self, store: &Store) {
        self.reach.lock().await.url.clear();
        if let Err(e) = store
            .internal_post("/internal/v1/web/reach/reset", &json!({}))
            .await
        {
            eprintln!("[internet] cache de joignabilité du Go : {e}");
        }
    }

    /// `crawlReachable` : le serveur Crawl4AI répond-il (moins de 500 sur
    /// `/health` ou `/`, deux secondes en tout) ? Le moteur intégré n'a rien à
    /// joindre.
    pub(crate) async fn reachable(&self, cfg: &Cfg, key: &str) -> bool {
        if web_engine(cfg) == ENGINE_GO {
            return true;
        }
        let base = crawl4ai_url(cfg);
        if base.is_empty() {
            return false;
        }
        let mut r = self.reach.lock().await;
        if base == r.url && r.when.is_some_and(|w| w.elapsed() < REACH_TTL) {
            return r.ok;
        }
        let probe = async {
            for path in ["/health", "/"] {
                let mut rq = self.client.get(format!("{base}{path}"));
                if !key.is_empty() {
                    rq = rq.header("Authorization", format!("Bearer {key}"));
                }
                if let Ok(resp) = rq.send().await
                    && resp.status().as_u16() < 500
                {
                    return true;
                }
            }
            false
        };
        let ok = tokio::time::timeout(Duration::from_secs(2), probe)
            .await
            .unwrap_or(false);
        *r = Reach {
            ok,
            url: base,
            when: Some(Instant::now()),
        };
        ok
    }
}

// --- Fonctions pures ---------------------------------------------------------

/// `crawl4aiURL` : sans slash final, vide si non configurée.
pub fn crawl4ai_url(cfg: &Cfg) -> String {
    serve::trim(serve::get(cfg, "CRAWL4AI_URL"))
        .trim_end_matches('/')
        .to_string()
}

/// `webEngine` : le moteur choisi ; sinon Crawl4AI si une URL est déjà là
/// (installation d'avant le choix), le moteur intégré par défaut.
pub fn web_engine(cfg: &Cfg) -> &'static str {
    match serve::trim(serve::get(cfg, "WEB_ENGINE"))
        .to_lowercase()
        .as_str()
    {
        ENGINE_GO => return ENGINE_GO,
        ENGINE_CRAWL => return ENGINE_CRAWL,
        _ => {}
    }
    if crawl4ai_url(cfg).is_empty() {
        ENGINE_GO
    } else {
        ENGINE_CRAWL
    }
}

/// `internetEnabled` : interrupteur armé ET moteur utilisable.
pub fn internet_enabled(cfg: &Cfg, switch_on: bool) -> bool {
    if web_engine(cfg) == ENGINE_CRAWL && crawl4ai_url(cfg).is_empty() {
        return false;
    }
    switch_on
}

/// Indice de la clé Crawl4AI : ses quatre derniers caractères.
pub fn key_hint(key: &str) -> String {
    let runes: Vec<char> = key.chars().collect();
    if runes.len() > 4 {
        format!("…{}", runes[runes.len() - 4..].iter().collect::<String>())
    } else if !key.is_empty() {
        "…".into()
    } else {
        String::new()
    }
}

/// Octets → texte comme l'encodeur JSON du Go : chaque octet invalide devient
/// U+FFFD.
pub(crate) fn go_lossy(mut b: &[u8]) -> String {
    let mut out = String::new();
    loop {
        match std::str::from_utf8(b) {
            Ok(s) => {
                out.push_str(s);
                return out;
            }
            Err(e) => {
                let (ok, rest) = b.split_at(e.valid_up_to());
                out.push_str(std::str::from_utf8(ok).unwrap_or_default());
                let bad = e.error_len().unwrap_or(rest.len());
                out.extend(std::iter::repeat_n('\u{FFFD}', bad));
                b = &rest[bad..];
            }
        }
    }
}

/// `maskAPIKey` : préfixe « sk-loki-? » et quatre derniers octets ; une clé de
/// treize octets au plus ne montre que les deux derniers, une clé d'un ou deux
/// octets rien.
pub fn mask_api_key(k: &str) -> String {
    let b = k.as_bytes();
    match b.len() {
        0 => String::new(),
        1 | 2 => "…".into(),
        n if n <= 13 => format!("…{}", go_lossy(&b[n - 2..])),
        n => format!("{}…{}", go_lossy(&b[..9]), go_lossy(&b[n - 4..])),
    }
}

/// `genAPIKey`.
pub fn gen_api_key() -> String {
    let mut b = [0u8; 24];
    let _ = getrandom::fill(&mut b);
    format!("sk-loki-{}", hex::encode(b))
}

/// `visionEnabled` : un projecteur multimodal est-il configuré ?
pub fn vision_enabled(cfg: &Cfg) -> bool {
    if serve::is_external(cfg) {
        return serve::trim(serve::get(cfg, "EXTERNAL_VISION")) == "1";
    }
    if serve::is_strata(cfg) && serve::get(cfg, "STRATA_VISION") != "0" {
        return true;
    }
    !serve::trim(serve::get(cfg, "MMPROJ")).is_empty()
}

/// `playwrightChromium` : le Chromium déposé par Playwright, le plus récent.
pub fn playwright_chromium(browsers_env: Option<&str>, home: Option<&str>) -> String {
    let mut roots = Vec::new();
    if let Some(p) = browsers_env
        .map(str::trim)
        .filter(|p| !p.is_empty() && *p != "0")
    {
        roots.push(p.to_string());
    }
    roots.push("/opt/pw-browsers".into());
    if let Some(h) = home.filter(|h| !h.is_empty()) {
        roots.push(crate::paths::go_join(&[h, ".cache", "ms-playwright"]));
    }
    for root in roots {
        let Ok(rd) = std::fs::read_dir(&root) else {
            continue;
        };
        let mut dirs: Vec<String> = rd
            .filter_map(|e| e.ok())
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .filter(|n| {
                n.strip_prefix("chromium")
                    .is_some_and(|rest| rest.contains('-'))
            })
            .map(|n| crate::paths::go_join(&[&root, &n]))
            .collect();
        dirs.sort();
        for d in dirs.iter().rev() {
            for rel in [
                ["chrome-linux", "chrome"],
                ["chrome-linux", "headless_shell"],
            ] {
                let p = crate::paths::go_join(&[d, rel[0], rel[1]]);
                if std::fs::metadata(&p).is_ok_and(|m| !m.is_dir()) {
                    return p;
                }
            }
        }
    }
    String::new()
}

/// `chromePath` : `LOKI_CHROME`, le Chromium de Playwright, puis le PATH.
pub fn chrome_path() -> String {
    let env = |k: &str| std::env::var(k).ok();
    if let Some(p) = env("LOKI_CHROME")
        .map(|p| p.trim().to_string())
        .filter(|p| !p.is_empty())
    {
        return p;
    }
    let p = playwright_chromium(
        env("PLAYWRIGHT_BROWSERS_PATH").as_deref(),
        env("HOME").as_deref(),
    );
    if !p.is_empty() {
        return p;
    }
    for n in [
        "google-chrome",
        "google-chrome-stable",
        "chromium",
        "chromium-browser",
        "microsoft-edge",
        "chrome",
    ] {
        if let Some(p) = crate::paths::look_path(n) {
            return p;
        }
    }
    String::new()
}

/// `engineHost`.
pub fn engine_host(cfg: &Cfg) -> String {
    let h = serve::trim(serve::get(cfg, "HOST"));
    if h.is_empty() {
        "0.0.0.0".into()
    } else {
        h.into()
    }
}

/// `lanExposed`.
pub fn lan_exposed(cfg: &Cfg) -> bool {
    let h = engine_host(cfg);
    h != "127.0.0.1" && h != "::1" && !h.eq_ignore_ascii_case("localhost")
}

/// `localIP` : l'adresse par laquelle cette machine sort sur le réseau.
pub fn local_ip() -> String {
    let s = match std::net::UdpSocket::bind("0.0.0.0:0") {
        Ok(s) => s,
        Err(_) => return "localhost".into(),
    };
    if s.connect("8.8.8.8:80").is_err() {
        return "localhost".into();
    }
    s.local_addr()
        .map(|a| a.ip().to_string())
        .unwrap_or_else(|_| "localhost".into())
}

/// `networkStatus` (aucun pare-feu piloté hors Windows).
pub fn network_status(cfg: &Cfg) -> Value {
    let port = crate::engstatus::llm_port(cfg);
    json!({
        "exposed": lan_exposed(cfg),
        "host": engine_host(cfg),
        "port": port,
        "url": format!("http://{}:{port}/v1", local_ip()),
        "firewall": "inconnu",
        "hint": "",
    })
}

/// `requestBase` : « schéma://hôte » de la requête (la façade ne termine pas
/// le TLS : le schéma ne vient que de X-Forwarded-Proto).
pub fn request_base(headers: &hyper::HeaderMap, uri: &hyper::Uri) -> String {
    let mut scheme = "http".to_string();
    if let Some(fwd) = headers
        .get("x-forwarded-proto")
        .and_then(|v| v.to_str().ok())
        .filter(|v| !v.is_empty())
    {
        let first = fwd.split(',').next().unwrap_or("").trim().to_lowercase();
        if first == "http" || first == "https" {
            scheme = first;
        }
    }
    let host = headers
        .get(HOST)
        .and_then(|v| v.to_str().ok())
        .map(str::to_string)
        .or_else(|| uri.authority().map(|a| a.to_string()))
        .filter(|h| !h.is_empty())
        .unwrap_or_else(|| "localhost".into());
    format!("{scheme}://{host}")
}

// --- Routes ------------------------------------------------------------------

/// Les routes servies par ce module.
pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/internet" | "/api/computer" | "/api/apikey" | "/api/network"
    )
}

/// `putStr` : une chaîne vide efface la clé.
async fn put_str(store: &Store, bucket: &str, key: &str, val: &str) -> Result<(), String> {
    if val.is_empty() {
        store.del(bucket, key).await
    } else {
        store.put(bucket, key, val.as_bytes().to_vec()).await
    }
}

/// `putBool`.
async fn put_bool(store: &Store, key: &str, on: bool) -> Result<(), String> {
    put_str(store, BK_STATE, key, if on { "1" } else { "" }).await
}

async fn get_bool(store: &Store, key: &str) -> bool {
    store.get_str(BK_STATE, key).await == "1"
}

fn fail(code: StatusCode, e: impl Into<String>) -> Response<Body> {
    send_json(code, &json!({ "ok": false, "error": e.into() }))
}

/// Le verrou de l'optimiseur (au Go) : `Some(réponse 409)` s'il tient le
/// moteur.
async fn tune_deny(store: &Store) -> Option<Response<Body>> {
    let v = store
        .internal_json(Method::GET, "/internal/v1/tune/guard", None)
        .await
        .ok()?;
    if v.get("busy").and_then(Value::as_bool) == Some(true) {
        return Some(send_json(
            StatusCode::CONFLICT,
            &json!({ "ok": false, "tuning": true, "error": v.get("error").and_then(Value::as_str).unwrap_or("") }),
        ));
    }
    None
}

/// La clé ne s'applique qu'au redémarrage du moteur : relancé s'il tourne,
/// par la façade si elle le pilote, sinon par le Go.
async fn restart_if_active(ctx: &Ctx) {
    if let Some(ctl) = &ctx.engine_ctl {
        if crate::enginectl::service_is_active(ctx) {
            let _ = crate::enginectl::service_action(ctx, ctl, "restart").await;
        }
        return;
    }
    if let Err(e) = ctx
        .store
        .internal_post(
            "/internal/v1/service/action",
            &json!({ "action": "restart", "if_active": true }),
        )
        .await
    {
        eprintln!("[clé] redémarrage du moteur : {e}");
    }
}

const INTERNET_REQ_TYPE: &str = r#"struct { Enabled *bool "json:\"enabled\""; URL *string "json:\"url\""; Key *string "json:\"key\""; Engine *string "json:\"engine\"" }"#;
const COMPUTER_REQ_TYPE: &str = r#"struct { On bool "json:\"on\"" }"#;
const APIKEY_REQ_TYPE: &str =
    r#"struct { Action string "json:\"action\""; Key string "json:\"key\"" }"#;
const NETWORK_REQ_TYPE: &str = r#"struct { Exposed *bool "json:\"exposed\"" }"#;

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(
    ctx: &Ctx,
    sys: &System,
    req: Request<Incoming>,
    from_go: bool,
) -> Response<Body> {
    let (parts, body) = req.into_parts();
    if !from_go && let Err(refus) = ctx.guard(&parts.headers).await {
        return *refus;
    }
    let post = parts.method == Method::POST;
    let raw = if post {
        Limited::new(body, 1 << 20)
            .collect()
            .await
            .map(|b| b.to_bytes().to_vec())
            .unwrap_or_default()
    } else {
        Vec::new()
    };
    let store = &ctx.store;
    match parts.uri.path() {
        "/api/internet" => {
            if post {
                let d = gojson::decode_struct(
                    &raw,
                    INTERNET_REQ_TYPE,
                    &[
                        ("enabled", Kind::OptBool),
                        ("url", Kind::OptStr),
                        ("key", Kind::OptStr),
                        ("engine", Kind::OptStr),
                    ],
                );
                if let Some(engine) = d.opt_str("engine") {
                    let e = engine.trim().to_lowercase();
                    if e != ENGINE_GO && e != ENGINE_CRAWL {
                        return fail(
                            StatusCode::BAD_REQUEST,
                            format!("moteur web inconnu : {}", gojson::quote(&e)),
                        );
                    }
                    if let Err(err) = store.put(BK_CONFIG, "WEB_ENGINE", e.into_bytes()).await {
                        return fail(StatusCode::BAD_REQUEST, err);
                    }
                    sys.reach_reset(store).await;
                }
                if let Some(key) = d.opt_str("key") {
                    // Rien ne traîne dans la configuration.
                    let _ = store.del(BK_CONFIG, "CRAWL4AI_KEY").await;
                    if let Err(err) = put_str(store, BK_STATE, "crawl_key", key.trim()).await {
                        return fail(StatusCode::INTERNAL_SERVER_ERROR, err);
                    }
                    sys.reach_reset(store).await;
                }
                if let Some(u) = d.opt_str("url") {
                    let u = u.trim().trim_end_matches('/');
                    if let Err(err) = put_str(store, BK_CONFIG, "CRAWL4AI_URL", u).await {
                        return fail(StatusCode::INTERNAL_SERVER_ERROR, err);
                    }
                    sys.reach_reset(store).await;
                }
                if let Some(on) = d.opt_bool("enabled")
                    && let Err(err) = put_bool(store, "internet", on).await
                {
                    return fail(StatusCode::INTERNAL_SERVER_ERROR, err);
                }
            }
            let cfg = store.config().await;
            let key = store.get_str(BK_STATE, "crawl_key").await;
            let switch_on = get_bool(store, "internet").await;
            send_json(
                StatusCode::OK,
                &json!({
                    "ok": true,
                    "enabled": internet_enabled(&cfg, switch_on),
                    "engine": web_engine(&cfg),
                    "url": crawl4ai_url(&cfg),
                    "reachable": sys.reachable(&cfg, &key).await,
                    "key_set": !key.is_empty(),
                    "key_hint": key_hint(&key),
                }),
            )
        }
        "/api/computer" => {
            if post {
                let on = gojson::decode_struct(&raw, COMPUTER_REQ_TYPE, &[("on", Kind::Bool)])
                    .bool("on");
                if let Err(e) = put_bool(store, "computer", on).await {
                    return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
                }
                // Couper l'interrupteur ferme le navigateur : celui de la
                // façade (lot 44), et celui du Go s'il en a ouvert un.
                if !on {
                    crate::browser::global().shutdown().await;
                }
                if !on
                    && let Err(e) = store
                        .internal_post("/internal/v1/cdp/shutdown", &json!({}))
                        .await
                {
                    eprintln!("[navigateur] fermeture demandée au Go : {e}");
                }
            }
            let bin = match &sys.chrome {
                Some(c) => c.clone(),
                None => tokio::task::spawn_blocking(chrome_path)
                    .await
                    .unwrap_or_default(),
            };
            let cfg = store.config().await;
            send_json(
                StatusCode::OK,
                &json!({
                    "ok": true,
                    "enabled": get_bool(store, "computer").await,
                    "browser": bin,
                    "browser_ok": !bin.is_empty(),
                    "vision": vision_enabled(&cfg),
                }),
            )
        }
        "/api/apikey" => {
            if post {
                if let Some(r) = tune_deny(store).await {
                    return r;
                }
                let d = gojson::decode_struct(
                    &raw,
                    APIKEY_REQ_TYPE,
                    &[("action", Kind::Str), ("key", Kind::Str)],
                );
                if let Some(e) = d.err.clone() {
                    return fail(StatusCode::BAD_REQUEST, e);
                }
                let key = match d.str("action").as_str() {
                    "generate" => gen_api_key(),
                    "set" => d.str("key").trim().to_string(),
                    "clear" => String::new(),
                    _ => return fail(StatusCode::BAD_REQUEST, "action inconnue"),
                };
                // Aucune ambiguïté avec une valeur résiduelle de config.env.
                let _ = store.del(BK_CONFIG, "API_KEY").await;
                if let Err(e) = put_str(store, BK_STATE, "api_key", &key).await {
                    return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
                }
                restart_if_active(ctx).await;
            }
            // `effectiveAPIKeyErr` : la clé en base, sinon celle de config.env.
            let mut k = store
                .get(BK_STATE, "api_key")
                .await
                .ok()
                .flatten()
                .map(|b| go_lossy(&b))
                .unwrap_or_default();
            if k.is_empty() {
                k = serve::trim(serve::get(&store.config().await, "API_KEY")).to_string();
            }
            let public = store.get_str(BK_STATE, "public_url").await;
            let base = if public.is_empty() {
                request_base(&parts.headers, &parts.uri)
            } else {
                public.clone()
            };
            send_json(
                StatusCode::OK,
                &json!({
                    "ok": true,
                    "set": !k.is_empty(),
                    "masked": mask_api_key(&k),
                    "key": k,
                    "url": format!("{base}/v1"),
                    "public_url": public,
                }),
            )
        }
        "/api/network" => {
            if post {
                let d =
                    gojson::decode_struct(&raw, NETWORK_REQ_TYPE, &[("exposed", Kind::OptBool)]);
                if let Some(e) = d.err.clone() {
                    return fail(StatusCode::BAD_REQUEST, e);
                }
                if let Some(on) = d.opt_bool("exposed") {
                    let host = if on { "0.0.0.0" } else { "127.0.0.1" };
                    if let Err(e) = store.put(BK_CONFIG, "HOST", host.as_bytes().to_vec()).await {
                        return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
                    }
                    let cfg = store.config().await;
                    let st = tokio::task::spawn_blocking(move || network_status(&cfg))
                        .await
                        .unwrap_or_default();
                    return send_json(StatusCode::OK, &json!({ "ok": true, "status": st }));
                }
            }
            let cfg = store.config().await;
            let st = tokio::task::spawn_blocking(move || network_status(&cfg))
                .await
                .unwrap_or_default();
            send_json(StatusCode::OK, &json!({ "ok": true, "status": st }))
        }
        _ => send_json(StatusCode::NOT_FOUND, &json!({ "error": "route inconnue" })),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn octets_invalides_comme_go() {
        assert_eq!(go_lossy(b"ab"), "ab");
        assert_eq!(go_lossy(&[0x61, 0xC3]), "a\u{FFFD}");
        assert_eq!(go_lossy(&[0xA9, 0x62]), "\u{FFFD}b");
        assert_eq!(go_lossy(&[0xE2, 0x82, 0x41]), "\u{FFFD}\u{FFFD}A");
    }

    #[test]
    fn masque_sur_les_octets() {
        assert_eq!(mask_api_key("k"), "…");
        assert_eq!(mask_api_key("abc"), "…bc");
        assert_eq!(mask_api_key("sk-loki-0123456789abcdef"), "sk-loki-0…cdef");
        // Coupé au milieu d'un caractère : U+FFFD, comme l'encodeur du Go.
        assert_eq!(mask_api_key("abcdefghijé0"), "…\u{FFFD}0");
        assert_eq!(mask_api_key("abcdefgh€xyz0123"), "abcdefgh\u{FFFD}…0123");
    }
}
