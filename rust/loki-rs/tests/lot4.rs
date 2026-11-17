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

fn json(body: &[u8]) -> serde_json::Value {
    serde_json::from_slice(body).unwrap()
}

#[tokio::test]
async fn statut_moteur_arrete_puis_preset_externe() {
    let t = tempfile::tempdir().unwrap();
    let (f, go, _stop) = fakego::start(t.path().into()).await;
    go.set("config", "CTX", "65536");
    go.set("config", "PORT", "8099");
    let (code, _, body) = fakego::get(f, "/api/status", &[]).await;
    assert_eq!(code, StatusCode::OK);
    let v = json(&body);
    assert_eq!(v["state"], "inactive");
    assert_eq!(v["active"], false);
    assert_eq!(v["health"], false);
    assert_eq!(v["ctx"], 65536);
    assert_eq!(v["port"], 8099);
    assert_eq!(v["version"], "0.15.0");
    assert_eq!(v["load_pct"], -1);
    assert_eq!(v["external"], false);
    for k in ["warn", "load_error", "preset", "hint"] {
        assert!(v[k].is_string(), "{k} : {v}");
    }
    // Preset externe : prêt sans moteur local.
    go.set("config", "EXTERNAL", "1");
    let (_, _, body) = fakego::get(f, "/api/status", &[]).await;
    let v = json(&body);
    assert_eq!(
        (v["active"].clone(), v["health"].clone(), v["state"].clone()),
        (true.into(), true.into(), "active".into())
    );
    assert_eq!(v["external"], true);
}

#[tokio::test]
async fn statut_explique_un_echec_de_chargement() {
    let t = tempfile::tempdir().unwrap();
    let (f, _go, _stop) = fakego::start(t.path().into()).await;
    std::fs::write(
        t.path().join("loki-engine.log"),
        "llama_model_load: loading model\nllama_model_load: error loading model: unknown model architecture: 'qwen9'\n",
    )
    .unwrap();
    let (_, _, body) = fakego::get(f, "/api/status", &[]).await;
    let v = json(&body);
    assert!(
        v["load_error"]
            .as_str()
            .unwrap()
            .starts_with("Modèle incompatible avec le moteur"),
        "{v}"
    );
    let (_, _, body) = fakego::get(f, "/api/service/log?n=1", &[]).await;
    assert_eq!(
        json(&body)["log"],
        "llama_model_load: error loading model: unknown model architecture: 'qwen9'\n"
    );
}

#[tokio::test]
async fn emplacements_et_couches() {
    let t = tempfile::tempdir().unwrap();
    let (f, _go, _stop) = fakego::start(t.path().into()).await;
    let (code, _, body) = fakego::get(f, "/api/paths", &[]).await;
    assert_eq!(code, StatusCode::OK);
    let v = json(&body);
    for k in [
        "home",
        "database",
        "exe",
        "installed",
        "workspace",
        "memory",
        "scripts",
        "presets",
        "models",
        "backends",
    ] {
        assert!(v[k].is_string(), "{k} : {v}");
    }
    let (_, _, body) = fakego::get(f, "/api/model/layers", &[]).await;
    assert_eq!(
        &body[..],
        b"{\"error\":\"aucun mod\xc3\xa8le\",\"ok\":false}\n"
    );
}

/// Faux llama-server en veille : `/health` 200 sans clé, `/props` avec
/// `is_sleeping` seulement sous la clé `k` (comme le vrai : seule `/health`
/// est publique).
async fn moteur_en_veille() -> u16 {
    use bytes::Bytes;
    use http_body_util::Full;
    use hyper::{Request, Response, body::Incoming, service::service_fn};
    use hyper_util::rt::TokioIo;
    let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = l.local_addr().unwrap().port();
    tokio::spawn(async move {
        while let Ok((s, _)) = l.accept().await {
            tokio::spawn(async move {
                let svc = service_fn(|req: Request<Incoming>| async move {
                    let auth = req
                        .headers()
                        .get("authorization")
                        .and_then(|v| v.to_str().ok())
                        == Some("Bearer k");
                    let (code, body): (u16, &'static [u8]) = match req.uri().path() {
                        "/health" => (200, b"{\"status\":\"ok\"}"),
                        "/props" if auth => (200, b"{\"is_sleeping\":true,\"total_slots\":1}"),
                        "/props" => (401, b"{}"),
                        _ => (404, b"{}"),
                    };
                    let mut r = Response::new(Full::new(Bytes::from_static(body)));
                    *r.status_mut() = StatusCode::from_u16(code).unwrap();
                    Ok::<_, std::convert::Infallible>(r)
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(s), svc)
                    .await;
            });
        }
    });
    port
}

#[tokio::test]
async fn statut_modele_en_veille() {
    let t = tempfile::tempdir().unwrap();
    let (f, go, _stop) = fakego::start(t.path().into()).await;
    // Moteur arrêté : pas en veille.
    let (_, _, body) = fakego::get(f, "/api/status", &[]).await;
    assert_eq!(json(&body)["sleeping"], false);
    // Le moteur tourne (PID de ce processus) et dort.
    std::fs::write(
        t.path().join("loki-engine.pid"),
        format!("{}\n", std::process::id()),
    )
    .unwrap();
    go.set("config", "PORT", &moteur_en_veille().await.to_string());
    go.set("state", "api_key", "k");
    let (_, _, body) = fakego::get(f, "/api/status", &[]).await;
    let v = json(&body);
    assert_eq!(
        (v["health"].clone(), v["sleeping"].clone()),
        (true.into(), true.into()),
        "{v}"
    );
    // Sans la bonne clé, /props refuse : « pas en veille », jamais d'erreur.
    go.set("state", "api_key", "autre");
    let (_, _, body) = fakego::get(f, "/api/status", &[]).await;
    let v = json(&body);
    assert_eq!(
        (v["health"].clone(), v["sleeping"].clone()),
        (true.into(), false.into()),
        "{v}"
    );
}
