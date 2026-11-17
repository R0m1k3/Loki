//! `/v1/models` liste les presets d'au moins 64 000 jetons ; une complétion
//! qui nomme un autre preset bascule le moteur (avec une clé) puis relaie.

mod common;

use bytes::Bytes;
use common::fakego;
use http_body_util::{BodyExt, Full};
use hyper::body::Incoming;
use hyper::{Request, Response, StatusCode, service::service_fn};
use hyper_util::client::legacy::Client;
use hyper_util::rt::{TokioExecutor, TokioIo};
use std::convert::Infallible;
use std::sync::atomic::Ordering;
use std::sync::{Arc, Mutex};
use tokio::net::TcpListener;

const CLE: &str = "cle-oai";

/// Faux moteur : note « chemin corps » et répond `{"ok":1}`.
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
                        let body = req.into_body().collect().await.unwrap().to_bytes();
                        vu.lock()
                            .unwrap()
                            .push(format!("{path} {}", String::from_utf8_lossy(&body)));
                        Ok::<_, Infallible>(Response::new(Full::new(Bytes::from_static(
                            b"{\"ok\":1}",
                        ))))
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

struct Banc {
    f: std::net::SocketAddr,
    go: Arc<fakego::FakeGo>,
    vu: Arc<Mutex<Vec<String>>>,
    _stop: tokio::sync::oneshot::Sender<()>,
    _home: tempfile::TempDir,
}

/// Presets : `a` (64K, en service), `b` (32K), `ext` (API externe, 128K),
/// `c` (128K). La clé de complétion est posée si `cle`.
async fn banc(cle: bool) -> Banc {
    let home = tempfile::tempdir().unwrap();
    let dir = home.path().join("presets");
    std::fs::create_dir_all(&dir).unwrap();
    for (id, body) in [
        ("a", "MODEL=a.gguf\nCTX=65536\n"),
        ("b", "MODEL=b.gguf\nCTX=32768\n"),
        ("ext", "EXTERNAL=1\nEXT_URL=https://x\nCTX=131072\n"),
        ("c", "MODEL=c.gguf\nCTX=131072\n"),
    ] {
        std::fs::write(dir.join(format!("{id}.env")), body).unwrap();
    }
    let (f, go, stop) = fakego::start(home.path().into()).await;
    let vu = Arc::new(Mutex::new(Vec::new()));
    *go.engine_port.lock().unwrap() = faux_moteur(vu.clone()).await;
    *go.apply_presets.lock().unwrap() = true;
    go.set("config", "MODEL", "a.gguf");
    go.set("config", "CTX", "65536");
    go.set("config", "BIN", "/machine/llama-server");
    if cle {
        go.set("state", "api_key", CLE);
    }
    Banc {
        f,
        go,
        vu,
        _stop: stop,
        _home: home,
    }
}

async fn send(b: &Banc, method: &str, path: &str, auth: bool, body: &str) -> Response<Incoming> {
    let mut req = Request::builder()
        .method(method)
        .uri(format!("http://{}{path}", b.f));
    if auth {
        req = req.header("authorization", format!("Bearer {CLE}"));
    }
    let client: Client<_, Full<Bytes>> = Client::builder(TokioExecutor::new()).build_http();
    client
        .request(req.body(Full::new(Bytes::from(body.to_string()))).unwrap())
        .await
        .unwrap()
}

async fn json_of(r: Response<Incoming>) -> serde_json::Value {
    let body = r.into_body().collect().await.unwrap().to_bytes();
    serde_json::from_slice(&body).unwrap_or_else(|_| panic!("{body:?}"))
}

fn completion(model: &str) -> String {
    serde_json::json!({ "model": model, "messages": [] }).to_string()
}

#[tokio::test]
async fn liste_filtree_derriere_la_cle() {
    let b = banc(true).await;
    let r = send(&b, "GET", "/v1/models", false, "").await;
    assert_eq!(r.status(), StatusCode::UNAUTHORIZED);

    let r = send(&b, "GET", "/v1/models", true, "").await;
    assert_eq!(r.status(), StatusCode::OK);
    let v = json_of(r).await;
    assert_eq!(v["object"], "list");
    let got: Vec<_> = v["data"]
        .as_array()
        .unwrap()
        .iter()
        .map(|m| {
            (
                m["id"].as_str().unwrap().to_string(),
                m["context_length"].as_i64().unwrap(),
                m["active"].as_bool().unwrap(),
                m["owned_by"].as_str().unwrap().to_string(),
            )
        })
        .collect();
    assert_eq!(
        got,
        vec![
            ("a".to_string(), 65536, true, "loki".to_string()),
            ("c".to_string(), 131072, false, "loki".to_string()),
        ]
    );
    // HEAD : même statut, corps vide.
    let r = send(&b, "HEAD", "/v1/models", true, "").await;
    assert_eq!(r.status(), StatusCode::OK);
    // Répondue sur place : ni ticket, ni moteur.
    assert!(b.go.oai_log.lock().unwrap().is_empty());
    assert!(b.vu.lock().unwrap().is_empty());
}

#[tokio::test]
async fn modele_actif_relaye_sans_bascule() {
    let b = banc(true).await;
    let r = send(&b, "POST", "/v1/chat/completions", true, &completion("a")).await;
    assert_eq!(r.status(), StatusCode::OK);
    assert!(b.go.switch_log.lock().unwrap().is_empty());
    assert_eq!(b.vu.lock().unwrap().len(), 1);
}

