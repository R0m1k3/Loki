//! Backends llama.cpp tenus par la façade (lot 22 de la migration,
//! `LOKI_BACKENDS=rust` ; expérimental, le Go reste le défaut) — port de
//! `web_llamacpp.go`, `web_llamacpp_job.go`, `backend_prebuilt.go`, de
//! `handleBackends`, `handleBackendsCustom`, `handleLlamacppUninstallCustom`
//! (`web_api.go`) et `handleBackendDevices` (`web_devices.go`).
//!
//! La façade détient LE job des moteurs ([`job::Jobs`]) : les siens
//! (binaires précompilés ; compilation depuis les sources — install, update,
//! install-custom — sous Linux et macOS depuis le lot 41, [`build`]) et ceux
//! que le Go fait encore tourner — compilation sous Windows, moteur de
//! l'image, Strata — qui lui rapportent chaque étape par l'API interne
//! (`/internal/facade/v1/build-job/*`).

pub mod build;
pub mod devices;
pub mod job;
pub mod prebuilt;
pub mod probe;

use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::Arc;

use http_body_util::{BodyExt, Limited};
use hyper::body::Incoming;
use hyper::{Method, Request, Response, StatusCode};
use serde_json::{Value, json};

use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::gojson::{self, Kind};
use crate::paths::{go_clean, go_dir};
use crate::proxy::Body;
use crate::serve::run::{EnginePaths, same_path};
use crate::store::{BK_CONFIG, Store};

use probe::{Probe, is_dir, is_file};

/// Préfixe de l'API interne du job (pour le Go seulement).
pub const INTERNAL_PREFIX: &str = "/internal/facade/v1/build-job/";

const INSTALL_REQ_TYPE: &str = r#"struct { Force bool "json:\"force\"" }"#;
const UPDATE_REQ_TYPE: &str = r#"struct { Clean bool "json:\"clean\"" }"#;
const CUSTOM_REQ_TYPE: &str = r#"struct { Repo string "json:\"repo\""; Name string "json:\"name\""; Ref string "json:\"ref\"" }"#;
const USE_REQ_TYPE: &str = r#"struct { Mode string "json:\"mode\"" }"#;
const NAME_REQ_TYPE: &str = r#"struct { Name string "json:\"name\"" }"#;
const BIN_REQ_TYPE: &str = r#"struct { Bin string "json:\"bin\"" }"#;

/// Les backends tenus par la façade.
pub struct Backends {
    pub home: PathBuf,
    pub probe: Probe,
    pub target: prebuilt::Target,
    /// `LOKI_ENGINE_BIN` (moteur livré par l'image), lu au démarrage.
    engine_bin_env: std::sync::RwLock<String>,
    /// `llamaReleasesAPI`.
    pub releases_api: String,
    pub jobs: job::Jobs,
    pub devices: devices::Devices,
    client: reqwest::Client,
}

impl Backends {
    pub fn new(home: PathBuf, engine_bin_env: String) -> Self {
        Self::with(
            home,
            engine_bin_env,
            Probe::system(),
            prebuilt::RELEASES_API.into(),
        )
    }

    /// Pour les tests : machine et API des releases choisies.
    pub fn with(home: PathBuf, engine_bin_env: String, probe: Probe, releases_api: String) -> Self {
        crate::chat::engine::install_crypto();
        Backends {
            jobs: job::Jobs::new(home.clone()),
            devices: devices::Devices::new(home.clone()),
            home,
            probe,
            target: prebuilt::Target::current(),
            engine_bin_env: std::sync::RwLock::new(engine_bin_env),
            releases_api,
            client: reqwest::Client::builder().build().unwrap_or_default(),
        }
    }

    /// Change le moteur livré par l'image (tests).
    pub fn set_engine_bin_env(&self, v: &str) {
        *self
            .engine_bin_env
            .write()
            .unwrap_or_else(|e| e.into_inner()) = v.to_string();
    }

    fn engine_paths(&self) -> EnginePaths {
        EnginePaths {
            home: self.home.clone(),
        }
    }

    fn home_s(&self) -> String {
        self.home.to_string_lossy().into_owned()
    }

    /// `backendsDir`.
    fn backends_dir(&self) -> PathBuf {
        self.home.join("backends")
    }

    /// `defaultRepoDir`.
    fn default_repo_dir(&self) -> PathBuf {
        self.backends_dir().join("llama.cpp")
    }

    /// `llamacppRepoDir` : la racine du dépôt du BIN, sinon le dépôt canonique.
    fn repo_dir(&self, cfg_bin: &str) -> PathBuf {
        if !cfg_bin.is_empty() {
            let real = crate::paths::eval_symlinks(Path::new(cfg_bin))
                .unwrap_or_else(|| cfg_bin.to_string());
            if let Some(root) = find_repo_root(&real) {
                return PathBuf::from(root);
            }
        }
        self.default_repo_dir()
    }

