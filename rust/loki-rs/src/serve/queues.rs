//! Files de lancement CUDA sur deux GPU ou plus
//! (`backend_serve_queues.go`) : `CUDA_SCALE_LAUNCH_QUEUES=4x` là où le
//! pipeline entre cartes peut exister.

use super::{Cfg, SysInfo, env_truthy, flag_value, flag_values, get, has_any_flag, trim};
use std::collections::HashMap;

pub const LAUNCH_QUEUES_AUTO: &str = "4x";

/// `serveArgEnv`.
pub const SERVE_ARG_ENV: &[&str] = &[
    "LLAMA_ARG_DEVICE",
    "LLAMA_ARG_SPLIT_MODE",
    "LLAMA_ARG_N_GPU_LAYERS",
    "LLAMA_ARG_OVERRIDE_TENSOR",
    "LLAMA_ARG_CPU_MOE",
    "LLAMA_ARG_N_CPU_MOE",
    "LLAMA_ARG_N_CPU_FFN",
    "LLAMA_ARG_KV_OFFLOAD",
    "LLAMA_ARG_NO_KV_OFFLOAD",
];

fn launch_queue_value(v: &str) -> bool {
    matches!(v, "0.25x" | "0.5x" | "2x" | "4x")
}

/// `launchQueuesEnv` : valeur vide = ne rien poser.
pub fn launch_queues_env(cfg: &Cfg, extra: &[String], si: &SysInfo) -> (String, Vec<String>) {
    if !si.launch_queues.is_empty() {
        return (String::new(), Vec::new());
    }
    let mut notes = Vec::new();
    let want = trim(get(cfg, "CUDA_LAUNCH_QUEUES")).to_lowercase();
    if want == "off" {
        return (String::new(), notes);
    }
    if launch_queue_value(&want) {
        return (
            want.clone(),
            vec![format!(
                "CUDA_SCALE_LAUNCH_QUEUES={want} (CUDA_LAUNCH_QUEUES)"
            )],
        );
    }
    if !want.is_empty() && want != "auto" {
        notes.push(format!(
            "CUDA_LAUNCH_QUEUES={} ignoré (0.25x, 0.5x, 2x, 4x ou off) → automatique",
            get(cfg, "CUDA_LAUNCH_QUEUES")
        ));
    }
    let n = served_gpu_count(extra, si);
    if n < 2 || !pipeline_blocker(cfg, extra, &si.arg_env).is_empty() {
        return (String::new(), notes);
    }
    notes.push(format!(
        "CUDA_SCALE_LAUNCH_QUEUES={LAUNCH_QUEUES_AUTO} ({n} GPU, parallélisme pipeline possible) — CUDA_LAUNCH_QUEUES=off pour s'en passer"
    ));
    (LAUNCH_QUEUES_AUTO.into(), notes)
}

/// `servedGPUCount`.
pub fn served_gpu_count(extra: &[String], si: &SysInfo) -> i64 {
    let mut dev = flag_value(extra, &["-dev", "--device"]);
    if dev.is_empty() {
        dev = si.env("LLAMA_ARG_DEVICE").to_string();
    }
    if dev.is_empty() {
        return si.gpus;
    }
    dev.split(',')
        .filter(|d| trim(d).to_uppercase().starts_with("CUDA"))
        .count() as i64
}

/// `pipelineBlocker` : pourquoi llama.cpp n'ouvrira PAS le pipeline (vide =
/// il peut).
pub fn pipeline_blocker(cfg: &Cfg, extra: &[String], arg_env: &HashMap<String, String>) -> String {
    let env = |k: &str| arg_env.get(k).map_or("", String::as_str);
    let r = tensor_override(extra, arg_env);
    if !r.is_empty() {
        return r;
    }
    if !kv_offloaded(extra, arg_env) {
        return "cache KV sur CPU".into();
    }
    let mut sm = flag_value(extra, &["-sm", "--split-mode"]);
    if sm.is_empty() {
        sm = env("LLAMA_ARG_SPLIT_MODE").into();
    }
    if !sm.is_empty() && sm != "layer" {
        return format!("découpe {sm}");
    }
    let mut ngl = flag_value(extra, &["-ngl", "--n-gpu-layers", "--gpu-layers"]);
    if ngl.is_empty() {
        ngl = trim(get(cfg, "NGL")).to_string();
        if ngl.eq_ignore_ascii_case("auto") {
            ngl = env("LLAMA_ARG_N_GPU_LAYERS").into();
        }
    }
    let ngl = trim(&ngl).to_lowercase();
    if !ngl.is_empty() && ngl != "auto" && ngl != "all" && ngl != "999" {
        return format!("couches GPU limitées à {ngl}");
    }
    String::new()
}

/// `tensorOverride` : tenseurs placés à la main (vide = aucun).
pub fn tensor_override(extra: &[String], arg_env: &HashMap<String, String>) -> String {
    let env = |k: &str| arg_env.get(k).map_or("", String::as_str);
    if has_any_flag(extra, &["-ot", "--override-tensor", "--cpu-moe", "-cmoe"])
        || !env("LLAMA_ARG_OVERRIDE_TENSOR").is_empty()
        || env_truthy(env("LLAMA_ARG_CPU_MOE"))
    {
        return "surcharge de tenseurs".into();
    }
    for (e, short, long) in [
        ("LLAMA_ARG_N_CPU_MOE", "-ncmoe", "--n-cpu-moe"),
        ("LLAMA_ARG_N_CPU_FFN", "-ncffn", "--n-cpu-ffn"),
    ] {
        let mut vals = flag_values(extra, &[short, long]);
        vals.push(env(e).to_string());
        for v in vals {
            let v = trim(&v);
            if !v.is_empty() && v != "0" {
                return format!("couches {long} sur CPU");
            }
        }
    }
    String::new()
}

/// `kvOffloaded` : le cache KV reste-t-il sur GPU ?
pub fn kv_offloaded(extra: &[String], arg_env: &HashMap<String, String>) -> bool {
    let (mut on, mut set) = (true, false);
    for a in extra {
        let name = a.split_once('=').map_or(a.as_str(), |(n, _)| n);
        match name {
            "-kvo" | "--kv-offload" => {
                on = true;
                set = true;
            }
            "-nkvo" | "--no-kv-offload" => {
                on = false;
                set = true;
            }
            _ => {}
        }
    }
    if set {
        return on;
    }
    if arg_env
        .get("LLAMA_ARG_NO_KV_OFFLOAD")
        .is_some_and(|v| !v.is_empty())
    {
        return false;
    }
    !matches!(
        trim(
            arg_env
                .get("LLAMA_ARG_KV_OFFLOAD")
                .map_or("", String::as_str)
        )
        .to_lowercase()
        .as_str(),
        "off" | "disabled" | "false" | "0"
    )
}
