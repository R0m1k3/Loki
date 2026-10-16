//! L'optimiseur côté web — port de `backend_tune_job.go` et
//! `backend_tune_run.go` (la partie pure est dans [`super::core`]).
//!
//! ```text
//! POST /api/tune          lance (202), 409 si le moteur est occupé
//! GET  /api/tune/status   phase, essais, ETA ; puis le résultat
//! POST /api/tune/cancel   annule (l'essai en cours est arrêté, le moteur relancé)
//! POST /api/tune/apply    {target:"copy"|"preset"} — SEULEMENT sur un clic
//! GET  /api/tune/last     dernier résultat d'un preset, et si le moteur a changé depuis
//! ```
//!
//! Le déroulé : refus d'entrée, verrou exclusif ([`super::lock`]), vrai
//! moteur arrêté ; chaque essai est une COPIE de la configuration avec ses
//! surcharges (`LOKI_HOME/tune/run/<essai>/config.env`, jamais config.env),
//! composée par « loki serve » du Go à blanc, puis lancée sur 127.0.0.1 et un
//! port libre, et mesurée par le bench complet à une profondeur fixe. Le
//! groupe de processus de l'essai est tué en sortie, quoi qu'il arrive. À la
//! fin, le vrai moteur est relancé et le verrou rendu. RIEN n'est écrit dans
//! un preset sans un geste de l'utilisateur.

use std::collections::{BTreeMap, HashMap};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use http_body_util::{BodyExt, Limited};
use hyper::body::Incoming;
use hyper::{Method, Request, Response, StatusCode};
use serde::{Deserialize, Deserializer, Serialize, Serializer};
use serde_json::value::RawValue;
use serde_json::{Map, Value, json};
use tokio::sync::{Notify, watch};

use super::core::{self as tc, Cfg, Launch, Run, Weights};
use super::lock::{self, ERR_TUNE_BUSY, Held, LockFile, Owner, Proc};
use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::bench::{self, BenchResult, Corpora, Engine, ser_go_f64};
use crate::chat::lease::{EngineLease, LEASE_TTL, LeaseError, LeaseSignal};
use crate::chat::wire::{EffortFallbacks, log_effort_fallback};
use crate::gojson::{self, Kind};
use crate::proxy::Body;
use crate::store::BK_STATE;

/// `tuneDefaultBudget`, `tuneMaxBudget`.
pub const DEFAULT_BUDGET: Duration = Duration::from_secs(30 * 60);
pub const MAX_BUDGET: Duration = Duration::from_secs(6 * 3600);
/// `tuneLoadTimeout` : chargement d'un essai (un gros MoE relu d'un disque
/// lent prend plusieurs minutes).
pub const LOAD_TIMEOUT: Duration = Duration::from_secs(20 * 60);

const RESULTS_KEY: &str = "tune_results";
const APPLY_COPY: &str = "copy";
const APPLY_PRESET: &str = "preset";

const TUNE_REQ: &str = r#"struct { Placement bool "json:\"placement\""; OptIn bool "json:\"optin\""; BudgetMin int "json:\"budget_min\""; Stages []string "json:\"stages\"" }"#;
const APPLY_REQ: &str = r#"struct { Target string "json:\"target\"" }"#;

// ---------------------------------------------------------------------------
// Ce qui se montre
// ---------------------------------------------------------------------------

/// `tuneOpts` : ce que l'utilisateur a demandé.
#[derive(Clone, Debug, Default)]
pub struct Opts {
    pub placement: bool,
    pub opt_in: bool,
    /// Zéro : [`DEFAULT_BUDGET`].
    pub budget: Duration,
    pub via: String,
    /// Étapes de base à faire ; vide = toutes.
    pub stages: Vec<String>,
}

fn zero(v: &f64) -> bool {
    *v == 0.0
}

/// Une liste vide s'écrit `null`, comme une tranche nil du Go.
fn ser_nil_vec<T: Serialize, S: Serializer>(v: &[T], s: S) -> Result<S::Ok, S::Error> {
    if v.is_empty() {
        s.serialize_none()
    } else {
        v.serialize(s)
    }
}

fn de_nil_vec<'de, T: Deserialize<'de>, D: Deserializer<'de>>(d: D) -> Result<Vec<T>, D::Error> {
    Ok(Option::<Vec<T>>::deserialize(d)?.unwrap_or_default())
}

/// `tuneTrial` : un essai, tel que montré à l'utilisateur.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct Trial {
    #[serde(default)]
    pub stage: String,
    #[serde(default)]
    pub label: String,
    /// en cours, mesuré, retenu, écarté, échec, doublon
    #[serde(default)]
    pub status: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub why: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub runs: Vec<Run>,
    #[serde(default, skip_serializing_if = "zero", serialize_with = "ser_go_f64")]
    pub turn_sec: f64,
    #[serde(default, serialize_with = "ser_go_f64")]
    pub gain: f64,
    /// VRAM libre minimale après le bench ; -1 = non mesurée.
    #[serde(default, rename = "headroom_mib")]
    pub headroom: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub offloaded: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub changes: Vec<String>,
    #[serde(default, serialize_with = "ser_go_f64")]
    pub sec: f64,
}

/// `tuneResult` : l'issue d'une optimisation.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct TuneResult {
    #[serde(default)]
    pub preset_id: String,
    #[serde(default)]
    pub preset_name: String,
    #[serde(default)]
    pub base_fp: String,
    #[serde(default)]
    pub preset_fp: String,
    #[serde(default)]
    pub engine: String,
    #[serde(default)]
    pub engine_build: i64,
    #[serde(default)]
    pub weights: Weights,
    #[serde(default)]
    pub depth: i64,
    #[serde(default)]
    pub n_ctx: i64,
    #[serde(default)]
    pub slots: i64,
    #[serde(default)]
    pub baseline: Trial,
    #[serde(
        default,
        serialize_with = "ser_nil_vec",
        deserialize_with = "de_nil_vec"
    )]
    pub trials: Vec<Trial>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub best: String,
    #[serde(default, serialize_with = "ser_go_f64")]
    pub gain: f64,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub set: BTreeMap<String, String>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub old: BTreeMap<String, String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub changes: Vec<String>,
    #[serde(default)]
    pub placement: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub partial: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub skipped: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub notes: Vec<String>,
    #[serde(default)]
    pub opt_in: bool,
    #[serde(default)]
    pub at: i64,
}

/// `tuneTracker` : la progression, lue par l'interface.
pub struct Tracker {
    inner: Mutex<TrackerState>,
}

struct TrackerState {
    phase: String,
    started: Instant,
    /// Fin de la mesure ; `None` = en cours.
    ended: Option<Instant>,
    trials: Vec<Trial>,
    avg_sec: f64,
    left: i64,
    budget: Duration,
}

impl Tracker {
    fn new() -> Self {
        Tracker {
            inner: Mutex::new(TrackerState {
                phase: String::new(),
                started: Instant::now(),
                ended: None,
                trials: Vec::new(),
                avg_sec: 0.0,
                left: 0,
                budget: Duration::ZERO,
            }),
        }
    }

    fn lock(&self) -> MutexGuard<'_, TrackerState> {
        self.inner.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn set_phase(&self, phase: impl Into<String>) {
        self.lock().phase = phase.into();
    }

    /// `put` : `i` hors des bornes ajoute ; rend l'index.
    fn put(&self, i: Option<usize>, t: Trial) -> usize {
        let mut st = self.lock();
        match i {
            Some(i) if i < st.trials.len() => {
                st.trials[i] = t;
                i
            }
            _ => {
                st.trials.push(t);
                st.trials.len() - 1
            }
        }
    }

    fn snapshot(&self) -> Map<String, Value> {
        let st = self.lock();
        let end = st.ended.unwrap_or_else(Instant::now);
        let el = end.saturating_duration_since(st.started);
        let mut eta = -1.0;
        if st.avg_sec > 0.0 {
            let rest = st.budget.as_secs_f64() - el.as_secs_f64();
            eta = (st.avg_sec * st.left as f64).min(rest).max(0.0);
        }
        let mut m = Map::new();
        m.insert("phase".into(), json!(st.phase));
        m.insert("elapsed_sec".into(), go_f64(el.as_secs_f64()));
        m.insert("eta_sec".into(), go_f64(eta));
        m.insert(
            "trials".into(),
            if st.trials.is_empty() {
                Value::Null
            } else {
                serde_json::to_value(&st.trials).unwrap_or(Value::Null)
            },
        );
        m
    }
}

/// Un flottant écrit comme le Go (entier sans « .0 »).
fn go_f64(v: f64) -> Value {
    if v.is_finite() && v.fract() == 0.0 && v.abs() < 1e15 {
        json!(v as i64)
    } else {
        json!(v)
    }
}

// ---------------------------------------------------------------------------
// Le job
// ---------------------------------------------------------------------------

#[derive(Default)]
struct State {
    running: bool,
    /// Entre le contrôle et la prise du bail : un second lancement est refusé.
    starting: bool,
    tracker: Option<Arc<Tracker>>,
    cancel: Option<watch::Sender<bool>>,
    result: Option<TuneResult>,
    err: String,
    applying: bool,
    apply_log: Vec<String>,
    apply_msg: String,
    apply_err: String,
}

