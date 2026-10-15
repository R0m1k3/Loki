//! Lot 13 : mémoire tenue par la façade (`LOKI_MEM=rust`).
//!
//! Le journal tiré du Go (`internal/loki/parity_mem_test.go` →
//! `tests/parity_mem/mem.json`) est rejoué : noms et titres de pages, slugs,
//! modes mémoire, dates des trackers (fuseau fixe UTC+2), puis la suite
//! d'appels aux routes par la façade, avec après chacun le dossier memory/ et
//! les clés touchées en base.

mod common;

use std::collections::BTreeMap;
use std::path::Path;
use std::sync::Arc;

use common::fakego::{self, CryptMode};
use loki_rs::memory::{self, Memory, Tz};
use serde_json::{Value, json};

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_mem/mem.json")).expect("journal")
}

fn now_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_millis() as i64
}

/// Masque identifiants de points et instants « maintenant », dans l'ordre
/// d'apparition (même règle que le test Go).
#[derive(Default)]
struct Masker {
    ids: Vec<(String, String)>,
}

impl Masker {
    fn mask(&mut self, s: &str) -> String {
        let id_re = regex::Regex::new(r#""id":"(\d{15,})""#).unwrap();
        let mut out = String::new();
        let mut last = 0;
        for c in id_re.captures_iter(s) {
            let m = c.get(0).unwrap();
            let id = c[1].to_string();
            let ph = match self.ids.iter().find(|(i, _)| *i == id) {
                Some((_, p)) => p.clone(),
                None => {
                    let p = format!("{{{{ID{}}}}}", self.ids.len() + 1);
                    self.ids.push((id, p.clone()));
                    p
                }
            };
            out.push_str(&s[last..m.start()]);
            out.push_str(&format!(r#""id":"{ph}""#));
            last = m.end();
        }
        out.push_str(&s[last..]);
        let now_re = regex::Regex::new(r#""(ts|first_ts|last_ts)":(\d{12,14})"#).unwrap();
        let now = now_ms();
        now_re
            .replace_all(&out, |c: &regex::Captures| {
                let v: i64 = c[2].parse().unwrap();
                if (v - now).abs() < 86_400_000 {
                    format!(r#""{}":"{{{{NOW}}}}""#, &c[1])
                } else {
                    c[0].to_string()
                }
            })
            .into_owned()
    }

    fn unmask(&self, s: &str) -> String {
        let mut s = s.to_string();
        for (id, ph) in &self.ids {
            s = s.replace(ph, id);
        }
        s
    }

    fn canon(&mut self, v: &Value) -> Value {
        serde_json::from_str(&self.mask(&serde_json::to_string(v).unwrap())).unwrap()
    }
}

#[test]
fn parite_des_fonctions_pures() {
    let fx = fixture();
    for c in fx["names"].as_array().unwrap() {
        let input = c["in"].as_str().unwrap();
        match memory::mem_file_name(input) {
            Ok(o) => assert_eq!(o, c["out"].as_str().unwrap_or("<erreur>"), "{input:?}"),
            Err(e) => assert_eq!(e, c["err"].as_str().unwrap_or("<ok>"), "{input:?}"),
        }
    }
    for (input, want) in fx["titles"].as_object().unwrap() {
        assert_eq!(memory::title_of(input), want.as_str().unwrap(), "{input:?}");
    }
    for (input, want) in fx["slugs"].as_object().unwrap() {
        assert_eq!(
            memory::slugify_ident(input),
            want.as_str().unwrap(),
            "{input:?}"
        );
    }
    for (input, want) in fx["modes"].as_object().unwrap() {
        assert_eq!(
            memory::normalize_mem_mode(input),
            want.as_str().unwrap(),
            "{input:?}"
        );
    }
    for c in fx["when"].as_array().unwrap() {
        let input = c["in"].as_str().unwrap();
        let now = now_ms();
        match memory::parse_when(input, now, Tz::Fixed(7200)) {
            Ok((ts, date_only)) => {
                assert!(c.get("err").is_none(), "{input:?} : erreur attendue");
                let got = if (ts - now).abs() < 86_400_000 {
                    json!("{{NOW}}")
                } else {
                    json!(ts)
                };
                assert_eq!(got, c["ts"], "{input:?}");
                assert_eq!(
                    date_only,
                    c["date_only"].as_bool().unwrap_or(false),
                    "{input:?}"
                );
            }
            Err(e) => assert_eq!(e, c["err"].as_str().unwrap_or("<ok>"), "{input:?}"),
        }
    }
}

fn apply_store(go: &fakego::FakeGo, m: &serde_json::Map<String, Value>) {
    for (k, v) in m {
        let (b, key) = k.split_once('/').unwrap();
        let v = v.as_str().unwrap();
        let mut s = go.store.lock().unwrap();
        let bucket = s.entry(b.to_string()).or_default();
        if v.is_empty() {
            bucket.remove(key);
        } else {
            bucket.insert(key.to_string(), v.as_bytes().to_vec());
        }
    }
}

fn disk(root: &Path) -> BTreeMap<String, String> {
    fn walk(root: &Path, dir: &Path, out: &mut BTreeMap<String, String>) {
        let Ok(rd) = std::fs::read_dir(dir) else {
            return;
        };
        for e in rd.flatten() {
            let p = e.path();
            if e.file_type().unwrap().is_dir() {
                walk(root, &p, out);
            } else {
                let rel = p
                    .strip_prefix(root)
                    .unwrap()
                    .to_string_lossy()
                    .replace('\\', "/");
                out.insert(
                    rel,
                    String::from_utf8_lossy(&std::fs::read(&p).unwrap()).into_owned(),
                );
            }
        }
    }
    let mut out = BTreeMap::new();
    walk(root, root, &mut out);
    out
}

fn state(go: &fakego::FakeGo) -> Value {
    let get = |b: &str, k: &str| go.get(b, k).unwrap_or_default();
    let projects = go
        .get("state", "projects")
        .and_then(|p| serde_json::from_str::<Value>(&p).ok())
        .unwrap_or(Value::Null);
    let mut trackers = serde_json::Map::new();
    let mut raw = serde_json::Map::new();
    if let Some(b) = go.store.lock().unwrap().get("tracker") {
        for (k, v) in b {
            let s = String::from_utf8_lossy(v).into_owned();
            match serde_json::from_str::<Value>(&s) {
                Ok(x) => {
                    trackers.insert(k.clone(), x);
                }
                Err(_) => {
                    raw.insert(k.clone(), Value::String(s));
                }
            }
        }
    }
    json!({
        "agent": get("state", "agent"),
        "active": get("state", "active_project"),
        "compact": get("config", "COMPACT"),
        "mem_mode": get("config", "MEM_MODE"),
        "projects": projects,
        "trackers": trackers,
        "tracker_raw": raw,
    })
}

async fn start(home: &Path, tz: Tz) -> (std::net::SocketAddr, Arc<fakego::FakeGo>, impl Drop) {
    let opts = fakego::Opts {
        mem: Some(Arc::new(Memory::with_tz(home.to_path_buf(), tz))),
        ..Default::default()
    };
    let (facade, go, stop, _) = fakego::start_with(home.to_path_buf(), opts).await;
    struct Stop(Option<tokio::sync::oneshot::Sender<()>>);
    impl Drop for Stop {
        fn drop(&mut self) {
            if let Some(s) = self.0.take() {
                let _ = s.send(());
            }
        }
    }
    (facade, go, Stop(Some(stop)))
}

#[tokio::test]
async fn parite_des_routes() {
    let fx = fixture();
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    let (facade, go, _stop) = start(&home, Tz::Fixed(7200)).await;
    *go.crypt_mode.lock().unwrap() = CryptMode::Plain;
    apply_store(&go, fx["seed"].as_object().unwrap());
    let root = home.join("memory");
    let mut mk = Masker::default();
    for (i, st) in fx["steps"].as_array().unwrap().iter().enumerate() {
        if let Some(files) = st.get("files").and_then(Value::as_object) {
            for (rel, content) in files {
                let p = root.join(rel);
                let content = content.as_str().unwrap();
                if content.is_empty() {
                    let _ = std::fs::remove_dir_all(&p);
                    let _ = std::fs::remove_file(&p);
                } else if rel.ends_with('/') {
                    std::fs::create_dir_all(&p).unwrap();
                } else {
                    std::fs::create_dir_all(p.parent().unwrap()).unwrap();
                    std::fs::write(&p, content).unwrap();
                }
            }
        }
        if let Some(s) = st.get("store").and_then(Value::as_object) {
            apply_store(&go, s);
        }
        let (m, p) = (st["method"].as_str().unwrap(), st["path"].as_str().unwrap());
        let body = st.get("body").and_then(Value::as_str).unwrap_or("");
        let (path, body) = (mk.unmask(p), mk.unmask(body));
        let (code, b) = fakego::send(facade, m, &path, &[], &body).await;
        let ctx = format!("étape {i} : {m} {p} {body}");
        assert_eq!(
            u64::from(code.as_u16()),
            st["status"].as_u64().unwrap(),
            "{ctx} : {}",
            String::from_utf8_lossy(&b)
        );
        let got: Value = serde_json::from_slice(&b).unwrap();
        assert_eq!(mk.canon(&got), st["out"], "{ctx}");
        let want_disk: BTreeMap<String, String> =
            serde_json::from_value(st["disk"].clone()).unwrap();
        assert_eq!(disk(&root), want_disk, "{ctx} (disque)");
        assert_eq!(mk.canon(&state(&go)), st["state"], "{ctx} (base)");
    }
}

/// Pages chiffrées : ouvertes par le Go, écrites scellées ; mémoire
/// verrouillée : listées sans contenu, jamais réécrites en clair.
#[tokio::test]
async fn pages_et_trackers_chiffres() {
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    let (facade, go, _stop) = start(&home, Tz::Fixed(0)).await;
    *go.crypt_mode.lock().unwrap() = CryptMode::Magic;
    let dir = home.join("memory").join("generale");
    let (code, _) = fakego::send(
        facade,
        "POST",
        "/api/mem/save",
        &[],
        r##"{"name":"secret","content":"# Secret\nmot de passe"}"##,
    )
    .await;
    assert_eq!(code, 200);
    let raw = std::fs::read(dir.join("secret.md")).unwrap();
    assert_eq!(raw, b"LOKIMEMv1# Secret\nmot de passe\n");
    // L'index, lui, reste en clair (comme le Go).
    let idx = std::fs::read_to_string(dir.join("MEMORY.md")).unwrap();
    assert!(idx.ends_with("- [Secret](secret.md)\n"), "{idx}");
    let (_, b) = fakego::send(facade, "GET", "/api/mem?name=secret", &[], "").await;
    let v: Value = serde_json::from_slice(&b).unwrap();
    assert_eq!(v["content"], "# Secret\nmot de passe\n");
    let (_, b) = fakego::send(
        facade,
        "POST",
        "/api/tracker",
        &[],
        r#"{"action":"add","name":"Tension","when":"2024-01-02","text":"12/8"}"#,
    )
    .await;
    assert_eq!(serde_json::from_slice::<Value>(&b).unwrap()["ok"], true);
    let sealed = go.get("tracker", "generale/tension").unwrap();
    assert!(sealed.starts_with("LOKIMEMv1{"), "{sealed}");

    // Verrouillée.
    *go.locked.lock().unwrap() = true;
    let (_, b) = fakego::send(facade, "GET", "/api/agent", &[], "").await;
    let v: Value = serde_json::from_slice(&b).unwrap();
    let pages = v["pages"].as_array().unwrap();
    let secret = pages.iter().find(|p| p["name"] == "secret.md").unwrap();
    assert_eq!(secret["desc"], "🔒 (chiffré — mémoire verrouillée)");
    let (code, _) = fakego::send(facade, "GET", "/api/mem?name=secret", &[], "").await;
    assert_eq!(code, 404);
    let (code, b) = fakego::send(
        facade,
        "POST",
        "/api/mem/save",
        &[],
        r#"{"name":"secret","content":"écrasé"}"#,
    )
    .await;
    assert_eq!(code, 400);
    assert_eq!(
        serde_json::from_slice::<Value>(&b).unwrap()["error"],
        "page chiffrée, mémoire verrouillée"
    );
    assert_eq!(
        std::fs::read(dir.join("secret.md")).unwrap(),
        raw,
        "page réécrite alors que la mémoire est verrouillée"
    );
    let (code, b) = fakego::send(
        facade,
        "POST",
        "/api/tracker",
        &[],
        r#"{"action":"add","name":"Autre","text":"x"}"#,
    )
    .await;
    assert_eq!(code, 400);
    assert_eq!(
        serde_json::from_slice::<Value>(&b).unwrap()["error"],
        "valeur chiffrée, mémoire verrouillée"
    );
    // Les trackers scellés ne sont plus lisibles : la liste les tait.
    let (_, b) = fakego::send(facade, "GET", "/api/tracker", &[], "").await;
    assert_eq!(
        serde_json::from_slice::<Value>(&b).unwrap()["trackers"],
        json!([])
    );
}

#[tokio::test]
async fn sans_la_variable_la_memoire_reste_au_go() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, _go, _stop) = fakego::start(tmp.path().to_path_buf()).await;
    for p in [
        "/api/mem",
        "/api/mem/save",
        "/api/mem/move",
        "/api/agent",
        "/api/skills/toggle",
        "/api/memory",
        "/api/tracker",
    ] {
        let (_, _, b) = fakego::get(facade, p, &[]).await;
        assert_eq!(&b[..], format!("go:{p}").as_bytes());
    }
}

/// Le coffre, les instantanés et les projets restent au Go même avec la
/// variable.
#[tokio::test]
async fn coffre_et_projets_restent_au_go() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, _go, _stop) = start(tmp.path(), Tz::Local).await;
    for p in [
        "/api/mem/health",
        "/api/mem/unlock",
        "/api/mem/snapshots",
        "/api/projects",
        "/api/backup/export",
    ] {
        let (_, _, b) = fakego::get(facade, p, &[]).await;
        assert_eq!(&b[..], format!("go:{p}").as_bytes());
    }
}