    /// `providedEngineBin`.
    pub(crate) fn provided_engine_bin(&self) -> String {
        let env = self
            .engine_bin_env
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .clone();
        let p = env.trim();
        if p.is_empty() {
            return String::new();
        }
        if is_file(Path::new(&self.engine_paths().prebuilt_resolve_bin(p))) {
            p.to_string()
        } else {
            String::new()
        }
    }

    /// `gitOutput` : sortie nettoyée, "" en cas d'échec.
    async fn git(&self, repo: &Path, args: &[&str]) -> String {
        self.probe
            .output_in(Some(repo), "git", args)
            .await
            .map(|o| String::from_utf8_lossy(&o).trim().to_string())
            .unwrap_or_default()
    }

    /// `listBackendBins` : un binaire par dossier de `backends/`.
    pub fn list_backend_bins(&self) -> Vec<(String, String)> {
        let root = format!("{}/backends", self.home_s());
        let Ok(rd) = std::fs::read_dir(&root) else {
            return Vec::new();
        };
        let mut names: Vec<String> = rd
            .flatten()
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .collect();
        names.sort();
        const SUBPATHS: [&str; 9] = [
            "build/bin/llama-server",
            "build-sm120/bin/llama-server",
            "build/llama-server",
            "bin/llama-server",
            "llama-server",
            "build/bin/Release/llama-server.exe",
            "build/bin/llama-server.exe",
            "build/bin/Release/llama-server",
            "llama-server.exe",
        ];
        let mut out = Vec::new();
        for name in names {
            if name.starts_with('.') {
                continue;
            }
            for sp in SUBPATHS {
                let p = format!("{root}/{name}/{sp}");
                if is_file(Path::new(&p)) {
                    out.push((name.clone(), p));
                    break;
                }
            }
        }
        out
    }

    /// `backendDir` : `backends/<nom>` sans évasion possible.
    fn backend_dir(&self, name: &str) -> Result<String, String> {
        let name = sanitize_backend_name(name);
        let root = crate::paths::go_abs(&self.backends_dir().to_string_lossy())
            .ok_or_else(|| "répertoire courant illisible".to_string())?;
        let p = crate::paths::go_join(&[&root, &name]);
        if !p.starts_with(&format!("{root}/")) {
            return Err("nom de backend invalide".into());
        }
        Ok(p)
    }
}

/// `findRepoRoot` : le dossier du dépôt llama.cpp au-dessus d'un binaire.
fn find_repo_root(bin: &str) -> Option<String> {
    let mut d = go_dir(bin);
    for _ in 0..6 {
        if is_dir(&Path::new(&d).join(".git")) || is_file(&Path::new(&d).join("CMakeLists.txt")) {
            return Some(d);
        }
        let parent = go_dir(&d);
        if parent == d {
            break;
        }
        d = parent;
    }
    None
}

/// `llamaServerBin` : le binaire compilé sous `repo`, "" sinon.
fn llama_server_bin(repo: &Path) -> String {
    let ext = if cfg!(windows) { ".exe" } else { "" };
    for rel in [
        format!("build/bin/Release/llama-server{ext}"),
        format!("build/bin/llama-server{ext}"),
        format!("build/Release/llama-server{ext}"),
        format!("build/llama-server{ext}"),
    ] {
        let p = go_clean(&repo.join(&rel).to_string_lossy());
        if is_file(Path::new(&p)) {
            return p;
        }
    }
    String::new()
}

/// `sanitizeBackendName`.
pub fn sanitize_backend_name(name: &str) -> String {
    let name = name.trim().to_lowercase();
    let mut b = String::new();
    for r in name.chars() {
        match r {
            'a'..='z' | '0'..='9' | '.' | '_' | '-' => b.push(r),
            ' ' | '/' | '\\' => b.push('-'),
            _ => {}
        }
    }
    let out = b.trim_matches(['.', '-']).to_string();
    if out.is_empty() {
        "backend".into()
    } else {
        out
    }
}

/// `engineOwns` / `engineTagOf`.
fn engine_tag_of(ep: &EnginePaths, bin: &str) -> String {
    if !ep.engine_owns(bin) {
        return String::new();
    }
    crate::paths::base_name(&go_dir(bin))
}

/// `strconv.Atoi` : 0 sur erreur de syntaxe, borné sur débordement.
pub fn go_atoi(s: &str) -> i64 {
    let digits = s.strip_prefix(['+', '-']).unwrap_or(s);
    if digits.is_empty() || !digits.bytes().all(|b| b.is_ascii_digit()) {
        return 0;
    }
    s.parse().unwrap_or(if s.starts_with('-') {
        i64::MIN
    } else {
        i64::MAX
    })
}

