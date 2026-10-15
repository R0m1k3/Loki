//! Presets tenus par la façade — port de `backend_presets.go` (écriture),
//! des handlers de presets de `web_api.go`, de `web_external.go`, et des
//! aperçus de l'éditeur (`cacheRAMPreview`, `sideSlotPreview`, `moePreview`,
//! `autoPlacementPreset`) — lot 15 de la migration, servi quand
//! `LOKI_PRESETS=rust` (expérimental ; le Go par défaut) :
//!
//! - `GET /api/presets` (pastilles et mesures), `POST /api/presets/order` ;
//! - `GET /api/preset?id=`, `POST /api/preset/save`, `/api/preset/delete` ;
//! - `POST /api/preset/cacheram`, `/api/preset/moe`, `/api/preset/autoplace` ;
//! - `GET /api/preset/external?id=`, `POST /api/preset/external/save`,
//!   `/api/preset/external/test` ;
//! - `POST /api/switch`.
//!
//! Les presets restent des fichiers `presets/*.env`. Ce qui touche au moteur
//! reste au Go jusqu'à son lot : le verrou de l'optimiseur
//! (`/internal/v1/tune/guard`) et l'application d'un preset à la
//! configuration vivante, état du slot gardé et moteur relancé
//! (`/internal/v1/preset/apply`).

use std::collections::{BTreeMap, HashMap};
use std::path::{Path, PathBuf};

use http_body_util::{BodyExt, Limited};
use hyper::body::Incoming;
use hyper::{Method, Request, Response, StatusCode};
use serde_json::{Value, json};

use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::gojson::{self, Kind};
use crate::presets::{
    PRESET_ORDER_KEY, config_fingerprint, list_presets, parse_env, parse_preset_order, preset_ctx,
    preset_display_name, preset_fingerprint, presets_dir, unquote_value,
};
use crate::proxy::Body;
use crate::serve::{self, Cfg, SysInfo};
use crate::store::{BK_STATE, Store};

const SYSPROMPT_PREFIX: &str = "sysprompt:";
const BENCH_KEY: &str = "bench_presets";
const BENCH_PROTOCOL: &str = "2";
/// `previewServeHelp` : l'aide supposée d'un moteur récent.
pub const PREVIEW_SERVE_HELP: &str = "--load-mode  --fit [on|off]  --fit-target";

/// Routes servies ici.
pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/presets"
            | "/api/presets/order"
            | "/api/preset"
            | "/api/preset/save"
            | "/api/preset/cacheram"
            | "/api/preset/moe"
            | "/api/preset/autoplace"
            | "/api/preset/delete"
            | "/api/preset/external"
            | "/api/preset/external/save"
            | "/api/preset/external/test"
            | "/api/switch"
    )
}

fn trim(s: &str) -> &str {
    s.trim_matches(char::is_whitespace)
}

fn get<'a>(cfg: &'a Cfg, k: &str) -> &'a str {
    cfg.get(k).map_or("", String::as_str)
}

// ---------------------------------------------------------------------------
// Fonctions pures
// ---------------------------------------------------------------------------

/// `withDisplayName` : la ligne `# NAME=` remplacée, ou ajoutée en tête.
pub fn with_display_name(content: &str, name: &str) -> String {
    let re = regex::Regex::new(r"(?mi)^[ \t]*#?[ \t]*NAME[ \t]*=.*$").expect("motif");
    let line = format!("# NAME={name}");
    if re.is_match(content) {
        // ReplaceAllString du Go : « $x » est développé, comme ici.
        return re.replace_all(content, line.as_str()).into_owned();
    }
    format!("{line}\n{content}")
}

/// `quoteValue`.
pub fn quote_value(v: &str) -> String {
    if v.is_empty() || v.contains('"') {
        return v.to_string();
    }
    if v.contains([' ', '\t']) || unquote_value(v) != v {
        return format!("\"{v}\"");
    }
    v.to_string()
}

/// `formatEnv` : clés triées.
pub fn format_env(m: &BTreeMap<String, String>) -> String {
    m.iter()
        .map(|(k, v)| format!("{k}={}\n", quote_value(v)))
        .collect()
}

/// `validPresetName`.
pub fn valid_preset_name(name: &str) -> Result<(), String> {
    if name.is_empty() {
        return Err("nom vide".into());
    }
    if name == "." || name == ".." {
        return Err("nom réservé".into());
    }
    if name.contains(['/', '\\', '\0']) {
        return Err("le nom ne peut pas contenir / ni \\".into());
    }
    if name.chars().any(|c| (c as u32) < 0x20) {
        return Err("le nom contient un caractère de contrôle invalide".into());
    }
    Ok(())
}

/// `safePresetPath`.
pub fn safe_preset_path(home: &Path, name: &str) -> Result<PathBuf, String> {
    let name = trim(name);
    valid_preset_name(name)?;
    Ok(presets_dir(home).join(format!("{name}.env")))
}

/// `uniquePresetID`.
pub fn unique_preset_id(home: &Path, name: &str) -> Result<String, String> {
    let replaced = name.replace(['/', '\\'], "-");
    let mut base = trim(&replaced).to_string();
    if base.is_empty() {
        base = "preset".into();
    }
    let mut cand = base.clone();
    for n in 2..10000 {
        let p = safe_preset_path(home, &cand)?;
        if matches!(std::fs::metadata(&p), Err(e) if e.kind() == std::io::ErrorKind::NotFound) {
            return Ok(cand);
        }
        cand = format!("{base} ({n})");
    }
    Err("impossible de générer un nom de fichier unique".into())
}

/// `ReadPreset`.
pub fn read_preset(home: &Path, id: &str) -> Result<String, String> {
    let p = safe_preset_path(home, id)?;
    std::fs::read(&p)
        .map(|b| String::from_utf8_lossy(&b).into_owned())
        .map_err(|e| crate::paths::os_err("open", &p.to_string_lossy(), &e))
}

fn write_preset(p: &Path, content: &str) -> Result<(), String> {
    if let Some(d) = p.parent() {
        let _ = std::fs::create_dir_all(d);
    }
    let mut o = std::fs::OpenOptions::new();
    o.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        o.mode(0o644);
    }
    use std::io::Write as _;
    o.open(p)
        .and_then(|mut f| f.write_all(content.as_bytes()))
        .map_err(|e| crate::paths::os_err("open", &p.to_string_lossy(), &e))
}

