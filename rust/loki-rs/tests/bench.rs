//! Lot 27 : banc d'essai tenu par la façade (`LOKI_BENCH=rust`).
//!
//! Le journal tiré du Go (`internal/loki/parity_bench_test.go` →
//! `tests/parity_bench/bench.json`) est rejoué par une façade contre le même
//! faux moteur (port de `fakeBenchEngine`) : chaque requête qu'il reçoit est
//! comparée, longs textes réduits à leur taille et à leur empreinte.

mod common;

use std::convert::Infallible;
use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use bytes::Bytes;
use common::fakego;
use common::parity::apply_files;
use http_body_util::{BodyExt, Full};
use hyper::service::service_fn;
use hyper::{Request, Response};
use hyper_util::rt::TokioIo;
use loki_rs::bench::job::Bench;
use regex::Regex;
use serde_json::{Map, Value, json};
use sha2::Digest;
use tokio::net::TcpListener;

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_bench/bench.json")).expect("journal")
}

fn s(v: &Value) -> &str {
    v.as_str().unwrap_or("")
}

const TUNE_BUSY: &str = "optimisation en cours (loki tune) : le moteur est réservé aux essais — attends la fin ou annule-la";

/// `parityBenchDigest`.
fn digest(v: &Value) -> Value {
    match v {
        Value::Object(o) => Value::Object(o.iter().map(|(k, e)| (k.clone(), digest(e))).collect()),
        Value::Array(a) => Value::Array(a.iter().map(digest).collect()),
        Value::String(x) if x.len() > 64 => {
            let sum = sha2::Sha256::digest(x.as_bytes());
            Value::String(format!("<{} octets {}>", x.len(), hex::encode(&sum[..6])))
        }
        _ => v.clone(),
    }
}

/// Le faux moteur (`fakeBenchEngine`) et le journal des requêtes reçues.
#[derive(Default)]
struct Fake {
    n_ctx: i64,
    busy: bool,
    fail_at: usize,
    no_timing: bool,
    reject_effort: String,
    slow_hot: Duration,
    prev: i64,
    chats: usize,
    log: Vec<Value>,
    /// En-têtes Authorization reçus.
    auth: Vec<String>,
}

fn reply(code: u16, body: String) -> Response<Full<Bytes>> {
    let mut r = Response::new(Full::new(Bytes::from(body)));
    *r.status_mut() = hyper::StatusCode::from_u16(code).unwrap();
    r
}

async fn fake_engine(f: Arc<Mutex<Fake>>) -> u16 {
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = l.local_addr().unwrap().port();
    tokio::spawn(async move {
        while let Ok((st, _)) = l.accept().await {
            let f = f.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<hyper::body::Incoming>| {
                    let f = f.clone();
                    async move { Ok::<_, Infallible>(handle(f, req).await) }
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(st), svc)
                    .await;
            });
        }
    });
    port
}