/// Journal d'un job tenu ici : chaque ligne va au job, dans l'ordre.
pub struct Log<'a> {
    pub jobs: &'a job::Jobs,
    pub store: &'a Store,
}

impl Log<'_> {
    pub async fn line(&self, s: &str) {
        self.jobs.append(self.store, s).await;
    }
    pub async fn phase(&self, s: &str) {
        self.jobs.phase(self.store, s).await;
    }
}

/// Le verrou de l'optimiseur (au Go) : le message s'il tient le moteur.
pub(crate) async fn tune_busy(store: &Store) -> Option<String> {
    let v = store
        .internal_json(Method::GET, "/internal/v1/tune/guard", None)
        .await
        .ok()?;
    (v.get("busy").and_then(Value::as_bool) == Some(true)).then(|| {
        v.get("error")
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_string()
    })
}

/// Le service moteur : celui de la façade si elle le pilote, sinon celui du
/// Go. `if_active` : seulement s'il tourne. Rend (lancé, erreur).
pub(crate) async fn service(ctx: &Ctx, action: &str, if_active: bool) -> (bool, String) {
    if let Some(ctl) = &ctx.engine_ctl {
        if if_active && !crate::enginectl::service_is_active(ctx) {
            return (false, String::new());
        }
        let err = crate::enginectl::service_action(ctx, ctl, action)
            .await
            .err()
            .unwrap_or_default();
        return (true, err);
    }
    match ctx
        .store
        .internal_json(
            Method::POST,
            "/internal/v1/service/action",
            Some(&json!({ "action": action, "if_active": if_active })),
        )
        .await
    {
        Ok(v) => (
            v.get("ran").and_then(Value::as_bool).unwrap_or(false),
            v.get("error")
                .and_then(Value::as_str)
                .unwrap_or("")
                .to_string(),
        ),
        Err(e) => (true, e),
    }
}

/// Arrête le moteur s'il tourne : (il tournait, erreur de l'arrêt).
pub(crate) async fn service_stop_if_up(ctx: &Ctx) -> (bool, String) {
    if ctx.engine_ctl.is_some() {
        if crate::enginectl::service_is_active(ctx) {
            return service(ctx, "stop", false).await;
        }
        return (false, String::new());
    }
    // Côté Go, « arrête-le s'il tourne » dit aussi s'il tournait.
    service(ctx, "stop", true).await
}

/// Les routes servies par ce module.
pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/llamacpp"
            | "/api/llamacpp/check"
            | "/api/llamacpp/prebuilt/check"
            | "/api/llamacpp/prebuilt"
            | "/api/llamacpp/use"
            | "/api/llamacpp/job"
            | "/api/llamacpp/uninstall-custom"
            | "/api/backends"
            | "/api/backends/custom"
            | "/api/backends/devices"
    ) || (cfg!(unix) && is_build_route(path))
}

/// Les routes de compilation depuis les sources (lot 41 ; Unix seulement,
/// la chaîne MSVC de Windows reste au Go).
fn is_build_route(path: &str) -> bool {
    matches!(
        path,
        "/api/llamacpp/install" | "/api/llamacpp/update" | "/api/llamacpp/install-custom"
    )
}

/// L'API interne du job ?
pub fn is_internal(path: &str) -> bool {
    path.starts_with(INTERNAL_PREFIX)
}