/// `SavePreset` : création (id vide, nom de fichier unique) ou mise à jour.
pub fn save_preset(home: &Path, id: &str, name: &str, content: &str) -> Result<String, String> {
    let name = trim(name);
    if name.is_empty() {
        return Err("nom requis".into());
    }
    let content = with_display_name(content, name);
    let id = if id.is_empty() {
        unique_preset_id(home, name)?
    } else {
        id.to_string()
    };
    let p = safe_preset_path(home, &id)?;
    write_preset(&p, &content)?;
    Ok(id)
}

/// `presetReasoning`.
pub fn preset_reasoning(content: &str) -> String {
    for line in content.split('\n') {
        let s = trim(line);
        if s.is_empty() || s.starts_with('#') {
            continue;
        }
        if let Some((k, v)) = s.split_once('=')
            && trim(k).eq_ignore_ascii_case("REASONING")
        {
            return unquote_value(trim(v)).to_string();
        }
    }
    String::new()
}

/// `modelFromPresetContent`.
pub fn model_from_preset_content(content: &str) -> String {
    for line in content.split('\n') {
        let s = trim(line);
        if s.is_empty() || s.starts_with('#') {
            continue;
        }
        if let Some((k, v)) = s.split_once('=')
            && trim(k) == "MODEL"
        {
            return unquote_value(trim(v)).to_string();
        }
    }
    String::new()
}

/// `detectQuant` : une ligne QUANT= (avec ou sans `#`), sinon le nom du
/// modèle.
pub fn detect_quant(content: &str) -> String {
    for line in content.split('\n') {
        let t = trim(line);
        let s = trim(t.strip_prefix('#').unwrap_or(t));
        if let Some((k, v)) = s.split_once('=')
            && trim(k).eq_ignore_ascii_case("QUANT")
        {
            let v = unquote_value(trim(v));
            if !v.is_empty() {
                return v.to_uppercase();
            }
        }
    }
    crate::models::quant_from_name(&crate::paths::base_name(&model_from_preset_content(
        content,
    )))
}

/// `completionsURL`.
pub fn completions_url(raw: &str) -> String {
    let u = trim(raw).trim_end_matches('/');
    if u.is_empty() {
        return String::new();
    }
    if u.ends_with("/chat/completions") {
        return u.to_string();
    }
    if u.ends_with("/v1") {
        return format!("{u}/chat/completions");
    }
    format!("{u}/v1/chat/completions")
}

/// `externalPresetContent`.
pub fn external_preset_content(
    url: &str,
    model: &str,
    key: &str,
    ctx: &str,
    vision: bool,
) -> String {
    let mut m = BTreeMap::new();
    m.insert("EXTERNAL".to_string(), "1".to_string());
    m.insert("EXTERNAL_URL".into(), trim(url).into());
    m.insert("EXTERNAL_MODEL".into(), trim(model).into());
    if !trim(key).is_empty() {
        m.insert("EXTERNAL_KEY".into(), trim(key).into());
    }
    if vision {
        m.insert("EXTERNAL_VISION".into(), "1".into());
    }
    if !trim(ctx).is_empty() {
        m.insert("CTX".into(), trim(ctx).into());
    }
    format_env(&m)
}

/// `extractAPIError` : `error.message` d'une réponse au format OpenAI.
pub fn extract_api_error(b: &[u8]) -> String {
    match serde_json::from_slice::<Value>(b) {
        Ok(v) => v
            .get("error")
            .and_then(|e| e.get("message"))
            .and_then(Value::as_str)
            .map(|s| trim(s).to_string())
            .unwrap_or_default(),
        Err(_) => String::new(),
    }
}

/// `presetImposesEngine` : seul un backend personnalisé est imposé par un
/// preset ; le moteur de l'image, un téléchargé ou un précompilé suivent la
/// machine.
pub fn preset_imposes_engine(home: &Path, p: &str) -> bool {
    let p = trim(p);
    let ep = serve::run::EnginePaths {
        home: home.to_path_buf(),
    };
    if p.is_empty() || ep.engine_owns(p) || ep.prebuilt_owns(p) {
        return false;
    }
    let img = std::env::var("LOKI_ENGINE_BIN").unwrap_or_default();
    let img = trim(&img);
    if !img.is_empty() && serve::run::same_path(p, img) {
        return false;
    }
    true
}

/// `newPresetSeed` : BIN (backend personnalisé seulement), HOST, PORT.
pub fn new_preset_seed(home: &Path, cur: &Cfg) -> BTreeMap<String, String> {
    let mut seed = BTreeMap::new();
    for k in ["BIN", "HOST", "PORT"] {
        let v = get(cur, k);
        if v.is_empty() || (k == "BIN" && !preset_imposes_engine(home, v)) {
            continue;
        }
        seed.insert(k.to_string(), v.to_string());
    }
    seed
}

/// `benchFingerprint`.
pub fn bench_fingerprint(cfg: &Cfg, cur: &Cfg) -> String {
    let mut gpus = trim(get(cfg, "CUDA_VISIBLE_DEVICES")).to_string();
    if gpus.is_empty() {
        gpus = trim(get(cur, "CUDA_VISIBLE_DEVICES")).to_string();
    }
    let mut m = HashMap::new();
    m.insert("config".to_string(), config_fingerprint(cfg));
    m.insert("gpus".into(), gpus);
    m.insert("protocol".into(), BENCH_PROTOCOL.into());
    config_fingerprint(&m)
}

/// `benchMatchesPreset`.
pub fn bench_matches_preset(sb: &Value, cfg: &Cfg, cur: &Cfg) -> bool {
    let fp = sb.get("fingerprint").and_then(Value::as_str).unwrap_or("");
    if !fp.is_empty() {
        return fp == bench_fingerprint(cfg, cur);
    }
    let m = trim(get(cfg, "MODEL"));
    !m.is_empty() && sb.get("model").and_then(Value::as_str) == Some(&crate::paths::base_name(m))
}

/// Nombre JSON au rendu d'`encoding/json` (un flottant entier s'écrit sans
/// « .0 »).
fn go_num(f: f64) -> Value {
    if f.fract() == 0.0 && f.abs() < 1e15 {
        json!(f as i64)
    } else {
        json!(f)
    }
}

fn num(v: &Value, k: &str) -> f64 {
    v.get(k).and_then(Value::as_f64).unwrap_or(0.0)
}

