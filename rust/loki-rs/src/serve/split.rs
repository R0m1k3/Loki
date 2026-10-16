//! Parallélisme de tenseurs (`backend_serve_split.go`, `SPLIT_MODE=tensor`,
//! expérimental) : `-sm tensor` entre cartes CUDA, avec une longue liste de
//! refus propres et `GGML_CUDA_ALLREDUCE=internal` (sommes dans le type
//! natif, sans la compression BF16 de NCCL).

use std::collections::BTreeMap;

use super::{
    Cfg, SplitDev, SysInfo, atoi, ctx_fixed, effective_kv_types, env_truthy, flag_value, get,
    gguf_state_bytes, has_any_flag, is_external, kv_offloaded, side_slot_on, spec_mode,
    spec_user_set, tensor_override, trim,
};

/// `splitTensorDenyArch`.
pub fn split_tensor_deny_arch(arch: &str) -> bool {
    matches!(
        arch,
        "grok"
            | "mpt"
            | "plamo2"
            | "minicpm3"
            | "gemma3n"
            | "mamba"
            | "mamba2"
            | "jamba"
            | "falcon-h1"
            | "olmo2"
            | "olmoe"
            | "deepseek2"
            | "deepseek32"
            | "hy_v4"
            | "dots3note"
            | "glm-dsa"
            | "bitnet"
            | "t5"
            | "nemotron_h"
            | "nemotron_h_moe"
            | "granitehybrid"
            | "minimax-01"
            | "minimax-m2"
            | "minimax-m3"
            | "mistral4"
            | "kimi-linear"
            | "bailingmoe3"
            | "kimi-k3"
            | "glm5-next"
            | "qwen3tts"
            | "qwen4exp"
    )
}

/// `splitArgEnv`.
pub const SPLIT_ARG_ENV: &[&str] = &[
    "LLAMA_ARG_FLASH_ATTN",
    "LLAMA_ARG_BACKEND_SAMPLING",
    "LLAMA_ARG_TENSOR_SPLIT",
];

/// `splitModeKey` : "" (rien à poser), "tensor", ou "?" si illisible.
pub fn split_mode_key(cfg: &Cfg) -> &'static str {
    match trim(get(cfg, "SPLIT_MODE")).to_lowercase().as_str() {
        "" | "off" | "layer" | "non" | "no" | "0" => "",
        "tensor" => "tensor",
        _ => "?",
    }
}

/// `helpSupportsSplitTensor`.
pub fn help_supports_split_tensor(help: &str) -> bool {
    help.contains("{none,layer,row,tensor}")
}

/// Décision de SPLIT_MODE=tensor (`splitPlan`).
#[derive(Clone, Debug, Default)]
pub struct SplitPlan {
    pub on: bool,
    pub why: String,
    pub devs: Vec<SplitDev>,
    pub need_mib: i64,
}

fn refuse(why: impl Into<String>) -> SplitPlan {
    SplitPlan {
        why: why.into(),
        ..SplitPlan::default()
    }
}

fn split_kv_ok(t: &str) -> bool {
    matches!(trim(t).to_lowercase().as_str(), "" | "f16" | "bf16" | "f32")
}

fn kv_name(t: &str) -> &str {
    if t.is_empty() { "f16" } else { t }
}

