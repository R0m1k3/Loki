//! Les routes des modèles tenues par la façade quand `LOKI_MODELS=rust` (lot
//! 8 de la migration ; le Go par défaut) :
//!
//! - `GET /api/models` : les `.gguf` du sélecteur, avec qui les référence
//!   (`handleModels`, `backend_models_refs.go`) ;
//! - `POST /api/models/delete` : suppression, garde-fous compris
//!   (`handleModelDelete`, `deleteModelFile`) ;
//! - `GET|POST /api/models/dirs` : dossiers de modèles (`handleModelDirs`) ;
//! - les téléchargements ([`crate::downloads`]) ;
//! - le catalogue Hugging Face ([`crate::hf`]).
//!
//! Le Go remet ces routes à la façade quand elles lui arrivent par le tunnel
//! du relais (`facade_models.go`).
//!
//! Écart assumé : le message d'un corps JSON illisible est celui de serde,
//! pas celui d'`encoding/json` (l'interface n'en envoie jamais).

use std::collections::{HashMap, HashSet};
use std::path::Path;
use std::sync::Arc;

use http_body_util::{BodyExt, Limited};
use hyper::body::Incoming;
use hyper::{Method, Request, Response, StatusCode};
use serde_json::{Map, Value, json};

use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::downloads::{self, Downloads, ModelDirs, disk_free, disk_free_reliable};
use crate::gguf;
use crate::hf;
use crate::paths::{base_name, go_abs, go_dir, go_join};
use crate::presets;
use crate::proxy::Body;
use crate::store::BK_STATE;

/// Ce que la façade tient quand `LOKI_MODELS=rust`.
pub struct Models {
    pub downloads: Arc<Downloads>,
    pub hf: hf::HfClient,
}

impl Default for Models {
    fn default() -> Self {
        Self::new()
    }
}

impl Models {
    pub fn new() -> Self {
        Models {
            downloads: Arc::new(Downloads::new()),
            hf: hf::HfClient::new(hf::HF_HOST),
        }
    }
}

/// Routes servies ici quand la façade tient les modèles.
pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/models" | "/api/models/delete" | "/api/models/dirs"
    ) || downloads::handles(path)
        || hf::handles(path)
}

// ---------------------------------------------------------------------------
// Fonctions pures
// ---------------------------------------------------------------------------

/// `quantFromName` : l'étiquette de quantification la plus longue d'un nom
/// (segments séparés par `-` et `.`), en majuscules ; "" sinon.
pub fn quant_from_name(name: &str) -> String {
    let mut base = name;
    if let Some(dot) = base.rfind('.')
        && base[dot..].eq_ignore_ascii_case(".gguf")
    {
        base = &base[..dot];
    }
    let mut best = "";
    for seg in base.split(['-', '.']).filter(|s| !s.is_empty()) {
        if is_quant_seg(seg) && seg.len() > best.len() {
            best = seg;
        }
    }
    best.to_uppercase()
}

/// `quantSegRe` : `(?i)^(I?Q\d+(_[A-Za-z0-9]+)*|\d+BPW|BF16|FP16|F16|FP32|F32)$`.
fn is_quant_seg(seg: &str) -> bool {
    let u = seg.to_ascii_uppercase();
    if matches!(u.as_str(), "BF16" | "FP16" | "F16" | "FP32" | "F32") {
        return true;
    }
    if let Some(d) = u.strip_suffix("BPW") {
        return !d.is_empty() && d.bytes().all(|c| c.is_ascii_digit());
    }
    let rest = u.strip_prefix("IQ").or_else(|| u.strip_prefix('Q'));
    let Some(rest) = rest else {
        return false;
    };
    let digits = rest.bytes().take_while(u8::is_ascii_digit).count();
    if digits == 0 {
        return false;
    }
    let tail = &rest[digits..];
    if tail.is_empty() {
        return true;
    }
    // (_[A-Za-z0-9]+)*
    tail.strip_prefix('_').is_some_and(|t| {
        t.split('_')
            .all(|p| !p.is_empty() && p.bytes().all(|c| c.is_ascii_alphanumeric()))
    })
}