/// L'optimiseur de la façade.
pub struct Tune {
    home: PathBuf,
    /// Le « loki » du Go, lancé en `serve` pour chaque essai.
    go_bin: PathBuf,
    pub lock: LockFile,
    state: Mutex<State>,
    effort: EffortFallbacks,
    client: reqwest::Client,
    builds: Mutex<HashMap<String, i64>>,
    /// Chargement d'un essai, et attente du moteur après une application.
    pub load_timeout: Duration,
    /// Entre deux sondes de /health d'un essai (1 s).
    pub poll: Duration,
    /// Entre deux sondes du moteur après une application (2 s).
    pub probe_poll: Duration,
    /// Sans nvidia-smi, la pause qui laisse la mémoire revenir après un essai
    /// (2 s).
    pub settle: Duration,
    /// Variables ajoutées aux essais (les tests).
    pub trial_env: Vec<(String, String)>,
    /// Durée du bail du moteur (renouvelé au tiers).
    pub lease_ttl: Duration,
    /// nvidia-smi (un faux dans les tests).
    pub nvidia_smi: String,
    /// Entre deux lectures du bilan de VRAM (500 ms).
    pub gpu_pause: Duration,
    /// Unité de `budget_min` (une minute ; plus court dans les tests).
    pub budget_unit: Duration,
}

impl Tune {
    pub fn new(home: PathBuf, go_bin: PathBuf) -> Self {
        let lock = LockFile::new(&home);
        Self::with_lock(home, go_bin, lock)
    }

    pub fn with_lock(home: PathBuf, go_bin: PathBuf, lock: LockFile) -> Self {
        crate::chat::engine::install_crypto();
        Tune {
            home,
            go_bin,
            lock,
            state: Mutex::new(State::default()),
            effort: EffortFallbacks::new(),
            client: reqwest::Client::builder().build().unwrap_or_default(),
            builds: Mutex::new(HashMap::new()),
            load_timeout: LOAD_TIMEOUT,
            poll: Duration::from_secs(1),
            probe_poll: Duration::from_secs(2),
            settle: Duration::from_secs(2),
            trial_env: Vec::new(),
            lease_ttl: LEASE_TTL,
            nvidia_smi: "nvidia-smi".into(),
            gpu_pause: Duration::from_millis(500),
            budget_unit: Duration::from_secs(60),
        }
    }

    /// `gpuStats` : lecture FRAÎCHE, vide sans nvidia-smi.
    async fn gpus(&self) -> Vec<crate::sysinfo::GpuStat> {
        crate::sysinfo::nvidia_smi_query_bin(&self.nvidia_smi)
            .await
            .map(|o| crate::sysinfo::parse_nvidia_smi(&String::from_utf8_lossy(&o)))
            .unwrap_or_default()
    }

    /// `gpuUsedSettled`.
    async fn gpu_settled(&self, before: i64) -> i64 {
        crate::enginectl::gpu_settle(
            before,
            || async {
                let g = self.gpus().await;
                (crate::enginectl::gpu_used_mb(&g), !g.is_empty())
            },
            self.gpu_pause,
        )
        .await
    }

    fn st(&self) -> MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn effort<'a>(&'a self, ctx: &'a Ctx) -> &'a EffortFallbacks {
        match &ctx.chat {
            Some(c) => &c.shared.effort,
            None => &self.effort,
        }
    }

    /// `tuneStart` : le statut HTTP et la réponse.
    pub async fn start(self: &Arc<Self>, ctx: &Ctx, mut opts: Opts) -> (StatusCode, Value) {
        let fail = |code: StatusCode, e: &str| (code, json!({ "ok": false, "error": e }));
        if let Err(e) = self.preflight(ctx).await {
            let code = if crate::engstatus::is_external(&ctx.store.config().await) {
                StatusCode::BAD_REQUEST
            } else {
                StatusCode::CONFLICT
            };
            return fail(code, &e);
        }
        {
            let mut st = self.st();
            if st.running || st.applying || st.starting {
                return fail(StatusCode::CONFLICT, ERR_TUNE_BUSY);
            }
            st.starting = true;
        }
        // Le verrou de la discussion : un message, une tâche, une compaction
        // reçoivent « optimisation en cours » ; le stop du chat annule.
        let lease = match EngineLease::acquire_for(ctx.store.clone(), self.lease_ttl, "tune").await
        {
            Ok(l) => l,
            Err(e) => {
                self.st().starting = false;
                let msg = match e {
                    LeaseError::Busy { error, .. } => error,
                    LeaseError::Other(e) => e,
                };
                return fail(StatusCode::CONFLICT, &msg);
            }
        };
        let (tx, rx) = watch::channel(false);
        let tracker = Arc::new(Tracker::new());
        {
            let mut st = self.st();
            st.starting = false;
            st.running = true;
            st.tracker = Some(tracker.clone());
            st.cancel = Some(tx);
            st.result = None;
            st.err.clear();
            st.apply_log.clear();
            st.apply_msg.clear();
            st.apply_err.clear();
        }
        opts.via = "web".into();
        let (me, ctx2) = (self.clone(), ctx.clone());
        tokio::spawn(async move { me.job(ctx2, opts, tracker, rx, lease).await });
        (StatusCode::ACCEPTED, json!({ "ok": true }))
    }

    async fn job(
        self: Arc<Self>,
        ctx: Ctx,
        opts: Opts,
        tracker: Arc<Tracker>,
        rx: watch::Receiver<bool>,
        mut lease: EngineLease,
    ) {
        let run = self.run_tune(&ctx, opts, &tracker, rx.clone());
        tokio::pin!(run);
        let mut interrupted = false;
        let res = loop {
            tokio::select! {
                r = &mut run => break r,
                s = lease.interrupted(), if !interrupted => {
                    interrupted = true;
                    if matches!(s, LeaseSignal::Stop | LeaseSignal::Lost)
                        && let Some(c) = &self.st().cancel
                    {
                        let _ = c.send(true);
                    }
                }
            }
        };
        tracker.lock().ended = Some(Instant::now());
        lease.release().await;
        let canceled = *rx.borrow();
        let mut st = self.st();
        st.running = false;
        st.cancel = None;
        match res {
            Ok(r) => st.result = Some(r),
            Err(e) => {
                st.result = None;
                st.err = if canceled {
                    "optimisation annulée".into()
                } else {
                    e
                };
            }
        }
    }

    /// `tuneStatus`.
    pub fn status(&self) -> Value {
        let st = self.st();
        let mut m = Map::new();
        m.insert("ok".into(), json!(true));
        m.insert("running".into(), json!(st.running));
        m.insert("applying".into(), json!(st.applying));
        if let Some(t) = &st.tracker {
            m.extend(t.snapshot());
        }
        if !st.running {
            if let Some(r) = &st.result {
                m.insert(
                    "result".into(),
                    serde_json::to_value(r).unwrap_or(Value::Null),
                );
            }
            if !st.err.is_empty() {
                m.insert("error".into(), json!(st.err));
            }
        }
        if !st.apply_log.is_empty() || !st.apply_msg.is_empty() || !st.apply_err.is_empty() {
            let log = if st.apply_log.is_empty() {
                Value::Null
            } else {
                json!(st.apply_log)
            };
            m.insert(
                "apply".into(),
                json!({ "log": log, "message": st.apply_msg, "error": st.apply_err }),
            );
        }
        // Une optimisation lancée ailleurs (« loki tune » en ligne de commande).
        if !st.running
            && !st.applying
            && let Some(o) = self.lock.active()
        {
            m.insert("elsewhere".into(), json!(o.via));
        }
        Value::Object(m)
    }

    /// `handleTuneCancel`.
    pub fn cancel(&self) -> bool {
        let st = self.st();
        match (&st.cancel, st.running) {
            (Some(c), true) => {
                let _ = c.send(true);
                true
            }
            _ => false,
        }
    }

    // -----------------------------------------------------------------------
    // Ce que le Go prête
    // -----------------------------------------------------------------------

    /// `/internal/v1/tune/context` : refus du processus web, moteur actif,
    /// télémétrie.
    async fn go_context(ctx: &Ctx) -> Value {
        ctx.store
            .internal_json(Method::GET, "/internal/v1/tune/context", None)
            .await
            .unwrap_or(Value::Null)
    }

    /// `engineNeedsStop` : le moteur de la façade si elle le pilote, sinon le
    /// Go.
    async fn main_active(ctx: &Ctx) -> bool {
        if ctx.engine_ctl.is_some() {
            return crate::enginectl::service_is_active(ctx);
        }
        Self::go_context(ctx).await["main_active"].as_bool() == Some(true)
    }

    /// `serviceActionOS` : démarrage ou arrêt SANS le refus de l'optimiseur —
    /// c'est nous qui tenons le verrou.
    async fn svc_os(ctx: &Ctx, action: &str) -> Result<(), String> {
        if let Some(ctl) = &ctx.engine_ctl {
            return ctl.run(action).await;
        }
        let (code, v) = ctx
            .store
            .internal_post(
                "/internal/v1/service/action",
                &json!({ "action": action, "os": true }),
            )
            .await?;
        if !code.is_success() {
            return Err(v
                .get("error")
                .and_then(Value::as_str)
                .map(str::to_string)
                .unwrap_or_else(|| format!("service {action} : {code}")));
        }
        match v.get("error").and_then(Value::as_str) {
            Some(e) if !e.is_empty() => Err(e.to_string()),
            _ => Ok(()),
        }
    }

    /// `preflightEngine`.
    async fn preflight_engine(ctx: &Ctx) -> Result<(), String> {
        let cfg = ctx.store.config().await;
        let d = crate::models::model_dirs(ctx).await;
        tokio::task::spawn_blocking(move || {
            crate::enginectl::preflight_engine(&d.home, &cfg, &d.stored, d.env.as_deref())
        })
        .await
        .unwrap_or_else(|e| Err(e.to_string()))
    }

