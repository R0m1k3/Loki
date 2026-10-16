//! Lot 44 : navigateur piloté (outils `browser_*`) tenu par la façade,
//! rejoué sur le journal du Go (`internal/loki/parity_browser_test.go` →
//! `tests/parity_browser/browser.json`) contre le même faux Chrome : un
//! script qui annonce le port d'un faux serveur CDP, qui répond selon le
//! scénario de chaque étape et note chaque commande reçue.
#![cfg(target_os = "linux")]

mod common;

use std::sync::{Arc, Mutex};

use common::fakecdp::{self, Calls, Scene, image_facts};
use loki_rs::browser::Browser;
use regex::Regex;
use serde_json::{Map, Value, json};
use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
use tokio::net::TcpListener;

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn navigateur_comme_le_go() {
    let fx: Value =
        serde_json::from_str(include_str!("parity_browser/browser.json")).expect("journal");
    let tmp = tempfile::tempdir().unwrap();
    let args_file = tmp.path().join("args");
    let ws = tmp.path().join("ws");
    let profiles = tmp.path().join("profils");
    std::fs::create_dir_all(&ws).unwrap();
    std::fs::create_dir_all(&profiles).unwrap();
    let scene: Scene = Arc::new(Mutex::new(json!({})));
    let calls: Calls = Default::default();
    let port = fakecdp::start(scene.clone(), calls.clone()).await;
    let chrome = fakecdp::chrome_script(tmp.path(), port, &args_file);
    let browser = Browser::with(&chrome.to_string_lossy(), profiles.clone());
    let cap = Regex::new(r"capture-[0-9]{8}-[0-9]{6}\.png").unwrap();
    for st in fx["steps"].as_array().unwrap() {
        let name = st["name"].as_str().unwrap();
        if let Some(sc) = st.get("scene") {
            *scene.lock().unwrap() = sc.clone();
        }
        calls.lock().unwrap().clear();
        let args: Map<String, Value> = serde_json::from_value(st["args"].clone()).unwrap();
        let o = browser
            .exec(
                st["tool"].as_str().unwrap(),
                &args,
                st["vision"] == true,
                &ws.to_string_lossy(),
            )
            .await
            .expect("un outil du navigateur");
        assert_eq!(
            cap.replace_all(&o.result, "capture-{{T}}.png"),
            st["result"].as_str().unwrap(),
            "{name}"
        );
        assert_eq!(
            o.image.as_ref().map(image_facts),
            st.get("image").cloned(),
            "{name} : image"
        );
        let mut saved: Vec<String> = std::fs::read_dir(&ws)
            .unwrap()
            .flatten()
            .map(|e| {
                let n = e.file_name().to_string_lossy().into_owned();
                let _ = std::fs::remove_file(e.path());
                cap.replace_all(&n, "capture-{{T}}.png").into_owned()
            })
            .collect();
        saved.sort();
        assert_eq!(json!(saved), st["saved"], "{name} : captures rangées");
        assert_eq!(
            json!(*calls.lock().unwrap()),
            st["calls"],
            "{name} : commandes CDP"
        );
    }
    let launch: Vec<String> = std::fs::read_to_string(&args_file)
        .unwrap()
        .trim_end_matches('\n')
        .split('\n')
        .map(|l| {
            if l.starts_with("--user-data-dir=") {
                "--user-data-dir={{PROFIL}}".into()
            } else {
                l.to_string()
            }
        })
        .collect();
    assert_eq!(json!(launch), fx["launch"], "lancement");

    // Aperçu : une capture JPEG gardée dans l'anneau, servie par son id.
    let id = browser.auto_shot().await;
    assert!(!id.is_empty());
    let jpeg = browser.shot(&id).unwrap();
    assert_eq!(&jpeg[..2], &[0xff, 0xd8], "JPEG");
    assert_eq!(
        image::load_from_memory(&jpeg)
            .unwrap()
            .to_rgb8()
            .dimensions(),
        (1280, 800)
    );
    // Fermeture : le profil temporaire part avec le navigateur.
    assert_eq!(std::fs::read_dir(&profiles).unwrap().count(), 1);
    browser.shutdown().await;
    assert_eq!(std::fs::read_dir(&profiles).unwrap().count(), 0);
    assert!(!browser.running().await);
}

/// Un vrai Chromium, de bout en bout (comme `TestCDPSmoke`) : page locale,
/// éléments numérotés, recherche, saisie, clic qui change la page, capture
/// quadrillée, aperçu. Seulement avec `LOKI_CU_SMOKE=1` et un navigateur.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn vrai_navigateur() {
    if std::env::var("LOKI_CU_SMOKE").as_deref() != Ok("1") {
        eprintln!("LOKI_CU_SMOKE=1 pour piloter un vrai navigateur");
        return;
    }
    let bin = loki_rs::system::chrome_path();
    if bin.is_empty() {
        eprintln!("aucun navigateur");
        return;
    }
    let page = r#"<!doctype html><html><head><title>Banc Loki</title></head><body>
