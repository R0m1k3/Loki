//! Lot 11 : fichiers de la discussion tenus par la façade (`LOKI_FILES=rust`).
//!
//! Le journal tiré du Go (`internal/loki/parity_files_test.go` →
//! `tests/parity_files/files.json`) est rejoué : noms de dépôt, slug,
//! orientation EXIF, redressement et réduction des pixels, reniflage, puis
//! 42 appels de route par la façade sur la même arborescence — panneau
//! Fichiers, téléchargement, image et vignette, dépôt en morceaux,
//! suppression, archive .zip — avec les fichiers restants après chaque POST.

mod common;

use std::collections::BTreeMap;
use std::net::SocketAddr;
use std::path::Path;
use std::sync::Arc;
use std::time::{Duration, UNIX_EPOCH};

use bytes::Bytes;
use common::fakego;
use http_body_util::{BodyExt, Full};
use hyper::{Request, StatusCode};
use loki_rs::convfiles::{self, Rgba};
use serde_json::Value;

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_files/files.json")).expect("journal")
}

fn b64(s: &str) -> Vec<u8> {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.decode(s).unwrap()
}

fn pix(v: &Value) -> Rgba {
    Rgba {
        w: v["w"].as_u64().unwrap() as usize,
        h: v["h"].as_u64().unwrap() as usize,
        pix: b64(v["pix"].as_str().unwrap_or("")),
    }
}

#[test]
fn parite_des_fonctions_pures() {
    let fx = fixture();
    for (n, want) in fx["upload_name"].as_object().unwrap() {
        assert_eq!(
            convfiles::safe_upload_name(n),
            want.as_str().unwrap(),
            "{n:?}"
        );
    }
    for (s, want) in fx["slug"].as_object().unwrap() {
        assert_eq!(convfiles::slugify(s), want.as_str().unwrap(), "{s:?}");
    }
    for c in fx["exif"].as_array().unwrap() {
        assert_eq!(
            convfiles::exif_orientation(&b64(c["in"].as_str().unwrap())),
            c["out"].as_i64().unwrap() as i32
        );
    }
    let src = pix(&fx["orient_in"]);
    for c in fx["orient"].as_array().unwrap() {
        let o = c["orient"].as_i64().unwrap() as i32;
        assert_eq!(
            convfiles::apply_orientation(&src, o),
            pix(&c["out"]),
            "orientation {o}"
        );
    }
    for c in fx["scale"].as_array().unwrap() {
        let max = c["max"].as_u64().unwrap() as usize;
        assert_eq!(
            convfiles::scale_down_to_max(&pix(&c["in"]), max),
            pix(&c["out"]),
            "{max}"
        );
    }
    for c in fx["sniff"].as_array().unwrap() {
        let want = c["out"].as_str().unwrap();
        let got = convfiles::sniff_image(&b64(c["in"].as_str().unwrap_or("")));
        if want.starts_with("image/") {
            assert_eq!(got, want);
        } else {
            assert_eq!(got, "", "{want}");
        }
    }
}

fn set_mtime(p: &Path, secs: i64) {
    let f = std::fs::File::open(p).unwrap();
    f.set_modified(UNIX_EPOCH + Duration::from_secs(secs as u64))
        .unwrap();
}

fn tree(dir: &Path) -> BTreeMap<String, u64> {
    let mut out = BTreeMap::new();
    let mut stack = vec![dir.to_path_buf()];
    while let Some(d) = stack.pop() {
        let Ok(rd) = std::fs::read_dir(&d) else {
            continue;
        };
        for e in rd.filter_map(Result::ok) {
            let p = e.path();
            if p.is_dir() {
                stack.push(p);
            } else {
                let rel = p.strip_prefix(dir).unwrap().to_string_lossy().into_owned();
                let rel = mask_part(&rel);
                out.insert(rel, e.metadata().unwrap().len());
            }
        }
    }
    out
}

fn mask_part(s: &str) -> String {
    let re = regex::Regex::new(r"\.upload-[0-9]+\.part").unwrap();
    re.replace_all(s, ".upload-{{N}}.part").into_owned()
}