/// `splitPlan`.
pub fn split_plan(cfg: &Cfg, extra: &[String], si: &SysInfo) -> SplitPlan {
    match split_mode_key(cfg) {
        "" => return SplitPlan::default(),
        "?" => {
            return refuse(format!(
                "SPLIT_MODE={} illisible (tensor ou off)",
                trim(get(cfg, "SPLIT_MODE"))
            ));
        }
        _ => {}
    }
    if is_external(cfg) {
        return refuse("preset externe");
    }
    if !help_supports_split_tensor(&si.help) {
        return refuse("ce moteur ne propose pas -sm tensor");
    }
    let sm = flag_value(extra, &["-sm", "--split-mode"]);
    if !sm.is_empty() {
        return refuse(format!("-sm {sm} déjà dans EXTRA_ARGS"));
    }
    if !si.env("LLAMA_ARG_SPLIT_MODE").is_empty() {
        return refuse("LLAMA_ARG_SPLIT_MODE déjà posée");
    }
    let mut fa = flag_value(extra, &["-fa", "--flash-attn"]);
    if fa.is_empty() {
        fa = si.env("LLAMA_ARG_FLASH_ATTN").into();
    }
    if matches!(
        trim(&fa).to_lowercase().as_str(),
        "off" | "disabled" | "false" | "0"
    ) {
        return refuse(format!(
            "Flash Attention coupée (-fa {fa}) : le mode tensor l'exige"
        ));
    }
    if has_any_flag(extra, &["-bs", "--backend-sampling"])
        || env_truthy(si.env("LLAMA_ARG_BACKEND_SAMPLING"))
    {
        return refuse(
            "échantillonnage sur GPU (--backend-sampling) non pris en charge en mode tensor",
        );
    }
    let (k, v, _) = effective_kv_types(cfg, extra, &si.arg_env);
    if !split_kv_ok(&k) || !split_kv_ok(&v) {
        return refuse(format!(
            "cache KV {}/{} : le mode tensor n'accepte que f16, bf16 ou f32 (jamais réécrit d'office : la VRAM changerait)",
            kv_name(&k),
            kv_name(&v)
        ));
    }
    let r = tensor_override(extra, &si.arg_env);
    if !r.is_empty() {
        return refuse(format!("{r} : le mode tensor met tout sur les cartes"));
    }
    if !kv_offloaded(extra, &si.arg_env) {
        return refuse("cache KV sur CPU");
    }
    let g = match si.gguf.as_ref() {
        Some(g) if si.model_bytes > 0 => g,
        _ => return refuse("métadonnées ou taille du modèle illisibles"),
    };
    if split_tensor_deny_arch(&g.arch.to_lowercase()) {
        return refuse(format!("architecture {} exclue du mode tensor", g.arch));
    }
    let (_, why) = split_ngl(cfg, extra, g.block_count);
    if !why.is_empty() {
        return refuse(why);
    }
    let (ctx_s, ok) = ctx_fixed(cfg, extra);
    if !ok {
        return refuse(format!(
            "contexte « {ctx_s} » non chiffré : pas de --fit en mode tensor, donne un CTX explicite"
        ));
    }
    if side_slot_on(cfg) {
        return refuse("SIDE_SLOT : cache doublé, sans --fit pour rattraper");
    }
    if spec_mode(cfg) != "off" || spec_user_set(extra, &si.arg_env) {
        return refuse(
            "décodage spéculatif : brouillon et vérification par lots non validés en mode tensor",
        );
    }
    let (devs, why) = split_devices(extra, si);
    if !why.is_empty() {
        return refuse(why);
    }
    let ctx = atoi(&ctx_s).unwrap_or(0);
    let (per_tok, rec) = gguf_state_bytes(g, &k, &v);
    if per_tok <= 0.0 {
        return refuse("taille du cache KV inconnue pour ce modèle");
    }
    let total: i64 = devs.iter().map(|d| d.total_mib).sum();
    let mm = if si.mmproj.is_empty() {
        0
    } else {
        std::fs::metadata(&si.mmproj).map_or(0, |m| m.len() as i64)
    };
    let mib = 1048576.0;
    let need = (si.model_bytes + mm) as f64 + per_tok * ctx as f64 + rec as f64;
    let need_mib = (need / mib).ceil() as i64;
    if need > 0.9 * total as f64 * mib {
        return refuse(format!(
            "VRAM insuffisante sans --fit : ~{need_mib} Mio (poids + cache de {ctx} jetons) pour {total} Mio sur {} cartes — CTX n'est jamais réduit d'office",
            devs.len()
        ));
    }
    SplitPlan {
        on: true,
        why: String::new(),
        devs,
        need_mib,
    }
}

/// `splitNGL`.
pub fn split_ngl(cfg: &Cfg, extra: &[String], blocks: i64) -> (Vec<String>, String) {
    let all = |v: &str| -> bool {
        let v = trim(v).to_lowercase();
        if v == "all" || v == "999" {
            return true;
        }
        atoi(&v).is_some_and(|n| blocks > 0 && n > blocks)
    };
    let v = flag_value(extra, &["-ngl", "--n-gpu-layers", "--gpu-layers"]);
    if !v.is_empty() {
        if all(&v) {
            return (Vec::new(), String::new());
        }
        return (
            Vec::new(),
            format!(
                "-ngl {v} dans EXTRA_ARGS : le mode tensor veut toutes les couches sur les cartes (all)"
            ),
        );
    }
    let v = trim(get(cfg, "NGL"));
    if v.is_empty() || v.eq_ignore_ascii_case("auto") || all(v) {
        (vec!["-ngl".into(), "all".into()], String::new())
    } else {
        (
            Vec::new(),
            format!("NGL={v} : le mode tensor veut toutes les couches sur les cartes (all)"),
        )
    }
}

/// `splitDevices`.
pub fn split_devices(extra: &[String], si: &SysInfo) -> (Vec<SplitDev>, String) {
    if si.split_devs.is_empty() {
        return (
            Vec::new(),
            "cartes inconnues (--list-devices n'a rien répondu)".into(),
        );
    }
    let mut devs = si.split_devs.clone();
    let mut sel = flag_value(extra, &["-dev", "--device"]);
    if sel.is_empty() {
        sel = si.env("LLAMA_ARG_DEVICE").into();
    }
    let sel = trim(&sel);
    if !sel.is_empty() {
        devs = Vec::new();
        for name in sel.split(',') {
            let name = trim(name);
            match si
                .split_devs
                .iter()
                .find(|d| d.id.eq_ignore_ascii_case(name))
            {
                Some(d) => devs.push(d.clone()),
                None => {
                    return (Vec::new(), format!("--device {name} inconnu de ce moteur"));
                }
            }
        }
    }
    for d in &devs {
        if !d.id.to_uppercase().starts_with("CUDA") {
            return (Vec::new(), format!("{} n'est pas une carte CUDA", d.id));
        }
    }
    if devs.len() < 2 {
        return (Vec::new(), "une seule carte".into());
    }
    (devs, String::new())
}