fn fail(code: StatusCode, e: impl Into<String>) -> Response<Body> {
    send_json(code, &json!({ "ok": false, "error": e.into() }))
}

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(
    ctx: &Ctx,
    b: &Arc<Backends>,
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
    let query = parts.uri.query().unwrap_or("");
    match parts.uri.path() {
        "/api/llamacpp" => send_json(StatusCode::OK, &status(b, store).await),
        "/api/llamacpp/check" => send_json(StatusCode::OK, &check(b, store).await),
        "/api/llamacpp/prebuilt/check" => send_json(StatusCode::OK, &prebuilt_check(b).await),
        "/api/llamacpp/prebuilt" => {
            if let Some(e) = tune_busy(store).await {
                return fail(StatusCode::CONFLICT, e);
            }
            if let Err(e) = b.jobs.start(store, "prebuilt").await {
                return fail(StatusCode::CONFLICT, e);
            }
            let (b2, ctx2) = (b.clone(), ctx.clone());
            tokio::spawn(async move {
                run_prebuilt(&ctx2, &b2).await;
                b2.jobs.end(&ctx2.store).await;
            });
            send_json(StatusCode::OK, &json!({ "ok": true }))
        }
        "/api/llamacpp/use" => {
            if let Some(e) = tune_busy(store).await {
                return send_json(
                    StatusCode::CONFLICT,
                    &json!({ "ok": false, "tuning": true, "error": e }),
                );
            }
            let d = gojson::decode_struct(&raw, USE_REQ_TYPE, &[("mode", Kind::Str)]);
            let cfg = store.config().await;
            let cfg_bin = cfg.get("BIN").cloned().unwrap_or_default();
            let bin = match d.str("mode").as_str() {
                "fast" => prebuilt::server_bin(&b.home),
                "opt" => llama_server_bin(&b.repo_dir(&cfg_bin)),
                _ => return fail(StatusCode::BAD_REQUEST, "mode inconnu"),
            };
            if bin.is_empty() {
                return fail(StatusCode::BAD_REQUEST, "cette version n'est pas installée");
            }
            if let Err(e) = store.put(BK_CONFIG, "BIN", bin.clone().into_bytes()).await {
                return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
            }
            let _ = service(ctx, "restart", true).await;
            send_json(StatusCode::OK, &json!({ "ok": true, "bin": bin }))
        }
        "/api/llamacpp/job" => {
            let from = query_param(query, "from");
            send_json(StatusCode::OK, &b.jobs.snapshot(go_atoi(&from), true).await)
        }
        "/api/llamacpp/uninstall-custom" => uninstall_custom(b, store, &raw).await,
        p if cfg!(unix) && is_build_route(p) => start_build(ctx, b, p, &raw).await,
        "/api/backends" => {
            let list: Vec<Value> = b
                .list_backend_bins()
                .into_iter()
                .map(|(name, path)| json!({ "name": name, "path": path }))
                .collect();
            send_json(StatusCode::OK, &json!(list))
        }
        "/api/backends/custom" => {
            let cfg_bin = store.config().await.get("BIN").cloned().unwrap_or_default();
            let list: Vec<Value> = b
                .list_backend_bins()
                .into_iter()
                .filter(|(name, _)| name != "llama.cpp" && name != "llama.cpp-prebuilt")
                .map(|(name, path)| {
                    let in_use = same_path(&path, &cfg_bin);
                    json!({ "name": name, "path": path, "in_use": in_use })
                })
                .collect();
            send_json(StatusCode::OK, &json!(list))
        }
        "/api/backends/devices" => {
            let d = gojson::decode_struct(&raw, BIN_REQ_TYPE, &[("bin", Kind::Str)]);
            let cfg = store.config().await;
            let mut bin = d.str("bin").trim().to_string();
            if bin.is_empty() {
                bin = cfg.get("BIN").cloned().unwrap_or_default();
            }
            let bin = b.engine_paths().prebuilt_resolve_bin(&bin);
            if bin.is_empty() || !is_file(Path::new(&bin)) {
                return send_json(
                    StatusCode::OK,
                    &json!({ "ok": false, "error": "moteur introuvable — choisissez d'abord un moteur" }),
                );
            }
            if !devices::is_llama_server(&bin) {
                return fail(
                    StatusCode::BAD_REQUEST,
                    "ce chemin n'est pas un llama-server",
                );
            }
            let cvd = cfg.get("CUDA_VISIBLE_DEVICES").cloned().unwrap_or_default();
            send_json(StatusCode::OK, &b.devices.list(&b.probe, &bin, &cvd).await)
        }
        _ => fail(StatusCode::NOT_FOUND, "route inconnue"),
    }
}

/// `handleLlamacppInstall`, `handleLlamacppUpdate`,
/// `handleLlamacppInstallCustom` : le job de compilation lancé.
async fn start_build(ctx: &Ctx, b: &Arc<Backends>, path: &str, raw: &[u8]) -> Response<Body> {
    let provided = b.provided_engine_bin();
    if !provided.is_empty() {
        return fail(
            StatusCode::CONFLICT,
            format!(
                "Le moteur est fourni par l'image Docker ({provided}) : il n'y a rien à compiler ici, \
                 et l'image ne contient ni cmake ni compilateur. Pour le mettre à jour, utilise \
                 « mettre à jour le moteur » dans Réglages → Moteur."
            ),
        );
    }
    enum Run {
        Install(bool),
        Update(bool),
        Custom(String, String, String),
    }
    let (action, run) = match path {
        "/api/llamacpp/install" => {
            let d = gojson::decode_struct(raw, INSTALL_REQ_TYPE, &[("force", Kind::Bool)]);
            ("install", Run::Install(d.bool("force")))
        }
        "/api/llamacpp/update" => {
            let d = gojson::decode_struct(raw, UPDATE_REQ_TYPE, &[("clean", Kind::Bool)]);
            ("update", Run::Update(d.bool("clean")))
        }
        _ => {
            let d = gojson::decode_struct(
                raw,
                CUSTOM_REQ_TYPE,
                &[("repo", Kind::Str), ("name", Kind::Str), ("ref", Kind::Str)],
            );
            if d.str("repo").trim().is_empty() {
                return fail(StatusCode::BAD_REQUEST, "URL du dépôt requise");
            }
            (
                "custom",
                Run::Custom(d.str("repo"), d.str("name"), d.str("ref")),
            )
        }
    };
    if let Some(e) = tune_busy(&ctx.store).await {
        return fail(StatusCode::CONFLICT, e);
    }
    if let Err(e) = b.jobs.start(&ctx.store, action).await {
        return fail(StatusCode::CONFLICT, e);
    }
    let (b2, ctx2) = (b.clone(), ctx.clone());
    tokio::spawn(async move {
        match run {
            Run::Install(force) => build::run_install(&ctx2, &b2, force).await,
            Run::Update(clean) => build::run_update(&ctx2, &b2, clean).await,
            Run::Custom(url, name, r) => build::run_custom(&ctx2, &b2, &url, &name, &r).await,
        }
        b2.jobs.end(&ctx2.store).await;
    });
    send_json(StatusCode::OK, &json!({ "ok": true }))
}

