//! Le banc d'essai en tâche de fond (`llm_bench_job.go`), ses quatre routes
//! et l'enregistrement des mesures (`llm_bench.go`, « enregistrement ») :
//!
//! - `POST /api/bench` lance (202 + id) ; refus clairs avant de lancer ;
//! - `GET /api/bench/status` : la phase en cours, puis le résultat ou l'erreur ;
//! - `POST /api/bench/cancel` annule ;
//! - `GET /api/bench/last` : la dernière mesure enregistrée.
//!
//! Pendant la mesure, le bench tient le verrou de génération du Go par le
//! bail du moteur, motif « bench » : un message, une tâche ou un client /v1
//! reçoivent le refus du bench. Un stop du chat (ou un Reset) côté Go
//! interrompt le bail : le bench est annulé.

use std::collections::{BTreeMap, HashMap};
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use http_body_util::{BodyExt, Limited};
use hyper::body::Incoming;
use hyper::{Method, Request, Response, StatusCode};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio::sync::watch;

use super::{BenchResult, Corpora, Engine, Opts, Setup};
use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::chat::lease::EngineLease;
use crate::chat::wire::{EffortFallbacks, log_effort_fallback};
use crate::gojson::{self, Kind};
use crate::proxy::Body;
use crate::store::BK_STATE;

type Cfg = HashMap<String, String>;

const LAST_KEY: &str = "last_bench";
const PRESETS_KEY: &str = "bench_presets";
const BENCH_REQ: &str =
    r#"struct { Mode string "json:\"mode\""; Prompt int "json:\"prompt\""; N int "json:\"n\"" }"#;

/// Le job : le bench en cours ou le dernier terminé (un seul à la fois).
#[derive(Default)]
struct JobState {
    seq: i64,
    running: bool,
    /// Lancement en cours (le bail se prend hors du verrou).
    starting: bool,
    id: String,
    mode: String,
    phase: String,
    step: i64,
    steps: i64,
    started: Option<Instant>,
    finished: Option<Instant>,
    cancel: Option<watch::Sender<bool>>,
    result: Option<BenchResult>,
    err: String,
    canceled: bool,
}

/// Le banc d'essai tenu par la façade.
pub struct Bench {
    pub home: PathBuf,
    state: Mutex<JobState>,
    /// Budget d'une requête de chat (`benchReqTimeout`).
    pub req_timeout: Mutex<Duration>,
    /// Durée du bail du moteur (renouvelé au tiers).
    pub lease_ttl: Duration,
    /// Niveaux de raisonnement appris quand la façade ne tient pas la
    /// conversation (sinon, ceux de sa conversation).
    effort: EffortFallbacks,
    client: reqwest::Client,
    /// Le service du moteur (PID, pour l'indication « possible thrash »).
    engine: crate::engine::Paths,
}

