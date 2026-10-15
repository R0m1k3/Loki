//! Tâches planifiées tenues par la façade (lot 26 de la migration,
//! `LOKI_TASKS=rust` ; expérimental, le Go reste le défaut) — port de
//! `tasks.go`, `tasks_sched.go`, `tasks_script.go` et `web_tasks.go`.
//!
//! - les routes `/api/tasks*` (liste, enregistrement, suppression, bascule,
//!   interrupteur maître, lancement, arrêt) ;
//! - le planificateur : un tic par minute, une tâche due par tic ;
//! - les tâches « script seul » (un script du dossier protégé, sans modèle).
//!
//! Une tâche IA passe par le verrou de génération et les outils de l'agent,
//! restés au Go : la façade la fait exécuter par lui
//! (`/internal/v1/tasks/run`), qui tient son état comme son propre
//! planificateur.

pub mod cron;

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use http_body_util::{BodyExt, Limited};
use hyper::body::Incoming;
use hyper::{Request, Response, StatusCode};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::gojson::{self, Kind};
use crate::proxy::Body;
use crate::store::{BK_STATE, Store};

/// `bkTasks`.
pub const BK_TASKS: &str = "tasks";
/// `reportMax`.
pub const REPORT_MAX: usize = 4000;
/// `scriptTaskTimeout`.
pub const SCRIPT_TIMEOUT: Duration = Duration::from_secs(10 * 60);
/// Attente d'une tâche IA exécutée par le Go (qui ne la borne pas).
const AI_TASK_LIMIT: Duration = Duration::from_secs(24 * 3600);

/// `Task` (mêmes champs JSON).
#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq)]
pub struct Task {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub prompt: String,
    #[serde(default)]
    pub schedule: String,
    #[serde(default)]
    pub tz: String,
    #[serde(default)]
    pub preset: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub project: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub kind: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub script: String,
    #[serde(default)]
    pub no_mem: bool,
    #[serde(default)]
    pub no_web: bool,
    #[serde(default)]
    pub enabled: bool,
    #[serde(default)]
    pub last_run: i64,
    #[serde(default)]
    pub next_run: i64,
    #[serde(default)]
    pub last_dur_ms: i64,
    #[serde(default)]
    pub last_ok: bool,
    #[serde(default)]
    pub last_error: String,
    #[serde(default)]
    pub last_report: String,
}

/// Maintenant, en ns Unix.
pub fn now_ns() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_nanos() as i64)
}

/// `newTaskID` : 6 octets aléatoires en hexadécimal.
pub fn new_task_id() -> String {
    let mut b = [0u8; 6];
    let _ = getrandom::fill(&mut b);
    hex::encode(b)
}

/// Les tâches tenues ici : la base, et l'exécution en cours.
pub struct Tasks {
    pub home: PathBuf,
    /// La tâche IA que le Go exécute pour la façade (son id).
    running: Mutex<Option<String>>,
    /// Les tâches script en cours (id → signal d'arrêt) — `scriptRunning`.
    scripts: Mutex<BTreeMap<String, tokio::sync::watch::Sender<bool>>>,
    /// Pas du planificateur (une minute).
    pub tick: Duration,
    /// L'heure (ns Unix) — remplaçable en test.
    pub clock: fn() -> i64,
    /// Un tic tout de suite (tests).
    kick: tokio::sync::Notify,
    /// Tics achevés (le premier passage compte pour un).
    ticks: AtomicU64,
    /// Joignabilité de Crawl4AI (cache de 30 s, comme le Go).
    reach: crate::system::System,
}

impl Tasks {
    pub fn new(home: PathBuf) -> Self {
        Self::with(home, Duration::from_secs(60), now_ns)
    }

    /// Avec le pas du planificateur et l'horloge donnés.
    pub fn with(home: PathBuf, tick: Duration, clock: fn() -> i64) -> Self {
        Tasks {
            home,
            running: Mutex::new(None),
            scripts: Mutex::new(BTreeMap::new()),
            tick,
            clock,
            kick: tokio::sync::Notify::new(),
            ticks: AtomicU64::new(0),
            reach: crate::system::System::new(),
        }
    }

