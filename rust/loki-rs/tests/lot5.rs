//! Lot 5 : /v1 relayé par la façade directement au moteur, décisions au Go.

mod common;

use bytes::Bytes;
use common::fakego;
use http_body_util::{BodyExt, Full, StreamBody};
use hyper::body::{Frame, Incoming};
use hyper::{Request, Response, StatusCode, service::service_fn};
use hyper_util::client::legacy::Client;
use hyper_util::rt::{TokioExecutor, TokioIo};
use std::convert::Infallible;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};
use tokio::net::TcpListener;

type B = http_body_util::combinators::BoxBody<Bytes, Infallible>;

/// Faux moteur : /v1/models renvoie la requête vue (en-têtes + corps),
/// /v1/chat/completions diffuse deux événements à 500 ms d'écart.
async fn faux_moteur(vu: Arc<Mutex<Vec<String>>>) -> u16 {
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = l.local_addr().unwrap().port();
    tokio::spawn(async move {
        while let Ok((s, _)) = l.accept().await {
            let vu = vu.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<Incoming>| {
                    let vu = vu.clone();
                    async move {
                        let path = req.uri().path().to_string();
                        let mut lines: Vec<String> = req
                            .headers()
                            .iter()
                            .map(|(k, v)| format!("{k}: {}", v.to_str().unwrap()))
                            .collect();
                        let body = req.into_body().collect().await.unwrap().to_bytes();
                        lines.push(format!("body: {}", String::from_utf8_lossy(&body)));
                        vu.lock().unwrap().push(lines.join("\n"));
                        let r: Response<B> = if path == "/v1/chat/completions" {
                            let (tx, rx) = tokio::sync::mpsc::channel::<Bytes>(2);
                            tokio::spawn(async move {
                                tx.send(Bytes::from_static(b"data: a\n\n")).await.unwrap();
                                tokio::time::sleep(Duration::from_millis(500)).await;
                                let _ = tx.send(Bytes::from_static(b"data: b\n\n")).await;
                            });
                            let s = futures_util::stream::unfold(rx, |mut rx| async move {
                                rx.recv()
                                    .await
                                    .map(|b| (Ok::<_, Infallible>(Frame::data(b)), rx))
                            });
                            Response::new(BodyExt::boxed(StreamBody::new(s)))
                        } else {
                            Response::new(Full::new(Bytes::from_static(b"{\"data\":[]}")).boxed())
                        };
                        Ok::<_, Infallible>(r)
                    }
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(s), svc)
                    .await;
            });
        }
    });
    port
}

async fn send(
    facade: std::net::SocketAddr,
    method: &str,
    path: &str,
    headers: &[(&str, &str)],
    body: &str,
) -> Response<Incoming> {
    let mut req = Request::builder()
        .method(method)
        .uri(format!("http://{facade}{path}"));
    for (k, v) in headers {
        req = req.header(*k, *v);
    }
    let client: Client<_, Full<Bytes>> = Client::builder(TokioExecutor::new()).build_http();
    client
        .request(req.body(Full::new(Bytes::from(body.to_string()))).unwrap())
        .await
        .unwrap()
}

