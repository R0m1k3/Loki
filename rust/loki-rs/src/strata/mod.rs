//! Strata, deuxième moteur de Loki (lot 25 de la migration,
//! `LOKI_STRATA=rust` ; expérimental, le Go reste le défaut) — port de
//! `backend_strata.go`.
//!
//! Strata répartit les experts d'un MoE de 125 B (famille Qwen3.8-Flash-Next)
//! entre VRAM, RAM et disque. Le moteur est le paquet FIGÉ publié par AJEAN
//! (sources + binaire Linux CUDA 12, vérifiés par SHA-256) ; le choix se fait
//! par preset (`ENGINE=strata`).
//!
//! Ici : la décision pure (placement des experts, configuration de lancement,
//! ce qui tient sur la machine, recommandation, réglages), la machine (cartes,
//! RAM, disque, Python) et les chemins. Les routes et l'installation sont dans
//! [`api`].

pub mod api;

use std::collections::HashMap;
use std::path::{Path, PathBuf};

use serde_json::{Map, Value, json};

/// Paquet figé (release moe-v1.1 d'AJEAN : experts de la carte d'aide
/// calculés par elle à la lecture des prompts). Un modèle installé avec la
/// 1.0 passe à la 1.1 à son premier lancement (`strataMigrate`, côté Go :
/// `loki serve` d'un preset Strata reste au Go).
pub const VERSION: &str = "1.1";
pub const RELEASE_BASE: &str = "https://github.com/nathaninline/ajean/releases/download/moe-v1.1/";
pub const SRC_ASSET: &str = "ajean-moe-src-1.1.tar.gz";
pub const ENGINE_ASSET: &str = "ajean-moe-engine-1.1-linux-x64-cuda12.zip";
/// Le nom sous lequel l'installeur attend le moteur (CUDA12_ASSET).
pub const ENGINE_LOCAL: &str = "strata-linux-x64-cuda12.zip";

/// `strataAssetSHA` (celles qu'AJEAN publie pour moe-v1.1).
pub fn default_asset_sha() -> HashMap<String, String> {
    HashMap::from([
        (
            SRC_ASSET.to_string(),
            "5a72a90f2182ff0ccab2bdbd7cff15598a21ad94501aa19de2b1b18e2dbc8115".to_string(),
        ),
        (
            ENGINE_ASSET.to_string(),
            "8c717206ee90120f967445169dc39bb11beffdad3d592c75ae08f75bec72f0d3".to_string(),
        ),
    ])
}

pub const ENGINE_LLAMACPP: &str = "llamacpp";
pub const ENGINE_STRATA: &str = "strata";

pub type Cfg = HashMap<String, String>;

fn cfg_get<'a>(cfg: &'a Cfg, k: &str) -> &'a str {
    cfg.get(k).map_or("", String::as_str)
}

/// `engineOf` : « moe » est accepté pour un preset recopié d'AJEAN.
pub fn engine_of(cfg: &Cfg) -> &'static str {
    match cfg_get(cfg, "ENGINE").trim().to_lowercase().as_str() {
        ENGINE_STRATA | "moe" => ENGINE_STRATA,
        _ => ENGINE_LLAMACPP,
    }
}

/// `isStrataConfig`.
pub fn is_strata(cfg: &Cfg) -> bool {
    engine_of(cfg) == ENGINE_STRATA
}

/// `strataFamily`.
pub struct Family {
    pub id: &'static str,
    pub label: &'static str,
    pub about: &'static str,
    pub quants: &'static [&'static str],
}

/// `strataQuant`.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Quant {
    pub id: &'static str,
    pub about: &'static str,
    pub download_gb: f64,
    pub arena_gb: f64,
}

impl Quant {
    fn to_json(self) -> Value {
        json!({
            "id": self.id, "about": self.about,
            "download_gb": go_num(self.download_gb), "arena_gb": go_num(self.arena_gb),
        })
    }
}

