//! Cache de prompts en RAM et dossier des slots (`backend_serve_cache.go`) :
//! --cache-ram agrandi quand le besoin se mesure et que la RAM est libre,
//! jamais sous le défaut du moteur ; --slot-save-path seulement pour un
//! moteur fermé aux autres.

use std::path::Path;

use super::{
    Cfg, SysInfo, atoi, effective_kv_types, env_truthy, flag_value, flag_values, get, has_any_flag,
    kv_offloaded, slot_persist_plan, trim,
};
use crate::gguf::GgufMeta;
use std::collections::HashMap;

/// `engineCacheRAMDefault` : cache_ram_mib par défaut de llama.cpp.
pub const ENGINE_CACHE_RAM_DEFAULT: i64 = 8192;
/// `cacheArgEnv`.
pub const CACHE_ARG_ENV: &[&str] = &["LLAMA_ARG_CACHE_RAM"];

/// `cgroupMemMiB` : (limite, consommation) du conteneur en Mio ; 0 = aucune.
pub fn cgroup_mem_mib(root: &Path) -> (i64, i64) {
    let read = |p: &str| -> i64 {
        let Ok(b) = std::fs::read(root.join(p)) else {
            return 0;
        };
        match atoi(trim(&String::from_utf8_lossy(&b))) {
            Some(n) if n > 0 && n < 1 << 60 => n >> 20,
            _ => 0,
        }
    };
    let limit = read("sys/fs/cgroup/memory.max");
    if limit > 0 {
        return (limit, read("sys/fs/cgroup/memory.current"));
    }
    let limit = read("sys/fs/cgroup/memory/memory.limit_in_bytes");
    if limit > 0 {
        return (limit, read("sys/fs/cgroup/memory/memory.usage_in_bytes"));
    }
    (0, 0)
}

/// `vramFromSMI` : VRAM des cartes choisies par CUDA_VISIBLE_DEVICES.
pub fn vram_from_smi(out: &str, cvd: &str, pci_order: bool) -> i64 {
    let mut by_idx: HashMap<i64, i64> = HashMap::new();
    let mut min_mib = 0i64;
    for line in trim(out).split('\n') {
        let f: Vec<&str> = line.split(',').collect();
        if f.len() < 2 {
            continue;
        }
        match (atoi(trim(f[0])), atoi(trim(f[1]))) {
            (Some(i), Some(m)) if m > 0 => {
                by_idx.insert(i, m);
                if min_mib == 0 || m < min_mib {
                    min_mib = m;
                }
            }
            _ => return 0,
        }
    }
    if by_idx.is_empty() {
        return 0;
    }
    let cvd = trim(cvd);
    if cvd.is_empty() {
        return by_idx.values().sum();
    }
    let (mut sum, mut n) = (0i64, 0i64);
    for d in cvd.split(',') {
        let d = trim(d);
        if d.is_empty() {
            continue;
        }
        let Some(i) = atoi(d) else { return 0 };
        let Some(m) = by_idx.get(&i) else { return 0 };
        sum += m;
        n += 1;
    }
    if !pci_order {
        return min_mib * n;
    }
    sum
}

/// `kvTypeBytes` : octets par élément d'un type de cache.
pub fn kv_type_bytes(t: &str) -> f64 {
    match trim(t).to_lowercase().as_str() {
        "f32" => 4.0,
        "q8_0" => 34.0 / 32.0,
        "q5_1" => 24.0 / 32.0,
        "q5_0" => 22.0 / 32.0,
        "q4_1" => 20.0 / 32.0,
        "q4_0" | "iq4_nl" => 18.0 / 32.0,
        _ => 2.0,
    }
}

/// `ggufStateBytes` : (octets de cache KV par jeton, octets d'état
/// récurrent).
pub fn gguf_state_bytes(g: &GgufMeta, kt: &str, vt: &str) -> (f64, i64) {
    let (mut kl, mut vl) = (g.key_len, g.val_len);
    if g.embedding_length > 0 && g.head_count > 0 {
        if kl == 0 {
            kl = g.embedding_length / g.head_count;
        }
        if vl == 0 {
            vl = g.embedding_length / g.head_count;
        }
    }
    if kl == 0 || vl == 0 || g.block_count == 0 {
        return (0.0, 0);
    }
    let (mut heads, mut rec_layers) = (0i64, 0i64);
    match g.head_count_kv_layers.as_deref() {
        Some(layers) if !layers.is_empty() => {
            for &h in layers {
                heads += h;
                if h == 0 && g.hybrid {
                    rec_layers += 1;
                }
            }
        }
        _ if g.hybrid && g.full_attn_interval > 0 => {
            let attn = g.block_count / g.full_attn_interval;
            heads = attn * g.head_count_kv;
            rec_layers = g.block_count - attn;
        }
        _ => {
            heads = g.block_count * g.head_count_kv;
            if heads == 0 && g.hybrid {
                rec_layers = g.block_count;
            }
        }
    }
    let per_token = heads as f64 * (kl as f64 * kv_type_bytes(kt) + vl as f64 * kv_type_bytes(vt));
    let mut recurrent = 0i64;
    if rec_layers > 0 {
        let conv = if g.ssm_conv > 1 {
            (g.ssm_conv - 1) * (g.ssm_inner + 2 * g.ssm_groups * g.ssm_state)
        } else {
            0
        };
        recurrent = rec_layers * (conv + g.ssm_state * g.ssm_inner) * 4;
    }
    (per_token, recurrent)
}