async fn call(
    facade: SocketAddr,
    method: &str,
    url: &str,
    body: &str,
) -> (StatusCode, hyper::HeaderMap, Bytes) {
    use hyper_util::client::legacy::Client;
    use hyper_util::rt::TokioExecutor;
    let client: Client<_, Full<Bytes>> = Client::builder(TokioExecutor::new()).build_http();
    let r = client
        .request(
            Request::builder()
                .method(method)
                .uri(format!("http://{facade}{url}"))
                .body(Full::new(Bytes::from(body.to_string())))
                .unwrap(),
        )
        .await
        .unwrap();
    let (p, b) = r.into_parts();
    (p.status, p.headers, b.collect().await.unwrap().to_bytes())
}

#[tokio::test]
async fn parite_des_routes() {
    let fx = fixture();
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().canonicalize().unwrap();
    let h = home.to_string_lossy().into_owned();
    // L'arborescence, fichiers profonds d'abord, puis les dates des dossiers.
    let mut nodes: Vec<(&String, &Value)> = fx["tree"].as_object().unwrap().iter().collect();
    nodes.sort_by_key(|(k, _)| std::cmp::Reverse(k.len()));
    for (k, n) in &nodes {
        let p = home.join(k);
        if n.get("dir").and_then(Value::as_bool).unwrap_or(false) {
            std::fs::create_dir_all(&p).unwrap();
        } else {
            std::fs::create_dir_all(p.parent().unwrap()).unwrap();
            std::fs::write(&p, b64(n.get("data").and_then(Value::as_str).unwrap_or(""))).unwrap();
        }
    }
    for (k, n) in &nodes {
        set_mtime(&home.join(k), n["mtime"].as_i64().unwrap());
    }
    let zips = home.join("tmp");
    std::fs::create_dir_all(&zips).unwrap();
    let opts = fakego::Opts {
        files: Some(Arc::new(convfiles::ConvFiles::new(zips))),
        ..Default::default()
    };
    let (facade, go, _stop, _) = fakego::start_with(home.clone(), opts).await;
    let ws: Value =
        serde_json::from_str(&fx["workspace"].to_string().replace("{{HOME}}", &h)).unwrap();
    *go.conv_workspace.lock().unwrap() = Some(ws.clone());
    let conv_dir = Path::new(ws["dir"].as_str().unwrap()).to_path_buf();

    let date_re = regex::Regex::new(r"[0-9]{4}-[0-9]{2}-[0-9]{2}\.zip").unwrap();
    let mut last_id = String::new();
    for (i, c) in fx["calls"].as_array().unwrap().iter().enumerate() {
        let (m, url) = (c["method"].as_str().unwrap(), c["url"].as_str().unwrap());
        let body = c
            .get("body")
            .and_then(Value::as_str)
            .unwrap_or("")
            .replace("{{ID}}", &last_id);
        let (code, headers, b) = call(facade, m, url, &body).await;
        let ctx = format!("appel {i} : {m} {url} {body}");
        assert_eq!(
            code.as_u16() as u64,
            c["status"].as_u64().unwrap(),
            "{ctx} : {b:?}"
        );
        if let Some(want) = c.get("out") {
            let txt = String::from_utf8_lossy(&b).replace(&h, "{{HOME}}");
            let txt = date_re.replace_all(&txt, "{{DATE}}.zip").into_owned();
            let mut got: Value = serde_json::from_str(&txt).unwrap();
            if let Some(id) = got.get("id").and_then(Value::as_str) {
                last_id = id.to_string();
                got["id"] = Value::String("{{ID}}".into());
            }
            if url.starts_with("/api/chat/files/zip") {
                if let Some(d) = got.get("data").and_then(Value::as_str) {
                    let raw = b64(d);
                    let mut zr = zip::ZipArchive::new(std::io::Cursor::new(raw)).unwrap();
                    let mut entries = BTreeMap::new();
                    for k in 0..zr.len() {
                        let mut f = zr.by_index(k).unwrap();
                        let mut v = Vec::new();
                        std::io::Read::read_to_end(&mut f, &mut v).unwrap();
                        entries.insert(f.name().to_string(), v);
                    }
                    let want_zip: BTreeMap<String, Vec<u8>> = c["zip"]
                        .as_object()
                        .unwrap()
                        .iter()
                        .map(|(k, v)| (k.clone(), b64(v.as_str().unwrap_or(""))))
                        .collect();
                    assert_eq!(entries, want_zip, "{ctx} (contenu de l'archive)");
                    got["data"] = Value::String("…".into());
                }
                if got.get("size").is_some() {
                    got["size"] = Value::String("…".into());
                }
            }
            if let Some(p) = got.get("path").and_then(Value::as_str) {
                got["path"] = Value::String(mask_part(p));
            }
            assert_eq!(&got, want, "{ctx}");
        }
        if let Some(raw) = c.get("raw").and_then(Value::as_str) {
            assert_eq!(b.to_vec(), b64(raw), "{ctx} (corps)");
        }
        if let Some(dims) = c.get("thumb").and_then(Value::as_array) {
            let img = image::load_from_memory(&b).unwrap();
            assert_eq!(
                [img.width() as u64, img.height() as u64],
                [dims[0].as_u64().unwrap(), dims[1].as_u64().unwrap()],
                "{ctx} (vignette)"
            );
        }
        if let Some(hs) = c.get("headers").and_then(Value::as_object) {
            for (k, v) in hs {
                assert_eq!(
                    headers.get(k.as_str()).and_then(|x| x.to_str().ok()),
                    v.as_str(),
                    "{ctx} (en-tête {k})"
                );
            }
        }
        if let Some(want) = c.get("tree") {
            let want: BTreeMap<String, u64> = serde_json::from_value(want.clone()).unwrap();
            assert_eq!(tree(&conv_dir), want, "{ctx} (fichiers)");
        }
    }
}

