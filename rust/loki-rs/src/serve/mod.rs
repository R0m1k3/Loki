//! `loki serve` — port de `backend_serve*.go` (lot 14 de la migration) :
//! composer la ligne de commande de llama-server à partir du preset, puis
//! céder la place au moteur (`exec`).
//!
//! Tout ce qui décide est PUR et vit ici ([`build_serve_args`] et ses
//! modules) : une table de lancements tirée du Go
//! (`internal/loki/parity_serve_test.go`) le vérifie argument par argument.
//! [`run`] fait les lectures du monde (aide du moteur, GGUF, RAM, VRAM,
//! cgroup, variables) avant d'appeler la partie pure, comme `cmdServe`.
//!
//! L'ordre des arguments fait partie du comportement : llama-server retient
//! la DERNIÈRE occurrence d'un drapeau.

use std::collections::{BTreeMap, HashMap};

use crate::gguf::GgufMeta;

pub mod cache;
pub mod ckpt;
pub mod expert;
pub mod fidelity;
pub mod moe;
pub mod persist;
pub mod queues;
pub mod run;
pub mod side;
pub mod spec;
pub mod split;
pub mod threads;

pub use cache::*;
pub use ckpt::*;
pub use expert::*;
pub use fidelity::*;
pub use moe::*;
pub use persist::*;
pub use queues::*;
pub use side::*;
pub use spec::*;
pub use split::*;
pub use threads::*;

/// Configuration d'un preset (`ReadConfig()`).
pub type Cfg = HashMap<String, String>;

/// Ce que `cmdServe` a dû demander au monde avant de composer la ligne de
/// commande (`serveSysInfo`). Zéro / vide = inconnu.
#[derive(Clone, Debug, Default)]
pub struct SysInfo {
    /// Sortie de `<bin> --help` ; vide = moteur inconnu, aucun risque pris.
    pub help: String,
    /// Chemin du .gguf principal, résolu et vérifié.
    pub model: String,
    /// Projecteur vision résolu ; vide = pas de vision.
    pub mmproj: String,
    /// Clé d'API effective ; vide = serveur ouvert.
    pub api_key: String,
    /// Sonde de conteneur (Linux).
    pub cpu: CpuBudget,
    /// `CUDA_SCALE_LAUNCH_QUEUES` déjà posée : intouchée.
    pub launch_queues: String,
    /// GPU visibles ; 0 = inconnu.
    pub gpus: i64,
    /// `LLAMA_ARG_*` de l'environnement qui décident de quelque chose.
    pub arg_env: HashMap<String, String>,
    /// `GGML_*` déjà posées par l'utilisateur.
    pub user_env: HashMap<String, String>,
    pub gguf: Option<GgufMeta>,
    pub model_bytes: i64,
    pub ram_mib: i64,
    pub ram_avail_mib: i64,
    pub vram_mib: i64,
    pub unified_mem: bool,
    pub engine_vram_mib: i64,
    pub slot_dir: String,
    pub engine_build: i64,
    pub draft: String,
    pub draft_err: String,
    pub draft_gguf: Option<GgufMeta>,
    pub draft_is_model: bool,
    pub spec_auto_blocked: String,
    pub split_devs: Vec<SplitDev>,
}

impl SysInfo {
    pub(crate) fn env(&self, k: &str) -> &str {
        self.arg_env.get(k).map_or("", String::as_str)
    }
    pub(crate) fn uenv(&self, k: &str) -> &str {
        self.user_env.get(k).map_or("", String::as_str)
    }
    pub(crate) fn blocks(&self) -> i64 {
        self.gguf.as_ref().map_or(0, |g| g.block_count)
    }
}

/// Une carte telle que la voit le moteur (`--list-devices`), dans son ordre.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct SplitDev {
    pub id: String,
    pub total_mib: i64,
}

// ---------------------------------------------------------------------------
// Petits outils au comportement du Go
// ---------------------------------------------------------------------------

/// `strings.TrimSpace`.
pub(crate) fn trim(s: &str) -> &str {
    s.trim_matches(char::is_whitespace)
}

/// `strconv.Atoi`.
pub(crate) fn atoi(s: &str) -> Option<i64> {
    s.parse::<i64>().ok()
}

