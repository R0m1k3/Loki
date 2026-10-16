//! Lot 24 : panneau « moteur de l'image » tenu par la façade
//! (`LOKI_ENGINE_IMAGE=rust`, avec `LOKI_BACKENDS=rust`).
//!
//! Le journal tiré du Go (`internal/loki/parity_engineimg_test.go` →
//! `tests/parity_engineimg/engineimg.json`) est rejoué par une façade, contre
//! un faux registre OCI (jeton, index multi-architecture, manifeste annoté,
//! couches .tar.gz) et un faux moteur local (`/health`, `/props`,
//! `/apply-template` : le `fakeTpl` du Go) pour la sonde de gabarit et la
//! vérification du rendu avant/après la bascule : statut, dernier build
//! publié, plan (dernière ou recommandée, risque de rendu du raisonnement),
//! mise à jour (couches, liens, essai à blanc, REASONING_PRESERVE=off dans le
//! preset actif ou la configuration, ménage, rendu comparé), bascules,
//! suppression, retour à la version précédente, et les échecs.

mod common;

use std::collections::BTreeMap;
use std::convert::Infallible;
use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use bytes::Bytes;
use common::fakego;
use common::parity::{apply_files, tar_gz, tree};
use http_body_util::{BodyExt, Full};
use hyper::service::service_fn;
use hyper::{Request, Response};
use hyper_util::rt::TokioIo;
use loki_rs::backends::Backends;
use loki_rs::backends::probe::Probe;
use loki_rs::chat::tplprobe::TplProbe;
use loki_rs::engineimg::{EngineImg, oci};
use regex::Regex;
use serde_json::{Value, json};
use tokio::net::TcpListener;

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_engineimg/engineimg.json")).expect("journal")
}

fn s(v: &Value) -> &str {
    v.as_str().unwrap_or("")
}

const TUNE_BUSY: &str = "optimisation en cours (loki tune) : le moteur est réservé aux essais — attends la fin ou annule-la";

