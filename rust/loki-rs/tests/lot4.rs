//! Lot 4 : routes /api portées en Rust, devant un faux Go à store en mémoire.

mod common;

use common::fakego;
use hyper::StatusCode;

#[tokio::test]
async fn config_triee_comme_le_go() {
    let t = tempfile::tempdir().unwrap();
    let (f, go, _stop) = fakego::start(t.path().into()).await;
    go.set("config", "PORT", "8081");
    go.set("config", "CTX", "65536");
    go.set("config", "MODEL", "Qwen.gguf");
    let (code, h, body) = fakego::get(f, "/api/config", &[]).await;
    assert_eq!(code, StatusCode::OK);
    assert_eq!(h["content-type"], "application/json");
    assert_eq!(
        &body[..],
        b"{\"CTX\":\"65536\",\"MODEL\":\"Qwen.gguf\",\"PORT\":\"8081\"}\n"
    );
    assert!(go.relayed.lock().unwrap().is_empty());
}

#[tokio::test]
async fn config_vide_si_base_illisible() {
    let t = tempfile::tempdir().unwrap();
    let (f, go, _stop) = fakego::start(t.path().into()).await;
    go.set("config", "PORT", "8081");
    // Clé lisible, puis base cassée : la garde d'auth ferme (503) — on ne sert
    // jamais une route de pilotage sans avoir pu lire la clé.
    *go.broken.lock().unwrap() = true;
    let (code, _, _) = fakego::get(f, "/api/config", &[]).await;
    assert_eq!(code, StatusCode::SERVICE_UNAVAILABLE);
}

#[tokio::test]
async fn commande_du_moteur() {
    let t = tempfile::tempdir().unwrap();
    let (f, go, _stop) = fakego::start(t.path().into()).await;
    let (_, _, body) = fakego::get(f, "/api/engine/cmdline", &[]).await;
    assert_eq!(&body[..], b"{\"cmdline\":\"\",\"ok\":true}\n");
    go.set(
        "state",
        "engine_cmdline",
        "CUDA_VISIBLE_DEVICES=0 /app/llama-server -m \"a b.gguf\"",
    );
    let (_, _, body) = fakego::get(f, "/api/engine/cmdline", &[]).await;
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(
        v["cmdline"],
        "CUDA_VISIBLE_DEVICES=0 /app/llama-server -m \"a b.gguf\""
    );
}

#[tokio::test]
async fn routes_portees_protegees_par_la_cle() {
    let t = tempfile::tempdir().unwrap();
    let (f, go, _stop) = fakego::start(t.path().into()).await;
    go.set("state", "web_key", "secret");
    for path in ["/api/config", "/api/engine/cmdline", "/api/ping"] {
        let (code, _, _) = fakego::get(f, path, &[]).await;
        assert_eq!(code, StatusCode::UNAUTHORIZED, "{path}");
        let (code, _, _) = fakego::get(f, path, &[("authorization", "Bearer secret")]).await;
        assert_eq!(code, StatusCode::OK, "{path}");
    }
}

#[tokio::test]
async fn ram_et_vram() {
    let t = tempfile::tempdir().unwrap();
    let (f, go, _stop) = fakego::start(t.path().into()).await;
    let (code, _, body) = fakego::get(f, "/api/ram", &[]).await;
    assert_eq!(code, StatusCode::OK);
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert!(v["used"].is_i64() && v["total"].is_i64(), "{v}");
    let (code, _, body) = fakego::get(f, "/api/vram", &[]).await;
    assert_eq!(code, StatusCode::OK);
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert!(v.is_array(), "{v}");
    assert!(go.relayed.lock().unwrap().is_empty());
}
