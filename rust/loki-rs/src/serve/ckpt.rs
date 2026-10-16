//! Points de reprise des modèles hybrides et conservation du raisonnement
//! (`backend_serve_ckpt.go`) : --ctx-checkpoints, --checkpoint-min-step,
//! --reasoning-preserve. Le build n'est cru que pour un moteur officiel.

use super::{
    Cfg, SysInfo, atoi, flag_value, flag_values, get, gguf_state_bytes, has_any_flag, trim,
};
use crate::gguf::GgufMeta;

pub const ENGINE_MIN_RECOMMENDED: i64 = 10864;
pub const ENGINE_BUILD_FLOOR: i64 = 5000;
pub const ENGINE_CKPT_DEFAULT: i64 = 32;
pub const CKPT_AUTO_MIN_STEP: i64 = 2048;
pub const CKPT_FALLBACK_MIB: i64 = 200;

/// `ckptArgEnv`.
pub const CKPT_ARG_ENV: &[&str] = &[
    "LLAMA_ARG_CTX_CHECKPOINTS",
    "LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT",
    "LLAMA_ARG_REASONING_PRESERVE",
    "LLAMA_ARG_CHAT_TEMPLATE_KWARGS",
];

/// `ggufHybrid` : couches récurrentes (métadonnées, ou architecture connue).
pub fn gguf_hybrid(g: Option<&GgufMeta>) -> bool {
    g.is_some_and(|g| {
        g.hybrid
            || matches!(
                g.arch.to_lowercase().as_str(),
                "qwen35" | "qwen35moe" | "qwen3next" | "qwen4exp"
            )
    })
}

/// `engineBuildTrust` : build cru seulement pour un moteur officiel.
pub fn engine_build_trust(source: &str, build: i64) -> i64 {
    if source == "custom" || build < ENGINE_BUILD_FLOOR {
        0
    } else {
        build
    }
}

/// `engineBuildNotice`.
pub fn engine_build_notice(build: i64) -> String {
    if build <= 0 || build >= ENGINE_MIN_RECOMMENDED {
        return String::new();
    }
    format!(
        "moteur b{build} : les points de reprise des modèles hybrides (Qwen3.5/3.6, Qwen3-Next…) y sont évincés trop tôt — jusqu'à ~8 k jetons recalculés à chaque reprise. Corrigé à partir de b{ENGINE_MIN_RECOMMENDED}."
    )
}

/// `ckptCount` : points de reprise par slot (-1 = illisible).
pub fn ckpt_count(cfg: &Cfg, extra: &[String], si: &SysInfo) -> i64 {
    let mut v = flag_value(extra, &["-ctxcp", "--ctx-checkpoints", "--swa-checkpoints"]);
    if v.is_empty() {
        v = si.env("LLAMA_ARG_CTX_CHECKPOINTS").into();
    }
    if v.is_empty() {
        return match atoi(trim(get(cfg, "CTX_CHECKPOINTS"))) {
            Some(n) if n >= 0 => n,
            _ => ENGINE_CKPT_DEFAULT,
        };
    }
    match atoi(trim(&v)) {
        Some(n) if n >= 0 => n,
        _ => -1,
    }
}

/// `ckptSlots`.
pub fn ckpt_slots(cfg: &Cfg, extra: &[String]) -> i64 {
    let mut v = flag_value(extra, &["-np", "--parallel"]);
    if v.is_empty() {
        v = trim(get(cfg, "PARALLEL")).to_string();
    }
    if v.is_empty() {
        return 1;
    }
    match atoi(trim(&v)) {
        Some(n) if n > 0 => n,
        _ => 4,
    }
}

/// `ckptMiB`.
pub fn ckpt_mib(g: Option<&GgufMeta>) -> i64 {
    if let Some(g) = g {
        let (_, rec) = gguf_state_bytes(g, "", "");
        if rec > 0 {
            return (rec + (1 << 20) - 1) >> 20;
        }
    }
    CKPT_FALLBACK_MIB
}

/// `ramTight` : le modèle occupe plus de 70 % de la RAM.
pub fn ram_tight(si: &SysInfo) -> bool {
    si.ram_mib > 0
        && si.model_bytes > 0
        && si.model_bytes as f64 > 0.7 * si.ram_mib as f64 * 1048576.0
}