    /// La clé du moteur local (`effectiveAPIKey`), pour les essais comme
    /// pour le vrai moteur.
    async fn engine_key(ctx: &Ctx) -> String {
        ctx.store
            .internal_json(
                Method::GET,
                "/internal/v1/oai/route?method=POST&path=%2Fv1%2Fchat%2Fcompletions",
                None,
            )
            .await
            .ok()
            .and_then(|v| v.get("key").and_then(Value::as_str).map(str::to_string))
            .unwrap_or_default()
    }

    fn engine(&self, base: String, key: String, cancel: watch::Receiver<bool>) -> Engine {
        Engine {
            base,
            key,
            client: self.client.clone(),
            cancel,
            req_timeout: bench::REQ_TIMEOUT,
            served: Arc::new(AtomicU64::new(0)),
        }
    }

    // -----------------------------------------------------------------------
    // Refus d'entrée
    // -----------------------------------------------------------------------

    /// `tuneActivePreset` : le preset dont l'empreinte est celle de la
    /// configuration active.
    async fn active_preset(
        &self,
        ctx: &Ctx,
        cfg: &Cfg,
    ) -> Result<(String, String, String), String> {
        let order = crate::presets::parse_preset_order(
            ctx.store
                .get(BK_STATE, crate::presets::PRESET_ORDER_KEY)
                .await
                .ok()
                .flatten()
                .as_deref(),
        );
        let list = crate::presets::list_presets(&self.home, cfg, &order)?;
        match list.into_iter().find(|p| p.active) {
            Some(p) => {
                let content = crate::presets_api::read_preset(&self.home, &p.id)?;
                Ok((p.id, p.name, content))
            }
            None => Err("aucun preset ne correspond à la configuration active : enregistre-la comme preset (ou bascule sur l'un d'eux) avant d'optimiser".into()),
        }
    }

    /// `tunePreflight` (processus web).
    async fn preflight(&self, ctx: &Ctx) -> Result<(), String> {
        let cfg = ctx.store.config().await;
        if crate::engstatus::is_external(&cfg) {
            return Err("optimisation indisponible : le preset actif est une API externe".into());
        }
        if crate::strata::is_strata(&cfg) {
            return Err("optimisation indisponible : le preset actif tourne sur Strata, réglé par sa propre fenêtre".into());
        }
        if self.lock.active().is_some() {
            return Err(ERR_TUNE_BUSY.into());
        }
        self.active_preset(ctx, &cfg).await?;
        let gc = Self::go_context(ctx).await;
        if let Some(b) = gc["busy"].as_str().filter(|b| !b.is_empty()) {
            return Err(b.to_string());
        }
        if crate::enginectl::generating(ctx).await {
            return Err("génération en cours".into());
        }
        let active = match &ctx.engine_ctl {
            Some(_) => crate::enginectl::service_is_active(ctx),
            None => gc["main_active"].as_bool() == Some(true),
        };
        if active {
            let (_tx, rx) = watch::channel(false);
            let e = self.engine(
                format!("http://localhost:{}", crate::engstatus::llm_port_raw(&cfg)),
                Self::engine_key(ctx).await,
                rx,
            );
            if e.idle().await.is_err() {
                return Err("le moteur traite une requête (discussion, tâche ou client /v1) : réessaie une fois libre".into());
            }
        }
        Ok(())
    }

    // -----------------------------------------------------------------------
    // Le verrou : ménage d'un verrou périmé
    // -----------------------------------------------------------------------

    /// `tuneReapStale` : l'essai orphelin d'un verrou périmé est arrêté
    /// (identité complète vérifiée), une application interrompue défaite.
    /// Rend `MainWasActive` du verrou écarté.
    async fn reap_stale(&self, ctx: &Ctx) -> bool {
        let Some(o) = self.lock.reap_claim() else {
            return false;
        };
        if let Some(t) = &o.trial
            && self.lock.proc_alive(t)
        {
            eprintln!(
                "[loki tune] essai orphelin (PID {}) d'une optimisation interrompue : arrêt",
                t.pid
            );
            let pid = t.pid;
            let _ = tokio::task::spawn_blocking(move || {
                lock::kill_tree(pid, &|| !lock::pid_alive(pid));
            })
            .await;
        }
        self.undo_apply(ctx, &o).await;
        o.main_was_active
    }

    /// `tuneUndoApply` : une application interrompue avant sa vérification
    /// remet la version d'avant du preset.
    async fn undo_apply(&self, ctx: &Ctx, o: &Owner) {
        if o.phase != "application" || o.backup.is_empty() || o.preset_id.is_empty() {
            return;
        }
        let b = match std::fs::read(&o.backup) {
            Ok(b) => String::from_utf8_lossy(&b).into_owned(),
            Err(e) => {
                eprintln!("[loki tune] application interrompue : sauvegarde illisible ({e})");
                return;
            }
        };
        if let Err(e) =
            crate::presets_api::save_applying(ctx, &o.preset_id, &o.preset_name, &b, "apply").await
        {
            eprintln!(
                "[loki tune] application interrompue : retour impossible ({e}) — l'ancienne version est dans {}",
                o.backup
            );
            return;
        }
        eprintln!(
            "[loki tune] application interrompue avant sa vérification : ancienne version du preset rétablie"
        );
    }

