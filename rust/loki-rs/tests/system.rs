//! Lot 17 : interrupteurs système tenus par la façade (`LOKI_SYSTEM=rust`).
//!
//! Le journal tiré du Go (`internal/loki/parity_system_test.go` →
//! `tests/parity_system/system.json`) est rejoué par une seule façade : accès
//! internet et Crawl4AI (en-têtes reçus par les faux serveurs et cache de
//! joignabilité compris), navigateur et vision, clé de complétion, écoute
//! réseau, avec après chaque appel les clés touchées en base ; puis le
//! Chromium de Playwright. Ce qui reste au Go (cache de ses outils web,
//! Chromium piloté, redémarrage du moteur) est vérifié de bout en bout contre
//! le faux Go.

mod common;

use std::collections::BTreeMap;
use std::convert::Infallible;
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};

use bytes::Bytes;
use common::fakego;
use http_body_util::{BodyExt, Full};
use hyper::service::service_fn;
use hyper::{Request, Response};
use hyper_util::rt::TokioIo;
use loki_rs::system::{self as sy, System};
use serde_json::{Value, json};
use tokio::net::TcpListener;

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_system/system.json")).expect("journal")
}

fn s(v: &Value) -> &str {
    v.as_str().unwrap()
}

const KEYS: &[&str] = &[
    "config/WEB_ENGINE",
    "config/CRAWL4AI_URL",
    "config/CRAWL4AI_KEY",
    "config/API_KEY",
    "config/HOST",
    "state/internet",
    "state/computer",
    "state/crawl_key",
    "state/api_key",
    "state/public_url",
];