/// `splitArgs` : découpe EXTRA_ARGS comme un shell simple (guillemets " et ').
pub fn split_args(s: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut cur = String::new();
    let mut quote: Option<char> = None;
    for r in s.chars() {
        match quote {
            Some(q) => {
                if r == q {
                    quote = None;
                    // Guillemets vides : argument vide explicite.
                    if cur.is_empty() {
                        out.push(String::new());
                    }
                } else {
                    cur.push(r);
                }
            }
            None if r == '"' || r == '\'' => quote = Some(r),
            None if matches!(r, ' ' | '\t' | '\n' | '\r') => {
                if !cur.is_empty() {
                    out.push(std::mem::take(&mut cur));
                }
            }
            None => cur.push(r),
        }
    }
    if !cur.is_empty() {
        out.push(cur);
    }
    out
}

/// `os.Stat` sans erreur et pas un dossier.
fn present(p: &str) -> bool {
    std::fs::metadata(p).is_ok_and(|m| !m.is_dir())
}

/// `shardFamilySize` : les tranches présentes dans `dir`.
fn family_size(dir: &str, name: &str) -> i64 {
    gguf::shard_family(name)
        .iter()
        .filter_map(|n| std::fs::metadata(go_join(&[dir, n])).ok())
        .filter(|m| !m.is_dir())
        .map(|m| m.len() as i64)
        .sum()
}

/// `shardFamilyMissing` : les tranches absentes de `dir`.
fn family_missing(dir: &str, name: &str) -> Vec<String> {
    gguf::shard_family(name)
        .into_iter()
        .filter(|n| !present(&go_join(&[dir, n])))
        .collect()
}

// ---------------------------------------------------------------------------
// Qui référence un .gguf (backend_models_refs.go)
// ---------------------------------------------------------------------------

/// `modelRefIndex` : chemins normalisés → presets qui les nomment, et ceux
/// de la configuration en service.
#[derive(Debug, Default)]
pub struct ModelRefs {
    active: HashSet<String>,
    presets: HashMap<String, Vec<String>>,
}

impl ModelDirs {
    /// `modelRefKey` : valeur MODEL=/MMPROJ= → clé d'index ("" : pas un .gguf).
    fn ref_key(&self, v: &str) -> String {
        let v = v.trim();
        if v.is_empty() {
            return String::new();
        }
        let p = gguf::resolve_serve_model_path(v, &self.home, &self.stored, self.env.as_deref())
            .map(|p| p.to_string_lossy().into_owned())
            // Introuvable : une clé comparable quand même.
            .unwrap_or_else(|_| go_join(&[&self.models_dir(), &base_name(v)]));
        gguf::norm_dir(&p)
    }

    /// `modelKeysOf` : modèle, projecteur, brouillon, `--mmproj` d'EXTRA_ARGS.
    fn keys_of(&self, env: &HashMap<String, String>) -> Vec<String> {
        let get = |k: &str| env.get(k).cloned().unwrap_or_default();
        let mut vals = vec![get("MODEL"), get("MMPROJ"), get("MODEL_DRAFT")];
        let args = split_args(&get("EXTRA_ARGS"));
        for (i, a) in args.iter().enumerate() {
            if a == "--mmproj" && i + 1 < args.len() {
                vals.push(args[i + 1].clone());
            }
        }
        vals.iter()
            .map(|v| self.ref_key(v))
            .filter(|k| !k.is_empty())
            .collect()
    }
}

impl ModelRefs {
    /// `buildModelRefIndex` : la configuration active et les presets
    /// (`(nom d'affichage, contenu)`, dans l'ordre de la liste).
    pub fn build(
        dirs: &ModelDirs,
        cfg: &HashMap<String, String>,
        presets: &[(String, String)],
    ) -> Self {
        let mut ix = ModelRefs::default();
        ix.active.extend(dirs.keys_of(cfg));
        for (name, content) in presets {
            let mut seen = HashSet::new();
            for k in dirs.keys_of(&presets::parse_env(content)) {
                if seen.insert(k.clone()) {
                    ix.presets.entry(k).or_default().push(name.clone());
                }
            }
        }
        ix
    }