/// Sert un handler HTTP sur un port libre ; renvoie l'adresse.
async fn serve<F>(h: F) -> SocketAddr
where
    F: Fn(String, String, Option<String>, Vec<u8>) -> (u16, Vec<u8>) + Send + Sync + 'static,
{
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = l.local_addr().unwrap();
    let h = Arc::new(h);
    tokio::spawn(async move {
        while let Ok((st, _)) = l.accept().await {
            let h = h.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<hyper::body::Incoming>| {
                    let h = h.clone();
                    async move {
                        let method = req.method().to_string();
                        let path = req.uri().path().to_string();
                        let auth = req
                            .headers()
                            .get("authorization")
                            .and_then(|v| v.to_str().ok())
                            .map(str::to_string);
                        let body = req
                            .into_body()
                            .collect()
                            .await
                            .map(|b| b.to_bytes().to_vec())
                            .unwrap_or_default();
                        let (code, out) = h(method, path, auth, body);
                        let mut r = Response::new(Full::new(Bytes::from(out)));
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
    addr
}

/// Le faux registre : `{token, token_body, manifest, manifest_body, no_arch,
/// blob, version, fixed, layers}`.
async fn fake_registry(reg: Arc<Mutex<Value>>) -> String {
    let addr = serve(move |_, path, auth, _| {
        let cur = reg.lock().unwrap().clone();
        let version = s(&cur["version"]).to_string();
        let mut blobs: BTreeMap<String, Vec<u8>> = BTreeMap::new();
        let mut layers = Vec::new();
        for (i, l) in cur["layers"].as_array().into_iter().flatten().enumerate() {
            let data = tar_gz(l["entries"].as_array().map_or(&[][..], Vec::as_slice));
            let digest = format!("sha256:couche{i}");
            let size = l["size"].as_i64().unwrap_or(0);
            let size = if size > 0 { size } else { data.len() as i64 };
            layers.push(json!({
                "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
                "digest": digest, "size": size,
            }));
            blobs.insert(digest, data);
        }
        let annotations = json!({ "org.opencontainers.image.version": version });
        let manifest = json!({ "schemaVersion": 2, "layers": layers, "annotations": annotations });
        let man_dig = "sha256:manifeste";
        let arch = if cur["no_arch"] == json!(true) {
            "s390x"
        } else {
            oci::oci_arch(cfg!(target_arch = "aarch64"))
        };
        let index = json!({
            "schemaVersion": 2,
            "manifests": [
                { "digest": "sha256:autre", "platform": { "os": "linux", "architecture": "ppc64le" } },
                { "digest": man_dig, "platform": { "os": "linux", "architecture": arch } },
            ],
        });
        let last = path.rsplit('/').next().unwrap_or("").to_string();
        if path == "/token" {
            return match cur["token"].as_u64() {
                Some(code) if code != 0 => (code as u16, Vec::new()),
                _ if !s(&cur["token_body"]).is_empty() => {
                    (200, s(&cur["token_body"]).as_bytes().to_vec())
                }
                _ => (200, br#"{"token":"jeton"}"#.to_vec()),
            };
        }
        if path.contains("/manifests/") {
            if auth.as_deref() != Some("Bearer jeton") {
                return (401, Vec::new());
            }
            if let Some(code) = cur["manifest"].as_u64().filter(|c| *c != 0) {
                return (code as u16, Vec::new());
            }
            if !s(&cur["manifest_body"]).is_empty() {
                return (200, s(&cur["manifest_body"]).as_bytes().to_vec());
            }
            let fixed = cur["fixed"]
                .as_array()
                .into_iter()
                .flatten()
                .any(|f| s(f) == last);
            return if last == man_dig {
                (200, manifest.to_string().into_bytes())
            } else if last == "server-cuda" || fixed {
                (200, index.to_string().into_bytes())
            } else {
                (404, Vec::new())
            };
        }
        if path.contains("/blobs/") {
            if let Some(code) = cur["blob"].as_u64().filter(|c| *c != 0) {
                return (code as u16, Vec::new());
            }
            return match blobs.remove(&last) {
                Some(d) => (200, d),
                None => (404, Vec::new()),
            };
        }
        (404, Vec::new())
    })
    .await;
    format!("http://{addr}")
}

/// Le faux moteur local : le `fakeTpl` du Go (mode, gabarit, build), ou
/// arrêté (503 partout).
#[derive(Clone, Default)]
struct Local {
    mode: String,
    build: String,
    tpl: String,
    down: bool,
    /// Statut imposé à `/apply-template` (0 : rendu).
    apply: u16,
}

/// `fakeTpl.render`.
fn render(mode: &str, body: &Value) -> (u16, Vec<u8>) {
    let id_re = Regex::new("^[A-Za-z0-9]{9}$").unwrap();
    let empty = Vec::new();
    let msgs = body["messages"].as_array().unwrap_or(&empty);
    let last_user = msgs.iter().rposition(|m| m["role"] == "user");
    let mut b = String::new();
    if let Some(tools) = body["tools"].as_array() {
        b.push_str(&format!("<tools n={}>\n", tools.len()));
    }
    for (i, m) in msgs.iter().enumerate() {
        let role = s(&m["role"]);
        b.push_str(&format!("<|im_start|>{role}\n"));
        let r = s(&m["reasoning_content"]);
        if !r.is_empty()
            && role == "assistant"
            && mode != "drop"
            && (mode != "qwen3" || last_user.is_none_or(|u| i > u))
        {
            b.push_str(&format!("<think>{r}</think>"));
        }
        b.push_str(s(&m["content"]));
        for call in m["tool_calls"].as_array().into_iter().flatten() {
            let id = s(&call["id"]);
            if !id_re.is_match(id) || i + 1 >= msgs.len() || msgs[i + 1]["tool_call_id"] != id {
                return (
                    500,
                    br#"{"error":{"code":500,"message":"Jinja Exception: tool call id invalide"}}"#
                        .to_vec(),
                );
            }
            b.push_str(&format!(
                "<tool_call>{}</tool_call>",
                s(&call["function"]["name"])
            ));
        }
        b.push_str("<|im_end|>\n");
    }
    if body["add_generation_prompt"] == json!(true) || mode == "noagp" {
        b.push_str("<|im_start|>assistant\n");
    }
    (200, json!({ "prompt": b }).to_string().into_bytes())
}

async fn fake_engine(local: Arc<Mutex<Local>>) -> u16 {
    serve(move |_, path, _, body| {
        let l = local.lock().unwrap().clone();
        if l.down {
            return (503, Vec::new());
        }
        match path.as_str() {
            "/health" => (200, br#"{"status":"ok"}"#.to_vec()),
            "/props" => {
                let tpl = if l.tpl.is_empty() { &l.mode } else { &l.tpl };
                let build = if l.build.is_empty() {
                    "b7000-abc"
                } else {
                    &l.build
                };
                let v = json!({
                    "build_info": build,
                    "model_path": "/models/Qwen3-8B-Q8_0.gguf",
                    "chat_template": tpl,
                });
                (200, v.to_string().into_bytes())
            }
            "/apply-template" if l.apply != 0 => (
                l.apply,
                r#"{"error":{"code":500,"message":"Jinja Exception: gabarit cassé"}}"#
                    .as_bytes()
                    .to_vec(),
            ),
            "/apply-template" => {
                let v: Value = serde_json::from_slice(&body).unwrap_or(Value::Null);
                render(&l.mode, &v)
            }
            _ => (404, Vec::new()),
        }
    })
    .await
    .port()
}

struct Bench {
    facade: SocketAddr,
    go: Arc<fakego::FakeGo>,
    _stop: tokio::sync::oneshot::Sender<()>,
    home: PathBuf,
    b: Arc<Backends>,
    reg: Arc<Mutex<Value>>,
    reg_base: String,
    local: Arc<Mutex<Local>>,
    port: u16,
    _tmp: Vec<tempfile::TempDir>,
}

async fn bench() -> Bench {
    bench_with(Duration::from_secs(10)).await
}

async fn bench_with(render_wait: Duration) -> Bench {
    let tmp = tempfile::tempdir().unwrap();
    let root = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    let reg = Arc::new(Mutex::new(json!({})));
    let reg_base = fake_registry(reg.clone()).await;
    let local = Arc::new(Mutex::new(Local::default()));
    let port = fake_engine(local.clone()).await;
    let b = Arc::new(Backends::with(
        home.clone(),
        home.join("image/llama-server")
            .to_string_lossy()
            .into_owned(),
        Probe {
            path: Some(vec![]),
            root: root.path().to_path_buf(),
        },
        String::new(),
    ));
    let mut probe = TplProbe::new();
    probe.auto = false;
    probe.backoff = Duration::from_millis(1);
    let mut e = EngineImg::new(
        home.clone(),
        oci::Registry::new(reg_base.clone(), cfg!(target_arch = "aarch64")),
        true,
        Arc::new(probe),
    );
    e.render_poll = Duration::from_millis(1);
    e.render_wait = render_wait;
    e.env = Some(Default::default());
    let (facade, go, stop, _) = fakego::start_with(
        home.clone(),
        fakego::Opts {
            backends: Some(b.clone()),
            engineimg: Some(Arc::new(e)),
            ..Default::default()
        },
    )
    .await;
    go.set("config", "PORT", &port.to_string());
    *go.apply_presets.lock().unwrap() = true;
    Bench {
        facade,
        go,
        _stop: stop,
        home,
        b,
        reg,
        reg_base,
        local,
        port,
        _tmp: vec![tmp, root],
    }
}

impl Bench {
    fn mask(&self, v: &str) -> String {
        let ts = Regex::new(r#""(started_at|ended_at)":[1-9][0-9]*"#).unwrap();
        let at = Regex::new(r#""at":"[^"]*""#).unwrap();
        let v = v
            .replace(&self.reg_base, "{{REG}}")
            .replace(&oci::host_of(&self.reg_base), "{{REGHOST}}")
            .replace(&self.home.to_string_lossy().into_owned(), "{{HOME}}");
        let v = ts.replace_all(&v, r#""$1":"{{T}}""#);
        at.replace_all(&v, r#""at":"{{T}}""#).into_owned()
    }

    fn unmask(&self, v: &str) -> String {
        v.replace("{{HOME}}", &self.home.to_string_lossy())
            .replace("{{PORT}}", &self.port.to_string())
    }

    async fn call(&self, method: &str, path: &str, body: &str) -> (u16, Value) {
        let (code, b) = fakego::send(self.facade, method, path, &[], body).await;
        let out = serde_json::from_str(&self.mask(&String::from_utf8_lossy(&b)))
            .unwrap_or_else(|_| panic!("{path} : corps illisible {b:?}"));
        (code.as_u16(), out)
    }

    fn set_local(&self, v: &Value) {
        *self.local.lock().unwrap() = Local {
            mode: s(&v["mode"]).into(),
            build: s(&v["build"]).into(),
            tpl: s(&v["template"]).into(),
            down: v["down"] == json!(true),
            apply: v["apply"].as_u64().unwrap_or(0) as u16,
        };
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

    async fn wait_render(&self) -> Value {
        for _ in 0..1500 {
            let (_, v) = self.call("GET", "/api/engine", "").await;
            let c = &v["render_check"];
            if !c.is_null() && c["status"] != "pending" {
                return c.clone();
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        panic!("la vérification du rendu ne finit pas");
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn parite_du_panneau() {
    let fx = fixture();
    if (fx["goarch"] == "arm64") != cfg!(target_arch = "aarch64") {
        eprintln!("journal tiré sur une autre architecture : rejeu sauté");
        return;
    }
    let bn = bench().await;
    for st in fx["steps"].as_array().unwrap() {
        let name = s(&st["name"]);
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
        if !st["registry"].is_null() {
            *bn.reg.lock().unwrap() = st["registry"].clone();
        }
        if !st["local"].is_null() {
            bn.set_local(&st["local"]);
        }
        if let Some(e) = st["engine_env"].as_str() {
            bn.b.set_engine_bin_env(&bn.unmask(e));
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
            if !st["after_job"].is_null() {
                bn.set_local(&st["after_job"]);
            }
        }
        if st["wait_render"].as_bool() == Some(true) {
            assert_eq!(bn.wait_render().await, st["render"], "{name} : rendu");
        }
        let mut after = BTreeMap::new();
        for k in ["BIN", "REASONING_PRESERVE"] {
            if let Some(v) = bn.go.get("config", k).filter(|v| !v.is_empty()) {
                after.insert(format!("config/{k}"), Value::String(bn.mask(&v)));
            }
        }
        if let Ok(b) = std::fs::read(bn.home.join("engine/previous-bin")) {
            after.insert(
                "previous-bin".to_string(),
                Value::String(bn.mask(&String::from_utf8_lossy(&b))),
            );
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
        let t = if t.is_empty() {
            Value::Null
        } else {
            Value::Array(t)
        };
        assert_eq!(t, st["tree"], "{name} : arborescence");
    }
}

/// Autour de la bascule d'une mise à jour, le moteur qui tournait est arrêté
/// puis relancé (le Go pilote le service) ; une bascule sans réseau le
/// redémarre s'il tourne.
#[tokio::test]
async fn service_arrete_puis_relance() {
    let fx = fixture();
    let steps = fx["steps"].as_array().unwrap();
    let step = |n: &str| steps.iter().find(|s| s["name"] == n).unwrap().clone();
    let bn = bench().await;
    let first = step("statut, moteur de l'image");
    let files: serde_json::Map<String, Value> = first["files"]
        .as_object()
        .unwrap()
        .iter()
        .map(|(k, v)| (k.clone(), Value::String(bn.unmask(s(v)))))
        .collect();
    apply_files(&bn.home, &Value::Object(files));
    *bn.reg.lock().unwrap() = first["registry"].clone();
    bn.set_local(&json!({ "down": true }));
    *bn.go.service_active.lock().unwrap() = true;

    assert_eq!(bn.call("POST", "/api/engine/update", "{}").await.0, 200);
    bn.wait_job().await;
    let (_, job) = bn.call("GET", "/api/llamacpp/job", "").await;
    let lines: Vec<&str> = job["lines"]
        .as_array()
        .unwrap()
        .iter()
        .map(|v| v.as_str().unwrap())
        .collect();
    let bin = "BIN → {{HOME}}/engine/server-cuda-b10900/llama-server";
    let at = lines.iter().position(|l| *l == bin).expect("BIN posé");
    assert_eq!(lines[at - 1], "▶ arrêt du moteur…");
    assert_eq!(lines[at + 1], "▶ redémarrage du moteur…");
    assert_eq!(
        *bn.go.service_actions.lock().unwrap(),
        vec![("stop".to_string(), true), ("start".to_string(), false)]
    );

    bn.go.service_actions.lock().unwrap().clear();
    let (c, out) = bn
        .call("POST", "/api/engine/use", r#"{"tag":"image"}"#)
        .await;
    assert_eq!((c, out["ok"].clone()), (200, json!(true)));
    assert_eq!(
        *bn.go.service_actions.lock().unwrap(),
        vec![("restart".to_string(), true)]
    );

    // Arrêté : la mise à jour ne le relance pas.
    *bn.go.service_active.lock().unwrap() = false;
    bn.go.service_actions.lock().unwrap().clear();
    let (c, _) = bn
        .call(
            "POST",
            "/api/engine/update",
            r#"{"tag":"server-cuda-b10900"}"#,
        )
        .await;
    assert_eq!(c, 200);
    bn.wait_job().await;
    assert_eq!(
        *bn.go.service_actions.lock().unwrap(),
        vec![("stop".to_string(), true)]
    );
    let (_, job) = bn.call("GET", "/api/llamacpp/job", "").await;
    assert!(
        !job["lines"]
            .as_array()
            .unwrap()
            .iter()
            .any(|l| l.as_str().unwrap().contains("arrêt du moteur")),
        "{job}"
    );
}

/// Sans les backends dans la façade (pas de job), le panneau répond 503.
#[tokio::test]
async fn sans_backends() {
    let tmp = tempfile::tempdir().unwrap();
    let e = EngineImg::new(
        tmp.path().to_path_buf(),
        oci::Registry::new("http://127.0.0.1:9".into(), false),
        true,
        Arc::new(TplProbe::new()),
    );
    let (facade, _go, _stop, _) = fakego::start_with(
        tmp.path().to_path_buf(),
        fakego::Opts {
            engineimg: Some(Arc::new(e)),
            ..Default::default()
        },
    )
    .await;
    let (c, b) = fakego::send(facade, "GET", "/api/engine", &[], "").await;
    assert_eq!(c.as_u16(), 503, "{b:?}");
}

/// Le nouveau moteur ne répond pas à temps (build inchangé) : relevé
/// « unknown », sans comparaison.
#[tokio::test]
async fn rendu_non_verifie_a_temps() {
    let fx = fixture();
    let steps = fx["steps"].as_array().unwrap();
    let first = steps[0].clone();
    let bn = bench_with(Duration::from_millis(200)).await;
    let files: serde_json::Map<String, Value> = first["files"]
        .as_object()
        .unwrap()
        .iter()
        .map(|(k, v)| (k.clone(), Value::String(bn.unmask(s(v)))))
        .collect();
    apply_files(&bn.home, &Value::Object(files));
    *bn.reg.lock().unwrap() = first["registry"].clone();
    bn.set_local(&first["local"]);
    assert_eq!(bn.call("POST", "/api/engine/update", "{}").await.0, 200);
    bn.wait_job().await;
    let c = bn.wait_render().await;
    assert_eq!(c["status"], "unknown", "{c}");
    assert_eq!(
        c["detail"],
        "le nouveau moteur n'a pas répondu à temps : rendu non vérifié"
    );
    assert_eq!(c["tag"], "server-cuda-b10900");
}
