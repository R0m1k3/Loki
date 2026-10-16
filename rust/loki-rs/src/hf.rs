//! Catalogue Hugging Face — port de `backend_hf.go`, `web_hf.go` et
//! `backend_catalog.go` (lot 8 de la migration), servi par la façade quand
//! `LOKI_MODELS=rust` :
//!
//! - `GET /api/hf/search?q=…` : dépôts GGUF ;
//! - `GET /api/hf/files?repo=auteur/dépôt` : modèles, projecteurs et poids
//!   MTP du dépôt, chaque modèle avec son verdict mémoire ;
//! - `GET|POST /api/hf/token` : le jeton (dépôts verrouillés), vérifié avant
//!   d'être enregistré, jamais rendu en clair.
//!
//! Rien n'est téléchargé ici : les liens produits passent par
//! [`crate::downloads`]. Réponses de l'API mises en cache (60 s pour une
//! recherche, 10 min pour un dépôt) ; un nouveau jeton vide le cache.
//!
//! Écarts assumés : erreur de transport au format de reqwest ; mémoire vive
//! inconnue hors Linux (la façade n'est livrée que pour Linux).

use std::collections::HashMap;
use std::sync::Mutex;
use std::time::{Duration, Instant};

use bytes::Bytes;
use hyper::body::Incoming;
use hyper::{Method, Request, Response, StatusCode};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::gguf;
use crate::models::quant_from_name;
use crate::paths::base_name;
use crate::proxy::Body;
use crate::store::BK_STATE;

pub const HF_HOST: &str = "https://huggingface.co";
const SEARCH_TTL: Duration = Duration::from_secs(60);
const FILES_TTL: Duration = Duration::from_secs(600);
const MAX_REPOS: usize = 25;
const MAX_QUERY: usize = 100;
const TIMEOUT: Duration = Duration::from_secs(10);

/// Routes servies ici.
pub fn handles(path: &str) -> bool {
    matches!(path, "/api/hf/search" | "/api/hf/files" | "/api/hf/token")
}

// ---------------------------------------------------------------------------
// Fonctions pures
// ---------------------------------------------------------------------------

/// `hfRepoRe`.
pub fn repo_ok(repo: &str) -> bool {
    let part = |s: &str| {
        let mut c = s.chars();
        c.next().is_some_and(|f| f.is_ascii_alphanumeric())
            && c.all(|ch| ch.is_ascii_alphanumeric() || matches!(ch, '.' | '_' | '-'))
    };
    match repo.split_once('/') {
        Some((a, b)) => part(a) && part(b),
        None => false,
    }
}

/// `maskHFToken` : assez de caractères pour reconnaître le jeton posé.
pub fn mask_hf_token(t: &str) -> String {
    let t = t.trim();
    if t.is_empty() {
        return String::new();
    }
    let b = t.as_bytes();
    let s = |r: std::ops::Range<usize>| String::from_utf8_lossy(&b[r]).into_owned();
    if b.len() <= 8 {
        return format!("…{}", s(b.len().saturating_sub(2)..b.len()));
    }
    format!("{}…{}", s(0..3), s(b.len() - 4..b.len()))
}

/// `hfGatedFlag` : `false`, `"auto"` ou `"manual"` — seul compte le verrou.
pub fn gated_flag(v: &Value) -> bool {
    match v {
        Value::Bool(b) => *b,
        Value::String(s) => !matches!(
            s.trim().to_lowercase().as_str(),
            "" | "false" | "none" | "no"
        ),
        _ => false,
    }
}

/// `url.PathEscape`.
pub fn go_path_escape(s: &str) -> String {
    let mut out = String::new();
    for &c in s.as_bytes() {
        if c.is_ascii_alphanumeric() || b"-_.~$&+:=@".contains(&c) {
            out.push(c as char);
        } else {
            out.push_str(&format!("%{c:02X}"));
        }
    }
    out
}