    /// `lookup` : presets qui référencent `path`, et s'il est en service.
    pub fn lookup(&self, path: &str) -> (Vec<String>, bool) {
        let k = gguf::norm_dir(path);
        (
            self.presets.get(&k).cloned().unwrap_or_default(),
            self.active.contains(&k),
        )
    }

    /// `modelUsers` : pour UN modèle (nom de fichier ou chemin).
    fn users(&self, dirs: &ModelDirs, name: &str) -> (Vec<String>, bool) {
        let k = dirs.ref_key(name);
        if k.is_empty() {
            return (Vec::new(), false);
        }
        self.lookup(&k)
    }
}

const MODEL_IN_SERVICE: &str =
    "modèle en service — bascule sur un autre preset avant de le supprimer";

/// `handleModels` : les `.gguf` des dossiers de modèles (tranches 2..N
/// exclues), avec leurs références.
pub fn list_models(dirs: &ModelDirs, refs: &ModelRefs) -> Value {
    let home = dirs.models_dir();
    let mut out = Vec::new();
    let mut seen = HashSet::new();
    for dir in dirs.all() {
        let Ok(rd) = std::fs::read_dir(&dir) else {
            continue;
        };
        // os.ReadDir : trié par nom.
        let mut entries: Vec<_> = rd.filter_map(Result::ok).collect();
        entries.sort_by_key(|e| e.file_name());
        let is_home = gguf::norm_dir(&dir) == gguf::norm_dir(&home);
        for e in entries {
            let name = e.file_name().to_string_lossy().into_owned();
            let is_dir = e.file_type().map(|t| t.is_dir()).unwrap_or(false);
            if is_dir || !name.to_lowercase().ends_with(".gguf") || gguf::is_follower_shard(&name) {
                continue;
            }
            let full = go_join(&[&dir, &name]);
            if !seen.insert(gguf::norm_dir(&full)) {
                continue;
            }
            let size = e.metadata().map(|m| m.len() as i64).unwrap_or(0);
            let value = if is_home { name.clone() } else { full.clone() };
            let mut m = Map::new();
            m.insert("name".into(), json!(name));
            m.insert("size".into(), json!(size));
            m.insert("value".into(), json!(value));
            m.insert("path".into(), json!(full));
            m.insert("dir".into(), json!(dir));
            m.insert("home".into(), json!(is_home));
            let (users, active) = refs.lookup(&full);
            if !users.is_empty() || active {
                if !users.is_empty() {
                    m.insert("used".into(), json!(users));
                }
                m.insert("active".into(), json!(active));
            }
            if let Some((_, total)) = gguf::shard_info(&name) {
                m.insert("shards".into(), json!(total));
                m.insert("size".into(), json!(family_size(&dir, &name)));
                let missing = family_missing(&dir, &name);
                if !missing.is_empty() {
                    m.insert("missing".into(), json!(missing));
                }
            }
            out.push(Value::Object(m));
        }
    }
    Value::Array(out)
}

/// `modelDirCount` : les `.gguf` lisibles d'un dossier (-1 : illisible).
pub fn dir_count(dir: &str) -> i64 {
    let Ok(rd) = std::fs::read_dir(dir) else {
        return -1;
    };
    rd.filter_map(Result::ok)
        .filter(|e| {
            let n = e.file_name().to_string_lossy().into_owned();
            !e.file_type().map(|t| t.is_dir()).unwrap_or(false)
                && n.to_lowercase().ends_with(".gguf")
                && !gguf::is_follower_shard(&n)
        })
        .count() as i64
}

/// `GET /api/models/dirs`.
pub fn dirs_json(dirs: &ModelDirs) -> Value {
    let home = dirs.models_dir();
    let mut out = vec![json!({
        "path": home, "home": true, "count": dir_count(&home), "exists": true,
        "free": disk_free(&home), "free_exact": disk_free_reliable(&home),
    })];
    for d in gguf::extra_model_dirs(&dirs.home, &dirs.stored, dirs.env.as_deref()) {
        let n = dir_count(&d);
        out.push(json!({
            "path": d, "home": false, "count": n, "exists": n >= 0,
            "free": disk_free(&d), "free_exact": disk_free_reliable(&d),
        }));
    }
    json!({ "ok": true, "dirs": out, "env": dirs.env.as_deref().is_some_and(|v| !v.is_empty()) })
}