pub const FAMILIES: &[Family] = &[
    Family {
        id: "swift",
        label: "Swift 1.5",
        about: "réfléchit moins, répond plus vite",
        quants: &["IQ2_XS", "IQ3_XXS"],
    },
    Family {
        id: "qwen",
        label: "Classique",
        about: "le Qwen3.8-Flash-Next original",
        quants: &["Q2_0", "IQ2_XS", "IQ3_XXS", "IQ3_S"],
    },
];

const QUANTS: &[Quant] = &[
    Quant {
        id: "Q2_0",
        about: "le plus rapide",
        download_gb: 66.4,
        arena_gb: 34.0,
    },
    Quant {
        id: "IQ2_XS",
        about: "un peu plus fidèle, presque aussi rapide",
        download_gb: 68.0,
        arena_gb: 35.5,
    },
    Quant {
        id: "IQ3_XXS",
        about: "99 % de la qualité du modèle complet",
        download_gb: 75.8,
        arena_gb: 42.9,
    },
    Quant {
        id: "IQ3_S",
        about: "la meilleure qualité, le plus lent",
        download_gb: 83.6,
        arena_gb: 50.3,
    },
];

/// `strataQuants[id]`.
pub fn quant(id: &str) -> Option<Quant> {
    QUANTS.iter().find(|q| q.id == id).copied()
}

pub const LOW_RAM_HEADROOM_GB: f64 = 10.0;
/// `strataDropHeadroomGB` (le moteur 1.1 prend ~1 Go de plus que la 1.0).
pub const DROP_HEADROOM_GB: f64 = 8.0;
/// `strataDropMaxCtx` : contexte maximal en mode hors RAM (le cache KV
/// au-delà de la part en VRAM est verrouillé en RAM).
pub const DROP_MAX_CTX: i64 = 131072;

/// `strataHelperExpertsGB`.
pub fn helper_experts_gb(vram_gb: f64) -> f64 {
    (vram_gb - 2.5).max(0.0)
}

pub const EXPERTS_AUTO: &str = "auto";
pub const EXPERTS_RAM: &str = "ram";
pub const EXPERTS_DISK: &str = "disk";

/// `strataMaxCtx`.
pub const MAX_CTX: i64 = 262144;
/// `strataMinArch`.
pub const MIN_ARCH: i64 = 75;
/// `strataLaunchModeKey` (bkState).
pub const LAUNCH_MODE_KEY: &str = "strata_experts_mode";
/// `strataServeMarker`.
pub const SERVE_MARKER: &str = "[loki serve] Strata";

/// Un nombre comme le Go l'écrit en JSON (entier sans « .0 »).
pub fn go_num(f: f64) -> Value {
    if f.fract() == 0.0 && f.abs() < 1e15 {
        json!(f as i64)
    } else {
        json!(f)
    }
}

/// `fmt.Sprint` d'une valeur JSON décodée par le Go (`any`).
pub fn go_sprint(v: &Value) -> String {
    match v {
        Value::String(s) => s.clone(),
        Value::Number(n) => go_g(n.as_f64().unwrap_or(0.0)),
        Value::Bool(b) => b.to_string(),
        Value::Null => "<nil>".into(),
        Value::Array(a) => format!(
            "[{}]",
            a.iter().map(go_sprint).collect::<Vec<_>>().join(" ")
        ),
        Value::Object(m) => format!(
            "map[{}]",
            m.iter()
                .map(|(k, v)| format!("{k}:{}", go_sprint(v)))
                .collect::<Vec<_>>()
                .join(" ")
        ),
    }
}

/// `strconv.FormatFloat(f, 'g', -1, 64)` (le `%v` d'un float64) : le plus
/// court, en notation exponentielle hors de [1e-4, 1e6[.
pub fn go_g(f: f64) -> String {
    if f.is_nan() {
        return "NaN".into();
    }
    if f.is_infinite() {
        return if f > 0.0 { "+Inf" } else { "-Inf" }.into();
    }
    if f == 0.0 {
        return if f.is_sign_negative() { "-0" } else { "0" }.into();
    }
    let e = format!("{f:e}");
    let (mant, exp) = e.split_once('e').unwrap_or((&e, "0"));
    let exp: i32 = exp.parse().unwrap_or(0);
    if !(-4..6).contains(&exp) {
        let sign = if exp < 0 { '-' } else { '+' };
        return format!("{mant}e{sign}{:02}", exp.abs());
    }
    format!("{f}")
}

