//! Lot 22 : backends llama.cpp tenus par la façade (`LOKI_BACKENDS=rust`).
//!
//! Le journal tiré du Go (`internal/loki/parity_backends_test.go` →
//! `tests/parity_backends/backends.json`) est rejoué par une seule façade :
//! statut (dépôt git, précompilé, moteur téléchargé ou de l'image, plan de
//! build et conseil), vérification git, backends custom et désinstallation,
//! bascule fast/opt, dernière release, job des binaires précompilés
//! (extraction avec liens, ménage, VERSION, échecs), GPU vus par un moteur ;
//! puis le plan et la release machine par machine (faux outils dans le PATH),
//! le job pas à pas et sa reprise au démarrage. Le reste — l'API interne par
//! laquelle les jobs du Go rapportent leurs étapes, le service arrêté puis
//! relancé autour de l'installation — est vérifié contre le faux Go.

mod common;

use std::collections::BTreeMap;
use std::convert::Infallible;
use std::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use bytes::Bytes;
use common::fakego;
use http_body_util::Full;
use hyper::service::service_fn;
use hyper::{Request, Response};
use hyper_util::rt::TokioIo;
use loki_rs::backends::job::Jobs;
use loki_rs::backends::probe::Probe;
use loki_rs::backends::{Backends, prebuilt};
use regex::Regex;
use serde_json::{Value, json};
use tokio::net::TcpListener;

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_backends/backends.json")).expect("journal")
}

fn s(v: &Value) -> &str {
    v.as_str().unwrap_or("")
}

const TUNE_BUSY: &str = "optimisation en cours (loki tune) : le moteur est réservé aux essais — attends la fin ou annule-la";

/// Le faux GitHub : la release courante (`{status, json, assets}`).
async fn fake_github(rel: Arc<Mutex<Value>>) -> String {
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    tokio::spawn(async move {
        while let Ok((st, _)) = l.accept().await {
            let rel = rel.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<hyper::body::Incoming>| {
                    let cur = rel.lock().unwrap().clone();
                    async move {
                        let host = req
                            .headers()
                            .get("host")
                            .and_then(|h| h.to_str().ok())
                            .unwrap_or("")
                            .to_string();
                        let path = req.uri().path().to_string();
                        let (code, body) = if path == "/latest" {
                            (
                                cur["status"].as_u64().unwrap_or(200) as u16,
                                s(&cur["json"])
                                    .replace("{{GH}}", &format!("http://{host}"))
                                    .into_bytes(),
                            )
                        } else {
                            let name = path.trim_start_matches("/dl/");
                            match cur["assets"].get(name).and_then(Value::as_array) {
                                Some(entries) => (200, tar_gz(entries)),
                                None => (404, b"404 page not found\n".to_vec()),
                            }
                        };
                        let mut r = Response::new(Full::new(Bytes::from(body)));
                        *r.status_mut() = hyper::StatusCode::from_u16(code).unwrap();
                        Ok::<_, Infallible>(r)
                    }
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(st), svc)
                    .await;
            });
        }
    });
    format!("http://{addr}")
}

/// Une archive .tar.gz ; les noms sont écrits tels quels (« ../x » compris).
fn tar_gz(entries: &[Value]) -> Vec<u8> {
    let gz = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
    let mut b = tar::Builder::new(gz);
    for e in entries {
        let mut h = tar::Header::new_ustar();
        let name = s(&e["name"]).as_bytes();
        h.as_old_mut().name[..name.len()].copy_from_slice(name);
        let link = s(&e["link"]).as_bytes();
        h.as_old_mut().linkname[..link.len()].copy_from_slice(link);
        h.set_mtime(1_700_000_000);
        h.set_mode(e["mode"].as_u64().unwrap_or(0) as u32);
        let body = s(&e["body"]).as_bytes().to_vec();
        match s(&e["type"]) {
            "dir" => h.set_entry_type(tar::EntryType::Directory),
            "symlink" => {
                h.set_entry_type(tar::EntryType::Symlink);
                h.set_mode(0o777);
            }
            "hardlink" => h.set_entry_type(tar::EntryType::Link),
            _ => {
                h.set_entry_type(tar::EntryType::Regular);
                h.set_size(body.len() as u64);
            }
        }
        if s(&e["type"]) != "file" {
            h.set_size(0);
        }
        h.set_cksum();
        let data: &[u8] = if s(&e["type"]) == "file" { &body } else { &[] };
        b.append(&h, data).unwrap();
    }
    b.into_inner().unwrap().finish().unwrap()
}