/// `url.QueryEscape` (sur des octets : la recherche est tronquée à l'octet
/// comme dans le Go).
pub fn go_query_escape(s: &[u8]) -> String {
    let mut out = String::new();
    for &c in s {
        if c.is_ascii_alphanumeric() || b"-_.~".contains(&c) {
            out.push(c as char);
        } else if c == b' ' {
            out.push('+');
        } else {
            out.push_str(&format!("%{c:02X}"));
        }
    }
    out
}

/// Un `.gguf` installable (`hfEntry`).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct HfEntry {
    pub name: String,
    pub url: String,
    pub size: i64,
    pub quant: String,
    pub shards: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub verdict: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub why: String,
}

/// `null` quand vide, comme une tranche nil du Go.
mod nil_vec {
    use serde::{Deserialize, Deserializer, Serialize, Serializer};
    pub fn serialize<S: Serializer, T: Serialize>(v: &[T], s: S) -> Result<S::Ok, S::Error> {
        if v.is_empty() {
            s.serialize_none()
        } else {
            v.serialize(s)
        }
    }
    pub fn deserialize<'de, D: Deserializer<'de>, T: Deserialize<'de>>(
        d: D,
    ) -> Result<Vec<T>, D::Error> {
        Ok(Option::<Vec<T>>::deserialize(d)?.unwrap_or_default())
    }
}

/// Le contenu utile d'un dépôt (`hfListing`).
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct HfListing {
    pub repo: String,
    pub gated: bool,
    #[serde(with = "nil_vec")]
    pub models: Vec<HfEntry>,
    #[serde(with = "nil_vec")]
    pub projectors: Vec<HfEntry>,
    #[serde(with = "nil_vec")]
    pub drafts: Vec<HfEntry>,
}

/// `hfResolveURL` : chaque segment échappé à part.
pub fn resolve_url(base: &str, repo: &str, file_path: &str) -> String {
    let segs: Vec<String> = file_path.split('/').map(go_path_escape).collect();
    format!("{base}/{repo}/resolve/main/{}", segs.join("/"))
}

/// `hfClassify` : modèles, projecteurs (`mmproj*`) et poids MTP (`mtp-*`),
/// une famille de tranches comptée en entier, triés par taille.
pub fn classify(base: &str, repo: &str, files: &[(String, i64)]) -> HfListing {
    let mut size: HashMap<&str, i64> = HashMap::new();
    let mut paths = Vec::new();
    for (p, n) in files {
        if !p.to_lowercase().ends_with(".gguf") {
            continue;
        }
        size.insert(p, *n);
        paths.push(p.as_str());
    }
    let mut out = HfListing {
        repo: repo.into(),
        ..Default::default()
    };
    for p in paths {
        let name = base_name(p);
        if gguf::is_follower_shard(&name) {
            continue;
        }
        let fam = gguf::shard_family(&name);
        let dir = p.rfind('/').map_or("", |i| &p[..=i]);
        let total = fam
            .iter()
            .map(|n| size.get(format!("{dir}{n}").as_str()).copied().unwrap_or(0))
            .sum();
        let e = HfEntry {
            url: resolve_url(base, repo, p),
            size: total,
            quant: quant_from_name(&name),
            shards: fam.len() as i64,
            name: name.clone(),
            verdict: String::new(),
            why: String::new(),
        };
        let low = name.to_lowercase();
        if low.starts_with("mmproj") {
            out.projectors.push(e);
        } else if low.starts_with("mtp-") {
            out.drafts.push(e);
        } else {
            out.models.push(e);
        }
    }
    // sort.Slice n'est pas stable ; à tailles égales l'ordre du Go n'est pas
    // garanti non plus.
    for v in [&mut out.models, &mut out.projectors, &mut out.drafts] {
        v.sort_by_key(|e| e.size);
    }
    out
}

/// `hfPickProjector` : le Q8_0 d'abord, sinon le plus léger.
pub fn pick_projector(list: &[HfEntry]) -> Option<&HfEntry> {
    if let Some(e) = list.iter().find(|e| e.quant.eq_ignore_ascii_case("Q8_0")) {
        return Some(e);
    }
    let mut best = list.first()?;
    for e in &list[1..] {
        if e.size < best.size {
            best = e;
        }
    }
    Some(best)
}