/// Pastille de mesure d'un preset (`bench` de `handlePresets`).
fn bench_badge(sb: &Value) -> Value {
    let r = sb.get("result").cloned().unwrap_or(Value::Null);
    let mut b = serde_json::Map::new();
    b.insert("prefill".into(), go_num(num(&r, "prompt_per_second")));
    b.insert("decode".into(), go_num(num(&r, "predicted_per_second")));
    b.insert(
        "at".into(),
        json!(sb.get("at").and_then(Value::as_i64).unwrap_or(0)),
    );
    let kv = r.get("kv").and_then(Value::as_str).unwrap_or("");
    if !kv.is_empty() {
        b.insert("kv".into(), json!(kv));
        b.insert(
            "kv_quantized".into(),
            json!(
                r.get("kv_quantized")
                    .and_then(Value::as_bool)
                    .unwrap_or(false)
            ),
        );
    }
    if let Some(d) = r.get("depth").filter(|d| d.is_object()) {
        let dd = num(d, "decode_per_second");
        if let Some(c) = d.get("cold").filter(|c| c.is_object())
            && dd > 0.0
        {
            b.insert(
                "depth".into(),
                json!(c.get("new").and_then(Value::as_i64).unwrap_or(0)),
            );
            b.insert("depth_decode".into(), go_num(dd));
        }
    }
    Value::Object(b)
}

/// `joinArgs` : l'inverse de `splitArgs`.
pub fn join_args(args: &[String]) -> String {
    args.iter()
        .map(|a| {
            if a.is_empty() {
                "\"\"".to_string()
            } else if a.contains([' ', '\t']) {
                if a.contains('\'') {
                    format!("\"{a}\"")
                } else {
                    format!("'{a}'")
                }
            } else {
                a.clone()
            }
        })
        .collect::<Vec<_>>()
        .join(" ")
}

/// `presetSetKey` : lignes KEY= remplacées (toutes), ajoutée à la fin si
/// absente, retirées si `val` est vide.
pub fn preset_set_key(content: &str, key: &str, val: &str) -> String {
    let k = regex::escape(key);
    if val.is_empty() {
        let re = regex::Regex::new(&format!(
            r"(?m)^[ \t]*(?:export[ \t]+)?{k}[ \t]*=.*(?:\n|$)"
        ))
        .expect("motif");
        return re.replace_all(content, regex::NoExpand("")).into_owned();
    }
    let re =
        regex::Regex::new(&format!(r"(?m)^[ \t]*(?:export[ \t]+)?{k}[ \t]*=.*$")).expect("motif");
    let line = format!("{key}={}", quote_value(val));
    if re.is_match(content) {
        return re.replace_all(content, regex::NoExpand(&line)).into_owned();
    }
    let mut c = content.to_string();
    if !c.is_empty() && !c.ends_with('\n') {
        c.push('\n');
    }
    c + &line + "\n"
}

/// `autoPlacementPreset` : la COPIE d'un preset MoE placé à la main, en
/// placement automatique, et ce qui change.
pub fn auto_placement_preset(content: &str) -> Result<(String, Vec<String>), String> {
    let cfg = parse_env(content);
    if serve::is_external(&cfg) {
        return Err("preset externe : le modèle tourne ailleurs".into());
    }
    let extra = crate::models::split_args(get(&cfg, "EXTRA_ARGS"));
    let mut kept: Vec<String> = Vec::new();
    let mut changes: Vec<String> = Vec::new();
    let mut migrated = false;
    let mut i = 0;
    while i < extra.len() {
        let a = &extra[i];
        let (name, val, has_eq) = match a.split_once('=') {
            Some((n, v)) => (n.to_string(), v.to_string(), true),
            None => (a.clone(), String::new(), false),
        };
        let mut value = || -> String {
            if has_eq {
                return val.clone();
            }
            if i + 1 < extra.len() {
                i += 1;
                return extra[i].clone();
            }
            String::new()
        };
        match name.as_str() {
            "-ot" | "--override-tensor" => {
                let v = value();
                let mut rest: Vec<String> = Vec::new();
                for el in v.split(',') {
                    let el = trim(el);
                    let pat = el.split_once('=').map_or(el, |(p, _)| p);
                    if el.is_empty() {
                    } else if serve::ot_targets_experts(pat) {
                        changes.push(format!("retiré : {name} {el} (experts : --fit les place)"));
                        migrated = true;
                    } else if pat.to_lowercase().contains("per_layer_token_embd")
                        && serve::ot_on_cpu(el)
                    {
                        changes.push(format!(
                            "retiré : {name} {el} (couche d'entrée, toujours au CPU)"
                        ));
                    } else {
                        rest.push(el.to_string());
                    }
                }
                if !rest.is_empty() {
                    kept.push(name.clone());
                    kept.push(rest.join(","));
                }
            }
            "-ncmoe" | "--n-cpu-moe" => {
                let v = value();
                changes.push(format!("retiré : {name} {v}"));
                if trim(&v) != "0" {
                    migrated = true;
                }
            }
            "-cmoe" | "--cpu-moe" => {
                changes.push(format!("retiré : {name}"));
                migrated = true;
            }
            "-ngl" | "--n-gpu-layers" | "--gpu-layers" | "-ts" | "--tensor-split" | "-b"
            | "--batch-size" | "-ub" | "--ubatch-size" | "--load-mode" | "-lm" => {
                let v = value();
                changes.push(format!("retiré : {name} {v}"));
            }
            "--mlock" | "--no-mmap" | "--mmap" => changes.push(format!("retiré : {name}")),
            "-fit" | "--fit" => {
                let v = value();
                if matches!(
                    trim(&v).to_lowercase().as_str(),
                    "off" | "disabled" | "false" | "0"
                ) {
                    changes.push(format!("retiré : {name} {v}"));
                } else {
                    kept.push(name.clone());
                    kept.push(v);
                }
            }
            _ => kept.push(a.clone()),
        }
        i += 1;
    }
    if !migrated {
        return Err("rien à migrer : ce preset ne place pas d'experts à la main (-ot …exps…, --n-cpu-moe, --cpu-moe)".into());
    }
    kept.push("--load-mode".into());
    kept.push("mmap".into());
    let mut nc = cfg.clone();
    nc.insert("EXTRA_ARGS".into(), join_args(&kept));
    nc.insert("UBATCH".into(), "2048".into());
    nc.insert("BATCH".into(), "4096".into());
    nc.remove("NGL");
    let (ctx, ok) = serve::ctx_fixed(&nc, &kept);
    if !ok {
        return Err(format!(
            "contexte « {ctx} » non chiffré : --fit pourrait le réduire pour tenir — donne un CTX chiffré d'abord"
        ));
    }
    if serve::split_mode_key(&nc) == "tensor" {
        return Err("SPLIT_MODE=tensor coupe --fit : retire-le d'abord".into());
    }
    let si = SysInfo {
        help: PREVIEW_SERVE_HELP.into(),
        ..SysInfo::default()
    };
    let why = serve::fit_blocker(&nc, &kept, &si);
    if !why.is_empty() {
        return Err(format!(
            "--fit resterait inactif ({why}) : retire-le à la main d'abord"
        ));
    }
    let ngl = trim(get(&cfg, "NGL"));
    if !ngl.is_empty() {
        changes.push(format!(
            "NGL={ngl} retiré : le moteur place les couches (-ngl auto)"
        ));
    }
    changes.push(format!(
        "UBATCH=2048, BATCH=4096 (avant : {} / {})",
        serve_first(&[get(&cfg, "UBATCH"), "512"]),
        serve_first(&[get(&cfg, "BATCH"), "2048"])
    ));
    changes.push("--load-mode mmap".into());
    changes.push(format!("CTX conservé : {ctx}"));
    let (k, v) = serve::serve_kv_types(&nc);
    if !k.is_empty()
        || !v.is_empty()
        || !serve::flag_value(&kept, &["-ctk", "--cache-type-k"]).is_empty()
        || !serve::flag_value(&kept, &["-ctv", "--cache-type-v"]).is_empty()
    {
        changes.push(
            "cache KV conservé tel quel — garde-le identique pour comparer les deux presets".into(),
        );
    }
    let mut out = content.to_string();
    out = preset_set_key(&out, "EXTRA_ARGS", get(&nc, "EXTRA_ARGS"));
    out = preset_set_key(&out, "UBATCH", "2048");
    out = preset_set_key(&out, "BATCH", "4096");
    out = preset_set_key(&out, "NGL", "");
    Ok((out, changes))
}

