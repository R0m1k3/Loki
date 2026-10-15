//! Panneau « Moteur de l'image » tenu par la façade (lot 24 de la migration,
//! `LOKI_ENGINE_IMAGE=rust` ; expérimental, le Go reste le défaut) — port de
//! `web_engine.go`, `web_engine_reco.go` et `backend_engine_oci.go` :
//!
//! - la version de llama.cpp qui tourne, les versions installées, la variante ;
//! - le dernier build publié sur le registre, l'installation (job partagé de
//!   [`crate::backends`]) avec essai à blanc avant toute bascule ;
//! - la bascule vers une version installée, sa suppression, le retour à la
//!   version précédente ;
//! - la version recommandée (choisie sur clic), le risque de rendu du
//!   raisonnement (défaut de b10763) et la vérification du rendu du gabarit
//!   avant/après la bascule, par la sonde de gabarit ([`crate::chat::tplprobe`]).
//!
//! Tenu seulement avec les backends (le job) — et la sonde de la conversation
//! quand la façade la tient.

pub mod oci;

use std::collections::{HashMap, HashSet};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime};

use http_body_util::{BodyExt, Limited};
use hyper::body::Incoming;
use hyper::{Method, Request, Response, StatusCode};
use serde_json::{Value, json};

use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::backends::{Backends, Log};
use crate::chat::tplprobe::{self, Shape, TplProbe};
use crate::chat::wire::TplTri;
use crate::gojson::{self, Kind};
use crate::serve::run::same_path;
use crate::store::{BK_CONFIG, BK_STATE, Store};

/// `engineUseImage`.
pub const USE_IMAGE: &str = "image";

/// `enginePreserveDefaultBuild` : preserve_reasoning activé par défaut.
pub const PRESERVE_DEFAULT_BUILD: i64 = 10763;
/// `engineQwen4MTPBuild`.
pub const QWEN4_MTP_BUILD: i64 = 11331;
/// `engineBuildFloor` : en dessous, un build n'est pas cru.
pub const BUILD_FLOOR: i64 = 5000;

const UPDATE_REQ: &str =
    r#"struct { Tag string "json:\"tag\""; PreserveOff bool "json:\"preserve_off\"" }"#;
const TAG_REQ: &str = r#"struct { Tag string "json:\"tag\"" }"#;
const PLAN_REQ: &str = r#"struct { Recommended bool "json:\"recommended\"" }"#;

/// `engineUnsupportedWhy`.
pub const UNSUPPORTED_WHY: &str = "les images officielles llama.cpp n'existent que pour Linux x86-64 et arm64 ; sur cette machine, utilise « llama.cpp précompilé » ou « llama.cpp compilé ».";

/// `engineRenderCheck`.
#[derive(Clone, Debug, PartialEq)]
pub struct RenderCheck {
    pub status: String,
    pub tag: String,
    pub detail: String,
    pub at: SystemTime,
}

impl RenderCheck {
    fn to_json(&self) -> Value {
        let mut v = json!({ "status": self.status, "tag": self.tag });
        if !self.detail.is_empty() {
            v["detail"] = json!(self.detail);
        }
        v["at"] = json!(tplprobe::rfc3339(self.at));
        v
    }
}

#[derive(Default)]
struct RenderState {
    generation: u64,
    last: Option<RenderCheck>,
}

/// Le relevé du rendu sur le moteur qui tourne encore.
#[derive(Clone, Debug, Default)]
pub struct RenderRef {
    pub text: String,
    pub shape: Shape,
    pub build: String,
}

/// Le panneau.
pub struct EngineImg {
    pub home: PathBuf,
    pub reg: oci::Registry,
    /// `engineOCISupported`.
    pub supported: bool,
    /// La sonde de gabarit (celle de la conversation quand la façade la tient).
    pub probe: Arc<TplProbe>,
    /// Délai laissé au nouveau moteur pour charger le modèle, et cadence.
    pub render_wait: Duration,
    pub render_poll: Duration,
    /// Variables d'environnement lues (`LLAMA_ARG_*`) ; `None` : le processus.
    pub env: Option<HashMap<String, String>>,
    render: Mutex<RenderState>,
    builds: Mutex<HashMap<String, i64>>,
}

impl EngineImg {
    pub fn new(home: PathBuf, reg: oci::Registry, supported: bool, probe: Arc<TplProbe>) -> Self {
        EngineImg {
            home,
            reg,
            supported,
            probe,
            render_wait: Duration::from_secs(20 * 60),
            render_poll: Duration::from_secs(5),
            env: None,
            render: Mutex::new(RenderState::default()),
            builds: Mutex::new(HashMap::new()),
        }
    }

    fn getenv(&self, k: &str) -> String {
        match &self.env {
            Some(m) => m.get(k).cloned().unwrap_or_default(),
            None => std::env::var(k).unwrap_or_default(),
        }
    }

    // --- état du rendu ----------------------------------------------------

    fn rs(&self) -> std::sync::MutexGuard<'_, RenderState> {
        self.render.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// `engineRenderBegin`.
    fn render_begin(&self) -> u64 {
        let mut s = self.rs();
        s.generation += 1;
        s.generation
    }