/// `POST /api/models/dirs` : la liste à enregistrer, ou le refus.
pub fn dirs_update(dirs: &ModelDirs, path: &str, action: &str) -> Result<Vec<String>, String> {
    let p = path.trim();
    if p.is_empty() {
        return Err("chemin vide".into());
    }
    let mut cur = gguf::extra_model_dirs(&dirs.home, &dirs.stored, dirs.env.as_deref());
    if action == "remove" {
        cur.retain(|d| gguf::norm_dir(d) != gguf::norm_dir(p));
    } else {
        let abs = go_abs(p).ok_or_else(|| "chemin invalide".to_string())?;
        if !std::fs::metadata(&abs).is_ok_and(|m| m.is_dir()) {
            return Err(format!("dossier introuvable : {abs}"));
        }
        cur.push(abs);
    }
    // saveExtraModelDirs : nettoyée, dédoublonnée, models/ exclu.
    Ok(gguf::dedup_dirs(&cur, &dirs.models_dir()))
}

/// `handleModelDelete` : refus si en service, 409 avec la liste des presets
/// sans `force`, sinon le modèle part avec toutes ses tranches.
pub fn delete_model(
    dirs: &ModelDirs,
    refs: &ModelRefs,
    name: &str,
    force: bool,
) -> (StatusCode, Value) {
    let (users, active) = refs.users(dirs, name);
    if active {
        return (
            StatusCode::CONFLICT,
            json!({ "ok": false, "error": MODEL_IN_SERVICE, "active": true }),
        );
    }
    if !users.is_empty() && !force {
        return (
            StatusCode::CONFLICT,
            json!({ "ok": false, "used": users, "error": format!("modèle utilisé par : {}", users.join(", ")) }),
        );
    }
    match delete_model_file(dirs, refs, name) {
        Ok(freed) => {
            let used = if users.is_empty() {
                Value::Null
            } else {
                json!(users)
            };
            (
                StatusCode::OK,
                json!({ "ok": true, "freed": freed, "used": used }),
            )
        }
        Err(e) => (StatusCode::BAD_REQUEST, json!({ "ok": false, "error": e })),
    }
}

/// `deleteModelFile` : rend les octets libérés.
fn delete_model_file(dirs: &ModelDirs, refs: &ModelRefs, name: &str) -> Result<i64, String> {
    let p = gguf::resolve_model_path(name, &dirs.home, &dirs.stored, dirs.env.as_deref())?
        .to_string_lossy()
        .into_owned();
    if refs.users(dirs, name).1 {
        return Err(MODEL_IN_SERVICE.into());
    }
    let dir = go_dir(&p);
    let base = base_name(&p);
    let freed = family_size(&dir, &base);
    if let Err(e) = std::fs::remove_file(&p) {
        if e.kind() == std::io::ErrorKind::NotFound {
            return Err(format!("modèle introuvable: {base}"));
        }
        return Err(crate::paths::os_err("remove", &p, &e));
    }
    for n in gguf::shard_family(&base) {
        let _ = std::fs::remove_file(Path::new(&dir).join(n));
    }
    Ok(freed)
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

/// Les dossiers de modèles du moment, lus en base.
pub async fn model_dirs(ctx: &Ctx) -> ModelDirs {
    let stored = gguf::parse_stored_model_dirs(
        ctx.store
            .get(BK_STATE, gguf::STATE_MODEL_DIRS_KEY)
            .await
            .ok()
            .flatten()
            .as_deref(),
    );
    ModelDirs {
        home: ctx.engine.home.clone(),
        stored,
        env: std::env::var(gguf::MODEL_DIRS_ENV).ok(),
    }
}

/// La configuration en service et les presets (nom, contenu), dans l'ordre
/// de la liste.
async fn refs_inputs(ctx: &Ctx) -> (HashMap<String, String>, Vec<String>) {
    let cfg = ctx.store.config().await;
    let order = presets::parse_preset_order(
        ctx.store
            .get(BK_STATE, presets::PRESET_ORDER_KEY)
            .await
            .ok()
            .flatten()
            .as_deref(),
    );
    (cfg, order)
}

/// L'index des références : configuration et presets lus sur le disque.
pub fn build_refs(dirs: &ModelDirs, cfg: &HashMap<String, String>, order: &[String]) -> ModelRefs {
    let list = presets::list_presets(&dirs.home, cfg, order).unwrap_or_default();
    let presets: Vec<(String, String)> = list
        .into_iter()
        .filter_map(|p| {
            std::fs::read(&p.path)
                .ok()
                .map(|b| (p.name, String::from_utf8_lossy(&b).into_owned()))
        })
        .collect();
    ModelRefs::build(dirs, cfg, &presets)
}

async fn blocking<T: Send + 'static>(f: impl FnOnce() -> T + Send + 'static) -> T {
    tokio::task::spawn_blocking(f)
        .await
        .expect("tâche bloquante interrompue")
}

