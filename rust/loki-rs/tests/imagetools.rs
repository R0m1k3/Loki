//! Lot 45 : outils d'image de l'agent (`see_image`, `web_screenshot`)
//! tenus par la façade, rejoués sur le journal du Go
//! (`internal/loki/parity_image_tools_test.go` →
//! `tests/parity_images/images.json`) : mêmes fichiers, même faux moteur
//! (`/props`), même faux Playwright. Une image laissée telle quelle se
//! compare à l'octet ; une image réencodée par ses dimensions et son type,
//! et une PNG réduite par ses pixels.
#![cfg(target_os = "linux")]

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
use regex::Regex;
use serde_json::{Map, Value, json};
use tokio::net::TcpListener;

const FAKE_PLAYWRIGHT: &str = r#"#!/bin/sh
d=$(dirname "$0")
printf '%s\n' "$@" > "$d/args"
for a in "$@"; do out="$a"; done
mode=$(cat "$d/mode" 2>/dev/null)
case "$mode" in
  echec) echo "Launching chromium" ; echo "" ; echo "page.goto: net::ERR_NAME_NOT_RESOLVED at https://x" >&2 ; echo "Call log:" >&2 ; echo "  - navigating" >&2 ; exit 1 ;;
  vide) : > "$out" ;;
  rien) ;;
  *) cat "$d/image" > "$out" ;;
esac
"#;

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