async fn handle(f: Arc<Mutex<Fake>>, req: Request<hyper::body::Incoming>) -> Response<Full<Bytes>> {
    let method = req.method().to_string();
    let path = req.uri().path().to_string();
    if let Some(a) = req.headers().get("authorization") {
        f.lock()
            .unwrap()
            .auth
            .push(String::from_utf8_lossy(a.as_bytes()).into_owned());
    }
    let raw = req.into_body().collect().await.unwrap().to_bytes();
    let body: Option<Value> = serde_json::from_slice(&raw).ok();
    {
        let mut entry = Map::new();
        entry.insert("method".into(), json!(method));
        entry.insert("path".into(), json!(path));
        if let Some(b) = &body {
            entry.insert("body".into(), digest(b));
        }
        f.lock().unwrap().log.push(Value::Object(entry));
    }
    match path.as_str() {
        "/health" => reply(200, r#"{"status":"ok"}"#.into()),
        "/slots" => {
            let busy = f.lock().unwrap().busy;
            reply(200, format!(r#"[{{"id":0,"is_processing":{busy}}}]"#))
        }
        "/props" => {
            let n = f.lock().unwrap().n_ctx;
            reply(
                200,
                format!(
                    r#"{{"build_info":"b9999-test","total_slots":1,"default_generation_settings":{{"n_ctx":{n}}}}}"#
                ),
            )
        }
        "/tokenize" => {
            let n = body
                .as_ref()
                .and_then(|b| b["content"].as_str())
                .map_or(0, |c| c.len() / 4);
            reply(200, json!({ "tokens": vec![0; n] }).to_string() + "\n")
        }
        "/v1/chat/completions" => {
            let p = body.unwrap_or(Value::Null);
            let (n, fail_at, reject, slow) = {
                let mut g = f.lock().unwrap();
                g.chats += 1;
                (g.chats, g.fail_at, g.reject_effort.clone(), g.slow_hot)
            };
            if fail_at == n {
                return reply(500, "{\"error\":\"boom\"}\n".into());
            }
            let effort = p["reasoning_effort"].as_str().unwrap_or("");
            if !reject.is_empty() && effort == reject {
                return reply(
                    500,
                    format!(
                        "{{\"error\":{{\"code\":500,\"message\":\"{}Unexpected reasoning effort {effort}. Supported types are xhigh (default), medium, and low.\"}}}}\n",
                        "jinja trace ".repeat(40)
                    ),
                );
            }
            let cache = p["cache_prompt"].as_bool() == Some(true);
            if cache && !slow.is_zero() {
                tokio::time::sleep(slow).await;
            }
            let total: i64 = p["messages"]
                .as_array()
                .into_iter()
                .flatten()
                .map(|m| m["content"].as_str().unwrap_or("").len() as i64 / 4 + 4)
                .sum();
            let (cached, no_timing) = {
                let mut g = f.lock().unwrap();
                let cached = if cache { g.prev.min(total) } else { 0 };
                g.prev = total;
                (cached, g.no_timing)
            };
            let gen_n = p["max_tokens"].as_f64().unwrap_or(0.0) as i64;
            let mut resp = json!({
                "choices": [{ "message": { "role": "assistant", "content": "réponse" } }],
                "usage": { "prompt_tokens": total, "completion_tokens": gen_n },
            });
            if !no_timing {
                resp["timings"] = json!({
                    "prompt_n": total - cached,
                    "prompt_ms": (total - cached) as f64 / 2.0,
                    "prompt_per_second": 2000.0,
                    "predicted_n": gen_n,
                    "predicted_ms": gen_n as f64 * 20.0,
                    "predicted_per_second": 50.0,
                    "cache_n": cached,
                    "draft_n": 10,
                    "draft_n_accepted": 7,
                });
            }
            reply(200, resp.to_string() + "\n")
        }
        _ => reply(404, "404 page not found\n".into()),
    }
}

struct Bn {
    facade: SocketAddr,
    go: Arc<fakego::FakeGo>,
    _stop: tokio::sync::oneshot::Sender<()>,
    home: PathBuf,
    bench: Arc<Bench>,
    fake: Arc<Mutex<Fake>>,
    port: u16,
    closed: u16,
    _tmp: tempfile::TempDir,
}

async fn start() -> Bn {
    start_with(|_| {}).await
}

async fn start_with(tweak: impl FnOnce(&mut Bench)) -> Bn {
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    let fake = Arc::new(Mutex::new(Fake::default()));
    let port = fake_engine(fake.clone()).await;
    let closed = std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port();
    let paths = loki_rs::engine::Paths {
        home: home.clone(),
        service: "loki-engine".into(),
    };
    let mut bench = Bench::new(paths);
    tweak(&mut bench);
    let bench = Arc::new(bench);
    let (facade, go, stop, _) = fakego::start_with(
        home.clone(),
        fakego::Opts {
            bench: Some(bench.clone()),
            ..Default::default()
        },
    )
    .await;
    Bn {
        facade,
        go,
        _stop: stop,
        home,
        bench,
        fake,
        port,
        closed,
        _tmp: tmp,
    }
}

impl Bn {
    fn mask(&self, v: &str) -> String {
        let id = Regex::new(r#""b[0-9]+-[0-9]+""#).unwrap();
        let t = Regex::new(r#""(elapsed_sec|at)":[0-9.e+-]+"#).unwrap();
        let fp = Regex::new(r#""fingerprint":"[0-9a-f]+""#).unwrap();
        let v = v.replace(&self.home.to_string_lossy().into_owned(), "{{HOME}}");
        let v = id.replace_all(&v, r#""{{ID}}""#);
        let v = t.replace_all(&v, r#""$1":"{{T}}""#);
        fp.replace_all(&v, r#""fingerprint":"{{FP}}""#).into_owned()
    }

    fn unmask(&self, v: &str) -> String {
        v.replace("{{PORT}}", &self.port.to_string())
            .replace("{{CLOSED}}", &self.closed.to_string())
            .replace("{{HOME}}", &self.home.to_string_lossy())
    }

    fn decode(&self, raw: &str) -> Value {
        if raw.is_empty() {
            return Value::Null;
        }
        serde_json::from_str(&self.mask(raw)).unwrap_or_else(|_| panic!("illisible : {raw:?}"))
    }

    async fn call(&self, method: &str, path: &str, body: &str) -> (u16, Value) {
        let (code, b) = fakego::send(self.facade, method, path, &[], body).await;
        (code.as_u16(), self.decode(&String::from_utf8_lossy(&b)))
    }

    async fn status(&self) -> Value {
        self.call("GET", "/api/bench/status", "").await.1
    }

    async fn wait(&self, name: &str) {
        for _ in 0..12000 {
            if self.status().await["running"] != json!(true) {
                return;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        panic!("{name} : le bench ne finit pas");
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn parite_du_banc_d_essai() {
    let fx = fixture();
    let bn = start().await;
    for st in fx["steps"].as_array().unwrap() {
        let name = s(&st["name"]);
        for (k, v) in st["config"].as_object().into_iter().flatten() {
            // putStr du Go : une valeur vide retire la clé.
            match bn.unmask(s(v)) {
                v if v.is_empty() => bn.go.del("config", k),
                v => bn.go.set("config", k, &v),
            }
        }
        let files: Map<String, Value> = st["files"]
            .as_object()
            .into_iter()
            .flatten()
            .map(|(k, v)| (k.clone(), Value::String(bn.unmask(s(v)))))
            .collect();
        apply_files(&bn.home, &Value::Object(files));
        if let Some(e) = st["engine"].as_object() {
            let n = |k: &str| e.get(k).and_then(Value::as_i64).unwrap_or(0);
            *bn.fake.lock().unwrap() = Fake {
                n_ctx: n("n_ctx"),
                busy: e.get("busy").and_then(Value::as_bool) == Some(true),
                fail_at: n("fail_at") as usize,
                no_timing: e.get("no_timing").and_then(Value::as_bool) == Some(true),
                reject_effort: e
                    .get("reject_effort")
                    .and_then(Value::as_str)
                    .unwrap_or("")
                    .to_string(),
                slow_hot: Duration::from_millis(n("slow_hot_ms") as u64),
                ..Default::default()
            };
            *bn.bench.req_timeout.lock().unwrap() = match n("timeout_ms") {
                0 => loki_rs::bench::REQ_TIMEOUT,
                ms => Duration::from_millis(ms as u64),
            };
        }
        bn.fake.lock().unwrap().log.clear();
        *bn.go.tune_busy.lock().unwrap() =
            (st["tune"].as_bool() == Some(true)).then(|| TUNE_BUSY.to_string());
        *bn.go.go_busy.lock().unwrap() =
            (st["generating"].as_bool() == Some(true)).then(|| "génération en cours".to_string());
        let (code, out) = bn
            .call(s(&st["method"]), s(&st["path"]), &bn.unmask(s(&st["body"])))
            .await;
        *bn.go.tune_busy.lock().unwrap() = None;
        *bn.go.go_busy.lock().unwrap() = None;
        assert_eq!(code as i64, st["status"].as_i64().unwrap(), "{name}");
        assert_eq!(out, st["out"], "{name}");
        if st["again"].as_bool() == Some(true) {
            let (_, again) = bn.call("POST", "/api/bench", "{}").await;
            assert_eq!(again, st["again_out"], "{name} : second lancement");
        }
        if let Some(phase) = st["cancel_at"].as_str() {
            let mut ok = false;
            for _ in 0..6000 {
                let p = bn.status().await["phase"]
                    .as_str()
                    .unwrap_or("")
                    .to_string();
                if p.starts_with(phase) {
                    ok = true;
                    break;
                }
                tokio::time::sleep(Duration::from_millis(2)).await;
            }
            assert!(ok, "{name} : phase {phase} jamais atteinte");
            while bn.fake.lock().unwrap().chats < 4 {
                tokio::time::sleep(Duration::from_millis(2)).await;
            }
            bn.call("POST", "/api/bench/cancel", "").await;
        }
        if st["wait"].as_bool() == Some(true) || st["cancel_at"].is_string() {
            bn.wait(name).await;
            assert_eq!(bn.status().await, st["job"], "{name} : job");
        }
        // Les requêtes reçues : dans l'ordre, sauf les sondes /health d'un
        // second lancement, concurrentes de celles du bench.
        let mut got = bn.fake.lock().unwrap().log.clone();
        let mut want = st["calls"].as_array().cloned().unwrap_or_default();
        let key = |v: &Value| v.to_string();
        if st["again"].as_bool() == Some(true) {
            got.sort_by_key(key);
            want.sort_by_key(key);
        }
        assert_eq!(
            Value::Array(got),
            Value::Array(want),
            "{name} : requêtes au moteur"
        );
        let stored = |k: &str| {
            bn.go
                .get("state", k)
                .map_or(Value::Null, |raw| bn.decode(&raw))
        };
        assert_eq!(stored("last_bench"), st["last"], "{name} : dernier bench");
        assert_eq!(
            stored("bench_presets"),
            st["presets"],
            "{name} : par preset"
        );
        if name == "mode rapide" {
            // L'empreinte masquée du journal est bien celle de la
            // configuration, et le Go a ouvert puis fermé le travail annexe.
            let sb: Value =
                serde_json::from_str(&bn.go.get("state", "last_bench").unwrap()).unwrap();
            let cfg = bn.go.config_map();
            assert_eq!(
                sb["fingerprint"],
                json!(loki_rs::presets_api::bench_fingerprint(&cfg, &cfg))
            );
        }
    }
}

impl Bn {
    /// La configuration de base du journal, le faux moteur réglé.
    fn base(&self, n_ctx: i64, slow_hot: Duration) {
        for (k, v) in [
            ("MODEL", "/models/qwen.gguf"),
            ("CTX", "16384"),
            ("REASONING", "off"),
            ("CACHE_ISOLATE", "off"),
        ] {
            self.go.set("config", k, v);
        }
        self.go.set("config", "PORT", &self.port.to_string());
        *self.fake.lock().unwrap() = Fake {
            n_ctx,
            slow_hot,
            ..Default::default()
        };
    }

    async fn until_turn(&self) {
        for _ in 0..6000 {
            let p = self.status().await["phase"]
                .as_str()
                .unwrap_or("")
                .to_string();
            if p.starts_with("tour 1/") && self.fake.lock().unwrap().chats >= 4 {
                return;
            }
            tokio::time::sleep(Duration::from_millis(2)).await;
        }
        panic!("le tour 1 n'arrive jamais");
    }
}

/// Le bench prend le bail du moteur au motif « bench », ouvre puis ferme un
/// travail annexe (slot servi) et laisse sa trace dans la télémétrie.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn bail_travail_annexe_et_trace() {
    let bn = start().await;
    bn.base(16384, Duration::ZERO);
    bn.go.set("state", "api_key", "sk-moteur");
    let (code, _) = bn.call("POST", "/api/bench", "{}").await;
    assert_eq!(code, 202);
    bn.wait("bench").await;
    assert_eq!(bn.status().await["saved"], json!(true));
    assert_eq!(*bn.go.lease_whys.lock().unwrap(), vec!["bench".to_string()]);
    let log = bn.go.lease_log.lock().unwrap().clone();
    assert!(
        log.first().is_some_and(|l| l.starts_with("acquire 30000")),
        "{log:?}"
    );
    assert!(
        log.last().is_some_and(|l| l.starts_with("release ")),
        "{log:?}"
    );
    assert!(bn.go.lease.lock().unwrap().is_none(), "bail rendu");
    assert_eq!(
        *bn.go.side_jobs.lock().unwrap(),
        vec!["begin 1".to_string(), "end 1 true".to_string()]
    );
    assert_eq!(
        *bn.go.bench_perfs.lock().unwrap(),
        vec![json!({
            "prompt_n": 2036, "prompt_ms": 1018.0, "prompt_per_second": 2000.0,
            "predicted_n": 300, "predicted_per_second": 50.0,
        })]
    );
    // La clé de l'API locale accompagne les requêtes du bench.
    let auth = bn.fake.lock().unwrap().auth.clone();
    assert!(!auth.is_empty());
    assert!(auth.iter().all(|a| a == "Bearer sk-moteur"), "{auth:?}");
    // Le second slot en service : le bench y passe.
    *bn.go.side_live.lock().unwrap() = true;
    bn.fake.lock().unwrap().log.clear();
    bn.call("POST", "/api/bench", "{}").await;
    bn.wait("bench").await;
    let chats: Vec<Value> = bn
        .fake
        .lock()
        .unwrap()
        .log
        .iter()
        .filter(|c| c["path"] == json!("/v1/chat/completions"))
        .map(|c| c["body"]["id_slot"].clone())
        .collect();
    assert_eq!(chats, vec![json!(1), json!(1)]);
    // Échec avant tout envoi : le travail annexe est fermé sans slot servi.
    bn.fake.lock().unwrap().busy = true;
    bn.go.side_jobs.lock().unwrap().clear();
    bn.call("POST", "/api/bench", "{}").await;
    bn.wait("bench").await;
    assert_eq!(
        *bn.go.side_jobs.lock().unwrap(),
        vec!["begin 1".to_string(), "end 1 false".to_string()]
    );
}

/// Un stop du chat côté Go (signalé au renouvellement du bail) annule le
/// bench ; un bail perdu aussi.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn stop_du_chat_et_bail_perdu() {
    let bn = start_with(|b| b.lease_ttl = Duration::from_millis(300)).await;
    bn.base(16384, Duration::from_secs(20));
    bn.call("POST", "/api/bench", r#"{"mode":"full"}"#).await;
    bn.until_turn().await;
    *bn.go.lease_stop.lock().unwrap() = true;
    bn.wait("stop").await;
    let st = bn.status().await;
    assert_eq!(
        (st["error"].clone(), st["canceled"].clone()),
        (json!("benchmark annulé"), json!(true))
    );
    assert!(bn.go.lease.lock().unwrap().is_none(), "bail rendu");
    *bn.go.lease_stop.lock().unwrap() = false;

    bn.base(16384, Duration::from_secs(20));
    bn.call("POST", "/api/bench", r#"{"mode":"full"}"#).await;
    bn.until_turn().await;
    *bn.go.lease.lock().unwrap() = None;
    bn.wait("perdu").await;
    let st = bn.status().await;
    assert_eq!(
        (st["error"].clone(), st["canceled"].clone()),
        (json!("benchmark annulé"), json!(true))
    );
}

/// Sans la façade pour le bench, ses routes partent au Go.
#[tokio::test]
async fn sans_la_facade() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, _go, _stop) = fakego::start(tmp.path().to_path_buf()).await;
    let (c, b) = fakego::send(facade, "GET", "/api/bench/status", &[], "").await;
    assert_eq!(c.as_u16(), 200);
    assert_eq!(String::from_utf8_lossy(&b), "go:/api/bench/status");
}
