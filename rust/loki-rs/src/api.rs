//! Routes `/api/*` servies par Rust. Toute route listée dans [`handles`] est
//! traitée ici ; les autres partent au Go.
//!
//! Chaque route de pilotage passe par [`Ctx::guard`], port de
//! `requireWebAuth` (clé relue à chaque requête, fermeture sur erreur).

use bytes::Bytes;
use hyper::body::Incoming;
use hyper::header::HeaderMap;
use hyper::{Method, Request, Response, StatusCode};
use serde_json::json;

use crate::auth::{self, HostEnv, InternalClient};
use crate::engine;
use crate::proxy::Body;
use crate::store::{BK_STATE, Store};
use crate::{engstatus, gguf, paths, presets, sysinfo};
use std::collections::BTreeMap;

/// Ce dont les routes ont besoin, partagé par toutes les connexions.
pub struct Ctx {
    pub store: Store,
    pub internal: InternalClient,
    pub host_env: HostEnv,
    /// `$LOKI_HOME` et nom du service moteur (fichiers PID / journal).
    pub engine: engine::Paths,
    /// Conversation tenue par la façade (`LOKI_CHAT=rust`) ; `None` : au Go.
    pub chat: Option<std::sync::Arc<crate::chat::generate::ChatService>>,
    /// Routes des modèles (liste, dossiers, suppression, téléchargements,
    /// catalogue Hugging Face) tenues par la façade (`LOKI_MODELS=rust`) ;
    /// `None` : au Go.
    pub models: Option<std::sync::Arc<crate::models::Models>>,
    /// Dictée vocale tenue par la façade (`LOKI_DICTATE=rust`) ; `None` : au Go.
    pub dictate: Option<std::sync::Arc<crate::dictate::Dictation>>,
    /// Réglages partagés tenus par la façade (`LOKI_SETTINGS=rust`).
    pub settings: bool,
    /// Fichiers de la discussion tenus par la façade (`LOKI_FILES=rust`).
    pub files: Option<std::sync::Arc<crate::convfiles::ConvFiles>>,
    /// Notifications Web Push tenues par la façade (`LOKI_PUSH=rust`).
    pub push: Option<std::sync::Arc<crate::push::Push>>,
    /// Jeton de l'API interne : une requête qui le porte vient du Go.
    pub internal_token: String,
}

impl Ctx {
    /// La façade tient-elle au moins un lot (le Go lui remet alors le trafic
    /// de ce lot qui lui arrive par le tunnel) ?
    pub fn holds_any(&self) -> bool {
        self.chat.is_some()
            || self.models.is_some()
            || self.dictate.is_some()
            || self.settings
            || self.files.is_some()
            || self.push.is_some()
    }

    /// La requête vient du Go (jeton interne valide) : déjà authentifiée par
    /// lui (tunnel du relais, redistribution E2E).
    pub fn from_go(&self, headers: &HeaderMap) -> bool {
        !self.internal_token.is_empty()
            && headers.get(crate::auth::INTERNAL_HEADER).is_some_and(|v| {
                auth::constant_time_eq(v.as_bytes(), self.internal_token.as_bytes())
            })
    }
}

impl Ctx {
    /// `Ok` = requête autorisée (avec la version du Go, pour `/api/ping`) ;
    /// sinon la réponse de refus.
    pub async fn guard(&self, headers: &HeaderMap) -> Result<String, Box<Response<Body>>> {
        let info = self.internal.auth().await;
        if let Some(refus) = auth::authorize(headers, &info, &self.host_env) {
            return Err(Box::new(refus));
        }
        Ok(info.map(|i| i.version).unwrap_or_default())
    }
}

/// Cette route est-elle servie par Rust ?
pub fn handles(_method: &Method, path: &str) -> bool {
    matches!(
        path,
        "/api/ping"
            | "/api/config"
            | "/api/engine/cmdline"
            | "/api/ram"
            | "/api/vram"
            | "/api/status"
            | "/api/service/log"
            | "/api/paths"
            | "/api/model/layers"
    )
}

