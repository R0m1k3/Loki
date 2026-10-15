//! Réglages partagés entre appareils — port de `web_prefs.go`,
//! `web_sysprompt.go`, `handleReasoningEffort` et `handlePublicAddress`
//! (lot 10 de la migration), servis par la façade quand
//! `LOKI_SETTINGS=rust` (expérimental ; le Go par défaut) :
//!
//! - `GET|POST /api/prefs` : thème et affichage, prénom, avatars ;
//! - `GET|POST /api/sysprompt` : prompt système du preset actif (repli sur le
//!   prompt global) ;
//! - `GET|POST /api/reasoning-effort` : intensité du raisonnement
//!   (`REASONING_EFFORT` de la configuration vivante) ;
//! - `GET|POST /api/oai/public` : adresse publique de l'instance.
//!
//! Tout vit dans la base du Go (mêmes clés) : le Go relit ces valeurs là où il
//! s'en sert (tour de chat, accès OpenAI).
//!
//! Écarts assumés : message d'un corps JSON syntaxiquement illisible (serde) ;
//! préférences réécrites clé par clé au lieu d'une transaction (le Go
//! remplaçait le bucket d'un bloc) ; port d'adresse publique nommé
//! (`:https`) refusé — le Go le résolvait par /etc/services.

use std::collections::{BTreeMap, HashMap};

use hyper::body::Incoming;
use hyper::{Method, Request, Response, StatusCode};
use serde_json::{Value, json};

use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::chat::wire::{reasoning_active, reasoning_effort_value};
use crate::models::body_json;
use crate::presets;
use crate::proxy::Body;
use crate::store::BK_STATE;

const BK_PREFS: &str = "prefs";
const BK_CONFIG: &str = "config";
const SYSPROMPT_GLOBAL: &str = "sysprompt";
const SYSPROMPT_PREFIX: &str = "sysprompt:";
const PUBLIC_URL_KEY: &str = "public_url";

/// Valeurs proposées pour `REASONING_EFFORT` (`ReasoningEfforts`).
pub const REASONING_EFFORTS: [&str; 5] = ["none", "low", "medium", "high", "xhigh"];

/// Avatars proposés (liste fermée).
pub const AVATARS: [&str; 16] = [
    "🙂", "😎", "🧑", "👤", "🦊", "🐱", "🐼", "🦉", "🚀", "⚡", "🌙", "🔥", "🌿", "🎯", "🧠", "🤖",
];

/// Routes servies ici.
pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/prefs" | "/api/sysprompt" | "/api/reasoning-effort" | "/api/oai/public"
    )
}

// ---------------------------------------------------------------------------
// Préférences
// ---------------------------------------------------------------------------

/// `webPrefsAllowed` : clés à valeurs fermées.
fn allowed(key: &str) -> Option<&'static [&'static str]> {
    Some(match key {
        "theme" => &["light", "dark"],
        "hide_reasoning" | "hide_tools" | "fold_tools" | "hide_side" | "hide_stats" => &["0", "1"],
        _ => return None,
    })
}

/// `strings.TrimSpace` (espaces Unicode).
fn go_trim(s: &str) -> &str {
    s.trim_matches(char::is_whitespace)
}

/// `webPrefsText` : nettoyage des préférences libres ; `None` : refusée.
fn clean_text(key: &str, v: &str) -> Option<Option<String>> {
    match key {
        "user_name" => {
            let v: String = v
                .chars()
                .filter(|&c| c >= '\u{20}' && c != '\u{7f}')
                .collect();
            let mut v = go_trim(&v).to_string();
            if v.chars().count() > 24 {
                v = go_trim(&v.chars().take(24).collect::<String>()).to_string();
            }
            Some(Some(v))
        }
        "avatar_user" | "avatar_ai" => {
            let v = go_trim(v);
            if v.is_empty() || AVATARS.contains(&v) {
                Some(Some(v.to_string()))
            } else {
                Some(None)
            }
        }
        _ => None,
    }
}

/// `saveWebPrefs` : fusionne les valeurs valides de `req` dans `prefs`.
pub fn merge_prefs(prefs: &mut BTreeMap<String, String>, req: &Value) {
    let Some(obj) = req.as_object() else {
        return;
    };
    for (k, v) in obj {
        // map[string]string du Go : une valeur non textuelle est sautée.
        let Some(v) = v.as_str() else { continue };
        if let Some(res) = clean_text(k, v) {
            if let Some(val) = res {
                prefs.insert(k.clone(), val);
            }
            continue;
        }
        let Some(ok) = allowed(k) else { continue };
        let v = go_trim(v).to_lowercase();
        if ok.contains(&v.as_str()) {
            prefs.insert(k.clone(), v);
        }
    }
}

