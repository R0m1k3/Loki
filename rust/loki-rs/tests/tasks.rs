//! Lot 26 : tâches planifiées tenues par la façade (`LOKI_TASKS=rust`).
//!
//! Le journal tiré du Go (`internal/loki/parity_tasks_test.go` →
//! `tests/parity_tasks/tasks.json`) est rejoué par une façade dont l'heure
//! des tâches est fixée pas à pas et dont les tics du planificateur sont
//! déclenchés à la demande. Le faux Go tient la base, le moteur occupé et la
//! discussion ouverte ; une tâche IA qu'il reçoit ne tourne pas (le modèle
//! n'est jamais prêt ici, comme dans le journal).

#![cfg(unix)]

mod common;

use std::collections::HashMap;
use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicI64, Ordering};
use std::time::Duration;

use common::fakego;
use common::parity::apply_files;
use loki_rs::store::Store;
use loki_rs::tasks::{self, Tasks};
use regex::Regex;
use serde_json::{Value, json};

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_tasks/tasks.json")).expect("journal")
}

fn s(v: &Value) -> &str {
    v.as_str().unwrap_or("")
}

const TUNE_BUSY: &str = "optimisation en cours (loki tune) : le moteur est réservé aux essais — attends la fin ou annule-la";

/// L'heure des tâches du rejeu (ns Unix).
static NOW: AtomicI64 = AtomicI64::new(0);

fn clock() -> i64 {
    NOW.load(Ordering::SeqCst)
}

fn rfc3339_ns(v: &str) -> i64 {
    let d = &v[..10];
    let t = &v[11..19];
    let p: Vec<i64> = d.split('-').map(|x| x.parse().unwrap()).collect();
    let h: Vec<i64> = t.split(':').map(|x| x.parse().unwrap()).collect();
    let utc = loki_rs::tz::Location::utc();
    utc.date(p[0], p[1], p[2], h[0], h[1], h[2]) * 1_000_000_000
}

struct Bench {
    facade: SocketAddr,
    go: Arc<fakego::FakeGo>,
    _stop: tokio::sync::oneshot::Sender<()>,
    home: PathBuf,
    tasks: Arc<Tasks>,
    store: Store,
    ids: HashMap<String, String>,
    order: Vec<String>,
    _tmp: tempfile::TempDir,
}