fn write_file(p: &Path, content: &str, mode: u32) {
    use std::os::unix::fs::OpenOptionsExt;
    let _ = std::fs::remove_file(p);
    let mut f = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(mode)
        .open(p)
        .unwrap();
    std::io::Write::write_all(&mut f, content.as_bytes()).unwrap();
}

fn remove_all(p: &Path) {
    match std::fs::symlink_metadata(p) {
        Ok(m) if m.is_dir() => std::fs::remove_dir_all(p).unwrap(),
        Ok(_) => std::fs::remove_file(p).unwrap(),
        Err(_) => {}
    }
}

/// Les fichiers d'une étape (`parityApplyFiles`).
fn apply_files(home: &Path, files: &Value) {
    for (n, v) in files.as_object().into_iter().flatten() {
        let v = s(v);
        let p = home.join(n.trim_end_matches('/'));
        match v {
            "-" => {
                remove_all(&p);
                continue;
            }
            "/" => {
                std::fs::create_dir_all(&p).unwrap();
                continue;
            }
            _ => {}
        }
        std::fs::create_dir_all(p.parent().unwrap()).unwrap();
        if let Some(target) = v.strip_prefix("-> ") {
            let _ = std::fs::remove_file(&p);
            std::os::unix::fs::symlink(target, &p).unwrap();
            continue;
        }
        write_file(&p, v, if v.starts_with("#!") { 0o755 } else { 0o644 });
    }
}

/// Les opérations git d'une étape (`parityGit`).
fn git_op(home: &Path, op: &str) {
    let f: Vec<&str> = op.splitn(4, ' ').collect();
    let git = |dir: &Path, date: &str, args: &[&str]| {
        let out = std::process::Command::new("git")
            .args(args)
            .current_dir(dir)
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .env("GIT_AUTHOR_NAME", "Loki")
            .env("GIT_AUTHOR_EMAIL", "loki@exemple.fr")
            .env("GIT_COMMITTER_NAME", "Loki")
            .env("GIT_COMMITTER_EMAIL", "loki@exemple.fr")
            .env("GIT_AUTHOR_DATE", date)
            .env("GIT_COMMITTER_DATE", date)
            .output()
            .unwrap();
        assert!(out.status.success(), "git {args:?} : {out:?}");
    };
    match f[0] {
        "init" => {
            let d = home.join(f[1]);
            std::fs::create_dir_all(&d).unwrap();
            git(&d, "", &["init", "-q", "-b", "master"]);
        }
        "commit" => {
            let d = home.join(f[1]);
            std::fs::write(d.join("README"), f[3]).unwrap();
            git(&d, f[2], &["add", "README"]);
            git(&d, f[2], &["commit", "-q", "-m", f[3]]);
        }
        "clone" => {
            let (src, dst) = (home.join(f[1]), home.join(f[2]));
            git(
                home,
                "",
                &[
                    "clone",
                    "-q",
                    &src.to_string_lossy(),
                    &dst.to_string_lossy(),
                ],
            );
        }
        "pull" => git(&home.join(f[1]), "", &["pull", "-q", "--ff-only"]),
        "detach" => git(&home.join(f[1]), "", &["checkout", "-q", "--detach"]),
        "remote-bad" => {
            let bad = home.join("nulle-part");
            git(
                &home.join(f[1]),
                "",
                &["remote", "set-url", "origin", &bad.to_string_lossy()],
            );
        }
        other => panic!("opération git inconnue : {other}"),
    }
}

