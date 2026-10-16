//! Pilotage du moteur tenu par la façade (lot 16 de la migration,
//! `LOKI_ENGINE=rust` ; expérimental, le Go reste le défaut) :
//!
//! - `/api/start|stop|restart` — port de `svcHandler` (`web_api.go`) et de la
//!   voie conteneur de `serviceAction` (`sys_service.go`,
//!   `sys_service_rust_linux.go`) : la façade lance elle-même
//!   `loki-rs engine <action>`, le superviseur que le Go appelait déjà ;
//! - `/api/vram/unload` et `/api/vram/reload` — port de `handleVramUnload` et
//!   `handleVramReload` (`web_vram.go`), bilan de VRAM (`gpuSettle`) compris ;
//! - `preflightEngine` (`sys_service.go`) : ce sans quoi le moteur ne PEUT
//!   pas démarrer, dit avant de le lancer.
//!
//! Restent au Go, derrière son API interne, jusqu'au lot qui les portera : le
//! verrou de l'optimiseur (`/internal/v1/service/prestart` avant tout
//! démarrage, `/internal/v1/tune/guard`), la génération en cours quand il
//! tient la conversation (`/internal/v1/chat/generating`) et l'arrêt de la
//! dictée quand il la tient (`/internal/v1/dictate/stop`).
//!
//! La façade ne tient ce lot qu'en conteneur, avec son superviseur : sous
//! systemd ou avec `LOKI_ENGINE_SUPERVISOR=go`, le Go le garde ([`usable`]).

use std::path::{Path, PathBuf};
use std::sync::LazyLock;
use std::time::Duration;

use http_body_util::{BodyExt, Limited};
use hyper::body::Incoming;
use hyper::header::{ALLOW, HeaderValue};
use hyper::{Method, Request, Response, StatusCode};
use regex::Regex;
use serde_json::{Value, json};

use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::gguf;
use crate::gojson::{self, Kind};
use crate::paths::{go_dir, go_is_abs, go_join};
use crate::proxy::Body;
use crate::serve::{self, Cfg};
use crate::store::Store;
use crate::sysinfo::{self, GpuStat};

/// Ce que la façade sait du superviseur du moteur.
pub struct EngineCtl {
    /// Le binaire lancé en `engine <action>` (`loki-rs` lui-même).
    pub bin: PathBuf,
    /// Variables passées au superviseur, en plus de l'environnement de la
    /// façade : `LOKI_GO_BIN`, `LOKI_HOME`, `LOKI_SERVICE` (comme
    /// `rustSvcAction`), et l'adresse et le jeton de l'API interne du Go, que
    /// `loki-rs engine serve` (`LOKI_SERVE=rust`) lit.
    pub env: Vec<(String, String)>,
    /// nvidia-smi (un faux dans les tests).
    pub nvidia_smi: String,
    /// Pause entre deux lectures du bilan de VRAM (500 ms).
    pub settle_pause: Duration,
}

impl EngineCtl {
    pub fn new(bin: PathBuf, env: Vec<(String, String)>) -> Self {
        EngineCtl {
            bin,
            env,
            nvidia_smi: "nvidia-smi".into(),
            settle_pause: Duration::from_millis(500),
        }
    }

    /// `rustSvcAction` : `loki-rs engine <action>`. Sa sortie s'affiche telle
    /// quelle ; en cas d'échec, sa dernière ligne d'erreur devient l'erreur
    /// rendue (c'est elle que l'UI montre), sans le préfixe du binaire.
    pub async fn run(&self, action: &str) -> Result<(), String> {
        use std::io::Write;
        let out = tokio::process::Command::new(&self.bin)
            .args(["engine", action])
            .envs(self.env.iter().map(|(k, v)| (k.as_str(), v.as_str())))
            .stdin(std::process::Stdio::null())
            .output()
            .await
            .map_err(|e| format!("loki-rs engine {action} : {e}"))?;
        let _ = std::io::stdout().write_all(&out.stdout);
        let _ = std::io::stderr().write_all(&out.stderr);
        if out.status.success() {
            return Ok(());
        }
        let msg = last_error_line(&String::from_utf8_lossy(&out.stderr));
        if !msg.is_empty() {
            return Err(msg);
        }
        Err(format!(
            "loki-rs engine {action} : {}",
            exit_text(&out.status)
        ))
    }