async fn attendre_end(go: &fakego::FakeGo) {
    for _ in 0..50 {
        if go
            .oai_log
            .lock()
            .unwrap()
            .iter()
            .any(|l| l.starts_with("end"))
        {
            return;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
}

#[tokio::test]
async fn relais_et_ticket() {
    let t = tempfile::tempdir().unwrap();
    let (f, go, _stop) = fakego::start(t.path().into()).await;
    let vu = Arc::new(Mutex::new(Vec::new()));
    *go.engine_port.lock().unwrap() = faux_moteur(vu.clone()).await;
    let r = send(
        f,
        "GET",
        "/v1/models",
        &[("host", "loki.exemple.fr"), ("x-forwarded-for", "6.6.6.6")],
        "",
    )
    .await;
    assert_eq!(r.status(), StatusCode::OK);
    let body = r.into_body().collect().await.unwrap().to_bytes();
    assert_eq!(&body[..], b"{\"data\":[]}");
    attendre_end(&go).await;
    assert_eq!(
        *go.oai_log.lock().unwrap(),
        vec!["begin read=true side=false post_v1=false", "end 7"]
    );
    let vu = vu.lock().unwrap()[0].clone();
    assert!(vu.contains("host: loki.exemple.fr"), "{vu}");
    assert!(!vu.contains("x-forwarded-for"), "{vu}");
    assert!(go.relayed.lock().unwrap().is_empty(), "/v1 relayé au Go");
}

#[tokio::test]
async fn flux_sans_tampon_ticket_rendu_a_la_fin() {
    let t = tempfile::tempdir().unwrap();
    let (f, go, _stop) = fakego::start(t.path().into()).await;
    *go.engine_port.lock().unwrap() = faux_moteur(Arc::new(Mutex::new(Vec::new()))).await;
    let t0 = Instant::now();
    let r = send(f, "POST", "/v1/chat/completions", &[], r#"{"stream":true}"#).await;
    let mut body = r.into_body();
    let first = body.frame().await.unwrap().unwrap().into_data().unwrap();
    assert_eq!(&first[..], b"data: a\n\n");
    assert!(
        t0.elapsed() < Duration::from_millis(300),
        "{:?}",
        t0.elapsed()
    );
    // Flux en cours : pas encore de end.
    assert!(
        !go.oai_log
            .lock()
            .unwrap()
            .iter()
            .any(|l| l.starts_with("end"))
    );
    while body.frame().await.is_some() {}
    attendre_end(&go).await;
    assert_eq!(
        *go.oai_log.lock().unwrap(),
        vec!["begin read=false side=false post_v1=true", "end 7"]
    );
}

#[tokio::test]
async fn second_slot_reecrit_le_corps() {
    let t = tempfile::tempdir().unwrap();
    let (f, go, _stop) = fakego::start(t.path().into()).await;
    let vu = Arc::new(Mutex::new(Vec::new()));
    *go.engine_port.lock().unwrap() = faux_moteur(vu.clone()).await;
    *go.side_live.lock().unwrap() = true;
    let r = send(f, "POST", "/v1/models", &[], r#"{"model":"m","id_slot":0}"#).await;
    assert_eq!(r.status(), StatusCode::OK);
    let vu = vu.lock().unwrap()[0].clone();
    assert!(vu.contains(r#"body: {"id_slot":1,"model":"m"}"#), "{vu}");
    assert!(go.oai_log.lock().unwrap()[0].contains("side=true"));
    // Corps trop gros pour le second slot (max_body = 1024 dans le faux Go).
    let gros = format!(r#"{{"x":"{}"}}"#, "a".repeat(2000));
    let r = send(f, "POST", "/v1/models", &[], &gros).await;
    assert_eq!(r.status(), StatusCode::PAYLOAD_TOO_LARGE);
}

#[tokio::test]
async fn cle_des_completions() {
    let t = tempfile::tempdir().unwrap();
    let (f, go, _stop) = fakego::start(t.path().into()).await;
    *go.engine_port.lock().unwrap() = faux_moteur(Arc::new(Mutex::new(Vec::new()))).await;
    go.set("state", "api_key", "cle-oai");
    let r = send(f, "GET", "/v1/models", &[], "").await;
    assert_eq!(r.status(), StatusCode::UNAUTHORIZED);
    assert_eq!(r.headers()["www-authenticate"], r#"Bearer realm="loki""#);
    let body = r.into_body().collect().await.unwrap().to_bytes();
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(v["error"]["code"], "invalid_api_key");
    assert!(
        go.oai_log.lock().unwrap().is_empty(),
        "begin sans authentification"
    );
    let r = send(
        f,
        "GET",
        "/v1/models",
        &[("authorization", "Bearer cle-oai")],
        "",
    )
    .await;
    assert_eq!(r.status(), StatusCode::OK);
    // Préflight CORS : jamais d'Authorization, jamais refusé.
    let r = send(f, "OPTIONS", "/v1/models", &[], "").await;
    assert_eq!(r.status(), StatusCode::OK);
}

#[tokio::test]
async fn refus_benchmark() {
    let t = tempfile::tempdir().unwrap();
    let (f, go, _stop) = fakego::start(t.path().into()).await;
    *go.engine_port.lock().unwrap() = faux_moteur(Arc::new(Mutex::new(Vec::new()))).await;
    *go.refuse.lock().unwrap() = Some(serde_json::json!({
        "status": 503, "message": "benchmark en cours sur ce moteur — réessaie dans un moment",
        "type": "server_error", "code": "benchmark_running", "retry_after": "30"
    }));
    let r = send(f, "POST", "/v1/chat/completions", &[], "{}").await;
    assert_eq!(r.status(), StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(r.headers()["retry-after"], "30");
}

#[tokio::test]
async fn moteur_absent() {
    let t = tempfile::tempdir().unwrap();
    let (f, go, _stop) = fakego::start(t.path().into()).await;
    let mort = TcpListener::bind("127.0.0.1:0")
        .await
        .unwrap()
        .local_addr()
        .unwrap()
        .port();
    *go.engine_port.lock().unwrap() = mort;
    let r = send(f, "GET", "/v1/models", &[], "").await;
    assert_eq!(r.status(), StatusCode::BAD_GATEWAY);
    attendre_end(&go).await;
    assert!(
        go.oai_log.lock().unwrap().iter().any(|l| l == "end 7"),
        "ticket non rendu"
    );
}