/// `splitTS` : répartition au prorata de la VRAM totale, en pour cent.
pub fn split_ts(devs: &[SplitDev]) -> String {
    let total: i64 = devs.iter().map(|d| d.total_mib).sum();
    if total <= 0 {
        return String::new();
    }
    devs.iter()
        .map(|d| go_round(100.0 * d.total_mib as f64 / total as f64).to_string())
        .collect::<Vec<_>>()
        .join(",")
}

/// `math.Round` : moitié arrondie loin de zéro (comme `f64::round`).
fn go_round(x: f64) -> i64 {
    x.round() as i64
}

/// Ce que SPLIT_MODE=tensor ajoute (`splitTensorArgs`).
#[derive(Clone, Debug, Default)]
pub struct SplitTensor {
    pub on: bool,
    pub args: Vec<String>,
    pub ngl: Vec<String>,
    pub env: BTreeMap<String, String>,
    pub notes: Vec<String>,
}

/// `splitTensorArgs`.
pub fn split_tensor_args(cfg: &Cfg, extra: &[String], si: &SysInfo) -> SplitTensor {
    let plan = split_plan(cfg, extra, si);
    let mut out = SplitTensor::default();
    if !plan.on {
        if !plan.why.is_empty() {
            out.notes.push(format!(
                "SPLIT_MODE=tensor refusé ({}) : découpe par couches, comme sans la clé",
                plan.why
            ));
        }
        return out;
    }
    let g = si.gguf.as_ref().expect("plan accepté : GGUF lu");
    out.on = true;
    out.ngl = split_ngl(cfg, extra, g.block_count).0;
    out.args = vec!["-sm".into(), "tensor".into()];
    let mut ts = String::new();
    if !has_any_flag(extra, &["-ts", "--tensor-split"])
        && si.env("LLAMA_ARG_TENSOR_SPLIT").is_empty()
    {
        ts = split_ts(&plan.devs);
        if !ts.is_empty() {
            out.args.push("-ts".into());
            out.args.push(ts.clone());
        }
    }
    if !has_any_flag(extra, &["-fa", "--flash-attn"]) && si.env("LLAMA_ARG_FLASH_ATTN").is_empty() {
        out.args.push("-fa".into());
        out.args.push("on".into());
    }
    let mut note = format!(
        "SPLIT_MODE=tensor (expérimental) : -sm tensor sur {} cartes",
        plan.devs.len()
    );
    if !ts.is_empty() {
        note.push_str(&format!(", -ts {ts} (VRAM totale)"));
    }
    note.push_str(&format!(", ~{} Mio estimés sans --fit ; décodage plus rapide, prefill plus lent — compare le temps par tour", plan.need_mib));
    let ar = trim(si.uenv("GGML_CUDA_ALLREDUCE")).to_lowercase();
    match ar.as_str() {
        "" => {
            out.env.insert("GGML_CUDA_ALLREDUCE".into(), "internal".into());
            note.push_str(" ; GGML_CUDA_ALLREDUCE=internal : sommes dans le type natif (NCCL les compresserait en BF16, avec perte). « NCCL not compiled in » ou « NCCL is unavailable » au journal : sans effet ici. GeForce sans P2P : les sommes passent par l'hôte ; GGML_CUDA_P2P=1 à poser soi-même si le pilote le permet");
        }
        "nccl" => note.push_str(" ; avertissement : GGML_CUDA_ALLREDUCE=nccl (environnement) — NCCL compresse en BF16 les grosses réductions du prefill : calcul avec perte. « NCCL not compiled in » au journal = repli sur la réduction interne"),
        _ => note.push_str(&format!(" ; GGML_CUDA_ALLREDUCE={ar} (environnement) gardé")),
    }
    if g.hybrid {
        note.push_str(" ; modèle hybride : état récurrent et points de reprise répartis entre cartes, non validés en amont");
    }
    out.notes.push(note);
    out
}

/// `parseListDevices` : les cartes de `llama-server --list-devices`.
pub fn parse_list_devices(out: &str) -> Vec<SplitDev> {
    let re =
        regex::Regex::new(r"^\s*([A-Za-z]+\d+):\s*(.+?)\s*\((\d+)\s*MiB,\s*(\d+)\s*MiB free\)\s*$")
            .expect("motif");
    out.split('\n')
        .filter_map(|line| {
            let m = re.captures(line.trim_end_matches('\r'))?;
            Some(SplitDev {
                id: m[1].to_string(),
                total_mib: m[3].parse().unwrap_or(0),
            })
        })
        .collect()
}