// ---------------------------------------------------------------------------
// Adresse publique
// ---------------------------------------------------------------------------

/// Caractères refusés dans un nom d'hôte par `url.Parse` (`InvalidHostError`).
fn host_char_ok(c: char) -> bool {
    !c.is_ascii() || c.is_ascii_alphanumeric() || "-_.~!$&'()*+,;=:[]<>\"%".contains(c)
}

/// `url.Parse` du Go, réduit à ce que l'adresse publique examine.
struct GoUrl {
    scheme: String,
    user: Option<String>,
    host: String,
    path: String,
    query: String,
    fragment: String,
}

fn go_url_parse(raw: &str) -> Option<GoUrl> {
    let (rest, fragment) = match raw.split_once('#') {
        Some((a, b)) => (a, b.to_string()),
        None => (raw, String::new()),
    };
    let (scheme, rest) = rest.split_once("://")?;
    let mut sc = scheme.chars();
    if !sc.next().is_some_and(|c| c.is_ascii_alphabetic())
        || !sc.all(|c| c.is_ascii_alphanumeric() || "+-.".contains(c))
    {
        return None;
    }
    let (rest, query) = match rest.split_once('?') {
        Some((a, b)) => (a, b.to_string()),
        None => (rest, String::new()),
    };
    let (authority, path) = match rest.find('/') {
        Some(i) => (&rest[..i], &rest[i..]),
        None => (rest, ""),
    };
    let (user, host) = match authority.rfind('@') {
        Some(i) => {
            let info = &authority[..i];
            let name = info.split(':').next().unwrap_or("");
            (Some(percent_decode(name)?), &authority[i + 1..])
        }
        None => (None, authority),
    };
    // Port : des chiffres seulement après le dernier « : » (hors crochets).
    if let Some(stripped) = host.strip_prefix('[') {
        let end = stripped.find(']')?;
        let after = &stripped[end + 1..];
        if !after.is_empty()
            && !(after.starts_with(':') && after[1..].bytes().all(|b| b.is_ascii_digit()))
        {
            return None;
        }
    } else if let Some(i) = host.rfind(':')
        && !host[i + 1..].bytes().all(|b| b.is_ascii_digit())
    {
        return None;
    }
    if !host.chars().all(host_char_ok) {
        return None;
    }
    Some(GoUrl {
        scheme: scheme.to_string(),
        user,
        host: host.to_string(),
        path: percent_decode(path)?,
        query,
        fragment,
    })
}

fn percent_decode(s: &str) -> Option<String> {
    let b = s.as_bytes();
    let mut out = Vec::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        if b[i] == b'%' {
            let h = std::str::from_utf8(b.get(i + 1..i + 3)?).ok()?;
            out.push(u8::from_str_radix(h, 16).ok()?);
            i += 3;
        } else {
            out.push(b[i]);
            i += 1;
        }
    }
    String::from_utf8(out).ok()
}

/// `net.SplitHostPort`, réduit : (hôte, port) ou rien.
fn split_host_port(host: &str) -> Option<(&str, &str)> {
    if let Some(rest) = host.strip_prefix('[') {
        let end = rest.find(']')?;
        let after = &rest[end + 1..];
        return after.strip_prefix(':').map(|p| (&rest[..end], p));
    }
    let i = host.rfind(':')?;
    let h = &host[..i];
    if h.contains(':') {
        return None; // trop de « : »
    }
    Some((h, &host[i + 1..]))
}

