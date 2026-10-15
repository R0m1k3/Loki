//! Lot 8 : routes des modèles tenues par la façade (`LOKI_MODELS=rust`).
//!
//! - le journal tiré du Go (`internal/loki/parity_models_test.go` →
//!   `tests/parity_models/models.json`) : fonctions pures du catalogue, puis
//!   un scénario sur disque (liste, dossiers, suppressions) rejoué à
//!   l'identique ;
//! - le catalogue Hugging Face contre un faux serveur (recherche, dépôt,
//!   jeton, cache) ;
//! - les routes par la façade.

mod common;

use std::collections::HashMap;
use std::convert::Infallible;
use std::net::SocketAddr;
use std::path::Path;
use std::sync::{Arc, Mutex};

use bytes::Bytes;
use common::fakego;
use http_body_util::Full;
use hyper::body::Incoming;
use hyper::{Request, Response, StatusCode, service::service_fn};
use hyper_util::rt::TokioIo;
use loki_rs::downloads::ModelDirs;
use loki_rs::hf::{self, Hardware, HfEntry, HfEnv, HfListing};
use loki_rs::models;
use serde_json::Value;

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_models/models.json")).expect("journal")
}

fn mask_free(v: &mut Value) {
    match v {
        Value::Object(m) => {
            for (k, e) in m.iter_mut() {
                if k == "free" || k == "free_exact" {
                    *e = Value::String("…".into());
                } else {
                    mask_free(e);
                }
            }
        }
        Value::Array(a) => a.iter_mut().for_each(mask_free),
        _ => {}
    }
}

#[test]
fn parite_des_fonctions_pures() {
    let fx = fixture();
    for (n, want) in fx["quant"].as_object().unwrap() {
        assert_eq!(
            models::quant_from_name(n),
            want.as_str().unwrap(),
            "quant({n})"
        );
    }
    for c in fx["split_args"].as_array().unwrap() {
        let want: Vec<String> = serde_json::from_value(c["out"].clone()).unwrap();
        assert_eq!(models::split_args(c["in"].as_str().unwrap()), want, "{c}");
    }
    for (t, want) in fx["mask"].as_object().unwrap() {
        assert_eq!(hf::mask_hf_token(t), want.as_str().unwrap(), "mask({t:?})");
    }
    for c in fx["gated"].as_array().unwrap() {
        assert_eq!(hf::gated_flag(&c["in"]), c["out"].as_bool().unwrap(), "{c}");
    }
    let cl = &fx["classify"];
    let files: Vec<(String, i64)> = cl["files"]
        .as_array()
        .unwrap()
        .iter()
        .map(|f| {
            (
                f["path"].as_str().unwrap().to_string(),
                f["size"].as_i64().unwrap(),
            )
        })
        .collect();
    let got = hf::classify(hf::HF_HOST, cl["repo"].as_str().unwrap(), &files);
    let want: HfListing = serde_json::from_value(cl["out"].clone()).unwrap();
    assert_eq!(got, want);
    // Sérialisé comme le Go (listes vides → null).
    assert_eq!(serde_json::to_value(&got).unwrap(), cl["out"]);
    for c in fx["projector"].as_array().unwrap() {
        let list: Vec<HfEntry> = serde_json::from_value(c["in"].clone()).unwrap_or_default();
        let got = hf::pick_projector(&list)
            .map(|e| e.name.clone())
            .unwrap_or_default();
        assert_eq!(got, c["name"].as_str().unwrap(), "{c}");
    }
    for c in fx["verdicts"].as_array().unwrap() {
        let hw: Hardware = serde_json::from_value(c["hardware"].clone()).unwrap();
        // Sans GPU : null, comme une tranche nil du Go (24.0 contre 24 : même
        // nombre en JSON).
        let gpus = &serde_json::to_value(&hw).unwrap()["gpus"];
        assert_eq!(gpus.is_null(), c["hardware"]["gpus"].is_null());
        let (v, why) = hf::fit_verdict(
            &hw,
            c["weights"].as_i64().unwrap(),
            c["mmproj"].as_i64().unwrap(),
            c["ctx"].as_i64().unwrap(),
        );
        assert_eq!(
            (v.as_str(), why.as_str()),
            (c["verdict"].as_str().unwrap(), c["why"].as_str().unwrap())
        );
    }
}

