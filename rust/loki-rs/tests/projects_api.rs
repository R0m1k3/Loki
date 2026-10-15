//! Lot 18 : hub des projets tenu par la façade (`LOKI_PROJECTS=rust`).
//!
//! Le journal tiré du Go (`internal/loki/parity_projects_test.go` →
//! `tests/parity_projects/projects.json`) est rejoué par la façade, la
//! conversation tenue par le (faux) Go : liste et comptes, création,
//! renommage, description, bascule, suppression, refus et erreurs, avec après
//! chaque appel le registre, le projet actif, l'arbre memory/, les trackers et
//! l'index des discussions. La bascule et la suppression quand la façade tient
//! la conversation sont vérifiées de bout en bout.

mod common;

use std::collections::BTreeSet;
use std::path::Path;

use common::fakego::{self, CryptMode};
use serde_json::{Value, json};

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_projects/projects.json")).expect("journal")
}

fn s(v: &Value) -> &str {
    v.as_str().unwrap()
}

/// Les dates propres à chaque exécution, à zéro (comme le journal).
fn zero_times(v: &mut Value) {
    match v {
        Value::Object(o) => {
            for (k, x) in o.iter_mut() {
                if matches!(k.as_str(), "created" | "created_at" | "updated") {
                    *x = json!(0);
                } else {
                    zero_times(x);
                }
            }
        }
        Value::Array(a) => a.iter_mut().for_each(zero_times),
        _ => {}
    }
}

fn tree(home: &Path) -> Vec<String> {
    fn walk(root: &Path, p: &Path, out: &mut Vec<String>) {
        let rel = p.strip_prefix(root).unwrap().to_string_lossy().into_owned();
        if p.is_dir() {
            out.push(format!("{rel}/"));
            for e in std::fs::read_dir(p).unwrap().flatten() {
                walk(root, &e.path(), out);
            }
        } else {
            out.push(rel);
        }
    }
    let mut out = Vec::new();
    if home.join("memory").exists() {
        walk(home, &home.join("memory"), &mut out);
    }
    out.sort();
    out
}

fn snapshot(go: &fakego::FakeGo, home: &Path, seen: &BTreeSet<String>) -> Value {
    let mut reg: Value = go
        .get("state", "projects")
        .and_then(|s| serde_json::from_str(&s).ok())
        .unwrap_or(Value::Null);
    zero_times(&mut reg);
    let st = go.store.lock().unwrap();
    let mut trackers: Vec<String> = st
        .get("tracker")
        .map(|b| b.keys().cloned().collect())
        .unwrap_or_default();
    trackers.sort();
    drop(st);
    let idx: Vec<Value> = go
        .get("chat", "index")
        .and_then(|s| serde_json::from_str(&s).ok())
        .unwrap_or_default();
    let mut convs: Vec<String> = idx
        .iter()
        .map(|m| {
            let id = s(&m["id"]);
            let id = if seen.contains(id) { id } else { "{{NEW}}" };
            format!("{id}:{}", m["project"].as_str().unwrap_or(""))
        })
        .collect();
    convs.sort();
    json!({
        "projects": reg,
        "active": go.get("state", "active_project").unwrap_or_default(),
        "tree": tree(home),
        "trackers": trackers,
        "convs": convs,
    })
}

