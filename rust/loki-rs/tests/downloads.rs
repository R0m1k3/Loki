//! Lot « modèles » : téléchargements tenus par la façade (`LOKI_DOWNLOADS=rust`).
//!
//! - les fonctions pures rejouent le journal tiré du Go
//!   (`internal/loki/parity_dl_test.go` → `tests/parity_models/download.json`) ;
//! - le téléchargement de bout en bout tourne contre un faux Hugging Face
//!   (plages d'octets, tranches, dépôt verrouillé, annulation).

mod common;

use std::collections::HashMap;
use std::convert::Infallible;
use std::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use bytes::Bytes;
use common::fakego;
use http_body_util::{BodyExt, Full, StreamBody, combinators::BoxBody};
use hyper::body::{Frame, Incoming};
use hyper::{Request, Response, StatusCode, service::service_fn};
use hyper_util::rt::TokioIo;
use loki_rs::downloads::{self, DlEnv, DlState, Downloads, ModelDirs};
use serde_json::Value;

// ---------------------------------------------------------------------------
// Journal de référence du Go
// ---------------------------------------------------------------------------

#[test]
fn parite_des_fonctions_pures() {
    let fx: Value =
        serde_json::from_str(include_str!("parity_models/download.json")).expect("journal");
    let s = |v: &Value, k: &str| v.get(k).and_then(Value::as_str).unwrap_or("").to_string();
    let list = |v: &Value, k: &str| -> Vec<String> {
        v.get(k)
            .and_then(Value::as_array)
            .map(|a| a.iter().map(|x| x.as_str().unwrap().to_string()).collect())
            .unwrap_or_default()
    };

    for c in fx["normalize"].as_array().unwrap() {
        let input = s(c, "in");
        match downloads::normalize_hf_url(&input) {
            Err(e) => assert_eq!(e, s(c, "err"), "normalize({input:?})"),
            Ok((url, name)) => {
                assert_eq!(s(c, "err"), "", "normalize({input:?}) aurait dû échouer");
                assert_eq!(url, s(c, "url"), "url de {input:?}");
                assert_eq!(name, s(c, "name"), "nom de {input:?}");
                let (urls, names) = downloads::shard_url_set(&url, &name);
                assert_eq!(urls, list(c, "urls"), "tranches de {input:?}");
                assert_eq!(names, list(c, "names"), "noms de {input:?}");
            }
        }
    }
    for (v, want) in fx["content_range"].as_object().unwrap() {
        assert_eq!(
            downloads::content_range_total(v),
            want.as_i64().unwrap(),
            "{v:?}"
        );
    }
    for c in fx["source"].as_array().unwrap() {
        let got = downloads::dl_source_error(
            c["status"].as_u64().unwrap() as u16,
            &s(c, "code"),
            &s(c, "url"),
            c["token_set"].as_bool().unwrap(),
        );
        assert_eq!(got, s(c, "err"), "{c}");
    }
    for (h, want) in fx["hf_host"].as_object().unwrap() {
        assert_eq!(downloads::is_hf_host(h), want.as_bool().unwrap(), "{h}");
    }
    for (u, want) in fx["hf_repo"].as_object().unwrap() {
        assert_eq!(
            downloads::hf_repo_from_url(u),
            want.as_str().unwrap(),
            "{u}"
        );
    }

    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().canonicalize().unwrap();
    let home_s = home.to_string_lossy().into_owned();
    std::fs::create_dir_all(home.join("disque")).unwrap();
    let dirs = ModelDirs {
        home: home.clone(),
        stored: vec![format!("{home_s}/disque")],
        env: None,
    };
    for c in fx["dest"].as_array().unwrap() {
        let dir = s(c, "dir").replace("{{HOME}}", &home_s);
        let got = match dirs.download_dest_path(&s(c, "name"), &dir) {
            Ok(p) => ("path", p.replace(&home_s, "{{HOME}}")),
            Err(e) => ("err", e.replace(&home_s, "{{HOME}}")),
        };
        let want = if c.get("path").is_some() {
            ("path", s(c, "path"))
        } else {
            ("err", s(c, "err"))
        };
        assert_eq!(got, want, "{c}");
    }
}