/// `cfg[k]` (vide si absent).
pub(crate) fn get<'a>(cfg: &'a Cfg, k: &str) -> &'a str {
    cfg.get(k).map_or("", String::as_str)
}

/// `hasAnyFlag` : l'un de ces drapeaux est-il écrit (forme `--x=v` comprise) ?
pub fn has_any_flag(args: &[String], flags: &[&str]) -> bool {
    args.iter().any(|a| {
        let name = match a.find('=') {
            Some(i) if i > 0 => &a[..i],
            _ => a.as_str(),
        };
        flags.contains(&name)
    })
}

/// `flagValue` : valeur de la DERNIÈRE occurrence (`-t 6` ou `-t=6`).
pub fn flag_value(args: &[String], flags: &[&str]) -> String {
    let mut v = String::new();
    for (i, a) in args.iter().enumerate() {
        let (name, val) = match a.split_once('=') {
            Some((n, v)) => (n, Some(v)),
            None => (a.as_str(), None),
        };
        if !flags.contains(&name) {
            continue;
        }
        match val {
            Some(x) => v = x.to_string(),
            None => {
                if let Some(n) = args.get(i + 1) {
                    v = n.clone();
                }
            }
        }
    }
    v
}

/// `flagValues` : les valeurs de TOUTES les occurrences.
pub fn flag_values(args: &[String], flags: &[&str]) -> Vec<String> {
    let mut vals = Vec::new();
    for (i, a) in args.iter().enumerate() {
        let (name, val) = match a.split_once('=') {
            Some((n, v)) => (n, Some(v)),
            None => (a.as_str(), None),
        };
        if !flags.contains(&name) {
            continue;
        }
        match val {
            Some(x) => vals.push(x.to_string()),
            None => {
                if let Some(n) = args.get(i + 1) {
                    vals.push(n.clone());
                }
            }
        }
    }
    vals
}

/// `hasFlagPrefix` : le nom du premier drapeau qui commence par l'un des
/// préfixes, ou vide.
pub fn has_flag_prefix(args: &[String], prefixes: &[&str]) -> String {
    for a in args {
        for p in prefixes {
            if a.starts_with(p) {
                return a.split_once('=').map_or(a.as_str(), |(n, _)| n).to_string();
            }
        }
    }
    String::new()
}

/// `envTruthy` : on, enabled, true, 1.
pub fn env_truthy(v: &str) -> bool {
    matches!(trim(v), "on" | "enabled" | "true" | "1")
}

/// `argValue` : valeur de la dernière occurrence de `flag` (forme séparée).
pub fn arg_value(args: &[String], flag: &str) -> String {
    let mut v = String::new();
    for i in 0..args.len().saturating_sub(1) {
        if args[i] == flag {
            v = args[i + 1].clone();
        }
    }
    v
}

/// `firstNonEmpty`.
pub(crate) fn first_non_empty<'a>(vals: &[&'a str]) -> &'a str {
    vals.iter()
        .copied()
        .find(|v| !trim(v).is_empty())
        .unwrap_or("")
}

/// `isExternalConfig`.
pub fn is_external(cfg: &Cfg) -> bool {
    trim(get(cfg, "EXTERNAL")) == "1"
}

/// `isStrataConfig` (`engineOf`).
pub fn is_strata(cfg: &Cfg) -> bool {
    matches!(
        trim(get(cfg, "ENGINE")).to_lowercase().as_str(),
        "strata" | "moe"
    )
}

pub(crate) fn s(v: &[&str]) -> Vec<String> {
    v.iter().map(|x| x.to_string()).collect()
}

// ---------------------------------------------------------------------------
// Aide du moteur
// ---------------------------------------------------------------------------

/// `helpSupportsReasoningFlag`.
pub fn help_supports_reasoning_flag(help: &str) -> bool {
    help.contains("--reasoning ")
}

/// `helpSupportsLoadMode`.
pub fn help_supports_load_mode(help: &str) -> bool {
    help.contains("--load-mode")
}