<h1>Bonjour</h1>
<a href="/suite">Voir la suite</a>
<label for="nom">Nom</label><input id="nom" value="">
<select id="pays"><option>Belgique</option><option>France</option></select>
<button onclick="document.title='Cliqué'">Valider</button>
<div style="height:3000px"></div><a href="/bas">Tout en bas</a>
</body></html>"#;
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    tokio::spawn(async move {
        while let Ok((mut st, _)) = l.accept().await {
            let mut buf = vec![0u8; 4096];
            let n = st.read(&mut buf).await.unwrap_or(0);
            let req = String::from_utf8_lossy(&buf[..n]);
            let body = if req.starts_with("GET /suite") {
                "<html><head><title>Suite</title></head><body><a href=\"/\">Retour</a></body></html>"
            } else {
                page
            };
            let _ = st
                .write_all(
                    format!("HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).as_bytes(),
                )
                .await;
        }
    });
    let tmp = tempfile::tempdir().unwrap();
    let b = Browser::with(&bin, tmp.path().to_path_buf());
    let ws = tmp.path().to_string_lossy().into_owned();
    let run = |name: &'static str, args: Value| {
        let b = &b;
        let ws = ws.clone();
        async move {
            let args: Map<String, Value> = serde_json::from_value(args).unwrap();
            b.exec(name, &args, true, &ws).await.unwrap()
        }
    };
    let open = run("browser_open", json!({ "url": format!("http://{addr}/") })).await;
    assert!(open.result.starts_with("# Banc Loki\n"), "{}", open.result);
    assert!(
        open.result.contains(r#"link "Voir la suite""#),
        "{}",
        open.result
    );
    assert!(open.result.contains("↓ encore ~"), "{}", open.result);
    let find = run("browser_find", json!({ "text": "tout en bas" })).await;
    assert!(
        find.result.contains(r#"link "Tout en bas""#),
        "{}",
        find.result
    );
    let snap = run("browser_snapshot", json!({})).await;
    let re = Regex::new(r#"\[(\d+)\] textbox "Nom""#).unwrap();
    let typed = if let Some(c) = re.captures(&snap.result) {
        run(
            "browser_type",
            json!({ "text": "Jean", "ref": c[1].parse::<f64>().unwrap() }),
        )
        .await
    } else {
        let open = run("browser_open", json!({ "url": format!("http://{addr}/") })).await;
        let c = re.captures(&open.result).expect("champ Nom");
        run(
            "browser_type",
            json!({ "text": "Jean", "ref": c[1].parse::<f64>().unwrap() }),
        )
        .await
    };
    assert!(
        typed.result.contains(r#"textbox "Nom = Jean""#),
        "{}",
        typed.result
    );
    let sel = Regex::new(r#"\[(\d+)\] combobox"#).unwrap();
    let c = sel.captures(&typed.result).expect("liste");
    let chosen = run(
        "browser_type",
        json!({ "text": "fra", "ref": c[1].parse::<f64>().unwrap() }),
    )
    .await;
    assert!(
        chosen.result.starts_with("[ok] saisi « fra »"),
        "{}",
        chosen.result
    );
    // Le lien est hors de la vue depuis le défilement de browser_find.
    let found = run("browser_find", json!({ "text": "voir la suite" })).await;
    let link = Regex::new(r#"\[(\d+)\] link "Voir la suite""#).unwrap();
    let c = link.captures(&found.result).expect("lien");
    let click = run(
        "browser_click",
        json!({ "ref": c[1].parse::<f64>().unwrap() }),
    )
    .await;
    assert!(click.result.contains("# Suite\n"), "{}", click.result);
    let back = run("browser_open", json!({ "url": "back" })).await;
    assert!(back.result.starts_with("# Banc Loki"), "{}", back.result);
    let shot = run("browser_screenshot", json!({})).await;
    assert!(
        shot.result.starts_with("[ok] capture prise"),
        "{}",
        shot.result
    );
    let facts = image_facts(shot.image.as_ref().unwrap());
    assert_eq!(
        (facts["w"].clone(), facts["h"].clone()),
        (json!(1280), json!(800))
    );
    assert!(
        std::fs::read_dir(tmp.path())
            .unwrap()
            .flatten()
            .any(|e| e.file_name().to_string_lossy().starts_with("capture-"))
    );
    let id = b.auto_shot().await;
    assert!(b.shot(&id).is_some());
    let bad = run("browser_open", json!({ "url": "http://127.0.0.1:9/" })).await;
    assert!(
        bad.result
            .starts_with("[erreur] navigation : chargement de http://127.0.0.1:9/ échoué"),
        "{}",
        bad.result
    );
    b.shutdown().await;
}