/// Valeur d'un paramètre de requête (dernière occurrence ignorée : la
/// première, comme `Query().Get`).
fn query_param(query: &str, key: &str) -> String {
    url::form_urlencoded::parse(query.as_bytes())
        .find(|(k, _)| k == key)
        .map(|(_, v)| v.into_owned())
        .unwrap_or_default()
}

/// `handleLlamacpp` : le statut complet.
pub async fn status(b: &Backends, store: &Store) -> Value {
    let cfg = store.config().await;
    let cfg_bin = cfg.get("BIN").cloned().unwrap_or_default();
    let repo = b.repo_dir(&cfg_bin);
    let installed = is_dir(&repo.join(".git"));
    let mut out = json!({
        "repo": repo.to_string_lossy(),
        "installed": installed,
    });
    if installed {
        let branch = b.git(&repo, &["rev-parse", "--abbrev-ref", "HEAD"]).await;
        out["commit"] = json!(b.git(&repo, &["rev-parse", "--short", "HEAD"]).await);
        out["commit_date"] = json!(b.git(&repo, &["log", "-1", "--format=%ci"]).await);
        out["commit_msg"] = json!(b.git(&repo, &["log", "-1", "--format=%s"]).await);
        if !branch.is_empty() && branch != "HEAD" {
            let behind = b
                .git(
                    &repo,
                    &["rev-list", "--count", &format!("HEAD..origin/{branch}")],
                )
                .await;
            if !behind.is_empty() {
                out["behind"] = json!(behind.parse::<i64>().unwrap_or(0));
            }
        }
        out["branch"] = json!(branch);
    }
    let bin = llama_server_bin(&repo);
    let ep = b.engine_paths();
    out["bin"] = json!(bin);
    out["config_bin"] = json!(cfg_bin);
    out["in_use"] = json!(!bin.is_empty() && same_path(&bin, &cfg_bin));
    let provided = b.provided_engine_bin();
    out["provided"] = json!(!provided.is_empty());
    out["provided_bin"] = json!(provided);
    out["engine"] = json!({
        "dir": b.home.join("engine").to_string_lossy(),
        "in_use": ep.engine_owns(&cfg_bin),
        "tag": engine_tag_of(&ep, &cfg_bin),
    });
    let plan = b.probe.detect_build_plan().await;
    out["plan"] = json!({ "backend": plan.backend, "arch": plan.cuda_arch, "jobs": plan.jobs });
    let (pb_tag, _) = prebuilt::version(&b.home);
    let pb_bin = prebuilt::server_bin(&b.home);
    out["prebuilt"] = json!({
        "tag": pb_tag,
        "bin": pb_bin,
        "dir": prebuilt::dir(&b.home).to_string_lossy(),
        "in_use": !pb_bin.is_empty() && ep.prebuilt_owns(&cfg_bin),
    });
    out["backends_dir"] = json!(b.backends_dir().to_string_lossy());
    out["reco"] = prebuilt::recommended_mode(plan.backend, b.target.os);
    out["job"] = b.jobs.snapshot(0, false).await;
    out
}

/// `runStep` (sans terminal interactif) : la commande, sorties sur celles de
/// la façade ; l'erreur au format d'`exec`.
async fn run_step(probe: &Probe, dir: &Path, name: &str, args: &[&str]) -> Result<(), String> {
    let Some(bin) = probe.look_path(name) else {
        return Err(format!(
            "exec: \"{name}\": executable file not found in $PATH"
        ));
    };
    let status = tokio::process::Command::new(&bin)
        .args(args)
        .current_dir(dir)
        .stdin(Stdio::null())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit())
        .status()
        .await
        .map_err(|e| crate::paths::os_err("fork/exec", &bin.to_string_lossy(), &e))?;
    match status.code() {
        Some(0) => Ok(()),
        Some(c) => Err(format!("exit status {c}")),
        None => Err(status.to_string()),
    }
}