/// Les outils d'une machine dans le PATH des sondes (git compris).
fn set_machine(tools: &Path, machines: &Value, name: &str) {
    for e in std::fs::read_dir(tools).unwrap().flatten() {
        std::fs::remove_file(e.path()).unwrap();
    }
    let git = loki_rs::paths::look_path("git").expect("git");
    std::os::unix::fs::symlink(git, tools.join("git")).unwrap();
    for (tool, script) in machines[name].as_object().into_iter().flatten() {
        write_file(&tools.join(tool), s(script), 0o755);
    }
}

/// `info.Mode().Perm().String()`.
fn perm_string(mode: u32) -> String {
    let mut out = String::from("-");
    for shift in [6, 3, 0] {
        let b = (mode >> shift) & 7;
        out.push(if b & 4 != 0 { 'r' } else { '-' });
        out.push(if b & 2 != 0 { 'w' } else { '-' });
        out.push(if b & 1 != 0 { 'x' } else { '-' });
    }
    out
}

/// `parityTree`.
fn tree(home: &Path) -> Vec<String> {
    use std::os::unix::fs::PermissionsExt;
    fn walk(home: &Path, p: &Path, out: &mut Vec<String>) {
        let Ok(m) = std::fs::symlink_metadata(p) else {
            return;
        };
        let rel = p.strip_prefix(home).unwrap().to_string_lossy().into_owned();
        if rel.contains("/.git/") || rel.ends_with("/.git") {
            return;
        }
        if m.file_type().is_symlink() {
            let t = std::fs::read_link(p).unwrap();
            out.push(format!("l {rel} -> {}", t.to_string_lossy()));
        } else if m.is_dir() {
            out.push(format!("d {rel}"));
            for e in std::fs::read_dir(p).unwrap().flatten() {
                walk(home, &e.path(), out);
            }
        } else {
            let b = std::fs::read(p).unwrap();
            out.push(format!(
                "f {rel} {} {}",
                perm_string(m.permissions().mode() & 0o777),
                String::from_utf8_lossy(&b)
            ));
        }
    }
    let mut out = Vec::new();
    for root in ["backends", "engine"] {
        walk(home, &home.join(root), &mut out);
    }
    out.sort();
    out
}

struct Bench {
    facade: SocketAddr,
    go: Arc<fakego::FakeGo>,
    _stop: tokio::sync::oneshot::Sender<()>,
    home: PathBuf,
    tools: PathBuf,
    b: Arc<Backends>,
    rel: Arc<Mutex<Value>>,
    gh: String,
    _tmp: Vec<tempfile::TempDir>,
}

async fn bench() -> Bench {
    let tmp = tempfile::tempdir().unwrap();
    let tools = tempfile::tempdir().unwrap();
    let root = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    let rel = Arc::new(Mutex::new(json!({ "status": 200, "json": "{}" })));
    let gh = fake_github(rel.clone()).await;
    let b = Arc::new(Backends::with(
        home.clone(),
        String::new(),
        Probe {
            path: Some(vec![tools.path().to_path_buf()]),
            root: root.path().to_path_buf(),
        },
        format!("{gh}/latest"),
    ));
    let (facade, go, stop, _) = fakego::start_with(
        home.clone(),
        fakego::Opts {
            backends: Some(b.clone()),
            ..Default::default()
        },
    )
    .await;
    Bench {
        facade,
        go,
        _stop: stop,
        home,
        tools: tools.path().to_path_buf(),
        b,
        rel,
        gh,
        _tmp: vec![tmp, tools, root],
    }
}

