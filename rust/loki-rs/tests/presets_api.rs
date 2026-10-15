//! Lot 15 : presets tenus par la façade (`LOKI_PRESETS=rust`).
//!
//! Le journal tiré du Go (`internal/loki/parity_presets_test.go` →
//! `tests/parity_presets/presets.json`) est rejoué : fonctions pures, puis la
//! suite d'appels aux routes par la façade, avec après chacun le dossier
//! presets/ (et models/) et les clés touchées en base. La bascule et
//! l'enregistrement du preset en service, qui pilotent le moteur au Go, sont
//! vérifiés de bout en bout contre le faux Go.

mod common;

use std::collections::{BTreeMap, HashMap};
use std::path::Path;

use common::fakego;
use loki_rs::presets_api as pa;
use serde_json::{Value, json};

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_presets/presets.json")).expect("journal")
}

fn s(v: &Value) -> &str {
    v.as_str().unwrap()
}

fn strs(v: &Value) -> Vec<String> {
    v.as_array()
        .map(|a| a.iter().map(|x| s(x).to_string()).collect())
        .unwrap_or_default()
}

fn cfg(v: &Value) -> HashMap<String, String> {
    v.as_object()
        .unwrap()
        .iter()
        .map(|(k, x)| (k.clone(), s(x).to_string()))
        .collect()
}

#[test]
fn parite_des_fonctions_pures() {
    let fx = fixture();
    for c in fx["display_name"].as_array().unwrap() {
        assert_eq!(
            pa::with_display_name(s(&c["content"]), s(&c["name"])),
            s(&c["out"]),
            "{c}"
        );
    }
    for (v, want) in fx["quote"].as_object().unwrap() {
        assert_eq!(pa::quote_value(v), s(want), "{v:?}");
    }
    let m: BTreeMap<String, String> = [("Z", "1"), ("A", "deux mots"), ("M", ""), ("Q", "q\"q")]
        .iter()
        .map(|(k, v)| (k.to_string(), v.to_string()))
        .collect();
    assert_eq!(pa::format_env(&m), s(&fx["format_env"]));
    let home = Path::new("/h");
    for (n, want) in fx["names"].as_object().unwrap() {
        let got = match pa::safe_preset_path(home, n) {
            Ok(_) => "ok".to_string(),
            Err(e) => e,
        };
        assert_eq!(got, s(want), "{n:?}");
    }
    for (c, want) in fx["quant"].as_object().unwrap() {
        assert_eq!(pa::detect_quant(c), s(want), "{c:?}");
    }
    for (c, want) in fx["reasoning"].as_object().unwrap() {
        assert_eq!(pa::preset_reasoning(c), s(want), "{c:?}");
    }
    for (c, want) in fx["model"].as_object().unwrap() {
        assert_eq!(pa::model_from_preset_content(c), s(want), "{c:?}");
    }
    for (u, want) in fx["completions"].as_object().unwrap() {
        assert_eq!(pa::completions_url(u), s(want), "{u:?}");
    }
    for c in fx["external"].as_array().unwrap() {
        assert_eq!(
            pa::external_preset_content(
                s(&c["url"]),
                s(&c["model"]),
                s(&c["key"]),
                s(&c["ctx"]),
                c["vision"].as_bool().unwrap()
            ),
            s(&c["out"])
        );
    }
    for (b, want) in fx["api_error"].as_object().unwrap() {
        assert_eq!(pa::extract_api_error(b.as_bytes()), s(want), "{b:?}");
    }
    for c in fx["join"].as_array().unwrap() {
        assert_eq!(pa::join_args(&strs(&c["args"])), s(&c["out"]), "{c}");
    }
    for c in fx["set_key"].as_array().unwrap() {
        assert_eq!(
            pa::preset_set_key(s(&c["content"]), s(&c["key"]), s(&c["val"])),
            s(&c["out"]),
            "{c}"
        );
    }
    for c in fx["autoplace"].as_array().unwrap() {
        match pa::auto_placement_preset(s(&c["in"])) {
            Ok((out, changes)) => {
                assert_eq!(s(&c["err"]), "", "{c}");
                assert_eq!(out, s(&c["out"]), "{c}");
                assert_eq!(changes, strs(&c["changes"]), "{c}");
            }
            Err(e) => assert_eq!(e, s(&c["err"]), "{c}"),
        }
    }
    for c in fx["bench_fp"].as_array().unwrap() {
        assert_eq!(
            pa::bench_fingerprint(&cfg(&c["cfg"]), &cfg(&c["cur"])),
            s(&c["out"]),
            "{c}"
        );
    }
    let tmp = tempfile::tempdir().unwrap();
    for (p, want) in fx["imposes"].as_object().unwrap() {
        let p = p.replace("{{HOME}}", tmp.path().to_str().unwrap());
        assert_eq!(
            pa::preset_imposes_engine(tmp.path(), &p),
            want.as_bool().unwrap(),
            "{p:?}"
        );
    }
}

