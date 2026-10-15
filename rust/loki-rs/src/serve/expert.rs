//! Réglages d'expert du moteur local (`backend_serve_expert.go`) : marge de
//! --fit par carte, seuil d'envoi au GPU des poids restés en RAM, branches
//! Q/K/V en parallèle. Tous éteints tant que le preset ne les demande pas.

use super::{
    Cfg, SysInfo, ctx_fixed, flag_value, flag_values, get, has_any_flag, help_fits_layers_itself,
    ngl_args, tensor_override, trim,
};
use std::collections::HashMap;

/// `expertEnvKeys`.
pub const EXPERT_ENV_KEYS: &[&str] = &["GGML_CUDA_GRAPH_OPT", "GGML_OP_OFFLOAD_MIN_BATCH"];
/// `fitArgEnv`.
pub const FIT_ARG_ENV: &[&str] = &[
    "LLAMA_ARG_FIT",
    "LLAMA_ARG_FIT_TARGET",
    "LLAMA_ARG_TENSOR_SPLIT",
];

pub const FIT_TARGET_MIN: i64 = 1024;
const FIT_TARGET_MAX_VALUES: usize = 8;

/// `parseFitTarget` : (valeurs, remontées sous 1024, valide).
pub fn parse_fit_target(v: &str) -> (Vec<i64>, bool, bool) {
    let parts: Vec<&str> = v.split([',', '/']).filter(|p| !p.is_empty()).collect();
    let seps = v.matches(',').count() + v.matches('/').count();
    if parts.is_empty() || parts.len() > FIT_TARGET_MAX_VALUES || seps != parts.len() - 1 {
        return (Vec::new(), false, false);
    }
    let mut vals = Vec::new();
    let mut raised = false;
    for p in parts {
        let p = trim(p);
        if p.is_empty() || !p.bytes().all(|b| b.is_ascii_digit()) || p.len() > 7 {
            return (Vec::new(), false, false);
        }
        let mut n: i64 = p.parse().unwrap_or(0);
        if n < FIT_TARGET_MIN {
            n = FIT_TARGET_MIN;
            raised = true;
        }
        vals.push(n);
    }
    (vals, raised, true)
}

/// `fitTargetArgs`.
pub fn fit_target_args(cfg: &Cfg, extra: &[String], si: &SysInfo) -> (Vec<String>, Vec<String>) {
    let raw = trim(get(cfg, "FIT_TARGET"));
    if raw.is_empty() {
        return (Vec::new(), Vec::new());
    }
    let ignore = |why: &str| (Vec::new(), vec![format!("FIT_TARGET={raw} ignoré : {why}")]);
    let (vals, raised, ok) = parse_fit_target(raw);
    if !ok {
        return ignore("des Mio par carte, ex. 1024,3072 (8 valeurs au plus)");
    }
    if !si.help.contains("--fit-target") {
        return ignore("ce moteur ne connaît pas --fit-target");
    }
    if has_any_flag(extra, &["-fitt", "--fit-target"]) {
        return ignore("--fit-target déjà dans EXTRA_ARGS");
    }
    if !si.env("LLAMA_ARG_FIT_TARGET").is_empty() {
        return ignore("LLAMA_ARG_FIT_TARGET déjà posé dans l'environnement");
    }
    let (ctx, ok) = ctx_fixed(cfg, extra);
    if !ok {
        return ignore(&format!(
            "contexte « {ctx} » non fixé — --fit pourrait le réduire pour tenir ; donne un CTX chiffré"
        ));
    }
    let why = fit_blocker(cfg, extra, si);
    if !why.is_empty() {
        return ignore(&format!(
            "--fit inactif ({why}), la marge ne servirait à rien"
        ));
    }
    let v = vals
        .iter()
        .map(i64::to_string)
        .collect::<Vec<_>>()
        .join(",");
    let mut note = format!(
        "--fit-target {v} (FIT_TARGET, Mio libres par carte, dans l'ordre vu par le moteur) — vérifie « offloaded N/N layers » dans le journal"
    );
    if raised {
        note.push_str(" ; marges sous 1024 Mio remontées à 1024");
    }
    (vec!["--fit-target".into(), v], vec![note])
}

