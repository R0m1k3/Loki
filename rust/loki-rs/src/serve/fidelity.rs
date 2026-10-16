//! Garde-fous de fidélité (`backend_serve_fidelity.go`) : Loki ne choisit
//! jamais un réglage qui change ce que le modèle calcule, mais il DIT ceux
//! que la ligne finale porte (cache KV quantifié, glissement de contexte,
//! réutilisation approchée du cache, acceptation synthétique).

use super::{
    Cfg, SysInfo, env_truthy, flag_value, has_any_flag, has_flag_prefix, serve_kv_types,
    tensor_override, trim,
};
use std::collections::HashMap;

/// `fidelityArgEnv`.
pub const FIDELITY_ARG_ENV: &[&str] = &[
    "LLAMA_ARG_CACHE_TYPE_K",
    "LLAMA_ARG_CACHE_TYPE_V",
    "LLAMA_ARG_CONTEXT_SHIFT",
    "LLAMA_ARG_NO_CONTEXT_SHIFT",
    "LLAMA_ARG_CACHE_REUSE",
    "LLAMA_ARG_MMPROJ",
    "LLAMA_ARG_MMPROJ_URL",
    "LLAMA_ARG_SPEC_SYNTH_LEN",
];

/// `effectiveKVTypes` : (K, V, source la plus forte).
pub fn effective_kv_types(
    cfg: &Cfg,
    extra: &[String],
    arg_env: &HashMap<String, String>,
) -> (String, String, String) {
    let env = |k: &str| arg_env.get(k).cloned().unwrap_or_default();
    let (mut k, mut v) = (env("LLAMA_ARG_CACHE_TYPE_K"), env("LLAMA_ARG_CACHE_TYPE_V"));
    let mut src = String::new();
    if !k.is_empty() || !v.is_empty() {
        src = "défini par LLAMA_ARG_CACHE_TYPE_K/V".into();
    }
    let (ck, cv) = serve_kv_types(cfg);
    if !ck.is_empty() || !cv.is_empty() {
        src = "KV_TYPE".into();
        if !ck.is_empty() {
            k = ck;
        }
        if !cv.is_empty() {
            v = cv;
        }
    }
    let sk = flag_value(extra, &["-ctk", "--cache-type-k"]);
    if !sk.is_empty() {
        k = sk;
        src = "défini par EXTRA_ARGS".into();
    }
    let sv = flag_value(extra, &["-ctv", "--cache-type-v"]);
    if !sv.is_empty() {
        v = sv;
        src = "défini par EXTRA_ARGS".into();
    }
    (k, v, src)
}

/// `kvFidelity` : rang (0 = mêmes calculs que f16) et libellé.
pub fn kv_fidelity(t: &str) -> (i64, &'static str) {
    match trim(t).to_lowercase().as_str() {
        "" | "f16" | "f32" => (0, ""),
        "bf16" => (1, "numérique différente de f16 (mantisse plus courte)"),
        "q8_0" | "q5_0" | "q5_1" => (2, "modifie légèrement les sorties par rapport à f16"),
        "q4_0" | "q4_1" | "iq4_nl" => (3, "perte mesurable par rapport à f16"),
        _ => (2, "modifie les sorties par rapport à f16"),
    }
}

fn or_f16(t: &str) -> &str {
    if t.is_empty() { "f16" } else { t }
}

/// `kvFidelityNote`.
pub fn kv_fidelity_note(
    k: &str,
    v: &str,
    src: &str,
    extra: &[String],
    arg_env: &HashMap<String, String>,
) -> String {
    let (rk, lk) = kv_fidelity(k);
    let (rv, lv) = kv_fidelity(v);
    if rk == 0 && rv == 0 {
        return String::new();
    }
    let label = if rv > rk { lv } else { lk };
    let mut note = format!(
        "cache KV {}/{} ({src}) : {label} — choix de l'utilisateur, laissé tel quel.",
        or_f16(k),
        or_f16(v)
    );
    if !tensor_override(extra, arg_env).is_empty() {
        note.push_str(" Repasser en f16 demande plus de VRAM, et le placement fixé par -ot/--n-cpu-moe empêche --fit de compenser : il faudrait sans doute relever --n-cpu-moe.");
    }
    note
}

