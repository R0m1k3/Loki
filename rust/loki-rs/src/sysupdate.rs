//! Mise à jour de Loki tenue par la façade (lot 21 de la migration,
//! `LOKI_UPDATE=rust` ; expérimental, le Go reste le défaut) — port de
//! `handleUpdateCheck` et `handleUpdateApply` (`sys_update.go`) en conteneur.
//!
//! La façade ne tourne qu'en conteneur (`LOKI_CONTAINER=1`), où l'image se met
//! à jour par Docker : les deux routes le disent, sans rien télécharger. Les
//! binaires de la façade, eux, suivent `loki-rs self-update`. Hors conteneur,
//! la façade ne tient pas ce lot (décidé au démarrage).

use hyper::body::Incoming;
use hyper::{Request, Response, StatusCode};
use serde_json::json;

use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::proxy::Body;

/// `containerUpdateNote`.
pub const CONTAINER_UPDATE_NOTE: &str =
    "Mise à jour par l'image Docker : docker compose pull && docker compose up -d";

/// La façade peut-elle tenir ce lot (`containerManaged`) ?
pub fn usable(container_env: Option<&str>) -> bool {
    container_env == Some("1")
}

/// Les routes servies par ce module.
pub fn handles(path: &str) -> bool {
    matches!(path, "/api/update" | "/api/update/apply")
}

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(ctx: &Ctx, req: Request<Incoming>, from_go: bool) -> Response<Body> {
    let version = if from_go {
        ctx.internal
            .auth()
            .await
            .map(|i| i.version)
            .unwrap_or_default()
    } else {
        match ctx.guard(req.headers()).await {
            Ok(v) => v,
            Err(refus) => return *refus,
        }
    };
    if req.uri().path() == "/api/update" {
        return send_json(
            StatusCode::OK,
            &json!({ "current": version, "available": false, "note": CONTAINER_UPDATE_NOTE }),
        );
    }
    send_json(
        StatusCode::CONFLICT,
        &json!({ "ok": false, "error": CONTAINER_UPDATE_NOTE }),
    )
}
