//! Faux serveur Go : API interne (`/internal/v1/auth`, `/internal/v1/store`,
//! `/internal/v1/store/all`) sur un store en mémoire, et `go:<chemin>` pour
//! toute autre route (ce qui prouve qu'elle a été relayée).

use bytes::Bytes;
use http_body_util::{BodyExt, Full};
use hyper::body::Incoming;
use hyper::{Method, Request, Response, StatusCode, service::service_fn};
use hyper_util::rt::TokioIo;
use std::collections::{BTreeMap, HashMap};
use std::convert::Infallible;
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};
use tokio::net::TcpListener;
use tokio::sync::oneshot;

pub const JETON: &str = "0123456789abcdef0123456789abcdef";

#[derive(Default)]
pub struct FakeGo {
    /// bucket → clé → valeur.
    pub store: Mutex<HashMap<String, BTreeMap<String, Vec<u8>>>>,
    /// Base illisible : toute l'API interne répond 503.
    pub broken: Mutex<bool>,
    /// Chemins relayés (hors API interne).
    pub relayed: Mutex<Vec<String>>,
    /// /v1 : port du faux moteur, second slot en service, refus à renvoyer
    /// par begin, journal des begin/end.
    pub engine_port: Mutex<u16>,
    pub side_live: Mutex<bool>,
    pub refuse: Mutex<Option<serde_json::Value>>,
    pub oai_log: Mutex<Vec<String>>,
}

impl FakeGo {
    pub fn set(&self, bucket: &str, key: &str, val: &str) {
        self.store
            .lock()
            .unwrap()
            .entry(bucket.into())
            .or_default()
            .insert(key.into(), val.as_bytes().to_vec());
    }
    pub fn get(&self, bucket: &str, key: &str) -> Option<String> {
        let s = self.store.lock().unwrap();
        s.get(bucket)?
            .get(key)
            .map(|v| String::from_utf8_lossy(v).into_owned())
    }
}

fn query(req: &Request<Incoming>) -> HashMap<String, String> {
    req.uri()
        .query()
        .unwrap_or("")
        .split('&')
        .filter_map(|kv| kv.split_once('='))
        .map(|(k, v)| (k.to_string(), decode(v)))
        .collect()
}

fn decode(s: &str) -> String {
    let b = s.as_bytes();
    let mut out = Vec::new();
    let mut i = 0;
    while i < b.len() {
        if b[i] == b'%' && i + 2 < b.len() {
            let h = std::str::from_utf8(&b[i + 1..i + 3]).unwrap();
            out.push(u8::from_str_radix(h, 16).unwrap());
            i += 3;
        } else {
            out.push(b[i]);
            i += 1;
        }
    }
    String::from_utf8(out).unwrap()
}

fn resp(code: u16, body: impl Into<Bytes>) -> Response<Full<Bytes>> {
    let mut r = Response::new(Full::new(body.into()));
    *r.status_mut() = StatusCode::from_u16(code).unwrap();
    r
}

