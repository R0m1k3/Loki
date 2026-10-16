//! Lot 42 : outils web de l'agent (`web_search`, `web_images`, `web_open`,
//! `web_read`, `web_grep`) exécutés par la façade.
//!
//! Le journal tiré du Go (`internal/loki/parity_web_tools_test.go` →
//! `tests/parity_web/outils.json`) est rejoué contre le port : mêmes
//! réponses du faux Crawl4AI et du faux DuckDuckGo, ce que le moteur intégré
//! du Go a tiré des pages servi par le faux Go ; chaque appel doit rendre le
//! même résultat et faire les mêmes requêtes (corps envoyés à Crawl4AI,
//! en-têtes et cookies envoyés à DuckDuckGo, pages et recherches demandées
//! au Go).

mod common;

use std::collections::HashMap;
use std::convert::Infallible;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use bytes::Bytes;
use common::fakego;
use http_body_util::{BodyExt, Full};
use hyper::service::service_fn;
use hyper::{Request, Response};
use hyper_util::rt::TokioIo;
use loki_rs::store::Store;
use loki_rs::webtools::Web;
use serde_json::{Map, Value, json};
use tokio::net::TcpListener;

fn journal() -> Value {
    serde_json::from_str(include_str!("parity_web/outils.json")).expect("journal")
}

fn s(v: &Value) -> &str {
    v.as_str().unwrap_or("")
}

type Log = Arc<Mutex<Vec<Value>>>;

/// Un petit serveur HTTP : `f(méthode, chemin?requête, en-têtes, corps)`
/// rend (statut, en-têtes, corps).
async fn serve<F>(f: F) -> String
where
    F: Fn(String, String, hyper::HeaderMap, Vec<u8>) -> (u16, Vec<(String, String)>, String)
        + Send
        + Sync
        + 'static,
{
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    let f = Arc::new(f);
    tokio::spawn(async move {
        while let Ok((st, _)) = l.accept().await {
            let f = f.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<hyper::body::Incoming>| {
                    let f = f.clone();
                    async move {
                        let method = req.method().to_string();
                        let uri = req
                            .uri()
                            .path_and_query()
                            .map(|p| p.to_string())
                            .unwrap_or_default();
                        let headers = req.headers().clone();
                        let body = req.into_body().collect().await.unwrap().to_bytes().to_vec();
                        let (code, hs, out) = f(method, uri, headers, body);
                        let mut r = Response::new(Full::new(Bytes::from(out)));
                        *r.status_mut() = hyper::StatusCode::from_u16(code).unwrap();
                        for (k, v) in hs {
                            r.headers_mut().insert(
                                hyper::header::HeaderName::from_bytes(k.as_bytes()).unwrap(),
                                v.parse().unwrap(),
                            );
                        }
                        Ok::<_, Infallible>(r)
                    }
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(st), svc)
                    .await;
            });
        }
    });
    format!("http://{addr}")
}

fn header(h: &hyper::HeaderMap, k: &str) -> String {
    h.get(k)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_string()
}

/// Une requête reçue, champs vides omis (comme `omitempty`).
fn req(fields: &[(&str, String)]) -> Value {
    let mut m = Map::new();
    for (k, v) in fields {
        if !v.is_empty() {
            m.insert((*k).into(), json!(v));
        }
    }
    Value::Object(m)
}

/// Le faux Crawl4AI : la réponse rangée pour l'URL demandée (dans l'ordre,
/// la dernière reste).
async fn fake_crawl(fx: Value, log: Log) -> String {
    let served: Arc<Mutex<HashMap<String, usize>>> = Default::default();
    serve(move |method, uri, h, body| {
        let path = uri.split('?').next().unwrap_or("").to_string();
        log.lock().unwrap().push(req(&[
            ("path", format!("{method} {path}")),
            ("auth", header(&h, "authorization")),
            ("ctype", header(&h, "content-type")),
            ("body", String::from_utf8_lossy(&body).into_owned()),
        ]));
        let v: Value = serde_json::from_slice(&body).unwrap_or_default();
        let target = s(&v["urls"][0]).to_string();
        let Some(list) = fx[&target].as_array().filter(|l| !l.is_empty()) else {
            return (404, vec![], format!("inconnu : {target}"));
        };
        let mut served = served.lock().unwrap();
        let n = served.entry(target).or_default();
        let r = &list[(*n).min(list.len() - 1)];
        *n += 1;
        (
            r["status"].as_u64().unwrap() as u16,
            vec![],
            s(&r["body"]).to_string(),
        )
    })
    .await
}