    pub fn now(&self) -> i64 {
        (self.clock)()
    }

    /// La tâche en cours vue par l'écran : l'IA d'abord, sinon un script.
    pub fn running_id(&self) -> String {
        if let Some(id) = self
            .running
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
        {
            return id;
        }
        self.scripts
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .keys()
            .next()
            .cloned()
            .unwrap_or_default()
    }

    /// Une tâche script tourne-t-elle encore ?
    pub fn script_running(&self) -> bool {
        !self
            .scripts
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .is_empty()
    }

    fn set_running(&self, r: Option<String>) {
        *self.running.lock().unwrap_or_else(|e| e.into_inner()) = r;
    }

    /// Demande un tic tout de suite.
    pub fn kick(&self) {
        self.kick.notify_one();
    }

    /// Le nombre de tics achevés.
    pub fn ticks(&self) -> u64 {
        self.ticks.load(Ordering::SeqCst)
    }

    /// `scriptsDir`.
    pub fn scripts_dir(&self) -> PathBuf {
        self.home.join("scripts")
    }
}

/// `listTasks` : triées par nom, puis par id.
pub async fn list_tasks(store: &Store) -> Vec<Task> {
    let mut out: Vec<Task> = store
        .all(BK_TASKS)
        .await
        .unwrap_or_default()
        .into_values()
        .filter_map(|v| serde_json::from_str(&v).ok())
        .collect();
    out.sort_by(|a, b| a.name.cmp(&b.name).then_with(|| a.id.cmp(&b.id)));
    out
}

/// `getTask`.
pub async fn get_task(store: &Store, id: &str) -> Option<Task> {
    let b = store.get(BK_TASKS, id).await.ok().flatten()?;
    if b.is_empty() {
        return None;
    }
    serde_json::from_slice(&b).ok()
}

/// `saveTask`.
pub async fn save_task(store: &Store, t: &Task) -> Result<(), String> {
    store
        .put(BK_TASKS, &t.id, gojson::marshal(t).into_bytes())
        .await
}

/// `tasksPaused`.
pub async fn tasks_paused(store: &Store) -> bool {
    store.get_str(BK_STATE, "tasks_paused").await == "1"
}

/// `scriptsPath` : un script DANS le dossier protégé — ni chemin absolu, ni
/// remontée, ni caractère hostile au shell.
pub fn scripts_path(dir: &Path, name: &str) -> Result<PathBuf, String> {
    let name = name.trim();
    if name.is_empty() {
        return Err("nom de script vide".into());
    }
    if name.starts_with('/') || name.starts_with('\\') {
        return Err(format!(
            "nom de script invalide (commence par un séparateur) : {name}"
        ));
    }
    if let Some(i) = name.find([
        '"', '\'', '`', '$', ';', '&', '|', '<', '>', '*', '?', '\\', '\n', '\r',
    ]) {
        return Err(format!(
            "nom de script invalide (caractère interdit {}) : {name}",
            gojson::quote(&name[i..i + 1])
        ));
    }
    let clean = crate::paths::go_clean(name);
    if clean.starts_with('/') || clean == ".." || clean.starts_with("../") {
        return Err(format!(
            "nom de script invalide (pas de chemin absolu ni de ../) : {name}"
        ));
    }
    Ok(dir.join(clean))
}

/// `scriptExists`.
pub fn script_exists(dir: &Path, name: &str) -> Result<(), String> {
    let full = scripts_path(dir, name)?;
    match std::fs::metadata(&full) {
        Err(_) => Err(format!("script introuvable : {name}")),
        Ok(m) if m.is_dir() => Err(format!("{name} est un dossier, pas un script")),
        Ok(_) => Ok(()),
    }
}