#[tokio::test]
async fn parite_du_hub() {
    let fx = fixture();
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    for (rel, body) in fx["seed_mem"].as_object().unwrap() {
        let p = home.join(rel);
        std::fs::create_dir_all(p.parent().unwrap()).unwrap();
        std::fs::write(p, s(body)).unwrap();
    }
    // Le dossier vide de la graine (sous/ a une page : rien d'autre à créer).
    let (facade, go, _stop, _) = fakego::start_with(
        home.clone(),
        fakego::Opts {
            projects: true,
            ..Default::default()
        },
    )
    .await;
    *go.crypt_mode.lock().unwrap() = CryptMode::Plain;
    for (k, v) in fx["seed_store"].as_object().unwrap() {
        let (b, key) = k.split_once('/').unwrap();
        go.set(b, key, s(v));
    }
    let seen: BTreeSet<String> =
        serde_json::from_str::<Vec<Value>>(s(&fx["seed_store"]["chat/index"]))
            .unwrap()
            .iter()
            .map(|m| s(&m["id"]).to_string())
            .collect();
    assert_eq!(snapshot(&go, &home, &seen), fx["seed"], "graine");
    for st in fx["steps"].as_array().unwrap() {
        let name = s(&st["name"]);
        *go.generating.lock().unwrap() = st["generating"].as_bool() == Some(true);
        let (code, body) = fakego::send(
            facade,
            s(&st["method"]),
            "/api/projects",
            &[],
            st["body"].as_str().unwrap_or(""),
        )
        .await;
        *go.generating.lock().unwrap() = false;
        assert_eq!(
            code.as_u16() as i64,
            st["status"].as_i64().unwrap(),
            "{name}"
        );
        let mut out: Value = serde_json::from_slice(&body).unwrap();
        zero_times(&mut out);
        assert_eq!(out, st["out"], "{name}");
        assert_eq!(snapshot(&go, &home, &seen), st["after"], "{name} : état");
    }
}

#[tokio::test]
async fn bascule_et_suppression_par_la_facade() {
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    let (facade, go, _stop, chat) = fakego::start_with(
        home.clone(),
        fakego::Opts {
            chat: true,
            projects: true,
            ..Default::default()
        },
    )
    .await;
    let chat = chat.unwrap();
    let post = |body: &'static str| async move {
        let (c, b) = fakego::send(facade, "POST", "/api/projects", &[], body).await;
        (c.as_u16(), serde_json::from_slice::<Value>(&b).unwrap())
    };
    let (c, _) = post(r#"{"action":"create","name":"Alpha"}"#).await;
    assert_eq!(c, 200);
    let (c, v) = post(r#"{"action":"switch","slug":"alpha"}"#).await;
    assert_eq!((c, v["active"].clone()), (200, json!("alpha")));
    // La conversation de la façade a suivi : une discussion d'Alpha ouverte.
    let idx = chat.sessions.read_index().await;
    let open = chat.sessions.conv().id();
    assert!(
        idx.iter().any(|m| m.id == open && m.project == "alpha"),
        "{idx:?}"
    );
    assert!(
        go.conv_log
            .lock()
            .unwrap()
            .contains(&"project alpha".to_string())
    );
    // Projet introuvable : refusé par la façade, sans toucher à la conversation.
    let (c, v) = post(r#"{"action":"switch","slug":"nope"}"#).await;
    assert_eq!((c, v["error"].clone()), (400, json!("projet introuvable")));
    // Suppression du projet actif : la façade revient sur Générale et supprime
    // les discussions d'Alpha ; le Go n'y touche pas.
    let (c, v) = post(r#"{"action":"delete","slug":"alpha"}"#).await;
    assert_eq!((c, v["active"].clone()), (200, json!("generale")), "{v}");
    let idx = chat.sessions.read_index().await;
    assert!(idx.iter().all(|m| m.project != "alpha"), "{idx:?}");
    assert!(!home.join("memory/alpha").exists());
    // Pendant un tour de la façade : 409.
    let (c, _) = post(r#"{"action":"create","name":"Beta"}"#).await;
    assert_eq!(c, 200);
    let ep = chat.sessions.conv().begin_turn(Box::new(|| {})).unwrap();
    let (c, v) = post(r#"{"action":"delete","slug":"beta"}"#).await;
    assert_eq!(
        (c, v["error"].clone()),
        (409, json!("une génération est en cours"))
    );
    let (c, _) = post(r#"{"action":"switch","slug":"beta"}"#).await;
    assert_eq!(c, 409);
    // Le tour passe avant tout le reste, projet inconnu compris (comme quand
    // le Go tient la conversation).
    let (c, _) = post(r#"{"action":"switch","slug":"nope"}"#).await;
    assert_eq!(c, 409);
    chat.sessions.conv().release_turn(ep);
}

#[tokio::test]
async fn sans_la_variable_le_hub_reste_au_go() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, _go, _stop) = fakego::start(tmp.path().to_path_buf()).await;
    let (_, _, b) = fakego::get(facade, "/api/projects", &[]).await;
    assert_eq!(&b[..], b"go:/api/projects");
}
