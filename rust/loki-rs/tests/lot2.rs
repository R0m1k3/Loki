//! Lot 2 : UI statique, authentification et /api/ping servis par la façade,
//! devant un faux serveur Go qui imite son API interne.

use bytes::Bytes;
use flate2::read::GzDecoder;
use http_body_util::{BodyExt, Full};
use hyper::body::Incoming;
use hyper::{Request, Response, StatusCode, service::service_fn};
use hyper_util::client::legacy::Client;
use hyper_util::rt::{TokioExecutor, TokioIo};
use std::convert::Infallible;
use std::io::Read;
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};
use tokio::net::TcpListener;
use tokio::sync::oneshot;

const JETON: &str = "0123456789abcdef0123456789abcdef";

/// État du faux Go : clé de pilotage (`None` = base illisible → 503) et
/// journal des requêtes reçues (chemin + en-tête x-loki-internal).
#[derive(Default)]
struct FauxGo {
    cle: Mutex<Option<String>>,
    vus: Mutex<Vec<(String, Option<String>)>>,
}

async fn faux_go(etat: Arc<FauxGo>) -> SocketAddr {
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    tokio::spawn(async move {
        while let Ok((s, _)) = l.accept().await {
            let etat = etat.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<Incoming>| {
                    let etat = etat.clone();
                    async move {
                        let jeton = req
                            .headers()
                            .get("x-loki-internal")
                            .map(|v| v.to_str().unwrap().to_string());
                        let path = req.uri().path().to_string();
                        etat.vus.lock().unwrap().push((path.clone(), jeton.clone()));
                        let (code, body) = if path == "/internal/v1/auth" {
                            if jeton.as_deref() != Some(JETON) {
                                (404, String::new())
                            } else {
                                match etat.cle.lock().unwrap().clone() {
                                    Some(k) => {
                                        (200, format!(r#"{{"web_key":"{k}","version":"0.15.0"}}"#))
                                    }
                                    None => (503, r#"{"error":"x"}"#.into()),
                                }
                            }
                        } else {
                            (200, format!("go:{path}"))
                        };
                        let mut r = Response::new(Full::new(Bytes::from(body)));
                        *r.status_mut() = StatusCode::from_u16(code).unwrap();
                        Ok::<_, Infallible>(r)
                    }
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(s), svc)
                    .await;
            });
        }
    });
    addr
}

struct Banc {
    facade: SocketAddr,
    etat: Arc<FauxGo>,
    _stop: oneshot::Sender<()>,
}

async fn banc(cle: Option<&str>) -> Banc {
    let etat = Arc::new(FauxGo::default());
    *etat.cle.lock().unwrap() = cle.map(String::from);
    let backend = faux_go(etat.clone()).await;
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let facade = l.local_addr().unwrap();
    let (tx, rx) = oneshot::channel::<()>();
    let cfg = loki_rs::proxy::Config {
        backend,
        internal_token: JETON.into(),
        host_env: loki_rs::auth::HostEnv {
            hostname: None,
            trusted: vec!["loki.exemple.fr".into()],
        },
        engine: loki_rs::engine::Paths {
            home: std::env::temp_dir(),
            service: "loki-engine".into(),
        },
        chat: None,
        models: None,
        dictate: None,
        settings: false,
        files: None,
        push: None,
        mem: None,
        presets: false,
        engine_ctl: None,
        system: None,
        projects: false,
        mcp: None,
        update: false,
        backends: None,
        engineimg: None,
        strata: None,
        tasks: None,
    };
    tokio::spawn(loki_rs::proxy::serve(l, cfg, async move {
        let _ = rx.await;
    }));
    Banc {
        facade,
        etat,
        _stop: tx,
    }
}

async fn get(
    b: &Banc,
    path: &str,
    headers: &[(&str, &str)],
) -> (StatusCode, hyper::HeaderMap, Bytes) {
    let mut req = Request::get(format!("http://{}{path}", b.facade));
    for (k, v) in headers {
        req = req.header(*k, *v);
    }
    let client: Client<_, Full<Bytes>> = Client::builder(TokioExecutor::new()).build_http();
    let resp = client
        .request(req.body(Full::new(Bytes::new())).unwrap())
        .await
        .unwrap();
    let (parts, body) = resp.into_parts();
    (
        parts.status,
        parts.headers,
        body.collect().await.unwrap().to_bytes(),
    )
}

#[tokio::test]
async fn ping_sans_cle() {
    let b = banc(Some("")).await;
    let (code, h, body) = get(&b, "/api/ping", &[]).await;
    assert_eq!(code, StatusCode::OK);
    assert_eq!(h["content-type"], "application/json");
    assert_eq!(
        &body[..],
        b"{\"ok\":true,\"service\":\"loki\",\"version\":\"0.15.0\"}\n"
    );
    // Servi par Rust : le Go n'a vu que l'appel à son API interne, jeton compris.
    let vus = b.etat.vus.lock().unwrap().clone();
    assert_eq!(
        vus,
        vec![("/internal/v1/auth".to_string(), Some(JETON.to_string()))]
    );
}

#[tokio::test]
async fn ping_avec_cle() {
    let b = banc(Some("secret")).await;
    let (code, h, body) = get(&b, "/api/ping", &[]).await;
    assert_eq!(code, StatusCode::UNAUTHORIZED);
    assert_eq!(h["www-authenticate"], r#"Bearer realm="loki""#);
    assert_eq!(&body[..], "{\"error\":\"non autorisé\"}\n".as_bytes());
    let (code, _, _) = get(&b, "/api/ping", &[("authorization", "Bearer faux")]).await;
    assert_eq!(code, StatusCode::UNAUTHORIZED);
    let (code, _, _) = get(&b, "/api/ping", &[("authorization", "Bearer secret")]).await;
    assert_eq!(code, StatusCode::OK);
}

#[tokio::test]
async fn ferme_si_la_base_est_illisible() {
    let b = banc(None).await;
    let (code, _, _) = get(&b, "/api/ping", &[]).await;
    assert_eq!(code, StatusCode::SERVICE_UNAVAILABLE);
}

#[tokio::test]
async fn refuse_le_rebinding() {
    let b = banc(Some("")).await;
    let (code, _, body) = get(&b, "/api/ping", &[("host", "evil.example:8090")]).await;
    assert_eq!(code, StatusCode::FORBIDDEN);
    assert!(String::from_utf8_lossy(&body).contains("non autorisé sans clé"));
    let (code, _, _) = get(&b, "/api/ping", &[("host", "loki.exemple.fr")]).await;
    assert_eq!(code, StatusCode::OK);
}

#[tokio::test]
async fn api_interne_fermee_de_l_exterieur() {
    let b = banc(Some("secret")).await;
    let (code, _, body) = get(&b, "/internal/v1/auth", &[("x-loki-internal", JETON)]).await;
    assert_eq!(code, StatusCode::NOT_FOUND);
    assert!(!String::from_utf8_lossy(&body).contains("secret"));
    assert!(
        b.etat.vus.lock().unwrap().is_empty(),
        "requête relayée au Go"
    );
}

#[tokio::test]
async fn jeton_entrant_efface_avant_relais() {
    let b = banc(Some("")).await;
    let (code, _, body) = get(&b, "/api/relayee-au-go", &[("x-loki-internal", JETON)]).await;
    assert_eq!(code, StatusCode::OK);
    assert_eq!(&body[..], b"go:/api/relayee-au-go");
    let vus = b.etat.vus.lock().unwrap().clone();
    assert_eq!(vus, vec![("/api/relayee-au-go".to_string(), None)]);
}

#[tokio::test]
async fn ui_servie_par_rust() {
    let b = banc(Some("")).await;
    let (code, h, body) = get(&b, "/", &[("accept-encoding", "gzip")]).await;
    assert_eq!(code, StatusCode::OK);
    assert_eq!(h["content-encoding"], "gzip");
    assert_eq!(h["cache-control"], "no-store, max-age=0");
    let mut html = Vec::new();
    GzDecoder::new(&body[..]).read_to_end(&mut html).unwrap();
    assert_eq!(html, loki_rs::ui::index_bytes());
    // Police inconnue : relayée au Go (qui rendra son 404).
    let (_, _, body) = get(&b, "/fonts/absente.woff2", &[]).await;
    assert_eq!(&body[..], b"go:/fonts/absente.woff2");
    let vus = b.etat.vus.lock().unwrap().clone();
    assert_eq!(
        vus.len(),
        1,
        "seule la police inconnue doit atteindre le Go : {vus:?}"
    );
}