/// `listScripts` : les scripts du dossier (chemins relatifs, triés comme
/// `filepath.WalkDir`).
pub fn list_scripts(dir: &Path) -> Vec<Value> {
    fn walk(root: &Path, p: &Path, out: &mut Vec<Value>) {
        let Ok(rd) = std::fs::read_dir(p) else {
            return;
        };
        let mut entries: Vec<_> = rd.flatten().collect();
        entries.sort_by_key(|e| e.file_name());
        for e in entries {
            let path = e.path();
            let Ok(ft) = e.file_type() else { continue };
            if ft.is_dir() {
                walk(root, &path, out);
                continue;
            }
            let rel = path
                .strip_prefix(root)
                .map(|r| r.to_string_lossy().replace('\\', "/"))
                .unwrap_or_default();
            let size = e.metadata().map_or(0, |m| m.len());
            out.push(json!({ "name": rel, "size": size }));
        }
    }
    let mut out = Vec::new();
    walk(dir, dir, &mut out);
    out.sort_by(|a, b| a["name"].as_str().cmp(&b["name"].as_str()));
    out
}

// ---------------------------------------------------------------------------
// Exécution
// ---------------------------------------------------------------------------

/// `ErrBusy`.
pub const ERR_BUSY: &str = "génération en cours";
/// `errModelLoading`.
pub const ERR_MODEL_LOADING: &str =
    "⏳ Le modèle est encore en train de charger — réessaie dans quelques secondes.";

/// Fin d'une exécution.
pub enum Outcome {
    Ok(String),
    Err(String),
    Canceled,
}

/// `recordTaskEnd` : durée, dernier et prochain passage, succès ou échec,
/// compte-rendu — sur la tâche relue (une édition concurrente n'est pas
/// écrasée) ; puis la notification.
pub async fn record_task_end(ctx: &Ctx, tasks: &Tasks, id: &str, start_ns: i64, out: Outcome) {
    let store = &ctx.store;
    let now = tasks.now();
    let Some(mut cur) = get_task(store, id).await else {
        return;
    };
    cur.last_dur_ms = (now - start_ns).div_euclid(1_000_000);
    cur.last_run = now.div_euclid(1_000_000);
    cur.next_run = cron::compute_next_run(&cur.schedule, &cur.tz, now);
    if cron::is_once(&cur.schedule) {
        cur.enabled = false;
        cur.next_run = 0;
    }
    let (failed, canceled) = match &out {
        Outcome::Canceled => {
            cur.last_ok = true;
            cur.last_error.clear();
            cur.last_report = "(interrompue manuellement)".into();
            (false, true)
        }
        Outcome::Err(e) => {
            cur.last_ok = false;
            cur.last_error = e.clone();
            (true, false)
        }
        Outcome::Ok(report) => {
            cur.last_ok = true;
            cur.last_error.clear();
            cur.last_report = truncate_report(report);
            (false, false)
        }
    };
    let _ = save_task(store, &cur).await;
    if !canceled {
        let title = if failed {
            "Loki · tâche en échec"
        } else {
            "Loki · tâche terminée"
        };
        push(ctx, title, &cur.name).await;
    }
}

/// Le compte-rendu borné à `reportMax` octets : `report[:n] + "…"` du Go,
/// qui peut couper un caractère — ses octets orphelins deviennent chacun
/// U+FFFD, comme à l'encodage JSON du Go.
fn truncate_report(r: &str) -> String {
    if r.len() <= REPORT_MAX {
        return r.to_string();
    }
    format!("{}…", crate::system::go_lossy(&r.as_bytes()[..REPORT_MAX]))
}

/// La notification de fin : par la façade si elle tient les notifications,
/// sinon par le Go.
async fn push(ctx: &Ctx, title: &str, body: &str) {
    if let Some(p) = ctx.push.clone() {
        let (store, t, b) = (ctx.store.clone(), title.to_string(), body.to_string());
        tokio::spawn(async move { p.send_all(&store, &t, &b).await });
        return;
    }
    let _ = ctx
        .store
        .internal_post(
            "/internal/v1/push/send",
            &json!({ "title": title, "body": body }),
        )
        .await;
}

/// `runTask` : une tâche script ici, une tâche IA par le Go.
pub async fn run_task(ctx: &Ctx, tasks: &Tasks, t: Task) {
    if t.kind == "script" {
        return run_script_task(ctx, tasks, t).await;
    }
    tasks.set_running(Some(t.id.clone()));
    // Le Go rend la main à la fin du passage (une tâche IA dure).
    let _ = ctx
        .store
        .internal_post_within(
            "/internal/v1/tasks/run",
            &json!({ "id": t.id }),
            AI_TASK_LIMIT,
        )
        .await;
    tasks.set_running(None);
}

