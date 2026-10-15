//! Experts MoE et chargement du modèle (`backend_serve_moe.go`) : des avis et
//! un garde-fou (LOAD_GUARD), jamais de réécriture silencieuse du preset.

use super::{
    Cfg, FIT_TARGET_MIN, SplitDev, SysInfo, atoi, env_truthy, first_non_empty, fit_blocker,
    flag_value, flag_values, get, has_any_flag, help_supports_load_mode, is_external, trim,
};
use std::collections::HashMap;

/// `otPatterns`.
pub fn ot_patterns(extra: &[String], arg_env: &HashMap<String, String>) -> Vec<String> {
    let mut vals = flag_values(extra, &["-ot", "--override-tensor"]);
    if let Some(v) = arg_env
        .get("LLAMA_ARG_OVERRIDE_TENSOR")
        .filter(|v| !v.is_empty())
    {
        vals.push(v.clone());
    }
    let mut out = Vec::new();
    for v in vals {
        for el in v.split(',') {
            let el = trim(el);
            if !el.is_empty() {
                out.push(el.to_string());
            }
        }
    }
    out
}

/// `otTargetsExperts`.
pub fn ot_targets_experts(pattern: &str) -> bool {
    pattern.to_lowercase().contains("exps")
}

/// `otOnCPU`.
pub fn ot_on_cpu(el: &str) -> bool {
    el.split_once('=')
        .is_some_and(|(_, b)| trim(b).to_uppercase().starts_with("CPU"))
}

/// `cpuExperts`.
pub fn cpu_experts(extra: &[String], arg_env: &HashMap<String, String>) -> String {
    for el in ot_patterns(extra, arg_env) {
        let pat = el.split_once('=').map_or(el.as_str(), |(p, _)| p);
        if ot_targets_experts(pat) && ot_on_cpu(&el) {
            return "-ot experts=CPU".into();
        }
    }
    let env = |k: &str| arg_env.get(k).map_or("", String::as_str);
    if has_any_flag(extra, &["--cpu-moe", "-cmoe"]) || env_truthy(env("LLAMA_ARG_CPU_MOE")) {
        return "--cpu-moe".into();
    }
    let mut vals = flag_values(extra, &["-ncmoe", "--n-cpu-moe"]);
    vals.push(env("LLAMA_ARG_N_CPU_MOE").to_string());
    for v in vals {
        let v = trim(&v);
        if !v.is_empty() && v != "0" {
            return format!("--n-cpu-moe {v}");
        }
    }
    String::new()
}

/// `helpSupportsFit` : « --fit » lui-même, pas seulement --fit-target.
pub fn help_supports_fit(help: &str) -> bool {
    let b = help.as_bytes();
    let pat = b"--fit";
    let mut i = 0;
    while let Some(off) = help[i..].find("--fit") {
        let start = i + off;
        let end = start + pat.len();
        let before_ok =
            start == 0 || matches!(b[start - 1], b',') || b[start - 1].is_ascii_whitespace();
        let after_ok =
            end == b.len() || matches!(b[end], b',' | b'=' | b'[') || b[end].is_ascii_whitespace();
        if before_ok && after_ok {
            return true;
        }
        i = start + 1;
    }
    false
}

/// `serveUBatch` (0 = illisible).
pub fn serve_ubatch(cfg: &Cfg, extra: &[String]) -> i64 {
    let mut sv = flag_value(extra, &["-ub", "--ubatch-size"]);
    if sv.is_empty() {
        sv = trim(get(cfg, "UBATCH")).to_string();
    }
    if sv.is_empty() {
        sv = "512".into();
    }
    match atoi(trim(&sv)) {
        Some(n) if n >= 0 => n,
        _ => 0,
    }
}

/// `gibText` : des Mio en « 12,3 Gio ».
pub fn gib_text(mib: i64) -> String {
    format!("{:.1}", mib as f64 / 1024.0).replacen('.', ",", 1) + " Gio"
}