#[tokio::test]
async fn plages_et_liens() {
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().canonicalize().unwrap();
    let dir = home.join("ws/discussions/c1");
    std::fs::create_dir_all(&dir).unwrap();
    std::fs::write(dir.join("a.txt"), b"0123456789").unwrap();
    // Un lien vers l'extérieur ne sert pas d'échappatoire.
    std::os::unix::fs::symlink("/etc", dir.join("dehors")).unwrap();
    let opts = fakego::Opts {
        files: Some(Arc::new(convfiles::ConvFiles::new(home.clone()))),
        ..Default::default()
    };
    let (facade, go, _stop, _) = fakego::start_with(home.clone(), opts).await;
    *go.conv_workspace.lock().unwrap() = Some(serde_json::json!({
        "dir": dir, "root": home.join("ws"), "id": "c1", "title": "Ma discussion"
    }));
    use hyper_util::client::legacy::Client;
    use hyper_util::rt::TokioExecutor;
    let client: Client<_, Full<Bytes>> = Client::builder(TokioExecutor::new()).build_http();
    let r = client
        .request(
            Request::get(format!("http://{facade}/api/chat/file?path=a.txt"))
                .header("range", "bytes=2-5")
                .body(Full::new(Bytes::new()))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(r.status(), StatusCode::PARTIAL_CONTENT);
    assert_eq!(r.headers()["content-range"], "bytes 2-5/10");
    assert_eq!(
        &r.into_body().collect().await.unwrap().to_bytes()[..],
        b"2345"
    );
    let (code, _, _) = call(
        facade,
        "GET",
        "/api/chat/file?path=dehors/passwd&meta=1",
        "",
    )
    .await;
    assert_eq!(code, StatusCode::FORBIDDEN);
    let (code, _, b) = call(facade, "GET", "/api/chat/files", "").await;
    assert_eq!(code, StatusCode::OK);
    let v: Value = serde_json::from_slice(&b).unwrap();
    // Le lien est listé (comme le Go), mais son contenu ne compte pas.
    assert_eq!(
        (v["total"].as_i64(), v["count"].as_i64()),
        (Some(10), Some(1))
    );
    // Nom d'archive tiré du titre.
    let (_, _, b) = call(facade, "GET", "/api/chat/files/zip?meta=1", "").await;
    let v: Value = serde_json::from_slice(&b).unwrap();
    assert!(
        v["name"]
            .as_str()
            .unwrap()
            .starts_with("loki-ma-discussion-"),
        "{v}"
    );
    assert_eq!(v["files"], 1);
}

#[tokio::test]
async fn sans_la_variable_les_fichiers_restent_au_go() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, _go, _stop) = fakego::start(tmp.path().to_path_buf()).await;
    for p in [
        "/api/chat/files",
        "/api/chat/file",
        "/api/chat/image",
        "/api/chat/files/zip",
    ] {
        let (_, _, b) = fakego::get(facade, p, &[]).await;
        assert_eq!(&b[..], format!("go:{p}").as_bytes());
    }
}