impl Bench {
    pub fn new(engine: crate::engine::Paths) -> Self {
        Bench {
            home: engine.home.clone(),
            state: Mutex::new(JobState::default()),
            req_timeout: Mutex::new(super::REQ_TIMEOUT),
            lease_ttl: Duration::from_secs(30),
            effort: EffortFallbacks::new(),
            client: {
                crate::chat::engine::install_crypto();
                reqwest::Client::builder().build().unwrap_or_default()
            },
            engine,
        }
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, JobState> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// `benchRunning`.
    pub fn running(&self) -> bool {
        self.lock().running
    }

    /// `benchCancel` : false si aucun ne tournait.
    pub fn cancel(&self) -> bool {
        let st = self.lock();
        match (&st.cancel, st.running) {
            (Some(c), true) => {
                let _ = c.send(true);
                true
            }
            _ => false,
        }
    }

    /// `benchStatus`.
    pub fn status(&self) -> Value {
        let st = self.lock();
        let mut out = json!({ "ok": true, "running": st.running, "id": st.id, "mode": st.mode });
        if st.id.is_empty() {
            return out;
        }
        let o = out.as_object_mut().expect("objet");
        o.insert("phase".into(), json!(st.phase));
        o.insert("step".into(), json!(st.step));
        o.insert("steps".into(), json!(st.steps));
        let end = if st.running {
            Some(Instant::now())
        } else {
            st.finished
        };
        let elapsed = match (st.started, end) {
            (Some(s), Some(e)) => e.saturating_duration_since(s).as_secs_f64(),
            _ => 0.0,
        };
        o.insert("elapsed_sec".into(), json!(elapsed));
        if !st.running {
            if let Some(r) = &st.result {
                o.insert(
                    "result".into(),
                    serde_json::to_value(r).unwrap_or(Value::Null),
                );
                o.insert("saved".into(), json!(savable(r)));
            }
            if !st.err.is_empty() {
                o.insert("error".into(), json!(st.err));
                o.insert("canceled".into(), json!(st.canceled));
            }
        }
        out
    }

    /// `benchStart` : le statut HTTP et la réponse.
    pub async fn start(self: &Arc<Self>, ctx: &Ctx, opts: Opts) -> (StatusCode, Value) {
        let store = &ctx.store;
        let fail = |code: StatusCode, e: &str| (code, json!({ "ok": false, "error": e }));
        let cfg = store.config().await;
        if crate::engstatus::is_external(&cfg) {
            return fail(
                StatusCode::BAD_REQUEST,
                "benchmark indisponible : le preset actif est une API externe",
            );
        }
        if let Some(e) = crate::backends::tune_busy(store).await {
            return fail(StatusCode::CONFLICT, &e);
        }
        if !crate::engstatus::health_check(&cfg).await {
            return fail(
                StatusCode::SERVICE_UNAVAILABLE,
                crate::tasks::ERR_MODEL_LOADING,
            );
        }
        {
            let mut st = self.lock();
            if st.running || st.starting {
                return fail(StatusCode::CONFLICT, "un benchmark tourne déjà");
            }
            st.starting = true;
        }
        let lease = match EngineLease::acquire_for(store.clone(), self.lease_ttl, "bench").await {
            Ok(l) => l,
            Err(e) => {
                self.lock().starting = false;
                return fail(StatusCode::CONFLICT, &e.to_string());
            }
        };
        let (tx, rx) = watch::channel(false);
        let id = {
            let mut st = self.lock();
            st.seq += 1;
            let id = format!("b{}-{}", unix_secs(), st.seq);
            *st = JobState {
                seq: st.seq,
                running: true,
                starting: false,
                id: id.clone(),
                mode: opts.mode.clone(),
                phase: "démarrage".into(),
                step: 0,
                steps: 0,
                started: Some(Instant::now()),
                finished: None,
                cancel: Some(tx),
                result: None,
                err: String::new(),
                canceled: false,
            };
            id
        };
        let (me, ctx2, id2) = (self.clone(), ctx.clone(), id.clone());
        tokio::spawn(async move { me.job(ctx2, id2, opts, rx, lease).await });
        (StatusCode::ACCEPTED, json!({ "ok": true, "id": id }))
    }

    /// Le bench en tâche de fond : le verrou est rendu AVANT d'annoncer la
    /// fin — l'interface qui lit « terminé » et envoie aussitôt un message ne
    /// doit pas se voir refuser « benchmark en cours ».
    async fn job(
        self: Arc<Self>,
        ctx: Ctx,
        id: String,
        opts: Opts,
        rx: watch::Receiver<bool>,
        mut lease: EngineLease,
    ) {
        let me = self.clone();
        let id2 = id.clone();
        let progress = move |phase: &str, step: i64, steps: i64| {
            let mut st = me.lock();
            if st.id == id2 {
                st.phase = phase.to_string();
                st.step = step;
                st.steps = steps;
            }
        };
        let run = self.run_bench(&ctx, opts, rx.clone(), &progress);
        tokio::pin!(run);
        let mut interrupted = false;
        let res = loop {
            tokio::select! {
                r = &mut run => break r,
                _ = lease.interrupted(), if !interrupted => {
                    // Stop du chat, Reset ou bail perdu côté Go : le bench
                    // s'arrête (le verrou n'est plus, ou ne sera plus, à lui).
                    interrupted = true;
                    if let Some(c) = &self.lock().cancel {
                        let _ = c.send(true);
                    }
                }
            }
        };
        let canceled = *rx.borrow();
        lease.release().await;
        let mut st = self.lock();
        if st.id != id {
            return;
        }
        st.running = false;
        st.finished = Some(Instant::now());
        st.cancel = None;
        match res {
            Ok(r) => st.result = Some(r),
            Err(e) => {
                st.result = None;
                st.canceled = canceled;
                st.err = if canceled {
                    "benchmark annulé".into()
                } else {
                    e
                };
            }
        }
    }

    /// `runBench` : mesure le moteur local et enregistre le résultat s'il
    /// est complet.
    async fn run_bench(
        &self,
        ctx: &Ctx,
        opts: Opts,
        cancel: watch::Receiver<bool>,
        progress: &(dyn Fn(&str, i64, i64) + Send + Sync),
    ) -> Result<BenchResult, String> {
        let store = &ctx.store;
        let cfg = store.config().await;
        if crate::engstatus::is_external(&cfg) {
            return Err("benchmark indisponible : le preset actif est une API externe".into());
        }
        if crate::strata::is_strata(&cfg) {
            return Err("benchmark indisponible : le preset actif tourne sur Strata (le débit s'affiche sous chaque réponse)".into());
        }
        if let Some(e) = crate::backends::tune_busy(store).await {
            return Err(e);
        }
        let port = crate::engstatus::llm_port_raw(&cfg);
        if !crate::engstatus::health_check(&cfg).await {
            return Err(format!("serveur injoignable sur :{port}"));
        }
        let arg_env: HashMap<String, String> = super::ARG_ENV
            .iter()
            .filter_map(|k| {
                std::env::var(k)
                    .ok()
                    .filter(|v| !v.is_empty())
                    .map(|v| (k.to_string(), v))
            })
            .collect();
        let effort: &EffortFallbacks = match &ctx.chat {
            Some(c) => &c.shared.effort,
            None => &self.effort,
        };
        let mut setup: Setup = super::setup_from(&cfg, &arg_env, effort);
        setup.hybrid = model_hybrid(ctx, &cfg).await;
        let route = store
            .internal_json(
                Method::GET,
                "/internal/v1/oai/route?method=POST&path=%2Fv1%2Fchat%2Fcompletions",
                None,
            )
            .await
            .unwrap_or(Value::Null);
        setup.side_slot = route.get("side_live").and_then(Value::as_bool) == Some(true);
        let served = Arc::new(AtomicU64::new(0));
        let engine = Engine {
            base: format!("http://localhost:{port}"),
            key: route
                .get("key")
                .and_then(Value::as_str)
                .unwrap_or("")
                .to_string(),
            client: self.client.clone(),
            cancel,
            req_timeout: *self.req_timeout.lock().unwrap_or_else(|e| e.into_inner()),
            served: served.clone(),
        };
        let cfg2 = cfg.clone();
        let learn = move |want: &str, got: &str| {
            effort.remember(&cfg2, want, got);
            log_effort_fallback(want, got);
        };
        let paths = self.engine.clone();
        let io = move || engine_io_sample(&paths);
        // Le bench prend le slot de la conversation : une fois fini, le Go
        // l'efface si c'est sans risque (engineSideJob).
        let side = store
            .internal_post("/internal/v1/engine/sidejob", &json!({}))
            .await
            .ok()
            .and_then(|(_, v)| v.get("id").and_then(Value::as_i64));
        let res = super::run(
            &engine,
            opts,
            &mut setup,
            &Corpora::default_set(),
            progress,
            &learn,
            &io,
        )
        .await;
        if let Some(id) = side {
            let _ = store
                .internal_post(
                    "/internal/v1/engine/sidejob",
                    &json!({ "end": id, "served": served.load(Ordering::SeqCst) > 0 }),
                )
                .await;
        }
        let res = res?;
        let _ = store
            .internal_post(
                "/internal/v1/bench/perf",
                &json!({
                    "prompt_n": res.prompt_n,
                    "prompt_ms": res.prompt_ms,
                    "prompt_per_second": res.prompt_per_second,
                    "predicted_n": res.predicted_n,
                    "predicted_per_second": res.predicted_per_second,
                }),
            )
            .await;
        if savable(&res) {
            save_last(ctx, &res, &cfg).await;
            self.save_for_active_preset(ctx, &res, &cfg).await;
        }
        Ok(res)
    }

    /// `saveBenchForActivePreset` : sous l'id du preset actif ; sans effet si
    /// aucun ne correspond.
    async fn save_for_active_preset(&self, ctx: &Ctx, res: &BenchResult, cfg: &Cfg) {
        let store = &ctx.store;
        let order = crate::presets::parse_preset_order(
            store
                .get(BK_STATE, crate::presets::PRESET_ORDER_KEY)
                .await
                .ok()
                .flatten()
                .as_deref(),
        );
        let Ok(list) = crate::presets::list_presets(&self.home, cfg, &order) else {
            return;
        };
        let Some(id) = list.into_iter().find(|p| p.active).map(|p| p.id) else {
            return;
        };
        let mut m = load_bench_store(ctx).await;
        m.insert(id, saved_bench(res, cfg));
        let _ = store
            .put(BK_STATE, PRESETS_KEY, gojson::marshal(&m).into_bytes())
            .await;
    }
}

fn unix_secs() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_secs() as i64)
}