// ---------------------------------------------------------------------------
// Chemins
// ---------------------------------------------------------------------------

/// Les chemins de Strata sous `$LOKI_HOME`.
#[derive(Clone, Debug)]
pub struct Paths {
    pub home: PathBuf,
}

impl Paths {
    pub fn strata_home(&self) -> PathBuf {
        self.home.join("strata")
    }
    pub fn src_dir(&self) -> PathBuf {
        self.strata_home().join(format!("ajean-moe-{VERSION}"))
    }
    pub fn data_dir(&self) -> PathBuf {
        self.strata_home().join("data")
    }
    pub fn dl_dir(&self) -> PathBuf {
        self.strata_home().join("dl")
    }
    pub fn presets_dir(&self) -> PathBuf {
        self.home.join("presets")
    }
    /// `strataEngineMarker` : le paquet dont vient le moteur installé.
    pub fn engine_marker(&self) -> PathBuf {
        self.src_dir().join("engine-cuda12").join(".loki-engine")
    }
    /// `strataEngineInstalled`.
    pub fn engine_installed(&self) -> String {
        std::fs::read_to_string(self.engine_marker())
            .map(|s| s.trim().to_string())
            .unwrap_or_default()
    }
}

// ---------------------------------------------------------------------------
// Experts
// ---------------------------------------------------------------------------

/// `strataConfigTag` : strata-swift-iq3_xxs.json → swift-iq3_xxs.
pub fn config_tag(path: &str) -> String {
    let b = crate::paths::go_base(path);
    let b = b.strip_suffix(".json").unwrap_or(&b);
    match b.find('-') {
        Some(i) => b[i + 1..].to_string(),
        None => b.to_string(),
    }
}

/// `strataArenaGB` : la taille des experts du quant d'un preset (0 = inconnu).
pub fn arena_gb(cfg: &Cfg) -> f64 {
    let tag = config_tag(cfg_get(cfg, "STRATA_CONFIG").trim()).to_uppercase();
    let tag = tag.strip_prefix("SWIFT-").unwrap_or(&tag);
    quant(tag).map_or(0.0, |q| q.arena_gb)
}

/// `strataPackHasExpertsBin`.
pub fn pack_has_experts_bin(args: &[String]) -> bool {
    args.windows(2)
        .any(|w| w[0] == "--pack" && Path::new(&w[1]).join("experts.bin").exists())
}

/// `strataHelperOn` : la carte d'aide existe et n'a pas été coupée.
pub fn helper_on(cfg: &Cfg) -> bool {
    let h = cfg_get(cfg, "STRATA_HELPER_GPU").trim();
    !h.is_empty() && h != "-1" && cfg_get(cfg, "STRATA_HELPER") != "0"
}

/// `strataLayerSplit` : les deux cartes se partagent les couches
/// (`STRATA_SPLIT=layers`) au lieu que la seconde n'aide que pour une part
/// des experts. Un choix, jamais imposé : le mode d'aide reste le défaut.
pub fn layer_split(cfg: &Cfg) -> bool {
    helper_on(cfg) && cfg_get(cfg, "STRATA_SPLIT") == "layers"
}

/// `strataResolveExperts` : le mode lancé (drop, ram, arena, disk), d'après
/// le choix du preset et la RAM disponible.
pub fn resolve_experts(
    paths: &Paths,
    cfg: &Cfg,
    args: &[String],
    avail_gb: f64,
    helper_gb: f64,
) -> &'static str {
    let bin_ready = pack_has_experts_bin(args) && paths.engine_installed() == ENGINE_ASSET;
    // (jamais en partage des couches : la seconde carte n'y garde pas d'experts)
    let drop_ready =
        bin_ready && helper_on(cfg) && !layer_split(cfg) && cfg_get(cfg, "STRATA_DROP") != "0";
    let arena = arena_gb(cfg);
    match cfg_get(cfg, "STRATA_EXPERTS")
        .trim()
        .to_lowercase()
        .as_str()
    {
        EXPERTS_DISK => return if bin_ready { "arena" } else { "disk" },
        EXPERTS_RAM => return if drop_ready { "drop" } else { "ram" },
        _ => {}
    }
    if arena <= 0.0 {
        return "disk";
    }
    if drop_ready && avail_gb >= arena - helper_experts_gb(helper_gb) + DROP_HEADROOM_GB {
        return "drop";
    }
    if avail_gb >= arena + LOW_RAM_HEADROOM_GB {
        return "ram";
    }
    if bin_ready { "arena" } else { "disk" }
}

