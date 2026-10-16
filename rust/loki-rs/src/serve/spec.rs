//! Décodage spéculatif géré par Loki (`backend_serve_spec.go`) : tête MTP,
//! modèle brouillon, n-grammes du contexte. Rien n'y touche au modèle :
//! chaque jeton émis est vérifié par le modèle cible.

use super::{
    Cfg, SysInfo, atoi, cpu_weights, fit_blocker, flag_value, flag_values, get, has_any_flag,
    has_flag_prefix, ngl_forced, op_offload_min_batch_env, trim,
};
use std::collections::HashMap;

pub const SPEC_AUTO_MIN_BUILD: i64 = 11009;
pub const SPEC_N_MAX_LIMIT: i64 = 64;
pub const SPEC_NGRAM_N_MATCH: i64 = 24;
pub const SPEC_NGRAM_N_MIN: i64 = 48;
pub const SPEC_NGRAM_N_MAX: i64 = 64;
pub const GGML_OFFLOAD_MIN_BATCH_DEFAULT: i64 = 32;

/// `specArgEnv`.
pub const SPEC_ARG_ENV: &[&str] = &[
    "LLAMA_ARG_SPEC_TYPE",
    "LLAMA_ARG_SPEC_DRAFT_MODEL",
    "LLAMA_ARG_SPEC_DRAFT_HF_REPO",
    "LLAMA_ARG_SPEC_DRAFT_N_MAX",
    "LLAMA_ARG_SPEC_DRAFT_SAMPLING",
];

const SPEC_USER_FLAGS: &[&str] = &[
    "--spec-type",
    "--spec_type",
    "-md",
    "--model-draft",
    "--spec-draft-model",
    "-hfd",
    "-hfrd",
    "--spec-draft-hf",
    "--hf-repo-draft",
    "--spec-default",
];
const SPEC_USER_PREFIXES: &[&str] = &["--spec-ngram-", "--spec_ngram_"];

/// `specSidecarArch`.
pub fn spec_sidecar_arch(arch: &str) -> bool {
    matches!(trim(arch).to_lowercase().as_str(), "eagle3" | "dflash")
}

/// `specMode` : "off", "auto", "mtp", "ngram", "mtp+ngram", ou "" illisible.
pub fn spec_mode(cfg: &Cfg) -> &'static str {
    match trim(get(cfg, "SPEC")).to_lowercase().as_str() {
        "" | "off" | "non" | "no" | "0" => "off",
        "auto" => "auto",
        "mtp" => "mtp",
        "ngram" => "ngram",
        "mtp+ngram" | "ngram+mtp" => "mtp+ngram",
        _ => "",
    }
}

/// `specUserSet`.
pub fn spec_user_set(extra: &[String], arg_env: &HashMap<String, String>) -> bool {
    if has_any_flag(extra, SPEC_USER_FLAGS)
        || !has_flag_prefix(extra, SPEC_USER_PREFIXES).is_empty()
    {
        return true;
    }
    [
        "LLAMA_ARG_SPEC_TYPE",
        "LLAMA_ARG_SPEC_DRAFT_MODEL",
        "LLAMA_ARG_SPEC_DRAFT_HF_REPO",
    ]
    .iter()
    .any(|k| arg_env.get(*k).is_some_and(|v| !v.is_empty()))
}

/// `helpSupportsMTP`.
pub fn help_supports_mtp(help: &str) -> bool {
    help.contains("draft-mtp") && help.contains("--spec-draft-n-max")
}

/// `ctxFixed` : (contexte vu par le moteur, chiffré > 0 ?).
pub fn ctx_fixed(cfg: &Cfg, extra: &[String]) -> (String, bool) {
    let mut c = flag_value(extra, &["-c", "--ctx-size"]);
    if c.is_empty() {
        c = trim(get(cfg, "CTX")).to_string();
    }
    if c.is_empty() {
        c = "32768".into();
    }
    let ok = atoi(&c).is_some_and(|n| n > 0);
    (c, ok)
}

/// `statefulSampler`.
pub fn stateful_sampler(extra: &[String]) -> String {
    for v in flag_values(extra, &["--mirostat"]) {
        let v = trim(&v);
        if !v.is_empty() && v != "0" {
            return format!("--mirostat {v}");
        }
    }
    for v in flag_values(extra, &["--samplers"]) {
        if v.replace('-', "_").to_lowercase().contains("adaptive_p") {
            return "--samplers adaptive-p".into();
        }
    }
    for v in flag_values(extra, &["--sampling-seq", "--sampler-seq"]) {
        if v.contains('a') {
            return "--sampling-seq avec adaptive-p".into();
        }
    }
    String::new()
}