/// `serveCtx` : contexte que le moteur ouvrira (0 = celui d'entraînement).
pub fn serve_ctx(cfg: &Cfg, extra: &[String], g: Option<&GgufMeta>) -> i64 {
    let mut sv = flag_value(extra, &["-c", "--ctx-size"]);
    if sv.is_empty() {
        sv = trim(get(cfg, "CTX")).to_string();
    }
    if sv.is_empty() {
        sv = "32768".into();
    }
    let mut n = match atoi(&sv) {
        Some(n) if n >= 0 => n,
        _ => return 0,
    };
    if n == 0
        && let Some(g) = g
    {
        n = g.context_length;
    }
    n
}

/// `cpuWeights` : pourquoi des poids (ou le cache KV) vivent en RAM hôte.
pub fn cpu_weights(
    cfg: &Cfg,
    extra: &[String],
    arg_env: &HashMap<String, String>,
    blocks: i64,
) -> String {
    let env = |k: &str| arg_env.get(k).map_or("", String::as_str);
    let mut ot = flag_values(extra, &["-ot", "--override-tensor"]);
    if !env("LLAMA_ARG_OVERRIDE_TENSOR").is_empty() {
        ot.push(env("LLAMA_ARG_OVERRIDE_TENSOR").into());
    }
    for v in &ot {
        for el in v.split(',') {
            if let Some((_, buft)) = el.split_once('=')
                && trim(buft).to_uppercase().starts_with("CPU")
            {
                return "tenseurs sur CPU (-ot)".into();
            }
        }
    }
    if has_any_flag(extra, &["--cpu-moe", "-cmoe"]) || env_truthy(env("LLAMA_ARG_CPU_MOE")) {
        return "experts sur CPU".into();
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
    if !kv_offloaded(extra, arg_env) {
        return "cache KV sur CPU".into();
    }
    let mut ngl = flag_value(extra, &["-ngl", "--n-gpu-layers", "--gpu-layers"]);
    if ngl.is_empty() {
        ngl = trim(get(cfg, "NGL")).to_string();
        if ngl.eq_ignore_ascii_case("auto") {
            ngl = env("LLAMA_ARG_N_GPU_LAYERS").into();
        }
    }
    if let Some(n) = atoi(trim(&ngl))
        && n != 999
        && (blocks == 0 || n < blocks + 1)
    {
        return format!("couches GPU limitées à {n}");
    }
    String::new()
}

/// `cacheRAMAuto` : taille du cache de prompts (0 = défaut du moteur).
pub fn cache_ram_auto(cfg: &Cfg, extra: &[String], si: &SysInfo) -> (i64, String) {
    if si.vram_mib <= 0 {
        return (0, "VRAM NVIDIA inconnue".into());
    }
    let Some(g) = si.gguf.as_ref() else {
        return (0, "métadonnées GGUF illisibles".into());
    };
    if si.ram_mib <= 0 || si.ram_avail_mib <= 0 {
        return (0, "RAM inconnue".into());
    }
    if si.model_bytes <= 0 {
        return (0, "taille du modèle inconnue".into());
    }
    let r = cpu_weights(cfg, extra, &si.arg_env, g.block_count);
    if !r.is_empty() {
        return (0, r);
    }
    let (kt, vt, _) = effective_kv_types(cfg, extra, &si.arg_env);
    let (per_tok, rec) = gguf_state_bytes(g, &kt, &vt);
    let ctx = serve_ctx(cfg, extra, Some(g));
    if per_tok <= 0.0 || ctx <= 0 {
        return (0, "taille d'état inconnue".into());
    }
    let mib = 1048576.0;
    let kv_bytes = per_tok * ctx as f64;
    if si.model_bytes as f64 + kv_bytes > 0.9 * si.vram_mib as f64 * mib {
        return (
            0,
            "modèle trop gros pour la VRAM : des poids restent en RAM".into(),
        );
    }
    let mut state = kv_bytes;
    if rec > 0 {
        state += rec as f64 * ((32i64.min(ctx / 8192 + 1) + 1) as f64);
    }
    let need = (2.5 * state / mib).ceil() as i64;
    let ceiling = (si.ram_mib * 30 / 100).min(si.ram_avail_mib / 2);
    if ceiling <= ENGINE_CACHE_RAM_DEFAULT {
        return (0, format!("RAM libre trop juste (plafond {ceiling} Mio)"));
    }
    if need <= ENGINE_CACHE_RAM_DEFAULT {
        return (0, format!("le défaut suffit (besoin estimé {need} Mio)"));
    }
    (
        need.min(ceiling),
        format!("besoin estimé {need} Mio, plafond {ceiling} Mio"),
    )
}

/// `cacheRAMSetting` : (valeur manuelle, auto, lisible).
pub fn cache_ram_setting(cfg: &Cfg) -> (String, bool, bool) {
    let v = trim(get(cfg, "CACHE_RAM"));
    if v.is_empty() || v.eq_ignore_ascii_case("auto") {
        return (String::new(), true, true);
    }
    match atoi(v) {
        Some(n) if n >= -1 => (n.to_string(), false, true),
        _ => (String::new(), false, false),
    }
}

/// `cacheRAMArgs`.
pub fn cache_ram_args(cfg: &Cfg, extra: &[String], si: &SysInfo) -> (Vec<String>, Vec<String>) {
    let (manual, auto, ok) = cache_ram_setting(cfg);
    if !ok {
        return (
            Vec::new(),
            vec![format!(
                "CACHE_RAM={} illisible (un nombre de Mio, -1, 0 ou auto) : ignoré",
                get(cfg, "CACHE_RAM")
            )],
        );
    }
    let mut notes = Vec::new();
    if !si.help.contains("--cache-ram") {
        if !auto {
            notes.push("ce moteur ne connaît pas --cache-ram : CACHE_RAM ignoré".into());
        }
        return (Vec::new(), notes);
    }
    if has_any_flag(extra, &["-cram", "--cache-ram"]) {
        return (Vec::new(), Vec::new());
    }
    if !si.env("LLAMA_ARG_CACHE_RAM").is_empty() {
        if !auto {
            notes.push("LLAMA_ARG_CACHE_RAM est posée : CACHE_RAM ignoré".into());
        }
        return (Vec::new(), notes);
    }
    if !auto {
        return (vec!["--cache-ram".into(), manual], Vec::new());
    }
    let (n, why) = cache_ram_auto(cfg, extra, si);
    if n <= 0 {
        return (Vec::new(), Vec::new());
    }
    (
        vec!["--cache-ram".into(), n.to_string()],
        vec![format!(
            "cache de prompts : --cache-ram {n} (RAM hôte, copie exacte de l'état, jamais la VRAM ; {why}). CACHE_RAM=<Mio> pour fixer, 0 pour couper."
        )],
    )
}

/// `cacheRAMOff`.
pub fn cache_ram_off(cfg: &Cfg, extra: &[String], arg_env: &HashMap<String, String>) -> bool {
    let mut v = flag_value(extra, &["-cram", "--cache-ram"]);
    if v.is_empty() {
        v = arg_env
            .get("LLAMA_ARG_CACHE_RAM")
            .cloned()
            .unwrap_or_default();
    }
    if v.is_empty() {
        v = cache_ram_setting(cfg).0;
    }
    trim(&v) == "0"
}

/// `cacheIsolationOn`.
pub fn cache_isolation_on(cfg: &Cfg) -> bool {
    !trim(get(cfg, "CACHE_ISOLATE")).eq_ignore_ascii_case("off")
}

/// `loopbackHost`.
pub fn loopback_host(h: &str) -> bool {
    matches!(
        trim(h).trim_matches(['[', ']']),
        "127.0.0.1" | "::1" | "localhost"
    )
}

/// `slotSaveArgs`.
pub fn slot_save_args(cfg: &Cfg, extra: &[String], si: &SysInfo) -> Vec<String> {
    if si.slot_dir.is_empty()
        || !si.help.contains("--slot-save-path")
        || has_any_flag(extra, &["--slot-save-path"])
    {
        return Vec::new();
    }
    let (persist, _) = slot_persist_plan(cfg, extra, si);
    if !persist && (!cache_isolation_on(cfg) || cache_ram_off(cfg, extra, &si.arg_env)) {
        return Vec::new();
    }
    if !super::slot_save_safe(cfg, extra, si) {
        return Vec::new();
    }
    vec!["--slot-save-path".into(), si.slot_dir.clone()]
}