/// `parityPartFacts`.
fn part_facts(part: &Value, raw: &Map<String, Value>) -> Value {
    let u = part["image_url"]["url"].as_str().unwrap();
    let i = u.find(";base64,").unwrap();
    let mime = u[..i].trim_start_matches("data:");
    let b = base64::engine::general_purpose::STANDARD
        .decode(&u[i + 8..])
        .unwrap();
    let mut out = Map::new();
    out.insert("mime".into(), json!(mime));
    for (name, r) in raw {
        let rb = base64::engine::general_purpose::STANDARD
            .decode(r.as_str().unwrap_or(""))
            .unwrap();
        if rb == b {
            out.insert("same_as".into(), json!(name));
            return Value::Object(out);
        }
    }
    let Ok(img) = image::load_from_memory(&b) else {
        panic!("image relayée illisible");
    };
    let img = img.to_rgba8();
    let (w, h) = img.dimensions();
    out.insert("w".into(), json!(w));
    out.insert("h".into(), json!(h));
    if mime == "image/png" {
        let mut px = Map::new();
        for (x, y) in [(0, 0), (1, 1), (100, 50), (w - 1, h - 1), (w / 2, h / 3)] {
            let p = img.get_pixel(x, y).0;
            px.insert(
                format!("{x},{y}"),
                json!(format!("{},{},{},{}", p[0], p[1], p[2], p[3])),
            );
        }
        out.insert("px".into(), Value::Object(px));
    }
    Value::Object(out)
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn outils_image_comme_le_go() {
    let fx: Value =
        serde_json::from_str(include_str!("parity_images/images.json")).expect("journal");
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
    std::fs::create_dir_all(files.join("dossier.png")).unwrap();
    std::fs::File::create(files.join("lourd.png"))
        .unwrap()
        .set_len(13 << 20)
        .unwrap();
    let files_s = files.to_string_lossy().into_owned();

    let sees = Arc::new(Mutex::new((false, 0u32)));
    let port = fake_engine(sees.clone()).await;
    let (_, go, _stop) = fakego::start(tmp.path().join("home")).await;
    let backend = go.backend.lock().unwrap().unwrap();
    let store = Store::new(backend, fakego::JETON.into());
    go.set("state", "api_key", "cle-moteur");
    let mut cfg = std::collections::HashMap::new();
    cfg.insert("PORT".to_string(), port.to_string());
    let probe = VisionProbe::default();

    let pw_dir = tmp.path().join("pw");
    std::fs::create_dir_all(&pw_dir).unwrap();
    let pw = pw_dir.join("playwright");
    std::fs::write(&pw, FAKE_PLAYWRIGHT).unwrap();
    use std::os::unix::fs::PermissionsExt as _;
    std::fs::set_permissions(&pw, std::fs::Permissions::from_mode(0o755)).unwrap();
    let conv_dir = tmp.path().join("conv");
    let cap_dir = conv_dir.join("captures");
    let stamp_re = Regex::new(r"[0-9]{8}-[0-9]{6}").unwrap();

    for (n, st) in fx["steps"].as_array().unwrap().iter().enumerate() {
        let name = st["name"].as_str().unwrap();
        *sees.lock().unwrap() = (st["sees"] == true, 0);
        probe.reset().await;
        std::fs::write(pw_dir.join("mode"), st["mode"].as_str().unwrap_or("")).unwrap();
        if let Some(img) = st["image"].as_str() {
            let b = base64::engine::general_purpose::STANDARD
                .decode(raw[img].as_str().unwrap())
                .unwrap();
            std::fs::write(pw_dir.join("image"), b).unwrap();
        }
        let _ = std::fs::remove_file(pw_dir.join("args"));
        if name == "ménage des captures" {
            let old = std::time::SystemTime::now() - std::time::Duration::from_secs(3600);
            for i in 0..22 {
                let p = cap_dir.join(format!("vieille-{i:02}.jpg"));
                std::fs::write(&p, "x").unwrap();
                let f = std::fs::File::options().write(true).open(&p).unwrap();
                f.set_modified(old + std::time::Duration::from_secs(i))
                    .unwrap();
            }
        }
        let args: Map<String, Value> =
            serde_json::from_str(&st["args"].to_string().replace("{{FICHIERS}}", &files_s))
                .unwrap();
        let label = st["label"]
            .as_str()
            .unwrap()
            .replace("{{FICHIERS}}", &files_s);
        let vision = st["vision"] == true;
        let (mut result, part, shown) = match st["tool"].as_str().unwrap() {
            "see_image" => {
                let (r, img) =
                    imagetools::see_image(&label, &files, "", vision, probe.sees(&store, &cfg))
                        .await;
                let relay = img.map(|i| loki_rs::chat::host::see_image_message(&label, i));
                (r, relay, String::new())
            }
            _ => {
                let stamp = format!("20260101-1200{:02}", n % 60);
                let shot = imagetools::web_screenshot(
                    &args,
                    &pw.to_string_lossy(),
                    &conv_dir,
                    st["internet"] == true,
                    &stamp,
                )
                .await;
                let relay = if !shot.rel.is_empty() && vision && probe.sees(&store, &cfg).await {
                    imagetools::screenshot_message(&shot.abs)
                        .and_then(|m| serde_json::from_value(m).ok())
                } else {
                    None
                };
                (shot.result, relay, shot.rel)
            }
        };
        let mut facts = None;
        if let Some(m) = &relay_parts(&part) {
            for p in m {
                if p["type"] == "image_url" {
                    facts = Some(part_facts(p, raw));
                }
                if p["type"] == "text" {
                    result += &format!("\n[relais] {}", p["text"].as_str().unwrap());
                }
            }
        }
        let mask = |s: &str| {
            stamp_re
                .replace_all(&s.replace(&files_s, "{{FICHIERS}}"), "{{T}}")
                .into_owned()
        };
        assert_eq!(mask(&result), st["result"].as_str().unwrap(), "{name}");
        assert_eq!(facts, st.get("part").cloned(), "{name} : image relayée");
        assert_eq!(
            mask(&shown),
            st["shown"].as_str().unwrap_or(""),
            "{name} : capture montrée"
        );
        assert_eq!(
            sees.lock().unwrap().1 as u64,
            st["probes"].as_u64().unwrap(),
            "{name} : sondes du moteur"
        );
        let pw_args: Vec<String> = std::fs::read_to_string(pw_dir.join("args"))
            .map(|b| {
                b.trim_end_matches('\n')
                    .split('\n')
                    .map(|l| {
                        mask(&l.replace(&cap_dir.to_string_lossy().into_owned(), "{{CAPTURES}}"))
                    })
                    .collect()
            })
            .unwrap_or_default();
        assert_eq!(
            json!(pw_args),
            st.get("pw_args").cloned().unwrap_or(json!([])),
            "{name} : Playwright"
        );
        if st["tool"] == "web_screenshot" {
            let mut caps: Vec<String> = std::fs::read_dir(&cap_dir)
                .into_iter()
                .flatten()
                .flatten()
                .map(|e| mask(&e.file_name().to_string_lossy()))
                .collect();
            caps.sort();
            assert_eq!(
                json!(caps),
                st.get("captures").cloned().unwrap_or(json!([])),
                "{name} : dossier des captures"
            );
        }
    }
}

/// Les parties d'un message relayé.
fn relay_parts(m: &Option<loki_rs::chat::types::Message>) -> Option<Vec<Value>> {
    let m = m.as_ref()?;
    m.content.as_ref()?.as_array().cloned()
}