pub(crate) async fn body_json(body: Incoming) -> Result<Value, String> {
    let b = Limited::new(body, 1 << 20)
        .collect()
        .await
        .map_err(|e| e.to_string())?
        .to_bytes();
    if b.iter().all(u8::is_ascii_whitespace) {
        return Err("EOF".into());
    }
    serde_json::from_slice(&b).map_err(|e| e.to_string())
}

fn bad(e: impl Into<String>) -> Response<Body> {
    send_json(
        StatusCode::BAD_REQUEST,
        &json!({ "ok": false, "error": e.into() }),
    )
}

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(
    ctx: &Ctx,
    models: &Arc<Models>,
    req: Request<Incoming>,
    from_go: bool,
) -> Response<Body> {
    let path = req.uri().path().to_string();
    if downloads::handles(&path) {
        return downloads::serve(ctx, &models.downloads, req, from_go).await;
    }
    if hf::handles(&path) {
        return hf::serve(ctx, &models.hf, req, from_go).await;
    }
    let (parts, body) = req.into_parts();
    if !from_go && let Err(refus) = ctx.guard(&parts.headers).await {
        return *refus;
    }
    let dirs = model_dirs(ctx).await;
    match path.as_str() {
        "/api/models" => {
            let (cfg, order) = refs_inputs(ctx).await;
            let out = blocking(move || {
                let refs = build_refs(&dirs, &cfg, &order);
                list_models(&dirs, &refs)
            })
            .await;
            send_json(StatusCode::OK, &out)
        }
        "/api/models/delete" => {
            let v = match body_json(body).await {
                Ok(v) => v,
                Err(e) => return bad(e),
            };
            let name = v
                .get("name")
                .and_then(Value::as_str)
                .unwrap_or("")
                .to_string();
            let force = v.get("force").and_then(Value::as_bool).unwrap_or(false);
            let (cfg, order) = refs_inputs(ctx).await;
            let (code, out) = blocking(move || {
                let refs = build_refs(&dirs, &cfg, &order);
                delete_model(&dirs, &refs, &name, force)
            })
            .await;
            send_json(code, &out)
        }
        "/api/models/dirs" if parts.method == Method::POST => {
            let v = match body_json(body).await {
                Ok(v) => v,
                Err(e) => return bad(e),
            };
            let p = v
                .get("path")
                .and_then(Value::as_str)
                .unwrap_or("")
                .to_string();
            let action = v
                .get("action")
                .and_then(Value::as_str)
                .unwrap_or("")
                .to_string();
            let res = blocking(move || dirs_update(&dirs, &p, &action)).await;
            match res {
                Ok(list) => {
                    let raw = serde_json::to_vec(&list).unwrap_or_default();
                    match ctx
                        .store
                        .put(BK_STATE, gguf::STATE_MODEL_DIRS_KEY, raw)
                        .await
                    {
                        Ok(()) => send_json(StatusCode::OK, &json!({ "ok": true })),
                        Err(e) => send_json(
                            StatusCode::INTERNAL_SERVER_ERROR,
                            &json!({ "ok": false, "error": e }),
                        ),
                    }
                }
                Err(e) => bad(e),
            }
        }
        "/api/models/dirs" => {
            let out = blocking(move || dirs_json(&dirs)).await;
            send_json(StatusCode::OK, &out)
        }
        _ => send_json(
            StatusCode::NOT_FOUND,
            &json!({ "error": "404 page not found" }),
        ),
    }
}