/// `benchSavable` : seul un résultat COMPLET est enregistré.
pub fn savable(res: &BenchResult) -> bool {
    res.depth.as_ref().is_none_or(|d| d.partial.is_empty())
}

/// `savedBench`.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct Saved {
    #[serde(default)]
    pub result: BenchResult,
    #[serde(default)]
    pub model: String,
    #[serde(default)]
    pub at: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub fingerprint: String,
}

/// `newSavedBench` : l'indication du moment (thrash) n'est pas enregistrée.
pub fn saved_bench(res: &BenchResult, cfg: &Cfg) -> Saved {
    let mut r = res.clone();
    if let Some(d) = &mut r.depth {
        d.hint.clear();
    }
    Saved {
        result: r,
        model: crate::paths::go_base(cfg.get("MODEL").map(String::as_str).unwrap_or("")),
        at: unix_secs(),
        fingerprint: crate::presets_api::bench_fingerprint(cfg, cfg),
    }
}

/// `saveLastBench`.
async fn save_last(ctx: &Ctx, res: &BenchResult, cfg: &Cfg) {
    let _ = ctx
        .store
        .put(
            BK_STATE,
            LAST_KEY,
            gojson::marshal(&saved_bench(res, cfg)).into_bytes(),
        )
        .await;
}