/// Une carte (`gpuBrief`).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct GpuBrief {
    pub name: String,
    pub vram_gb: f64,
}

/// La machine (`hardwareInfo`).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct Hardware {
    pub os: String,
    pub arch: String,
    pub ram_gb: f64,
    pub vram_gb: f64,
    #[serde(with = "nil_vec")]
    pub gpus: Vec<GpuBrief>,
}

/// `runtime.GOOS` / `runtime.GOARCH`.
fn go_os_arch() -> (String, String) {
    let arch = match std::env::consts::ARCH {
        "x86_64" => "amd64",
        "aarch64" => "arm64",
        "x86" => "386",
        a => a,
    };
    (std::env::consts::OS.into(), arch.into())
}

/// `totalRAMGB` (Linux : MemTotal de /proc/meminfo).
fn total_ram_gb() -> f64 {
    #[cfg(target_os = "linux")]
    {
        let Ok(b) = std::fs::read_to_string("/proc/meminfo") else {
            return 0.0;
        };
        for line in b.lines() {
            if line.starts_with("MemTotal:") {
                return line
                    .split_whitespace()
                    .nth(1)
                    .and_then(|v| v.parse::<f64>().ok())
                    .map_or(0.0, |kb| kb / (1024.0 * 1024.0));
            }
        }
        0.0
    }
    #[cfg(not(target_os = "linux"))]
    {
        0.0
    }
}

/// `detectHardware` : la VRAM vient de nvidia-smi (cache partagé des jauges).
pub async fn detect_hardware() -> Hardware {
    let (os, arch) = go_os_arch();
    let mut h = Hardware {
        os,
        arch,
        ram_gb: total_ram_gb(),
        vram_gb: 0.0,
        gpus: Vec::new(),
    };
    for g in crate::sysinfo::gpu_stats_cached().await {
        let gb = g.total as f64 / 1024.0;
        h.vram_gb += gb;
        h.gpus.push(GpuBrief {
            name: g.name,
            vram_gb: gb,
        });
    }
    h
}

/// `fitVerdict` : « ok », « juste » ou « trop », et pourquoi.
pub fn fit_verdict(h: &Hardware, weights: i64, mmproj: i64, ctx_tokens: i64) -> (String, String) {
    const GB: f64 = (1u64 << 30) as f64;
    let w = weights as f64 / GB;
    let m = mmproj as f64 / GB;
    let ctx = if ctx_tokens <= 0 { 32768 } else { ctx_tokens };
    let kv = w / 20.0 * (ctx as f64 / 32768.0);
    let need = w + m + kv;
    let (budget, place) = if h.vram_gb > 0.0 {
        (h.vram_gb, "VRAM")
    } else {
        (h.ram_gb, "RAM")
    };
    if budget <= 0.0 {
        return (String::new(), String::new());
    }
    let mut detail = format!("{w:.1} Go de poids");
    if m > 0.0 {
        detail += &format!(" + {m:.1} Go de projecteur");
    }
    detail += &format!(
        " + ~{kv:.1} Go de contexte (estimation) = ~{need:.1} Go pour {budget:.1} Go de {place}"
    );
    if need > budget {
        ("trop".into(), format!("ne tient pas : {detail}"))
    } else if need > budget * 0.9 {
        ("juste".into(), format!("ça passe de justesse : {detail}"))
    } else {
        ("ok".into(), detail)
    }
}

