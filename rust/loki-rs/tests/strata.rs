//! Lot 25 : Strata tenu par la façade (`LOKI_STRATA=rust`, avec
//! `LOKI_BACKENDS=rust`).
//!
//! Le journal tiré du Go (`internal/loki/parity_strata_test.go` →
//! `tests/parity_strata/strata.json`) est rejoué par une façade : fausses
//! machines dans le PATH des sondes (nvidia-smi, python3), faux /proc/meminfo,
//! place libre imposée, fausse release (sources, moteur, empreintes) et
//! installeur factice ; la bascule finale part au faux Go, qui applique le
//! preset comme `applyPresetFile`.

mod common;

use std::collections::BTreeMap;
use std::convert::Infallible;
use std::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use bytes::Bytes;
use common::fakego;
use common::parity::{apply_files, tar_gz, write_file};
use http_body_util::Full;
use hyper::service::service_fn;
use hyper::{Request, Response};
use hyper_util::rt::TokioIo;
use loki_rs::backends::Backends;
use loki_rs::backends::probe::Probe;
use loki_rs::strata::api::Strata;
use regex::Regex;
use serde_json::{Value, json};
use sha2::Digest;
use tokio::net::TcpListener;

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_strata/strata.json")).expect("journal")
}

fn s(v: &Value) -> &str {
    v.as_str().unwrap_or("")
}

const TUNE_BUSY: &str = "optimisation en cours (loki tune) : le moteur est réservé aux essais — attends la fin ou annule-la";