/// `moeNotes`.
pub fn moe_notes(cfg: &Cfg, extra: &[String], si: &SysInfo) -> Vec<String> {
    let Some(g) = si.gguf.as_ref() else {
        return Vec::new();
    };
    if is_external(cfg) || g.expert_count <= 0 {
        return Vec::new();
    }
    let ub = serve_ubatch(cfg, extra);
    let r = cpu_experts(extra, &si.arg_env);
    if !r.is_empty() {
        let mut n = format!("experts placés à la main ({r}) : --fit ne les place pas");
        if ub > 0 && ub <= 512 {
            n.push_str(" ; UBATCH plus grand = tampon de calcul plus gros sur chaque GPU : monte --n-cpu-moe d'abord, ou passe au placement auto (--fit, « Dupliquer en placement auto » dans l'éditeur)");
        }
        return vec![n];
    }
    let model_mib = si.model_bytes >> 20;
    if model_mib <= 0 || si.vram_mib <= 0 || si.unified_mem || trim(&si.help).is_empty() {
        return Vec::new();
    }
    let cards = si.gpus.max(1);
    if model_mib <= si.vram_mib - FIT_TARGET_MIN * cards {
        return Vec::new();
    }
    let sizes = format!(
        "modèle {}, VRAM {}",
        gib_text(model_mib),
        gib_text(si.vram_mib)
    );
    if !help_supports_fit(&si.help) {
        let ngl_s = flag_value(extra, &["-ngl", "--n-gpu-layers", "--gpu-layers"]);
        if let Some(n) = atoi(trim(first_non_empty(&[&ngl_s, get(cfg, "NGL")])))
            && n < 999
        {
            return Vec::new();
        }
        return vec![format!(
            "MoE plus gros que la VRAM ({sizes}) : ce moteur ne place pas les experts (pas de --fit) — garde --n-cpu-moe"
        )];
    }
    let why = fit_blocker(cfg, extra, si);
    if !why.is_empty() {
        if why == "surcharge de tenseurs" {
            return vec![format!(
                "MoE plus gros que la VRAM ({sizes}) et --fit coupé par un -ot qui ne vise pas les experts : rien ne les sort du GPU — --n-cpu-moe, ou retire ce -ot"
            )];
        }
        return Vec::new();
    }
    if ub > 0 && ub <= 512 {
        return vec![format!(
            "UBATCH 2048+ recommandé : --fit laisse des experts en RAM ({sizes}), recopiés vers le GPU à chaque micro-lot du prefill ; BATCH 4096 avec — à mesurer (bench complet)"
        )];
    }
    Vec::new()
}

/// `loadArgEnv`.
pub const LOAD_ARG_ENV: &[&str] = &[
    "LLAMA_ARG_LOAD_MODE",
    "LLAMA_ARG_MLOCK",
    "LLAMA_ARG_NO_MMAP",
    "LLAMA_ARG_RPC",
];

/// `residentLoadMode`.
pub fn resident_load_mode(args: &[String], arg_env: &HashMap<String, String>) -> String {
    let env = |k: &str| arg_env.get(k).map_or("", String::as_str);
    let mut mode = trim(&flag_value(args, &["--load-mode", "-lm"])).to_lowercase();
    let (mut mlock, mut nommap) = (
        has_any_flag(args, &["--mlock"]),
        has_any_flag(args, &["--no-mmap"]),
    );
    if mode.is_empty() && !mlock && !nommap {
        mode = trim(env("LLAMA_ARG_LOAD_MODE")).to_lowercase();
        if mode.is_empty() {
            mlock = env_truthy(env("LLAMA_ARG_MLOCK"));
            nommap = env_truthy(env("LLAMA_ARG_NO_MMAP"));
        }
    }
    if matches!(mode.as_str(), "none" | "mlock" | "dio" | "mmap+mlock") {
        return format!("--load-mode {mode}");
    }
    match (mlock, nommap) {
        (true, true) => "--mlock --no-mmap".into(),
        (true, false) => "--mlock".into(),
        (false, true) => "--no-mmap".into(),
        _ => String::new(),
    }
}

/// `loadGuardMarker`.
pub const LOAD_GUARD_MARKER: &str = "LOAD_GUARD=off pour lancer quand même";
/// `loadGuardExitStatus` (EX_CONFIG).
pub const LOAD_GUARD_EXIT_STATUS: i32 = 78;