async fn bench(clock: fn() -> i64) -> Bench {
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    let tasks = Arc::new(Tasks::with(home.clone(), Duration::from_secs(3600), clock));
    let (facade, go, stop, _) = fakego::start_with(
        home.clone(),
        fakego::Opts {
            tasks: Some(tasks.clone()),
            ..Default::default()
        },
    )
    .await;
    // Pas de modèle prêt : le port du moteur ne répond pas.
    let port = std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port();
    go.set("config", "PORT", &port.to_string());
    let ws = home.join("workspace").join("actif");
    std::fs::create_dir_all(&ws).unwrap();
    *go.conv_workspace.lock().unwrap() = Some(json!({
        "dir": ws, "root": home.join("workspace"), "id": "actif", "title": ""
    }));
    *go.conv_ensure.lock().unwrap() = true;
    let backend = go.backend.lock().unwrap().unwrap();
    let store = Store::new(backend, fakego::JETON.to_string());
    // Le premier passage du planificateur.
    for _ in 0..500 {
        if tasks.ticks() > 0 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    Bench {
        facade,
        go,
        _stop: stop,
        home,
        tasks,
        store,
        ids: HashMap::new(),
        order: Vec::new(),
        _tmp: tmp,
    }
}

impl Bench {
    /// Les identifiants tirés au hasard deviennent {{ID1}}, {{ID2}}… dans
    /// l'ordre où ils apparaissent.
    fn mask(&mut self, v: &str) -> String {
        let id = Regex::new(r"\b[0-9a-f]{12}\b").unwrap();
        let dur = Regex::new(r#"exit: (-?[0-9]+) · [^\n"\\]*"#).unwrap();
        let created = Regex::new(r#""created":[0-9]+"#).unwrap();
        let mut out = String::new();
        let mut last = 0;
        for m in id.find_iter(v) {
            out.push_str(&v[last..m.start()]);
            let k = m.as_str().to_string();
            if !self.ids.contains_key(&k) {
                self.order.push(k.clone());
                self.ids
                    .insert(k.clone(), format!("{{{{ID{}}}}}", self.order.len()));
            }
            out.push_str(&self.ids[&k]);
            last = m.end();
        }
        out.push_str(&v[last..]);
        let out = out.replace(&self.home.to_string_lossy().into_owned(), "{{HOME}}");
        let out = created.replace_all(&out, r#""created":"{{T}}""#);
        dur.replace_all(&out, "exit: $1 · {{DUR}}").into_owned()
    }

    fn unmask(&self, v: &str) -> String {
        let mut v = v.to_string();
        // Les plus longs d'abord : {{ID10}} avant {{ID1}}.
        let mut ids: Vec<_> = self.ids.iter().collect();
        ids.sort_by_key(|(_, m)| std::cmp::Reverse(m.len()));
        for (id, m) in ids {
            v = v.replace(m, id);
        }
        v.replace("{{HOME}}", &self.home.to_string_lossy())
    }

    async fn call(&mut self, method: &str, path: &str, body: &str) -> (u16, Value) {
        let (code, b) = fakego::send(self.facade, method, path, &[], body).await;
        let raw = String::from_utf8_lossy(&b).into_owned();
        let out = serde_json::from_str(&self.mask(&raw))
            .unwrap_or_else(|_| panic!("{path} : corps illisible {raw:?}"));
        (code.as_u16(), out)
    }

    async fn snapshot(&mut self) -> Value {
        let list = tasks::list_tasks(&self.store).await;
        if list.is_empty() {
            return Value::Null;
        }
        let raw = serde_json::to_string(&list).unwrap();
        serde_json::from_str(&self.mask(&raw)).unwrap()
    }

    async fn last_run(&self, id: &str) -> i64 {
        tasks::get_task(&self.store, id)
            .await
            .map_or(0, |t| t.last_run)
    }

    async fn tick(&self) {
        let before = self.tasks.ticks();
        self.tasks.kick();
        for _ in 0..2000 {
            if self.tasks.ticks() > before {
                return;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        panic!("le tic ne finit pas");
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn parite_des_taches() {
    let fx = fixture();
    let steps = fx["steps"].as_array().unwrap();
    NOW.store(rfc3339_ns(s(&steps[0]["now"])), Ordering::SeqCst);
    let mut bn = bench(clock).await;
    let mut ia_runs = 0;
    for st in steps {
        let name = s(&st["name"]);
        if let Some(n) = st["now"].as_str() {
            NOW.store(rfc3339_ns(n), Ordering::SeqCst);
        }
        let files: serde_json::Map<String, Value> = st["files"]
            .as_object()
            .into_iter()
            .flatten()
            .map(|(k, v)| (k.clone(), Value::String(bn.unmask(s(v)))))
            .collect();
        apply_files(&bn.home, &Value::Object(files));
        for (k, v) in st["config"].as_object().into_iter().flatten() {
            bn.go.set("config", k, s(v));
        }
        for (k, v) in st["state"].as_object().into_iter().flatten() {
            bn.go.set("state", k, s(v));
        }
        if let Some(t) = st["put_task"].as_str() {
            let v: Value = serde_json::from_str(t).unwrap();
            bn.go.set("tasks", s(&v["id"]), t);
        }
        *bn.go.tune_busy.lock().unwrap() =
            (st["tune"].as_bool() == Some(true)).then(|| TUNE_BUSY.to_string());
        let generating = st["generating"].as_bool() == Some(true);
        *bn.go.generating.lock().unwrap() = generating;
        if st["tick"].as_bool() == Some(true) {
            bn.tick().await;
        }
        let before = match st["wait_done"].as_str() {
            Some(id) => bn.last_run(&bn.unmask(id)).await,
            None => 0,
        };
        if let Some(path) = st["path"].as_str() {
            let body = bn.unmask(s(&st["body"]));
            let (code, out) = bn.call(s(&st["method"]), path, &body).await;
            assert_eq!(code as i64, st["status"].as_i64().unwrap(), "{name}");
            assert_eq!(out, st["out"], "{name}");
        }
        *bn.go.generating.lock().unwrap() = false;
        *bn.go.tune_busy.lock().unwrap() = None;
        if let Some(want) = st["wait_running"].as_str() {
            let mut ok = false;
            for _ in 0..1000 {
                let (_, out) = bn.call("GET", "/api/tasks", "").await;
                if out["running_id"] == json!(want) {
                    ok = true;
                    break;
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
            assert!(ok, "{name} : la tâche ne tourne pas");
        }
        if let Some(id) = st["wait_done"].as_str() {
            let id = bn.unmask(id);
            let mut ok = false;
            for _ in 0..2000 {
                if bn.last_run(&id).await != before {
                    ok = true;
                    break;
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
            assert!(ok, "{name} : le passage ne finit pas");
            // Le registre des scripts est vidé juste après l'écriture.
            while bn.tasks.script_running() {
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        }
        let snap = bn.snapshot().await;
        assert_eq!(snap, st["tasks"], "{name} : tâches");
        // Une tâche IA due part au Go, qui ne la fait pas tourner (modèle pas
        // prêt) : son état ne bouge pas.
        let runs = bn.go.task_runs.lock().unwrap().clone();
        if runs.len() != ia_runs {
            assert_eq!(name, "tic : une tâche IA due attend le modèle");
            assert_eq!(runs.len(), ia_runs + 1, "{name}");
            let masked = bn.mask(runs.last().unwrap());
            assert_eq!(masked, "{{ID1}}", "{name}");
            ia_runs = runs.len();
        }
    }
    assert_eq!(ia_runs, 1, "la tâche IA due n'est jamais partie au Go");
    // Les notifications des scripts partent au Go (la façade ne les tient
    // pas) : succès, échec, puis les passages du planificateur.
    let sends = bn.go.push_sends.lock().unwrap().clone();
    let titles: Vec<&str> = sends.iter().map(|(t, _)| t.as_str()).collect();
    assert_eq!(
        titles.first().copied(),
        Some("Loki · tâche terminée"),
        "{sends:?}"
    );
    assert!(
        sends
            .iter()
            .any(|(t, b)| t == "Loki · tâche en échec" && b == "K échec"),
        "{sends:?}"
    );
    assert!(
        !sends.iter().any(|(_, b)| b == "L lente"),
        "un arrêt manuel ne notifie pas : {sends:?}"
    );
}

/// Une tâche IA lancée à la main part au Go, qui l'exécute ; le moteur prêt
/// (preset externe), la façade ne la refuse pas.
#[tokio::test]
async fn tache_ia_executee_par_le_go() {
    let mut bn = bench(tasks::now_ns).await;
    bn.go.set("config", loki_rs::engstatus::EXT_KEY_FLAG, "1");
    let (code, out) = bn
        .call(
            "POST",
            "/api/tasks/save",
            r#"{"name":"IA","prompt":"Fais le point.","schedule":"@every 1h","enabled":true}"#,
        )
        .await;
    assert_eq!(code, 200, "{out}");
    let id = bn.unmask(s(&out["id"]));
    // Le Go tient la tâche un moment : l'écran la montre en cours.
    *bn.go.task_run_delay.lock().unwrap() = Some(Duration::from_millis(400));
    let (code, out) = bn
        .call("POST", "/api/tasks/run", &json!({ "id": id }).to_string())
        .await;
    assert_eq!((code, out), (200, json!({ "ok": true })));
    let masked = bn.mask(&id);
    let mut seen = false;
    for _ in 0..200 {
        let (_, v) = bn.call("GET", "/api/tasks", "").await;
        if v["running_id"] == json!(masked) {
            seen = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    assert!(seen, "la tâche IA n'est pas montrée en cours");
    for _ in 0..500 {
        let (_, v) = bn.call("GET", "/api/tasks", "").await;
        if v["running_id"] == json!("") {
            break;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    let (_, v) = bn.call("GET", "/api/tasks", "").await;
    assert_eq!(v["running_id"], json!(""));
    assert_eq!(*bn.go.task_runs.lock().unwrap(), vec![id.clone()]);
    // Le Go ne l'a pas fait tourner (occupé) : rien n'est enregistré ici.
    let t = tasks::get_task(&bn.store, &id).await.unwrap();
    assert_eq!(
        (t.last_run, t.last_ok, t.last_report.as_str()),
        (0, false, "")
    );
    // Arrêt d'une tâche IA : la génération du Go.
    let (code, _) = bn
        .call("POST", "/api/tasks/stop", &json!({ "id": id }).to_string())
        .await;
    assert_eq!(code, 200);
    assert_eq!(*bn.go.lease_log.lock().unwrap(), vec!["stop".to_string()]);
    let (code, _) = bn
        .call("POST", "/api/tasks/run", r#"{"id":"absente"}"#)
        .await;
    assert_eq!(code, 404);
}

/// Sans demande, le planificateur tourne à son pas.
#[tokio::test]
async fn tics_au_pas_du_planificateur() {
    let tmp = tempfile::tempdir().unwrap();
    let t = Arc::new(Tasks::with(
        tmp.path().to_path_buf(),
        Duration::from_millis(20),
        tasks::now_ns,
    ));
    let (_facade, _go, _stop, _) = fakego::start_with(
        tmp.path().to_path_buf(),
        fakego::Opts {
            tasks: Some(t.clone()),
            ..Default::default()
        },
    )
    .await;
    for _ in 0..500 {
        if t.ticks() >= 4 {
            return;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    panic!("le planificateur ne tourne pas : {} tics", t.ticks());
}

/// Sans la façade pour les tâches, leurs routes partent au Go.
#[tokio::test]
async fn sans_la_facade() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, go, _stop) = fakego::start(tmp.path().to_path_buf()).await;
    let (c, b) = fakego::send(facade, "GET", "/api/tasks", &[], "").await;
    assert_eq!(c.as_u16(), 200);
    assert_eq!(String::from_utf8_lossy(&b), "go:/api/tasks");
    assert!(
        go.relayed
            .lock()
            .unwrap()
            .contains(&"/api/tasks".to_string())
    );
}

/// Une horloge qui avance d'une seconde et demie à chaque lecture.
static TICKING: AtomicI64 = AtomicI64::new(1_774_738_800_000_000_000);

fn ticking() -> i64 {
    TICKING.fetch_add(1_500_000_000, Ordering::SeqCst)
}

/// La durée d'un passage : de son début à sa fin, à l'horloge des tâches.
#[tokio::test]
async fn duree_du_passage() {
    let mut bn = bench(ticking).await;
    apply_files(
        &bn.home,
        &json!({ "scripts/ok.sh": "#!/bin/sh\necho bonjour\n" }),
    );
    let (code, out) = bn
        .call(
            "POST",
            "/api/tasks/save",
            r#"{"name":"S","kind":"script","script":"ok.sh","schedule":"@every 1h","enabled":true}"#,
        )
        .await;
    assert_eq!(code, 200, "{out}");
    let id = bn.unmask(s(&out["id"]));
    let (code, _) = bn
        .call("POST", "/api/tasks/run", &json!({ "id": id }).to_string())
        .await;
    assert_eq!(code, 200);
    let mut t = None;
    for _ in 0..1000 {
        t = tasks::get_task(&bn.store, &id)
            .await
            .filter(|t| t.last_run > 0);
        if t.is_some() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    let t = t.expect("le passage ne finit pas");
    assert!(t.last_ok, "{t:?}");
    assert!(
        t.last_dur_ms > 0 && t.last_dur_ms % 1500 == 0,
        "durée : {}",
        t.last_dur_ms
    );
}

/// Au démarrage, le prochain passage des tâches actives qui n'en ont pas
/// (`primeNextRuns`) ; les autres ne bougent pas.
#[tokio::test]
async fn amorce_au_demarrage() {
    let tmp = tempfile::tempdir().unwrap();
    let t = Arc::new(Tasks::with(
        tmp.path().to_path_buf(),
        Duration::from_secs(3600),
        tasks::now_ns,
    ));
    let task = |id: &str, enabled: bool, next: i64| {
        (
            "tasks".to_string(),
            id.to_string(),
            json!({ "id": id, "name": id, "kind": "script", "script": "ok.sh",
                    "schedule": "@every 1h", "enabled": enabled, "next_run": next })
            .to_string(),
        )
    };
    let (_facade, go, _stop, _) = fakego::start_with(
        tmp.path().to_path_buf(),
        fakego::Opts {
            tasks: Some(t.clone()),
            seed: vec![
                task("active", true, 0),
                task("eteinte", false, 0),
                task("posee", true, 42),
            ],
            ..Default::default()
        },
    )
    .await;
    for _ in 0..500 {
        if t.ticks() > 0 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    assert!(t.ticks() > 0, "pas de premier passage");
    let next = |id: &str| -> i64 {
        let v: Value = serde_json::from_str(&go.get("tasks", id).unwrap()).unwrap();
        v["next_run"].as_i64().unwrap()
    };
    let now_ms = tasks::now_ns() / 1_000_000;
    let a = next("active");
    assert!(
        (now_ms + 3_590_000..=now_ms + 3_600_000).contains(&a),
        "active : {a}"
    );
    assert_eq!(next("eteinte"), 0);
    assert_eq!(next("posee"), 42);
}

fn zero_clock() -> i64 {
    0
}

/// Lot 37 : les outils task_* de l'agent, rejoués contre le journal du Go
/// (`internal/loki/parity_tasks_tools_test.go` → `parity_tasks/tools.json`) :
/// chaque appel, son résultat tel que le modèle le reçoit, les tâches en base
/// après coup.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn parite_des_outils() {
    use loki_rs::tasks::tools::{ToolEnv, exec, is_tool};
    let fx: Value = serde_json::from_str(include_str!("parity_tasks/tools.json")).expect("journal");
    let mut bn = bench(zero_clock).await;
    let scripts = bn.home.join("scripts");
    let mut now = 0;
    for st in fx.as_array().unwrap() {
        let name = s(&st["name"]);
        if let Some(n) = st["now"].as_str() {
            now = rfc3339_ns(n);
        }
        let files: serde_json::Map<String, Value> = st["files"]
            .as_object()
            .into_iter()
            .flatten()
            .map(|(k, v)| (k.clone(), Value::String(bn.unmask(s(v)))))
            .collect();
        apply_files(&bn.home, &Value::Object(files));
        for (k, v) in st["state"].as_object().into_iter().flatten() {
            bn.go.set("state", k, s(v));
        }
        if let Some(t) = st["put_task"].as_str() {
            let raw = bn.unmask(t);
            let v: Value = serde_json::from_str(&raw).unwrap();
            bn.go.set("tasks", s(&v["id"]), &raw);
            bn.mask(&raw);
        }
        let tool = s(&st["tool"]);
        assert!(is_tool(tool), "{name}");
        let args: serde_json::Map<String, Value> =
            serde_json::from_str(&bn.unmask(&st["args"].to_string())).unwrap();
        let env = ToolEnv {
            store: &bn.store,
            scripts_dir: &scripts,
            now_ns: now,
        };
        let out = exec(&env, tool, &args).await.expect("outil task_*");
        assert_eq!(bn.mask(&out), s(&st["out"]), "{name}");
        assert_eq!(bn.snapshot().await, st["tasks"], "{name}");
    }
    let env = ToolEnv {
        store: &bn.store,
        scripts_dir: &scripts,
        now_ns: now,
    };
    assert_eq!(exec(&env, "bash", &serde_json::Map::new()).await, None);
    assert!(!is_tool("task_run"));
}