pub async fn serve(ctx: &Ctx, req: Request<Incoming>) -> Response<Body> {
    let (parts, _body) = req.into_parts();
    let version = match ctx.guard(&parts.headers).await {
        Ok(v) => v,
        Err(refus) => return *refus,
    };
    match parts.uri.path() {
        // Vérifie la connectivité ET la clé (200 = bonne clé, 401 = mauvaise).
        "/api/ping" => auth::json(
            StatusCode::OK,
            &json!({ "ok": true, "service": "loki", "version": version }),
        ),
        // ReadConfig() du Go : json.Encoder trie les clés d'une map, d'où le
        // BTreeMap (un HashMap sortirait dans un ordre aléatoire).
        "/api/config" => {
            let cfg: BTreeMap<String, String> = ctx.store.config().await.into_iter().collect();
            auth::json(StatusCode::OK, &json!(cfg))
        }
        // Commande du dernier lancement du moteur (engine_cmdline.go).
        "/api/engine/cmdline" => {
            let cmdline = ctx.store.get_str(BK_STATE, "engine_cmdline").await;
            auth::json(StatusCode::OK, &json!({ "ok": true, "cmdline": cmdline }))
        }
        // RAM système en Mo (total = 0 : l'UI masque le bloc).
        "/api/ram" => {
            let (used, total) = sysinfo::ram_usage_mb();
            auth::json(StatusCode::OK, &json!({ "used": used, "total": total }))
        }
        // Jauges des cartes : un seul nvidia-smi partagé par tous les onglets.
        "/api/vram" => {
            let gpus: Vec<_> = sysinfo::gpu_stats_cached()
                .await
                .into_iter()
                .map(|g| {
                    json!({ "name": g.name, "used": g.used, "total": g.total, "util": g.util, "temp": g.temp })
                })
                .collect();
            auth::json(StatusCode::OK, &json!(gpus))
        }
        "/api/status" => status(ctx, version).await,
        // Fin du journal du moteur : POURQUOI un modèle ne charge pas.
        "/api/service/log" => {
            let n = query_param(&parts.uri, "n")
                .and_then(|v| v.parse::<usize>().ok())
                .filter(|k| (1..=500).contains(k))
                .unwrap_or(80);
            let paths = ctx.engine.clone();
            let log = blocking(move || engstatus::service_log_tail(&paths, n)).await;
            auth::json(StatusCode::OK, &json!({ "log": log }))
        }
        // Bloc « Emplacements » de l'UI.
        "/api/paths" => {
            let project = ctx
                .store
                .get_str(BK_STATE, paths::ST_ACTIVE_PROJECT_KEY)
                .await;
            let home = ctx.engine.home.clone();
            let p = blocking(move || {
                let exe = std::env::current_exe().unwrap_or_default();
                let go_exe = crate::child::go_bin(std::env::var("LOKI_GO_BIN").ok(), &exe);
                let ws = std::env::var(paths::WORKSPACE_ENV).ok();
                paths::current_paths(&home, &go_exe, &project, ws.as_deref())
            })
            .await;
            auth::json(StatusCode::OK, &p)
        }
        // Nombre de couches du modèle, lu dans l'en-tête GGUF.
        "/api/model/layers" => {
            let q = query_param(&parts.uri, "model");
            let cfg_model = ctx.store.config().await.remove("MODEL");
            let stored = stored_model_dirs(ctx).await;
            let home = ctx.engine.home.clone();
            let v = blocking(move || {
                let env = std::env::var(gguf::MODEL_DIRS_ENV).ok();
                gguf::model_layers_json(
                    q.as_deref(),
                    cfg_model.as_deref(),
                    &home,
                    &stored,
                    env.as_deref(),
                )
            })
            .await;
            auth::json(StatusCode::OK, &v)
        }
        _ => not_found(),
    }
}