/// `runScriptTask` : le script, sans modèle ; sa sortie fait le compte-rendu.
async fn run_script_task(ctx: &Ctx, tasks: &Tasks, t: Task) {
    let start = tasks.now();
    let name = t.script.trim();
    if name.is_empty() {
        let e = "aucun script associé à la tâche".to_string();
        return record_task_end(ctx, tasks, &t.id, start, Outcome::Err(e)).await;
    }
    let full = match scripts_path(&tasks.scripts_dir(), name) {
        Ok(p) => p,
        Err(e) => return record_task_end(ctx, tasks, &t.id, start, Outcome::Err(e)).await,
    };
    if std::fs::metadata(&full).is_err() {
        let e = format!("script introuvable : {name}");
        return record_task_end(ctx, tasks, &t.id, start, Outcome::Err(e)).await;
    }
    let (stop_tx, mut stop_rx) = tokio::sync::watch::channel(false);
    tasks
        .scripts
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .insert(t.id.clone(), stop_tx);
    let (cwd, mem) = shell_dirs(ctx).await;
    let cmd = script_run_command(&full);
    let run = crate::tools::shell::run_shell(&cwd, &mem, &cmd, SCRIPT_TIMEOUT.as_secs() as i64);
    let out = tokio::select! {
        out = run => Some(out),
        _ = stop_rx.wait_for(|s| *s) => None,
    };
    let outcome = match out {
        None => Outcome::Canceled,
        Some(o) if o.starts_with("exit: 0") => Outcome::Ok(o),
        Some(o) => Outcome::Err(o),
    };
    record_task_end(ctx, tasks, &t.id, start, outcome).await;
    tasks
        .scripts
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .remove(&t.id);
}

/// `scriptRunCommand` : le script lancé directement (son shebang choisit
/// l'interpréteur) ; sous Windows, un `.ps1` passe par PowerShell. Le nom est
/// déjà bridé par `scripts_path` : les guillemets suffisent.
fn script_run_command(path: &Path) -> String {
    let q = format!("\"{}\"", path.to_string_lossy());
    let ps1 = path
        .extension()
        .is_some_and(|e| e.eq_ignore_ascii_case("ps1"));
    if cfg!(windows) && ps1 {
        return format!("powershell -NoProfile -ExecutionPolicy Bypass -File {q}");
    }
    q
}

/// Le dossier de travail de la discussion ouverte (là où `runShell` lance
/// ses commandes) et le dossier mémoire du projet actif (gardé du shell).
async fn shell_dirs(ctx: &Ctx) -> (PathBuf, PathBuf) {
    let v = ctx
        .store
        .internal_json(
            hyper::Method::POST,
            "/internal/v1/conv/workspace",
            Some(&json!({})),
        )
        .await
        .unwrap_or(Value::Null);
    let cwd = v
        .get("dir")
        .and_then(Value::as_str)
        .unwrap_or("")
        .to_string();
    let active = crate::memory::active_project(&ctx.store).await;
    let mem = crate::paths::project_memory_dir(&ctx.engine.home, &active);
    (PathBuf::from(cwd), PathBuf::from(mem))
}

// ---------------------------------------------------------------------------
// Planificateur
// ---------------------------------------------------------------------------

/// `primeNextRuns` : le prochain passage des tâches actives qui n'en ont pas.
pub async fn prime_next_runs(ctx: &Ctx, tasks: &Tasks) {
    let now = tasks.now();
    for mut t in list_tasks(&ctx.store).await {
        if t.enabled && t.next_run == 0 {
            t.next_run = cron::compute_next_run(&t.schedule, &t.tz, now);
            let _ = save_task(&ctx.store, &t).await;
        }
    }
}

