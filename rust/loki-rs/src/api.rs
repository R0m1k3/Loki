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
use crate::sysinfo;
use std::collections::BTreeMap;

/// Ce dont les routes ont besoin, partagé par toutes les connexions.
pub struct Ctx {
    pub store: Store,
    pub internal: InternalClient,
    pub host_env: HostEnv,
    /// `$LOKI_HOME` et nom du service moteur (fichiers PID / journal).
    pub engine: engine::Paths,
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
        "/api/ping" | "/api/config" | "/api/engine/cmdline" | "/api/ram" | "/api/vram"
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
        _ => not_found(),
    }
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