/// `handleStatus` : état du moteur pour l'en-tête de l'UI.
async fn status(ctx: &Ctx, version: String) -> Response<Body> {
    let cfg = ctx.store.config().await;
    let stored = stored_model_dirs(ctx).await;
    let order = presets::parse_preset_order(
        ctx.store
            .get(BK_STATE, presets::PRESET_ORDER_KEY)
            .await
            .ok()
            .flatten()
            .as_deref(),
    );
    let paths = ctx.engine.clone();
    let pid = engine::read_pid(&paths.pid());
    let mut active = pid_alive(pid);
    let mut state = if active { "active" } else { "inactive" };
    let mut health = active && engstatus::health_check(&cfg).await;
    // Preset externe : pas de moteur local, mais le chat EST prêt.
    let external = engstatus::is_external(&cfg);
    if external {
        (active, state, health) = (true, "active", true);
    }
    let ctx_len = cfg
        .get("CTX")
        .and_then(|v| v.parse::<i64>().ok())
        .filter(|n| *n > 0)
        .unwrap_or(32768);
    let port = engstatus::llm_port(&cfg);
    let home = paths.home.clone();
    let (load_err, load_pct, preset, data_warn) = {
        let (cfg, stored) = (cfg.clone(), stored.clone());
        blocking(move || {
            let mut load_err = String::new();
            let mut load_pct = -1;
            if !health {
                load_err =
                    engstatus::model_load_error_from(&engstatus::service_log_tail(&paths, 200));
                if active && load_err.is_empty() {
                    load_pct = engstatus::engine_load_pct_dirs(&paths, &cfg, &stored);
                }
            }
            let preset = presets::active_preset_id(&home, &cfg, &order);
            (load_err, load_pct, preset, sysinfo::data_volume_warning())
        })
        .await
    };
    // appWarning() n'a de contenu que sous macOS : en conteneur, seule
    // l'alerte de volume compte.
    let warn = data_warn;
    let home = ctx.engine.home.clone();
    let hint = sysinfo::storage_fuse_hint(move || async move {
        let env = std::env::var(gguf::MODEL_DIRS_ENV).ok();
        gguf::model_dirs(&home, &stored, env.as_deref())
    })
    .await;
    auth::json(
        StatusCode::OK,
        &json!({
            "state": state,
            "active": active,
            "health": health,
            "port": port,
            "ctx": ctx_len,
            "version": version,
            "warn": warn,
            "load_error": load_err,
            "load_pct": load_pct,
            "external": external,
            "preset": preset,
            "hint": hint,
        }),
    )
}

/// Dossiers de modèles déclarés dans l'UI (`state/model_dirs`).
async fn stored_model_dirs(ctx: &Ctx) -> Vec<String> {
    gguf::parse_stored_model_dirs(
        ctx.store
            .get(BK_STATE, gguf::STATE_MODEL_DIRS_KEY)
            .await
            .ok()
            .flatten()
            .as_deref(),
    )
}

/// E/S de fichiers hors du runtime asynchrone.
async fn blocking<T: Send + 'static>(f: impl FnOnce() -> T + Send + 'static) -> T {
    tokio::task::spawn_blocking(f)
        .await
        .expect("tâche bloquante interrompue")
}

#[cfg(target_os = "linux")]
fn pid_alive(pid: i32) -> bool {
    engine::proctree::linux::pid_alive(pid)
}

#[cfg(not(target_os = "linux"))]
fn pid_alive(_: i32) -> bool {
    false
}

/// Paramètre de requête décodé comme `url.Query().Get` (`+` = espace).
pub fn query_param(uri: &hyper::Uri, key: &str) -> Option<String> {
    uri.query()?.split('&').find_map(|kv| {
        let (k, v) = kv.split_once('=').unwrap_or((kv, ""));
        (pct_decode(k) == key).then(|| pct_decode(v))
    })
}

fn pct_decode(s: &str) -> String {
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
                        i += 2;
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

fn not_found() -> Response<Body> {
    auth::json(StatusCode::NOT_FOUND, &json!({ "error": "route inconnue" }))
}

/// Lecture bornée du corps d'une requête (routes POST portées).
#[allow(dead_code)]
pub async fn read_body(body: Incoming, max: usize) -> Result<Bytes, String> {
    use http_body_util::{BodyExt, Limited};
    Limited::new(body, max)
        .collect()
        .await
        .map(|c| c.to_bytes())
        .map_err(|e| e.to_string())
}