/// `hfSearch` : l'URL appelée (clés triées comme `url.Values.Encode`).
pub fn search_endpoint(base: &str, q: &str) -> String {
    let q = q.trim().as_bytes();
    let q = &q[..q.len().min(MAX_QUERY)];
    format!(
        "{base}/api/models?direction=-1&expand%5B%5D=gated&expand%5B%5D=downloads&expand%5B%5D=likes&filter=gguf&limit={MAX_REPOS}&search={}&sort=downloads",
        go_query_escape(q)
    )
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

/// Appels à l'API Hugging Face, avec cache (`hfGetJSON`).
pub struct HfClient {
    base: String,
    client: reqwest::Client,
    cache: Mutex<HashMap<String, (Instant, Bytes)>>,
}

/// Ce dont un appel a besoin : jeton et version (User-Agent).
pub struct HfEnv {
    pub token: String,
    pub version: String,
}

#[derive(Deserialize)]
struct RawRepo {
    #[serde(default)]
    id: String,
    #[serde(default)]
    downloads: i64,
    #[serde(default)]
    likes: i64,
    #[serde(default)]
    gated: Value,
}

#[derive(Deserialize)]
struct RawFile {
    #[serde(default, rename = "type")]
    kind: String,
    #[serde(default)]
    path: String,
    #[serde(default)]
    size: i64,
    #[serde(default)]
    lfs: Option<RawLfs>,
}

#[derive(Deserialize)]
struct RawLfs {
    #[serde(default)]
    size: i64,
}

impl HfClient {
    /// `base` : `https://huggingface.co` (un faux serveur dans les tests).
    pub fn new(base: &str) -> Self {
        crate::chat::engine::install_crypto();
        HfClient {
            base: base.trim_end_matches('/').to_string(),
            client: reqwest::Client::builder()
                .timeout(TIMEOUT)
                .build()
                .unwrap_or_default(),
            cache: Mutex::new(HashMap::new()),
        }
    }

    pub fn base(&self) -> &str {
        &self.base
    }

    fn cache(&self) -> std::sync::MutexGuard<'_, HashMap<String, (Instant, Bytes)>> {
        self.cache.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Un nouveau jeton : les réponses obtenues sans lui ne valent plus.
    pub fn clear_cache(&self) {
        self.cache().clear();
    }

    async fn get_json<T: serde::de::DeserializeOwned>(
        &self,
        env: &HfEnv,
        endpoint: &str,
        ttl: Duration,
    ) -> Result<T, String> {
        let hit = self
            .cache()
            .get(endpoint)
            .filter(|(at, _)| at.elapsed() < ttl)
            .map(|(_, b)| b.clone());
        if let Some(raw) = hit {
            return serde_json::from_slice(&raw).map_err(|e| e.to_string());
        }
        let mut rb = self
            .client
            .get(endpoint)
            .header("User-Agent", format!("loki/{}", env.version))
            .header("Accept", "application/json");
        if !env.token.is_empty() {
            rb = rb.header("Authorization", format!("Bearer {}", env.token));
        }
        let resp = rb
            .send()
            .await
            .map_err(|e| format!("Hugging Face injoignable : {e}"))?;
        match resp.status().as_u16() {
            200 => {}
            401 | 403 => {
                return Err("dépôt à accès restreint — renseigne HF_TOKEN pour y accéder".into());
            }
            404 => return Err("dépôt introuvable sur Hugging Face".into()),
            s => return Err(format!("Hugging Face a répondu {s}")),
        }
        let raw = read_capped(resp, 4 << 20).await?;
        self.cache()
            .insert(endpoint.to_string(), (Instant::now(), raw.clone()));
        serde_json::from_slice(&raw).map_err(|e| e.to_string())
    }

    /// `hfSearch`.
    pub async fn search(&self, env: &HfEnv, q: &str) -> Result<Value, String> {
        if q.trim().is_empty() {
            return Err("recherche vide".into());
        }
        let raw: Vec<RawRepo> = self
            .get_json(env, &search_endpoint(&self.base, q), SEARCH_TTL)
            .await?;
        Ok(Value::Array(
            raw.into_iter()
                .map(|r| {
                    json!({ "id": r.id, "downloads": r.downloads, "likes": r.likes, "gated": gated_flag(&r.gated) })
                })
                .collect(),
        ))
    }

    /// `hfFiles`.
    pub async fn files(&self, env: &HfEnv, repo: &str) -> Result<HfListing, String> {
        let repo = repo.trim().trim_matches('/');
        if !repo_ok(repo) {
            return Err("nom de dépôt invalide (attendu « auteur/dépôt »)".into());
        }
        let endpoint = format!("{}/api/models/{repo}/tree/main?recursive=1", self.base);
        let raw: Vec<RawFile> = self.get_json(env, &endpoint, FILES_TTL).await?;
        let files: Vec<(String, i64)> = raw
            .into_iter()
            .filter(|f| f.kind != "directory")
            .map(|f| {
                let n = f.lfs.map_or(0, |l| l.size);
                (f.path, if n == 0 { f.size } else { n })
            })
            .collect();
        let mut out = classify(&self.base, repo, &files);
        out.gated = self.repo_gated(env, repo).await;
        Ok(out)
    }

    /// `hfRepoGated` : une erreur ne casse rien (pas d'avertissement, c'est
    /// tout).
    async fn repo_gated(&self, env: &HfEnv, repo: &str) -> bool {
        #[derive(Deserialize)]
        struct Info {
            #[serde(default)]
            gated: Value,
        }
        let endpoint = format!("{}/api/models/{repo}?expand%5B%5D=gated", self.base);
        self.get_json::<Info>(env, &endpoint, FILES_TTL)
            .await
            .is_ok_and(|i| gated_flag(&i.gated))
    }

    /// `hfWhoAmI` : le compte du jeton, ou pourquoi il est refusé.
    pub async fn whoami(&self, version: &str, tok: &str) -> Result<String, String> {
        let resp = self
            .client
            .get(format!("{}/api/whoami-v2", self.base))
            .header("Authorization", format!("Bearer {}", tok.trim()))
            .header("User-Agent", format!("loki/{version}"))
            .header("Accept", "application/json")
            .send()
            .await
            .map_err(|e| format!("Hugging Face injoignable : {e}"))?;
        match resp.status().as_u16() {
            200 => {}
            401 | 403 => {
                return Err("jeton refusé par Hugging Face (expiré, révoqué ou mal copié)".into());
            }
            s => return Err(format!("Hugging Face a répondu {s}")),
        }
        #[derive(Deserialize)]
        struct Who {
            #[serde(default)]
            name: String,
        }
        let raw = read_capped(resp, 1 << 20).await?;
        serde_json::from_slice::<Who>(&raw)
            .map(|w| w.name)
            .map_err(|e| e.to_string())
    }
}

/// Lit au plus `max` octets du corps (réponse d'un tiers).
async fn read_capped(mut resp: reqwest::Response, max: usize) -> Result<Bytes, String> {
    let mut buf = Vec::new();
    while let Some(c) = resp.chunk().await.map_err(|e| e.to_string())? {
        let room = max - buf.len();
        buf.extend_from_slice(&c[..c.len().min(room)]);
        if buf.len() >= max {
            break;
        }
    }
    Ok(Bytes::from(buf))
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

/// `hfTokenSource` : « config », « env » ou "".
async fn token_source(ctx: &Ctx) -> (&'static str, String) {
    let stored = ctx.store.get_str(BK_STATE, "hf_token").await;
    if !stored.trim().is_empty() {
        return ("config", stored.trim().to_string());
    }
    let env = std::env::var("HF_TOKEN").unwrap_or_default();
    if !env.trim().is_empty() {
        return ("env", env.trim().to_string());
    }
    ("", String::new())
}

fn token_state(src: &str, tok: &str, account: &str) -> Value {
    json!({ "ok": true, "set": !src.is_empty(), "source": src, "masked": mask_hf_token(tok), "account": account })
}

fn query_param(uri: &hyper::Uri, key: &str) -> String {
    let q = uri.query().unwrap_or("");
    for kv in q.split('&') {
        let (k, v) = kv.split_once('=').unwrap_or((kv, ""));
        if form_decode(k) == key {
            return form_decode(v);
        }
    }
    String::new()
}

/// Décodage `application/x-www-form-urlencoded` (`+` → espace, `%XX`).
fn form_decode(s: &str) -> String {
    let b = s.as_bytes();
    let mut out = Vec::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        match b[i] {
            b'+' => out.push(b' '),
            b'%' if i + 2 < b.len() => {
                match u8::from_str_radix(std::str::from_utf8(&b[i + 1..i + 3]).unwrap_or("zz"), 16)
                {
                    Ok(v) => {
                        out.push(v);
                        i += 3;
                        continue;
                    }
                    Err(_) => out.push(b'%'),
                }
            }
            c => out.push(c),
        }
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

pub async fn serve(
    ctx: &Ctx,
    hf: &HfClient,
    req: Request<Incoming>,
    from_go: bool,
) -> Response<Body> {
    let (parts, body) = req.into_parts();
    let version = if from_go {
        ctx.internal
            .auth()
            .await
            .map(|i| i.version)
            .unwrap_or_default()
    } else {
        match ctx.guard(&parts.headers).await {
            Ok(v) => v,
            Err(refus) => return *refus,
        }
    };
    let (src, token) = token_source(ctx).await;
    let env = HfEnv {
        token: token.clone(),
        version: version.clone(),
    };
    let err = |code: StatusCode, e: String| send_json(code, &json!({ "ok": false, "error": e }));
    match parts.uri.path() {
        "/api/hf/search" => {
            let q = query_param(&parts.uri, "q");
            if q.trim().is_empty() {
                return err(StatusCode::BAD_REQUEST, "précise ce que tu cherches".into());
            }
            match hf.search(&env, &q).await {
                Ok(repos) => send_json(
                    StatusCode::OK,
                    &json!({ "ok": true, "hardware": detect_hardware().await, "repos": repos, "hf_token": !token.is_empty() }),
                ),
                Err(e) => err(StatusCode::BAD_GATEWAY, e),
            }
        }
        "/api/hf/files" => {
            let repo = query_param(&parts.uri, "repo");
            if repo.trim().is_empty() {
                return err(StatusCode::BAD_REQUEST, "dépôt manquant".into());
            }
            let mut list = match hf.files(&env, &repo).await {
                Ok(l) => l,
                Err(e) => return err(StatusCode::BAD_GATEWAY, e),
            };
            let hw = detect_hardware().await;
            // Contexte du preset actif, défaut de serve.
            let ctx_tokens = ctx
                .store
                .config()
                .await
                .get("CTX")
                .and_then(|v| v.trim().parse::<i64>().ok())
                .filter(|v| *v > 0)
                .unwrap_or(32768);
            let mm = pick_projector(&list.projectors).map_or(0, |p| p.size);
            for m in &mut list.models {
                (m.verdict, m.why) = fit_verdict(&hw, m.size, mm, ctx_tokens);
            }
            let vision = !list.projectors.is_empty();
            let nil = |v: &Vec<HfEntry>| {
                if v.is_empty() { Value::Null } else { json!(v) }
            };
            send_json(
                StatusCode::OK,
                &json!({
                    "ok": true, "hardware": hw, "ctx": ctx_tokens,
                    "repo": list.repo, "models": nil(&list.models),
                    "projectors": nil(&list.projectors), "drafts": nil(&list.drafts),
                    "vision": vision, "gated": list.gated, "hf_token": !token.is_empty(),
                }),
            )
        }
        "/api/hf/token" if parts.method == Method::POST => {
            let v = match crate::models::body_json(body).await {
                Ok(v) => v,
                Err(_) => return err(StatusCode::BAD_REQUEST, "requête illisible".into()),
            };
            let tok = v
                .get("token")
                .and_then(Value::as_str)
                .unwrap_or("")
                .trim()
                .to_string();
            let mut account = String::new();
            if !tok.is_empty() {
                match hf.whoami(&version, &tok).await {
                    Ok(a) => account = a,
                    Err(e) => return err(StatusCode::BAD_REQUEST, e),
                }
            }
            // putStr : une valeur vide efface la clé.
            let res = if tok.is_empty() {
                ctx.store.del(BK_STATE, "hf_token").await
            } else {
                ctx.store
                    .put(BK_STATE, "hf_token", tok.clone().into_bytes())
                    .await
            };
            if let Err(e) = res {
                return err(StatusCode::INTERNAL_SERVER_ERROR, e);
            }
            hf.clear_cache();
            let (src, token) = token_source(ctx).await;
            send_json(StatusCode::OK, &token_state(src, &token, &account))
        }
        "/api/hf/token" => send_json(StatusCode::OK, &token_state(src, &token, "")),
        _ => send_json(
            StatusCode::NOT_FOUND,
            &json!({ "error": "404 page not found" }),
        ),
    }
}