    /// `gpuStats` : lecture FRAÎCHE, vide sans mesure.
    async fn gpus(&self) -> Vec<GpuStat> {
        sysinfo::nvidia_smi_query_bin(&self.nvidia_smi)
            .await
            .map(|o| sysinfo::parse_nvidia_smi(&String::from_utf8_lossy(&o)))
            .unwrap_or_default()
    }
}

/// `ProcessState.String()` du Go : « exit status 1 », « signal: killed ».
fn exit_text(st: &std::process::ExitStatus) -> String {
    if let Some(c) = st.code() {
        return format!("exit status {c}");
    }
    #[cfg(unix)]
    {
        use std::os::unix::process::ExitStatusExt;
        if let Some(s) = st.signal() {
            let name = match s {
                1 => "hangup".to_string(),
                2 => "interrupt".to_string(),
                6 => "aborted".to_string(),
                9 => "killed".to_string(),
                11 => "segmentation fault".to_string(),
                15 => "terminated".to_string(),
                n => format!("signal {n}"),
            };
            return format!("signal: {name}");
        }
    }
    st.to_string()
}

/// La façade peut-elle tenir ce lot ? `Err(raison)` sinon : sous systemd
/// (`systemdAvailable`), le Go pilote son unité ; `LOKI_ENGINE_SUPERVISOR=go`
/// est la porte de sortie du superviseur Rust.
pub fn usable(container_env: Option<&str>, supervisor_env: Option<&str>) -> Result<(), String> {
    if supervisor_env.is_some_and(|v| v.trim().eq_ignore_ascii_case("go")) {
        return Err("LOKI_ENGINE_SUPERVISOR=go".into());
    }
    if container_env != Some("1") && Path::new("/run/systemd/system").is_dir() {
        return Err("systemd pilote le moteur".into());
    }
    Ok(())
}

/// `lastErrorLine`.
pub fn last_error_line(s: &str) -> String {
    let t = s.trim();
    let last = t.rsplit('\n').next().unwrap_or("").trim();
    last.strip_prefix("[loki-rs engine]")
        .unwrap_or(last)
        .trim()
        .to_string()
}

static ANSI: LazyLock<Regex> = LazyLock::new(|| Regex::new("\x1b\\[[0-9;]*m").expect("motif ANSI"));

/// `plainErr` : les séquences ANSI des messages écrits pour le terminal
/// retirées.
pub fn plain_err(s: &str) -> String {
    ANSI.replace_all(s, "").into_owned()
}

/// `gpuUsedMB`.
pub fn gpu_used_mb(gpus: &[GpuStat]) -> i64 {
    gpus.iter().map(|g| g.used).sum()
}

/// `gpuSettle` : attend que la VRAM ait fini de redescendre avant de la
/// relire. Une mesure ratée n'est pas « 0 Mo » (sautée) ; un palier ne
/// conclut qu'une fois la baisse commencée ; deux lectures égales de suite
/// après la baisse, et la valeur est rendue — sinon la dernière lecture après
/// huit essais. `sample` rend (Mo occupés, lecture réussie).
pub async fn gpu_settle<F, Fut>(before: i64, mut sample: F, pause: Duration) -> i64
where
    F: FnMut() -> Fut,
    Fut: std::future::Future<Output = (i64, bool)>,
{
    let (mut last, mut lu) = (before, false);
    let (mut baisse, mut palier) = (false, 0);
    for _ in 0..8 {
        if !pause.is_zero() {
            tokio::time::sleep(pause).await;
        }
        let (cur, ok) = sample().await;
        if !ok {
            continue;
        }
        if cur < before {
            baisse = true;
        }
        if lu && cur == last {
            palier += 1;
        } else {
            palier = 0;
        }
        (last, lu) = (cur, true);
        if baisse && palier >= 1 {
            return cur;
        }
    }
    last
}