async fn handle(st: Arc<FakeGo>, req: Request<Incoming>) -> Response<Full<Bytes>> {
    let path = req.uri().path().to_string();
    if !path.starts_with("/internal/") {
        st.relayed.lock().unwrap().push(path.clone());
        return resp(200, format!("go:{path}"));
    }
    if req.headers().get("x-loki-internal").map(|v| v.as_bytes()) != Some(JETON.as_bytes()) {
        return resp(404, "");
    }
    if *st.broken.lock().unwrap() {
        return resp(503, r#"{"error":"base illisible"}"#);
    }
    let q = query(&req);
    let method = req.method().clone();
    match path.as_str() {
        "/internal/v1/auth" => {
            let key = st.get("state", "web_key").unwrap_or_default();
            resp(200, format!(r#"{{"web_key":"{key}","version":"0.15.0"}}"#))
        }
        "/internal/v1/oai/route" => {
            let key = st.get("state", "api_key").unwrap_or_default();
            let v = serde_json::json!({
                "port": *st.engine_port.lock().unwrap(),
                "strata": false,
                "key": key,
                "key_ok": true,
                "max_body": 1024,
                "side_live": *st.side_live.lock().unwrap(),
            });
            resp(200, v.to_string())
        }
        "/internal/v1/oai/begin" => {
            let body = req.into_body().collect().await.unwrap().to_bytes();
            let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
            st.oai_log.lock().unwrap().push(format!(
                "begin read={} side={} post_v1={}",
                v["read"], v["side"], v["post_v1"]
            ));
            if let Some(r) = st.refuse.lock().unwrap().clone() {
                return resp(200, serde_json::json!({ "refuse": r }).to_string());
            }
            resp(200, r#"{"ticket":7}"#)
        }
        "/internal/v1/oai/end" => {
            let body = req.into_body().collect().await.unwrap().to_bytes();
            let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
            st.oai_log
                .lock()
                .unwrap()
                .push(format!("end {}", v["ticket"]));
            resp(204, "")
        }
        "/internal/v1/store/all" => {
            let s = st.store.lock().unwrap();
            let m: BTreeMap<String, String> = s
                .get(&q["bucket"])
                .map(|b| {
                    b.iter()
                        .map(|(k, v)| (k.clone(), String::from_utf8_lossy(v).into_owned()))
                        .collect()
                })
                .unwrap_or_default();
            resp(200, serde_json::to_string(&m).unwrap())
        }
        "/internal/v1/store" => {
            let (bucket, key) = (q["bucket"].clone(), q["key"].clone());
            match method {
                Method::GET => match st
                    .store
                    .lock()
                    .unwrap()
                    .get(&bucket)
                    .and_then(|b| b.get(&key))
                {
                    Some(v) => resp(200, v.clone()),
                    None => resp(404, ""),
                },
                Method::PUT => {
                    let body = req.into_body().collect().await.unwrap().to_bytes();
                    st.store
                        .lock()
                        .unwrap()
                        .entry(bucket)
                        .or_default()
                        .insert(key, body.to_vec());
                    resp(204, "")
                }
                Method::DELETE => {
                    if let Some(b) = st.store.lock().unwrap().get_mut(&bucket) {
                        b.remove(&key);
                    }
                    resp(204, "")
                }
                _ => resp(405, ""),
            }
        }
        _ => resp(404, ""),
    }
}

/// Lance le faux Go puis la façade devant lui ; rend (adresse de la façade,
/// état du faux Go, garde d'arrêt).
pub async fn start(home: std::path::PathBuf) -> (SocketAddr, Arc<FakeGo>, oneshot::Sender<()>) {
    let st = Arc::new(FakeGo::default());
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let backend = l.local_addr().unwrap();
    let st2 = st.clone();
    tokio::spawn(async move {
        while let Ok((s, _)) = l.accept().await {
            let st = st2.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req| {
                    let st = st.clone();
                    async move { Ok::<_, Infallible>(handle(st, req).await) }
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(s), svc)
                    .await;
            });
        }
    });
    let fl = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let facade = fl.local_addr().unwrap();
    let (tx, rx) = oneshot::channel::<()>();
    let cfg = loki_rs::proxy::Config {
        backend,
        internal_token: JETON.into(),
        host_env: loki_rs::auth::HostEnv::default(),
        engine: loki_rs::engine::Paths {
            home,
            service: "loki-engine".into(),
        },
    };
    tokio::spawn(loki_rs::proxy::serve(fl, cfg, async move {
        let _ = rx.await;
    }));
    (facade, st, tx)
}

/// GET via la façade : (code, en-têtes, corps).
pub async fn get(
    facade: SocketAddr,
    path: &str,
    headers: &[(&str, &str)],
) -> (StatusCode, hyper::HeaderMap, Bytes) {
    use hyper_util::client::legacy::Client;
    use hyper_util::rt::TokioExecutor;
    let mut req = Request::get(format!("http://{facade}{path}"));
    for (k, v) in headers {
        req = req.header(*k, *v);
    }
    let client: Client<_, Full<Bytes>> = Client::builder(TokioExecutor::new()).build_http();
    let r = client
        .request(req.body(Full::new(Bytes::new())).unwrap())
        .await
        .unwrap();
    let (parts, body) = r.into_parts();
    (
        parts.status,
        parts.headers,
        body.collect().await.unwrap().to_bytes(),
    )
}