/// `helpSupportsNGLAuto` : « 'auto' » dans les 400 octets qui suivent le
/// drapeau des couches GPU.
pub fn help_supports_ngl_auto(h: &str) -> bool {
    let i = match h.find("--n-gpu-layers").or_else(|| h.find("-ngl")) {
        Some(i) => i,
        None => return false,
    };
    let end = (i + 400).min(h.len());
    // Tranche d'octets (le Go découpe sans égard aux runes).
    String::from_utf8_lossy(&h.as_bytes()[i..end]).contains("'auto'")
}

/// `helpFitsLayersItself`.
pub fn help_fits_layers_itself(help: &str) -> bool {
    help_supports_ngl_auto(help) || help_supports_load_mode(help)
}

const NGL_999_NOTE: &str = "NGL=999 (sentinelle « toutes les couches ») → -ngl auto : ce moteur mesure lui-même la VRAM libre. NGL=all pour forcer l'ancien comportement.";

/// `nglArgs`.
pub fn ngl_args(ngl: &str, fits_itself: bool) -> (Vec<String>, String) {
    let ngl = trim(ngl);
    if ngl.eq_ignore_ascii_case("auto") {
        (Vec::new(), String::new())
    } else if ngl == "999" && fits_itself {
        (s(&["-ngl", "auto"]), NGL_999_NOTE.into())
    } else if !ngl.is_empty() {
        (s(&["-ngl", ngl]), String::new())
    } else if fits_itself {
        (s(&["-ngl", "auto"]), String::new())
    } else {
        (s(&["-ngl", "999"]), String::new())
    }
}

/// `normalizeLoadFlags`.
pub fn normalize_load_flags(args: &[String], supports_load_mode: bool) -> (Vec<String>, String) {
    if !supports_load_mode {
        return downgrade_load_mode(args);
    }
    let explicit = has_any_flag(args, &["--load-mode", "-lm"]);
    let (mut mlock, mut nommap, mut mmap) = (false, false, false);
    let mut out = Vec::with_capacity(args.len() + 2);
    for a in args {
        match a.as_str() {
            "--mlock" => mlock = true,
            "--no-mmap" => nommap = true,
            "--mmap" => mmap = true,
            _ => out.push(a.clone()),
        }
    }
    if explicit || !(mlock || nommap || mmap) {
        return (out, String::new());
    }
    let mode = if mlock && nommap {
        "mlock"
    } else if mlock {
        "mmap+mlock"
    } else if nommap {
        "none"
    } else {
        "mmap"
    };
    out.push("--load-mode".into());
    out.push(mode.into());
    (out, String::new())
}

/// `downgradeLoadMode`.
pub fn downgrade_load_mode(args: &[String]) -> (Vec<String>, String) {
    let (mut mode, mut found) = (String::new(), false);
    let mut kept: Vec<String> = Vec::with_capacity(args.len());
    let mut i = 0;
    while i < args.len() {
        let a = &args[i];
        if a == "--load-mode" || a == "-lm" {
            found = true;
            if i + 1 < args.len() {
                mode = args[i + 1].to_lowercase();
                i += 1;
            }
            i += 1;
            continue;
        }
        if let Some(v) = a.strip_prefix("--load-mode=") {
            found = true;
            mode = v.to_lowercase();
            i += 1;
            continue;
        }
        kept.push(a.clone());
        i += 1;
    }
    if !found {
        return (args.to_vec(), String::new());
    }
    let add = |kept: &mut Vec<String>, flag: &str| {
        if !kept.iter().any(|a| a == flag) {
            kept.push(flag.into());
        }
    };
    match mode.as_str() {
        "none" => add(&mut kept, "--no-mmap"),
        "mlock" => {
            add(&mut kept, "--mlock");
            add(&mut kept, "--no-mmap");
        }
        "mmap+mlock" => add(&mut kept, "--mlock"),
        "dio" => {
            return (
                kept,
                "ce moteur ne connaît pas --load-mode dio (DirectIO) : chargement par défaut"
                    .into(),
            );
        }
        _ => {}
    }
    (kept, String::new())
}

/// `cudaDeviceEnv`.
pub fn cuda_device_env(cfg: &Cfg) -> BTreeMap<String, String> {
    let mut env = BTreeMap::new();
    let v = get(cfg, "CUDA_VISIBLE_DEVICES");
    if !v.is_empty() {
        env.insert("CUDA_VISIBLE_DEVICES".into(), v.into());
        env.insert("CUDA_DEVICE_ORDER".into(), "PCI_BUS_ID".into());
    }
    env
}