    /// `engineRenderCancel` : la vérification en cours périmée, son relevé
    /// effacé.
    pub fn render_cancel(&self) {
        let mut s = self.rs();
        s.generation += 1;
        s.last = None;
    }

    fn render_current(&self, generation: u64) -> bool {
        self.rs().generation == generation
    }

    /// `engineRenderSet`.
    fn render_set(&self, generation: u64, status: &str, tag: &str, detail: &str) {
        let c = RenderCheck {
            status: status.into(),
            tag: tag.into(),
            detail: detail.into(),
            at: SystemTime::now(),
        };
        {
            let mut s = self.rs();
            if generation != s.generation {
                return;
            }
            s.last = Some(c);
        }
        if status != "pending" {
            let mut line = format!("[moteur] rendu du gabarit après {tag} : {status}");
            if !detail.is_empty() {
                line.push_str(&format!(" — {detail}"));
            }
            eprintln!("{line}");
        }
    }

    /// `engineRenderSnapshot`.
    pub fn render_snapshot(&self) -> Option<RenderCheck> {
        self.rs().last.clone()
    }

    // --- moteur courant ---------------------------------------------------

    /// `currentEngineBin` : BIN (suivi de version du précompilé), sinon le
    /// moteur de l'image.
    pub fn current_bin(&self, b: &Backends, cfg: &HashMap<String, String>) -> String {
        let ep = crate::serve::run::EnginePaths {
            home: self.home.clone(),
        };
        let bin = ep.prebuilt_resolve_bin(cfg.get("BIN").map_or("", String::as_str));
        if !bin.is_empty() && crate::backends::probe::is_file(Path::new(&bin)) {
            return bin;
        }
        b.provided_engine_bin()
    }

    /// `engineSource`.
    pub fn source(&self, b: &Backends, bin: &str) -> &'static str {
        let ep = crate::serve::run::EnginePaths {
            home: self.home.clone(),
        };
        let img = b.provided_engine_bin();
        if !img.is_empty() && same_path(bin, &img) {
            "image"
        } else if ep.engine_owns(bin) {
            "downloaded"
        } else if ep.prebuilt_owns(bin) {
            "prebuilt"
        } else {
            "custom"
        }
    }

    /// `engineVariant` : d'après les backends ggml posés à côté du moteur.
    pub fn variant(&self, bin: &str) -> &'static str {
        if bin.is_empty() {
            return "server";
        }
        let dir = crate::paths::go_dir(bin);
        let names: Vec<String> = std::fs::read_dir(&dir)
            .map(|rd| {
                rd.flatten()
                    .map(|e| e.file_name().to_string_lossy().into_owned())
                    .collect()
            })
            .unwrap_or_default();
        let has = |p: &str| names.iter().any(|n| n.starts_with(p));
        if has("libggml-cuda.so") {
            "server-cuda"
        } else if has("libggml-musa.so") {
            "server-musa"
        } else if has("libggml-sycl.so") {
            "server-intel"
        } else if has("libggml-vulkan.so") {
            "server-vulkan"
        } else {
            "server"
        }
    }

    /// `engineBuildCached` : le build d'un binaire, gardé tant que sa taille
    /// et sa date ne changent pas.
    pub async fn build_cached(&self, bin: &str) -> i64 {
        let Ok(md) = std::fs::metadata(bin) else {
            return 0;
        };
        let mtime = md
            .modified()
            .ok()
            .and_then(|t| t.duration_since(SystemTime::UNIX_EPOCH).ok())
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
        let (b, _) = oci::build_of(bin).await;
        self.builds
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .insert(key, b);
        b
    }

    /// `engineBuildTrusted`.
    pub async fn build_trusted(&self, b: &Backends, bin: &str) -> i64 {
        let src = self.source(b, bin);
        if src == "custom" {
            return 0;
        }
        build_trust(src, self.build_cached(bin).await)
    }

    // --- version précédente -----------------------------------------------

    fn prev_file(&self) -> PathBuf {
        oci::engine_dir(&self.home).join("previous-bin")
    }

    /// `engineRememberPrevious`.
    pub fn remember_previous(&self, left: &str, next: &str) {
        if left.is_empty() || same_path(left, next) {
            return;
        }
        let _ = std::fs::create_dir_all(oci::engine_dir(&self.home));
        let _ = std::fs::write(self.prev_file(), format!("{left}\n"));
    }

    /// `enginePrevious`.
    pub async fn previous(&self, b: &Backends, cur: &str) -> Option<Value> {
        let s = std::fs::read_to_string(self.prev_file()).ok()?;
        let bin = s.trim().to_string();
        if bin.is_empty()
            || !crate::backends::probe::is_file(Path::new(&bin))
            || same_path(&bin, cur)
        {
            return None;
        }
        let mut out = json!({ "bin": bin, "source": self.source(b, &bin) });
        let tag = tag_of(&self.home, &bin);
        if !tag.is_empty() {
            out["build"] = json!(oci::tag_build(&tag));
            out["tag"] = json!(tag);
        } else {
            out["build"] = json!(self.build_cached(&bin).await);
        }
        Some(out)
    }

    // --- rendu du raisonnement --------------------------------------------

    /// La sonde de gabarit vers le moteur local.
    async fn probe_env(&self, store: &Store, cfg: &HashMap<String, String>) -> tplprobe::Env {
        tplprobe::Env {
            external: crate::engstatus::is_external(cfg),
            port: crate::engstatus::llm_port(cfg).into(),
            key: store.get_str(BK_STATE, "api_key").await,
        }
    }

    /// `enginePreserveInputs`.
    pub async fn preserve_inputs(&self, b: &Backends, store: &Store, target: i64) -> PreserveIn {
        let cfg = store.config().await;
        let cur = self.current_bin(b, &cfg);
        let mut inp = PreserveIn {
            external: crate::engstatus::is_external(&cfg),
            current: self.build_trusted(b, &cur).await,
            target,
            key: cfg
                .get("REASONING_PRESERVE")
                .map_or("", |s| s.trim())
                .into(),
            overridden: preserve_overridden(&cfg, |k| self.getenv(k)),
            ..PreserveIn::default()
        };
        if inp.external
            || !inp.key.is_empty()
            || !inp.overridden.is_empty()
            || (target > 0 && target < PRESERVE_DEFAULT_BUILD)
        {
            return inp;
        }
        let env = self.probe_env(store, &cfg).await;
        let p = self.probe.prober(&env);
        if !p.healthy().await {
            return inp;
        }
        let Ok(props) = p.props().await else {
            return inp;
        };
        inp.tpl_known = !props.chat_template.is_empty();
        inp.template = props.chat_template;
        inp.preserves = self
            .probe
            .ensure(&env, &self.probe.last_shape())
            .await
            .preserves_history;
        inp
    }

    /// `engineRenderBefore` : le rendu sur le moteur qui tourne encore.
    pub async fn render_before(&self, store: &Store) -> Option<RenderRef> {
        let cfg = store.config().await;
        let mut r = RenderRef {
            shape: self.probe.last_shape(),
            ..RenderRef::default()
        };
        if crate::engstatus::is_external(&cfg) {
            return None;
        }
        let env = self.probe_env(store, &cfg).await;
        let p = self.probe.prober(&env);
        if !p.healthy().await {
            return None;
        }
        if let Ok(props) = p.props().await {
            r.build = props.build_info;
        }
        r.text = p.render_print(&r.shape).await.ok()?;
        Some(r)
    }

    /// `engineRenderAfter` : attend le nouveau moteur, compare, range le
    /// relevé (en tâche de fond).
    pub async fn render_after(&self, store: &Store, r: RenderRef, tag: &str) {
        let generation = self.render_begin();
        self.render_set(generation, "pending", tag, "");
        let deadline = Instant::now() + self.render_wait;
        let cfg = store.config().await;
        let env = self.probe_env(store, &cfg).await;
        let p = self.probe.prober(&env);
        loop {
            let ready = p.healthy().await
                && (r.build.is_empty() || p.props().await.is_ok_and(|pr| pr.build_info != r.build));
            if ready {
                break;
            }
            if !self.render_current(generation) {
                return;
            }
            if Instant::now() + self.render_poll > deadline {
                self.render_set(
                    generation,
                    "unknown",
                    tag,
                    "le nouveau moteur n'a pas répondu à temps : rendu non vérifié",
                );
                return;
            }
            tokio::time::sleep(self.render_poll).await;
        }
        match p.render_print(&r.shape).await {
            Err(e) => self.render_set(
                generation,
                "unknown",
                tag,
                &format!("rendu impossible sur le nouveau moteur : {e}"),
            ),
            Ok(after) => match tplprobe::render_diff(&r.text, &after) {
                (true, _) => self.render_set(generation, "same", tag, ""),
                (false, d) => self.render_set(generation, "changed", tag, &d),
            },
        }
    }
}