/// `normalizePublicURL` : « schéma://hôte[:port] », sans slash final.
pub fn normalize_public_url(raw: &str) -> Result<String, String> {
    let raw = go_trim(raw);
    if raw.is_empty() {
        return Ok(String::new());
    }
    let raw = if raw.contains("://") {
        raw.to_string()
    } else {
        format!("https://{raw}")
    };
    let u = go_url_parse(&raw).ok_or_else(|| format!("adresse illisible : {raw}"))?;
    let scheme = u.scheme.to_lowercase();
    if scheme != "http" && scheme != "https" {
        return Err(format!(
            "adresse en {scheme}:// — seuls http:// et https:// sont acceptés"
        ));
    }
    if u.host.is_empty() {
        return Err("adresse sans nom de machine".into());
    }
    if let Some(name) = u.user {
        return Err(format!(
            "l'adresse ne doit pas porter d'identifiants ({name}@…)"
        ));
    }
    if !u.query.is_empty() || !u.fragment.is_empty() {
        return Err("l'adresse ne doit porter ni paramètre ni ancre".into());
    }
    match u.path.strip_suffix('/').unwrap_or(&u.path) {
        "" | "/v1" => {}
        _ => {
            return Err(format!(
                "l'adresse ne doit pas contenir de chemin ({}) : Loki sert /v1 à la racine de son port",
                u.path
            ));
        }
    }
    match split_host_port(&u.host) {
        Some((h, port)) => {
            if h.is_empty() {
                return Err("adresse sans nom de machine".into());
            }
            if !port.parse::<u32>().is_ok_and(|n| n > 0 && n <= 65535) {
                return Err(format!("port invalide : {port}"));
            }
        }
        None if u.host.contains(':') && !u.host.starts_with('[') => {
            return Err(format!(
                "adresse IPv6 à écrire entre crochets : [{}]",
                u.host
            ));
        }
        None => {}
    }
    Ok(format!("{scheme}://{}", u.host))
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

/// Sérialise les écritures de préférences (lecture, fusion, écriture).
static PREFS_LOCK: tokio::sync::Mutex<()> = tokio::sync::Mutex::const_new(());

async fn load_prefs(ctx: &Ctx) -> Result<BTreeMap<String, String>, String> {
    Ok(ctx.store.all(BK_PREFS).await?.into_iter().collect())
}

/// `putStr` : une chaîne vide efface la clé.
async fn put_str(ctx: &Ctx, bucket: &str, key: &str, val: &str) -> Result<(), String> {
    if val.is_empty() {
        ctx.store.del(bucket, key).await
    } else {
        ctx.store.put(bucket, key, val.as_bytes().to_vec()).await
    }
}

/// Erreur de type d'`encoding/json` pour un champ texte attendu.
fn go_type_error(field: &str, v: &Value) -> Option<String> {
    let kind = match v {
        Value::Null | Value::String(_) => return None,
        Value::Bool(_) => "bool",
        Value::Number(_) => "number",
        Value::Array(_) => "array",
        Value::Object(_) => "object",
    };
    Some(format!(
        "json: cannot unmarshal {kind} into Go struct field .{field} of type string"
    ))
}

async fn active_preset(ctx: &Ctx) -> String {
    let cfg: HashMap<String, String> = ctx.store.config().await;
    let order = presets::parse_preset_order(
        ctx.store
            .get(BK_STATE, presets::PRESET_ORDER_KEY)
            .await
            .ok()
            .flatten()
            .as_deref(),
    );
    let home = ctx.engine.home.clone();
    tokio::task::spawn_blocking(move || presets::active_preset_id(&home, &cfg, &order))
        .await
        .unwrap_or_default()
}

/// `readSysPrompt` : celui du preset actif, sinon le global.
async fn read_sysprompt(ctx: &Ctx) -> String {
    let id = active_preset(ctx).await;
    if !id.is_empty() {
        let s = ctx
            .store
            .get_str(BK_STATE, &format!("{SYSPROMPT_PREFIX}{id}"))
            .await;
        if !go_trim(&s).is_empty() {
            return go_trim(&s).to_string();
        }
    }
    ctx.store.get_str(BK_STATE, SYSPROMPT_GLOBAL).await
}

fn fail(code: StatusCode, e: impl Into<String>) -> Response<Body> {
    send_json(code, &json!({ "ok": false, "error": e.into() }))
}

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(ctx: &Ctx, req: Request<Incoming>, from_go: bool) -> Response<Body> {
    let (parts, body) = req.into_parts();
    if !from_go && let Err(refus) = ctx.guard(&parts.headers).await {
        return *refus;
    }
    let post = parts.method == Method::POST;
    match parts.uri.path() {
        "/api/prefs" if post => {
            let req = body_json(body).await.unwrap_or(Value::Null);
            let _guard = PREFS_LOCK.lock().await;
            let old = match load_prefs(ctx).await {
                Ok(p) => p,
                Err(e) => return fail(StatusCode::INTERNAL_SERVER_ERROR, e),
            };
            let mut prefs = old.clone();
            merge_prefs(&mut prefs, &req);
            // replaceKV : les valeurs vides ne sont pas gardées.
            for (k, v) in &prefs {
                let res = if v.is_empty() {
                    if old.contains_key(k) {
                        ctx.store.del(BK_PREFS, k).await
                    } else {
                        Ok(())
                    }
                } else if old.get(k) != Some(v) {
                    ctx.store.put(BK_PREFS, k, v.as_bytes().to_vec()).await
                } else {
                    Ok(())
                };
                if let Err(e) = res {
                    return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
                }
            }
            send_json(StatusCode::OK, &json!({ "ok": true, "prefs": prefs }))
        }
        "/api/prefs" => match load_prefs(ctx).await {
            Ok(p) => send_json(
                StatusCode::OK,
                &json!({ "ok": true, "prefs": p, "avatars": AVATARS }),
            ),
            Err(_) => send_json(
                StatusCode::OK,
                &json!({ "ok": true, "prefs": {}, "avatars": AVATARS }),
            ),
        },
        "/api/sysprompt" => match parts.method {
            Method::GET => send_json(
                StatusCode::OK,
                &json!({ "ok": true, "text": read_sysprompt(ctx).await }),
            ),
            Method::POST => {
                let v = match body_json(body).await {
                    Ok(v) => v,
                    Err(e) => return fail(StatusCode::BAD_REQUEST, e),
                };
                let raw = v.get("text").cloned().unwrap_or(Value::Null);
                if let Some(e) = go_type_error("text", &raw) {
                    return fail(StatusCode::BAD_REQUEST, e);
                }
                let text = go_trim(raw.as_str().unwrap_or("")).to_string();
                let id = active_preset(ctx).await;
                let key = if id.is_empty() {
                    SYSPROMPT_GLOBAL.to_string()
                } else {
                    format!("{SYSPROMPT_PREFIX}{id}")
                };
                if let Err(e) = put_str(ctx, BK_STATE, &key, &text).await {
                    return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
                }
                send_json(
                    StatusCode::OK,
                    &json!({ "ok": true, "text": read_sysprompt(ctx).await }),
                )
            }
            _ => fail(StatusCode::METHOD_NOT_ALLOWED, "méthode non autorisée"),
        },
        "/api/reasoning-effort" => {
            if post {
                let v = body_json(body).await.unwrap_or(Value::Null);
                let v =
                    go_trim(v.get("value").and_then(Value::as_str).unwrap_or("")).to_lowercase();
                if !v.is_empty() && !REASONING_EFFORTS.contains(&v.as_str()) {
                    return fail(StatusCode::BAD_REQUEST, format!("valeur inconnue : {v}"));
                }
                if let Err(e) = put_str(ctx, BK_CONFIG, "REASONING_EFFORT", &v).await {
                    return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
                }
            }
            let cfg = ctx.store.config().await;
            let get = |k: &str| cfg.get(k).map(String::as_str).unwrap_or("");
            send_json(
                StatusCode::OK,
                &json!({
                    "ok": true,
                    "value": reasoning_effort_value(get("REASONING_EFFORT")),
                    "on": reasoning_active(get("REASONING")),
                    "choices": REASONING_EFFORTS,
                }),
            )
        }
        "/api/oai/public" if post => {
            let v = match body_json(body).await {
                Ok(Value::Object(o)) => o,
                _ => return fail(StatusCode::BAD_REQUEST, "requête illisible"),
            };
            let raw = v.get("url").cloned().unwrap_or(Value::Null);
            if go_type_error("url", &raw).is_some() {
                return fail(StatusCode::BAD_REQUEST, "requête illisible");
            }
            let clean = match normalize_public_url(raw.as_str().unwrap_or("")) {
                Ok(c) => c,
                Err(e) => return fail(StatusCode::BAD_REQUEST, e),
            };
            if let Err(e) = put_str(ctx, BK_STATE, PUBLIC_URL_KEY, &clean).await {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            send_json(
                StatusCode::OK,
                &json!({ "ok": true, "url": clean, "set": !clean.is_empty() }),
            )
        }
        "/api/oai/public" => {
            let u = ctx.store.get_str(BK_STATE, PUBLIC_URL_KEY).await;
            send_json(
                StatusCode::OK,
                &json!({ "ok": true, "url": u, "set": !u.is_empty() }),
            )
        }
        _ => send_json(
            StatusCode::NOT_FOUND,
            &json!({ "error": "404 page not found" }),
        ),
    }
}