/// `serveKVTypes` : KV_TYPE pour les deux, KV_TYPE_K / KV_TYPE_V à part.
pub fn serve_kv_types(cfg: &Cfg) -> (String, String) {
    let kv = get(cfg, "KV_TYPE");
    let (mut k, mut v) = (kv.to_string(), kv.to_string());
    let sk = get(cfg, "KV_TYPE_K");
    if !sk.is_empty() {
        k = sk.into();
    }
    let sv = get(cfg, "KV_TYPE_V");
    if !sv.is_empty() {
        v = sv.into();
    }
    (k, v)
}

/// Ligne de commande, environnement et notes d'un lancement.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Launch {
    pub args: Vec<String>,
    pub env: BTreeMap<String, String>,
    pub notes: Vec<String>,
}

/// `buildServeArgs` : la ligne de commande de llama-server (`bin` en tête),
/// l'environnement à poser et les notes. Fonction pure.
pub fn build_serve_args(cfg: &Cfg, extra: &[String], bin: &str, si: &SysInfo) -> Launch {
    let g = |key: &str, fallback: &str| -> String {
        match cfg.get(key) {
            Some(v) if !v.is_empty() => v.clone(),
            _ => fallback.to_string(),
        }
    };
    let (ktv, vtv) = serve_kv_types(cfg);
    let mut env = cuda_device_env(cfg);
    let mut notes: Vec<String> = Vec::new();

    let (extra, load_note) = normalize_load_flags(extra, help_supports_load_mode(&si.help));
    let extra = &extra[..];
    if !load_note.is_empty() {
        notes.push(load_note);
    }

    let st = split_tensor_args(cfg, extra, si);
    let mut place: Vec<String> = extra.to_vec();
    if st.on {
        place.push("-sm".into());
        place.push("tensor".into());
    }
    for (k, v) in &st.env {
        env.insert(k.clone(), v.clone());
    }
    notes.extend(st.notes.iter().cloned());

    let (q, q_notes) = launch_queues_env(cfg, &place, si);
    if !q.is_empty() {
        env.insert("CUDA_SCALE_LAUNCH_QUEUES".into(), q);
    }
    notes.extend(q_notes);
    let (gv, g_notes) = graph_opt_env(cfg, &place, si);
    if !gv.is_empty() {
        env.insert("GGML_CUDA_GRAPH_OPT".into(), gv);
    }
    let (ov, o_notes) = op_offload_min_batch_env(cfg, si);
    if !ov.is_empty() {
        env.insert("GGML_OP_OFFLOAD_MIN_BATCH".into(), ov);
    }
    notes.extend(g_notes);
    notes.extend(o_notes);

    let (threads, thread_notes) = thread_args(
        get(cfg, "THREADS"),
        get(cfg, "THREADS_BATCH"),
        extra,
        &si.cpu,
    );
    notes.extend(thread_notes);
    let (side_ctx, side_mib, side_why) = side_slot_plan(cfg, extra, si);
    let mut ctx_arg = g("CTX", "32768");
    if side_ctx > 0 {
        ctx_arg = (2 * side_ctx).to_string();
    }
    let mut args: Vec<String> = vec![
        bin.to_string(),
        "-m".into(),
        si.model.clone(),
        "-c".into(),
        ctx_arg,
    ];
    args.extend(threads);
    args.extend([
        "-b".into(),
        g("BATCH", "2048"),
        "-ub".into(),
        g("UBATCH", "512"),
        "--host".into(),
        g("HOST", "0.0.0.0"),
        "--port".into(),
        g("PORT", "8080"),
    ]);
    if side_ctx > 0 {
        args.extend(s(&["--parallel", "2", "--no-kv-unified"]));
    } else if !has_any_flag(extra, &["--parallel", "-np"]) {
        args.push("--parallel".into());
        args.push(g("PARALLEL", "1"));
    }
    let n = side_slot_note(side_ctx, side_mib, &side_why, ngl_forced(cfg, extra));
    if !n.is_empty() {
        notes.push(n);
    }
    if st.on {
        args.extend(st.ngl.iter().cloned());
    } else if !has_any_flag(extra, &["-ngl", "--n-gpu-layers", "--gpu-layers"]) {
        let (ngl, note) = ngl_args(&g("NGL", ""), help_fits_layers_itself(&si.help));
        if !note.is_empty() {
            notes.push(note);
        }
        args.extend(ngl);
    }
    let (fitt, fit_notes) = fit_target_args(cfg, &place, si);
    args.extend(fitt);
    args.extend(st.args.iter().cloned());
    notes.extend(fit_notes);
    if !ktv.is_empty() {
        args.push("-ctk".into());
        args.push(ktv);
    }
    if !vtv.is_empty() {
        args.push("-ctv".into());
        args.push(vtv);
    }
    if !si.mmproj.is_empty() {
        args.push("--mmproj".into());
        args.push(si.mmproj.clone());
    }
    let r = trim(get(cfg, "REASONING"));
    if !r.is_empty() {
        if crate::chat::wire::reasoning_active(r) {
            args.extend([
                "--reasoning".into(),
                r.to_string(),
                "--reasoning-budget".into(),
                g("REASONING_BUDGET", "-1"),
            ]);
        } else if help_supports_reasoning_flag(&si.help) {
            args.extend(s(&["--reasoning", "off"]));
        } else {
            notes.push(
                "ce moteur ne connaît pas --reasoning : impossible de désactiver le raisonnement"
                    .into(),
            );
        }
    }
    let (rp, rp_notes) = reasoning_preserve_args(cfg, extra, si);
    args.extend(rp);
    notes.extend(rp_notes);
    if !si.api_key.is_empty() {
        args.push("--api-key".into());
        args.push(si.api_key.clone());
    }
    let (cram, cram_notes) = cache_ram_args(cfg, extra, si);
    args.extend(cram);
    args.extend(slot_save_args(cfg, extra, si));
    notes.extend(cram_notes);
    let (ok, why) = slot_persist_plan(cfg, extra, si);
    let n = slot_persist_note(ok, &why);
    if !n.is_empty() {
        notes.push(n);
    }
    let (ck, ck_notes) = ckpt_args(cfg, extra, si);
    args.extend(ck);
    notes.extend(ck_notes);
    let sp = spec_args(cfg, extra, si);
    args.extend(sp.args);
    notes.extend(sp.notes);
    args.extend(extra.iter().cloned());
    let (ek, ev, src) = effective_kv_types(cfg, extra, &si.arg_env);
    let n = kv_fidelity_note(&ek, &ev, &src, extra, &si.arg_env);
    if !n.is_empty() {
        notes.push(n);
    }
    notes.extend(lossy_cache_notes(extra, si));
    notes.extend(moe_notes(cfg, extra, si));
    let (w, _) = load_mode_risk(cfg, &args, si);
    if !w.is_empty() {
        notes.push(w);
    }
    Launch { args, env, notes }
}