/// `engineBuildTrust`.
pub fn build_trust(source: &str, build: i64) -> i64 {
    if source == "custom" || build < BUILD_FLOOR {
        0
    } else {
        build
    }
}

/// `engineTagOf`.
pub fn tag_of(home: &Path, bin: &str) -> String {
    let ep = crate::serve::run::EnginePaths {
        home: home.to_path_buf(),
    };
    if !ep.engine_owns(bin) {
        return String::new();
    }
    crate::paths::go_base(&crate::paths::go_dir(bin))
}

/// `engineRecommendation` : l'encart, ou rien.
pub fn recommendation(trusted: i64, supported: bool) -> Value {
    let min = crate::serve::ckpt::ENGINE_MIN_RECOMMENDED;
    if trusted <= 0 || trusted >= min || !supported {
        return Value::Null;
    }
    json!({
        "current": trusted,
        "min": min,
        "gains": [
            format!("points de reprise des modèles hybrides (Qwen3.5/3.6, Qwen3-Next…) gardés : plus de ~8 k jetons recalculés à chaque reprise (b{min})"),
            format!("MTP rapide (graphes CUDA) à partir de b{}", crate::serve::spec::SPEC_AUTO_MIN_BUILD),
            format!("MTP de qwen4exp à partir de b{QWEN4_MTP_BUILD}"),
        ],
    })
}

/// `preserveIn`.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct PreserveIn {
    pub external: bool,
    pub current: i64,
    pub target: i64,
    pub key: String,
    pub overridden: String,
    pub template: String,
    pub tpl_known: bool,
    pub preserves: TplTri,
}