    async fn acquire(&self, ctx: &Ctx, via: &str, main_active: bool) -> Result<Held<'_>, String> {
        self.lock
            .acquire(via, main_active, || self.reap_stale(ctx))
            .await
    }

    // -----------------------------------------------------------------------
    // Le déroulé
    // -----------------------------------------------------------------------

    /// `runTune` : le verrou, l'arrêt et la relance du vrai moteur sont faits
    /// ici.
    pub async fn run_tune(
        &self,
        ctx: &Ctx,
        mut opts: Opts,
        tr: &Tracker,
        cancel: watch::Receiver<bool>,
    ) -> Result<TuneResult, String> {
        if opts.budget.is_zero() {
            opts.budget = DEFAULT_BUDGET;
        }
        opts.budget = opts.budget.min(MAX_BUDGET);
        {
            let mut st = tr.lock();
            st.started = Instant::now();
            st.budget = opts.budget;
        }
        let cfg = ctx.store.config().await;
        let preset = self.active_preset(ctx, &cfg).await?;
        let main_active = Self::main_active(ctx).await;
        let held = self.acquire(ctx, &opts.via, main_active).await?;
        let mut out = self
            .run_held(ctx, &opts, tr, cancel, &held, &cfg, preset)
            .await;
        // Le vrai moteur revient AVANT que le verrou ne soit rendu : personne
        // ne le relance par-dessus un essai.
        if main_active {
            held.set_phase("relance");
            tr.set_phase("relance du moteur principal");
            let mut failed = |msg: String| {
                eprintln!("[loki tune] {msg}");
                match &mut out {
                    Ok(r) => r.notes.push(format!("⚠️ {msg}")),
                    Err(e) => *e = format!("{e} — {msg}"),
                }
            };
            match Self::preflight_engine(ctx).await {
                Err(e) => failed(format!(
                    "relance du moteur principal impossible : {}",
                    crate::enginectl::plain_err(&e)
                )),
                Ok(()) => {
                    if let Err(e) = Self::svc_os(ctx, "start").await {
                        failed(format!(
                            "relance du moteur principal : {}",
                            crate::enginectl::plain_err(&e)
                        ));
                    }
                }
            }
            crate::sysinfo::GPU_MONITOR.invalidate().await;
        }
        held.release();
        out
    }

    #[allow(clippy::too_many_arguments)]
    async fn run_held(
        &self,
        ctx: &Ctx,
        opts: &Opts,
        tr: &Tracker,
        cancel: watch::Receiver<bool>,
        held: &Held<'_>,
        cfg: &Cfg,
        (preset_id, preset_name, content): (String, String, String),
    ) -> Result<TuneResult, String> {
        let canceled = || *cancel.borrow();
        let run_dir = self.home.join("tune").join("run");
        let _ = std::fs::remove_dir_all(&run_dir);
        mkdir_private(&run_dir).map_err(|e| e.to_string())?;
        let mut t = Trialer {
            tune: self,
            dir: run_dir,
            seq: 0,
            held,
            key: Self::engine_key(ctx).await,
        };
        if held.snapshot().main_was_active {
            tr.set_phase("arrêt du moteur principal");
            let gpus = self.gpus().await;
            let before = crate::enginectl::gpu_used_mb(&gpus);
            if let Err(e) = Self::svc_os(ctx, "stop").await {
                return Err(format!(
                    "arrêt du moteur : {}",
                    crate::enginectl::plain_err(&e)
                ));
            }
            crate::enginectl::dictate_stop(ctx).await;
            if !gpus.is_empty() {
                self.gpu_settled(before).await;
            }
            crate::sysinfo::GPU_MONITOR.invalidate().await;
        }
        let perf: Vec<tc::PerfRec> =
            serde_json::from_value(Self::go_context(ctx).await["perf"].clone()).unwrap_or_default();
        let mut res = TuneResult {
            preset_id,
            preset_name,
            base_fp: crate::presets::config_fingerprint(cfg),
            preset_fp: crate::presets::preset_fingerprint(&content),
            opt_in: opts.opt_in,
            at: unix_secs(),
            weights: tc::weights_from(&perf),
            ..Default::default()
        };

        // Référence : la ligne composée telle quelle, la sonde de la machine.
        tr.set_phase("référence : composition de la ligne de commande");
        let (base_dir, base_port) = t.prepare(cfg)?;
        let base_launch = t
            .dry(&cancel, &base_dir)
            .await
            .map_err(|e| format!("la configuration active ne se lance pas : {e}"))?;
        let probe = base_launch.probe.clone().unwrap_or_default();
        let smi = self.gpus().await;
        let mut env = tc::Env {
            help: probe.help.clone(),
            gpus: probe.gpus,
            vram_mib: probe.vram_mib,
            model_bytes: probe.model_bytes,
            moe: probe.moe,
            phys_cores: phys_cores(&probe),
            smi: !smi.is_empty(),
            arg_env: probe.arg_env.clone(),
            user_env: probe.user_env.clone(),
            queues_env: probe.queues_env,
            placement: opts.placement,
            opt_in: opts.opt_in,
        };
        let visible = tc::visible_gpus(cfg);
        if env.vram_mib == 0 && env.smi {
            for (i, g) in smi.iter().enumerate() {
                if visible.is_empty() || visible.contains(&(i as i64)) {
                    env.vram_mib += g.total;
                }
            }
        }
        if env.gpus == 0 && env.smi {
            env.gpus = if visible.is_empty() {
                smi.len() as i64
            } else {
                visible.len() as i64
            };
        }
        let extra = crate::models::split_args(cfg.get("EXTRA_ARGS").map_or("", String::as_str));
        let (kt, vt, _) = crate::serve::fidelity::effective_kv_types(cfg, &extra, &env.arg_env);
        let (label, quant) = bench::kv_label(&kt, &vt);
        if quant {
            res.notes.push(format!(
                "cache KV {label} quantifié : zone grise, conservé tel quel (jamais modifié par l'optimiseur)"
            ));
        }
        if !env.smi {
            res.notes.push("VRAM libre non mesurable (nvidia-smi absent) : ni micro-lot plus grand ni marges --fit essayés".into());
        }
        let (_, total) = crate::sysinfo::ram_usage_mb();
        if total > 0 && env.model_bytes > total << 20 {
            res.notes.push("modèle plus gros que la RAM : chaque essai le relit depuis le disque — prévoir un budget plus long, ou ne garder que quelques étapes".into());
        }
        let mut reserve = tc::TIGHT_MIB;
        if !cfg.get("MMPROJ").map_or("", |s| s.trim()).is_empty() {
            reserve += tc::VISION_RESERVE_MIB;
        }
        let cpu_placed = !crate::serve::queues::tensor_override(&extra, &env.arg_env).is_empty();
        let hybrid = crate::bench::job::model_hybrid(ctx, cfg).await;

        // La référence se mesure deux fois : son écart entre passages est le
        // bruit que tout gain doit dépasser.
        tr.set_phase("référence : chargement");
        let mut bt = Trial {
            stage: "référence".into(),
            label: "configuration actuelle".into(),
            status: "en cours".into(),
            headroom: -1,
            ..Default::default()
        };
        let bi = tr.put(None, bt.clone());
        let t0 = Instant::now();
        let (base, mut stop) = t.load(&cancel, &base_dir, base_port, &base_launch).await;
        let base = match base {
            Ok(b) => b,
            Err(e) => {
                stop.stop().await;
                return Err(format!("la référence ne charge pas : {e}"));
            }
        };
        res.engine = base.build.clone();
        res.n_ctx = base.n_ctx;
        res.slots = base.slots;
        res.engine_build = build_number(&base.build);
        // Une profondeur pour tous : celle que la référence peut tenir avec le
        // plus gros préchauffage essayé (2 × 4096 jetons, au plus un quart du
        // contexte).
        let warm_max = (2 * 4096).min(base.n_ctx / 4).max(64) + bench::WARM_GEN;
        res.depth = bench::depth_for(base.n_ctx, cpu_placed, warm_max).0;
        let m = Measure {
            tune: self,
            ctx,
            tr,
            env: &env,
            cpu_placed,
            hybrid,
            weights: res.weights.clone(),
            bopts: bench::Opts {
                mode: bench::MODE_FULL.into(),
                prompt: 2000,
                predict: 300,
                depth: res.depth,
            },
            corp: Corpora::default_set(),
        };
        let headroom = || async {
            if !env.smi {
                return (-1, false);
            }
            let g: Vec<(i64, i64)> = self
                .gpus()
                .await
                .iter()
                .map(|g| (g.total, g.used))
                .collect();
            match tc::headroom(&g, &visible) {
                Some(free) => (free, free < reserve),
                None => (-1, false),
            }
        };
        let (base_runs, err) = m.measure(cfg, &base, 2, "référence").await;
        let (hr, _) = headroom().await;
        stop.stop().await;
        if let Some(e) = err {
            return Err(format!("mesure de la référence : {e}"));
        }
        bt.status = "mesuré".into();
        bt.turn_sec = turn_mean(&base_runs);
        bt.runs = base_runs.clone();
        bt.headroom = hr;
        bt.sec = t0.elapsed().as_secs_f64();
        if base.offloaded >= 0 {
            bt.offloaded = format!("{}/{}", base.offloaded, base.layers);
        }
        res.baseline = bt.clone();
        tr.put(Some(bi), bt.clone());
        if hr >= 0 && hr < reserve {
            res.notes.push(format!(
                "la configuration actuelle laisse déjà moins de {reserve} Mio libres sur une carte"
            ));
        }

        let (mut best, mut best_runs, mut best_label) =
            (cfg.clone(), base_runs.clone(), String::new());
        let mut durations = vec![bt.sec];
        let mut measured: HashMap<String, String> =
            HashMap::from([(tc::launch_key(&base_launch), bt.label.clone())]);
        let started = tr.lock().started;
        let deadline = started + opts.budget;

        'stages: for (si, (name, stage_gen)) in tc::STAGES.iter().enumerate() {
            if !tc::stage_wanted(name, &opts.stages) {
                res.skipped.push(format!("{name} : non demandé"));
                continue;
            }
            let (cands, why) = stage_gen(&best, &env);
            if !why.is_empty() {
                res.skipped.push(format!("{name} : {why}"));
                continue;
            }
            for (ci, c) in cands.iter().enumerate() {
                // Ce qui reste à faire, pour l'estimation : les essais
                // restants de l'étape, plus ceux des étapes suivantes vus
                // depuis la meilleure configuration du moment.
                let mut left = (cands.len() - ci) as i64;
                for (nx, ngen) in &tc::STAGES[si + 1..] {
                    if tc::stage_wanted(nx, &opts.stages) {
                        left += ngen(&best, &env).0.len() as i64;
                    }
                }
                let avg = tc::mean(&durations);
                {
                    let mut st = tr.lock();
                    st.avg_sec = avg;
                    st.left = left;
                }
                if canceled() {
                    res.partial = "annulé".into();
                    break 'stages;
                }
                let until = deadline.saturating_duration_since(Instant::now());
                if until.as_secs_f64() < avg {
                    res.partial = format!(
                        "budget de {} atteint : {left} essai(s) non faits",
                        bench::go_dur(round_minute(opts.budget))
                    );
                    break 'stages;
                }
                let mut trial = Trial {
                    stage: c.stage.clone(),
                    label: c.label.clone(),
                    status: "en cours".into(),
                    headroom: -1,
                    changes: tc::diff(cfg, &c.cfg),
                    ..Default::default()
                };
                let idx = tr.put(None, trial.clone());
                let mut finish = |trial: &mut Trial, status: &str, why: String| {
                    trial.status = status.into();
                    trial.why = why;
                    tr.put(Some(idx), trial.clone());
                    res.trials.push(trial.clone());
                };
                if let Err(e) = tc::cfg_check(cfg, &c.cfg, opts.opt_in) {
                    finish(&mut trial, "écarté", format!("dénature : {e}"));
                    continue;
                }
                tr.set_phase(format!("{name} — {} : composition", c.label));
                let (dir, port) = match t.prepare(&c.cfg) {
                    Ok(x) => x,
                    Err(e) => {
                        finish(&mut trial, "échec", e);
                        continue;
                    }
                };
                let launch = match t.dry(&cancel, &dir).await {
                    Ok(l) => l,
                    Err(e) => {
                        if canceled() {
                            finish(&mut trial, "échec", "annulé".into());
                            res.partial = "annulé".into();
                            break 'stages;
                        }
                        finish(&mut trial, "écarté", format!("refusé au lancement : {e}"));
                        continue;
                    }
                };
                let why = tc::denature(&base_launch, &launch, opts.opt_in);
                if !why.is_empty() {
                    finish(&mut trial, "écarté", format!("dénature : {why}"));
                    continue;
                }
                let key = tc::launch_key(&launch);
                if let Some(prev) = measured.get(&key) {
                    let prev = prev.clone();
                    finish(
                        &mut trial,
                        "doublon",
                        format!("même ligne de commande que « {prev} »"),
                    );
                    continue;
                }
                measured.insert(key, c.label.clone());
                let t1 = Instant::now();
                tr.set_phase(format!("{name} — {} : chargement", c.label));
                let (l, mut stop) = t.load(&cancel, &dir, port, &launch).await;
                let l = match l {
                    Ok(l) => l,
                    Err(e) => {
                        stop.stop().await;
                        if canceled() {
                            finish(&mut trial, "échec", "annulé".into());
                            res.partial = "annulé".into();
                            break 'stages;
                        }
                        finish(&mut trial, "échec", e);
                        durations.push(t1.elapsed().as_secs_f64());
                        continue;
                    }
                };
                trial.sec = t1.elapsed().as_secs_f64();
                if l.offloaded >= 0 {
                    trial.offloaded = format!("{}/{}", l.offloaded, l.layers);
                }
                // Contre-vérification à l'exécution : même contexte, mêmes
                // slots, mêmes types de cache que la référence.
                if l.n_ctx != base.n_ctx || l.slots != base.slots {
                    stop.stop().await;
                    finish(
                        &mut trial,
                        "écarté",
                        format!(
                            "dénature : contexte {} × {} slot(s) au lieu de {} × {}",
                            l.n_ctx, l.slots, base.n_ctx, base.slots
                        ),
                    );
                    continue;
                }
                if !base.kv.is_empty() && !l.kv.is_empty() && l.kv != base.kv {
                    stop.stop().await;
                    finish(
                        &mut trial,
                        "écarté",
                        format!("dénature : cache KV {} au lieu de {}", l.kv, base.kv),
                    );
                    continue;
                }
                let fit_regressed =
                    base.offloaded >= 0 && l.offloaded >= 0 && l.offloaded < base.offloaded;
                let phase = format!("{name} — {}", c.label);
                let (mut runs, mut err) = m.measure(&c.cfg, &l, 1, &phase).await;
                if err.is_none()
                    && runs.len() == 1
                    && tc::worth_repeat(&turn_secs(&best_runs), runs[0].turn_sec)
                    && !fit_regressed
                {
                    let (more, e) = m
                        .measure(&c.cfg, &l, 1, &format!("{phase} (confirmation)"))
                        .await;
                    runs.extend(more);
                    err = e;
                }
                let (hr, tight) = headroom().await;
                stop.stop().await;
                trial.sec = t1.elapsed().as_secs_f64();
                durations.push(trial.sec);
                trial.runs = runs.clone();
                trial.headroom = hr;
                if let Some(e) = err {
                    if canceled() {
                        finish(&mut trial, "échec", "annulé".into());
                        res.partial = "annulé".into();
                        break 'stages;
                    }
                    finish(&mut trial, "échec", format!("mesure : {e}"));
                    continue;
                }
                trial.turn_sec = turn_mean(&runs);
                let (gain, need, wins) = tc::wins(&turn_secs(&best_runs), &turn_secs(&runs));
                trial.gain = gain;
                let (prose_best, prose_cand) = (prose_mean(&best_runs), prose_mean(&runs));
                if fit_regressed {
                    finish(
                        &mut trial,
                        "écarté",
                        format!(
                            "fit en recul : {} couches déportées au lieu de {} (performance)",
                            l.offloaded, base.offloaded
                        ),
                    );
                } else if tight {
                    finish(
                        &mut trial,
                        "écarté",
                        format!("trop juste : {hr} Mio libres sur une carte (< {reserve})"),
                    );
                } else if prose_best > 0.0 && prose_cand < 0.95 * prose_best {
                    finish(
                        &mut trial,
                        "mesuré",
                        format!(
                            "decode en prose plus lent de {:.0} %",
                            (1.0 - prose_cand / prose_best) * 100.0
                        ),
                    );
                } else if wins {
                    best = c.cfg.clone();
                    best_runs = runs.clone();
                    best_label = c.label.clone();
                    if c.stage == "placement" {
                        res.placement = true;
                    }
                    finish(
                        &mut trial,
                        "retenu",
                        format!(
                            "−{:.1} % par tour (seuil {:.1} %)",
                            gain * 100.0,
                            need * 100.0
                        ),
                    );
                } else if runs.len() < 2 && gain > 0.0 {
                    finish(
                        &mut trial,
                        "mesuré",
                        format!(
                            "−{:.1} %, sous le seuil de {:.0} %",
                            gain * 100.0,
                            tc::MIN_GAIN * 100.0
                        ),
                    );
                } else {
                    let mut delta = format!("{:+.1} %", -gain * 100.0);
                    if gain * 1000.0 > -0.5 && gain * 1000.0 < 0.5 {
                        delta = "≈ 0 %".into();
                    }
                    finish(
                        &mut trial,
                        "mesuré",
                        format!("{delta} (seuil {:.1} %)", need * 100.0),
                    );
                }
            }
        }
        tr.lock().left = 0;

        res.best = best_label.clone();
        if !best_label.is_empty() {
            let mb = turn_mean(&base_runs);
            res.gain = (mb - turn_mean(&best_runs)) / mb;
            res.changes = tc::diff(cfg, &best);
            for k in cfg.keys().chain(best.keys()) {
                let (a, b) = (
                    cfg.get(k).map_or("", String::as_str),
                    best.get(k).map_or("", String::as_str),
                );
                if a != b {
                    res.set.insert(k.clone(), b.to_string());
                    res.old.insert(k.clone(), a.to_string());
                }
            }
        }
        if canceled() && res.partial.is_empty() {
            res.partial = "annulé".into();
        }
        save_result(ctx, &res).await;
        Ok(res)
    }

    // -----------------------------------------------------------------------
    // Application
    // -----------------------------------------------------------------------

    /// `tunePlanApply` : le contenu du preset réglé, sans rien écrire.
    /// Refusé si le preset a changé depuis la mesure.
    pub fn plan_apply(&self, res: &TuneResult) -> Result<(String, String), String> {
        if res.set.is_empty() {
            return Err("rien à appliquer : la configuration actuelle reste la meilleure".into());
        }
        let content = crate::presets_api::read_preset(&self.home, &res.preset_id)
            .map_err(|e| format!("preset « {} » introuvable : {e}", res.preset_name))?;
        if crate::presets::preset_fingerprint(&content) != res.preset_fp {
            return Err("le preset a changé depuis la mesure : relance l'optimisation".into());
        }
        let base = crate::presets::parse_env(&content);
        let mut win = base.clone();
        for (k, v) in &res.set {
            if v.is_empty() {
                win.remove(k);
            } else {
                win.insert(k.clone(), v.clone());
            }
        }
        tc::cfg_check(&base, &win, res.opt_in)?;
        let patched = tc::patch_preset(&content, &base, &win)?;
        Ok((content, patched))
    }

    /// `tuneApply` : dans une COPIE du preset, ou dans le preset lui-même —
    /// encore actif ; le moteur redémarre alors sur la nouvelle version, qui
    /// doit répondre, garder son contexte et ses slots, et passer une sonde ;
    /// sinon l'ancienne version est rétablie.
    pub async fn apply(
        &self,
        ctx: &Ctx,
        res: &TuneResult,
        target: &str,
        say: &(dyn Fn(String) + Send + Sync),
    ) -> Result<String, String> {
        let (content, patched) = self.plan_apply(res)?;
        match target {
            APPLY_COPY => {
                let name = format!("{} (optimisé)", res.preset_name);
                let id = crate::presets_api::save_preset(&self.home, "", &name, &patched)?;
                return Ok(format!(
                    "copie « {name} » créée ({id}) — bascule dessus pour l'utiliser"
                ));
            }
            APPLY_PRESET => {}
            t => return Err(format!("cible inconnue : {t}")),
        }
        let cur = ctx.store.config().await;
        if crate::engstatus::is_external(&cur) {
            return Err("le preset actif est une API externe".into());
        }
        if crate::presets::preset_fingerprint(&content) != crate::presets::config_fingerprint(&cur)
        {
            return Err("ce preset n'est plus celui en service : bascule dessus, ou enregistre le résultat dans une copie".into());
        }
        if crate::presets::config_fingerprint(&cur) != res.base_fp {
            return Err(
                "la configuration active a changé depuis la mesure : relance l'optimisation".into(),
            );
        }
        let held = self
            .acquire(ctx, "apply", Self::main_active(ctx).await)
            .await?;
        held.set_phase("application");
        let out = self
            .apply_held(ctx, res, &content, &patched, &held, say)
            .await;
        held.release();
        out
    }

    async fn apply_held(
        &self,
        ctx: &Ctx,
        res: &TuneResult,
        content: &str,
        patched: &str,
        held: &Held<'_>,
        say: &(dyn Fn(String) + Send + Sync),
    ) -> Result<String, String> {
        let backup = self.home.join("tune").join("backup").join(format!(
            "{}-{}.env",
            res.preset_id,
            unix_secs()
        ));
        if let Some(d) = backup.parent() {
            let _ = mkdir_private(d);
        }
        write_private(&backup, content.as_bytes())
            .map_err(|e| format!("sauvegarde du preset : {e}"))?;
        let backup = backup.to_string_lossy().into_owned();
        // Noté dans le verrou AVANT d'écrire : un Loki tué pendant
        // l'application la défait au redémarrage.
        held.set_backup(&backup, &res.preset_id, &res.preset_name);
        let backup = &backup;
        let revert = |why: String| async move {
            say(format!("échec ({why}) : retour à l'ancienne version"));
            if let Err(e) = crate::presets_api::save_applying(
                ctx,
                &res.preset_id,
                &res.preset_name,
                content,
                "apply",
            )
            .await
            {
                return Err(format!(
                    "{why} ; RETOUR IMPOSSIBLE ({e}) — l'ancienne version est dans {backup}"
                ));
            }
            if let Err(e) = Self::svc_os(ctx, "restart").await {
                return Err(format!(
                    "{why} ; ancienne version remise, redémarrage : {}",
                    crate::enginectl::plain_err(&e)
                ));
            }
            Err(format!("{why} : ancienne version rétablie"))
        };
        say(format!("écriture du preset (sauvegarde : {backup})"));
        let applied = match crate::presets_api::save_applying(
            ctx,
            &res.preset_id,
            &res.preset_name,
            patched,
            "apply",
        )
        .await
        {
            Ok((_, applied)) => applied,
            Err(e) => return revert(format!("écriture : {e}")).await,
        };
        if !applied {
            return revert("le preset n'a pas été appliqué à la configuration active".into()).await;
        }
        // Même configuration que l'essai retenu, à l'empreinte près.
        let why = applied_mismatch(&ctx.store.config().await, res);
        if !why.is_empty() {
            return revert(why).await;
        }
        say("redémarrage du moteur".into());
        if let Err(e) = Self::svc_os(ctx, "restart").await {
            return revert(format!("redémarrage : {}", crate::enginectl::plain_err(&e))).await;
        }
        if let Err(e) = self.post_apply_probe(ctx, res, say).await {
            return revert(e).await;
        }
        Ok(format!(
            "preset « {} » optimisé et vérifié",
            res.preset_name
        ))
    }

    /// `tunePostApplyProbe` : le moteur redémarré répond, garde le contexte
    /// et les slots mesurés, et tient un prompt à la profondeur de la mesure
    /// suivi d'un decode.
    async fn post_apply_probe(
        &self,
        ctx: &Ctx,
        res: &TuneResult,
        say: &(dyn Fn(String) + Send + Sync),
    ) -> Result<(), String> {
        say("attente du moteur".into());
        let cfg = ctx.store.config().await;
        let (_tx, rx) = watch::channel(false);
        let e = self.engine(
            format!("http://localhost:{}", crate::engstatus::llm_port_raw(&cfg)),
            Self::engine_key(ctx).await,
            rx,
        );
        let deadline = Instant::now() + self.load_timeout;
        // Un moteur mort au chargement (OOM) se voit vite : plus de service du
        // tout, trois fois de suite après les premières secondes.
        let (mut gone, t0) = (0, Instant::now());
        while !crate::engstatus::health_check(&ctx.store.config().await).await {
            if Instant::now() > deadline {
                return Err(format!(
                    "le moteur ne répond pas après {}",
                    bench::go_dur(self.load_timeout)
                ));
            }
            if t0.elapsed() > Duration::from_secs(10) && !Self::main_active(ctx).await {
                gone += 1;
                if gone >= 3 {
                    return Err("le moteur s'est arrêté au chargement".into());
                }
            } else {
                gone = 0;
            }
            tokio::time::sleep(self.probe_poll).await;
        }
        let (n_ctx, slots, _) = props(&e)
            .await
            .map_err(|e| format!("/props illisible : {e}"))?;
        if n_ctx != res.n_ctx || slots != res.slots {
            return Err(format!(
                "contexte {n_ctx} × {slots} slot(s) au lieu de {} × {}",
                res.n_ctx, res.slots
            ));
        }
        if res.depth <= 0 {
            return Ok(());
        }
        say(format!(
            "sonde : prompt de {} jetons puis decode",
            res.depth
        ));
        let cfg = ctx.store.config().await;
        let effort = self.effort(ctx);
        let mut s = bench::setup_from(&cfg, &HashMap::new(), effort);
        let code = Corpora::default_set().code;
        let (sample, _) = bench::slice(&code, 0, bench::SAMPLE_BYTES as i64);
        let ratio = e.bytes_per_token(sample).await;
        let (text, _) = bench::slice(&code, 0, (res.depth as f64 * ratio) as i64);
        let side = ctx
            .store
            .internal_post("/internal/v1/engine/sidejob", &json!({}))
            .await
            .ok()
            .and_then(|(_, v)| v.get("id").and_then(Value::as_i64));
        let learn = |want: &str, got: &str| {
            effort.remember(&cfg, want, got);
            log_effort_fallback(want, got);
        };
        let msgs = vec![(
            "user".to_string(),
            format!("{text}\n\n{}", bench::ASK_FIRST),
        )];
        let r = s.ask(&e, &msgs, 64, false, &learn).await;
        if let Some(id) = side {
            let _ = ctx
                .store
                .internal_post(
                    "/internal/v1/engine/sidejob",
                    &json!({ "end": id, "served": e.served.load(Ordering::SeqCst) > 0 }),
                )
                .await;
        }
        r.map(|_| ()).map_err(|e| format!("sonde : {e}"))
    }

    /// `tuneEngineBin` puis `engineBuildCached` : le build du moteur de la
    /// configuration, gardé tant que le binaire ne change pas.
    async fn engine_build(&self, cfg: &Cfg) -> i64 {
        let bin = cfg.get("BIN").map_or("", |s| s.trim());
        if bin.is_empty() {
            return 0;
        }
        let bin = if crate::paths::go_is_abs(bin) {
            bin.to_string()
        } else {
            crate::paths::go_join(&[&self.home.to_string_lossy(), bin])
        };
        let bin = crate::serve::run::EnginePaths {
            home: self.home.clone(),
        }
        .prebuilt_resolve_bin(&bin);
        let Ok(md) = std::fs::metadata(&bin) else {
            return 0;
        };
        let mtime = md
            .modified()
            .ok()
            .and_then(|t| t.duration_since(UNIX_EPOCH).ok())
            .map_or(0, |d| d.as_nanos());
        let key = format!("{bin}|{}|{mtime}", md.len());
        if let Some(b) = self
            .builds
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .get(&key)
        {
            return *b;
        }
        let (b, _) = crate::engineimg::oci::build_of(&bin).await;
        self.builds
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .insert(key, b);
        b
    }

    /// `handleTuneLast`.
    async fn last(&self, ctx: &Ctx, id: &str) -> Value {
        let Some(res) = load_result(ctx, id).await else {
            return json!({ "ok": false });
        };
        let mut out = Map::new();
        out.insert("ok".into(), json!(true));
        out.insert(
            "result".into(),
            serde_json::to_value(&res).unwrap_or(Value::Null),
        );
        if let Ok(content) = crate::presets_api::read_preset(&self.home, id) {
            out.insert(
                "preset_changed".into(),
                json!(crate::presets::preset_fingerprint(&content) != res.preset_fp),
            );
        }
        if res.engine_build > 0 {
            let b = self.engine_build(&ctx.store.config().await).await;
            if b > 0 && b != res.engine_build {
                out.insert(
                    "engine_changed".into(),
                    json!(format!("b{} → b{b}", res.engine_build)),
                );
            }
        }
        Value::Object(out)
    }

    /// `handleTuneApply`.
    async fn apply_route(self: &Arc<Self>, ctx: &Ctx, target: &str) -> (StatusCode, Value) {
        let fail = |code: StatusCode, e: &str| (code, json!({ "ok": false, "error": e }));
        let (res, busy) = {
            let st = self.st();
            (st.result.clone(), st.running || st.applying || st.starting)
        };
        if busy {
            return fail(StatusCode::CONFLICT, ERR_TUNE_BUSY);
        }
        let Some(res) = res else {
            return fail(StatusCode::BAD_REQUEST, "aucun résultat à appliquer");
        };
        if target == APPLY_COPY {
            return match self.apply(ctx, &res, APPLY_COPY, &|_| {}).await {
                Ok(msg) => (StatusCode::OK, json!({ "ok": true, "message": msg })),
                Err(e) => fail(StatusCode::OK, &e),
            };
        }
        if target != APPLY_PRESET {
            return fail(StatusCode::BAD_REQUEST, "cible inconnue");
        }
        if let Err(e) = self.plan_apply(&res) {
            return fail(StatusCode::OK, &e);
        }
        if crate::enginectl::generating(ctx).await {
            let gc = Self::go_context(ctx).await;
            let why = gc["busy"]
                .as_str()
                .filter(|s| !s.is_empty())
                .unwrap_or("génération en cours")
                .to_string();
            return fail(StatusCode::CONFLICT, &why);
        }
        // Vérifié et posé d'un seul tenant : deux clics rapprochés passaient
        // tous deux le test du début.
        {
            let mut st = self.st();
            if st.running || st.applying || st.starting {
                return fail(StatusCode::CONFLICT, ERR_TUNE_BUSY);
            }
            st.applying = true;
            st.apply_log.clear();
            st.apply_msg.clear();
            st.apply_err.clear();
        }
        let (me, ctx2) = (self.clone(), ctx.clone());
        tokio::spawn(async move {
            let me2 = me.clone();
            let say = move |s: String| me2.st().apply_log.push(s);
            let out = me.apply(&ctx2, &res, APPLY_PRESET, &say).await;
            let mut st = me.st();
            st.applying = false;
            match out {
                Ok(m) => st.apply_msg = m,
                Err(e) => st.apply_err = e,
            }
        });
        (StatusCode::ACCEPTED, json!({ "ok": true }))
    }
}