/// `handleLlamacppCheck` : git fetch, puis le retard sur origin.
async fn check(b: &Backends, store: &Store) -> Value {
    let cfg_bin = store.config().await.get("BIN").cloned().unwrap_or_default();
    let repo = b.repo_dir(&cfg_bin);
    if !is_dir(&repo.join(".git")) {
        return json!({ "ok": false, "error": "llama.cpp n'est pas installé" });
    }
    let mut branch = b.git(&repo, &["rev-parse", "--abbrev-ref", "HEAD"]).await;
    if branch.is_empty() || branch == "HEAD" {
        branch = "master".into();
    }
    if let Err(e) = run_step(&b.probe, &repo, "git", &["fetch", "origin", "--quiet"]).await {
        return json!({ "ok": false, "error": format!("git fetch a échoué : {e}") });
    }
    let origin = format!("origin/{branch}");
    let behind_s = b
        .git(&repo, &["rev-list", "--count", &format!("HEAD..{origin}")])
        .await;
    let behind = if behind_s.is_empty() {
        0
    } else {
        behind_s.parse::<i64>().unwrap_or(0)
    };
    let has_binary = !llama_server_bin(&repo).is_empty();
    json!({
        "ok": true,
        "behind": behind,
        "local": b.git(&repo, &["rev-parse", "--short", "HEAD"]).await,
        "remote": b.git(&repo, &["rev-parse", "--short", &origin]).await,
        "remote_date": b.git(&repo, &["log", "-1", "--format=%ci", &origin]).await,
        "remote_msg": b.git(&repo, &["log", "-1", "--format=%s", &origin]).await,
        "branch": branch,
        "has_binary": has_binary,
        "needs_rebuild": behind > 0 || !has_binary,
    })
}

/// La dernière release officielle (`fetchLlamaLatest`).
async fn fetch_latest(b: &Backends) -> Result<(String, Vec<prebuilt::Asset>), String> {
    prebuilt::fetch_latest(&b.client, &b.releases_api).await
}

/// `handleLlamacppPrebuiltCheck`.
async fn prebuilt_check(b: &Backends) -> Value {
    let (tag, assets) = match fetch_latest(b).await {
        Ok(r) => r,
        Err(e) => return json!({ "ok": false, "error": e }),
    };
    let pick = match prebuilt::pick(&assets, &b.probe, b.target).await {
        Ok(p) => p,
        Err(e) => return json!({ "ok": false, "error": e }),
    };
    let (cur, _) = prebuilt::version(&b.home);
    let update = cur != tag || prebuilt::server_bin(&b.home).is_empty();
    json!({
        "ok": true,
        "latest": tag,
        "current": cur,
        "variant": pick.label,
        "size_mb": pick.main.size / 1_000_000,
        "update": update,
    })
}

/// `lcRunPrebuilt` : installe, pointe BIN, service arrêté pendant le
/// remplacement puis relancé.
async fn run_prebuilt(ctx: &Ctx, b: &Backends) {
    let log = Log {
        jobs: &b.jobs,
        store: &ctx.store,
    };
    let (svc_was_up, err) = service_stop_if_up(ctx).await;
    if svc_was_up {
        log.phase("arrêt du service le temps de l'installation…")
            .await;
    }
    if svc_was_up && !err.is_empty() {
        log.line(&format!("[warn] impossible d'arrêter le service : {err}"))
            .await;
    }
    let mut res = install(b, &log).await;
    if let Ok(bin) = &res {
        match ctx
            .store
            .put(BK_CONFIG, "BIN", bin.clone().into_bytes())
            .await
        {
            Err(e) => {
                res = Err(format!(
                    "binaires installés mais échec d'écriture de BIN : {e}"
                ))
            }
            Ok(()) => log.line("BIN mis à jour").await,
        }
    }
    if svc_was_up {
        log.phase("redémarrage du service…").await;
        let (_, e) = service(ctx, "start", false).await;
        if !e.is_empty() {
            log.line(&format!("[warn] redémarrage du service échoué : {e}"))
                .await;
        }
    }
    match res {
        Err(e) => b.jobs.fail(&ctx.store, &e).await,
        Ok(_) => {
            let (tag, _) = prebuilt::version(&b.home);
            b.jobs
                .done(
                    &ctx.store,
                    &format!("binaires précompilés installés ({tag})"),
                )
                .await;
        }
    }
}