/// `tplPreserveVars`.
const TPL_PRESERVE_VARS: [&str; 4] = [
    "preserve_thinking",
    "clear_thinking",
    "truncate_history_thinking",
    "preserve_reasoning",
];

/// `reasoningPreserveRisk` : {risk, why, suggest}.
pub fn preserve_risk(i: &PreserveIn) -> Value {
    let no = |why: String| json!({ "risk": "no", "why": why, "suggest": false });
    if i.external {
        return no("preset externe : son gabarit n'est pas rendu par ce moteur".into());
    }
    if i.target > 0 && i.target < PRESERVE_DEFAULT_BUILD {
        return no(format!(
            "b{} n'a pas encore preserve_reasoning par défaut (b{PRESERVE_DEFAULT_BUILD})",
            i.target
        ));
    }
    if i.current >= PRESERVE_DEFAULT_BUILD {
        return no(format!(
            "le moteur courant (b{}) a déjà preserve_reasoning par défaut",
            i.current
        ));
    }
    if !i.key.is_empty() {
        return no(format!(
            "REASONING_PRESERVE={} est posée : le rendu ne dépend pas du défaut du moteur",
            i.key
        ));
    }
    if !i.overridden.is_empty() {
        return no(format!("{} règle déjà preserve_reasoning", i.overridden));
    }
    if !i.tpl_known {
        return json!({
            "risk": "unknown",
            "why": format!("moteur arrêté ou gabarit illisible : impossible de vérifier. À partir de b{PRESERVE_DEFAULT_BUILD}, un gabarit de type Qwen3.6 rend la réflexion des tours passés, vide puisque Loki ne la renvoie pas (sans REASONING_ECHO) ; REASONING_PRESERVE=off garde le rendu actuel. À l'inverse, sur un gabarit qui la garde déjà (Qwen3.8), off changerait le rendu."),
            "suggest": false,
        });
    }
    let tpl = TPL_PRESERVE_VARS.iter().any(|v| i.template.contains(v));
    if !tpl {
        return no(
            "le gabarit chargé ne lit pas preserve_thinking / clear_thinking : rendu inchangé"
                .into(),
        );
    }
    match i.preserves {
        TplTri::Yes => no(
            "le gabarit garde déjà la réflexion des tours passés : rendu inchangé (ne pas poser off)"
                .into(),
        ),
        TplTri::No => json!({
            "risk": "yes",
            "why": format!("le gabarit chargé retire la réflexion des tours passés ; à partir de b{PRESERVE_DEFAULT_BUILD} le moteur la lui fait garder, et sans REASONING_ECHO Loki ne la renvoie pas : chaque tour passé serait rendu avec un bloc de réflexion vide. REASONING_PRESERVE=off garde le rendu actuel."),
            "suggest": true,
        }),
        TplTri::Unknown => json!({
            "risk": "unknown",
            "why": "le gabarit lit preserve_thinking mais la sonde n'a pas conclu : REASONING_PRESERVE=off garde le rendu actuel s'il retire la réflexion des tours passés",
            "suggest": false,
        }),
    }
}

/// `preserveOverridden` : EXTRA_ARGS ou variable qui fixent déjà
/// preserve_reasoning ("" = aucune).
pub fn preserve_overridden(
    cfg: &HashMap<String, String>,
    getenv: impl Fn(&str) -> String,
) -> String {
    let extra = crate::models::split_args(cfg.get("EXTRA_ARGS").map_or("", String::as_str));
    if crate::serve::has_any_flag(&extra, &["--reasoning-preserve", "--no-reasoning-preserve"]) {
        return "EXTRA_ARGS".into();
    }
    if crate::serve::flag_values(&extra, &["--chat-template-kwargs"])
        .join(" ")
        .contains("preserve_reasoning")
    {
        return "--chat-template-kwargs".into();
    }
    if !getenv("LLAMA_ARG_REASONING_PRESERVE").trim().is_empty() {
        return "LLAMA_ARG_REASONING_PRESERVE".into();
    }
    if getenv("LLAMA_ARG_CHAT_TEMPLATE_KWARGS").contains("preserve_reasoning") {
        return "LLAMA_ARG_CHAT_TEMPLATE_KWARGS".into();
    }
    String::new()
}

/// Les routes servies par ce module.
pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/engine"
            | "/api/engine/check"
            | "/api/engine/update"
            | "/api/engine/use"
            | "/api/engine/remove"
            | "/api/engine/plan"
            | "/api/engine/rollback"
    )
}

fn fail(code: StatusCode, e: impl Into<String>) -> Response<Body> {
    send_json(code, &json!({ "ok": false, "error": e.into() }))
}