/// `tickTasks` : la première tâche due (une par tic), sauf interrupteur
/// maître ou optimisation en cours.
pub async fn tick_tasks(ctx: &Ctx, tasks: &Tasks) {
    if tasks_paused(&ctx.store).await || crate::backends::tune_busy(&ctx.store).await.is_some() {
        return;
    }
    let now = tasks.now();
    let now_ms = now.div_euclid(1_000_000);
    for mut t in list_tasks(&ctx.store).await {
        if !t.enabled {
            continue;
        }
        if t.next_run == 0 {
            t.next_run = cron::compute_next_run(&t.schedule, &t.tz, now);
            let _ = save_task(&ctx.store, &t).await;
            continue;
        }
        if t.next_run <= now_ms {
            run_task(ctx, tasks, t).await;
            return;
        }
    }
}

/// La boucle : un premier passage tout de suite, puis un tic par minute (ou
/// sur demande).
pub async fn sched_loop(ctx: Ctx, tasks: Arc<Tasks>) {
    prime_next_runs(&ctx, &tasks).await;
    tasks.ticks.fetch_add(1, Ordering::SeqCst);
    let mut iv = tokio::time::interval(tasks.tick);
    iv.tick().await;
    loop {
        tokio::select! {
            _ = iv.tick() => {}
            _ = tasks.kick.notified() => {}
        }
        tick_tasks(&ctx, &tasks).await;
        tasks.ticks.fetch_add(1, Ordering::SeqCst);
    }
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

const SAVE_REQ: &str = r#"struct { ID string "json:\"id\""; Name string "json:\"name\""; Prompt string "json:\"prompt\""; Schedule string "json:\"schedule\""; TZ string "json:\"tz\""; Preset string "json:\"preset\""; Project string "json:\"project\""; Kind string "json:\"kind\""; Script string "json:\"script\""; NoMem bool "json:\"no_mem\""; NoWeb bool "json:\"no_web\""; Enabled bool "json:\"enabled\"" }"#;
const ID_REQ: &str = r#"struct { ID string "json:\"id\"" }"#;
const TOGGLE_REQ: &str = r#"struct { ID string "json:\"id\""; On bool "json:\"on\"" }"#;
const ON_REQ: &str = r#"struct { On bool "json:\"on\"" }"#;

/// Les routes servies par ce module.
pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/tasks"
            | "/api/tasks/save"
            | "/api/tasks/delete"
            | "/api/tasks/toggle"
            | "/api/tasks/pause"
            | "/api/tasks/run"
            | "/api/tasks/stop"
    )
}

fn fail(code: StatusCode, e: impl Into<String>) -> Response<Body> {
    send_json(code, &json!({ "ok": false, "error": e.into() }))
}