fn serve_first<'a>(vals: &[&'a str]) -> &'a str {
    vals.iter()
        .copied()
        .find(|v| !trim(v).is_empty())
        .unwrap_or("")
}

// ---------------------------------------------------------------------------
// Aperçus de l'éditeur (lectures du monde : GGUF, RAM, nvidia-smi)
// ---------------------------------------------------------------------------

/// Résolution d'un MODEL= pour les aperçus.
pub struct Resolver {
    pub home: PathBuf,
    pub stored: Vec<String>,
    pub env: Option<String>,
}

impl Resolver {
    fn resolve(&self, m: &str) -> String {
        crate::gguf::resolve_serve_model_path(m, &self.home, &self.stored, self.env.as_deref())
            .map(|p| p.to_string_lossy().into_owned())
            .unwrap_or_default()
    }
}

fn lookup_env(si: &mut SysInfo, keys: &[&[&str]], trimmed: bool) {
    for group in keys {
        for k in *group {
            if let Ok(v) = std::env::var(k) {
                let keep = if trimmed {
                    !trim(&v).is_empty()
                } else {
                    !v.is_empty()
                };
                if keep {
                    si.arg_env.insert((*k).into(), v);
                }
            }
        }
    }
}

/// `cacheRAMPreview` : (Mio, pourquoi) ; 0 = défaut du moteur.
pub fn cache_ram_preview(r: &Resolver, content: &str) -> (i64, String) {
    let cfg = parse_env(content);
    if serve::is_external(&cfg) {
        return (0, "preset externe".into());
    }
    let extra = crate::models::split_args(get(&cfg, "EXTRA_ARGS"));
    let v = serve::flag_value(&extra, &["-cram", "--cache-ram"]);
    if !v.is_empty() {
        return (v.parse().unwrap_or(0), "fixé par EXTRA_ARGS".into());
    }
    let e = std::env::var("LLAMA_ARG_CACHE_RAM").unwrap_or_default();
    if !trim(&e).is_empty() {
        return (
            trim(&e).parse().unwrap_or(0),
            "fixé par LLAMA_ARG_CACHE_RAM".into(),
        );
    }
    let (manual, auto, ok) = serve::cache_ram_setting(&cfg);
    if !ok {
        return (0, "CACHE_RAM illisible".into());
    }
    if !auto {
        return (manual.parse().unwrap_or(0), "fixé par CACHE_RAM".into());
    }
    let mut si = SysInfo::default();
    lookup_env(
        &mut si,
        &[
            serve::SERVE_ARG_ENV,
            serve::FIDELITY_ARG_ENV,
            serve::CACHE_ARG_ENV,
        ],
        false,
    );
    let m = trim(get(&cfg, "MODEL"));
    if !m.is_empty() {
        si.model = r.resolve(m);
    }
    if si.model.is_empty() {
        return (0, "modèle introuvable".into());
    }
    serve::run::probe_cache_ram(&serve::run::Env::default(), &cfg, &extra, &mut si);
    serve::cache_ram_auto(&cfg, &extra, &si)
}

/// `sideSlotPreview` : (accepté, Mio, pourquoi).
pub fn side_slot_preview(r: &Resolver, content: &str) -> (bool, i64, String) {
    let cfg = parse_env(content);
    if !serve::side_slot_on(&cfg) {
        return (false, 0, String::new());
    }
    if serve::is_external(&cfg) {
        return (false, 0, "preset externe".into());
    }
    let extra = crate::models::split_args(get(&cfg, "EXTRA_ARGS"));
    let mut si = SysInfo {
        help: "--no-kv-unified".into(),
        ..SysInfo::default()
    };
    lookup_env(
        &mut si,
        &[
            serve::SERVE_ARG_ENV,
            serve::FIDELITY_ARG_ENV,
            serve::CACHE_ARG_ENV,
            serve::SIDE_SLOT_ARG_ENV,
        ],
        true,
    );
    let m = trim(get(&cfg, "MODEL"));
    if !m.is_empty() {
        si.model = r.resolve(m);
    }
    if si.model.is_empty() {
        return (false, 0, "modèle introuvable".into());
    }
    serve::run::probe_cache_ram(&serve::run::Env::default(), &cfg, &extra, &mut si);
    let (ctx, mib, why) = serve::side_slot_plan(&cfg, &extra, &si);
    (ctx > 0, mib, why)
}