// ---------------------------------------------------------------------------
// Les essais
// ---------------------------------------------------------------------------

/// `tuneLoaded` : un essai chargé et prêt.
struct Loaded {
    eng: Engine,
    n_ctx: i64,
    slots: i64,
    build: String,
    offloaded: i64,
    layers: i64,
    kv: String,
}

/// L'arrêt d'un essai : le groupe entier, sa trace effacée du verrou, la VRAM
/// rendue. Sans processus : rien.
struct Stopper<'a> {
    run: Option<(i64, Arc<AtomicBool>)>,
    held: &'a Held<'a>,
    tune: &'a Tune,
}

impl Stopper<'_> {
    async fn stop(&mut self) {
        let Some((pid, done)) = self.run.take() else {
            return;
        };
        let before = crate::enginectl::gpu_used_mb(&self.tune.gpus().await);
        let d = done.clone();
        let _ = tokio::task::spawn_blocking(move || {
            lock::kill_tree(pid, &|| d.load(Ordering::SeqCst));
        })
        .await;
        // Noté jusqu'à sa fin constatée : un essai qui survit à l'arrêt reste
        // désigné dans le verrou, qu'un Loki redémarré pourra encore arrêter.
        if done.load(Ordering::SeqCst) {
            self.held.set_trial(None);
        } else {
            eprintln!("[loki tune] l'essai (PID {pid}) ne s'est pas arrêté dans les temps");
        }
        if before > 0 {
            self.tune.gpu_settled(before).await;
        } else {
            tokio::time::sleep(self.tune.settle).await;
        }
    }
}

