//! see_video (repris d'AJEAN) tenu par la façade, rejoué sur le journal du
//! Go (`internal/loki/parity_video_tool_test.go` →
//! `tests/parity_images/video.json`) : mêmes fichiers, même faux moteur
//! (`/props`), même capacité vidéo du moteur, étape par étape. La vidéo
//! relayée se compare à l'octet (jamais réencodée).

mod common;

use std::convert::Infallible;
use std::sync::{Arc, Mutex};

use base64::Engine as _;
use bytes::Bytes;
use common::fakego;
use http_body_util::Full;
use hyper::service::service_fn;
use hyper::{Request, Response};
use hyper_util::rt::TokioIo;
use loki_rs::imagetools::{self, VisionProbe};
use loki_rs::store::Store;
use serde_json::{Map, Value, json};
use tokio::net::TcpListener;

/// Le faux moteur : `/props` authentifié, la vision réglée par le test.
async fn fake_engine(sees: Arc<Mutex<(bool, u32)>>) -> u16 {
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = l.local_addr().unwrap().port();
    tokio::spawn(async move {
        while let Ok((st, _)) = l.accept().await {
            let sees = sees.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<hyper::body::Incoming>| {
                    let sees = sees.clone();
                    async move {
                        let ok = req.uri().path() == "/props"
                            && req
                                .headers()
                                .get("authorization")
                                .and_then(|h| h.to_str().ok())
                                == Some("Bearer cle-moteur");
                        let mut r = Response::new(Full::new(Bytes::new()));
                        if ok {
                            let mut s = sees.lock().unwrap();
                            s.1 += 1;
                            r = Response::new(Full::new(Bytes::from(
                                json!({ "modalities": { "vision": s.0 } }).to_string(),
                            )));
                        } else {
                            *r.status_mut() = hyper::StatusCode::UNAUTHORIZED;
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
    port
}

/// `parityVideoFacts`.
fn part_facts(part: &Value, raw: &Map<String, Value>) -> Value {
    let u = part["input_video"]["url"].as_str().unwrap();
    let i = u.find(";base64,").unwrap();
    let b = base64::engine::general_purpose::STANDARD
        .decode(&u[i + 8..])
        .unwrap();
    let mut out = Map::new();
    out.insert("mime".into(), json!(u[..i].trim_start_matches("data:")));
    for (name, r) in raw {
        let rb = base64::engine::general_purpose::STANDARD
            .decode(r.as_str().unwrap_or(""))
            .unwrap();
        if rb == b {
            out.insert("same_as".into(), json!(name));
        }
    }
    Value::Object(out)
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn see_video_comme_le_go() {
    let fx: Value =
        serde_json::from_str(include_str!("parity_images/video.json")).expect("journal");
    let tmp = tempfile::tempdir().unwrap();
    let files = tmp.path().join("fichiers");
    let raw = fx["files"].as_object().unwrap();
    for (name, b) in raw {
        let p = files.join(name);
        std::fs::create_dir_all(p.parent().unwrap()).unwrap();
        std::fs::write(
            &p,
            base64::engine::general_purpose::STANDARD
                .decode(b.as_str().unwrap_or(""))
                .unwrap(),
        )
        .unwrap();
    }
    std::fs::create_dir_all(files.join("dossier.mp4")).unwrap();
    std::fs::File::create(files.join("lourd.mp4"))
        .unwrap()
        .set_len((imagetools::MAX_VIDEO_BYTES + 1) as u64)
        .unwrap();
    let files_s = files.to_string_lossy().replace('\\', "/");

    let sees = Arc::new(Mutex::new((false, 0u32)));
    let port = fake_engine(sees.clone()).await;
    let (_, go, _stop) = fakego::start(tmp.path().join("home")).await;
    let backend = go.backend.lock().unwrap().unwrap();
    let store = Store::new(backend, fakego::JETON.into());
    go.set("state", "api_key", "cle-moteur");
    let mut cfg = std::collections::HashMap::new();
    cfg.insert("PORT".to_string(), port.to_string());
    let probe = VisionProbe::default();

    for st in fx["steps"].as_array().unwrap() {
        let name = st["name"].as_str().unwrap();
        *sees.lock().unwrap() = (st["sees"] == true, 0);
        probe.reset().await;
        let label = st["label"]
            .as_str()
            .unwrap()
            .replace("{{FICHIERS}}", &files_s);
        let (mut result, vid) = imagetools::see_video(
            &label,
            &files,
            "",
            st["vision"] == true,
            st["video"] == true,
            probe.sees(&store, &cfg),
        )
        .await;
        let relay = vid.map(|v| loki_rs::chat::host::see_video_message(&label, v));
        let mut facts = None;
        if let Some(parts) = relay
            .as_ref()
            .and_then(|m| m.content.as_ref())
            .and_then(Value::as_array)
        {
            for p in parts {
                if p["type"] == "input_video" {
                    facts = Some(part_facts(p, raw));
                }
                if p["type"] == "text" {
                    result += &format!("\n[relais] {}", p["text"].as_str().unwrap());
                }
            }
        }
        assert_eq!(
            result.replace(&files_s, "{{FICHIERS}}"),
            st["result"].as_str().unwrap(),
            "{name}"
        );
        assert_eq!(facts, st.get("part").cloned(), "{name} : vidéo relayée");
        assert_eq!(
            sees.lock().unwrap().1 as u64,
            st["probes"].as_u64().unwrap(),
            "{name} : sondes du moteur"
        );
    }
}

/// `videoInputSupported` : jamais pour une API externe, Strata, un preset
/// sans projecteur ou sans moteur ; sinon l'aide du binaire fait foi, relue
/// quand il change.
#[test]
fn capacite_video_du_moteur() {
    let tmp = tempfile::tempdir().unwrap();
    let cfg = |kv: &[(&str, &str)]| -> std::collections::HashMap<String, String> {
        kv.iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect()
    };
    for c in [
        cfg(&[("BIN", "llama-server"), ("MODEL", "m.gguf")]),
        cfg(&[("MMPROJ", "mm.gguf"), ("MODEL", "m.gguf")]),
        cfg(&[("MMPROJ", "mm.gguf"), ("BIN", "absent/llama-server")]),
        cfg(&[
            ("MMPROJ", "mm.gguf"),
            ("EXTERNAL", "1"),
            ("EXTERNAL_VISION", "1"),
        ]),
        cfg(&[
            ("MMPROJ", "mm.gguf"),
            ("ENGINE", "strata"),
            ("BIN", "/bin/sh"),
        ]),
    ] {
        assert!(
            !imagetools::video_input_supported(&c, tmp.path()),
            "{c:?} : vidéo annoncée"
        );
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        let bin = tmp.path().join("llama-server");
        let write = |help: &str, age: u64| {
            std::fs::write(&bin, format!("#!/bin/sh\necho '{help}'\n")).unwrap();
            std::fs::set_permissions(&bin, std::fs::Permissions::from_mode(0o755)).unwrap();
            let f = std::fs::File::options().write(true).open(&bin).unwrap();
            f.set_modified(std::time::SystemTime::now() - std::time::Duration::from_secs(age))
                .unwrap();
        };
        let c = cfg(&[("MMPROJ", "mm.gguf"), ("BIN", "llama-server")]);
        write("--mmproj FILE", 3600);
        assert!(!imagetools::video_input_supported(&c, tmp.path()));
        write("--video-fps N   target fps", 0);
        assert!(
            imagetools::video_input_supported(&c, tmp.path()),
            "moteur mis à jour : --video-fps non relu"
        );
    }
}

#[test]
fn types_de_video() {
    for (f, m) in [
        ("a.mp4", "video/mp4"),
        ("A.MOV", "video/quicktime"),
        ("x/b.webm", "video/webm"),
        ("c.mkv", "video/x-matroska"),
        ("d.avi", "video/x-msvideo"),
        ("e.png", ""),
        ("mp4", ""),
    ] {
        assert_eq!(imagetools::video_mime(f), m, "{f}");
    }
}