/// `strataPreflight`.
fn strata_preflight(cfg: &Cfg) -> Result<(), String> {
    let path = serve::trim(serve::get(cfg, "STRATA_CONFIG"));
    if path.is_empty() {
        return Err(
            "STRATA_CONFIG non défini : réinstalle le modèle depuis Paramètres → Moteur → Strata"
                .into(),
        );
    }
    if std::fs::metadata(path).is_err() {
        return Err(format!(
            "configuration de Strata introuvable : {path} — réinstalle le modèle"
        ));
    }
    let py = go_join(&[&go_dir(path), ".venv", "bin", "python"]);
    if std::fs::metadata(&py).is_err() {
        return Err(format!(
            "environnement Python de Strata absent ({py}) : réinstalle le modèle"
        ));
    }
    Ok(())
}

/// `preflightEngine` (message déjà débarrassé de ses couleurs, comme
/// `plainErr` le rend à l'UI). Entrées/sorties bloquantes.
///
/// - `stored_dirs` / `env_model_dirs` : voir
///   [`gguf::resolve_serve_model_path`].
pub fn preflight_engine(
    home: &Path,
    cfg: &Cfg,
    stored_dirs: &[String],
    env_model_dirs: Option<&str>,
) -> Result<(), String> {
    // Preset externe : aucun llama-server local à lancer.
    if serve::is_external(cfg) {
        return Ok(());
    }
    // Strata n'a ni BIN ni MODEL : son installeur a tout rangé.
    if serve::is_strata(cfg) {
        return strata_preflight(cfg);
    }
    let mut bin = serve::trim(serve::get(cfg, "BIN")).to_string();
    if bin.is_empty() {
        return Err(
            "BIN non défini — installe un moteur : loki llamacpp install (ou renseigne BIN avec loki edit)"
                .into(),
        );
    }
    if !go_is_abs(&bin) {
        bin = go_join(&[&home.to_string_lossy(), &bin]);
    }
    let eng = serve::run::EnginePaths {
        home: home.to_path_buf(),
    };
    if std::fs::metadata(eng.prebuilt_resolve_bin(&bin)).is_err() {
        return Err(format!(
            "moteur introuvable : {bin} — relance loki llamacpp install"
        ));
    }
    let model = serve::trim(serve::get(cfg, "MODEL"));
    if model.is_empty() {
        return Err(format!(
            "MODEL non défini — indique un .gguf avec loki edit (dossier des modèles : {})",
            gguf::models_dir(home)
        ));
    }
    let p = gguf::resolve_serve_model_path(model, home, stored_dirs, env_model_dirs)
        .map_err(|e| format!("MODEL={model} : {e}"))?;
    let p = p.to_string_lossy().into_owned();
    if std::fs::metadata(&p).is_err() {
        return Err(format!(
            "modèle introuvable : {p} — corrige MODEL avec loki edit"
        ));
    }
    // Modèle en plusieurs fichiers : une tranche manquante ne se voit qu'au
    // moment où llama-server réclame un tenseur absent.
    let (dir, base) = (go_dir(&p), go_base(&p));
    let missing = gguf::shard_family_missing(Path::new(&dir), &base);
    if !missing.is_empty() {
        return Err(format!(
            "modèle incomplet : il manque {} dans {dir} — ce modèle tient en {} fichiers, télécharge-les tous",
            missing.join(", "),
            gguf::shard_family(&base).len()
        ));
    }
    Ok(())
}

/// `filepath.Base`.
fn go_base(p: &str) -> String {
    let t = p.trim_end_matches('/');
    if t.is_empty() {
        return if p.is_empty() { ".".into() } else { "/".into() };
    }
    t.rsplit('/').next().unwrap_or(t).to_string()
}

/// Les routes servies par ce module.
pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/start" | "/api/stop" | "/api/restart" | "/api/vram/unload" | "/api/vram/reload"
    )
}

/// Avant un démarrage : refus pendant une optimisation, puis ménage d'un
/// verrou périmé — au Go (`enginePrestart`).
async fn prestart(store: &Store) -> Result<(), String> {
    let (code, v) = store
        .internal_post("/internal/v1/service/prestart", &json!({}))
        .await
        .map_err(|e| format!("verrou de l'optimiseur : {e}"))?;
    if !code.is_success() {
        return Err(format!("verrou de l'optimiseur : {code}"));
    }
    match v.get("error").and_then(Value::as_str) {
        Some(e) if !e.is_empty() => Err(e.to_string()),
        _ => Ok(()),
    }
}