/// `lossyCacheNotes`.
pub fn lossy_cache_notes(extra: &[String], si: &SysInfo) -> Vec<String> {
    let mut notes = Vec::new();
    let ignored = if !si.mmproj.is_empty()
        || has_any_flag(extra, &["-mm", "--mmproj", "-mmu", "--mmproj-url"])
        || !si.env("LLAMA_ARG_MMPROJ").is_empty()
        || !si.env("LLAMA_ARG_MMPROJ_URL").is_empty()
    {
        " (vision chargée : llama.cpp l'ignore de toute façon)"
    } else {
        ""
    };
    let (on, src) = context_shift(extra, &si.arg_env, &si.help);
    if on && src.is_empty() {
        notes.push(format!("avertissement : ce llama-server ancien glisse le contexte par défaut — contexte plein, il jette des jetons anciens et le modèle perd une partie de la conversation sans le savoir{ignored}. --no-context-shift dans EXTRA_ARGS l'en empêche."));
    } else if on {
        notes.push(format!("avertissement : --context-shift ({src}) — contexte plein, le moteur jette des jetons anciens et le modèle perd une partie de la conversation sans le savoir{ignored}."));
    }
    let (mut sv, mut src) = (flag_value(extra, &["--cache-reuse"]), "EXTRA_ARGS");
    if sv.is_empty() {
        sv = si.env("LLAMA_ARG_CACHE_REUSE").into();
        src = "LLAMA_ARG_CACHE_REUSE";
    }
    if let Some(n) = super::atoi(trim(&sv))
        && n > 0
    {
        notes.push(format!("avertissement : --cache-reuse {n} ({src}) — réutilise des morceaux de cache calculés sous un autre préfixe : les sorties ne sont plus exactement celles d'un calcul complet{ignored}."));
    }
    let mut f = has_flag_prefix(extra, &["--spec-synth-"]);
    if !f.is_empty() || !si.env("LLAMA_ARG_SPEC_SYNTH_LEN").is_empty() {
        if f.is_empty() {
            f = "LLAMA_ARG_SPEC_SYNTH_LEN".into();
        }
        notes.push(format!("avertissement : {f} — le moteur accepte des jetons du brouillon AU HASARD, sans vérification : les réponses ne sont plus celles du modèle. Réservé aux mesures, retire-le."));
    }
    notes
}

/// `contextShift` : (glisse ?, qui l'a décidé — vide : son défaut).
pub fn context_shift(
    extra: &[String],
    arg_env: &HashMap<String, String>,
    help: &str,
) -> (bool, String) {
    let env = |k: &str| arg_env.get(k).map_or("", String::as_str);
    let (mut on, mut set) = (false, false);
    for a in extra {
        let name = a.split_once('=').map_or(a.as_str(), |(n, _)| n);
        match name {
            "--context-shift" => {
                on = true;
                set = true;
            }
            "--no-context-shift" => {
                on = false;
                set = true;
            }
            _ => {}
        }
    }
    if set {
        return (on, "EXTRA_ARGS".into());
    }
    if help.contains("--no-context-shift") && !help.contains("--context-shift") {
        if env_truthy(env("LLAMA_ARG_NO_CONTEXT_SHIFT")) {
            return (false, "LLAMA_ARG_NO_CONTEXT_SHIFT".into());
        }
        return (true, String::new());
    }
    if env_truthy(env("LLAMA_ARG_CONTEXT_SHIFT")) {
        return (true, "LLAMA_ARG_CONTEXT_SHIFT".into());
    }
    (false, String::new())
}