#[test]
fn tailles_lisibles_comme_le_go() {
    assert_eq!(downloads::human_bytes(0), "0 o");
    assert_eq!(downloads::human_bytes((1 << 20) - 1), "1048575 o");
    assert_eq!(downloads::human_bytes(1 << 20), "1 Mo");
    assert_eq!(downloads::human_bytes(1536 << 20), "1.5 Go");
}

// ---------------------------------------------------------------------------
// Faux Hugging Face
// ---------------------------------------------------------------------------

type Body = BoxBody<Bytes, Infallible>;

#[derive(Default)]
struct FakeHf {
    files: Mutex<HashMap<String, Arc<Vec<u8>>>>,
    /// Chemin → (statut, X-Error-Code) imposés.
    refuse: Mutex<HashMap<String, (u16, String)>>,
    /// Ignore les plages (200, corps entier).
    no_ranges: Mutex<bool>,
    /// Corps envoyé au goutte-à-goutte (annulation).
    slow: Mutex<bool>,
    /// « chemin Range » de chaque requête, et en-têtes Authorization vus.
    log: Mutex<Vec<String>>,
    auth: Mutex<Vec<String>>,
}

fn full(status: u16, body: impl Into<Bytes>) -> Response<Body> {
    let mut r = Response::new(Full::new(body.into()).map_err(|e| match e {}).boxed());
    *r.status_mut() = StatusCode::from_u16(status).unwrap();
    r
}

async fn hf_handle(hf: Arc<FakeHf>, req: Request<Incoming>) -> Response<Body> {
    let path = req.uri().path().to_string();
    let range = req
        .headers()
        .get("range")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_string();
    if let Some(a) = req.headers().get("authorization") {
        hf.auth
            .lock()
            .unwrap()
            .push(a.to_str().unwrap().to_string());
    }
    hf.log.lock().unwrap().push(format!("{path} {range}"));
    if let Some((st, code)) = hf.refuse.lock().unwrap().get(&path).cloned() {
        let mut r = full(st, "refus");
        if !code.is_empty() {
            r.headers_mut()
                .insert("x-error-code", code.parse().unwrap());
        }
        return r;
    }
    let Some(data) = hf.files.lock().unwrap().get(&path).cloned() else {
        return full(404, "absent");
    };
    let total = data.len();
    let (status, start, end) = match range.strip_prefix("bytes=") {
        Some(r) if !*hf.no_ranges.lock().unwrap() => {
            let (a, b) = r.split_once('-').unwrap();
            let a: usize = a.parse().unwrap();
            let b: usize = if b.is_empty() {
                total - 1
            } else {
                b.parse::<usize>().unwrap().min(total - 1)
            };
            (206, a, b)
        }
        _ => (200, 0, total - 1),
    };
    let chunk = Bytes::copy_from_slice(&data[start..=end]);
    let mut r = if *hf.slow.lock().unwrap() && chunk.len() > 1 {
        // 64 Kio toutes les 20 ms : le téléchargement dure, on peut l'annuler.
        let s = futures_util::stream::unfold(chunk, |mut rest| async move {
            if rest.is_empty() {
                return None;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
            let part = rest.split_to(rest.len().min(64 << 10));
            Some((Ok::<_, Infallible>(Frame::data(part)), rest))
        });
        let mut r = Response::new(StreamBody::new(s).boxed());
        *r.status_mut() = StatusCode::from_u16(status).unwrap();
        r
    } else {
        full(status, chunk)
    };
    if status == 206 {
        r.headers_mut().insert(
            "content-range",
            format!("bytes {start}-{end}/{total}").parse().unwrap(),
        );
    }
    r
}

async fn fake_hf() -> (SocketAddr, Arc<FakeHf>) {
    let hf = Arc::new(FakeHf::default());
    let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    let h = hf.clone();
    tokio::spawn(async move {
        while let Ok((s, _)) = l.accept().await {
            let h = h.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req| {
                    let h = h.clone();
                    async move { Ok::<_, Infallible>(hf_handle(h, req).await) }
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(s), svc)
                    .await;
            });
        }
    });
    (addr, hf)
}

/// Contenu reconnaissable : un décalage d'octets se verrait.
fn payload(n: usize, seed: u8) -> Vec<u8> {
    (0..n)
        .map(|i| (i as u32).wrapping_mul(2_654_435_761).to_le_bytes()[1] ^ seed)
        .collect()
}