/// `tuneTrialer` : lance les essais d'une optimisation.
struct Trialer<'a> {
    tune: &'a Tune,
    dir: PathBuf,
    seq: u32,
    held: &'a Held<'a>,
    key: String,
}

impl<'a> Trialer<'a> {
    /// `prepare` : la configuration d'un essai (copie + adresse privée) dans
    /// un dossier neuf.
    fn prepare(&mut self, cfg: &Cfg) -> Result<(PathBuf, u16), String> {
        self.seq += 1;
        let dir = self.dir.join(format!("e{:02}", self.seq));
        mkdir_private(&dir).map_err(|e| e.to_string())?;
        let port = std::net::TcpListener::bind("127.0.0.1:0")
            .and_then(|l| l.local_addr())
            .map_err(|e| e.to_string())?
            .port();
        let mut c = cfg.clone();
        c.insert("HOST".into(), "127.0.0.1".into());
        c.insert("PORT".into(), port.to_string());
        let text = tc::trial_config_text(&c)?;
        write_private(&dir.join("config.env"), text.as_bytes()).map_err(|e| e.to_string())?;
        Ok((dir, port))
    }

    fn command(&self, dir: &Path) -> tokio::process::Command {
        let mut cmd = tokio::process::Command::new(&self.tune.go_bin);
        cmd.arg("serve")
            .current_dir(&self.tune.home)
            .env(tc::TRIAL_ENV, dir)
            .stdin(std::process::Stdio::null());
        for (k, v) in &self.tune.trial_env {
            cmd.env(k, v);
        }
        cmd
    }