/// `strataExpertsLabel`.
pub fn experts_label(mode: &str) -> String {
    match mode {
        "drop" => "verrouillés en RAM, hors ceux de la carte d'appoint".into(),
        "ram" => "chargés et verrouillés en RAM".into(),
        "arena" => "experts.bin mappé (cache système)".into(),
        "disk" => "lus depuis le disque (mmap)".into(),
        m => m.into(),
    }
}

// ---------------------------------------------------------------------------
// Configuration de lancement
// ---------------------------------------------------------------------------

/// `strataTuning` (ordre des clés de la map Go, itérée au hasard mais
/// idempotente : chaque drapeau est posé une fois).
const TUNING: [(&str, &str); 3] = [
    ("--prefill", "16384"),
    ("--short-read", "150"),
    ("--prompt-cache-root", "256"),
];

/// `strataEnvTuning`.
const ENV_TUNING: [(&str, &str); 2] = [("STRATA_PLE_BATCH", "0"), ("STRATA_NO_LARGEPAGES", "1")];

/// `strataSetArg`.
pub fn set_arg(mut args: Vec<String>, flag: &str, value: &str) -> Vec<String> {
    for i in 0..args.len().saturating_sub(1) {
        if args[i] == flag {
            args[i + 1] = value.to_string();
            return args;
        }
    }
    args.push(flag.to_string());
    args.push(value.to_string());
    args
}

/// `strataDropFlag` : retire un drapeau SANS valeur.
pub fn drop_flag(args: Vec<String>, flag: &str) -> Vec<String> {
    args.into_iter().filter(|a| a != flag).collect()
}

/// `strataDropFlagValue` : retire un drapeau ET sa valeur.
pub fn drop_flag_value(args: Vec<String>, flag: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut i = 0;
    while i < args.len() {
        if args[i] == flag {
            i += 2;
            continue;
        }
        out.push(args[i].clone());
        i += 1;
    }
    out
}

/// `strataHasArg`.
pub fn has_arg(args: &[String], flag: &str) -> bool {
    args.iter().any(|a| a == flag)
}

/// `strataArgList`.
pub fn arg_list(cfg: &Value) -> Vec<String> {
    cfg.get("args")
        .and_then(Value::as_array)
        .map(|l| l.iter().map(go_sprint).collect())
        .unwrap_or_default()
}

fn first_non_empty<'a>(a: &'a str, b: &'a str) -> &'a str {
    if a.is_empty() { b } else { a }
}