/// `serviceAction` en conteneur : garde-fous du Go avant un démarrage, preset
/// externe (rien à lancer : `loki serve` sortirait aussitôt et le superviseur
/// le prendrait pour un plantage), puis le superviseur.
pub async fn service_action(ctx: &Ctx, ctl: &EngineCtl, action: &str) -> Result<(), String> {
    if action == "start" || action == "restart" {
        prestart(&ctx.store).await?;
    }
    match action {
        "start" | "restart" => {
            if serve::is_external(&ctx.store.config().await) {
                if action == "restart" {
                    let _ = ctl.run("stop").await;
                }
                println!("[info] preset externe actif : pas de moteur local à démarrer");
                return Ok(());
            }
            ctl.run(action).await
        }
        "stop" | "status" => ctl.run(action).await,
        other => Err(format!("action inconnue: {other}")),
    }
}

/// `serviceIsActive` en conteneur : le PID du fichier vit.
pub(crate) fn service_is_active(ctx: &Ctx) -> bool {
    pid_alive(crate::engine::read_pid(&ctx.engine.pid()))
}

#[cfg(target_os = "linux")]
fn pid_alive(pid: i32) -> bool {
    crate::engine::proctree::linux::pid_alive(pid)
}

#[cfg(not(target_os = "linux"))]
fn pid_alive(_: i32) -> bool {
    false
}

/// La conversation écrit-elle une réponse ? Celle de la façade si elle la
/// tient, sinon celle du Go.
pub(crate) async fn generating(ctx: &Ctx) -> bool {
    if let Some(chat) = &ctx.chat {
        return chat.sessions.conv().is_generating();
    }
    ctx.store
        .internal_json(Method::GET, "/internal/v1/chat/generating", None)
        .await
        .ok()
        .and_then(|v| v.get("generating").and_then(Value::as_bool))
        .unwrap_or(false)
}

/// `asrShutdownVite` : la dictée de la façade si elle la tient, sinon celle
/// du Go.
pub(crate) async fn dictate_stop(ctx: &Ctx) {
    if let Some(d) = &ctx.dictate {
        d.shutdown_fast().await;
        return;
    }
    if let Err(e) = ctx
        .store
        .internal_post("/internal/v1/dictate/stop", &json!({}))
        .await
    {
        eprintln!("[dictée] arrêt demandé au Go : {e}");
    }
}

/// Le verrou de l'optimiseur (au Go) : `Some(réponse 409)` s'il tient le
/// moteur.
async fn tune_deny(store: &Store) -> Option<Response<Body>> {
    let v = store
        .internal_json(Method::GET, "/internal/v1/tune/guard", None)
        .await
        .ok()?;
    if v.get("busy").and_then(Value::as_bool) == Some(true) {
        return Some(send_json(
            StatusCode::CONFLICT,
            &json!({ "ok": false, "tuning": true, "error": v.get("error").and_then(Value::as_str).unwrap_or("") }),
        ));
    }
    None
}

/// `postOnly` : ces routes arrêtent ou relancent des processus ; un simple GET
/// (une balise `<img>` sur une page tierce) ne doit pas y suffire.
fn post_only(method: &Method) -> Option<Response<Body>> {
    if method == Method::POST {
        return None;
    }
    let mut r = send_json(
        StatusCode::METHOD_NOT_ALLOWED,
        &json!({ "ok": false, "error": format!("méthode {method} refusée : POST attendu") }),
    );
    r.headers_mut()
        .insert(ALLOW, HeaderValue::from_static("POST"));
    Some(r)
}