    /// `dry` : « loki serve » à blanc — la ligne de commande, rien de chargé.
    async fn dry(&self, cancel: &watch::Receiver<bool>, dir: &Path) -> Result<Launch, String> {
        let out_path = dir.join("dry.log");
        let out = std::fs::File::create(&out_path).map_err(|e| e.to_string())?;
        let err = out.try_clone().map_err(|e| e.to_string())?;
        let mut cmd = self.command(dir);
        cmd.env(tc::DRY_RUN_ENV, "1")
            .stdout(out)
            .stderr(err)
            .kill_on_drop(true);
        let mut child = cmd.spawn().map_err(|e| e.to_string())?;
        let mut cancel = cancel.clone();
        let status = tokio::select! {
            s = tokio::time::timeout(Duration::from_secs(120), child.wait()) => match s {
                Ok(Ok(s)) => s,
                Ok(Err(e)) => return Err(e.to_string()),
                Err(_) => {
                    let _ = child.kill().await;
                    return Err(last_line(&out_path, "signal: killed"));
                }
            },
            _ = async {
                if cancel.wait_for(|c| *c).await.is_err() {
                    std::future::pending::<()>().await;
                }
            } => {
                let _ = child.kill().await;
                return Err("signal: killed".into());
            }
        };
        if !status.success() {
            let why = match status.code() {
                Some(c) => format!("exit status {c}"),
                None => "signal: killed".into(),
            };
            return Err(last_line(&out_path, &why));
        }
        let p = dir.join("launch.json");
        let b = std::fs::read(&p).map_err(|e| format!("open {}: {e}", p.display()))?;
        serde_json::from_slice(&b).map_err(|e| e.to_string())
    }

    /// `load` : lance l'essai pour de vrai et attend qu'il réponde. Le
    /// [`Stopper`] rendu l'arrête : à appeler quoi qu'il arrive.
    async fn load(
        &self,
        cancel: &watch::Receiver<bool>,
        dir: &Path,
        port: u16,
        launch: &Launch,
    ) -> (Result<Loaded, String>, Stopper<'a>) {
        let mut stopper = Stopper {
            run: None,
            held: self.held,
            tune: self.tune,
        };
        let log_path = dir.join("engine.log");
        let (out, err) =
            match std::fs::File::create(&log_path).and_then(|f| f.try_clone().map(|g| (f, g))) {
                Ok(x) => x,
                Err(e) => return (Err(e.to_string()), stopper),
            };
        let mut cmd = self.command(dir);
        cmd.stdout(out).stderr(err);
        #[cfg(unix)]
        cmd.process_group(0);
        let mut child = match cmd.spawn() {
            Ok(c) => c,
            Err(e) => return (Err(e.to_string()), stopper),
        };
        let pid = i64::from(child.id().unwrap_or(0));
        let done = Arc::new(AtomicBool::new(false));
        let ended = Arc::new(Notify::new());
        {
            let (done, ended) = (done.clone(), ended.clone());
            tokio::spawn(async move {
                let _ = child.wait().await;
                done.store(true, Ordering::SeqCst);
                ended.notify_waiters();
            });
        }
        let mut proc = Proc {
            pid,
            start: String::new(),
            exes: vec![
                self.tune.go_bin.to_string_lossy().into_owned(),
                launch.bin.clone(),
            ],
        };
        if let Some((start, _)) = lock::proc_identity(pid) {
            proc.start = start;
        }
        self.held.set_trial(Some(proc));
        stopper.run = Some((pid, done.clone()));
        let read_log = || {
            std::fs::read(&log_path)
                .map(|b| String::from_utf8_lossy(&b).into_owned())
                .unwrap_or_default()
        };
        let (_tx, rx) = watch::channel(false);
        let mut eng = self
            .tune
            .engine(format!("http://127.0.0.1:{port}"), self.key.clone(), rx);
        eng.cancel = cancel.clone();
        let deadline = Instant::now() + self.tune.load_timeout;
        let hc = reqwest::Client::builder()
            .timeout(Duration::from_secs(3))
            .build()
            .unwrap_or_default();
        let mut cancel = cancel.clone();
        loop {
            let notified = ended.notified();
            if done.load(Ordering::SeqCst) {
                return (Err(tc::load_failure(&read_log())), stopper);
            }
            tokio::select! {
                _ = async {
                    if cancel.wait_for(|c| *c).await.is_err() {
                        std::future::pending::<()>().await;
                    }
                } => return (Err("context canceled".into()), stopper),
                _ = notified => return (Err(tc::load_failure(&read_log())), stopper),
                _ = tokio::time::sleep(self.tune.poll) => {}
            }
            if let Ok(r) = hc.get(format!("{}/health", eng.base)).send().await
                && r.status() == reqwest::StatusCode::OK
            {
                break;
            }
            if Instant::now() > deadline {
                return (
                    Err(format!(
                        "pas prêt après {}",
                        bench::go_dur(self.tune.load_timeout)
                    )),
                    stopper,
                );
            }
        }
        let (n_ctx, slots, build) = match props(&eng).await {
            Ok(p) => p,
            Err(e) => return (Err(format!("/props illisible : {e}")), stopper),
        };
        let (offloaded, layers, kv) = tc::log_facts(&read_log());
        (
            Ok(Loaded {
                eng,
                n_ctx,
                slots,
                build,
                offloaded,
                layers,
                kv,
            }),
            stopper,
        )
    }
}

/// `tuneProps` : n_ctx, total_slots et build_info du moteur.
async fn props(e: &Engine) -> Result<(i64, i64, String), String> {
    let mut req = e
        .client
        .get(format!("{}/props", e.base))
        .timeout(Duration::from_secs(10));
    if !e.key.is_empty() {
        req = req.bearer_auth(&e.key);
    }
    let r = req.send().await.map_err(|e| e.to_string())?;
    let status = r.status();
    let b = r.bytes().await.map_err(|e| e.to_string())?;
    if status != reqwest::StatusCode::OK {
        return Err(format!(
            "GET /props : {} {}",
            status.as_u16(),
            status.canonical_reason().unwrap_or("")
        ));
    }
    let v: Value = serde_json::from_slice(&b).map_err(|e| e.to_string())?;
    let int = |v: &Value| v.as_i64().unwrap_or(0);
    let mut n_ctx = int(&v["default_generation_settings"]["n_ctx"]);
    if n_ctx <= 0 {
        n_ctx = int(&v["n_ctx"]);
    }
    Ok((
        n_ctx,
        int(&v["total_slots"]),
        v["build_info"].as_str().unwrap_or("").to_string(),
    ))
}

/// La mesure d'un essai : le bench complet du preset de l'essai.
struct Measure<'a> {
    tune: &'a Tune,
    ctx: &'a Ctx,
    tr: &'a Tracker,
    env: &'a tc::Env,
    cpu_placed: bool,
    hybrid: bool,
    weights: Weights,
    bopts: bench::Opts,
    corp: Corpora,
}

impl Measure<'_> {
    /// `n` passages ; rend ceux qui ont abouti et la première erreur.
    async fn measure(
        &self,
        cfg: &Cfg,
        l: &Loaded,
        n: usize,
        phase: &str,
    ) -> (Vec<Run>, Option<String>) {
        let mut runs = Vec::new();
        let effort = self.tune.effort(self.ctx);
        let learn = |want: &str, got: &str| {
            effort.remember(cfg, want, got);
            log_effort_fallback(want, got);
        };
        for i in 0..n {
            self.tr.set_phase(format!("{phase} : mesure {}/{n}", i + 1));
            // `tuneBenchSetup` : l'échantillonnage et le raisonnement réels du
            // preset. SIDE_SLOT : les tours restent sur le second slot.
            let mut s = bench::setup_from(cfg, &self.env.arg_env, effort);
            s.cpu_placed = self.cpu_placed;
            s.hybrid = self.hybrid;
            s.side_slot = crate::serve::side::side_slot_on(cfg) && l.slots >= 2;
            let r: Result<BenchResult, String> = bench::run(
                &l.eng,
                self.bopts.clone(),
                &mut s,
                &self.corp,
                &|_, _, _| {},
                &learn,
                &|| None,
            )
            .await;
            let r = match r {
                Ok(r) => r,
                Err(e) => return (runs, Some(e)),
            };
            match tc::run_from(Some(&r), &self.weights) {
                Ok(run) => runs.push(run),
                Err(e) => return (runs, Some(e)),
            }
        }
        (runs, None)
    }
}