/// `loadBenchStore` : les mesures par preset (vide s'il n'y en a pas).
async fn load_bench_store(ctx: &Ctx) -> BTreeMap<String, Saved> {
    ctx.store
        .get(BK_STATE, PRESETS_KEY)
        .await
        .ok()
        .flatten()
        .and_then(|b| serde_json::from_slice::<Option<BTreeMap<String, Saved>>>(&b).ok())
        .flatten()
        .unwrap_or_default()
}

/// `loadLastBench`.
async fn load_last(ctx: &Ctx) -> Option<Saved> {
    let b = ctx.store.get(BK_STATE, LAST_KEY).await.ok().flatten()?;
    serde_json::from_slice(&b).ok()
}

/// Le modèle du preset est-il hybride (reprise du cache aux points de
/// contrôle) ?
pub(crate) async fn model_hybrid(ctx: &Ctx, cfg: &Cfg) -> bool {
    let m = cfg.get("MODEL").map(|s| s.trim()).unwrap_or("");
    if m.is_empty() {
        return false;
    }
    let stored = crate::gguf::parse_stored_model_dirs(
        ctx.store
            .get(BK_STATE, crate::gguf::STATE_MODEL_DIRS_KEY)
            .await
            .ok()
            .flatten()
            .as_deref(),
    );
    let env = std::env::var(crate::gguf::MODEL_DIRS_ENV).ok();
    let Ok(p) = crate::gguf::resolve_serve_model_path(m, &ctx.engine.home, &stored, env.as_deref())
    else {
        return false;
    };
    match crate::gguf::gguf_meta(&p) {
        Ok(g) => crate::serve::ckpt::gguf_hybrid(Some(&g)),
        Err(_) => false,
    }
}

/// `engineIOSample` : octets lus du disque et RSS du moteur ; `None` si le
/// PID ou /proc est illisible.
fn engine_io_sample(paths: &crate::engine::Paths) -> Option<(i64, i64)> {
    let pid = crate::engine::read_pid(&paths.pid());
    if pid <= 0 {
        return None;
    }
    let io = std::fs::read_to_string(format!("/proc/{pid}/io")).ok()?;
    for line in io.split('\n') {
        if let Some(v) = line.strip_prefix("read_bytes:") {
            let n = v.trim().parse::<i64>().ok()?;
            let rss = std::fs::read_to_string(format!("/proc/{pid}/status"))
                .map(|s| crate::engstatus::parse_vm_rss(&s))
                .unwrap_or(0);
            return Some((n, rss));
        }
    }
    None
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

/// Les routes servies par ce module.
pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/bench" | "/api/bench/status" | "/api/bench/cancel" | "/api/bench/last"
    )
}