/// `fastKV` / `slowKV` : combinaisons accélérées en Flash-Attention CUDA.
pub fn slow_kv(k: &str, v: &str) -> bool {
    let k = if k.is_empty() { "f16" } else { k };
    let v = if v.is_empty() { "f16" } else { v };
    !matches!(
        (k, v),
        ("f16", "f16") | ("q8_0", "q8_0") | ("q4_0", "q4_0") | ("bf16", "bf16")
    )
}

/// `engineCmdline` : forme copiable (variables triées en tête, guillemets
/// autour de ce qui contient un blanc ou un guillemet, clé masquée).
pub fn engine_cmdline(env: &BTreeMap<String, String>, args: &[String]) -> String {
    let quote = |a: &str| -> String {
        if a.is_empty() || a.contains([' ', '\t', '"', '\'']) {
            format!("\"{}\"", a.replace('"', "\\\""))
        } else {
            a.to_string()
        }
    };
    let mut out: Vec<String> = env
        .iter()
        .map(|(k, v)| format!("{k}={}", quote(v)))
        .collect();
    for (i, a) in args.iter().enumerate() {
        if i > 0 && (args[i - 1] == "--api-key" || args[i - 1] == "--api-key-file") {
            out.push(quote("<clé masquée>"));
        } else {
            out.push(quote(a));
        }
    }
    out.join(" ")
}
