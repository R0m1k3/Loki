use bytes::Bytes;
use futures_util::{SinkExt, StreamExt};
use http_body_util::{BodyExt, Empty, Full, StreamBody, combinators::BoxBody};
use hyper::body::{Frame, Incoming};
use hyper::{Request, Response, StatusCode, service::service_fn};
use hyper_util::client::legacy::Client;
use hyper_util::rt::{TokioExecutor, TokioIo};
use std::convert::Infallible;
use std::net::SocketAddr;
use std::time::{Duration, Instant};
use tokio::net::TcpListener;
use tokio::sync::oneshot;
use tokio_tungstenite::tungstenite::Message;

type B = BoxBody<Bytes, Infallible>;

/// Backend factice : /echo renvoie les en-tÃªtes reÃ§us, /sse Ã©met un Ã©vÃ©nement,
/// attend 500 ms, puis un second ; /post renvoie le corps reÃ§u.
async fn backend() -> SocketAddr {
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    tokio::spawn(async move {
        loop {
            let (s, _) = l.accept().await.unwrap();
            tokio::spawn(async move {
                let svc = service_fn(|req: Request<Incoming>| async move {
                    let resp: Response<B> = match req.uri().path() {
                        "/echo" => {
                            let mut lines: Vec<String> = req
                                .headers()
                                .iter()
                                .map(|(k, v)| format!("{}: {}", k, v.to_str().unwrap()))
                                .collect();
                            lines.push(format!("uri: {}", req.uri()));
                            Response::new(Full::new(Bytes::from(lines.join("\n"))).boxed())
                        }
                        "/post" => {
                            let body = req.into_body().collect().await.unwrap().to_bytes();
                            Response::new(Full::new(body).boxed())
                        }
                        "/sse" => {
                            let (tx, rx) = tokio::sync::mpsc::channel::<Bytes>(4);
                            tokio::spawn(async move {
                                tx.send(Bytes::from_static(b"data: un\n\n")).await.unwrap();
                                tokio::time::sleep(Duration::from_millis(500)).await;
                                let _ = tx.send(Bytes::from_static(b"data: deux\n\n")).await;
                            });
                            let stream = futures_util::stream::unfold(rx, |mut rx| async move {
                                rx.recv()
                                    .await
                                    .map(|b| (Ok::<_, Infallible>(Frame::data(b)), rx))
                            });
                            let mut r = Response::new(BodyExt::boxed(StreamBody::new(stream)));
                            r.headers_mut()
                                .insert("content-type", "text/event-stream".parse().unwrap());
                            r
                        }
                        _ => Response::new(Empty::new().boxed()),
                    };
                    Ok::<_, Infallible>(resp)
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(s), svc)
                    .await;
            });
        }
    });
    addr
}

async fn ws_backend() -> SocketAddr {
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    tokio::spawn(async move {
        while let Ok((s, _)) = l.accept().await {
            tokio::spawn(async move {
                let mut ws = tokio_tungstenite::accept_async(s).await.unwrap();
                while let Some(Ok(m)) = ws.next().await {
                    if m.is_text() {
                        ws.send(m).await.unwrap();
                    }
                }
            });
        }
    });
    addr
}

/// Lance la faÃ§ade devant `backend`, rend son adresse.
async fn facade(backend: SocketAddr) -> (SocketAddr, oneshot::Sender<()>) {
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    let (tx, rx) = oneshot::channel::<()>();
    let cfg = loki_rs::proxy::Config {
        backend,
        internal_token: "jeton-de-test".into(),
        host_env: Default::default(),
        engine: loki_rs::engine::Paths {
            home: std::env::temp_dir(),
            service: "loki-engine".into(),
        },
    };
    tokio::spawn(loki_rs::proxy::serve(l, cfg, async move {
        let _ = rx.await;
    }));
    (addr, tx)
}

fn client() -> Client<hyper_util::client::legacy::connect::HttpConnector, Full<Bytes>> {
    Client::builder(TokioExecutor::new()).build_http()
}

#[tokio::test]
async fn transmet_les_en_tetes_et_efface_x_loki_via() {
    let (f, _stop) = facade(backend().await).await;
    let req = Request::get(format!("http://{f}/echo?a=1"))
        .header("host", "loki.exemple.fr")
        .header("authorization", "Bearer secret")
        .header("origin", "https://loki.exemple.fr")
        .header("x-forwarded-proto", "https")
        .header("sec-fetch-site", "same-origin")
        .header("x-loki-via", "tunnel")
        .body(Full::new(Bytes::new()))
        .unwrap();
    let resp = client().request(req).await.unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let body = String::from_utf8(
        resp.into_body()
            .collect()
            .await
            .unwrap()
            .to_bytes()
            .to_vec(),
    )
    .unwrap();
    for want in [
        "host: loki.exemple.fr",
        "authorization: Bearer secret",
        "origin: https://loki.exemple.fr",
        "x-forwarded-proto: https",
        "sec-fetch-site: same-origin",
        "uri: /echo?a=1",
    ] {
        assert!(
            body.lines().any(|l| l == want),
            "manque Â« {want} Â» dans :\n{body}"
        );
    }
    assert!(
        !body.contains("x-loki-via"),
        "X-Loki-Via a traversÃ© :\n{body}"
    );
}

#[tokio::test]
async fn relaie_le_corps() {
    let (f, _stop) = facade(backend().await).await;
    let req = Request::post(format!("http://{f}/post"))
        .body(Full::new(Bytes::from_static(b"{\"x\":1}")))
        .unwrap();
    let resp = client().request(req).await.unwrap();
    let body = resp.into_body().collect().await.unwrap().to_bytes();
    assert_eq!(&body[..], b"{\"x\":1}");
}

#[tokio::test]
async fn sse_sans_tampon() {
    let (f, _stop) = facade(backend().await).await;
    let t0 = Instant::now();
    let req = Request::get(format!("http://{f}/sse"))
        .body(Full::new(Bytes::new()))
        .unwrap();
    let mut body = client().request(req).await.unwrap().into_body();
    let first = body.frame().await.unwrap().unwrap().into_data().unwrap();
    let elapsed = t0.elapsed();
    assert_eq!(&first[..], b"data: un\n\n");
    assert!(
        elapsed < Duration::from_millis(300),
        "premier Ã©vÃ©nement reÃ§u aprÃ¨s {elapsed:?}"
    );
    let second = body.frame().await.unwrap().unwrap().into_data().unwrap();
    assert_eq!(&second[..], b"data: deux\n\n");
}

#[tokio::test]
async fn websocket_relaye() {
    let (f, _stop) = facade(ws_backend().await).await;
    let (mut ws, resp) = tokio_tungstenite::connect_async(format!("ws://{f}/api/node/ws"))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::SWITCHING_PROTOCOLS);
    ws.send(Message::text("bonjour")).await.unwrap();
    let got = tokio::time::timeout(Duration::from_secs(5), ws.next())
        .await
        .unwrap()
        .unwrap()
        .unwrap();
    assert_eq!(got.into_text().unwrap().as_str(), "bonjour");
}

#[tokio::test]
async fn backend_absent_donne_502() {
    // Port rÃ©servÃ© puis libÃ©rÃ© : personne n'y Ã©coute.
    let dead = TcpListener::bind("127.0.0.1:0")
        .await
        .unwrap()
        .local_addr()
        .unwrap();
    let (f, _stop) = facade(dead).await;
    // Route relayée au Go (« / » est servie par la façade elle-même).
    let req = Request::get(format!("http://{f}/api/status"))
        .body(Full::new(Bytes::new()))
        .unwrap();
    let resp = client().request(req).await.unwrap();
    assert_eq!(resp.status(), StatusCode::BAD_GATEWAY);
}