/// `specAutoBlocker`.
pub fn spec_auto_blocker(cfg: &Cfg, extra: &[String], si: &SysInfo) -> String {
    if si
        .gguf
        .as_ref()
        .is_some_and(|g| g.arch.eq_ignore_ascii_case("qwen4exp"))
    {
        return "qwen4exp : MTP trop récent pour ce moteur, SPEC=mtp pour l'imposer".into();
    }
    let why = fit_blocker(cfg, extra, si);
    if !why.is_empty() {
        return format!("placement manuel ({why}) — --fit ne compte pas la VRAM du brouillon");
    }
    if has_any_flag(extra, &["-dev", "--device"]) || !si.env("LLAMA_ARG_DEVICE").is_empty() {
        return "placement manuel (--device) — --fit ne compte pas la VRAM du brouillon".into();
    }
    if !ctx_fixed(cfg, extra).1 {
        return "contexte non chiffré — fit pourrait le réduire pour loger le brouillon".into();
    }
    if !si.mmproj.is_empty() || has_any_flag(extra, &["--mmproj", "-mm"]) {
        return "projecteur vision chargé".into();
    }
    if si.engine_build < SPEC_AUTO_MIN_BUILD {
        if si.engine_build <= 0 {
            return "build du moteur inconnu (compilé ou fork)".into();
        }
        return format!(
            "moteur b{}, b{SPEC_AUTO_MIN_BUILD} au moins",
            si.engine_build
        );
    }
    si.spec_auto_blocked.clone()
}

/// Drapeaux, notes, et « vient de SPEC=auto ».
#[derive(Clone, Debug, Default)]
pub struct SpecOut {
    pub args: Vec<String>,
    pub notes: Vec<String>,
    pub auto: bool,
}

fn base_name(p: &str) -> String {
    crate::paths::base_name(p)
}