/// Le faux DuckDuckGo des images.
async fn fake_ddg(fx: Value, log: Log) -> String {
    serve(move |_, uri, h, _| {
        log.lock().unwrap().push(req(&[
            ("path", uri.clone()),
            ("referer", header(&h, "referer")),
            ("accept", header(&h, "accept")),
            ("lang", header(&h, "accept-language")),
            ("ua", header(&h, "user-agent")),
            ("cookie", header(&h, "cookie")),
        ]));
        let (path, query) = uri.split_once('?').unwrap_or((&uri, ""));
        let q = url::form_urlencoded::parse(query.as_bytes())
            .find(|(k, _)| k == "q")
            .map(|(_, v)| v.into_owned())
            .unwrap_or_default();
        let f = &fx[&q];
        if path == "/i.js" {
            return (
                f["api_status"].as_u64().unwrap() as u16,
                vec![],
                s(&f["api"]).to_string(),
            );
        }
        let mut hs = vec![];
        if !s(&f["cookie"]).is_empty() {
            hs.push(("set-cookie".to_string(), s(&f["cookie"]).to_string()));
        }
        (
            f["page_status"].as_u64().unwrap() as u16,
            hs,
            s(&f["page"]).to_string(),
        )
    })
    .await
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn outils_web_comme_le_go() {
    let fx = journal();
    let tmp = tempfile::tempdir().unwrap();
    let (_, go, _stop) = fakego::start(tmp.path().to_path_buf()).await;
    let backend = go.backend.lock().unwrap().unwrap();
    let store = Store::new(backend, fakego::JETON.into());

    let (crawl_log, img_log): (Log, Log) = Default::default();
    let crawl = fake_crawl(fx["crawl"].clone(), crawl_log.clone()).await;
    let ddg = fake_ddg(fx["images"].clone(), img_log.clone()).await;
    let dead = {
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        l.local_addr().unwrap().to_string()
    };
    let pages = "http://pages.invalid".to_string();
    let unmask = |v: &str| {
        v.replace("{{CRAWL}}", &crawl)
            .replace("{{DDG}}", &ddg)
            .replace("{{PAGES}}", &pages)
            .replace("{{DEADADDR}}", &dead)
            .replace("{{DEAD}}", &format!("http://{dead}"))
    };
    let mask = |v: &str| {
        v.replace(&crawl, "{{CRAWL}}")
            .replace(&ddg, "{{DDG}}")
            .replace(&pages, "{{PAGES}}")
            .replace(&format!("http://{dead}"), "{{DEAD}}")
            .replace(&dead, "{{DEADADDR}}")
    };
    for (k, v) in fx["go_fetch"].as_object().unwrap() {
        let v: Value = serde_json::from_str(&unmask(&v.to_string())).unwrap();
        go.web_fetch.lock().unwrap().insert(unmask(k), v);
    }
    for (k, v) in fx["go_search"].as_object().unwrap() {
        go.web_search.lock().unwrap().insert(k.clone(), v.clone());
    }

    let web = Web::with_ddg_base(&ddg);
    for c in fx["calls"].as_array().unwrap() {
        let name = s(&c["name"]);
        for (k, v) in c["setup"].as_object().into_iter().flatten() {
            let (bucket, v) = if k == "crawl_key" {
                ("state", s(v).to_string())
            } else {
                ("config", unmask(s(v)))
            };
            if v == "-" {
                go.del(bucket, k);
            } else {
                go.set(bucket, k, &v);
            }
        }
        if let Some(n) = c["advance_s"].as_u64() {
            web.advance(Duration::from_secs(n));
        }
        let args: Map<String, Value> =
            serde_json::from_str(&unmask(&c["args"].to_string())).unwrap();
        let got = web
            .exec(&store, s(&c["tool"]), &args)
            .await
            .expect("un outil web");
        assert_eq!(mask(&got), s(&c["result"]), "{name}");

        let masked = |log: &Log| -> Value {
            let v = Value::Array(std::mem::take(&mut *log.lock().unwrap()));
            serde_json::from_str(&mask(&v.to_string())).unwrap()
        };
        assert_eq!(masked(&crawl_log), c["crawls"], "{name} : Crawl4AI");
        // reqwest pose « Accept: */* » quand la requête n'en a pas ; le
        // client du Go n'en pose aucun (sans effet sur DuckDuckGo).
        let mut imgs = masked(&img_log);
        for r in imgs.as_array_mut().into_iter().flatten() {
            if r["accept"] == "*/*" {
                r.as_object_mut().unwrap().remove("accept");
            }
        }
        assert_eq!(imgs, c["images"], "{name} : DuckDuckGo");
        let calls = std::mem::take(&mut *go.web_log.lock().unwrap());
        let fetched: Vec<String> = calls
            .iter()
            .filter_map(|l| l.strip_prefix("fetch "))
            .map(&mask)
            .collect();
        let searched: Vec<String> = calls
            .iter()
            .filter_map(|l| l.strip_prefix("search "))
            .map(|q| q.rsplit_once('|').map_or(q, |(q, _)| q).to_string())
            .collect();
        assert_eq!(json!(fetched), c["go_fetch"], "{name} : pages du Go");
        assert_eq!(json!(searched), c["go_search"], "{name} : recherches du Go");
    }
}

/// Le Go injoignable : le moteur intégré (par défaut sans configuration)
/// le dit ; un autre outil n'est pas servi ici.
#[tokio::test]
async fn moteur_integre_injoignable() {
    let dead = std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap();
    let store = Store::new(dead, fakego::JETON.into());
    let web = Web::new();
    let mut args = Map::new();
    args.insert("url".into(), json!("https://exemple.test/"));
    let got = web.exec(&store, "web_open", &args).await.unwrap();
    assert!(
        got.starts_with("❌ moteur web intégré injoignable : "),
        "{got}"
    );
    args.insert("query".into(), json!("x"));
    let got = web.exec(&store, "web_search", &args).await.unwrap();
    assert!(
        got.starts_with("❌ Recherche échouée : moteur web intégré injoignable : "),
        "{got}"
    );
    assert_eq!(web.exec(&store, "web_screenshot", &args).await, None);
}