/// `ckptArgs`.
pub fn ckpt_args(cfg: &Cfg, extra: &[String], si: &SysInfo) -> (Vec<String>, Vec<String>) {
    let mut args: Vec<String> = Vec::new();
    let mut notes: Vec<String> = Vec::new();
    let hybrid = gguf_hybrid(si.gguf.as_ref());
    let cp_flags = ["-ctxcp", "--ctx-checkpoints", "--swa-checkpoints"];

    let v = trim(get(cfg, "CTX_CHECKPOINTS"));
    if !v.is_empty() {
        match atoi(v) {
            Some(n) if n >= 0 => {
                if !si.help.contains("--ctx-checkpoints") {
                    notes.push(
                        "ce moteur ne connaît pas --ctx-checkpoints : CTX_CHECKPOINTS ignoré"
                            .into(),
                    );
                } else if has_any_flag(extra, &cp_flags) {
                } else if !si.env("LLAMA_ARG_CTX_CHECKPOINTS").is_empty() {
                    notes.push(
                        "LLAMA_ARG_CTX_CHECKPOINTS est posée : CTX_CHECKPOINTS ignoré".into(),
                    );
                } else {
                    args.push("--ctx-checkpoints".into());
                    args.push(n.to_string());
                }
            }
            _ => notes.push(format!(
                "CTX_CHECKPOINTS={v} illisible (un nombre de points de reprise par slot, ≥ 0) : ignoré"
            )),
        }
    } else if hybrid
        && ram_tight(si)
        && si.help.contains("--ctx-checkpoints")
        && !has_any_flag(extra, &cp_flags)
        && si.env("LLAMA_ARG_CTX_CHECKPOINTS").is_empty()
    {
        notes.push(format!(
            "modèle hybride de {} Gio pour {} Gio de RAM : jusqu'à {ENGINE_CKPT_DEFAULT} points de reprise (~{} Mio chacun) prennent sur le cache de pages des poids. CTX_CHECKPOINTS=8 à 16 si le débit baisse.",
            si.model_bytes >> 30,
            si.ram_mib >> 10,
            ckpt_mib(si.gguf.as_ref())
        ));
    }

    let step_user = has_any_flag(extra, &["-cms", "--checkpoint-min-step"])
        || !si.env("LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT").is_empty();
    let v = trim(get(cfg, "CKPT_MIN_STEP"));
    if !v.is_empty() {
        match atoi(v) {
            Some(n) if n > 0 => {
                if !si.help.contains("--checkpoint-min-step") {
                    notes.push(
                        "ce moteur ne connaît pas --checkpoint-min-step : CKPT_MIN_STEP ignoré"
                            .into(),
                    );
                } else if has_any_flag(extra, &["-cms", "--checkpoint-min-step"]) {
                } else if !si.env("LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT").is_empty() {
                    notes.push(
                        "LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT est posée : CKPT_MIN_STEP ignoré"
                            .into(),
                    );
                } else {
                    args.push("--checkpoint-min-step".into());
                    args.push(n.to_string());
                }
            }
            _ => notes.push(format!(
                "CKPT_MIN_STEP={v} refusé (un nombre de jetons > 0 ; 0 laisserait le moteur jeter le point de reprise du début de la conversation) : défaut du moteur"
            )),
        }
        return (args, notes);
    }

    let notice = engine_build_notice(si.engine_build);
    if !hybrid || notice.is_empty() {
        return (args, notes);
    }
    let n = ckpt_count(cfg, extra, si);
    if step_user || !si.help.contains("--checkpoint-min-step") || n <= 0 {
        notes.push(notice);
        return (args, notes);
    }
    if si.ram_mib <= 0 || si.model_bytes <= 0 || ram_tight(si) {
        notes.push(notice + " Pas d'espacement resserré d'office : RAM inconnue ou déjà prise par le modèle (CKPT_MIN_STEP=2048 pour l'imposer).");
        return (args, notes);
    }
    let total = n * ckpt_slots(cfg, extra);
    let worst = total * ckpt_mib(si.gguf.as_ref());
    if si.ram_avail_mib > 0 && worst > si.ram_avail_mib / 4 {
        notes.push(format!(
            "{notice} Pas d'espacement resserré d'office : {total} points de reprise (~{worst} Mio) dépasseraient le quart de la RAM libre (CKPT_MIN_STEP=2048 pour l'imposer)."
        ));
        return (args, notes);
    }
    args.push("--checkpoint-min-step".into());
    args.push(CKPT_AUTO_MIN_STEP.to_string());
    notes.push(format!(
        "{notice} → --checkpoint-min-step {CKPT_AUTO_MIN_STEP} : points de reprise exacts plus rapprochés, ~{worst} Mio de RAM hôte au plus. CKPT_MIN_STEP=<jetons> pour fixer."
    ));
    (args, notes)
}

/// `reasoningPreserveArgs`.
pub fn reasoning_preserve_args(
    cfg: &Cfg,
    extra: &[String],
    si: &SysInfo,
) -> (Vec<String>, Vec<String>) {
    let v = trim(get(cfg, "REASONING_PRESERVE")).to_lowercase();
    if v.is_empty() {
        return (Vec::new(), Vec::new());
    }
    let flag = match v.as_str() {
        "on" => "--reasoning-preserve",
        "off" => "--no-reasoning-preserve",
        _ => {
            return (
                Vec::new(),
                vec![format!(
                    "REASONING_PRESERVE={} illisible (on ou off) : ignoré",
                    get(cfg, "REASONING_PRESERVE")
                )],
            );
        }
    };
    let one = |n: &str| (Vec::new(), vec![n.to_string()]);
    if !si.help.contains("--no-reasoning-preserve") {
        return one("ce moteur ne connaît pas --reasoning-preserve : REASONING_PRESERVE ignoré");
    }
    if has_any_flag(extra, &["--reasoning-preserve", "--no-reasoning-preserve"]) {
        return (Vec::new(), Vec::new());
    }
    if flag_values(extra, &["--chat-template-kwargs"])
        .join(" ")
        .contains("preserve_reasoning")
    {
        return one(
            "--chat-template-kwargs fixe déjà preserve_reasoning : REASONING_PRESERVE ignoré",
        );
    }
    if !si.env("LLAMA_ARG_REASONING_PRESERVE").is_empty() {
        return one("LLAMA_ARG_REASONING_PRESERVE est posée : REASONING_PRESERVE ignoré");
    }
    if si
        .env("LLAMA_ARG_CHAT_TEMPLATE_KWARGS")
        .contains("preserve_reasoning")
    {
        return one(
            "LLAMA_ARG_CHAT_TEMPLATE_KWARGS fixe déjà preserve_reasoning : REASONING_PRESERVE ignoré",
        );
    }
    (vec![flag.to_string()], Vec::new())
}