/// `specArgs`.
pub fn spec_args(cfg: &Cfg, extra: &[String], si: &SysInfo) -> SpecOut {
    if si.draft_is_model && !trim(get(cfg, "MODEL_DRAFT")).is_empty() {
        let mut c = cfg.clone();
        c.remove("MODEL_DRAFT");
        let mut out = spec_args(&c, extra, si);
        out.notes.insert(0, "MODEL_DRAFT est le modèle principal lui-même : ignoré — la tête MTP intégrée sert de brouillon sans recharger les poids (retire MODEL_DRAFT du preset)".into());
        return out;
    }
    let mode = spec_mode(cfg);
    let draft_key = trim(get(cfg, "MODEL_DRAFT"));
    match mode {
        "" => {
            return SpecOut {
                notes: vec![format!(
                    "SPEC={} illisible (off, auto, mtp, ngram ou mtp+ngram) : sans décodage spéculatif",
                    get(cfg, "SPEC")
                )],
                ..SpecOut::default()
            };
        }
        "off" => {
            let mut out = SpecOut::default();
            if !draft_key.is_empty() {
                out.notes
                    .push("MODEL_DRAFT ignoré : SPEC=off (auto ou mtp pour s'en servir)".into());
            }
            return out;
        }
        _ => {}
    }
    if spec_user_set(extra, &si.arg_env) {
        return SpecOut {
            notes: vec![
                "décodage spéculatif déjà réglé dans EXTRA_ARGS ou LLAMA_ARG_* : SPEC ignoré"
                    .into(),
            ],
            ..SpecOut::default()
        };
    }
    let skip = |why: &str| SpecOut {
        notes: vec![format!("SPEC={mode} : {why} — sans décodage spéculatif")],
        ..SpecOut::default()
    };

    if mode == "ngram" || mode == "mtp+ngram" {
        let why = ngram_blocker(cfg, extra, si, mode);
        if !why.is_empty() {
            return skip(&why);
        }
    }
    if mode == "ngram" {
        let (args, notes) = ngram_spec_args(cfg, extra, si, "");
        return SpecOut {
            args,
            notes,
            auto: false,
        };
    }

    let (src, mtp, why, quiet) = spec_source(mode, draft_key, si);
    if mode == "mtp+ngram" && !mtp {
        let why = if why.is_empty() {
            "MODEL_DRAFT n'est pas une tête MTP".to_string()
        } else {
            why
        };
        let (args, notes) = ngram_spec_args(
            cfg,
            extra,
            si,
            &format!("SPEC=mtp+ngram : sans MTP ({why}) → n-grammes seuls. "),
        );
        return SpecOut {
            args,
            notes,
            auto: false,
        };
    }
    if !why.is_empty() {
        return skip(&why);
    }
    if quiet {
        return SpecOut::default();
    }
    let mut args = src;
    if mode == "mtp+ngram" {
        if let Some(last) = args.last_mut() {
            *last = "draft-mtp,ngram-mod".into();
        }
        args.extend(ngram_mod_params());
    }
    let mut notes: Vec<String> = Vec::new();
    let mut auto = false;
    let label = if mtp {
        "tête MTP".to_string()
    } else {
        format!("brouillon {}", base_name(&si.draft))
    };
    if mode == "auto" {
        let why = spec_auto_blocker(cfg, extra, si);
        if !why.is_empty() {
            return skip(&why);
        }
        auto = true;
        notes.push(format!("SPEC=auto → {label} : sortie inchangée (chaque jeton est vérifié par le modèle), ~1-2 Go de VRAM en plus. Mesure le prefill ; SPEC=off pour couper."));
    } else {
        let mut note =
            format!("SPEC={mode} → {label} imposé : sortie inchangée, ~1-2 Go de VRAM en plus");
        if mode == "mtp+ngram" {
            note.push_str(
                " ; n-grammes en plus (ngram-mod, prioritaires quand ils trouvent une répétition)",
            );
        }
        let mut why = fit_blocker(cfg, extra, si);
        if !why.is_empty() || has_any_flag(extra, &["-dev", "--device"]) {
            if why.is_empty() {
                why = "--device".into();
            }
            note.push_str(&format!(" ; placement manuel ({why}) : --fit ne compte pas le brouillon, place-le avec -devd / -ngld si la VRAM manque"));
        }
        if !ctx_fixed(cfg, extra).1 {
            note.push_str(
                " ; contexte non chiffré : fit pourrait le réduire pour loger le brouillon",
            );
        }
        if si
            .gguf
            .as_ref()
            .is_some_and(|g| g.arch.eq_ignore_ascii_case("qwen4exp"))
        {
            note.push_str(" ; MTP qwen4exp très récent");
        }
        notes.push(note);
        if mode == "mtp+ngram" {
            notes.extend(ngram_cost_notes(cfg, extra, si));
        }
    }

    let v = trim(get(cfg, "SPEC_N_MAX"));
    if !v.is_empty() {
        match atoi(v) {
            Some(n) if (1..=SPEC_N_MAX_LIMIT).contains(&n) => {
                if !si.help.contains("--spec-draft-n-max") {
                    notes.push(
                        "ce moteur ne connaît pas --spec-draft-n-max : SPEC_N_MAX ignoré".into(),
                    );
                } else if has_any_flag(extra, &["--spec-draft-n-max"])
                    || !si.env("LLAMA_ARG_SPEC_DRAFT_N_MAX").is_empty()
                {
                } else {
                    args.push("--spec-draft-n-max".into());
                    args.push(n.to_string());
                }
            }
            _ => notes.push(format!(
                "SPEC_N_MAX={v} illisible (1 à 64 jetons) : défaut du moteur"
            )),
        }
    }

    let mut sampling = "greedy".to_string();
    let sv = trim(get(cfg, "SPEC_SAMPLING")).to_lowercase();
    match sv.as_str() {
        "" | "greedy" => {}
        "probabilistic" => {
            let st = stateful_sampler(extra);
            if mode == "auto" {
                notes.push("SPEC_SAMPLING=probabilistic ignoré avec SPEC=auto (SPEC=mtp pour le choisir) : greedy".into());
            } else if !st.is_empty() {
                notes.push(format!("SPEC_SAMPLING=probabilistic refusé avec {st} (cet échantillonneur avancerait sur des jetons rejetés) : greedy"));
            } else if !si.help.contains("probabilistic") {
                notes.push(
                    "ce moteur ne connaît pas --spec-draft-sampling probabilistic : greedy".into(),
                );
            } else {
                sampling = sv.clone();
            }
        }
        _ => notes.push(format!(
            "SPEC_SAMPLING={sv} illisible (greedy ou probabilistic) : greedy"
        )),
    }
    if si.help.contains("--spec-draft-sampling")
        && !has_any_flag(extra, &["--spec-draft-sampling"])
        && si.env("LLAMA_ARG_SPEC_DRAFT_SAMPLING").is_empty()
    {
        args.push("--spec-draft-sampling".into());
        args.push(sampling);
    }
    SpecOut { args, notes, auto }
}