use crate::proxy::Body;

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(
    ctx: &Ctx,
    e: &Arc<EngineImg>,
    req: Request<Incoming>,
    from_go: bool,
) -> Response<Body> {
    let Some(b) = ctx.backends.clone() else {
        return fail(
            StatusCode::SERVICE_UNAVAILABLE,
            "backends non tenus par la façade",
        );
    };
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
    let post = parts.method == Method::POST;
    match parts.uri.path() {
        "/api/engine" => send_json(StatusCode::OK, &status(e, &b, store).await),
        "/api/engine/check" => {
            if !e.supported {
                return fail(StatusCode::OK, UNSUPPORTED_WHY);
            }
            let cfg = store.config().await;
            let variant = e.variant(&e.current_bin(&b, &cfg));
            match e.reg.latest(variant).await {
                Err(err) => fail(StatusCode::OK, err),
                Ok((build, tag)) => {
                    let (cur, _) = oci::build_of(&e.current_bin(&b, &cfg)).await;
                    send_json(
                        StatusCode::OK,
                        &json!({
                            "ok": true, "variant": variant, "latest": build, "tag": tag,
                            "current": cur, "update": build == 0 || cur == 0 || build > cur,
                        }),
                    )
                }
            }
        }
        "/api/engine/update" => {
            if !e.supported {
                return fail(StatusCode::BAD_REQUEST, UNSUPPORTED_WHY);
            }
            let d = gojson::decode_struct(
                &raw,
                UPDATE_REQ,
                &[("tag", Kind::Str), ("preserve_off", Kind::Bool)],
            );
            let tag = d.str("tag").trim().to_string();
            let preserve_off = d.bool("preserve_off");
            if let Some(err) = crate::backends::tune_busy(store).await {
                return fail(StatusCode::CONFLICT, err);
            }
            if let Err(err) = b.jobs.start(store, "engine").await {
                return fail(StatusCode::CONFLICT, err);
            }
            let (ctx2, e2, b2) = (ctx.clone(), e.clone(), b.clone());
            tokio::spawn(async move {
                run_update(&ctx2, &e2, &b2, tag, preserve_off).await;
                b2.jobs.end(&ctx2.store).await;
            });
            send_json(StatusCode::OK, &json!({ "ok": true }))
        }
        "/api/engine/use" => {
            if let Some(err) = crate::backends::tune_busy(store).await {
                return send_json(
                    StatusCode::CONFLICT,
                    &json!({ "ok": false, "tuning": true, "error": err }),
                );
            }
            let d = gojson::decode_struct(&raw, TAG_REQ, &[("tag", Kind::Str)]);
            let tag = d.str("tag").trim().to_string();
            let bin = if tag == USE_IMAGE {
                let img = b.provided_engine_bin();
                if img.is_empty() {
                    return fail(
                        StatusCode::BAD_REQUEST,
                        "cette installation n'a pas de moteur d'image (LOKI_ENGINE_BIN)",
                    );
                }
                img
            } else {
                let p = oci::bin_for(&e.home, &tag);
                if p.is_empty() {
                    return fail(
                        StatusCode::BAD_REQUEST,
                        format!("version non installée : {tag}"),
                    );
                }
                p
            };
            switch_simple(ctx, e, &b, &bin).await
        }
        "/api/engine/remove" => {
            let d = gojson::decode_struct(&raw, TAG_REQ, &[("tag", Kind::Str)]);
            let tag = crate::paths::go_base(d.str("tag").trim());
            let bin = oci::bin_for(&e.home, &tag);
            if bin.is_empty() {
                return fail(
                    StatusCode::BAD_REQUEST,
                    format!("version non installée : {tag}"),
                );
            }
            let cfg = store.config().await;
            if same_path(&bin, &e.current_bin(&b, &cfg)) {
                return fail(
                    StatusCode::CONFLICT,
                    "c'est le moteur utilisé — bascule d'abord sur une autre version",
                );
            }
            let dir = oci::engine_dir(&e.home).join(&tag);
            if let Err(err) = std::fs::remove_dir_all(&dir) {
                return fail(
                    StatusCode::INTERNAL_SERVER_ERROR,
                    crate::paths::os_err("unlinkat", &dir.to_string_lossy(), &err),
                );
            }
            send_json(StatusCode::OK, &json!({ "ok": true }))
        }
        "/api/engine/plan" => {
            if !post {
                return fail(StatusCode::METHOD_NOT_ALLOWED, "POST requis");
            }
            if !e.supported {
                return fail(StatusCode::OK, UNSUPPORTED_WHY);
            }
            let d = gojson::decode_struct(&raw, PLAN_REQ, &[("recommended", Kind::Bool)]);
            let cfg = store.config().await;
            let cur = e.current_bin(&b, &cfg);
            let variant = e.variant(&cur);
            let res = if d.bool("recommended") {
                recommended_tag(e, variant).await
            } else {
                e.reg.latest(variant).await
            };
            let (build, tag) = match res {
                Ok(r) => r,
                Err(err) => return fail(StatusCode::OK, err),
            };
            let (cur_build, _) = oci::build_of(&cur).await;
            let inp =
                tokio::time::timeout(Duration::from_secs(20), e.preserve_inputs(&b, store, build))
                    .await
                    .unwrap_or_else(|_| PreserveIn {
                        target: build,
                        ..PreserveIn::default()
                    });
            send_json(
                StatusCode::OK,
                &json!({
                    "ok": true, "tag": tag, "build": build, "current": cur_build,
                    "uptodate": build > 0 && cur_build == build,
                    "preserve": preserve_risk(&inp),
                    "keeps": tag_of(&e.home, &cur),
                }),
            )
        }
        "/api/engine/rollback" => {
            if !post {
                return fail(StatusCode::METHOD_NOT_ALLOWED, "POST requis");
            }
            if let Some(err) = crate::backends::tune_busy(store).await {
                return send_json(
                    StatusCode::CONFLICT,
                    &json!({ "ok": false, "tuning": true, "error": err }),
                );
            }
            let cfg = store.config().await;
            let cur = e.current_bin(&b, &cfg);
            let Some(prev) = e.previous(&b, &cur).await else {
                return fail(
                    StatusCode::BAD_REQUEST,
                    "aucune version précédente conservée",
                );
            };
            let bin = prev["bin"].as_str().unwrap_or("").to_string();
            switch_simple(ctx, e, &b, &bin).await
        }
        _ => fail(StatusCode::NOT_FOUND, "route inconnue"),
    }
}