/// `prebuiltInstall` : télécharge et installe la dernière release ; rend le
/// chemin du binaire.
pub async fn install(b: &Backends, log: &Log<'_>) -> Result<String, String> {
    log.phase("récupération de la dernière release llama.cpp…")
        .await;
    let (tag, assets) = fetch_latest(b)
        .await
        .map_err(|e| format!("impossible d'interroger les releases llama.cpp : {e}"))?;
    let (cur_tag, cur_cuda) = prebuilt::version(&b.home);
    let pick = prebuilt::pick(&assets, &b.probe, b.target).await?;
    log.line(&format!("release {tag} — variant retenu : {}", pick.label))
        .await;
    let cur = prebuilt::server_bin(&b.home);
    if cur_tag == tag && prebuilt::version_format(&b.home) == prebuilt::FORMAT && !cur.is_empty() {
        log.line(&format!("déjà à jour ({tag})")).await;
        prune(b, &cur, log).await;
        return Ok(cur);
    }
    let dir = prebuilt::dir(&b.home);
    std::fs::create_dir_all(&dir)
        .map_err(|e| crate::paths::os_err("mkdir", &dir.to_string_lossy(), &e))?;
    let mut cudart = pick.cudart.clone();
    if cudart.is_some() && !cudart_present(&dir).is_empty() && cur_cuda == pick.cuda_ver {
        log.line(&format!(
            "cudart {} déjà présent — téléchargement évité",
            pick.cuda_ver
        ))
        .await;
        cudart = None;
    }
    for a in [Some(pick.main.clone()), cudart].into_iter().flatten() {
        log.phase(&format!(
            "téléchargement de {} ({} Mo)…",
            a.name,
            a.size / 1_000_000
        ))
        .await;
        let tmp = dir.join(format!("{}.part", a.name));
        if let Err(e) = download(b, &a, &tmp, log).await {
            let _ = std::fs::remove_file(&tmp);
            return Err(format!("téléchargement de {} : {e}", a.name));
        }
        log.phase(&format!("extraction de {}…", a.name)).await;
        let (t2, d2) = (tmp.clone(), dir.clone());
        let ex = tokio::task::spawn_blocking(move || prebuilt::extract(&t2, &d2))
            .await
            .unwrap_or_else(|e| Err(e.to_string()));
        if let Err(e) = ex {
            let _ = std::fs::remove_file(&tmp);
            return Err(format!("extraction de {} : {e}", a.name));
        }
        let _ = std::fs::remove_file(&tmp);
    }
    let vpath = dir.join("VERSION");
    std::fs::write(
        &vpath,
        format!("{tag} {} {}\n", pick.cuda_ver, prebuilt::FORMAT),
    )
    .map_err(|e| crate::paths::os_err("open", &vpath.to_string_lossy(), &e))?;
    let bin = prebuilt::server_bin(&b.home);
    if bin.is_empty() {
        return Err(format!(
            "archives extraites mais llama-server introuvable sous {}",
            dir.to_string_lossy()
        ));
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = std::fs::set_permissions(&bin, std::fs::Permissions::from_mode(0o755));
    }
    prune(b, &bin, log).await;
    log.line(&format!("binaire installé : {bin} (release {tag})"))
        .await;
    Ok(bin)
}

/// Le téléchargement d'un asset, sa progression au journal du job.
async fn download(
    b: &Backends,
    a: &prebuilt::Asset,
    dest: &Path,
    log: &Log<'_>,
) -> Result<(), String> {
    let rq = b
        .client
        .get(&a.url)
        .header("User-Agent", "Go-http-client/1.1");
    log_download(log, rq, dest, a.size).await
}

/// Un téléchargement dont la progression part au journal du job au fil de
/// l'eau, dans l'ordre.
pub async fn log_download(
    log: &Log<'_>,
    rq: reqwest::RequestBuilder,
    dest: &Path,
    total: i64,
) -> Result<(), String> {
    let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel::<String>();
    let dl = async move {
        let mut logf = |s: String| {
            let _ = tx.send(s);
        };
        prebuilt::download_request(rq, dest, total, &mut logf).await
    };
    let pump = async {
        while let Some(s) = rx.recv().await {
            log.line(&s).await;
        }
    };
    let (res, ()) = tokio::join!(dl, pump);
    res
}

/// cudart déjà extrait (`filepath.Glob(dir/**/cudart64*.dll)` puis
/// `dir/cudart64*.dll` : `**` vaut `*` pour Glob).
fn cudart_present(dir: &Path) -> Vec<PathBuf> {
    let is_dll = |n: &str| n.starts_with("cudart64") && n.ends_with(".dll");
    let mut found = Vec::new();
    if let Ok(rd) = std::fs::read_dir(dir) {
        for e in rd.flatten() {
            if let Ok(sub) = std::fs::read_dir(e.path()) {
                for s in sub.flatten() {
                    if is_dll(&s.file_name().to_string_lossy()) {
                        found.push(s.path());
                    }
                }
            }
        }
    }
    if found.is_empty()
        && let Ok(rd) = std::fs::read_dir(dir)
    {
        for e in rd.flatten() {
            if is_dll(&e.file_name().to_string_lossy()) {
                found.push(e.path());
            }
        }
    }
    found
}