/// `specSource` : (drapeaux, tête MTP ?, pourquoi aucun, rien à dire).
pub fn spec_source(mode: &str, draft_key: &str, si: &SysInfo) -> (Vec<String>, bool, String, bool) {
    let none = |w: String| (Vec::new(), false, w, false);
    if !draft_key.is_empty() {
        if si.draft.is_empty() {
            let mut why = format!("MODEL_DRAFT={draft_key} introuvable");
            if !si.draft_err.is_empty() {
                why.push_str(&format!(" ({})", si.draft_err));
            }
            return none(why);
        }
        let Some(dg) = si.draft_gguf.as_ref() else {
            return none(
                "MODEL_DRAFT illisible (GGUF incomplet ou en cours de téléchargement ?)".into(),
            );
        };
        if dg.has_next_n_tensor {
            if !help_supports_mtp(&si.help) {
                return none("ce moteur ne connaît pas draft-mtp".into());
            }
            return (
                vec![
                    "-md".into(),
                    si.draft.clone(),
                    "--spec-type".into(),
                    "draft-mtp".into(),
                ],
                true,
                String::new(),
                false,
            );
        }
        if spec_sidecar_arch(&dg.arch) {
            return none(format!(
                "MODEL_DRAFT est une tête {}, pas un modèle brouillon : règle -md et --spec-type dans EXTRA_ARGS",
                dg.arch
            ));
        }
        if mode == "mtp+ngram" {
            return none(String::new());
        }
        if !si.help.contains("--spec-type") || !si.help.contains("draft-simple") {
            return none("ce moteur ne connaît pas --spec-type draft-simple".into());
        }
        return (
            vec![
                "-md".into(),
                si.draft.clone(),
                "--spec-type".into(),
                "draft-simple".into(),
            ],
            false,
            String::new(),
            false,
        );
    }
    let Some(g) = si.gguf.as_ref() else {
        return none("métadonnées du modèle illisibles".into());
    };
    if !g.has_next_n_tensor {
        if mode != "auto" || g.next_n > 0 {
            return none(
                "pas de tête MTP dans ce fichier (publiée à part ? MODEL_DRAFT=mtp-….gguf)".into(),
            );
        }
        return (Vec::new(), false, String::new(), true);
    }
    if !help_supports_mtp(&si.help) {
        return none("ce moteur ne connaît pas draft-mtp".into());
    }
    (
        vec!["--spec-type".into(), "draft-mtp".into()],
        true,
        String::new(),
        false,
    )
}

/// `ngramModParams`.
pub fn ngram_mod_params() -> Vec<String> {
    vec![
        "--spec-ngram-mod-n-match".into(),
        SPEC_NGRAM_N_MATCH.to_string(),
        "--spec-ngram-mod-n-min".into(),
        SPEC_NGRAM_N_MIN.to_string(),
        "--spec-ngram-mod-n-max".into(),
        SPEC_NGRAM_N_MAX.to_string(),
    ]
}

/// `ngramBlocker`.
pub fn ngram_blocker(cfg: &Cfg, extra: &[String], si: &SysInfo, mode: &str) -> String {
    if !si.help.contains("--spec-ngram-mod-n-match") {
        return "ce moteur ne connaît pas --spec-ngram-mod-n-match".into();
    }
    if mode == "ngram" {
        let f = has_flag_prefix(extra, &["--spec-draft-", "--spec_draft_"]);
        if !f.is_empty() {
            return format!("{f} dans EXTRA_ARGS (brouillon réglé à la main)");
        }
    }
    let r = cpu_weights(cfg, extra, &si.arg_env, si.blocks());
    if !r.is_empty() {
        let thr = op_offload_threshold(cfg, si);
        if SPEC_NGRAM_N_MAX + 1 >= thr {
            return format!(
                "{r} : un lot de vérification de {} jetons atteint GGML_OP_OFFLOAD_MIN_BATCH ({thr}) et recopierait ces poids vers le GPU à chaque brouillon — OP_OFFLOAD_MIN_BATCH=128 pour l'essayer, à mesurer",
                SPEC_NGRAM_N_MAX + 1
            );
        }
    }
    String::new()
}