/// Bascule sans réseau (version installée, image, retour) : BIN, version
/// précédente notée, vérification du rendu périmée, moteur relancé s'il
/// tournait.
async fn switch_simple(ctx: &Ctx, e: &EngineImg, b: &Backends, bin: &str) -> Response<Body> {
    let store = &ctx.store;
    let left = e.current_bin(b, &store.config().await);
    if let Err(err) = store.put(BK_CONFIG, "BIN", bin.as_bytes().to_vec()).await {
        return fail(StatusCode::INTERNAL_SERVER_ERROR, err);
    }
    e.remember_previous(&left, bin);
    e.render_cancel();
    let _ = crate::backends::service(ctx, "restart", true).await;
    send_json(StatusCode::OK, &json!({ "ok": true, "bin": bin }))
}

/// `handleEngineStatus`.
pub async fn status(e: &EngineImg, b: &Backends, store: &Store) -> Value {
    let cfg = store.config().await;
    let bin = e.current_bin(b, &cfg);
    let (build, commit) = oci::build_of(&bin).await;
    let source = e.source(b, &bin);
    let installed: Vec<Value> = oci::installed(&e.home)
        .into_iter()
        .map(|tag| {
            let p = oci::bin_for(&e.home, &tag);
            json!({
                "tag": tag, "bin": p, "build": oci::tag_build(&tag),
                "in_use": same_path(&p, &bin),
            })
        })
        .collect();
    let trust = build_trust(source, build);
    json!({
        "supported": e.supported,
        "variant": e.variant(&bin),
        "repo": format!("{}/{}", e.reg.host(), oci::ENGINE_REPO),
        "bin": bin,
        "build": build,
        "commit": commit,
        "source": source,
        "image_bin": b.provided_engine_bin(),
        "dir": oci::engine_dir(&e.home).to_string_lossy(),
        "installed": if installed.is_empty() { Value::Null } else { Value::Array(installed) },
        "job": b.jobs.snapshot(0, false).await,
        "notice": crate::serve::ckpt::engine_build_notice(trust),
        "recommend": recommendation(trust, e.supported),
        "previous": e.previous(b, &bin).await.unwrap_or(Value::Null),
        "render_check": e.render_snapshot().map_or(Value::Null, |c| c.to_json()),
    })
}

/// `engineRecommendedTag`.
async fn recommended_tag(e: &EngineImg, variant: &str) -> Result<(i64, String), String> {
    let min = crate::serve::ckpt::ENGINE_MIN_RECOMMENDED;
    let (build, tag) = e
        .reg
        .latest(variant)
        .await
        .map_err(|err| format!("registre injoignable, aucune version choisie : {err}"))?;
    if build == 0 {
        return Err(format!(
            "le registre n'annonce pas le build du tag « {variant} » : impossible de vérifier qu'il atteint b{min}"
        ));
    }
    if build < min {
        return Err(format!(
            "dernier build publié pour {variant} : b{build}, antérieur à la version recommandée b{min}"
        ));
    }
    if let Err(err) = e.reg.resolve(oci::ENGINE_REPO, &tag).await {
        return Err(format!(
            "{tag} annoncé mais introuvable sur le registre : {err}"
        ));
    }
    Ok((build, tag))
}

/// `engineHelpHas` : l'aide du binaire mentionne-t-elle ce drapeau ?
async fn help_has(bin: &str, flag: &str) -> bool {
    let (out, _) = oci::run_bin(bin, "--help", Some(Duration::from_secs(15))).await;
    out.contains(flag)
}

/// `enginePreserveOffIf` : REASONING_PRESERVE=off, dans le preset actif
/// s'il y en a un, dans la configuration sinon.
async fn preserve_off(ctx: &Ctx, e: &EngineImg, new_bin: &str) -> Result<String, String> {
    let store = &ctx.store;
    let cfg = store.config().await;
    let v = cfg.get("REASONING_PRESERVE").map_or("", |s| s.trim());
    if !v.is_empty() {
        return Ok(format!("REASONING_PRESERVE={v} déjà posée : inchangée"));
    }
    if !help_has(new_bin, "--no-reasoning-preserve").await {
        return Err("le nouveau moteur ne connaît pas --no-reasoning-preserve".into());
    }
    if let Some(err) = crate::backends::tune_busy(store).await {
        return Err(err);
    }
    let order = crate::presets::parse_preset_order(
        store
            .get(BK_STATE, crate::presets::PRESET_ORDER_KEY)
            .await
            .ok()
            .flatten()
            .as_deref(),
    );
    if let Ok(list) = crate::presets::list_presets(&e.home, &cfg, &order)
        && let Some(p) = list.into_iter().find(|p| p.active)
    {
        let content = crate::presets_api::read_preset(&e.home, &p.id)?;
        crate::presets_api::save_applying(
            ctx,
            &p.id,
            &p.name,
            &crate::presets_api::preset_set_key(&content, "REASONING_PRESERVE", "off"),
            "save",
        )
        .await?;
        return Ok(format!(
            "REASONING_PRESERVE=off posée dans le preset « {} »",
            p.name
        ));
    }
    store
        .put(BK_CONFIG, "REASONING_PRESERVE", b"off".to_vec())
        .await?;
    Ok("REASONING_PRESERVE=off posée dans la configuration active (aucun preset ne lui correspond)".into())
}

