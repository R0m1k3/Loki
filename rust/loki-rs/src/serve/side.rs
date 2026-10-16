//! Second slot pour les travaux annexes (`backend_serve_side.go`,
//! `SIDE_SLOT`, off par défaut) : deux flux KV séparés de CTX jetons
//! (`-c 2×CTX --parallel 2 --no-kv-unified`), refusé quand le compte de VRAM
//! ne tient pas ou que l'utilisateur a réglé l'un des leviers.

use super::{
    Cfg, SysInfo, atoi, cpu_weights, effective_kv_types, flag_value, get, gguf_state_bytes,
    has_any_flag, is_external, trim,
};

/// `sideSlotOn`.
pub fn side_slot_on(cfg: &Cfg) -> bool {
    matches!(
        trim(get(cfg, "SIDE_SLOT")).to_lowercase().as_str(),
        "on" | "1" | "true" | "yes" | "oui"
    )
}

/// `sideSlotConflictFlags`.
pub const SIDE_SLOT_CONFLICT_FLAGS: &[&str] = &[
    "-c",
    "--ctx-size",
    "-np",
    "--parallel",
    "-kvu",
    "--kv-unified",
    "-no-kvu",
    "--no-kv-unified",
    "--kv-unified-per-slot",
    "--cache-idle-slots",
    "--no-cache-idle-slots",
];

/// `sideSlotArgEnv`.
pub const SIDE_SLOT_ARG_ENV: &[&str] = &[
    "LLAMA_ARG_N_PARALLEL",
    "LLAMA_ARG_KV_UNIFIED",
    "LLAMA_ARG_KV_UNIFIED_PER_SLOT",
    "LLAMA_ARG_CACHE_IDLE_SLOTS",
    "LLAMA_ARG_CTX_SIZE",
];

/// `sideSlotPlan` : (jetons par slot, Mio de VRAM en plus, refus).
pub fn side_slot_plan(cfg: &Cfg, extra: &[String], si: &SysInfo) -> (i64, i64, String) {
    if !side_slot_on(cfg) {
        return (0, 0, String::new());
    }
    if is_external(cfg) {
        return (0, 0, "preset externe".into());
    }
    let p = trim(get(cfg, "PARALLEL"));
    if !p.is_empty() && p != "2" {
        return (
            0,
            0,
            format!("PARALLEL={p} (SIDE_SLOT ouvre exactement 2 slots)"),
        );
    }
    for f in SIDE_SLOT_CONFLICT_FLAGS {
        if has_any_flag(extra, &[f]) {
            return (0, 0, format!("{f} est réglé dans EXTRA_ARGS"));
        }
    }
    for k in SIDE_SLOT_ARG_ENV {
        if !si.env(k).is_empty() {
            return (0, 0, format!("{k} est posée"));
        }
    }
    if !si.help.contains("--no-kv-unified") {
        return (0, 0, "ce moteur ne connaît pas --no-kv-unified".into());
    }
    let raw = trim(get(cfg, "CTX"));
    let n = if raw.is_empty() {
        Some(32768)
    } else {
        atoi(raw)
    };
    let n = match n {
        Some(n) if n > 0 => n,
        _ => return (0, 0, "CTX doit être un nombre de jetons explicite".into()),
    };
    let (extra_mib, why) = side_slot_vram(cfg, extra, si, n);
    if !why.is_empty() {
        return (0, 0, why);
    }
    (n, extra_mib, String::new())
}

/// `sideSlotVRAM`.
pub fn side_slot_vram(cfg: &Cfg, extra: &[String], si: &SysInfo, ctx: i64) -> (i64, String) {
    if si.vram_mib <= 0 {
        return (0, "VRAM NVIDIA inconnue (macOS, AMD, --device…)".into());
    }
    let Some(g) = si.gguf.as_ref() else {
        return (0, "métadonnées GGUF illisibles".into());
    };
    if si.model_bytes <= 0 {
        return (0, "taille du modèle inconnue".into());
    }
    let r = cpu_weights(cfg, extra, &si.arg_env, g.block_count);
    if !r.is_empty() {
        return (0, r);
    }
    let (kt, vt, _) = effective_kv_types(cfg, extra, &si.arg_env);
    let (per_tok, rec) = gguf_state_bytes(g, &kt, &vt);
    if per_tok <= 0.0 {
        return (0, "taille d'état inconnue".into());
    }
    let state = per_tok * ctx as f64 + rec as f64;
    let mib = 1048576.0;
    if si.model_bytes as f64 + 2.0 * state > 0.9 * si.vram_mib as f64 * mib {
        return (
            0,
            format!(
                "VRAM insuffisante : modèle + 2 × {} Mio d'état dépassent 90 % de {} Mio",
                (state / mib).ceil() as i64,
                si.vram_mib
            ),
        );
    }
    ((state / mib).ceil() as i64, String::new())
}

/// `sideSlotNote`.
pub fn side_slot_note(ctx: i64, extra_mib: i64, why: &str, ngl_forced: bool) -> String {
    if ctx <= 0 {
        if why.is_empty() {
            return String::new();
        }
        return format!("SIDE_SLOT refusé ({why}) : un seul slot, comme sans la clé");
    }
    let n = format!(
        "SIDE_SLOT : 2 slots de {ctx} jetons (-c {}, --no-kv-unified), ~{extra_mib} Mio de VRAM en plus pour le second (cache KV et état récurrent ; le brouillon MTP éventuel et les points de reprise en RAM hôte doublent aussi)",
        2 * ctx
    );
    if ngl_forced {
        return n + " ; NGL est imposé : surveille la VRAM au premier long prompt";
    }
    n + " ; le placement automatique sortira des couches du GPU s'il manque de place"
}

/// `nglForced` : le preset impose-t-il un nombre de couches GPU ?
pub fn ngl_forced(cfg: &Cfg, extra: &[String]) -> bool {
    let mut v = flag_value(extra, &["-ngl", "--n-gpu-layers", "--gpu-layers"]);
    if v.is_empty() {
        v = trim(get(cfg, "NGL")).to_string();
        if v == "999" {
            return false;
        }
    }
    !v.is_empty() && !v.eq_ignore_ascii_case("auto")
}