/// `loadGuardOff`.
pub fn load_guard_off(cfg: &Cfg) -> bool {
    matches!(
        trim(get(cfg, "LOAD_GUARD")).to_lowercase().as_str(),
        "off" | "0" | "false" | "non" | "no"
    )
}

/// `loadModeRisk` : (avertissement, refus).
pub fn load_mode_risk(cfg: &Cfg, args: &[String], si: &SysInfo) -> (String, String) {
    if is_external(cfg) {
        return (String::new(), String::new());
    }
    let restricted;
    let env = if help_supports_load_mode(&si.help)
        && (!si.env("LLAMA_ARG_MLOCK").is_empty() || !si.env("LLAMA_ARG_NO_MMAP").is_empty())
    {
        let mut m = HashMap::new();
        m.insert(
            "LLAMA_ARG_LOAD_MODE".to_string(),
            si.env("LLAMA_ARG_LOAD_MODE").to_string(),
        );
        restricted = m;
        &restricted
    } else {
        &si.arg_env
    };
    let mode = resident_load_mode(args, env);
    let model_mib = si.model_bytes >> 20;
    if mode.is_empty() || model_mib <= 0 || si.ram_mib <= 0 {
        return (String::new(), String::new());
    }
    let remote = has_any_flag(args, &["--rpc"]) || !trim(si.env("LLAMA_ARG_RPC")).is_empty();
    let (mut low, mut est, mut up_to, mut unsure) = (0i64, model_mib, true, false);
    if remote {
    } else if si.unified_mem {
        low = model_mib;
        est = model_mib;
        up_to = false;
    } else if si.engine_vram_mib > 0 {
        low = (model_mib - si.engine_vram_mib).max(0);
        est = low;
        up_to = false;
    } else if si.vram_mib > 0 {
        est = (model_mib - si.vram_mib).max(0);
        up_to = false;
        unsure = true;
    }
    let ram = si.ram_mib;
    let what = if up_to { "jusqu'à " } else { "environ " };
    if unsure && est * 10 > ram * 9 && !load_guard_off(cfg) {
        return (
            format!(
                "{mode} : environ {} de poids en RAM (modèle {}, VRAM NVIDIA {}) pour {} — échec probable ; refus seulement si les cartes listées par le moteur le confirment, lancement sinon ; --load-mode mmap est plus sûr",
                gib_text(est),
                gib_text(model_mib),
                gib_text(si.vram_mib),
                gib_text(ram)
            ),
            String::new(),
        );
    }
    if low * 10 > ram * 9 {
        let msg = format!(
            "{mode} : au moins {} de poids restent en RAM (modèle {}) pour {} de RAM, limite du conteneur comprise — échec certain : swap et décodage effondré, ou processus tué (llama.cpp #26110). --load-mode mmap laisse le système relire les pages au besoin",
            gib_text(low),
            gib_text(model_mib),
            gib_text(ram)
        );
        if load_guard_off(cfg) {
            return (msg + " ; LOAD_GUARD=off : lancé quand même", String::new());
        }
        return (String::new(), msg + " ; " + LOAD_GUARD_MARKER);
    }
    if est * 10 > ram * 8 {
        return (
            format!(
                "{mode} : {what}{} de poids en RAM pour {} — au-delà, swap (décodage de 25 à 7,5 t/s dans llama.cpp #26110) ou processus tué ; --load-mode mmap est plus sûr",
                gib_text(est),
                gib_text(ram)
            ),
            String::new(),
        );
    }
    (String::new(), String::new())
}

/// `loadGuardNeedsDevices`.
pub fn load_guard_needs_devices(cfg: &Cfg, args: &[String], si: &SysInfo) -> bool {
    if si.engine_vram_mib > 0 || si.vram_mib <= 0 {
        return false;
    }
    let mut s2 = si.clone();
    s2.engine_vram_mib = si.vram_mib;
    !load_mode_risk(cfg, args, &s2).1.is_empty()
}

/// `engineDevsVRAM`.
pub fn engine_devs_vram(devs: &[SplitDev]) -> i64 {
    let mut total = 0;
    for d in devs {
        if d.total_mib <= 0 {
            return 0;
        }
        total += d.total_mib;
    }
    total
}