/// `strataBuildConfig` : la config de l'installeur avec les réglages du
/// preset et les experts placés selon `mode`. Pure.
pub fn build_config(base: &Value, cfg: &Cfg, api_key: &str, mode: &str) -> Result<Value, String> {
    let mut out: Map<String, Value> = match base {
        Value::Object(m) => m.clone(),
        _ => Map::new(),
    };
    if !out.get("args").is_some_and(Value::is_array) {
        return Err("configuration de Strata sans « args »".into());
    }
    let mut args = arg_list(&Value::Object(out.clone()));
    for (flag, v) in TUNING {
        args = set_arg(args, flag, v);
    }
    let helper = cfg_get(cfg, "STRATA_HELPER_GPU").trim();
    let mut has_helper = helper_on(cfg);
    if layer_split(cfg) {
        // Partage des couches : la clé « gpu » liste les deux cartes (numéros
        // nvidia-smi), le serveur de Strata en déduit --layer-split.
        let main = cfg_get(cfg, "STRATA_MAIN_GPU")
            .trim()
            .parse::<i64>()
            .unwrap_or(0);
        let second = helper.parse::<i64>().unwrap_or(0);
        out.insert("gpu".into(), json!([main, second]));
        has_helper = false;
    }
    if has_helper {
        args = set_arg(args, "--expert-cache-device1", "auto");
        if !has_arg(&args, "--remote-expert-opt") {
            args.push("--remote-expert-opt".into());
        }
        out.remove("gpu");
        args = drop_flag_value(args, "--vram-reserve-mib");
        if let Some(Value::Object(v)) = out.get_mut("vision")
            && let Ok(n) = helper.parse::<i64>()
        {
            v.insert("cuda_device".into(), json!(n));
        }
    }
    let mut env: Map<String, Value> = match out.get("env") {
        Some(Value::Object(e)) => e.clone(),
        _ => Map::new(),
    };
    args = drop_flag(drop_flag(args, "--mmap-experts"), "--resident-experts");
    env.remove("STRATA_ARENA_MMAP");
    env.remove("STRATA_REMOTE_DROP");
    match mode {
        "drop" if has_helper => {
            args = drop_flag_value(args, "--pcie-frac");
            env.insert("STRATA_REMOTE_DROP".into(), json!("1"));
            env.insert("STRATA_STAGER_THREADS".into(), json!("8"));
            env.insert("STRATA_STAGER_RING".into(), json!("32"));
            // VRAM libre réservée de la carte d'aide : 640 Mio (défaut 1024)
            env.insert("STRATA_REMOTE_DROP_RESERVE_MIB".into(), json!("640"));
        }
        "arena" => {
            args = set_arg(args, "--pcie-frac", "0");
            env.insert("STRATA_ARENA_MMAP".into(), json!("1"));
        }
        "disk" => {
            args.push("--mmap-experts".into());
            args = set_arg(args, "--pcie-frac", "0");
        }
        _ => {}
    }
    for (k, v) in ENV_TUNING {
        env.insert(k.into(), json!(v));
    }
    if has_helper {
        // Lecture d'un prompt (moteur 1.1) : les experts de la carte d'aide
        // sont calculés par elle (source « helper ») ; le chemin direct ne
        // garde que les ajouts sous 48 jetons.
        env.insert("STRATA_PREFILL_HELPER_SOURCE".into(), json!("1"));
        args = set_arg(args, "--short-read", "48");
    }
    let drop_on = env.get("STRATA_REMOTE_DROP").and_then(Value::as_str) == Some("1");
    out.insert("env".into(), Value::Object(env));
    let ctx = cfg_get(cfg, "CTX").trim();
    if !ctx.is_empty() {
        // hors RAM, le cache KV verrouillé au-delà de DROP_MAX_CTX ne tient plus
        let capped = match ctx.parse::<i64>() {
            Ok(n) if n > DROP_MAX_CTX && drop_on => DROP_MAX_CTX.to_string(),
            _ => ctx.to_string(),
        };
        args = set_arg(args, "--max-context", &capped);
    }
    for (key, flag) in [
        ("STRATA_KV", "--kv"),
        ("STRATA_SPEC", "--spec"),
        ("STRATA_KV_RESIDENT", "--kv-resident"),
    ] {
        let v = cfg_get(cfg, key).trim();
        if !v.is_empty() {
            args = set_arg(args, flag, v);
        }
    }
    if cfg_get(cfg, "STRATA_VISION") == "0" {
        args = drop_flag(args, "--vision");
        out.remove("vision");
    }
    out.insert(
        "args".into(),
        Value::Array(args.into_iter().map(Value::String).collect()),
    );
    let port = first_non_empty(cfg_get(cfg, "PORT"), "8080")
        .parse::<i64>()
        .unwrap_or(0);
    out.insert("port".into(), json!(port));
    out.insert(
        "host".into(),
        json!(first_non_empty(cfg_get(cfg, "HOST"), "127.0.0.1")),
    );
    if !api_key.is_empty() {
        out.insert("api_key".into(), json!(api_key));
    }
    Ok(Value::Object(out))
}