impl Bench {
    fn mask(&self, v: &str) -> String {
        let ts = Regex::new(r#""(started_at|ended_at)":[1-9][0-9]*"#).unwrap();
        let jobs = Regex::new(r#""jobs":[0-9]+"#).unwrap();
        let v = v
            .replace(&self.gh, "{{GH}}")
            .replace(&self.home.to_string_lossy().into_owned(), "{{HOME}}");
        let v = jobs.replace_all(&v, r#""jobs":"{{JOBS}}""#);
        ts.replace_all(&v, r#""$1":"{{T}}""#).into_owned()
    }

    fn unmask(&self, v: &str) -> String {
        v.replace("{{HOME}}", &self.home.to_string_lossy())
    }

    async fn call(&self, method: &str, path: &str, body: &str) -> (u16, Value) {
        let (code, b) = fakego::send(self.facade, method, path, &[], body).await;
        let out = serde_json::from_str(&self.mask(&String::from_utf8_lossy(&b)))
            .unwrap_or_else(|_| panic!("{path} : corps illisible {b:?}"));
        (code.as_u16(), out)
    }

    async fn wait_job(&self) {
        for _ in 0..1500 {
            let (_, v) = self.call("GET", "/api/llamacpp/job", "").await;
            if v["running"] != json!(true) {
                return;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        panic!("le job ne finit pas");
    }
}

#[tokio::test]
async fn parite_des_routes() {
    let fx = fixture();
    if (fx["goarch"] == "arm64") != cfg!(target_arch = "aarch64") {
        eprintln!("journal tiré sur une autre architecture : rejeu sauté");
        return;
    }
    let bn = bench().await;
    let machines = &fx["machines"];
    set_machine(&bn.tools, machines, "nu");
    for st in fx["steps"].as_array().unwrap() {
        let name = s(&st["name"]);
        if let Some(m) = st["machine"].as_str() {
            set_machine(&bn.tools, machines, m);
        }
        for g in st["git"].as_array().into_iter().flatten() {
            git_op(&bn.home, s(g));
        }
        let files: serde_json::Map<String, Value> = st["files"]
            .as_object()
            .into_iter()
            .flatten()
            .map(|(k, v)| (k.clone(), Value::String(bn.unmask(s(v)))))
            .collect();
        apply_files(&bn.home, &Value::Object(files));
        for (k, v) in st["config"].as_object().into_iter().flatten() {
            let v = bn.unmask(s(v));
            if v.is_empty() {
                bn.go.del("config", k);
            } else {
                bn.go.set("config", k, &v);
            }
        }
        if let Some(e) = st["engine_env"].as_str() {
            bn.b.set_engine_bin_env(&bn.unmask(e));
        }
        if !st["release"].is_null() {
            *bn.rel.lock().unwrap() = st["release"].clone();
        }
        *bn.go.tune_busy.lock().unwrap() =
            (st["tune"].as_bool() == Some(true)).then(|| TUNE_BUSY.to_string());
        let (code, out) = bn
            .call(s(&st["method"]), s(&st["path"]), &bn.unmask(s(&st["body"])))
            .await;
        *bn.go.tune_busy.lock().unwrap() = None;
        assert_eq!(code as i64, st["status"].as_i64().unwrap(), "{name}");
        assert_eq!(out, st["out"], "{name}");
        if st["wait"].as_bool() == Some(true) {
            bn.wait_job().await;
            let (_, job) = bn.call("GET", "/api/llamacpp/job?from=0", "").await;
            assert_eq!(job, st["job"], "{name} : job");
        }
        let mut after = BTreeMap::new();
        if let Some(v) = bn.go.get("config", "BIN").filter(|v| !v.is_empty()) {
            after.insert("config/BIN".to_string(), Value::String(bn.mask(&v)));
        }
        if let Some(v) = bn.go.get("state", "build_job").filter(|v| !v.is_empty()) {
            after.insert("state/build_job".to_string(), Value::String(bn.mask(&v)));
        }
        for f in ["loki-build.log", "devices.json"] {
            if let Ok(b) = std::fs::read(bn.home.join(f)) {
                after.insert(
                    f.to_string(),
                    Value::String(bn.mask(&String::from_utf8_lossy(&b))),
                );
            }
        }
        assert_eq!(
            Value::Object(after.into_iter().collect()),
            st["after"],
            "{name} : après"
        );
        let t: Vec<Value> = tree(&bn.home)
            .into_iter()
            .map(|l| Value::String(bn.mask(&l)))
            .collect();
        // Une liste vide est nil côté Go : null.
        let t = if t.is_empty() {
            Value::Null
        } else {
            Value::Array(t)
        };
        assert_eq!(t, st["tree"], "{name} : arborescence");
    }

    // Plan, conseil et release, machine par machine.
    *bn.rel.lock().unwrap() = fx["plan_release"].clone();
    for p in fx["plans"].as_array().unwrap() {
        let m = s(&p["machine"]);
        set_machine(&bn.tools, machines, m);
        let (_, out) = bn.call("GET", "/api/llamacpp", "").await;
        assert_eq!(out["plan"], p["plan"], "{m} : plan");
        assert_eq!(out["reco"], p["reco"], "{m} : conseil");
        let (_, check) = bn.call("POST", "/api/llamacpp/prebuilt/check", "").await;
        assert_eq!(check, p["check"], "{m} : release");
    }
    for (machine, key) in [
        ("vulkaninfo-seul", "sans_vulkan"),
        ("rocm", "rocm_sans_asset"),
    ] {
        set_machine(&bn.tools, machines, machine);
        *bn.rel.lock().unwrap() = fx[key]["release"].clone();
        let (_, check) = bn.call("POST", "/api/llamacpp/prebuilt/check", "").await;
        assert_eq!(check, fx[key]["out"], "{key}");
    }
}

/// `norm` du journal : les instants masqués.
fn norm(v: &Value) -> Value {
    let ts = Regex::new(r#""(started_at|ended_at)":[1-9][0-9]*"#).unwrap();
    serde_json::from_str(&ts.replace_all(&v.to_string(), r#""$1":"{{T}}""#)).unwrap()
}

#[tokio::test]
async fn parite_du_job_pas_a_pas() {
    let fx = fixture();
    let bn = bench().await;
    let backend = bn.go.backend.lock().unwrap().unwrap();
    let store = loki_rs::store::Store::new(backend, fakego::JETON.into());
    let ts = Regex::new(r#""(started_at|ended_at)":[1-9][0-9]*"#).unwrap();
    let state = || {
        ts.replace_all(
            &bn.go.get("state", "build_job").unwrap_or_default(),
            r#""$1":"{{T}}""#,
        )
        .into_owned()
    };
    let jobs = Jobs::new(bn.home.clone());
    let job = &fx["job"];
    for o in job["ops"].as_array().unwrap() {
        let (op, arg) = (s(&o["op"]), s(&o["arg"]));
        let from = o["from"].as_i64().unwrap_or(0);
        let got = match op {
            "start" => Some(Value::String(match jobs.start(&store, arg).await {
                Ok(()) => "ok".into(),
                Err(e) => e,
            })),
            "line" => {
                jobs.append(&store, arg).await;
                None
            }
            "lines" => {
                for _ in 0..4002 {
                    jobs.append(&store, &format!("[{arg}] bruit")).await;
                }
                None
            }
            "phase" => {
                jobs.phase(&store, arg).await;
                None
            }
            "fail" => {
                jobs.fail(&store, arg).await;
                None
            }
            "done" => {
                jobs.done(&store, arg).await;
                None
            }
            "old" => {
                jobs.set_commits(Some(arg), None).await;
                None
            }
            "new" => {
                jobs.set_commits(None, Some(arg)).await;
                None
            }
            "end" => {
                jobs.end(&store).await;
                None
            }
            "snapshot" => Some(norm(&jobs.snapshot(from, true).await)),
            "summary" => {
                let mut v = jobs.snapshot(from, true).await;
                let lines = v["lines"].as_array().unwrap().clone();
                v["lines"] = json!({
                    "n": lines.len(),
                    "first": lines[0],
                    "last": lines[lines.len() - 1],
                });
                Some(norm(&v))
            }
            "head" => Some(norm(&jobs.snapshot(0, false).await)),
            "state" => Some(Value::String(state())),
            other => panic!("op inconnue {other}"),
        };
        assert_eq!(got.unwrap_or(Value::Null), o["out"], "{o}");
    }
    let log = std::fs::read_to_string(jobs.log_path()).unwrap();
    assert_eq!(
        log.matches('\n').count() as i64,
        job["log_lines"].as_i64().unwrap()
    );

    for rc in job["restores"].as_array().unwrap() {
        let st = s(&rc["state"]);
        if st.is_empty() {
            bn.go.del("state", "build_job");
        } else {
            bn.go.set("state", "build_job", st);
        }
        let jobs = Jobs::new(bn.home.clone());
        let _ = std::fs::remove_file(jobs.log_path());
        if let Some(l) = rc["log"].as_str() {
            std::fs::write(jobs.log_path(), l).unwrap();
        }
        jobs.restore(&store).await;
        assert_eq!(norm(&jobs.snapshot(0, true).await), rc["out"], "{rc}");
        assert_eq!(state(), s(&rc["after"]), "{rc} : base");
    }
    let jobs = Jobs::new(bn.home.clone());
    bn.go.set(
        "state",
        "build_job",
        r#"{"action":"update","running":false}"#,
    );
    std::fs::write(jobs.log_path(), "ligne\n".repeat(4003)).unwrap();
    jobs.restore(&store).await;
    assert_eq!(norm(&jobs.snapshot(4000, true).await), job["long_restore"]);
}

/// Les étapes des jobs du Go, par l'API interne (jeton du Go seulement).
#[tokio::test]
async fn api_interne_du_job() {
    let bn = bench().await;
    let tok = [("X-Loki-Internal", fakego::JETON)];
    let post = |op: &'static str, body: &'static str| {
        let f = bn.facade;
        async move {
            let (c, b) = fakego::send(
                f,
                "POST",
                &format!("/internal/facade/v1/build-job/{op}"),
                &tok,
                body,
            )
            .await;
            (c.as_u16(), String::from_utf8_lossy(&b).into_owned())
        }
    };
    // Sans le jeton : l'API interne n'existe pas.
    let (c, _) = fakego::send(
        bn.facade,
        "POST",
        "/internal/facade/v1/build-job/start",
        &[],
        r#"{"action":"engine"}"#,
    )
    .await;
    assert_eq!(c.as_u16(), 404);
    assert_eq!(post("start", r#"{"action":"engine"}"#).await.0, 200);
    let (c, b) = post("start", r#"{"action":"strata"}"#).await;
    assert_eq!(
        (c, b.contains("un job engine est déjà en cours")),
        (409, true)
    );
    // La route publique refuse aussi un second job (précompilé).
    let (c, out) = bn.call("POST", "/api/llamacpp/prebuilt", "").await;
    assert_eq!(c, 409);
    assert_eq!(out["error"], "un job engine est déjà en cours");
    post("commits", r#"{"old":"aaa"}"#).await;
    post(
        "phase",
        r#"{"phase":"recherche de la dernière version publiée…"}"#,
    )
    .await;
    post(
        "line",
        r#"{"line":"[ 50%] Building CXX object a/b/c.cpp.o"}"#,
    )
    .await;
    post("commits", r#"{"new":"bbb"}"#).await;
    post("fail", r#"{"error":"raté"}"#).await;
    post("done", r#"{"msg":"rattrapé"}"#).await;
    let (_, v) = bn.call("GET", "/api/llamacpp/job?from=1", "").await;
    assert_eq!(v["running"], true);
    assert_eq!(v["compiled"], 1);
    assert_eq!(v["old"], "aaa");
    assert_eq!(v["new"], "bbb");
    assert_eq!(
        v["lines"],
        json!([
            "[ 50%] Building CXX object a/b/c.cpp.o",
            "✗ raté",
            "✓ rattrapé"
        ])
    );
    // La vue que le Go relaie (statut du moteur de l'image).
    let (c, b) = fakego::send(
        bn.facade,
        "GET",
        "/internal/facade/v1/build-job/snapshot?from=0&lines=0",
        &tok,
        "",
    )
    .await;
    assert_eq!(c.as_u16(), 200);
    let head: Value = serde_json::from_slice(&b).unwrap();
    assert_eq!(head["phase"], "rattrapé");
    assert!(head.get("lines").is_none());
    assert_eq!(post("end", "{}").await.0, 200);
    assert_eq!(
        bn.call("GET", "/api/llamacpp", "").await.1["job"]["running"],
        false
    );
    // Méthode et opération.
    let (c, _) = fakego::send(
        bn.facade,
        "GET",
        "/internal/facade/v1/build-job/line",
        &tok,
        "",
    )
    .await;
    assert_eq!(c.as_u16(), 405);
    assert_eq!(post("autre", "{}").await.0, 404);
    // L'entête suit en base.
    let st: Value = serde_json::from_str(&bn.go.get("state", "build_job").unwrap()).unwrap();
    assert_eq!(st["action"], "engine");
    assert_eq!(st["running"], false);
    assert_eq!(st["error"], "raté");
}

/// Le moteur tourne : arrêté pendant l'installation, relancé après ; la
/// bascule le relance.
#[tokio::test]
async fn service_arrete_puis_relance() {
    let fx = fixture();
    let bn = bench().await;
    set_machine(&bn.tools, &fx["machines"], "nu");
    *bn.go.service_active.lock().unwrap() = true;
    let ok = fx["steps"]
        .as_array()
        .unwrap()
        .iter()
        .find(|s| s["name"] == "installation")
        .unwrap();
    *bn.rel.lock().unwrap() = ok["release"].clone();
    assert_eq!(bn.call("POST", "/api/llamacpp/prebuilt", "").await.0, 200);
    bn.wait_job().await;
    let (_, job) = bn.call("GET", "/api/llamacpp/job", "").await;
    let lines: Vec<&str> = job["lines"]
        .as_array()
        .unwrap()
        .iter()
        .map(|v| v.as_str().unwrap())
        .collect();
    assert_eq!(lines[0], "▶ arrêt du service le temps de l'installation…");
    assert_eq!(
        &lines[lines.len() - 3..],
        &[
            "BIN mis à jour",
            "▶ redémarrage du service…",
            "✓ binaires précompilés installés (b300)"
        ]
    );
    assert_eq!(
        *bn.go.service_actions.lock().unwrap(),
        vec![("stop".to_string(), true), ("start".to_string(), false)]
    );
    bn.go.service_actions.lock().unwrap().clear();
    let (c, out) = bn
        .call("POST", "/api/llamacpp/use", r#"{"mode":"fast"}"#)
        .await;
    assert_eq!((c, out["ok"].clone()), (200, json!(true)));
    assert_eq!(
        *bn.go.service_actions.lock().unwrap(),
        vec![("restart".to_string(), true)]
    );
    // Arrêté : rien n'est relancé.
    *bn.go.service_active.lock().unwrap() = false;
    bn.go.service_actions.lock().unwrap().clear();
    assert_eq!(bn.call("POST", "/api/llamacpp/prebuilt", "").await.0, 200);
    bn.wait_job().await;
    assert_eq!(
        *bn.go.service_actions.lock().unwrap(),
        vec![("stop".to_string(), true)]
    );
}

/// Une archive .zip (Windows) s'extrait aussi, sans évasion possible.
#[test]
fn extraction_zip() {
    let tmp = tempfile::tempdir().unwrap();
    let zpath = tmp.path().join("a.zip");
    {
        let f = std::fs::File::create(&zpath).unwrap();
        let mut z = zip::ZipWriter::new(f);
        let o = zip::write::SimpleFileOptions::default();
        z.add_directory("llama-b1/", o).unwrap();
        z.start_file("llama-b1/llama-server.exe", o).unwrap();
        std::io::Write::write_all(&mut z, b"exe").unwrap();
        z.finish().unwrap();
    }
    let out = tmp.path().join("out");
    prebuilt::extract(&zpath, &out).unwrap();
    assert_eq!(
        std::fs::read(out.join("llama-b1/llama-server.exe")).unwrap(),
        b"exe"
    );
    let bad = tmp.path().join("b.zip");
    {
        let f = std::fs::File::create(&bad).unwrap();
        let mut z = zip::ZipWriter::new(f);
        z.start_file("../evasion", zip::write::SimpleFileOptions::default())
            .unwrap();
        z.finish().unwrap();
    }
    assert_eq!(
        prebuilt::extract(&bad, &out).unwrap_err(),
        "entrée d'archive suspecte : ../evasion"
    );
}