/// `engineSwitchTo` : BIN posé, moteur arrêté puis relancé s'il tournait.
async fn switch_to(ctx: &Ctx, log: &Log<'_>, bin: &str) -> Result<(), String> {
    if let Some(err) = crate::backends::tune_busy(&ctx.store).await {
        return Err(err);
    }
    let (was_up, err) = crate::backends::service_stop_if_up(ctx).await;
    if was_up {
        log.phase("arrêt du moteur…").await;
        if !err.is_empty() {
            log.line(&format!("[warn] arrêt du moteur : {err}")).await;
        }
    }
    if let Err(e) = ctx
        .store
        .put(BK_CONFIG, "BIN", bin.as_bytes().to_vec())
        .await
    {
        if was_up {
            let _ = crate::backends::service(ctx, "start", false).await;
        }
        return Err(format!(
            "moteur installé mais échec d'écriture de BIN : {e}"
        ));
    }
    log.line(&format!("BIN → {bin}")).await;
    if was_up {
        log.phase("redémarrage du moteur…").await;
        let (_, e) = crate::backends::service(ctx, "start", false).await;
        if !e.is_empty() {
            log.line(&format!("[warn] redémarrage du moteur : {e}"))
                .await;
        }
    }
    Ok(())
}

/// `engineRunUpdate` : télécharger, ESSAYER, puis seulement basculer.
async fn run_update(
    ctx: &Ctx,
    e: &Arc<EngineImg>,
    b: &Backends,
    mut tag: String,
    preserve_off_asked: bool,
) {
    let store = &ctx.store;
    let log = Log {
        jobs: &b.jobs,
        store,
    };
    let cfg = store.config().await;
    let ancien = e.current_bin(b, &cfg);
    let variant = e.variant(&ancien);
    if tag.is_empty() {
        log.phase("recherche de la dernière version publiée…").await;
        match e.reg.latest(variant).await {
            Err(err) => return b.jobs.fail(store, &err).await,
            Ok((build, latest)) => {
                tag = latest;
                if build > 0 {
                    let (cur, _) = oci::build_of(&e.current_bin(b, &store.config().await)).await;
                    if cur == build {
                        return b
                            .jobs
                            .done(store, &format!("déjà à jour (build {build})"))
                            .await;
                    }
                }
            }
        }
    }
    log.line(&format!("variante : {variant} — version visée : {tag}"))
        .await;
    let bin = match oci::install(&e.home, &e.reg, &tag, &log).await {
        Ok(bin) => bin,
        Err(err) => return b.jobs.fail(store, &err).await,
    };
    log.phase("essai du nouveau moteur…").await;
    if let Err(err) = oci::smoke_test(&bin, &ancien).await {
        let _ = std::fs::remove_dir_all(crate::paths::go_dir(&bin));
        return b
            .jobs
            .fail(
                store,
                &format!("{err}\n\nLe moteur courant n'a PAS été touché. Cette version demande sans doute un runtime CUDA plus récent que celui de l'image : reconstruis l'image avec un LLAMACPP_IMAGE récent, ou épingle un build antérieur"),
            )
            .await;
    }
    let (new_build, commit) = oci::build_of(&bin).await;
    if new_build > 0 {
        log.line(&format!("moteur testé : build {new_build} ({commit})"))
            .await;
    }
    if preserve_off_asked {
        match preserve_off(ctx, e, &bin).await {
            Err(err) => {
                return b
                    .jobs
                    .fail(
                        store,
                        &format!("REASONING_PRESERVE=off demandée mais pas posée : {err} — moteur courant inchangé ; le nouveau reste installé"),
                    )
                    .await;
            }
            Ok(msg) => log.line(&msg).await,
        }
    } else {
        let inp = tokio::time::timeout(
            Duration::from_secs(20),
            e.preserve_inputs(b, store, new_build),
        )
        .await
        .unwrap_or_else(|_| PreserveIn {
            target: new_build,
            ..PreserveIn::default()
        });
        let risk = preserve_risk(&inp);
        if risk["risk"] != "no" {
            log.line(&format!(
                "[warn] rendu du raisonnement : {}",
                risk["why"].as_str().unwrap_or("")
            ))
            .await;
        }
    }
    let reference = tokio::time::timeout(Duration::from_secs(30), e.render_before(store))
        .await
        .ok()
        .flatten();
    e.render_cancel();
    if let Err(err) = switch_to(ctx, &log, &bin).await {
        return b.jobs.fail(store, &err).await;
    }
    e.remember_previous(&ancien, &bin);
    let mut keep = HashSet::new();
    keep.insert(crate::paths::go_base(&crate::paths::go_dir(&bin)));
    let prev = tag_of(&e.home, &ancien);
    if !prev.is_empty() && !keep.contains(&prev) {
        keep.insert(prev.clone());
        log.line(&format!(
            "version précédente gardée pour revenir en arrière : {prev}"
        ))
        .await;
    }
    for l in oci::prune(&e.home, &keep) {
        log.line(&l).await;
    }
    if let Some(r) = reference {
        log.line("vérification du rendu du gabarit une fois le modèle chargé (panneau Moteur, journal [moteur])")
            .await;
        let (e2, store2, tag2) = (e.clone(), store.clone(), tag.clone());
        tokio::spawn(async move { e2.render_after(&store2, r, &tag2).await });
    }
    b.jobs
        .done(store, &format!("moteur mis à jour ({tag})"))
        .await;
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn confiance() {
        assert_eq!(build_trust("custom", 10000), 0);
        assert_eq!(build_trust("image", 4999), 0);
        assert_eq!(build_trust("image", 5000), 5000);
        assert!(recommendation(10000, true).is_object());
        assert!(recommendation(10864, true).is_null());
        assert!(recommendation(0, true).is_null());
        assert!(recommendation(10000, false).is_null());
    }

    #[test]
    fn variantes() {
        let d = tempfile::tempdir().unwrap();
        let e = EngineImg::new(
            d.path().to_path_buf(),
            oci::Registry::new("http://127.0.0.1:9".into(), false),
            true,
            Arc::new(TplProbe::new()),
        );
        let bin = d.path().join("llama-server");
        let bin = bin.to_string_lossy();
        assert_eq!(e.variant(""), "server");
        assert_eq!(e.variant(&bin), "server");
        for (lib, want) in [
            ("libggml-vulkan.so", "server-vulkan"),
            ("libggml-sycl.so", "server-intel"),
            ("libggml-musa.so.1", "server-musa"),
            ("libggml-cuda.so", "server-cuda"),
        ] {
            std::fs::write(d.path().join(lib), "").unwrap();
            assert_eq!(e.variant(&bin), want, "{lib}");
        }
    }

    /// Une vérification périmée par une bascule plus récente ne range rien.
    #[test]
    fn verification_perimee() {
        let d = tempfile::tempdir().unwrap();
        let e = EngineImg::new(
            d.path().to_path_buf(),
            oci::Registry::new("http://127.0.0.1:9".into(), false),
            true,
            Arc::new(TplProbe::new()),
        );
        let old = e.render_begin();
        e.render_set(old, "pending", "a", "");
        let new = e.render_begin();
        assert!(!e.render_current(old));
        e.render_set(old, "same", "a", "");
        assert_eq!(e.render_snapshot().unwrap().status, "pending");
        e.render_set(new, "changed", "b", "d");
        let c = e.render_snapshot().unwrap();
        assert_eq!((c.status.as_str(), c.tag.as_str()), ("changed", "b"));
        assert_eq!(c.to_json()["detail"], "d");
        e.render_cancel();
        assert!(e.render_snapshot().is_none());
        assert!(!e.render_current(new));
    }

    /// Les bornes du défaut de b10763.
    #[test]
    fn bornes_du_rendu() {
        let base = PreserveIn {
            tpl_known: true,
            template: "preserve_thinking".into(),
            preserves: TplTri::No,
            ..PreserveIn::default()
        };
        let risk = |i: PreserveIn| preserve_risk(&i)["why"].as_str().unwrap().to_string();
        assert!(
            risk(PreserveIn {
                target: 10762,
                ..base.clone()
            })
            .starts_with("b10762 n'a pas")
        );
        assert!(
            risk(PreserveIn {
                target: 10763,
                ..base.clone()
            })
            .starts_with("le gabarit chargé retire")
        );
        assert!(risk(base.clone()).starts_with("le gabarit chargé retire"));
        assert!(
            risk(PreserveIn {
                current: 10763,
                ..base.clone()
            })
            .starts_with("le moteur courant (b10763)")
        );
        assert!(
            risk(PreserveIn {
                current: 10762,
                ..base.clone()
            })
            .starts_with("le gabarit chargé retire")
        );
        assert_eq!(preserve_risk(&base)["suggest"], true);
    }

    #[test]
    fn surcharges() {
        let cfg = HashMap::from([(
            "EXTRA_ARGS".to_string(),
            "--no-reasoning-preserve".to_string(),
        )]);
        assert_eq!(preserve_overridden(&cfg, |_| String::new()), "EXTRA_ARGS");
        let cfg = HashMap::from([(
            "EXTRA_ARGS".to_string(),
            r#"--chat-template-kwargs '{"preserve_reasoning":false}'"#.to_string(),
        )]);
        assert_eq!(
            preserve_overridden(&cfg, |_| String::new()),
            "--chat-template-kwargs"
        );
        let none = HashMap::new();
        assert_eq!(
            preserve_overridden(&none, |k| if k == "LLAMA_ARG_REASONING_PRESERVE" {
                " 0 ".into()
            } else {
                String::new()
            }),
            "LLAMA_ARG_REASONING_PRESERVE"
        );
        assert_eq!(preserve_overridden(&none, |_| String::new()), "");
    }
}