/// `fitBlocker` : pourquoi llama.cpp ne lancera PAS --fit (vide = il le
/// lancera).
pub fn fit_blocker(cfg: &Cfg, extra: &[String], si: &SysInfo) -> String {
    let mut fit = flag_value(extra, &["-fit", "--fit"]);
    if fit.is_empty() {
        fit = si.env("LLAMA_ARG_FIT").into();
    }
    if matches!(
        trim(&fit).to_lowercase().as_str(),
        "off" | "disabled" | "false" | "0"
    ) {
        return "--fit off".into();
    }
    let mut ngl = flag_value(extra, &["-ngl", "--n-gpu-layers", "--gpu-layers"]);
    if ngl.is_empty() {
        let (a, _) = ngl_args(get(cfg, "NGL"), help_fits_layers_itself(&si.help));
        if a.len() == 2 {
            ngl = a[1].clone();
        } else {
            ngl = si.env("LLAMA_ARG_N_GPU_LAYERS").into();
        }
    }
    let ngl = trim(&ngl);
    if !ngl.is_empty() && ngl != "-1" && !ngl.eq_ignore_ascii_case("auto") {
        return format!("couches GPU fixées à {ngl}");
    }
    if has_any_flag(extra, &["-ts", "--tensor-split"])
        || !si.env("LLAMA_ARG_TENSOR_SPLIT").is_empty()
    {
        return "--tensor-split".into();
    }
    let r = tensor_override(extra, &si.arg_env);
    if !r.is_empty() {
        return r;
    }
    let mut sm = flag_value(extra, &["-sm", "--split-mode"]);
    if sm.is_empty() {
        sm = si.env("LLAMA_ARG_SPLIT_MODE").into();
    }
    let sm = trim(&sm).to_lowercase();
    if sm == "row" || sm == "tensor" {
        return format!("-sm {sm}");
    }
    String::new()
}

/// `opOffloadMinBatchEnv`.
pub fn op_offload_min_batch_env(cfg: &Cfg, si: &SysInfo) -> (String, Vec<String>) {
    let raw = trim(get(cfg, "OP_OFFLOAD_MIN_BATCH"));
    if raw.is_empty() || !si.uenv("GGML_OP_OFFLOAD_MIN_BATCH").is_empty() {
        return (String::new(), Vec::new());
    }
    match super::atoi(raw) {
        Some(n) if (1..=1 << 20).contains(&n) => {
            let v = n.to_string();
            (
                v.clone(),
                vec![format!(
                    "GGML_OP_OFFLOAD_MIN_BATCH={v} (OP_OFFLOAD_MIN_BATCH) : lots de moins de {v} jetons calculés sur CPU pour les poids restés en RAM"
                )],
            )
        }
        _ => (
            String::new(),
            vec![format!(
                "OP_OFFLOAD_MIN_BATCH={raw} ignoré : un nombre de jetons, ex. 128"
            )],
        ),
    }
}

/// `graphOptEnv`.
pub fn graph_opt_env(cfg: &Cfg, extra: &[String], si: &SysInfo) -> (String, Vec<String>) {
    let raw = trim(get(cfg, "CUDA_GRAPH_OPT"));
    match raw.to_lowercase().as_str() {
        "" | "off" => return (String::new(), Vec::new()),
        "on" => {}
        _ => {
            return (
                String::new(),
                vec![format!("CUDA_GRAPH_OPT={raw} ignoré (on ou off)")],
            );
        }
    }
    if !si.uenv("GGML_CUDA_GRAPH_OPT").is_empty() {
        return (String::new(), Vec::new());
    }
    let why = qkv_split_risk(extra, &si.arg_env);
    if !why.is_empty() {
        return (
            String::new(),
            vec![format!(
                "CUDA_GRAPH_OPT ignoré : {why} peut séparer Q, K et V d'une couche entre cartes"
            )],
        );
    }
    ("1".into(), vec!["GGML_CUDA_GRAPH_OPT=1 (CUDA_GRAPH_OPT, expérimental) — si le moteur s'arrête sur un GGML_ASSERT, CUDA_GRAPH_OPT=off puis redémarre".into()])
}

/// `qkvSplitRisk`.
pub fn qkv_split_risk(extra: &[String], arg_env: &HashMap<String, String>) -> String {
    let env = |k: &str| arg_env.get(k).map_or("", String::as_str);
    let mut sm = flag_value(extra, &["-sm", "--split-mode"]);
    if sm.is_empty() {
        sm = env("LLAMA_ARG_SPLIT_MODE").into();
    }
    let sm = trim(&sm).to_lowercase();
    if sm == "row" || sm == "tensor" {
        return format!("-sm {sm}");
    }
    let mut vals = flag_values(extra, &["-ot", "--override-tensor"]);
    vals.push(env("LLAMA_ARG_OVERRIDE_TENSOR").to_string());
    if vals.iter().any(|v| v.to_lowercase().contains("attn")) {
        return "-ot sur l'attention".into();
    }
    String::new()
}