fn tree_files(home: &Path) -> Vec<String> {
    let mut out = Vec::new();
    for top in ["disque", "models"] {
        let mut stack = vec![home.join(top)];
        while let Some(d) = stack.pop() {
            let Ok(rd) = std::fs::read_dir(&d) else {
                continue;
            };
            for e in rd.filter_map(Result::ok) {
                let p = e.path();
                if p.is_dir() {
                    stack.push(p);
                } else {
                    out.push(p.strip_prefix(home).unwrap().to_string_lossy().into_owned());
                }
            }
        }
    }
    out.sort();
    out
}

/// Monte l'arborescence du scénario sous `home`.
fn setup_tree(sc: &Value, home: &Path) -> (Vec<String>, HashMap<String, String>) {
    let h = home.to_string_lossy().into_owned();
    let unhome = |s: &str| s.replace("{{HOME}}", &h);
    for (rel, size) in sc["files"].as_object().unwrap() {
        let p = home.join(rel);
        let size = size.as_i64().unwrap();
        if size < 0 {
            std::fs::create_dir_all(&p).unwrap();
            continue;
        }
        std::fs::create_dir_all(p.parent().unwrap()).unwrap();
        std::fs::write(&p, vec![0u8; size as usize]).unwrap();
    }
    std::fs::create_dir_all(home.join("presets")).unwrap();
    for (id, content) in sc["presets"].as_object().unwrap() {
        std::fs::write(
            home.join("presets").join(format!("{id}.env")),
            unhome(content.as_str().unwrap()),
        )
        .unwrap();
    }
    let extra = sc["extra"]
        .as_array()
        .unwrap()
        .iter()
        .map(|d| unhome(d.as_str().unwrap()))
        .collect::<Vec<_>>();
    let cfg = sc["config"]
        .as_object()
        .unwrap()
        .iter()
        .map(|(k, v)| (k.clone(), unhome(v.as_str().unwrap())))
        .collect();
    (extra, cfg)
}

#[test]
fn parite_du_scenario_sur_disque() {
    let fx = fixture();
    let sc = &fx["scenario"];
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().canonicalize().unwrap();
    let h = home.to_string_lossy().into_owned();
    let (extra, cfg) = setup_tree(sc, &home);
    let mut dirs = ModelDirs {
        home: home.clone(),
        // saveExtraModelDirs : nettoyée, dédoublonnée.
        stored: loki_rs::gguf::dedup_dirs(&extra, &format!("{h}/models")),
        env: None,
    };
    let norm = |v: Value| -> Value {
        let s = serde_json::to_string(&v).unwrap().replace(&h, "{{HOME}}");
        let mut v: Value = serde_json::from_str(&s).unwrap();
        mask_free(&mut v);
        v
    };
    for (i, c) in sc["calls"].as_array().unwrap().iter().enumerate() {
        let (method, path) = (c["method"].as_str().unwrap(), c["path"].as_str().unwrap());
        let body: Value = c
            .get("body")
            .and_then(Value::as_str)
            .map(|b| serde_json::from_str(&b.replace("{{HOME}}", &h)).unwrap())
            .unwrap_or(Value::Null);
        let s = |k: &str| {
            body.get(k)
                .and_then(Value::as_str)
                .unwrap_or("")
                .to_string()
        };
        let (status, out) = match (method, path) {
            ("GET", "/api/models") => {
                let refs = models::build_refs(&dirs, &cfg, &[]);
                (200, models::list_models(&dirs, &refs))
            }
            ("GET", "/api/models/dirs") => (200, models::dirs_json(&dirs)),
            ("POST", "/api/models/delete") => {
                let refs = models::build_refs(&dirs, &cfg, &[]);
                let force = body.get("force").and_then(Value::as_bool).unwrap_or(false);
                let (code, out) = models::delete_model(&dirs, &refs, &s("name"), force);
                (code.as_u16(), out)
            }
            ("POST", "/api/models/dirs") => {
                match models::dirs_update(&dirs, &s("path"), &s("action")) {
                    Ok(list) => {
                        dirs.stored = list;
                        (200, serde_json::json!({ "ok": true }))
                    }
                    Err(e) => (400, serde_json::json!({ "ok": false, "error": e })),
                }
            }
            _ => panic!("appel inattendu {c}"),
        };
        let ctx = format!(
            "appel {i} : {method} {path} {}",
            c.get("body").unwrap_or(&Value::Null)
        );
        assert_eq!(status as u64, c["status"].as_u64().unwrap(), "{ctx}");
        assert_eq!(norm(out), c["out"], "{ctx}");
        if let Some(want) = c.get("files") {
            let want: Vec<String> = serde_json::from_value(want.clone()).unwrap();
            assert_eq!(tree_files(&home), want, "{ctx}");
        }
        if let Some(want) = c.get("stored") {
            let want: Vec<String> = serde_json::from_value(want.clone()).unwrap();
            let got: Vec<String> = dirs
                .stored
                .iter()
                .map(|d| d.replace(&h, "{{HOME}}"))
                .collect();
            assert_eq!(got, want, "{ctx}");
        }
    }
}