/// `handleTasks`.
async fn overview(ctx: &Ctx, tasks: &Tasks) -> Value {
    let store = &ctx.store;
    let list = list_tasks(store).await;
    let running_id = tasks.running_id();
    let cfg = store.config().await;
    let order = crate::presets::parse_preset_order(
        store
            .get(BK_STATE, crate::presets::PRESET_ORDER_KEY)
            .await
            .ok()
            .flatten()
            .as_deref(),
    );
    let presets: Vec<Value> = crate::presets::list_presets(&tasks.home, &cfg, &order)
        .map(|l| {
            l.into_iter()
                .map(|p| json!({ "id": p.id, "name": p.name, "active": p.active }))
                .collect()
        })
        .unwrap_or_default();
    let switch_on = store.get_str(BK_STATE, "internet").await == "1";
    let key = store.get_str(BK_STATE, "crawl_key").await;
    let web_on =
        crate::system::internet_enabled(&cfg, switch_on) && tasks.reach.reachable(&cfg, &key).await;
    json!({
        "ok": true,
        "tasks": if list.is_empty() { Value::Null } else { serde_json::to_value(&list).unwrap_or(Value::Null) },
        "scripts": list_scripts(&tasks.scripts_dir()),
        "scripts_dir": tasks.scripts_dir().to_string_lossy(),
        "paused": tasks_paused(store).await,
        "agent": store.get_str(BK_STATE, "agent").await == "1",
        "running_id": running_id,
        "presets": presets,
        "projects": crate::projects_api::project_dtos(ctx).await,
        "mem_on": crate::memory::Memory::mem_mode(store).await != "off",
        "web_on": web_on,
    })
}

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(
    ctx: &Ctx,
    tasks: &Arc<Tasks>,
    req: Request<Incoming>,
    from_go: bool,
) -> Response<Body> {
    let (parts, body) = req.into_parts();
    if !from_go && let Err(refus) = ctx.guard(&parts.headers).await {
        return *refus;
    }
    let raw = Limited::new(body, 1 << 20)
        .collect()
        .await
        .map(|b| b.to_bytes().to_vec())
        .unwrap_or_default();
    let store = &ctx.store;
    match parts.uri.path() {
        "/api/tasks" => send_json(StatusCode::OK, &overview(ctx, tasks).await),
        "/api/tasks/save" => {
            let d = gojson::decode_struct(
                &raw,
                SAVE_REQ,
                &[
                    ("id", Kind::Str),
                    ("name", Kind::Str),
                    ("prompt", Kind::Str),
                    ("schedule", Kind::Str),
                    ("tz", Kind::Str),
                    ("preset", Kind::Str),
                    ("project", Kind::Str),
                    ("kind", Kind::Str),
                    ("script", Kind::Str),
                    ("no_mem", Kind::Bool),
                    ("no_web", Kind::Bool),
                    ("enabled", Kind::Bool),
                ],
            );
            if let Some(e) = d.err.clone() {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let name = d.str("name").trim().to_string();
            let prompt = d.str("prompt").trim().to_string();
            let schedule = d.str("schedule").trim().to_string();
            let kind = d.str("kind").trim().to_string();
            let script = d.str("script").trim().to_string();
            let tz = d.str("tz");
            if name.is_empty() {
                return fail(StatusCode::BAD_REQUEST, "nom obligatoire");
            }
            if kind == "script" {
                if script.is_empty() {
                    return fail(
                        StatusCode::BAD_REQUEST,
                        "script obligatoire pour une tâche script",
                    );
                }
                if let Err(e) = script_exists(&tasks.scripts_dir(), &script) {
                    return fail(StatusCode::BAD_REQUEST, e);
                }
            } else if prompt.is_empty() {
                return fail(StatusCode::BAD_REQUEST, "consigne obligatoire");
            }
            let now = tasks.now();
            if let Err(e) = cron::next_after(&schedule, &tz, now) {
                return fail(StatusCode::BAD_REQUEST, format!("fréquence invalide : {e}"));
            }
            let id = d.str("id");
            let mut t = Task::default();
            if !id.is_empty() {
                match get_task(store, &id).await {
                    Some(cur) => t = cur,
                    None => t.id = id,
                }
            }
            if t.id.is_empty() {
                t.id = new_task_id();
            }
            t.name = name;
            t.prompt = prompt;
            t.enabled = d.bool("enabled");
            t.tz = tz.clone();
            t.preset = d.str("preset");
            t.kind = kind;
            t.script = script;
            let project = d.str("project").trim().to_string();
            let projects = crate::memory::list_projects(store).await;
            if !project.is_empty() && projects.iter().any(|p| p.slug == project) {
                t.project = project;
            } else if t.project.is_empty() {
                t.project = crate::memory::active_project(store).await;
            }
            t.no_mem = d.bool("no_mem");
            t.no_web = d.bool("no_web");
            if t.schedule != schedule || t.next_run == 0 {
                t.next_run = cron::compute_next_run(&schedule, &tz, now);
            }
            t.schedule = schedule;
            if let Err(e) = save_task(store, &t).await {
                return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
            }
            send_json(StatusCode::OK, &json!({ "ok": true, "id": t.id }))
        }
        "/api/tasks/delete" => {
            let d = gojson::decode_struct(&raw, ID_REQ, &[("id", Kind::Str)]);
            let id = d.str("id");
            if id.is_empty() {
                return fail(StatusCode::BAD_REQUEST, "id manquant");
            }
            if let Err(e) = store.del(BK_TASKS, &id).await {
                return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
            }
            send_json(StatusCode::OK, &json!({ "ok": true }))
        }
        "/api/tasks/toggle" => {
            let d =
                gojson::decode_struct(&raw, TOGGLE_REQ, &[("id", Kind::Str), ("on", Kind::Bool)]);
            let Some(mut t) = get_task(store, &d.str("id")).await else {
                return fail(StatusCode::NOT_FOUND, "tâche introuvable");
            };
            t.enabled = d.bool("on");
            if t.enabled {
                t.next_run = cron::compute_next_run(&t.schedule, &t.tz, tasks.now());
            }
            if let Err(e) = save_task(store, &t).await {
                return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
            }
            send_json(StatusCode::OK, &json!({ "ok": true }))
        }
        "/api/tasks/pause" => {
            let d = gojson::decode_struct(&raw, ON_REQ, &[("on", Kind::Bool)]);
            let res = if d.bool("on") {
                store.put(BK_STATE, "tasks_paused", b"1".to_vec()).await
            } else {
                store.del(BK_STATE, "tasks_paused").await
            };
            if let Err(e) = res {
                return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
            }
            send_json(
                StatusCode::OK,
                &json!({ "ok": true, "paused": tasks_paused(store).await }),
            )
        }
        "/api/tasks/stop" => {
            let d = gojson::decode_struct(&raw, ID_REQ, &[("id", Kind::Str)]);
            let id = d.str("id");
            let stop = (!id.is_empty())
                .then(|| {
                    tasks
                        .scripts
                        .lock()
                        .unwrap_or_else(|e| e.into_inner())
                        .get(&id)
                        .cloned()
                })
                .flatten();
            if let Some(s) = stop {
                let _ = s.send(true);
                return send_json(StatusCode::OK, &json!({ "ok": true }));
            }
            let _ = store
                .internal_post("/internal/v1/engine/stop", &json!({}))
                .await;
            send_json(StatusCode::OK, &json!({ "ok": true }))
        }
        "/api/tasks/run" => {
            let d = gojson::decode_struct(&raw, ID_REQ, &[("id", Kind::Str)]);
            let Some(t) = get_task(store, &d.str("id")).await else {
                return fail(StatusCode::NOT_FOUND, "tâche introuvable");
            };
            if t.kind != "script" {
                if crate::enginectl::generating(ctx).await {
                    return fail(StatusCode::CONFLICT, ERR_BUSY);
                }
                if !crate::engstatus::health_check(&store.config().await).await {
                    return fail(StatusCode::SERVICE_UNAVAILABLE, ERR_MODEL_LOADING);
                }
            }
            let (ctx2, tasks2) = (ctx.clone(), tasks.clone());
            tokio::spawn(async move { run_task(&ctx2, &tasks2, t).await });
            send_json(StatusCode::OK, &json!({ "ok": true }))
        }
        _ => fail(StatusCode::NOT_FOUND, "route inconnue"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn compte_rendu_borne() {
        let pile = "x".repeat(REPORT_MAX);
        assert_eq!(truncate_report(&pile), pile);
        let long = format!("a{}", "é".repeat(2000));
        let want = format!("a{}\u{FFFD}…", "é".repeat(1999));
        assert_eq!(truncate_report(&long), want);
        // Coupé entre deux caractères : rien à remplacer.
        let net = format!("{}é", "x".repeat(REPORT_MAX));
        assert_eq!(
            truncate_report(&net),
            format!("{}…", "x".repeat(REPORT_MAX))
        );
    }

    #[test]
    fn noms_de_script() {
        let dir = Path::new("/srv/scripts");
        assert_eq!(scripts_path(dir, " sous/a.sh "), Ok(dir.join("sous/a.sh")));
        assert_eq!(scripts_path(dir, "./a.sh"), Ok(dir.join("a.sh")));
        assert_eq!(scripts_path(dir, "  "), Err("nom de script vide".into()));
        assert_eq!(
            scripts_path(dir, "a/../../b"),
            Err("nom de script invalide (pas de chemin absolu ni de ../) : a/../../b".into())
        );
        assert_eq!(
            scripts_path(dir, "a\nb"),
            Err("nom de script invalide (caractère interdit \"\\n\") : a\nb".into())
        );
    }
}