/// `postOnly`.
fn post_only(method: &Method) -> Option<Response<Body>> {
    if method == Method::POST {
        return None;
    }
    let mut r = send_json(
        StatusCode::METHOD_NOT_ALLOWED,
        &json!({ "ok": false, "error": format!("méthode {method} refusée : POST attendu") }),
    );
    r.headers_mut().insert(
        hyper::header::ALLOW,
        hyper::header::HeaderValue::from_static("POST"),
    );
    Some(r)
}

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(
    ctx: &Ctx,
    bench: &Arc<Bench>,
    req: Request<Incoming>,
    from_go: bool,
) -> Response<Body> {
    let (parts, body) = req.into_parts();
    if !from_go && let Err(refus) = ctx.guard(&parts.headers).await {
        return *refus;
    }
    match parts.uri.path() {
        "/api/bench" => {
            if let Some(r) = post_only(&parts.method) {
                return r;
            }
            let raw = Limited::new(body, 1 << 20)
                .collect()
                .await
                .map(|b| b.to_bytes().to_vec())
                .unwrap_or_default();
            // Les erreurs de décodage sont ignorées (champs facultatifs).
            let d = gojson::decode_struct(
                &raw,
                BENCH_REQ,
                &[("mode", Kind::Str), ("prompt", Kind::Int), ("n", Kind::Int)],
            );
            let mut opts = Opts {
                mode: super::MODE_QUICK.into(),
                prompt: 2000,
                predict: 300,
                depth: 0,
            };
            if d.str("mode").trim().eq_ignore_ascii_case(super::MODE_FULL) {
                opts.mode = super::MODE_FULL.into();
            }
            if d.int("prompt") > 0 {
                opts.prompt = d.int("prompt").min(32768);
            }
            if d.int("n") > 0 {
                opts.predict = d.int("n").min(4096);
            }
            let (code, out) = bench.start(ctx, opts).await;
            send_json(code, &out)
        }
        "/api/bench/status" => send_json(StatusCode::OK, &bench.status()),
        "/api/bench/cancel" => {
            if let Some(r) = post_only(&parts.method) {
                return r;
            }
            send_json(StatusCode::OK, &json!({ "ok": bench.cancel() }))
        }
        "/api/bench/last" => match load_last(ctx).await {
            None => send_json(StatusCode::OK, &json!({ "ok": false })),
            Some(sb) => send_json(
                StatusCode::OK,
                &json!({ "ok": true, "result": sb.result, "model": sb.model, "at": sb.at }),
            ),
        },
        _ => send_json(
            StatusCode::NOT_FOUND,
            &json!({ "ok": false, "error": "route inconnue" }),
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[cfg(target_os = "linux")]
    #[test]
    fn lecture_du_moteur() {
        let tmp = tempfile::tempdir().unwrap();
        let paths = crate::engine::Paths {
            home: tmp.path().to_path_buf(),
            service: "loki-engine".into(),
        };
        assert_eq!(engine_io_sample(&paths), None, "sans PID");
        std::fs::create_dir_all(paths.pid().parent().unwrap()).unwrap();
        std::fs::write(paths.pid(), "0\n").unwrap();
        assert_eq!(engine_io_sample(&paths), None, "PID nul");
        std::fs::write(paths.pid(), format!("{}\n", std::process::id())).unwrap();
        let (read, rss) = engine_io_sample(&paths).expect("ce processus");
        assert!(read >= 0 && rss > 0, "{read} {rss}");
    }

    #[test]
    fn mesure_enregistree() {
        let cfg: Cfg = [("MODEL", "/models/Q.gguf"), ("CTX", "32768")]
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect();
        let res = BenchResult {
            prompt_per_second: 1.0,
            depth: Some(super::super::Depth {
                hint: "possible thrash".into(),
                ..Default::default()
            }),
            ..Default::default()
        };
        let sb = saved_bench(&res, &cfg);
        assert_eq!(sb.model, "Q.gguf");
        assert!(!sb.fingerprint.is_empty());
        assert_eq!(
            sb.fingerprint,
            crate::presets_api::bench_fingerprint(&cfg, &cfg)
        );
        assert_eq!(
            sb.result.depth.as_ref().unwrap().hint,
            "",
            "l'indication ne s'enregistre pas"
        );
        assert_eq!(res.depth.as_ref().unwrap().hint, "possible thrash");
        assert!(sb.at > 0);
        assert_eq!(saved_bench(&res, &Cfg::new()).model, ".");
        // Seul un résultat complet s'enregistre.
        assert!(savable(&BenchResult::default()));
        assert!(savable(&res));
        let partial = BenchResult {
            depth: Some(super::super::Depth {
                partial: "tour 1 interrompu".into(),
                ..Default::default()
            }),
            ..Default::default()
        };
        assert!(!savable(&partial));
    }
}