/// `prebuiltPrune`, ses lignes au job.
async fn prune(b: &Backends, keep: &str, log: &Log<'_>) {
    let mut lines = Vec::new();
    prebuilt::prune(&b.home, keep, &mut |s| lines.push(s));
    for l in lines {
        log.line(&l).await;
    }
}

/// `handleLlamacppUninstallCustom`.
async fn uninstall_custom(b: &Backends, store: &Store, raw: &[u8]) -> Response<Body> {
    let d = gojson::decode_struct(raw, NAME_REQ_TYPE, &[("name", Kind::Str)]);
    let name = sanitize_backend_name(&d.str("name"));
    if name.is_empty() {
        return fail(StatusCode::BAD_REQUEST, "nom requis");
    }
    if name == "llama.cpp" || name == "llama.cpp-prebuilt" {
        return fail(
            StatusCode::BAD_REQUEST,
            "ce moteur se gère depuis les cartes ⚡ / 🔧",
        );
    }
    let dir = match b.backend_dir(&name) {
        Ok(d) => d,
        Err(e) => return fail(StatusCode::BAD_REQUEST, e),
    };
    if !is_dir(Path::new(&dir)) {
        return fail(StatusCode::NOT_FOUND, "backend introuvable");
    }
    let cfg_bin = store.config().await.get("BIN").cloned().unwrap_or_default();
    for (n, p) in b.list_backend_bins() {
        if n == name && same_path(&p, &cfg_bin) {
            return fail(
                StatusCode::CONFLICT,
                "backend utilisé par le modèle actif — bascule d'abord ce modèle sur un autre moteur",
            );
        }
    }
    if let Err(e) = std::fs::remove_dir_all(&dir) {
        return fail(
            StatusCode::INTERNAL_SERVER_ERROR,
            crate::paths::os_err("unlinkat", &dir, &e),
        );
    }
    send_json(StatusCode::OK, &json!({ "ok": true }))
}

/// L'API interne du job : le Go y rapporte les étapes de ses jobs.
pub async fn serve_internal(ctx: &Ctx, b: &Backends, req: Request<Incoming>) -> Response<Body> {
    let (parts, body) = req.into_parts();
    let raw = Limited::new(body, 1 << 20)
        .collect()
        .await
        .map(|b| b.to_bytes().to_vec())
        .unwrap_or_default();
    let v: Value = serde_json::from_slice(&raw).unwrap_or(Value::Null);
    let s = |k: &str| v.get(k).and_then(Value::as_str).unwrap_or("").to_string();
    let store = &ctx.store;
    let op = parts.uri.path().trim_start_matches(INTERNAL_PREFIX);
    if op == "snapshot" {
        let q = parts.uri.query().unwrap_or("");
        let from = go_atoi(&query_param(q, "from"));
        let lines = query_param(q, "lines") == "1";
        return send_json(StatusCode::OK, &b.jobs.snapshot(from, lines).await);
    }
    if parts.method != Method::POST {
        return fail(StatusCode::METHOD_NOT_ALLOWED, "POST attendu");
    }
    match op {
        "start" => {
            if let Err(e) = b.jobs.start(store, &s("action")).await {
                return fail(StatusCode::CONFLICT, e);
            }
            return send_json(StatusCode::OK, &json!({ "ok": true }));
        }
        "line" => b.jobs.append(store, &s("line")).await,
        "phase" => b.jobs.phase(store, &s("phase")).await,
        "fail" => b.jobs.fail(store, &s("error")).await,
        "done" => b.jobs.done(store, &s("msg")).await,
        "end" => b.jobs.end(store).await,
        "commits" => {
            let old = v.get("old").and_then(Value::as_str);
            let new = v.get("new").and_then(Value::as_str);
            b.jobs.set_commits(old, new).await;
        }
        _ => return fail(StatusCode::NOT_FOUND, "route inconnue"),
    }
    send_json(StatusCode::OK, &json!({ "ok": true }))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sanitize() {
        assert_eq!(sanitize_backend_name("  Mon Fork/Test "), "mon-fork-test");
        assert_eq!(sanitize_backend_name("../.."), "backend");
        assert_eq!(sanitize_backend_name(""), "backend");
        assert_eq!(sanitize_backend_name("é-x."), "x");
    }

    #[test]
    fn atoi() {
        assert_eq!(go_atoi("12"), 12);
        assert_eq!(go_atoi("-3"), -3);
        assert_eq!(go_atoi("+4"), 4);
        assert_eq!(go_atoi("x"), 0);
        assert_eq!(go_atoi(""), 0);
        assert_eq!(go_atoi("99999999999999999999"), i64::MAX);
    }
}