#[test]
fn adresses_et_echappements_comme_le_go() {
    assert_eq!(
        hf::search_endpoint("https://huggingface.co", "  qwen 3 & co  "),
        "https://huggingface.co/api/models?direction=-1&expand%5B%5D=gated&expand%5B%5D=downloads&expand%5B%5D=likes&filter=gguf&limit=25&search=qwen+3+%26+co&sort=downloads"
    );
    // Tronquée à 100 octets, même au milieu d'un caractère (q[:100] du Go).
    let long = format!("{}é", "a".repeat(99));
    assert!(hf::search_endpoint("h", &long).contains(&format!("search={}%C3&", "a".repeat(99))));
    assert_eq!(
        hf::go_path_escape("a b+c&(d)/é"),
        "a%20b+c&%28d%29%2F%C3%A9"
    );
    assert!(
        hf::repo_ok("a/b.c-d_e")
            && !hf::repo_ok("../x")
            && !hf::repo_ok("a/b/c")
            && !hf::repo_ok("-a/b")
    );
}

// ---------------------------------------------------------------------------
// Faux Hugging Face
// ---------------------------------------------------------------------------

#[derive(Default)]
struct FakeHf {
    log: Mutex<Vec<String>>,
    auth: Mutex<Vec<String>>,
    gated: Mutex<Value>,
}