/// Un faux Crawl4AI : répond `code` partout, note « chemin Authorization ».
async fn fake_crawl(code: u16, log: Arc<Mutex<Vec<String>>>) -> String {
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    tokio::spawn(async move {
        while let Ok((st, _)) = l.accept().await {
            let log = log.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<hyper::body::Incoming>| {
                    let log = log.clone();
                    async move {
                        let auth = req
                            .headers()
                            .get("authorization")
                            .map(|v| String::from_utf8_lossy(v.as_bytes()).into_owned())
                            .unwrap_or_default();
                        log.lock()
                            .unwrap()
                            .push(format!("{} {auth}", req.uri().path()).trim().to_string());
                        let mut r = Response::new(Full::new(Bytes::new()));
                        *r.status_mut() = hyper::StatusCode::from_u16(code).unwrap();
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

async fn call(
    facade: SocketAddr,
    method: &str,
    path: &str,
    body: &str,
    headers: &[(String, String)],
) -> (u16, String) {
    use hyper_util::client::legacy::Client;
    use hyper_util::rt::TokioExecutor;
    let mut req = Request::builder()
        .method(method)
        .uri(format!("http://{facade}{path}"))
        .header("host", "loki.local:8090");
    for (k, v) in headers {
        req = req.header(k.as_str(), v.as_str());
    }
    let client: Client<_, Full<Bytes>> = Client::builder(TokioExecutor::new()).build_http();
    let r = client
        .request(req.body(Full::new(Bytes::from(body.to_string()))).unwrap())
        .await
        .unwrap();
    let code = r.status().as_u16();
    let b = r.into_body().collect().await.unwrap().to_bytes();
    (code, String::from_utf8_lossy(&b).into_owned())
}

async fn banc(
    opts: fakego::Opts,
) -> (
    SocketAddr,
    Arc<fakego::FakeGo>,
    tokio::sync::oneshot::Sender<()>,
    tempfile::TempDir,
) {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, go, stop, _) = fakego::start_with(tmp.path().to_path_buf(), opts).await;
    (facade, go, stop, tmp)
}

fn system() -> Arc<System> {
    let mut s = System::new();
    s.chrome = Some("/opt/faux/chrome".into());
    Arc::new(s)
}

#[tokio::test]
async fn parite_des_routes() {
    let log = Arc::new(Mutex::new(Vec::new()));
    let ok = fake_crawl(200, log.clone()).await;
    let down = fake_crawl(500, log.clone()).await;
    let nf = fake_crawl(404, log.clone()).await;
    let (facade, go, _stop, _tmp) = banc(fakego::Opts {
        system: Some(system()),
        ..Default::default()
    })
    .await;
    let ip = sy::local_ip();
    let key_re = regex::Regex::new(r"sk-loki-[0-9a-f]{48}").unwrap();
    let masked_re = regex::Regex::new(r"sk-loki-[0-9a-f]…[0-9a-f]{4}").unwrap();
    let unmask = |v: &str| {
        v.replace("{{OK}}", &ok)
            .replace("{{DOWN}}", &down)
            .replace("{{NOTFOUND}}", &nf)
    };
    let mask = |v: &str| {
        let v = v
            .replace(&ok, "{{OK}}")
            .replace(&down, "{{DOWN}}")
            .replace(&nf, "{{NOTFOUND}}")
            .replace(&format!("http://{ip}:"), "http://{{IP}}:");
        let v = key_re.replace_all(&v, "{{KEY}}").into_owned();
        masked_re.replace_all(&v, "{{MASKED}}").into_owned()
    };
    for st in fixture()["steps"].as_array().unwrap() {
        let name = s(&st["name"]);
        for (bucket, field) in [("config", "config"), ("state", "state")] {
            for (k, v) in st[field].as_object().into_iter().flatten() {
                let v = unmask(s(v));
                if v.is_empty() {
                    go.del(bucket, k);
                } else {
                    go.set(bucket, k, &v);
                }
            }
        }
        *go.tune_busy.lock().unwrap() = (st["tune"].as_bool() == Some(true)).then(|| {
            "optimisation en cours (loki tune) : le moteur est réservé aux essais — attends la fin ou annule-la".into()
        });
        log.lock().unwrap().clear();
        let headers: Vec<(String, String)> = st["headers"]
            .as_object()
            .into_iter()
            .flatten()
            .map(|(k, v)| (k.clone(), s(v).to_string()))
            .collect();
        let (code, body) = call(
            facade,
            s(&st["method"]),
            s(&st["path"]),
            &unmask(st["body"].as_str().unwrap_or("")),
            &headers,
        )
        .await;
        assert_eq!(code as i64, st["status"].as_i64().unwrap(), "{name}");
        let out: Value = serde_json::from_str(&mask(&body)).unwrap();
        assert_eq!(out, st["out"], "{name}");
        let mut after = BTreeMap::new();
        for k in KEYS {
            let (b, key) = k.split_once('/').unwrap();
            if let Some(v) = go.get(b, key).filter(|v| !v.is_empty()) {
                after.insert(k.to_string(), Value::String(mask(&v)));
            }
        }
        assert_eq!(
            Value::Object(after.into_iter().collect()),
            st["after"],
            "{name} : base"
        );
        let probes: Vec<Value> = log
            .lock()
            .unwrap()
            .iter()
            .map(|p| Value::String(mask(p)))
            .collect();
        assert_eq!(Value::Array(probes), st["probes"], "{name} : sondes");
    }
}

#[test]
fn parite_du_chromium_de_playwright() {
    for c in fixture()["chromium"].as_array().unwrap() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().to_string_lossy().into_owned();
        for f in c["files"].as_array().unwrap() {
            let f = s(f);
            let p = tmp.path().join(f.trim_end_matches('/'));
            if f.ends_with('/') {
                std::fs::create_dir_all(&p).unwrap();
            } else {
                std::fs::create_dir_all(p.parent().unwrap()).unwrap();
                std::fs::write(&p, "").unwrap();
            }
        }
        let got = sy::playwright_chromium(Some(&root), None);
        assert_eq!(
            got.strip_prefix(&format!("{root}/")).unwrap_or(&got),
            s(&c["out"]),
            "{c}"
        );
    }
}

#[tokio::test]
async fn ce_qui_reste_au_go() {
    let (facade, go, _stop, _tmp) = banc(fakego::Opts {
        system: Some(system()),
        ..Default::default()
    })
    .await;
    // Le cache des outils web du Go est vidé à chaque changement d'URL, de clé
    // ou de moteur — pas pour l'interrupteur seul, ni en lecture.
    for (body, resets) in [
        (r#"{"enabled":true}"#, 0),
        (r#"{"url":"http://127.0.0.1:1"}"#, 1),
        (r#"{"key":"k1"}"#, 2),
        (r#"{"engine":"go"}"#, 3),
        (r#"{"engine":"go","key":"","url":""}"#, 6),
        (r#"{"engine":"autre","url":"x"}"#, 6),
    ] {
        call(facade, "POST", "/api/internet", body, &[]).await;
        assert_eq!(*go.reach_resets.lock().unwrap(), resets, "{body}");
    }
    call(facade, "GET", "/api/internet", "", &[]).await;
    assert_eq!(*go.reach_resets.lock().unwrap(), 6);

    // Le navigateur piloté (au Go) se ferme quand l'interrupteur se coupe.
    call(facade, "POST", "/api/computer", r#"{"on":true}"#, &[]).await;
    assert_eq!(*go.cdp_shutdowns.lock().unwrap(), 0);
    call(facade, "POST", "/api/computer", r#"{"on":false}"#, &[]).await;
    call(facade, "GET", "/api/computer", "", &[]).await;
    assert_eq!(*go.cdp_shutdowns.lock().unwrap(), 1);

    // Une clé neuve : le moteur est relancé s'il tourne, par le Go quand la
    // façade ne le pilote pas ; jamais sur une lecture ni sur un refus.
    call(facade, "GET", "/api/apikey", "", &[]).await;
    call(facade, "POST", "/api/apikey", r#"{"action":"bof"}"#, &[]).await;
    assert!(go.service_actions.lock().unwrap().is_empty());
    call(
        facade,
        "POST",
        "/api/apikey",
        r#"{"action":"generate"}"#,
        &[],
    )
    .await;
    assert_eq!(
        *go.service_actions.lock().unwrap(),
        [("restart".to_string(), true)]
    );
}

/// Faux superviseur du moteur (voir tests/enginectl.rs).
fn supervisor(dir: &std::path::Path) -> std::path::PathBuf {
    let p = dir.join("loki-rs");
    let body = "#!/bin/sh\necho \"$2\" >> \"$LOKI_HOME/trace\"\nexit 0\n";
    let ok = std::process::Command::new("sh")
        .args(["-c", "printf %s \"$1\" > \"$2\" && chmod 755 \"$2\""])
        .arg("sh")
        .arg(body)
        .arg(&p)
        .status()
        .unwrap()
        .success();
    assert!(ok);
    p
}

#[tokio::test]
async fn cle_neuve_moteur_pilote_par_la_facade() {
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    let ctl = loki_rs::enginectl::EngineCtl::new(
        supervisor(&home),
        vec![("LOKI_HOME".into(), home.to_string_lossy().into_owned())],
    );
    let (facade, go, _stop, _) = fakego::start_with(
        home.clone(),
        fakego::Opts {
            system: Some(system()),
            engine_ctl: Some(Arc::new(ctl)),
            ..Default::default()
        },
    )
    .await;
    let trace = || std::fs::read_to_string(home.join("trace")).unwrap_or_default();
    // Moteur arrêté : rien à relancer.
    let (_, b) = call(
        facade,
        "POST",
        "/api/apikey",
        r#"{"action":"set","key":"abc"}"#,
        &[],
    )
    .await;
    assert_eq!(
        serde_json::from_str::<Value>(&b).unwrap()["masked"],
        json!("…bc")
    );
    assert_eq!(trace(), "");
    // Moteur actif : relancé par le superviseur, sans passer par le Go.
    let mut sleeper = std::process::Command::new("sleep")
        .arg("30")
        .spawn()
        .unwrap();
    std::fs::write(home.join("loki-engine.pid"), sleeper.id().to_string()).unwrap();
    call(facade, "POST", "/api/apikey", r#"{"action":"clear"}"#, &[]).await;
    assert_eq!(trace(), "restart\n");
    assert_eq!(*go.prestarts.lock().unwrap(), 1);
    assert!(go.service_actions.lock().unwrap().is_empty());
    let _ = sleeper.kill();
    let _ = sleeper.wait();
}

#[tokio::test]
async fn sans_la_variable_les_interrupteurs_restent_au_go() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, _go, _stop) = fakego::start(tmp.path().to_path_buf()).await;
    for p in [
        "/api/internet",
        "/api/computer",
        "/api/apikey",
        "/api/network",
    ] {
        let (_, _, b) = fakego::get(facade, p, &[]).await;
        assert_eq!(&b[..], format!("go:{p}").as_bytes());
    }
}

#[test]
fn fonctions_pures() {
    assert_eq!(sy::key_hint("abcdé"), "…bcdé");
    assert_eq!(sy::key_hint("abcd"), "…");
    assert_eq!(sy::key_hint(""), "");
    let k = sy::gen_api_key();
    assert!(k.starts_with("sk-loki-") && k.len() == 56, "{k}");
    assert_ne!(k, sy::gen_api_key());
}