/// `strataCudaDevices` : la principale en CUDA0.
pub fn cuda_devices(cfg: &Cfg) -> String {
    let main = cfg_get(cfg, "STRATA_MAIN_GPU").trim();
    if main.is_empty() || main == "-1" {
        return String::new();
    }
    if helper_on(cfg) {
        format!("{main},{}", cfg_get(cfg, "STRATA_HELPER_GPU").trim())
    } else {
        main.to_string()
    }
}

/// `strataPreflight` : ce qu'il faut pour lancer Strata.
pub fn preflight(cfg: &Cfg) -> Result<(), String> {
    let path = cfg_get(cfg, "STRATA_CONFIG").trim();
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
    let py = Path::new(&crate::paths::go_dir(path))
        .join(".venv")
        .join("bin")
        .join("python");
    if std::fs::metadata(&py).is_err() {
        return Err(format!(
            "environnement Python de Strata absent ({}) : réinstalle le modèle",
            py.to_string_lossy()
        ));
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Machine
// ---------------------------------------------------------------------------

/// `strataGPU`.
#[derive(Clone, Debug, PartialEq)]
pub struct Gpu {
    pub index: i64,
    pub name: String,
    pub vram_gb: f64,
    pub arch: i64,
}

impl Gpu {
    fn to_json(&self) -> Value {
        json!({ "index": self.index, "name": self.name, "vram_gb": go_num(self.vram_gb), "arch": self.arch })
    }
}

/// `strataEnv`.
#[derive(Clone, Debug, PartialEq)]
pub struct Env {
    pub supported: bool,
    pub reason: String,
    pub gpus: Vec<Gpu>,
    pub main: i64,
    pub helper: i64,
    pub ram_gb: f64,
    pub avail_gb: f64,
    pub disk_free_gb: f64,
    pub python: String,
}

impl Default for Env {
    fn default() -> Self {
        Env {
            supported: false,
            reason: String::new(),
            gpus: Vec::new(),
            main: -1,
            helper: -1,
            ram_gb: 0.0,
            avail_gb: 0.0,
            disk_free_gb: 0.0,
            python: String::new(),
        }
    }
}

impl Env {
    pub fn to_json(&self) -> Value {
        let mut v = json!({
            "supported": self.supported,
            "gpus": if self.gpus.is_empty() { Value::Null } else { Value::Array(self.gpus.iter().map(Gpu::to_json).collect()) },
            "main": self.main, "helper": self.helper,
            "ram_gb": go_num(self.ram_gb), "avail_gb": go_num(self.avail_gb),
            "disk_free_gb": go_num(self.disk_free_gb),
        });
        if !self.reason.is_empty() {
            v["reason"] = json!(self.reason);
        }
        if !self.python.is_empty() {
            v["python"] = json!(self.python);
        }
        v
    }
}

/// `strataTag` : « swift-IQ3_XXS », « IQ3_XXS ».
pub fn tag(family: &str, quant: &str) -> String {
    if family == "swift" {
        format!("swift-{quant}")
    } else {
        quant.to_string()
    }
}

/// `strataDirGB` : la taille des fichiers d'un dossier, en Go.
pub fn dir_gb(dir: &Path) -> f64 {
    fn walk(p: &Path) -> u64 {
        let Ok(rd) = std::fs::read_dir(p) else {
            return 0;
        };
        let mut t = 0;
        for e in rd.flatten() {
            let Ok(ft) = e.file_type() else { continue };
            if ft.is_dir() {
                t += walk(&e.path());
            } else if ft.is_file() {
                t += e.metadata().map_or(0, |m| m.len());
            }
        }
        t
    }
    walk(dir) as f64 / 1e9
}

/// `strataHaveGB` : ce qui est déjà sur le disque pour CE choix.
pub fn have_gb(paths: &Paths, family: &str, quant: &str) -> f64 {
    let t = tag(family, quant);
    let data = paths.data_dir();
    dir_gb(&data.join("models").join(&t))
        + dir_gb(&data.join("packs").join(t.to_lowercase()))
        + dir_gb(&data.join("mtp"))
}

/// `strataFit`.
#[derive(Clone, Debug, PartialEq)]
pub struct Fit {
    pub quant: Quant,
    pub disk_gb: f64,
    pub mmap: bool,
    pub drop: bool,
    pub ok: bool,
    pub why: String,
    pub preset: String,
    pub active: bool,
    pub details: Option<Value>,
}

impl Fit {
    pub fn to_json(&self) -> Value {
        let mut v = json!({
            "quant": self.quant.to_json(), "disk_gb": go_num(self.disk_gb),
            "mmap": self.mmap, "drop": self.drop, "ok": self.ok,
        });
        if !self.why.is_empty() {
            v["why"] = json!(self.why);
        }
        if !self.preset.is_empty() {
            v["preset"] = json!(self.preset);
        }
        if self.active {
            v["active"] = json!(true);
        }
        if let Some(d) = &self.details {
            v["details"] = d.clone();
        }
        v
    }
}

/// `%.0f` du Go (arrondi au plus proche, demi vers le pair).
pub fn f0(x: f64) -> String {
    format!("{x:.0}")
}

/// `strataFitFor` : ce qu'un quant demande sur cette machine, et s'il y tient.
pub fn fit_for(q: Quant, env: &Env, have: f64) -> Fit {
    let mut f = Fit {
        quant: q,
        disk_gb: (q.download_gb - have).max(10.0),
        mmap: env.avail_gb < q.arena_gb + LOW_RAM_HEADROOM_GB,
        drop: false,
        ok: false,
        why: String::new(),
        preset: String::new(),
        active: false,
        details: None,
    };
    let vram: f64 = env
        .gpus
        .iter()
        .filter(|g| g.index == env.main || g.index == env.helper)
        .map(|g| g.vram_gb)
        .sum();
    if env.ram_gb + vram - 12.0 < q.arena_gb {
        f.why = format!(
            "demande plus de mémoire : {} Go de RAM + VRAM, il en faut ~{}",
            f0(env.ram_gb + vram),
            f0(q.arena_gb + 12.0)
        );
    } else if env.disk_free_gb > 0.0 && env.disk_free_gb < f.disk_gb + 5.0 {
        f.why = format!(
            "pas assez d'espace disque : {} Go libres, il en faut ~{}",
            f0(env.disk_free_gb),
            f0(f.disk_gb + 5.0)
        );
    } else {
        f.ok = true;
    }
    f
}

/// `strataRecommend` : le quant le plus fidèle qui tient.
pub fn recommend(paths: &Paths, family: &str, env: &Env) -> String {
    let Some(fam) = FAMILIES.iter().find(|f| f.id == family) else {
        return String::new();
    };
    let mut best = String::new();
    for id in ["IQ3_XXS", "IQ2_XS", "Q2_0"] {
        if !fam.quants.contains(&id) {
            continue;
        }
        let q = quant(id).expect("quant");
        if fit_for(q, env, have_gb(paths, family, id)).ok {
            best = id.to_string();
            break;
        }
    }
    if fam.quants.contains(&"IQ3_S") {
        let q = quant("IQ3_S").expect("quant");
        let f = fit_for(q, env, have_gb(paths, family, "IQ3_S"));
        if f.ok && !f.mmap {
            best = "IQ3_S".into();
        }
    }
    best
}

/// `strataPresetName`.
pub fn preset_name(family: &str, quant: &str) -> String {
    let label = if family == "swift" {
        "Swift 1.5"
    } else {
        "Classique"
    };
    format!("STRATA FLASH NEXT {} {quant}", label.to_uppercase())
}

/// `strataFamilyLabel`.
pub fn family_label(id: &str) -> String {
    match FAMILIES.iter().find(|f| f.id == id) {
        Some(f) if f.id == "qwen" => "Qwen3.8 Flash Next".into(),
        Some(f) => format!("Qwen3.8 Flash Next {}", f.label),
        None => id.into(),
    }
}

/// `strataBool`.
pub fn sbool(b: bool) -> &'static str {
    if b { "1" } else { "0" }
}

/// `strataSettingsReq`.
#[derive(Clone, Debug, Default)]
pub struct SettingsReq {
    pub preset: String,
    pub ctx: i64,
    pub kv: String,
    pub spec: i64,
    pub vision: bool,
    pub helper: bool,
    pub experts: String,
    /// « helper » ou « layers » ; vide = inchangé (anciens clients).
    pub split: String,
}

/// `strataApplySettings` : réécrit les clés du preset. Pure.
pub fn apply_settings(content: &str, r: &SettingsReq) -> Result<String, String> {
    if r.ctx < 32768 || r.ctx > MAX_CTX || r.ctx % 4096 != 0 {
        return Err(format!("contexte hors limites : {}", r.ctx));
    }
    if r.kv != "fp16" && r.kv != "int8" {
        return Err(format!("cache KV inconnu : {}", r.kv));
    }
    if r.spec < 2 || r.spec > 4 {
        return Err(format!("MTP hors limites : {}", r.spec));
    }
    if !r.experts.is_empty()
        && r.experts != EXPERTS_AUTO
        && r.experts != EXPERTS_RAM
        && r.experts != EXPERTS_DISK
    {
        return Err(format!("placement des experts inconnu : {}", r.experts));
    }
    if !r.split.is_empty() && r.split != "helper" && r.split != "layers" {
        return Err(format!("répartition inconnue : {}", r.split));
    }
    let mut set: Vec<(&str, String)> = vec![
        ("CTX", r.ctx.to_string()),
        ("STRATA_KV", r.kv.clone()),
        ("STRATA_SPEC", r.spec.to_string()),
        ("STRATA_VISION", sbool(r.vision).into()),
        ("STRATA_HELPER", sbool(r.helper).into()),
    ];
    if !r.experts.is_empty() {
        set.push(("STRATA_EXPERTS", r.experts.clone()));
    }
    if !r.split.is_empty() {
        // « helper » est le défaut : la clé disparaît plutôt que d'être écrite
        let v = if r.split == "layers" { "layers" } else { "" };
        set.push(("STRATA_SPLIT", v.into()));
    }
    let mut lines: Vec<String> = Vec::new();
    for l in content.trim_end_matches('\n').split('\n') {
        let k = l.trim().split_once('=').map_or(l.trim(), |(k, _)| k);
        if set.iter().any(|(s, _)| *s == k) {
            continue;
        }
        lines.push(l.to_string());
    }
    for (k, v) in set {
        if k == "STRATA_SPLIT" && v.is_empty() {
            continue;
        }
        lines.push(format!("{k}={v}"));
    }
    Ok(lines.join("\n") + "\n")
}

/// `strataHelperVRAM` : VRAM de la carte d'appoint du preset, 0 sans.
pub fn helper_vram(cfg: &Cfg, gpus: &[Gpu]) -> f64 {
    if !helper_on(cfg) {
        return 0.0;
    }
    let h = cfg_get(cfg, "STRATA_HELPER_GPU").trim();
    gpus.iter()
        .find(|g| g.index.to_string() == h)
        .map_or(0.0, |g| g.vram_gb)
}

/// `strataFindConfig` : la config que l'installeur a écrite pour ce quant.
pub fn find_config(src_dir: &Path, quant: &str) -> Result<String, String> {
    let q = quant.to_lowercase();
    let mut names: Vec<String> = std::fs::read_dir(src_dir)
        .map(|rd| {
            rd.flatten()
                .map(|e| e.file_name().to_string_lossy().into_owned())
                .filter(|n| n.starts_with("strata-") && n.ends_with(".json"))
                .collect()
        })
        .unwrap_or_default();
    names.sort();
    for n in names {
        if n.to_lowercase().contains(&q) {
            return Ok(src_dir.join(n).to_string_lossy().into_owned());
        }
    }
    Err(format!(
        "l'installeur n'a pas laissé de configuration pour {quant} dans {}",
        src_dir.to_string_lossy()
    ))
}

#[cfg(test)]
mod tests;