/// `moePreview` : (notes, avis de chargement).
pub fn moe_preview(r: &Resolver, live_bin: &str, content: &str) -> (Vec<String>, String) {
    let cfg = parse_env(content);
    if serve::is_external(&cfg) {
        return (Vec::new(), String::new());
    }
    let mut si = SysInfo {
        help: PREVIEW_SERVE_HELP.into(),
        ..SysInfo::default()
    };
    lookup_env(
        &mut si,
        &[
            serve::SERVE_ARG_ENV,
            serve::FIT_ARG_ENV,
            serve::LOAD_ARG_ENV,
        ],
        true,
    );
    let m = trim(get(&cfg, "MODEL"));
    if !m.is_empty() {
        si.model = r.resolve(m);
    }
    if si.model.is_empty() {
        return (Vec::new(), String::new());
    }
    let extra = crate::models::split_args(get(&cfg, "EXTRA_ARGS"));
    serve::run::probe_cache_ram(&serve::run::Env::default(), &cfg, &extra, &mut si);
    let (norm, _) = serve::normalize_load_flags(&extra, true);
    if serve::load_guard_needs_devices(&cfg, &norm, &si) {
        let ep = serve::run::EnginePaths {
            home: r.home.clone(),
        };
        let bin = ep.prebuilt_resolve_bin(serve_first(&[get(&cfg, "BIN"), live_bin]));
        let key = format!("{bin}\0{}", get(&cfg, "CUDA_VISIBLE_DEVICES"));
        if let Some(devs) = dev_persist_get(&r.home, &key) {
            si.engine_vram_mib = serve::engine_devs_vram(&devs);
        }
    }
    let (mut warn, refuse) = serve::load_mode_risk(&cfg, &norm, &si);
    if !refuse.is_empty() {
        warn = format!("refusé au lancement — {refuse}");
    }
    (serve::moe_notes(&cfg, &extra, &si), warn)
}