/// La fausse release : `{status, src, engine}`.
async fn fake_release(rel: Arc<Mutex<Value>>) -> String {
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    tokio::spawn(async move {
        while let Ok((st, _)) = l.accept().await {
            let rel = rel.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<hyper::body::Incoming>| {
                    let cur = rel.lock().unwrap().clone();
                    async move {
                        let path = req.uri().path().trim_start_matches('/').to_string();
                        let (code, body) = match cur["status"].as_u64() {
                            Some(c) if c != 0 => (c as u16, Vec::new()),
                            _ if path == loki_rs::strata::SRC_ASSET => (200, src_bytes(&cur)),
                            _ if path == loki_rs::strata::ENGINE_ASSET => {
                                (200, s(&cur["engine"]).as_bytes().to_vec())
                            }
                            _ => (404, Vec::new()),
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

fn src_bytes(rel: &Value) -> Vec<u8> {
    tar_gz(rel["src"].as_array().map_or(&[][..], Vec::as_slice))
}

fn sha(b: &[u8]) -> String {
    hex::encode(sha2::Sha256::digest(b))
}

struct Bench {
    facade: SocketAddr,
    go: Arc<fakego::FakeGo>,
    _stop: tokio::sync::oneshot::Sender<()>,
    home: PathBuf,
    tools: PathBuf,
    root: PathBuf,
    strata: Arc<Strata>,
    rel: Arc<Mutex<Value>>,
    rel_base: String,
    _tmp: Vec<tempfile::TempDir>,
}

async fn bench() -> Bench {
    let tmp = tempfile::tempdir().unwrap();
    let tools = tempfile::tempdir().unwrap();
    let root = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    let probe = || Probe {
        path: Some(vec![tools.path().to_path_buf()]),
        root: root.path().to_path_buf(),
    };
    std::fs::create_dir_all(root.path().join("proc")).unwrap();
    let rel = Arc::new(Mutex::new(json!({})));
    let rel_base = fake_release(rel.clone()).await;
    let b = Arc::new(Backends::with(
        home.clone(),
        String::new(),
        probe(),
        String::new(),
    ));
    let mut st = Strata::new(home.clone(), probe());
    st.release_base = format!("{rel_base}/");
    st.platform_ok = true;
    let strata = Arc::new(st);
    let (facade, go, stop, _) = fakego::start_with(
        home.clone(),
        fakego::Opts {
            backends: Some(b),
            strata: Some(strata.clone()),
            ..Default::default()
        },
    )
    .await;
    *go.apply_presets.lock().unwrap() = true;
    Bench {
        facade,
        go,
        _stop: stop,
        home,
        tools: tools.path().to_path_buf(),
        root: root.path().to_path_buf(),
        strata,
        rel,
        rel_base,
        _tmp: vec![tmp, tools, root],
    }
}

impl Bench {
    fn mask(&self, v: &str) -> String {
        let ts = Regex::new(r#""(started_at|ended_at)":[1-9][0-9]*"#).unwrap();
        let v = v
            .replace(&self.rel_base, "{{REL}}")
            .replace(&self.home.to_string_lossy().into_owned(), "{{HOME}}");
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

    /// Les outils d'une machine (bash, mkdir et tr toujours).
    fn set_machine(&self, machines: &Value, name: &str) {
        for e in std::fs::read_dir(&self.tools).unwrap().flatten() {
            std::fs::remove_file(e.path()).unwrap();
        }
        for tool in ["bash", "mkdir", "tr"] {
            let p = loki_rs::paths::look_path(tool).expect(tool);
            std::os::unix::fs::symlink(p, self.tools.join(tool)).unwrap();
        }
        for (tool, script) in machines[name].as_object().into_iter().flatten() {
            write_file(&self.tools.join(tool), s(script), 0o755);
        }
    }

    fn set_release(&self, r: &Value) {
        *self.rel.lock().unwrap() = r.clone();
        let mut want = std::collections::HashMap::from([
            (loki_rs::strata::SRC_ASSET.to_string(), sha(&src_bytes(r))),
            (
                loki_rs::strata::ENGINE_ASSET.to_string(),
                sha(s(&r["engine"]).as_bytes()),
            ),
        ]);
        if r["bad_sha"] == json!(true) {
            want.insert(loki_rs::strata::ENGINE_ASSET.to_string(), "0".repeat(64));
        }
        *self.strata.asset_sha.lock().unwrap() = want;
    }

    /// presets/ et strata/, sans le contenu des archives.
    fn tree(&self) -> Vec<String> {
        use std::os::unix::fs::PermissionsExt;
        fn walk(b: &Bench, p: &Path, out: &mut Vec<String>) {
            let Ok(m) = std::fs::symlink_metadata(p) else {
                return;
            };
            let rel = p
                .strip_prefix(&b.home)
                .unwrap()
                .to_string_lossy()
                .into_owned();
            if m.is_dir() {
                out.push(format!("d {rel}"));
                for e in std::fs::read_dir(p).unwrap().flatten() {
                    walk(b, &e.path(), out);
                }
            } else if rel.starts_with("strata/dl/") || rel.starts_with("strata/prebuilt/") {
                out.push(format!("f {rel}"));
            } else {
                let c = std::fs::read(p).unwrap();
                out.push(format!(
                    "f {rel} {} {}",
                    common::parity::perm_string(m.permissions().mode() & 0o777),
                    b.mask(&String::from_utf8_lossy(&c))
                ));
            }
        }
        let mut out = Vec::new();
        for root in ["presets", "strata"] {
            walk(self, &self.home.join(root), &mut out);
        }
        out.sort();
        out
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn parite_de_strata() {
    if !cfg!(target_arch = "x86_64") {
        eprintln!("Strata n'est proposé que sous Linux x86_64 : rejeu sauté");
        return;
    }
    let fx = fixture();
    let bn = bench().await;
    let machines = &fx["machines"];
    for st in fx["steps"].as_array().unwrap() {
        let name = s(&st["name"]);
        if let Some(m) = st["machine"].as_str() {
            bn.set_machine(machines, m);
        }
        if let Some(m) = st["meminfo"].as_str() {
            std::fs::write(bn.root.join("proc/meminfo"), m).unwrap();
        }
        if let Some(d) = st["disk_free"].as_i64() {
            *bn.strata.disk_free.lock().unwrap() = Some(d);
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
        for (k, v) in st["state"].as_object().into_iter().flatten() {
            if s(v).is_empty() {
                bn.go.del("state", k);
            } else {
                bn.go.set("state", k, s(v));
            }
        }
        if !st["release"].is_null() {
            bn.set_release(&st["release"]);
        }
        bn.go.preset_applies.lock().unwrap().clear();
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
        // Une bascule faite par le Go : application du preset puis
        // redémarrage du service.
        let actions: Vec<Value> = bn
            .go
            .preset_applies
            .lock()
            .unwrap()
            .iter()
            .filter(|(_, mode)| mode == "switch")
            .map(|_| json!("restart"))
            .collect();
        assert_eq!(Value::Array(actions), st["actions"], "{name} : service");
        let mut after = BTreeMap::new();
        for k in [
            "ENGINE",
            "STRATA_CONFIG",
            "CTX",
            "STRATA_KV",
            "STRATA_HELPER",
            "STRATA_SPLIT",
        ] {
            if let Some(v) = bn.go.get("config", k).filter(|v| !v.is_empty()) {
                after.insert(format!("config/{k}"), Value::String(bn.mask(&v)));
            }
        }
        assert_eq!(
            Value::Object(after.into_iter().collect()),
            st["after"],
            "{name} : après"
        );
        let t: Vec<Value> = bn.tree().into_iter().map(Value::String).collect();
        let t = if t.is_empty() {
            Value::Null
        } else {
            Value::Array(t)
        };
        assert_eq!(t, st["tree"], "{name} : arborescence");
    }
}

/// Sans les backends dans la façade (pas de job), Strata répond 503.
#[tokio::test]
async fn sans_backends() {
    let tmp = tempfile::tempdir().unwrap();
    let st = Strata::new(tmp.path().to_path_buf(), Probe::system());
    let (facade, _go, _stop, _) = fakego::start_with(
        tmp.path().to_path_buf(),
        fakego::Opts {
            strata: Some(Arc::new(st)),
            ..Default::default()
        },
    )
    .await;
    let (c, b) = fakego::send(facade, "GET", "/api/strata", &[], "").await;
    assert_eq!(c.as_u16(), 503, "{b:?}");
}
