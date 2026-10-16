//! Lot 23 : sonde de gabarit portée en Rust (`chat/tplprobe.rs`).
//!
//! Le journal tiré du Go (`internal/loki/parity_tplprobe_test.go` →
//! `tests/parity_tplprobe/tplprobe.json`) est rejoué échange par échange : un
//! faux moteur rend les réponses enregistrées dans l'ordre et vérifie que le
//! port envoie, octet pour octet, les mêmes requêtes (méthode, chemin, clé,
//! corps). Puis verdicts, lignes de journal, derniers verdicts (courant et par
//! forme), cache, relevés du rendu, comparaisons et empreintes des formes.

use std::collections::VecDeque;
use std::convert::Infallible;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use bytes::Bytes;
use http_body_util::{BodyExt, Full};
use hyper::service::service_fn;
use hyper::{Request, Response};
use hyper_util::rt::TokioIo;
use loki_rs::chat::tplprobe::{self as tp, Env, Prober, Shape, TplProbe};
use loki_rs::chat::types::Tool;
use serde_json::{Map, Value};
use tokio::net::TcpListener;

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_tplprobe/tplprobe.json")).expect("journal")
}

fn s(v: &Value) -> &str {
    v.as_str().unwrap_or("")
}

/// Le faux moteur : les échanges attendus, et les écarts constatés.
#[derive(Default)]
struct Script {
    expected: VecDeque<Value>,
    errors: Vec<String>,
}