const UNLOAD_REQ_TYPE: &str = r#"struct { Force bool "json:\"force\"" }"#;

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(
    ctx: &Ctx,
    ctl: &EngineCtl,
    req: Request<Incoming>,
    from_go: bool,
) -> Response<Body> {
    let (parts, body) = req.into_parts();
    if !from_go && let Err(refus) = ctx.guard(&parts.headers).await {
        return *refus;
    }
    match parts.uri.path() {
        p @ ("/api/start" | "/api/stop" | "/api/restart") => {
            let action = &p["/api/".len()..];
            let res = service_action(ctx, ctl, action).await;
            let msg = match &res {
                Ok(()) => "ok".to_string(),
                Err(e) => e.clone(),
            };
            send_json(StatusCode::OK, &json!({ "ok": res.is_ok(), "out": msg }))
        }
        "/api/vram/unload" => {
            if let Some(r) = post_only(&parts.method) {
                return r;
            }
            let raw = Limited::new(body, 1 << 20)
                .collect()
                .await
                .map(|b| b.to_bytes().to_vec())
                .unwrap_or_default();
            let force = gojson::decode_struct(&raw, UNLOAD_REQ_TYPE, &[("force", Kind::Bool)])
                .bool("force");
            if !force && generating(ctx).await {
                return send_json(
                    StatusCode::CONFLICT,
                    &json!({ "ok": false, "generating": true,
                        "error": "une réponse est en cours d'écriture — décharger maintenant l'interromprait" }),
                );
            }
            unload(ctx, ctl).await
        }
        "/api/vram/reload" => {
            if let Some(r) = post_only(&parts.method) {
                return r;
            }
            if let Some(r) = tune_deny(&ctx.store).await {
                return r;
            }
            if service_is_active(ctx) {
                return send_json(StatusCode::OK, &json!({ "ok": true, "already": true }));
            }
            let cfg = ctx.store.config().await;
            let d = crate::models::model_dirs(ctx).await;
            let pre = tokio::task::spawn_blocking(move || {
                preflight_engine(&d.home, &cfg, &d.stored, d.env.as_deref())
            })
            .await
            .unwrap_or_else(|e| Err(e.to_string()));
            if let Err(e) = pre {
                return send_json(
                    StatusCode::OK,
                    &json!({ "ok": false, "error": plain_err(&e) }),
                );
            }
            if let Err(e) = service_action(ctx, ctl, "start").await {
                return send_json(
                    StatusCode::OK,
                    &json!({ "ok": false, "error": plain_err(&e) }),
                );
            }
            send_json(StatusCode::OK, &json!({ "ok": true }))
        }
        _ => send_json(StatusCode::NOT_FOUND, &json!({ "error": "route inconnue" })),
    }
}

/// `handleVramUnload` après les refus : moteur arrêté, dictée coupée, bilan
/// de la VRAM rendue.
async fn unload(ctx: &Ctx, ctl: &EngineCtl) -> Response<Body> {
    let gpus = ctl.gpus().await;
    let before = gpu_used_mb(&gpus);
    // `engineNeedsStop` : en conteneur, actif suffit (les états transitoires
    // n'existent que sous systemd, où ce lot reste au Go).
    let was_active = service_is_active(ctx);
    let mut errs = Vec::new();
    if was_active && let Err(e) = service_action(ctx, ctl, "stop").await {
        errs.push(format!("arrêt du moteur : {}", plain_err(&e)));
    }
    dictate_stop(ctx).await;
    let mut after = before;
    if !gpus.is_empty() {
        after = gpu_settle(
            before,
            || async {
                let g = ctl.gpus().await;
                (gpu_used_mb(&g), !g.is_empty())
            },
            ctl.settle_pause,
        )
        .await;
    }
    // Les jauges ne doivent pas montrer, deux secondes encore, une carte
    // pleine qu'on vient d'annoncer libérée.
    sysinfo::GPU_MONITOR.invalidate().await;
    let freed = (before - after).max(0);
    send_json(
        StatusCode::OK,
        &json!({
            "ok": errs.is_empty(),
            "error": errs.join(" ; "),
            "was_active": was_active,
            "active": service_is_active(ctx),
            "gpu": !gpus.is_empty(),
            "before_mb": before,
            "used_mb": after,
            "freed_mb": freed,
        }),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn base_comme_go() {
        assert_eq!(go_base("/a/b.gguf"), "b.gguf");
        assert_eq!(go_base("/a/b/"), "b");
        assert_eq!(go_base("/"), "/");
        assert_eq!(go_base(""), ".");
    }

    #[test]
    fn porte_de_sortie() {
        assert!(usable(Some("1"), Some(" Go ")).is_err());
        assert!(usable(Some("1"), None).is_ok());
        assert!(usable(Some("1"), Some("rust")).is_ok());
    }
}