// ---------------------------------------------------------------------------
// Aides
// ---------------------------------------------------------------------------

fn turn_secs(r: &[Run]) -> Vec<f64> {
    r.iter().map(|x| x.turn_sec).collect()
}

fn turn_mean(r: &[Run]) -> f64 {
    tc::mean(&turn_secs(r))
}

fn prose_mean(r: &[Run]) -> f64 {
    tc::mean(&r.iter().map(|x| x.prose_tg).collect::<Vec<_>>())
}

/// `reBuildInfo` : `^b(\d+)`.
fn build_number(build: &str) -> i64 {
    let d: String = build
        .strip_prefix('b')
        .unwrap_or("")
        .chars()
        .take_while(char::is_ascii_digit)
        .collect();
    d.parse().unwrap_or(0)
}

/// `Duration.Round(time.Minute)`.
fn round_minute(d: Duration) -> Duration {
    let m = 60_000_000_000u128;
    let n = d.as_nanos();
    let r = n % m;
    let n = if r + r < m { n - r } else { n + m - r };
    Duration::from_nanos(n as u64)
}

fn unix_secs() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_secs() as i64)
}

/// `tunePhysCores` : cœurs physiques (Linux ; ailleurs inconnu).
fn phys_cores(probe: &tc::Probe) -> i64 {
    if probe.cpu_threads > 0 {
        return probe.cpu_threads;
    }
    let root = Path::new("/");
    let Some(online) = crate::serve::threads::read_cpu_list(root, "sys/devices/system/cpu/online")
        .filter(|o| !o.is_empty())
    else {
        return 0;
    };
    let atom =
        crate::serve::threads::read_cpu_list(root, "sys/devices/cpu_atom/cpus").unwrap_or_default();
    crate::serve::threads::physical_cores(root, &online, &atom).unwrap_or(0)
}

/// La dernière ligne de la sortie, sans couleurs ; vide : `fallback`.
fn last_line(path: &Path, fallback: &str) -> String {
    let out = std::fs::read(path)
        .map(|b| String::from_utf8_lossy(&b).into_owned())
        .unwrap_or_default();
    let msg = crate::enginectl::plain_err(&out);
    let msg = msg.trim();
    let msg = msg.rsplit('\n').next().unwrap_or("");
    if msg.is_empty() {
        fallback.to_string()
    } else {
        msg.to_string()
    }
}

fn mkdir_private(d: &Path) -> std::io::Result<()> {
    let mut b = std::fs::DirBuilder::new();
    b.recursive(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        b.mode(0o700);
    }
    b.create(d)
}

fn write_private(p: &Path, b: &[u8]) -> std::io::Result<()> {
    let mut o = std::fs::OpenOptions::new();
    o.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        o.mode(0o600);
    }
    use std::io::Write as _;
    o.open(p)?.write_all(b)
}

/// `tuneAppliedMismatch` : la configuration active après application doit
/// être EXACTEMENT la référence mesurée plus les clés réglées. "" = conforme.
pub fn applied_mismatch(now: &Cfg, res: &TuneResult) -> String {
    let mut back = now.clone();
    for (k, v) in &res.set {
        if now.get(k).map_or("", String::as_str) != v {
            return format!("la configuration appliquée diffère de l'essai retenu sur {k}");
        }
        match res.old.get(k).map_or("", String::as_str) {
            "" => {
                back.remove(k);
            }
            old => {
                back.insert(k.clone(), old.to_string());
            }
        }
    }
    if crate::presets::config_fingerprint(&back) != res.base_fp {
        return "la configuration a changé ailleurs que sur les clés réglées depuis la mesure"
            .into();
    }
    String::new()
}

/// `saveTuneResult` : le dernier résultat par preset. Les autres entrées
/// gardent leurs octets.
async fn save_result(ctx: &Ctx, res: &TuneResult) {
    let mut m: BTreeMap<String, Box<RawValue>> = ctx
        .store
        .get(BK_STATE, RESULTS_KEY)
        .await
        .ok()
        .flatten()
        .and_then(|b| serde_json::from_slice(&b).ok())
        .unwrap_or_default();
    let Ok(raw) = serde_json::to_string(res).and_then(RawValue::from_string) else {
        return;
    };
    m.insert(res.preset_id.clone(), raw);
    let _ = ctx
        .store
        .put(BK_STATE, RESULTS_KEY, gojson::marshal(&m).into_bytes())
        .await;
}

/// `loadTuneResult`.
async fn load_result(ctx: &Ctx, id: &str) -> Option<TuneResult> {
    let b = ctx.store.get(BK_STATE, RESULTS_KEY).await.ok().flatten()?;
    let mut m: HashMap<String, TuneResult> = serde_json::from_slice(&b).ok()?;
    m.remove(id)
}

// ---------------------------------------------------------------------------
// Les routes
// ---------------------------------------------------------------------------

pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/tune"
            | "/api/tune/status"
            | "/api/tune/cancel"
            | "/api/tune/apply"
            | "/api/tune/last"
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
    tune: &Arc<Tune>,
    req: Request<Incoming>,
    from_go: bool,
) -> Response<Body> {
    let (parts, body) = req.into_parts();
    if !from_go && let Err(refus) = ctx.guard(&parts.headers).await {
        return *refus;
    }
    let read = || async {
        Limited::new(body, 1 << 16)
            .collect()
            .await
            .map(|b| b.to_bytes().to_vec())
            .unwrap_or_default()
    };
    match parts.uri.path() {
        "/api/tune" => {
            if let Some(r) = post_only(&parts.method) {
                return r;
            }
            // Les erreurs de décodage sont ignorées (champs facultatifs).
            let d = gojson::decode_struct(
                &read().await,
                TUNE_REQ,
                &[
                    ("placement", Kind::Bool),
                    ("optin", Kind::Bool),
                    ("budget_min", Kind::Int),
                    ("stages", Kind::StrList),
                ],
            );
            let mut opts = Opts {
                placement: d.bool("placement"),
                opt_in: d.bool("optin"),
                stages: d.str_list("stages").unwrap_or_default(),
                ..Default::default()
            };
            if d.int("budget_min") > 0 {
                opts.budget = tune.budget_unit * d.int("budget_min").min(1 << 20) as u32;
            }
            let (code, out) = tune.start(ctx, opts).await;
            send_json(code, &out)
        }
        "/api/tune/status" => send_json(StatusCode::OK, &tune.status()),
        "/api/tune/cancel" => {
            if let Some(r) = post_only(&parts.method) {
                return r;
            }
            send_json(StatusCode::OK, &json!({ "ok": tune.cancel() }))
        }
        "/api/tune/apply" => {
            if let Some(r) = post_only(&parts.method) {
                return r;
            }
            let d = gojson::decode_struct(&read().await, APPLY_REQ, &[("target", Kind::Str)]);
            let (code, out) = tune.apply_route(ctx, &d.str("target")).await;
            send_json(code, &out)
        }
        "/api/tune/last" => {
            let id = url::form_urlencoded::parse(parts.uri.query().unwrap_or("").as_bytes())
                .find(|(k, _)| k == "id")
                .map(|(_, v)| v.into_owned())
                .unwrap_or_default();
            send_json(StatusCode::OK, &tune.last(ctx, &id).await)
        }
        _ => send_json(
            StatusCode::NOT_FOUND,
            &json!({ "ok": false, "error": "route inconnue" }),
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// L'ETA : inconnue (-1) tant qu'aucun essai n'est mesuré ; ensuite la
    /// moyenne fois les essais restants, bornée par le budget qui reste,
    /// jamais négative.
    #[test]
    fn eta_du_suivi() {
        let tr = Tracker::new();
        tr.lock().budget = Duration::from_secs(600);
        assert_eq!(tr.snapshot()["eta_sec"], json!(-1));
        assert_eq!(tr.snapshot()["trials"], Value::Null);
        {
            let mut st = tr.lock();
            st.avg_sec = 10.0;
            st.left = 5;
        }
        assert_eq!(tr.snapshot()["eta_sec"], json!(50));
        tr.lock().left = 100;
        let eta = tr.snapshot()["eta_sec"].as_f64().unwrap();
        assert!(eta > 590.0 && eta <= 600.0, "{eta}");
        tr.lock().started = Instant::now() - Duration::from_secs(700);
        assert_eq!(tr.snapshot()["eta_sec"], json!(0));
        // Fin : le temps écoulé s'arrête.
        let started = tr.lock().started;
        tr.lock().ended = Some(started + Duration::from_secs(3));
        assert_eq!(tr.snapshot()["elapsed_sec"], json!(3));
        // Un essai remplacé à sa place, un nouveau ajouté.
        let i = tr.put(None, Trial::default());
        tr.put(
            Some(i),
            Trial {
                label: "a".into(),
                ..Default::default()
            },
        );
        tr.put(Some(9), Trial::default());
        let t = tr.snapshot()["trials"].clone();
        assert_eq!(t.as_array().unwrap().len(), 2);
        assert_eq!(t[0]["label"], json!("a"));
    }

    #[test]
    fn arrondi_a_la_minute() {
        let m = |s: u64| round_minute(Duration::from_secs(s)).as_secs();
        assert_eq!(
            (m(29), m(30), m(89), m(90), m(3600)),
            (0, 60, 60, 120, 3600)
        );
        assert_eq!(build_number("b7000-x"), 7000);
        assert_eq!(build_number("7000"), 0);
        assert_eq!(build_number("b"), 0);
    }
}