/// `opOffloadThreshold`.
pub fn op_offload_threshold(cfg: &Cfg, si: &SysInfo) -> i64 {
    let mut v = trim(si.uenv("GGML_OP_OFFLOAD_MIN_BATCH")).to_string();
    if v.is_empty() {
        v = op_offload_min_batch_env(cfg, si).0;
    }
    match atoi(&v) {
        Some(n) if n > 0 => n,
        _ => GGML_OFFLOAD_MIN_BATCH_DEFAULT,
    }
}

/// `ngramSpecArgs`.
pub fn ngram_spec_args(
    cfg: &Cfg,
    extra: &[String],
    si: &SysInfo,
    lead: &str,
) -> (Vec<String>, Vec<String>) {
    let mut args = vec!["--spec-type".to_string(), "ngram-mod".to_string()];
    args.extend(ngram_mod_params());
    let mut notes = vec![format!(
        "{lead}SPEC=ngram → n-grammes du contexte (ngram-mod, brouillons de {SPEC_NGRAM_N_MIN} à {SPEC_NGRAM_N_MAX} jetons) : distribution inchangée (chaque jeton vérifié), ~16 Mo de RAM ; utile surtout en mode code, à mesurer (acceptation par nature dans /api/perf/summary)"
    )];
    if lead.is_empty() {
        if !trim(get(cfg, "MODEL_DRAFT")).is_empty() {
            notes.push("MODEL_DRAFT ignoré avec SPEC=ngram : SPEC=mtp+ngram pour les deux".into());
        } else if si.gguf.as_ref().is_some_and(|g| g.has_next_n_tensor) {
            notes.push(
                "tête MTP du modèle inutilisée avec SPEC=ngram : SPEC=mtp+ngram pour les deux"
                    .into(),
            );
        }
    }
    if !trim(get(cfg, "SPEC_N_MAX")).is_empty() || !trim(get(cfg, "SPEC_SAMPLING")).is_empty() {
        notes.push(
            "SPEC_N_MAX et SPEC_SAMPLING ignorés : les n-grammes n'ont pas de brouillon à régler"
                .into(),
        );
    }
    notes.extend(ngram_cost_notes(cfg, extra, si));
    (args, notes)
}

/// `ngramCostNotes`.
pub fn ngram_cost_notes(cfg: &Cfg, extra: &[String], si: &SysInfo) -> Vec<String> {
    let mut notes = Vec::new();
    if si.gguf.as_ref().is_some_and(|g| g.hybrid) {
        notes.push("modèle hybride : chaque brouillon n-gramme copie l'état récurrent (point de reprise en RAM hôte) et le recharge s'il est rejeté — compare le temps par tour, pas seulement le débit".into());
    }
    if !cpu_weights(cfg, extra, &si.arg_env, si.blocks()).is_empty()
        && si.model_bytes > 0
        && si.ram_mib > 0
        && si.model_bytes > si.ram_mib << 20
    {
        notes.push("modèle plus gros que la RAM, poids sur CPU : un lot de vérification lit la plupart des experts de chaque couche — surveille les lectures disque, pas seulement les jetons/s".into());
    }
    if ngl_forced(cfg, extra) {
        notes.push(format!("NGL imposé (--fit inactif) : les logits de {} positions par slot prennent quelques dizaines de Mio de VRAM en plus", SPEC_NGRAM_N_MAX + 1));
    }
    notes
}

// --- Jeton de tentative ------------------------------------------------------

pub const SPEC_ATTEMPT_KEY: &str = "spec_auto_attempt";
pub const SPEC_FAILED_KEY: &str = "spec_auto_failed";
pub const SPEC_FAILED_MAX: usize = 32;

/// `specAutoMark` : une combinaison preset × moteur.
#[derive(Clone, Debug, Default, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct SpecAutoMark {
    #[serde(default)]
    pub fp: String,
    #[serde(default)]
    pub bin: String,
    #[serde(default)]
    pub build: i64,
}

impl SpecAutoMark {
    pub fn key(&self) -> String {
        format!("{}|{}|{}", self.fp, self.bin, self.build)
    }
}

/// `specAutoVerdict` : (raison de couper l'auto, inscrire l'échec ?).
pub fn spec_auto_verdict(
    cur: &SpecAutoMark,
    attempt: Option<&SpecAutoMark>,
    failed: &HashMap<String, String>,
) -> (String, bool) {
    if let Some(w) = failed.get(&cur.key()).filter(|w| !w.is_empty()) {
        return (w.clone(), false);
    }
    if attempt.is_some_and(|a| a.key() == cur.key()) {
        return (
            "le dernier lancement avec cette configuration n'a jamais répondu".into(),
            true,
        );
    }
    (String::new(), false)
}