/// `devPersistGet` : la dernière liste de cartes donnée à l'éditeur
/// (`devices.json`).
fn dev_persist_get(home: &Path, key: &str) -> Option<Vec<serve::SplitDev>> {
    let b = std::fs::read(home.join("devices.json")).ok()?;
    let m: HashMap<String, Vec<Value>> = serde_json::from_slice(&b).ok()?;
    let list = m.get(key).filter(|l| !l.is_empty())?;
    Some(
        list.iter()
            .map(|d| serve::SplitDev {
                id: d.get("id").and_then(Value::as_str).unwrap_or("").into(),
                total_mib: d.get("total_mib").and_then(Value::as_f64).unwrap_or(0.0) as i64,
            })
            .collect(),
    )
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

fn fail(code: StatusCode, e: impl Into<String>) -> Response<Body> {
    send_json(code, &json!({ "ok": false, "error": e.into() }))
}

async fn read_body(body: Incoming, limit: usize) -> Result<Vec<u8>, String> {
    Limited::new(body, limit)
        .collect()
        .await
        .map(|b| b.to_bytes().to_vec())
        .map_err(|_| "http: request body too large".to_string())
}

const PRESET_SAVE_REQ: &[(&str, Kind)] = &[
    ("id", Kind::Str),
    ("name", Kind::Str),
    ("content", Kind::Str),
    ("deleteModel", Kind::Bool),
    ("sysprompt", Kind::OptStr),
];

const EXTERNAL_SAVE_REQ: &[(&str, Kind)] = &[
    ("id", Kind::Str),
    ("name", Kind::Str),
    ("url", Kind::Str),
    ("model", Kind::Str),
    ("key", Kind::Str),
    ("ctx", Kind::Str),
    ("keyTouched", Kind::Bool),
    ("vision", Kind::Bool),
];

const CONTENT_REQ_TYPE: &str = r#"struct { Content string "json:\"content\"" }"#;
const ORDER_REQ_TYPE: &str = r#"struct { IDs []string "json:\"ids\"" }"#;
const AUTOPLACE_REQ_TYPE: &str = r#"struct { ID string "json:\"id\""; Name string "json:\"name\""; Content string "json:\"content\"" }"#;
const SWITCH_REQ_TYPE: &str = r#"struct { N int "json:\"n\""; ID string "json:\"id\"" }"#;

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

/// Applique un preset à la configuration vivante (au Go, voir
/// `facade_presets.go`).
async fn preset_apply(store: &Store, path: &Path, mode: &str) -> Result<(), String> {
    let (code, v) = store
        .internal_post(
            "/internal/v1/preset/apply",
            &json!({ "path": path.to_string_lossy(), "mode": mode }),
        )
        .await?;
    if code.is_success() {
        return Ok(());
    }
    Err(v
        .get("error")
        .and_then(Value::as_str)
        .map(str::to_string)
        .unwrap_or_else(|| format!("application du preset : {code}")))
}

struct Live {
    cfg: Cfg,
    order: Vec<String>,
}

async fn live(store: &Store) -> Live {
    Live {
        cfg: store.config().await,
        order: parse_preset_order(
            store
                .get(BK_STATE, PRESET_ORDER_KEY)
                .await
                .ok()
                .flatten()
                .as_deref(),
        ),
    }
}

/// `SavePresetApplying` : enregistre ; si c'était le preset EN SERVICE (avant
/// l'écriture), l'applique (`mode` : save ou apply).
async fn save_applying(
    ctx: &Ctx,
    id: &str,
    name: &str,
    content: &str,
    mode: &str,
) -> Result<(String, bool), String> {
    let home = &ctx.engine.home;
    let mut was_active = false;
    if !id.is_empty()
        && let Ok(old) = read_preset(home, id)
    {
        was_active = preset_fingerprint(&old) == config_fingerprint(&ctx.store.config().await);
    }
    let new_id = save_preset(home, id, name, content)?;
    if !was_active {
        return Ok((new_id, false));
    }
    let p = safe_preset_path(home, &new_id)?;
    preset_apply(&ctx.store, &p, mode).await?;
    Ok((new_id, true))
}

async fn resolver(ctx: &Ctx) -> Resolver {
    let d = crate::models::model_dirs(ctx).await;
    Resolver {
        home: d.home,
        stored: d.stored,
        env: d.env,
    }
}

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(ctx: &Ctx, req: Request<Incoming>, from_go: bool) -> Response<Body> {
    let (parts, body) = req.into_parts();
    if !from_go && let Err(refus) = ctx.guard(&parts.headers).await {
        return *refus;
    }
    let store = &ctx.store;
    let home = ctx.engine.home.clone();
    let mut query: HashMap<String, String> = HashMap::new();
    for (k, v) in url::form_urlencoded::parse(parts.uri.query().unwrap_or("").as_bytes()) {
        query
            .entry(k.into_owned())
            .or_insert_with(|| v.into_owned());
    }
    let q = |k: &str| trim(query.get(k).map_or("", String::as_str)).to_string();
    match parts.uri.path() {
        "/api/presets" => {
            let lv = live(store).await;
            let list = match list_presets(&home, &lv.cfg, &lv.order) {
                Ok(l) => l,
                Err(e) => {
                    return send_json(StatusCode::INTERNAL_SERVER_ERROR, &json!({ "error": e }));
                }
            };
            let benches: HashMap<String, Value> = store
                .get(BK_STATE, BENCH_KEY)
                .await
                .ok()
                .flatten()
                .and_then(|b| serde_json::from_slice(&b).ok())
                .unwrap_or_default();
            let mut out = Vec::new();
            for p in list {
                let mut item = serde_json::Map::new();
                item.insert("id".into(), json!(p.id));
                item.insert("name".into(), json!(p.name));
                item.insert("active".into(), json!(p.active));
                let content = read_preset(&home, &p.id).unwrap_or_default();
                let cfg = parse_env(&content);
                if !content.is_empty() {
                    if serve::is_external(&cfg) {
                        item.insert("external".into(), json!(true));
                        let m = trim(get(&cfg, "EXTERNAL_MODEL"));
                        if !m.is_empty() {
                            item.insert("model".into(), json!(m));
                        }
                        if trim(get(&cfg, "EXTERNAL_VISION")) == "1" {
                            item.insert("vision".into(), json!(true));
                        }
                        out.push(Value::Object(item));
                        continue;
                    }
                    let c = preset_ctx(&content);
                    if !c.is_empty() {
                        item.insert("ctx".into(), json!(c));
                    }
                    if !trim(get(&cfg, "MMPROJ")).is_empty() {
                        item.insert("vision".into(), json!(true));
                    }
                    if serve::is_strata(&cfg) {
                        item.insert("strata".into(), json!(true));
                        if get(&cfg, "STRATA_VISION") != "0" {
                            item.insert("vision".into(), json!(true));
                        }
                    }
                    let qn = detect_quant(&content);
                    if !qn.is_empty() {
                        item.insert("quant".into(), json!(qn));
                    }
                    let r = preset_reasoning(&content);
                    if crate::chat::wire::reasoning_active(&r) {
                        item.insert("reasoning".into(), json!(r.to_lowercase()));
                    }
                }
                if let Some(sb) = benches.get(&p.id)
                    && bench_matches_preset(sb, &cfg, &lv.cfg)
                {
                    item.insert("bench".into(), bench_badge(sb));
                }
                out.push(Value::Object(item));
            }
            send_json(StatusCode::OK, &Value::Array(out))
        }
        "/api/presets/order" => {
            if parts.method != Method::POST {
                return fail(StatusCode::METHOD_NOT_ALLOWED, "POST attendu");
            }
            let b = read_body(body, 8 << 20).await.unwrap_or_default();
            let d = gojson::decode_struct(&b, ORDER_REQ_TYPE, &[("ids", Kind::StrList)]);
            if let Some(e) = d.err {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let ids = d.str_list("ids");
            if let Err(e) = store
                .put(
                    BK_STATE,
                    PRESET_ORDER_KEY,
                    gojson::marshal(&ids).into_bytes(),
                )
                .await
            {
                return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
            }
            send_json(StatusCode::OK, &json!({ "ok": true }))
        }
        "/api/preset" => {
            let id = q("id");
            if id.is_empty() {
                let cur = store.config().await;
                return send_json(
                    StatusCode::OK,
                    &json!({ "id": "", "name": "", "content": format_env(&new_preset_seed(&home, &cur)) }),
                );
            }
            match read_preset(&home, &id) {
                Ok(content) => {
                    let sp = store
                        .get_str(BK_STATE, &format!("{SYSPROMPT_PREFIX}{id}"))
                        .await;
                    send_json(
                        StatusCode::OK,
                        &json!({ "id": id, "name": preset_display_name(&content, &id), "content": content, "sysprompt": sp }),
                    )
                }
                Err(_) => send_json(StatusCode::NOT_FOUND, &json!({ "error": "not found" })),
            }
        }
        "/api/preset/save" => {
            if let Some(r) = tune_deny(store).await {
                return r;
            }
            let b = read_body(body, 8 << 20).await.unwrap_or_default();
            let d = gojson::decode_struct(&b, "presetSaveReq", PRESET_SAVE_REQ);
            if let Some(e) = d.err.clone() {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let (new_id, applied) =
                match save_applying(ctx, &d.str("id"), &d.str("name"), &d.str("content"), "save")
                    .await
                {
                    Ok(x) => x,
                    Err(e) => return fail(StatusCode::BAD_REQUEST, e),
                };
            if let Some(sp) = d.opt_str("sysprompt") {
                let key = format!("{SYSPROMPT_PREFIX}{new_id}");
                let res = if trim(&sp).is_empty() {
                    store.del(BK_STATE, &key).await
                } else {
                    store
                        .put(BK_STATE, &key, trim(&sp).as_bytes().to_vec())
                        .await
                };
                if let Err(e) = res {
                    return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
                }
            }
            send_json(
                StatusCode::OK,
                &json!({ "ok": true, "id": new_id, "name": d.str("name"), "applied": applied }),
            )
        }
        "/api/preset/cacheram" => {
            let b = match read_body(body, 1 << 20).await {
                Ok(b) => b,
                Err(e) => return fail(StatusCode::BAD_REQUEST, e),
            };
            let d = gojson::decode_struct(&b, CONTENT_REQ_TYPE, &[("content", Kind::Str)]);
            if let Some(e) = d.err.clone() {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let r = resolver(ctx).await;
            let content = d.str("content");
            let res = tokio::task::spawn_blocking(move || {
                (
                    cache_ram_preview(&r, &content),
                    side_slot_preview(&r, &content),
                )
            })
            .await;
            let Ok(((mib, why), (on, extra, swhy))) = res else {
                return fail(StatusCode::INTERNAL_SERVER_ERROR, "aperçu interrompu");
            };
            let mut out = serde_json::Map::new();
            out.insert("ok".into(), json!(true));
            out.insert("mib".into(), json!(mib));
            out.insert("default".into(), json!(serve::ENGINE_CACHE_RAM_DEFAULT));
            out.insert("why".into(), json!(why));
            if on || !swhy.is_empty() {
                out.insert(
                    "side".into(),
                    json!({ "on": on, "vramMiB": extra, "why": swhy }),
                );
            }
            send_json(StatusCode::OK, &Value::Object(out))
        }
        "/api/preset/moe" => {
            let b = match read_body(body, 1 << 20).await {
                Ok(b) => b,
                Err(e) => return fail(StatusCode::BAD_REQUEST, e),
            };
            let d = gojson::decode_struct(&b, CONTENT_REQ_TYPE, &[("content", Kind::Str)]);
            if let Some(e) = d.err.clone() {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let r = resolver(ctx).await;
            let live_bin = get(&store.config().await, "BIN").to_string();
            let content = d.str("content");
            let c2 = content.clone();
            let Ok((notes, load)) =
                tokio::task::spawn_blocking(move || moe_preview(&r, &live_bin, &c2)).await
            else {
                return fail(StatusCode::INTERNAL_SERVER_ERROR, "aperçu interrompu");
            };
            let mut out = serde_json::Map::new();
            out.insert("ok".into(), json!(true));
            out.insert(
                "notes".into(),
                if notes.is_empty() {
                    Value::Null
                } else {
                    json!(notes)
                },
            );
            out.insert("load".into(), json!(load));
            let cfg = parse_env(&content);
            if !serve::is_external(&cfg)
                && !serve::cpu_experts(
                    &crate::models::split_args(get(&cfg, "EXTRA_ARGS")),
                    &HashMap::new(),
                )
                .is_empty()
            {
                out.insert(
                    "autoplace".into(),
                    match auto_placement_preset(&content) {
                        Ok((_, changes)) => json!({ "ok": true, "changes": changes }),
                        Err(e) => json!({ "ok": false, "why": e }),
                    },
                );
            }
            send_json(StatusCode::OK, &Value::Object(out))
        }
        "/api/preset/autoplace" => {
            if parts.method != Method::POST {
                let mut r = fail(
                    StatusCode::METHOD_NOT_ALLOWED,
                    format!("méthode {} refusée : POST attendu", parts.method),
                );
                r.headers_mut().insert(
                    hyper::header::ALLOW,
                    hyper::header::HeaderValue::from_static("POST"),
                );
                return r;
            }
            let b = match read_body(body, 1 << 20).await {
                Ok(b) => b,
                Err(e) => return fail(StatusCode::BAD_REQUEST, e),
            };
            let d = gojson::decode_struct(
                &b,
                AUTOPLACE_REQ_TYPE,
                &[
                    ("id", Kind::Str),
                    ("name", Kind::Str),
                    ("content", Kind::Str),
                ],
            );
            if let Some(e) = d.err.clone() {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let content = d.str("content");
            let (out, changes) = match auto_placement_preset(&content) {
                Ok(x) => x,
                Err(e) => return send_json(StatusCode::OK, &json!({ "ok": false, "error": e })),
            };
            let shown = preset_display_name(&content, &d.str("id"));
            let name = format!(
                "{} (placement auto)",
                serve_first(&[trim(&d.str("name")), &shown, "preset"])
            );
            match save_preset(&home, "", &name, &out) {
                Ok(id) => send_json(
                    StatusCode::OK,
                    &json!({ "ok": true, "id": id, "name": name, "changes": changes }),
                ),
                Err(e) => send_json(StatusCode::OK, &json!({ "ok": false, "error": e })),
            }
        }
        "/api/preset/delete" => {
            let b = read_body(body, 8 << 20).await.unwrap_or_default();
            let d = gojson::decode_struct(&b, "presetSaveReq", PRESET_SAVE_REQ);
            if let Some(e) = d.err.clone() {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let id = d.str("id");
            let mut model = String::new();
            if d.bool("deleteModel")
                && let Ok(c) = read_preset(&home, &id)
            {
                model = model_from_preset_content(&c);
            }
            if let Err(e) = delete_preset(ctx, &id).await {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let (mut model_deleted, mut model_err, mut freed) =
                (String::new(), String::new(), 0i64);
            if d.bool("deleteModel") && !model.is_empty() {
                let dirs = crate::models::model_dirs(ctx).await;
                let lv = live(store).await;
                let m2 = model.clone();
                let res = tokio::task::spawn_blocking(move || {
                    let refs = crate::models::build_refs(&dirs, &lv.cfg, &lv.order);
                    let (others, _) = refs.users(&dirs, &m2);
                    (others, crate::models::delete_model_file(&dirs, &refs, &m2))
                })
                .await;
                if let Ok((others, r)) = res {
                    match r {
                        Err(e) => model_err = e,
                        Ok(n) => {
                            model_deleted = model;
                            freed = n;
                            if !others.is_empty() {
                                model_err = format!(
                                    "supprimé, mais il était aussi référencé par : {}",
                                    others.join(", ")
                                );
                            }
                        }
                    }
                }
            }
            send_json(
                StatusCode::OK,
                &json!({ "ok": true, "modelDeleted": model_deleted, "modelError": model_err, "freed": freed }),
            )
        }
        "/api/preset/external" => {
            let id = q("id");
            if id.is_empty() {
                return send_json(
                    StatusCode::OK,
                    &json!({ "id": "", "name": "", "url": "", "model": "", "hasKey": false }),
                );
            }
            let Ok(content) = read_preset(&home, &id) else {
                return send_json(StatusCode::NOT_FOUND, &json!({ "error": "not found" }));
            };
            let cfg = parse_env(&content);
            if !serve::is_external(&cfg) {
                return send_json(
                    StatusCode::BAD_REQUEST,
                    &json!({ "error": "pas un preset externe" }),
                );
            }
            send_json(
                StatusCode::OK,
                &json!({
                    "id": id,
                    "name": preset_display_name(&content, &id),
                    "url": trim(get(&cfg, "EXTERNAL_URL")),
                    "model": trim(get(&cfg, "EXTERNAL_MODEL")),
                    "ctx": trim(get(&cfg, "CTX")),
                    "hasKey": !trim(get(&cfg, "EXTERNAL_KEY")).is_empty(),
                    "vision": trim(get(&cfg, "EXTERNAL_VISION")) == "1",
                }),
            )
        }
        "/api/preset/external/save" => {
            let b = read_body(body, 8 << 20).await.unwrap_or_default();
            let d = gojson::decode_struct(&b, "externalSaveReq", EXTERNAL_SAVE_REQ);
            if let Some(e) = d.err.clone() {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let name = trim(&d.str("name")).to_string();
            let url = trim(&d.str("url")).to_string();
            let model = trim(&d.str("model")).to_string();
            if name.is_empty() {
                return fail(StatusCode::BAD_REQUEST, "nom requis");
            }
            if url.is_empty() {
                return fail(StatusCode::BAD_REQUEST, "URL requise");
            }
            if model.is_empty() {
                return fail(StatusCode::BAD_REQUEST, "modèle requis");
            }
            let id = d.str("id");
            let mut key = d.str("key");
            if !id.is_empty()
                && !d.bool("keyTouched")
                && let Ok(c) = read_preset(&home, &id)
            {
                key = trim(get(&parse_env(&c), "EXTERNAL_KEY")).to_string();
            }
            let content =
                external_preset_content(&url, &model, &key, &d.str("ctx"), d.bool("vision"));
            match save_applying(ctx, &id, &name, &content, "apply").await {
                Ok((new_id, applied)) => send_json(
                    StatusCode::OK,
                    &json!({ "ok": true, "id": new_id, "name": name, "applied": applied }),
                ),
                Err(e) => fail(StatusCode::BAD_REQUEST, e),
            }
        }
        "/api/preset/external/test" => {
            let b = read_body(body, 8 << 20).await.unwrap_or_default();
            let d = gojson::decode_struct(&b, "externalSaveReq", EXTERNAL_SAVE_REQ);
            if let Some(e) = d.err.clone() {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let url = completions_url(&d.str("url"));
            let model = trim(&d.str("model")).to_string();
            if url.is_empty() || model.is_empty() {
                return send_json(
                    StatusCode::OK,
                    &json!({ "ok": false, "error": "URL et modèle requis" }),
                );
            }
            let mut key = trim(&d.str("key")).to_string();
            let id = d.str("id");
            if key.is_empty()
                && !id.is_empty()
                && !d.bool("keyTouched")
                && let Ok(c) = read_preset(&home, &id)
            {
                key = trim(get(&parse_env(&c), "EXTERNAL_KEY")).to_string();
            }
            send_json(StatusCode::OK, &external_test(&url, &model, &key).await)
        }
        "/api/switch" => {
            if let Some(r) = tune_deny(store).await {
                return r;
            }
            let b = read_body(body, 8 << 20).await.unwrap_or_default();
            let d =
                gojson::decode_struct(&b, SWITCH_REQ_TYPE, &[("n", Kind::Int), ("id", Kind::Str)]);
            if let Some(e) = d.err.clone() {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let lv = live(store).await;
            let list = match list_presets(&home, &lv.cfg, &lv.order) {
                Ok(l) => l,
                Err(e) => return fail(StatusCode::INTERNAL_SERVER_ERROR, e),
            };
            let id = trim(&d.str("id")).to_string();
            let target = if !id.is_empty() {
                match list.into_iter().find(|p| p.id == id) {
                    Some(p) => p,
                    None => {
                        return fail(
                            StatusCode::NOT_FOUND,
                            "preset introuvable (supprimé ou renommé ?)",
                        );
                    }
                }
            } else {
                let n = d.int("n");
                if n < 1 || n as usize > list.len() {
                    return fail(StatusCode::BAD_REQUEST, "index hors limites");
                }
                list.into_iter().nth(n as usize - 1).expect("borné")
            };
            if let Err(e) = preset_apply(store, &target.path, "switch").await {
                return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
            }
            send_json(
                StatusCode::OK,
                &json!({ "ok": true, "preset": target.name }),
            )
        }
        _ => fail(StatusCode::NOT_FOUND, "route inconnue"),
    }
}

/// `DeletePreset` : refusé pour le preset actif ; emporte son prompt système
/// et sa mesure.
async fn delete_preset(ctx: &Ctx, id: &str) -> Result<(), String> {
    let home = &ctx.engine.home;
    let p = safe_preset_path(home, id)?;
    let target = std::fs::read(&p).map_err(|_| "introuvable".to_string())?;
    if preset_fingerprint(&String::from_utf8_lossy(&target))
        == config_fingerprint(&ctx.store.config().await)
    {
        return Err("preset actif, switche d'abord".into());
    }
    std::fs::remove_file(&p)
        .map_err(|e| crate::paths::os_err("remove", &p.to_string_lossy(), &e))?;
    let _ = ctx
        .store
        .del(BK_STATE, &format!("{SYSPROMPT_PREFIX}{id}"))
        .await;
    if let Ok(Some(b)) = ctx.store.get(BK_STATE, BENCH_KEY).await
        && let Ok(mut m) = serde_json::from_slice::<BTreeMap<String, Value>>(&b)
        && m.remove(id).is_some()
    {
        let _ = ctx
            .store
            .put(BK_STATE, BENCH_KEY, gojson::marshal(&m).into_bytes())
            .await;
    }
    Ok(())
}

/// `handlePresetExternalTest` : un appel de complétion minimal.
pub async fn external_test(url: &str, model: &str, key: &str) -> Value {
    crate::chat::engine::install_crypto();
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(20))
        .build()
        .unwrap_or_default();
    let payload = json!({
        "max_tokens": 1,
        "messages": [{ "content": "ping", "role": "user" }],
        "model": model,
        "stream": false,
    });
    let mut rq = client
        .post(url)
        .header("Content-Type", "application/json")
        .body(serde_json::to_vec(&payload).unwrap_or_default());
    if !key.is_empty() {
        rq = rq.header("Authorization", format!("Bearer {key}"));
    }
    let resp = match rq.send().await {
        Ok(r) => r,
        Err(e) => return json!({ "ok": false, "error": format!("injoignable : {e}") }),
    };
    let status = resp.status();
    let mut body = Vec::new();
    let mut stream = resp;
    while body.len() < 2000 {
        match stream.chunk().await {
            Ok(Some(c)) => body.extend_from_slice(&c),
            _ => break,
        }
    }
    body.truncate(2000);
    if status != reqwest::StatusCode::OK {
        let mut msg = trim(&String::from_utf8_lossy(&body)).to_string();
        let m = extract_api_error(&body);
        if !m.is_empty() {
            msg = m;
        }
        if msg.is_empty() {
            msg = format!(
                "{} {}",
                status.as_u16(),
                status.canonical_reason().unwrap_or("")
            );
        }
        return json!({ "ok": false, "status": status.as_u16(), "error": msg });
    }
    json!({ "ok": true })
}