fn env() -> DlEnv {
    DlEnv {
        hf_token: "hf_secret".into(),
        version: "9.9.9".into(),
    }
}

fn dirs_at(home: &Path) -> ModelDirs {
    ModelDirs {
        home: home.to_path_buf(),
        stored: vec![],
        env: None,
    }
}

async fn wait_finished(dl: &Downloads, name: &str) -> DlState {
    for _ in 0..1000 {
        if let Some(s) = dl.status().into_iter().find(|s| s.filename == name)
            && s.finished
        {
            return s;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    panic!("téléchargement {name} jamais terminé : {:?}", dl.status());
}

fn models(home: &Path) -> PathBuf {
    home.join("models")
}

fn entries(dir: &Path) -> Vec<String> {
    let mut v: Vec<String> = std::fs::read_dir(dir)
        .map(|rd| {
            rd.filter_map(Result::ok)
                .map(|e| e.file_name().to_string_lossy().into_owned())
                .collect()
        })
        .unwrap_or_default();
    v.sort();
    v
}

// ---------------------------------------------------------------------------
// De bout en bout
// ---------------------------------------------------------------------------

#[tokio::test]
async fn un_fichier_sur_plusieurs_connexions() {
    let (addr, hf) = fake_hf().await;
    let data = payload(40 << 20, 7); // 40 Mio → 3 parts de 16 Mio au plus
    hf.files
        .lock()
        .unwrap()
        .insert("/u/r/resolve/main/m.gguf".into(), Arc::new(data.clone()));
    let tmp = tempfile::tempdir().unwrap();
    let dl = Arc::new(Downloads::new());
    let (code, out) = dl.start(
        env(),
        &dirs_at(tmp.path()),
        &format!("http://{addr}/u/r/resolve/main/m.gguf"),
        "",
    );
    assert_eq!(code, StatusCode::OK, "{out}");
    assert_eq!(out["filename"], "m.gguf");
    assert_eq!(out["parts"], 1);
    let st = wait_finished(&dl, "m.gguf").await;
    assert_eq!(st.err, "");
    assert!(!st.canceled);
    assert_eq!(
        (st.total, st.done, st.conns, st.parts, st.part),
        (40 << 20, 40 << 20, 3, 1, 1)
    );
    assert_eq!(st.dir, models(tmp.path()).to_string_lossy());
    let got = std::fs::read(models(tmp.path()).join("m.gguf")).unwrap();
    assert!(got == data, "contenu différent");
    assert_eq!(entries(&models(tmp.path())), ["m.gguf"]);
    // Sonde d'un octet, puis trois plages disjointes qui couvrent le fichier.
    let log = hf.log.lock().unwrap().clone();
    assert_eq!(log[0], "/u/r/resolve/main/m.gguf bytes=0-0");
    let mut ranges: Vec<&str> = log[1..]
        .iter()
        .map(|l| l.split(' ').nth(1).unwrap())
        .collect();
    ranges.sort();
    assert_eq!(
        ranges,
        [
            "bytes=0-13981012",
            "bytes=13981013-27962025",
            "bytes=27962026-41943039"
        ]
    );
    // Hôte hors Hugging Face : le jeton ne part pas.
    assert!(hf.auth.lock().unwrap().is_empty());
}

#[tokio::test]
async fn source_sans_plages_un_seul_flux() {
    let (addr, hf) = fake_hf().await;
    let data = payload(3 << 20, 1);
    hf.files
        .lock()
        .unwrap()
        .insert("/m.gguf".into(), Arc::new(data.clone()));
    *hf.no_ranges.lock().unwrap() = true;
    let tmp = tempfile::tempdir().unwrap();
    let dl = Arc::new(Downloads::new());
    let (code, _) = dl.start(
        env(),
        &dirs_at(tmp.path()),
        &format!("http://{addr}/m.gguf"),
        "",
    );
    assert_eq!(code, StatusCode::OK);
    let st = wait_finished(&dl, "m.gguf").await;
    assert_eq!(
        (st.err.as_str(), st.total, st.done, st.conns),
        ("", 3 << 20, 3 << 20, 1)
    );
    assert!(std::fs::read(models(tmp.path()).join("m.gguf")).unwrap() == data);
}

#[tokio::test]
async fn modele_en_tranches_rapatrie_en_entier() {
    let (addr, hf) = fake_hf().await;
    let parts: Vec<Vec<u8>> = (0..3).map(|i| payload(1 << 20, i as u8)).collect();
    for (i, p) in parts.iter().enumerate() {
        hf.files.lock().unwrap().insert(
            format!("/u/r/resolve/main/Q4/m-0000{}-of-00003.gguf", i + 1),
            Arc::new(p.clone()),
        );
    }
    let tmp = tempfile::tempdir().unwrap();
    let m = models(tmp.path());
    std::fs::create_dir_all(&m).unwrap();
    // La première est déjà là : pas retéléchargée.
    std::fs::write(m.join("m-00001-of-00003.gguf"), &parts[0]).unwrap();
    let dl = Arc::new(Downloads::new());
    let (code, out) = dl.start(
        env(),
        &dirs_at(tmp.path()),
        &format!("http://{addr}/u/r/resolve/main/Q4/m-00002-of-00003.gguf"),
        "",
    );
    assert_eq!(code, StatusCode::OK, "{out}");
    assert_eq!(
        (out["filename"].as_str(), out["parts"].as_i64()),
        (Some("m-00001-of-00003.gguf"), Some(2))
    );
    let st = wait_finished(&dl, "m-00001-of-00003.gguf").await;
    assert_eq!(
        (st.err.as_str(), st.total, st.done, st.parts, st.part),
        ("", 2 << 20, 2 << 20, 2, 2)
    );
    for (i, p) in parts.iter().enumerate() {
        let got = std::fs::read(m.join(format!("m-0000{}-of-00003.gguf", i + 1))).unwrap();
        assert!(&got == p, "tranche {}", i + 1);
    }
    assert!(
        !hf.log
            .lock()
            .unwrap()
            .iter()
            .any(|l| l.contains("00001-of")),
        "tranche présente retéléchargée"
    );
    // Tout est là : rien à faire, le modèle est à sélectionner.
    let (code, out) = dl.start(
        env(),
        &dirs_at(tmp.path()),
        &format!("http://{addr}/u/r/resolve/main/Q4/m-00003-of-00003.gguf"),
        "",
    );
    assert_eq!(code, StatusCode::OK);
    assert_eq!(out["exists"], true);
    assert_eq!(out["value"], "m-00001-of-00003.gguf");
    assert_eq!(out["parts"], 0);
}

#[tokio::test]
async fn depot_verrouille_rien_ne_reste() {
    let (addr, hf) = fake_hf().await;
    for i in 1..=2 {
        hf.files.lock().unwrap().insert(
            format!("/g/d/resolve/main/m-0000{i}-of-00002.gguf"),
            Arc::new(payload(1 << 20, i)),
        );
    }
    hf.refuse.lock().unwrap().insert(
        "/g/d/resolve/main/m-00002-of-00002.gguf".into(),
        (401, "GatedRepo".into()),
    );
    let tmp = tempfile::tempdir().unwrap();
    let dl = Arc::new(Downloads::new());
    let (code, _) = dl.start(
        env(),
        &dirs_at(tmp.path()),
        &format!("http://{addr}/g/d/resolve/main/m-00001-of-00002.gguf"),
        "",
    );
    assert_eq!(code, StatusCode::OK);
    let st = wait_finished(&dl, "m-00001-of-00002.gguf").await;
    // Refusé dès la sonde : rien n'a été écrit.
    assert_eq!(
        st.err,
        "dépôt à accès restreint : ses conditions doivent être acceptées sur huggingface.co — le jeton Hugging Face enregistré n'y donne pas accès (expiré, ou conditions non acceptées avec ce compte)"
    );
    assert_eq!(entries(&models(tmp.path())), Vec::<String>::new());
    assert!(
        hf.log
            .lock()
            .unwrap()
            .iter()
            .all(|l| l.ends_with("bytes=0-0"))
    );
}

#[tokio::test]
async fn panne_pendant_la_deuxieme_tranche_efface_la_premiere() {
    let (addr, hf) = fake_hf().await;
    let tmp = tempfile::tempdir().unwrap();
    let dl = Arc::new(Downloads::new());
    for i in 1..=2 {
        hf.files.lock().unwrap().insert(
            format!("/m-0000{i}-of-00002.gguf"),
            Arc::new(payload(1 << 20, i)),
        );
    }
    // La sonde passe, le transfert de la deuxième échoue (fichier retiré).
    let (code, _) = dl.start(
        env(),
        &dirs_at(tmp.path()),
        &format!("http://{addr}/m-00001-of-00002.gguf"),
        "",
    );
    assert_eq!(code, StatusCode::OK);
    // Retire la deuxième dès que la première est écrite.
    let m = models(tmp.path());
    for _ in 0..500 {
        if m.join("m-00001-of-00002.gguf").exists() {
            break;
        }
        if dl.status()[0].finished {
            break;
        }
        tokio::time::sleep(Duration::from_millis(2)).await;
    }
    let st0 = dl.status()[0].clone();
    if st0.finished {
        // Trop rapide pour la machine : le scénario ne se joue pas.
        return;
    }
    hf.files.lock().unwrap().remove("/m-00002-of-00002.gguf");
    let st = wait_finished(&dl, "m-00001-of-00002.gguf").await;
    if st.err.is_empty() {
        return; // deuxième tranche déjà partie avant le retrait
    }
    assert_eq!(st.err, "lien introuvable (HTTP 404)");
    assert_eq!(entries(&m), Vec::<String>::new(), "tranches laissées");
}

#[tokio::test]
async fn annulation_efface_le_part() {
    let (addr, hf) = fake_hf().await;
    hf.files
        .lock()
        .unwrap()
        .insert("/m.gguf".into(), Arc::new(payload(8 << 20, 3)));
    *hf.slow.lock().unwrap() = true;
    let tmp = tempfile::tempdir().unwrap();
    let dl = Arc::new(Downloads::new());
    let (code, _) = dl.start(
        env(),
        &dirs_at(tmp.path()),
        &format!("http://{addr}/m.gguf"),
        "",
    );
    assert_eq!(code, StatusCode::OK);
    // Deuxième lancement pendant le premier : refusé.
    let (code, out) = dl.start(
        env(),
        &dirs_at(tmp.path()),
        &format!("http://{addr}/m.gguf"),
        "",
    );
    assert_eq!(code, StatusCode::CONFLICT);
    assert_eq!(out["error"], "téléchargement déjà en cours pour m.gguf");
    let part = models(tmp.path()).join("m.gguf.part");
    for _ in 0..500 {
        if part.exists() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    assert!(part.exists(), "pas de .part en cours");
    assert!(dl.cancel("m.gguf"));
    assert!(!dl.cancel("autre.gguf"));
    let st = wait_finished(&dl, "m.gguf").await;
    assert!(st.canceled && st.err.is_empty(), "{st:?}");
    assert_eq!(st.speed, 0);
    assert_eq!(entries(&models(tmp.path())), Vec::<String>::new());
}

#[tokio::test]
async fn refus_avant_de_lancer() {
    let tmp = tempfile::tempdir().unwrap();
    let dl = Arc::new(Downloads::new());
    let d = dirs_at(tmp.path());
    let (code, out) = dl.start(env(), &d, "pas un lien", "");
    assert_eq!(
        (code, out["error"].as_str()),
        (
            StatusCode::BAD_REQUEST,
            Some("lien invalide (http/https attendu)")
        )
    );
    let (code, out) = dl.start(env(), &d, "http://h/m.gguf", "/etc");
    assert_eq!(code, StatusCode::BAD_REQUEST);
    assert_eq!(
        out["error"],
        "dossier non autorisé : /etc — ajoute-le dans « Dossiers de modèles »"
    );
    assert!(dl.status().is_empty());
}

// ---------------------------------------------------------------------------
// Par la façade
// ---------------------------------------------------------------------------

#[tokio::test]
async fn routes_servies_par_la_facade() {
    let (addr, hf) = fake_hf().await;
    hf.files.lock().unwrap().insert(
        "/u/r/resolve/main/m.gguf".into(),
        Arc::new(payload(1 << 20, 9)),
    );
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().canonicalize().unwrap();
    let (facade, go, _stop) = fakego::start_downloads(home.clone()).await;
    let url = format!("http://{addr}/u/r/resolve/main/m.gguf");
    let body = format!(r#"{{"url":"{url}","dir":""}}"#);

    // Sonde : taille, dossier, espace.
    let (code, b) = fakego::send(facade, "POST", "/api/models/download/probe", &[], &body).await;
    assert_eq!(code, StatusCode::OK, "{b:?}");
    let v: Value = serde_json::from_slice(&b).unwrap();
    assert_eq!(v["ok"], true);
    assert_eq!(v["size"], 1 << 20);
    assert_eq!(v["parts"], 1);
    assert_eq!(v["enough"], true);
    assert_eq!(v["dir"], models(&home).to_string_lossy().as_ref());

    // Lancement, suivi, puis la sonde dit « déjà là ».
    let (code, b) = fakego::send(facade, "POST", "/api/models/download", &[], &body).await;
    assert_eq!(code, StatusCode::OK, "{b:?}");
    let mut done = false;
    for _ in 0..500 {
        let (_, _, b) = fakego::get(facade, "/api/models/download/status", &[]).await;
        let v: Value = serde_json::from_slice(&b).unwrap();
        if v[0]["finished"] == true {
            assert_eq!(v[0]["error"], "");
            assert_eq!(v[0]["done"], 1 << 20);
            done = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    assert!(done);
    let (_, b) = fakego::send(facade, "POST", "/api/models/download/probe", &[], &body).await;
    let v: Value = serde_json::from_slice(&b).unwrap();
    assert_eq!(
        (v["exists"].as_bool(), v["value"].as_str()),
        (Some(true), Some("m.gguf"))
    );
    assert_eq!(v["size"], 1 << 20);

    // Annuler l'inconnu ; corps vide.
    let (code, b) = fakego::send(
        facade,
        "POST",
        "/api/models/download/cancel",
        &[],
        r#"{"filename":"../x.gguf"}"#,
    )
    .await;
    assert_eq!(code, StatusCode::NOT_FOUND);
    assert_eq!(
        serde_json::from_slice::<Value>(&b).unwrap()["error"],
        "aucun téléchargement pour x.gguf"
    );
    let (code, b) = fakego::send(facade, "POST", "/api/models/download", &[], "").await;
    assert_eq!(
        (
            code,
            serde_json::from_slice::<Value>(&b).unwrap()["error"].as_str()
        ),
        (StatusCode::BAD_REQUEST, Some("EOF"))
    );

    // Clé de pilotage posée : refus sans elle, la remise du Go passe.
    go.set("state", "web_key", "cle");
    let (code, _, _) = fakego::get(facade, "/api/models/download/status", &[]).await;
    assert_eq!(code, StatusCode::UNAUTHORIZED);
    let (code, _, _) = fakego::get(
        facade,
        "/api/models/download/status",
        &[("x-loki-internal", fakego::JETON)],
    )
    .await;
    assert_eq!(code, StatusCode::OK);
    // Rien n'est parti au Go.
    assert!(
        !go.relayed
            .lock()
            .unwrap()
            .iter()
            .any(|p| p.contains("download")),
        "{:?}",
        go.relayed.lock().unwrap()
    );
}

#[tokio::test]
async fn sans_la_variable_les_routes_restent_au_go() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, go, _stop) = fakego::start(tmp.path().to_path_buf()).await;
    let (_, _, b) = fakego::get(facade, "/api/models/download/status", &[]).await;
    assert_eq!(&b[..], b"go:/api/models/download/status");
    assert!(
        go.relayed
            .lock()
            .unwrap()
            .iter()
            .any(|p| p == "/api/models/download/status")
    );
}

#[test]
fn part_perimes_vides_dans_chaque_dossier() {
    let a = tempfile::tempdir().unwrap();
    let b = tempfile::tempdir().unwrap();
    for d in [a.path(), b.path()] {
        std::fs::write(d.join("x.gguf.part"), b"x").unwrap();
        std::fs::write(d.join("X.GGUF.PART"), b"x").unwrap();
        std::fs::write(d.join("y.gguf"), b"y").unwrap();
    }
    downloads::clean_stale_part_files(&[
        a.path().to_string_lossy().into_owned(),
        b.path().to_string_lossy().into_owned(),
        "/nexiste/pas".into(),
    ]);
    for d in [a.path(), b.path()] {
        // Le motif du Go est sensible à la casse.
        assert_eq!(entries(d), ["X.GGUF.PART", "y.gguf"]);
    }
}