async fn fake_engine(script: Arc<Mutex<Script>>) -> u16 {
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = l.local_addr().unwrap().port();
    tokio::spawn(async move {
        while let Ok((st, _)) = l.accept().await {
            let script = script.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<hyper::body::Incoming>| {
                    let script = script.clone();
                    async move {
                        let method = req.method().to_string();
                        let path = req.uri().path().to_string();
                        let auth = req
                            .headers()
                            .get("authorization")
                            .map(|v| String::from_utf8_lossy(v.as_bytes()).into_owned())
                            .unwrap_or_default();
                        let body = req.into_body().collect().await.unwrap().to_bytes();
                        let body = String::from_utf8_lossy(&body).into_owned();
                        let mut sc = script.lock().unwrap();
                        let (code, reply) = match sc.expected.pop_front() {
                            None => {
                                sc.errors
                                    .push(format!("requête en trop : {method} {path} {body}"));
                                (599, String::new())
                            }
                            Some(want) => {
                                let got =
                                    (method.as_str(), path.as_str(), auth.as_str(), body.as_str());
                                let exp = (
                                    s(&want["method"]),
                                    s(&want["path"]),
                                    s(&want["auth"]),
                                    s(&want["body"]),
                                );
                                if got != exp {
                                    sc.errors.push(format!("attendu {exp:?}\n   reçu {got:?}"));
                                }
                                (
                                    want["status"].as_u64().unwrap() as u16,
                                    s(&want["reply"]).to_string(),
                                )
                            }
                        };
                        let mut r = Response::new(Full::new(Bytes::from(reply)));
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
    port
}

fn shape(fx: &Value, name: &str) -> Shape {
    let sh = &fx["shapes"][name];
    let tools: Vec<Tool> = if s(&sh["tools"]).is_empty() {
        Vec::new()
    } else {
        serde_json::from_str(s(&sh["tools"])).unwrap()
    };
    let kwargs: Option<Map<String, Value>> = sh["kwargs"].as_object().cloned();
    Shape::new(&tools, kwargs.as_ref(), s(&sh["effort"]))
}

/// Le verdict comme le journal le range : sans l'instant, avec `cacheable`.
fn result_json(r: &tp::ProbeResult) -> Value {
    let mut v = r.to_json();
    let m = v.as_object_mut().unwrap();
    m.remove("at");
    m.insert("cacheable".into(), Value::Bool(r.cacheable));
    v
}

fn opt_json(r: Option<tp::ProbeResult>) -> Value {
    r.map(|r| result_json(&r)).unwrap_or(Value::Null)
}

fn take(script: &Arc<Mutex<Script>>, ctx: &str) {
    let mut sc = script.lock().unwrap();
    assert!(sc.errors.is_empty(), "{ctx} :\n{}", sc.errors.join("\n"));
    assert!(
        sc.expected.is_empty(),
        "{ctx} : échanges non faits {:?}",
        sc.expected
    );
    sc.errors.clear();
}

#[tokio::test]
async fn parite_des_verdicts() {
    let fx = fixture();
    for sc in fx["scenarios"].as_array().unwrap() {
        let name = s(&sc["name"]);
        let script = Arc::new(Mutex::new(Script::default()));
        let port = fake_engine(script.clone()).await;
        let mut probe = TplProbe::new();
        probe.auto = false;
        probe.backoff = Duration::from_millis(1);
        // Preset externe : décidé sur la configuration, comme le Go.
        let cfg: std::collections::HashMap<String, String> = sc["config"]
            .as_object()
            .into_iter()
            .flatten()
            .map(|(k, v)| (k.clone(), s(v).to_string()))
            .collect();
        let env = Env {
            external: loki_rs::engstatus::is_external(&cfg),
            port: port.into(),
            key: s(&sc["api_key"]).to_string(),
        };
        assert_eq!(
            env.external,
            sc["external"].as_bool().unwrap_or(false),
            "{name}"
        );
        for (i, st) in sc["steps"].as_array().unwrap().iter().enumerate() {
            let ctx = format!("{name} #{i}");
            script.lock().unwrap().expected = st["exchanges"]
                .as_array()
                .into_iter()
                .flatten()
                .cloned()
                .collect();
            let sh = shape(&fx, s(&st["shape"]));
            let r = probe.ensure(&env, &sh).await;
            take(&script, &ctx);
            assert_eq!(result_json(&r), st["result"], "{ctx} : verdict");
            assert_eq!(tp::probe_line(&r), s(&st["line"]), "{ctx} : journal");
            assert_eq!(
                opt_json(probe.caps_current(env.external)),
                st["current"],
                "{ctx} : courant"
            );
            assert_eq!(
                opt_json(probe.caps_for(env.external, &sh)),
                st["for"],
                "{ctx} : forme"
            );
            // Un preset externe n'affiche jamais le verdict du moteur local.
            assert!(probe.caps_current(true).is_none(), "{ctx}");
            assert!(probe.caps_for(true, &sh).is_none(), "{ctx}");
        }
    }
}

#[tokio::test]
async fn parite_des_releves() {
    let fx = fixture();
    let script = Arc::new(Mutex::new(Script::default()));
    let port = fake_engine(script.clone()).await;
    let p = Prober::local(port.into(), "", Duration::from_millis(1));
    for (i, pc) in fx["prints"].as_array().unwrap().iter().enumerate() {
        script.lock().unwrap().expected = pc["exchanges"]
            .as_array()
            .into_iter()
            .flatten()
            .cloned()
            .collect();
        let got = p.render_print(&shape(&fx, s(&pc["shape"]))).await;
        take(&script, &format!("relevé #{i}"));
        match got {
            Ok(out) => {
                assert_eq!(out, s(&pc["out"]), "relevé #{i}");
                assert_eq!(s(&pc["err"]), "", "relevé #{i}");
            }
            Err(e) => {
                assert_eq!(e, s(&pc["err"]), "relevé #{i}");
                assert_eq!(s(&pc["out"]), "", "relevé #{i}");
            }
        }
    }
    for d in fx["diffs"].as_array().unwrap() {
        let (same, detail) = tp::render_diff(s(&d["before"]), s(&d["after"]));
        assert_eq!(same, d["same"].as_bool().unwrap(), "{d}");
        assert_eq!(detail, s(&d["detail"]), "{d}");
    }
    for (name, h) in fx["hashes"].as_object().unwrap() {
        assert_eq!(shape(&fx, name).hash(), s(h), "empreinte {name}");
    }
}

/// La sonde automatique : une à la fois, au plus une par intervalle, et
/// jamais quand elle est coupée.
#[tokio::test]
async fn declenchement_borne() {
    let script = Arc::new(Mutex::new(Script::default()));
    let port = fake_engine(script.clone()).await;
    let env = Env {
        external: false,
        port: port.into(),
        key: String::new(),
    };
    let health =
        || serde_json::json!({"method": "GET", "path": "/health", "status": 503, "reply": ""});
    let mut off = TplProbe::new();
    off.auto = false;
    let off = Arc::new(off);
    off.kick(env.clone(), Shape::default());
    tokio::time::sleep(Duration::from_millis(100)).await;
    take(&script, "coupée");

    let mut p = TplProbe::new();
    p.every = Duration::from_millis(300);
    let p = Arc::new(p);
    script.lock().unwrap().expected = [health()].into();
    p.kick(env.clone(), Shape::default());
    p.kick(env.clone(), Shape::default()); // encore en vol : rien
    tokio::time::sleep(Duration::from_millis(100)).await;
    take(&script, "première");
    p.kick(env.clone(), Shape::default()); // finie, mais trop tôt : rien
    tokio::time::sleep(Duration::from_millis(50)).await;
    take(&script, "trop tôt");
    tokio::time::sleep(Duration::from_millis(300)).await;
    script.lock().unwrap().expected = [health()].into();
    p.kick(env.clone(), Shape::default());
    tokio::time::sleep(Duration::from_millis(100)).await;
    take(&script, "après l'intervalle");
    // La forme sondée en dernier est retenue pour le relevé du rendu.
    assert_eq!(p.last_shape(), Shape::default());
}