#[tokio::test]
async fn autre_preset_bascule_puis_relaie() {
    let b = banc(true).await;
    let body = completion("c");
    let r = send(&b, "POST", "/v1/chat/completions", true, &body).await;
    assert_eq!(r.status(), StatusCode::OK);
    assert_eq!(*b.go.switch_log.lock().unwrap(), vec!["switch c.env"]);
    // route relu après la bascule.
    assert_eq!(b.go.route_calls.load(Ordering::SeqCst), 2);
    assert_eq!(b.go.config_map()["MODEL"], "c.gguf");
    assert_eq!(b.go.config_map()["BIN"], "/machine/llama-server");
    // Le corps part tel quel au moteur.
    assert_eq!(
        *b.vu.lock().unwrap(),
        vec![format!("/v1/chat/completions {body}")]
    );
    // Le preset c est désormais l'actif de la liste.
    let v = json_of(send(&b, "GET", "/v1/models", true, "").await).await;
    let actif: Vec<_> = v["data"]
        .as_array()
        .unwrap()
        .iter()
        .filter(|m| m["active"] == true)
        .map(|m| m["id"].as_str().unwrap().to_string())
        .collect();
    assert_eq!(actif, vec!["c"]);
}

#[tokio::test]
async fn modele_inconnu_ou_trop_petit_va_au_modele_charge() {
    let b = banc(true).await;
    for m in ["gpt-4o", "b", "ext"] {
        let r = send(&b, "POST", "/v1/chat/completions", true, &completion(m)).await;
        assert_eq!(r.status(), StatusCode::OK, "{m}");
    }
    assert!(b.go.switch_log.lock().unwrap().is_empty());
    assert_eq!(b.go.route_calls.load(Ordering::SeqCst), 3);
    assert_eq!(b.vu.lock().unwrap().len(), 3);
}

#[tokio::test]
async fn sans_cle_jamais_de_bascule() {
    let b = banc(false).await;
    let r = send(&b, "POST", "/v1/chat/completions", false, &completion("c")).await;
    assert_eq!(r.status(), StatusCode::OK);
    assert!(b.go.switch_log.lock().unwrap().is_empty());
    assert_eq!(b.go.config_map()["MODEL"], "a.gguf");
}

#[tokio::test]
async fn bascule_refusee_503_avec_retry_after() {
    let b = banc(true).await;
    *b.go.switch_refuse.lock().unwrap() = Some(serde_json::json!({
        "status": 503, "message": "Loki est occupé — réessaie dans un moment",
        "type": "server_error", "code": "engine_busy", "retry_after": "5"
    }));
    let r = send(&b, "POST", "/v1/chat/completions", true, &completion("c")).await;
    assert_eq!(r.status(), StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(r.headers()["retry-after"], "5");
    assert_eq!(json_of(r).await["error"]["code"], "engine_busy");
    assert!(b.vu.lock().unwrap().is_empty(), "relayé malgré le refus");
    assert!(b.go.oai_log.lock().unwrap().is_empty(), "ticket pris");
}

#[tokio::test]
async fn deux_requetes_une_seule_bascule() {
    let b = Arc::new(banc(true).await);
    *b.go.switch_delay_ms.lock().unwrap() = 500;
    let (b1, b2) = (b.clone(), b.clone());
    let one = tokio::spawn(async move {
        send(&b1, "POST", "/v1/chat/completions", true, &completion("c"))
            .await
            .status()
    });
    tokio::time::sleep(std::time::Duration::from_millis(100)).await;
    let two = tokio::spawn(async move {
        send(&b2, "POST", "/v1/chat/completions", true, &completion("c"))
            .await
            .status()
    });
    assert_eq!(one.await.unwrap(), StatusCode::OK);
    assert_eq!(two.await.unwrap(), StatusCode::OK);
    assert_eq!(b.go.switch_log.lock().unwrap().len(), 1);
    assert_eq!(b.vu.lock().unwrap().len(), 2);
}

#[tokio::test]
async fn bascule_lente_la_suivante_recoit_503() {
    let b = Arc::new(banc(true).await);
    *b.go.switch_delay_ms.lock().unwrap() = 3000;
    let b1 = b.clone();
    let one = tokio::spawn(async move {
        send(&b1, "POST", "/v1/chat/completions", true, &completion("c"))
            .await
            .status()
    });
    tokio::time::sleep(std::time::Duration::from_millis(200)).await;
    let r = send(&b, "POST", "/v1/chat/completions", true, &completion("a")).await;
    assert_eq!(r.status(), StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(r.headers()["retry-after"], "30");
    assert_eq!(json_of(r).await["error"]["code"], "model_switching");
    assert_eq!(one.await.unwrap(), StatusCode::OK);
}

#[tokio::test]
async fn corps_trop_gros_413_hors_second_slot() {
    let b = banc(true).await;
    // max_body = 1024 dans le faux Go.
    let gros = serde_json::json!({ "model": "a", "x": "a".repeat(2000) }).to_string();
    let r = send(&b, "POST", "/v1/chat/completions", true, &gros).await;
    assert_eq!(r.status(), StatusCode::PAYLOAD_TOO_LARGE);
    assert_eq!(json_of(r).await["error"]["code"], "body_too_large");
    assert!(b.vu.lock().unwrap().is_empty());
}