/// Faux service de complétions : mêmes réponses que celui du test Go.
async fn fake_completions() -> String {
    use http_body_util::{BodyExt, Full};
    use hyper::service::service_fn;
    use hyper_util::rt::TokioIo;
    let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    tokio::spawn(async move {
        while let Ok((sock, _)) = l.accept().await {
            tokio::spawn(async move {
                let svc = service_fn(|req: hyper::Request<hyper::body::Incoming>| async move {
                    let path = req.uri().path().to_string();
                    let auth = req
                        .headers()
                        .get("authorization")
                        .map(|v| v.to_str().unwrap().to_string())
                        .unwrap_or_default();
                    let body = req.into_body().collect().await.unwrap().to_bytes();
                    let (code, out): (u16, &str) = if path.starts_with("/ok/") {
                        if String::from_utf8_lossy(&body).contains("\"max_tokens\":1")
                            && auth == "Bearer k1"
                        {
                            (200, "{}")
                        } else {
                            (400, "")
                        }
                    } else if path.starts_with("/refus/") {
                        (401, r#"{"error":{"message":"clé invalide"}}"#)
                    } else if path.starts_with("/brut/") {
                        (404, " page absente ")
                    } else {
                        (500, "")
                    };
                    let mut r = hyper::Response::new(Full::new(bytes::Bytes::from(out)));
                    *r.status_mut() = hyper::StatusCode::from_u16(code).unwrap();
                    Ok::<_, std::convert::Infallible>(r)
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(sock), svc)
                    .await;
            });
        }
    });
    format!("http://{addr}")
}

fn apply_store(go: &fakego::FakeGo, m: &serde_json::Map<String, Value>) {
    for (k, v) in m {
        let (b, key) = k.split_once('/').unwrap();
        let mut st = go.store.lock().unwrap();
        let bucket = st.entry(b.to_string()).or_default();
        if s(v).is_empty() {
            bucket.remove(key);
        } else {
            bucket.insert(key.to_string(), s(v).as_bytes().to_vec());
        }
    }
}

fn disk(home: &Path) -> BTreeMap<String, String> {
    let mut out = BTreeMap::new();
    for d in ["presets", "models"] {
        if let Ok(rd) = std::fs::read_dir(home.join(d)) {
            for e in rd.flatten() {
                out.insert(
                    format!("{d}/{}", e.file_name().to_string_lossy()),
                    String::from_utf8_lossy(&std::fs::read(e.path()).unwrap()).into_owned(),
                );
            }
        }
    }
    out
}

/// Retire récursivement les champs à zéro : le Go réécrit les mesures restantes
/// par sa struct (champs nuls ajoutés), la façade les garde telles quelles —
/// même lecture pour le Go.
fn sans_zeros(v: &Value) -> Value {
    match v {
        Value::Object(m) => Value::Object(
            m.iter()
                .filter(|(_, x)| {
                    !(x.is_null()
                        || x.as_f64() == Some(0.0)
                        || x.as_str() == Some("")
                        || x.as_bool() == Some(false))
                })
                .map(|(k, x)| (k.clone(), sans_zeros(x)))
                .collect(),
        ),
        Value::Array(a) => Value::Array(a.iter().map(sans_zeros).collect()),
        x => x.clone(),
    }
}

fn state(go: &fakego::FakeGo) -> Value {
    let mut st = serde_json::Map::new();
    if let Some(b) = go.store.lock().unwrap().get("state") {
        for (k, v) in b {
            let txt = String::from_utf8_lossy(v).into_owned();
            if k.starts_with("sysprompt:") {
                st.insert(k.clone(), json!(txt));
            } else if k == "preset_order" || k == "bench_presets" {
                st.insert(k.clone(), serde_json::from_str(&txt).unwrap_or(Value::Null));
            }
        }
    }
    Value::Object(st)
}

#[tokio::test]
async fn parite_des_routes() {
    let fx = fixture();
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    let h = home.to_string_lossy().into_owned();
    let url = fake_completions().await;
    let opts = fakego::Opts {
        presets: true,
        ..Default::default()
    };
    let (facade, go, _stop, _) = fakego::start_with(home.clone(), opts).await;
    apply_store(&go, fx["seed"].as_object().unwrap());
    for (i, st) in fx["steps"].as_array().unwrap().iter().enumerate() {
        if let Some(files) = st.get("files").and_then(Value::as_object) {
            for (rel, content) in files {
                let p = home.join(rel);
                if s(content).is_empty() {
                    let _ = std::fs::remove_file(&p);
                } else {
                    std::fs::create_dir_all(p.parent().unwrap()).unwrap();
                    std::fs::write(&p, s(content)).unwrap();
                }
            }
        }
        if let Some(m) = st.get("store").and_then(Value::as_object) {
            apply_store(&go, m);
        }
        let (m, p) = (s(&st["method"]), s(&st["path"]));
        let body = st
            .get("body")
            .and_then(Value::as_str)
            .unwrap_or("")
            .replace("{{URL}}", &url);
        let (code, b) = fakego::send(facade, m, p, &[], &body).await;
        let ctx = format!("étape {i} : {m} {p} {body}");
        let txt = String::from_utf8_lossy(&b)
            .replace(&url, "{{URL}}")
            .replace(&h, "{{HOME}}");
        assert_eq!(
            u64::from(code.as_u16()),
            st["status"].as_u64().unwrap(),
            "{ctx} : {txt}"
        );
        let got: Value = serde_json::from_str(&txt).unwrap();
        assert_eq!(got, st["out"], "{ctx}");
        let d: BTreeMap<String, String> = disk(&home)
            .into_iter()
            .map(|(k, v)| (k, v.replace(&url, "{{URL}}")))
            .collect();
        let want_disk: BTreeMap<String, String> =
            serde_json::from_value(st["disk"].clone()).unwrap();
        assert_eq!(d, want_disk, "{ctx} (disque)");
        assert_eq!(
            sans_zeros(&state(&go)),
            sans_zeros(&st["state"]),
            "{ctx} (base)"
        );
    }
    assert!(go.preset_applies.lock().unwrap().is_empty());
}

/// Bascule, enregistrement du preset en service, verrou de l'optimiseur :
/// la façade fait appliquer par le Go.
#[tokio::test]
async fn bascule_et_application() {
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    let opts = fakego::Opts {
        presets: true,
        ..Default::default()
    };
    let (facade, go, _stop, _) = fakego::start_with(home.clone(), opts).await;
    let dir = home.join("presets");
    std::fs::create_dir_all(&dir).unwrap();
    std::fs::write(dir.join("a.env"), "# NAME=Alpha\nMODEL=a.gguf\n").unwrap();
    std::fs::write(dir.join("b.env"), "# NAME=Beta\nMODEL=b.gguf\n").unwrap();
    std::fs::write(dir.join("refus.env"), "# NAME=Zut\nMODEL=z.gguf\n").unwrap();
    go.set("config", "MODEL", "a.gguf");

    let (c, b) = fakego::send(facade, "POST", "/api/switch", &[], r#"{"id":"b"}"#).await;
    assert_eq!(c, 200);
    assert_eq!(
        serde_json::from_slice::<Value>(&b).unwrap(),
        json!({ "ok": true, "preset": "Beta" })
    );
    let (c, b) = fakego::send(facade, "POST", "/api/switch", &[], r#"{"n":1}"#).await;
    assert_eq!(c, 200, "{}", String::from_utf8_lossy(&b));
    let (c, b) = fakego::send(facade, "POST", "/api/switch", &[], r#"{"id":"zz"}"#).await;
    assert_eq!(c, 404);
    assert_eq!(
        serde_json::from_slice::<Value>(&b).unwrap()["error"],
        "preset introuvable (supprimé ou renommé ?)"
    );
    let (c, _) = fakego::send(facade, "POST", "/api/switch", &[], r#"{"n":9}"#).await;
    assert_eq!(c, 400);
    let (c, b) = fakego::send(facade, "POST", "/api/switch", &[], r#"{"id":"refus"}"#).await;
    assert_eq!(c, 500);
    assert_eq!(
        serde_json::from_slice::<Value>(&b).unwrap()["error"],
        "application refusée"
    );
    // Le preset en service (a) modifié : appliqué, moteur relancé par le Go.
    let (c, b) = fakego::send(
        facade,
        "POST",
        "/api/preset/save",
        &[],
        r#"{"id":"a","name":"Alpha","content":"MODEL=a.gguf\nCTX=100000\n","sysprompt":"Bref."}"#,
    )
    .await;
    assert_eq!(c, 200);
    assert_eq!(
        serde_json::from_slice::<Value>(&b).unwrap()["applied"],
        true
    );
    assert_eq!(go.get("state", "sysprompt:a").as_deref(), Some("Bref."));
    let applies = go.preset_applies.lock().unwrap().clone();
    let names: Vec<(String, String)> = applies
        .iter()
        .map(|(p, m)| {
            (
                Path::new(p)
                    .file_name()
                    .unwrap()
                    .to_string_lossy()
                    .into_owned(),
                m.clone(),
            )
        })
        .collect();
    assert_eq!(
        names,
        vec![
            ("b.env".to_string(), "switch".to_string()),
            ("a.env".to_string(), "switch".to_string()),
            ("a.env".to_string(), "save".to_string()),
        ]
    );
    // Optimisation en cours : refus, rien d'appliqué.
    *go.tune_busy.lock().unwrap() = Some("une optimisation tient le moteur".into());
    let (c, b) = fakego::send(facade, "POST", "/api/switch", &[], r#"{"id":"b"}"#).await;
    assert_eq!(c, 409);
    assert_eq!(
        serde_json::from_slice::<Value>(&b).unwrap(),
        json!({ "ok": false, "tuning": true, "error": "une optimisation tient le moteur" })
    );
    let (c, _) = fakego::send(
        facade,
        "POST",
        "/api/preset/save",
        &[],
        r#"{"name":"N","content":"X=1"}"#,
    )
    .await;
    assert_eq!(c, 409);
    assert_eq!(go.preset_applies.lock().unwrap().len(), 3);
}

#[tokio::test]
async fn sans_la_variable_les_presets_restent_au_go() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, _go, _stop) = fakego::start(tmp.path().to_path_buf()).await;
    for p in [
        "/api/presets",
        "/api/preset/save",
        "/api/switch",
        "/api/preset/external/test",
    ] {
        let (_, _, b) = fakego::get(facade, p, &[]).await;
        assert_eq!(&b[..], format!("go:{p}").as_bytes());
    }
}
