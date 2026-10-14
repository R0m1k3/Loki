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
use crate::store::Store;

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
    matches!(path, "/api/ping")
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