async fn hf_handle(st: Arc<FakeHf>, req: Request<Incoming>) -> Response<Full<Bytes>> {
    let pq = req
        .uri()
        .path_and_query()
        .map(|p| p.as_str().to_string())
        .unwrap_or_default();
    st.log.lock().unwrap().push(pq.clone());
    let auth = req
        .headers()
        .get("authorization")
        .map(|v| v.to_str().unwrap().to_string())
        .unwrap_or_default();
    st.auth.lock().unwrap().push(auth.clone());
    let path = req.uri().path();
    let (code, body) = if path == "/api/whoami-v2" {
        if auth == "Bearer hf_bon_jeton" {
            (200, r#"{"name":"moi","type":"user"}"#.to_string())
        } else {
            (401, r#"{"error":"Invalid credentials"}"#.to_string())
        }
    } else if path == "/api/models" {
        (200, r#"[{"id":"u/a-GGUF","downloads":10,"likes":2,"gated":false},{"id":"u/b-GGUF","downloads":5,"likes":1,"gated":"manual"}]"#.to_string())
    } else if path == "/api/models/u/a-GGUF/tree/main" {
        (200, r#"[{"type":"directory","path":"Q4"},{"type":"file","path":"Q4/a-Q4_K_M.gguf","size":134,"lfs":{"size":5368709120}},{"type":"file","path":"mmproj-Q8_0.gguf","size":629145600},{"type":"file","path":"README.md","size":10}]"#.to_string())
    } else if path == "/api/models/u/a-GGUF" {
        (
            200,
            format!(
                r#"{{"id":"u/a-GGUF","gated":{}}}"#,
                st.gated.lock().unwrap()
            ),
        )
    } else if path == "/api/models/u/verrou/tree/main" {
        (401, "{}".to_string())
    } else {
        (404, "{}".to_string())
    };
    let mut r = Response::new(Full::new(Bytes::from(body)));
    *r.status_mut() = StatusCode::from_u16(code).unwrap();
    r
}

async fn fake_hf() -> (SocketAddr, Arc<FakeHf>) {
    let st = Arc::new(FakeHf::default());
    *st.gated.lock().unwrap() = Value::String("auto".into());
    let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    let s2 = st.clone();
    tokio::spawn(async move {
        while let Ok((s, _)) = l.accept().await {
            let st = s2.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req| {
                    let st = st.clone();
                    async move { Ok::<_, Infallible>(hf_handle(st, req).await) }
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(s), svc)
                    .await;
            });
        }
    });
    (addr, st)
}

#[tokio::test]
async fn catalogue_contre_un_faux_hugging_face() {
    let (addr, st) = fake_hf().await;
    let base = format!("http://{addr}");
    let c = hf::HfClient::new(&base);
    let env = HfEnv {
        token: "hf_x".into(),
        version: "1.2.3".into(),
    };
    let repos = c.search(&env, "qwen").await.unwrap();
    assert_eq!(repos[1]["gated"], true);
    assert_eq!(repos[0]["id"], "u/a-GGUF");
    // Deuxième recherche identique : servie par le cache.
    c.search(&env, "qwen").await.unwrap();
    assert_eq!(st.log.lock().unwrap().len(), 1);

    let l = c.files(&env, " /u/a-GGUF/ ").await.unwrap();
    assert!(l.gated);
    assert_eq!(l.models.len(), 1);
    assert_eq!(l.models[0].size, 5368709120, "taille LFS prise");
    assert_eq!(
        l.models[0].url,
        format!("{base}/u/a-GGUF/resolve/main/Q4/a-Q4_K_M.gguf")
    );
    assert_eq!(l.projectors[0].quant, "Q8_0");
    assert!(l.drafts.is_empty());
    assert!(st.auth.lock().unwrap().iter().all(|a| a == "Bearer hf_x"));

    assert_eq!(
        c.files(&env, "u/verrou").await.unwrap_err(),
        "dépôt à accès restreint — renseigne HF_TOKEN pour y accéder"
    );
    assert_eq!(
        c.files(&env, "u/absent").await.unwrap_err(),
        "dépôt introuvable sur Hugging Face"
    );
    assert_eq!(
        c.files(&env, "../x").await.unwrap_err(),
        "nom de dépôt invalide (attendu « auteur/dépôt »)"
    );
    assert_eq!(c.whoami("1", "hf_bon_jeton").await.unwrap(), "moi");
    assert_eq!(
        c.whoami("1", "mauvais").await.unwrap_err(),
        "jeton refusé par Hugging Face (expiré, révoqué ou mal copié)"
    );
    // Vider le cache : la recherche repart vers Hugging Face.
    let n = st.log.lock().unwrap().len();
    c.clear_cache();
    c.search(&env, "qwen").await.unwrap();
    assert_eq!(st.log.lock().unwrap().len(), n + 1);
}

#[tokio::test]
async fn routes_des_modeles_par_la_facade() {
    let (addr, st) = fake_hf().await;
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().canonicalize().unwrap();
    std::fs::create_dir_all(home.join("models")).unwrap();
    std::fs::write(home.join("models/a.gguf"), b"0123").unwrap();
    std::fs::create_dir_all(home.join("presets")).unwrap();
    std::fs::write(home.join("presets/p.env"), "NAME=P\nMODEL=a.gguf\n").unwrap();
    let ext = home.join("ext");
    std::fs::create_dir_all(&ext).unwrap();
    let (facade, go, _stop) = fakego::start_models(home.clone(), &format!("http://{addr}")).await;

    let (code, _, b) = fakego::get(facade, "/api/models", &[]).await;
    assert_eq!(code, StatusCode::OK);
    let v: Value = serde_json::from_slice(&b).unwrap();
    assert_eq!(v[0]["name"], "a.gguf");
    assert_eq!(v[0]["used"], serde_json::json!(["P"]));

    // Dossier ajouté : rangé dans la base du Go.
    let body = format!(r#"{{"path":"{}"}}"#, ext.display());
    let (code, _) = fakego::send(facade, "POST", "/api/models/dirs", &[], &body).await;
    assert_eq!(code, StatusCode::OK);
    let stored: Vec<String> =
        serde_json::from_str(&go.get("state", "model_dirs").unwrap()).unwrap();
    assert_eq!(stored, [ext.to_string_lossy().into_owned()]);
    let (_, _, b) = fakego::get(facade, "/api/models/dirs", &[]).await;
    let v: Value = serde_json::from_slice(&b).unwrap();
    assert_eq!(v["dirs"][1]["path"], ext.to_string_lossy().as_ref());
    assert_eq!(v["dirs"][1]["count"], 0);

    // Utilisé par un preset : 409, puis force.
    let (code, b) = fakego::send(
        facade,
        "POST",
        "/api/models/delete",
        &[],
        r#"{"name":"a.gguf"}"#,
    )
    .await;
    assert_eq!(code, StatusCode::CONFLICT, "{b:?}");
    let (code, b) = fakego::send(
        facade,
        "POST",
        "/api/models/delete",
        &[],
        r#"{"name":"a.gguf","force":true}"#,
    )
    .await;
    assert_eq!(code, StatusCode::OK);
    assert_eq!(serde_json::from_slice::<Value>(&b).unwrap()["freed"], 4);
    assert!(!home.join("models/a.gguf").exists());

    // Catalogue : matériel joint, jeton absent.
    let (code, _, b) = fakego::get(facade, "/api/hf/search?q=qwen+3", &[]).await;
    assert_eq!(code, StatusCode::OK);
    let v: Value = serde_json::from_slice(&b).unwrap();
    assert_eq!(v["hf_token"], false);
    assert!(v["hardware"]["os"].is_string());
    assert!(st.log.lock().unwrap()[0].contains("search=qwen+3&"));
    let (code, _, b) = fakego::get(facade, "/api/hf/search?q=%20", &[]).await;
    assert_eq!(code, StatusCode::BAD_REQUEST);
    assert_eq!(
        serde_json::from_slice::<Value>(&b).unwrap()["error"],
        "précise ce que tu cherches"
    );
    go.set("config", "CTX", "65536");
    let (code, _, b) = fakego::get(facade, "/api/hf/files?repo=u/a-GGUF", &[]).await;
    assert_eq!(code, StatusCode::OK);
    let v: Value = serde_json::from_slice(&b).unwrap();
    assert_eq!(
        (
            v["ctx"].as_i64(),
            v["vision"].as_bool(),
            v["gated"].as_bool()
        ),
        (Some(65536), Some(true), Some(true))
    );
    assert_eq!(v["drafts"], Value::Null);
    assert!(v["models"][0].get("verdict").is_none() || v["models"][0]["verdict"].is_string());
    let (code, _, _) = fakego::get(facade, "/api/hf/files?repo=u/absent", &[]).await;
    assert_eq!(code, StatusCode::BAD_GATEWAY);

    // Jeton : refusé, accepté (vérifié, enregistré, masqué), effacé.
    let (code, b) = fakego::send(
        facade,
        "POST",
        "/api/hf/token",
        &[],
        r#"{"token":"mauvais"}"#,
    )
    .await;
    assert_eq!(code, StatusCode::BAD_REQUEST, "{b:?}");
    let (code, b) = fakego::send(
        facade,
        "POST",
        "/api/hf/token",
        &[],
        r#"{"token":" hf_bon_jeton "}"#,
    )
    .await;
    assert_eq!(code, StatusCode::OK);
    let v: Value = serde_json::from_slice(&b).unwrap();
    assert_eq!(
        (
            v["set"].as_bool(),
            v["source"].as_str(),
            v["masked"].as_str(),
            v["account"].as_str()
        ),
        (Some(true), Some("config"), Some("hf_…eton"), Some("moi"))
    );
    assert_eq!(go.get("state", "hf_token").as_deref(), Some("hf_bon_jeton"));
    let (_, _, b) = fakego::get(facade, "/api/hf/search?q=autre", &[]).await;
    assert_eq!(
        serde_json::from_slice::<Value>(&b).unwrap()["hf_token"],
        true
    );
    assert_eq!(
        st.auth.lock().unwrap().last().unwrap(),
        "Bearer hf_bon_jeton"
    );
    let (code, b) = fakego::send(facade, "POST", "/api/hf/token", &[], r#"{"token":""}"#).await;
    assert_eq!(code, StatusCode::OK);
    assert_eq!(serde_json::from_slice::<Value>(&b).unwrap()["set"], false);
    assert_eq!(go.get("state", "hf_token"), None);
    let (code, b) = fakego::send(facade, "POST", "/api/hf/token", &[], "x").await;
    assert_eq!(
        (
            code,
            serde_json::from_slice::<Value>(&b).unwrap()["error"].as_str()
        ),
        (StatusCode::BAD_REQUEST, Some("requête illisible"))
    );

    // Rien n'est parti au Go.
    let relayed = go.relayed.lock().unwrap().clone();
    assert!(
        !relayed
            .iter()
            .any(|p| p.starts_with("/api/models") || p.starts_with("/api/hf")),
        "{relayed:?}"
    );
}

#[tokio::test]
async fn sans_la_variable_les_routes_restent_au_go() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, _go, _stop) = fakego::start(tmp.path().to_path_buf()).await;
    for p in [
        "/api/models",
        "/api/models/dirs",
        "/api/hf/token",
        "/api/hf/search?q=x",
    ] {
        let (_, _, b) = fakego::get(facade, p, &[]).await;
        assert!(b.starts_with(b"go:/api/"), "{p} : {b:?}");
    }
}
