//! Optimiseur sans perte — la partie PURE (port de `backend_tune.go`) : ce
//! qu'on a le droit de changer, les essais à tenter, la comparaison des
//! lignes de commande, le score, la réécriture du preset.
//!
//! Seuls des réglages d'ordonnancement bougent (placement, lots, threads,
//! files CUDA ; spéculation et graphes CUDA sur demande) : jamais le modèle,
//! le contexte, les types du cache KV, le raisonnement, l'échantillonnage,
//! ni un drapeau d'EXTRA_ARGS hors de la liste blanche. Un essai dont la
//! ligne de commande diffère de la référence ailleurs que sur les drapeaux
//! réglés est écarté (« dénature ») avant même d'être chargé.

use std::collections::{BTreeMap, BTreeSet, HashMap};

use serde::{Deserialize, Serialize};

use crate::bench::{BenchResult, ser_go_f64};
use crate::models::split_args;
use crate::presets_api::join_args;
use crate::serve::{SysInfo, flag_value, has_any_flag};

pub type Cfg = HashMap<String, String>;

/// `tuneTrialEnv` : le dossier de l'essai, où `loki serve` lit sa
/// configuration.
pub const TRIAL_ENV: &str = "LOKI_TUNE_TRIAL";
/// `tuneDryRunEnv` : `loki serve` compose la ligne, l'écrit, et s'arrête.
pub const DRY_RUN_ENV: &str = "LOKI_TUNE_DRYRUN";

/// `tuneKeys` : les clés que l'optimiseur peut écrire.
pub const KEYS: [&str; 8] = [
    "BATCH",
    "UBATCH",
    "THREADS",
    "THREADS_BATCH",
    "FIT_TARGET",
    "OP_OFFLOAD_MIN_BATCH",
    "CUDA_LAUNCH_QUEUES",
    "EXTRA_ARGS",
];

/// `tuneOptInKeys`.
pub const OPT_IN_KEYS: [&str; 3] = ["SPEC", "SPEC_N_MAX", "CUDA_GRAPH_OPT"];

/// `tuneNeverKeys` : ce que l'optimiseur ne touche jamais.
pub const NEVER_KEYS: [&str; 27] = [
    "MODEL",
    "CTX",
    "KV_TYPE",
    "KV_TYPE_K",
    "KV_TYPE_V",
    "MMPROJ",
    "REASONING",
    "REASONING_BUDGET",
    "REASONING_EFFORT",
    "REASONING_PRESERVE",
    "REASONING_ECHO",
    "TEMP",
    "TOP_P",
    "TOP_K",
    "MIN_P",
    "PRESENCE_PENALTY",
    "REPEAT_PENALTY",
    "PARALLEL",
    "SIDE_SLOT",
    "MODEL_DRAFT",
    "SPEC_SAMPLING",
    "NGL",
    "SPLIT_MODE",
    "CUDA_VISIBLE_DEVICES",
    "BIN",
    "HOST",
    "PORT",
];

fn is_key(k: &str) -> bool {
    KEYS.contains(&k)
}

fn is_opt_in_key(k: &str) -> bool {
    OPT_IN_KEYS.contains(&k)
}

/// `tuneFlag` : un drapeau d'EXTRA_ARGS que l'optimiseur peut retirer ou
/// régler.
pub struct Flag {
    pub names: &'static [&'static str],
    /// 0 = interrupteur, 1 = une valeur.
    pub arity: usize,
    pub opt_in: bool,
}

/// `tuneFlags` : la liste blanche.
pub const FLAGS: [Flag; 14] = [
    Flag {
        names: &["-ot", "--override-tensor"],
        arity: 1,
        opt_in: false,
    },
    Flag {
        names: &["-ncmoe", "--n-cpu-moe"],
        arity: 1,
        opt_in: false,
    },
    Flag {
        names: &["-cmoe", "--cpu-moe"],
        arity: 0,
        opt_in: false,
    },
    Flag {
        names: &["-sm", "--split-mode"],
        arity: 1,
        opt_in: false,
    },
    Flag {
        names: &["-ts", "--tensor-split"],
        arity: 1,
        opt_in: false,
    },
    Flag {
        names: &["-mg", "--main-gpu"],
        arity: 1,
        opt_in: false,
    },
    Flag {
        names: &["-fit", "--fit"],
        arity: 1,
        opt_in: false,
    },
    Flag {
        names: &["-fitt", "--fit-target"],
        arity: 1,
        opt_in: false,
    },
    Flag {
        names: &["-b", "--batch-size"],
        arity: 1,
        opt_in: false,
    },
    Flag {
        names: &["-ub", "--ubatch-size"],
        arity: 1,
        opt_in: false,
    },
    Flag {
        names: &["-t", "--threads"],
        arity: 1,
        opt_in: false,
    },
    Flag {
        names: &["-tb", "--threads-batch"],
        arity: 1,
        opt_in: false,
    },
    Flag {
        names: &["--spec-draft-n-max", "--draft-max", "--draft-n"],
        arity: 1,
        opt_in: true,
    },
    Flag {
        names: &["-bs", "--backend-sampling"],
        arity: 0,
        opt_in: true,
    },
];

/// `tuneForbiddenFlags` : jamais ajoutés, retirés ni modifiés.
pub const FORBIDDEN_FLAGS: [&str; 34] = [
    "-ctk",
    "--cache-type-k",
    "-ctv",
    "--cache-type-v",
    "-fa",
    "--flash-attn",
    "-c",
    "--ctx-size",
    "--temp",
    "--top-k",
    "--top-p",
    "--min-p",
    "--samplers",
    "--repeat-penalty",
    "--repeat-last-n",
    "--dry-multiplier",
    "--reasoning",
    "--reasoning-budget",
    "--reasoning-format",
    "--chat-template",
    "--chat-template-file",
    "--jinja",
    "--cache-reuse",
    "--context-shift",
    "--swa-full",
    "-kvu",
    "--kv-unified",
    "--keep",
    "-np",
    "--parallel",
    "-m",
    "--model",
    "--mmproj",
    "-ngl",
];

/// `tuneFlagFor`.
pub fn flag_for(name: &str, opt_in: bool) -> Option<&'static Flag> {
    FLAGS
        .iter()
        .filter(|f| opt_in || !f.opt_in)
        .find(|f| f.names.contains(&name))
}

// ---------------------------------------------------------------------------
// EXTRA_ARGS jeton par jeton
// ---------------------------------------------------------------------------

/// `argSpan` : un argument et sa place exacte dans la chaîne.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ArgSpan {
    pub start: usize,
    pub end: usize,
    pub val: String,
}

/// `argSpans` : découpe comme `splitArgs`, positions gardées.
pub fn arg_spans(s: &str) -> Vec<ArgSpan> {
    let mut out = Vec::new();
    let mut cur = String::new();
    let mut quote: Option<char> = None;
    let mut start: Option<usize> = None;
    let flush =
        |out: &mut Vec<ArgSpan>, cur: &mut String, start: &mut Option<usize>, end: usize| {
            if !cur.is_empty() {
                out.push(ArgSpan {
                    start: start.unwrap_or(0),
                    end,
                    val: std::mem::take(cur),
                });
            }
            *start = None;
        };
    for (i, r) in s.char_indices() {
        match quote {
            Some(q) => {
                if r == q {
                    quote = None;
                    if cur.is_empty() {
                        // « "" » : un argument vide explicite.
                        out.push(ArgSpan {
                            start: start.unwrap_or(i),
                            end: i + 1,
                            val: String::new(),
                        });
                        start = None;
                    }
                } else {
                    cur.push(r);
                }
            }
            None if r == '"' || r == '\'' => {
                if start.is_none() {
                    start = Some(i);
                }
                quote = Some(r);
            }
            None if matches!(r, ' ' | '\t' | '\n' | '\r') => {
                flush(&mut out, &mut cur, &mut start, i);
            }
            None => {
                if start.is_none() {
                    start = Some(i);
                }
                cur.push(r);
            }
        }
    }
    flush(&mut out, &mut cur, &mut start, s.len());
    out
}

/// `extraFlagSpan` : une occurrence d'un drapeau, avec sa valeur éventuelle.
#[derive(Clone, Debug)]
pub struct FlagSpan {
    pub name: String,
    pub flag: ArgSpan,
    pub value: Option<ArgSpan>,
    pub eq_val: String,
    pub has_eq: bool,
    pub tokens: Vec<ArgSpan>,
}

/// `extraFlags`.
pub fn extra_flags(spans: &[ArgSpan], arity: usize, names: &[&str]) -> Vec<FlagSpan> {
    let mut out = Vec::new();
    let mut i = 0;
    while i < spans.len() {
        let (name, val, has_eq) = match spans[i].val.split_once('=') {
            Some((n, v)) => (n.to_string(), v.to_string(), true),
            None => (spans[i].val.clone(), String::new(), false),
        };
        if !names.contains(&name.as_str()) {
            i += 1;
            continue;
        }
        let mut f = FlagSpan {
            name,
            flag: spans[i].clone(),
            value: None,
            eq_val: val,
            has_eq,
            tokens: vec![spans[i].clone()],
        };
        if arity == 1 && !has_eq && i + 1 < spans.len() {
            f.value = Some(spans[i + 1].clone());
            f.tokens.push(spans[i + 1].clone());
            i += 1;
        }
        out.push(f);
        i += 1;
    }
    out
}

/// `spliceEdit` : un morceau remplacé (`repl` vide = retiré).
#[derive(Clone, Debug)]
pub struct Edit {
    pub start: usize,
    pub end: usize,
    pub repl: String,
}

/// `applyEdits` : remplacements disjoints ; un retrait emporte les blancs
/// qui le précèdent (ou, en tête, ceux qui le suivent).
pub fn apply_edits(s: &str, mut edits: Vec<Edit>) -> String {
    if edits.is_empty() {
        return s.to_string();
    }
    edits.sort_by_key(|e| e.start);
    let b = s.as_bytes();
    let mut out = String::new();
    let mut last = 0;
    for e in edits {
        let (mut start, mut end) = (e.start, e.end);
        if e.repl.is_empty() {
            let mut ws = start;
            while ws > last && (b[ws - 1] == b' ' || b[ws - 1] == b'\t') {
                ws -= 1;
            }
            if ws == start || ws == 0 {
                while end < b.len() && (b[end] == b' ' || b[end] == b'\t') {
                    end += 1;
                }
                if ws == 0 {
                    start = 0;
                }
            } else {
                start = ws;
            }
        }
        if start < last {
            start = last;
        }
        out.push_str(&s[last..start]);
        out.push_str(&e.repl);
        last = end;
    }
    out.push_str(&s[last..]);
    out.trim().to_string()
}

/// `quoteArg`.
pub fn quote_arg(a: &str) -> String {
    join_args(&[a.to_string()])
}

/// `tuneExtraSet` : la DERNIÈRE occurrence voit sa valeur remplacée sur
/// place ; absent, le drapeau est ajouté ; `val` vide retire tout.
pub fn extra_set(s: &str, names: &[&str], val: &str) -> String {
    let occ = extra_flags(&arg_spans(s), 1, names);
    if val.is_empty() {
        let edits = occ
            .iter()
            .flat_map(|o| o.tokens.iter())
            .map(|t| Edit {
                start: t.start,
                end: t.end,
                repl: String::new(),
            })
            .collect();
        return apply_edits(s, edits);
    }
    let Some(o) = occ.last() else {
        let add = format!("{} {}", names[0], quote_arg(val));
        if s.trim().is_empty() {
            return add;
        }
        return format!("{} {add}", s.trim_end_matches([' ', '\t']));
    };
    if o.has_eq {
        return apply_edits(
            s,
            vec![Edit {
                start: o.flag.start,
                end: o.flag.end,
                repl: format!("{}={}", o.name, quote_arg(val)),
            }],
        );
    }
    if let Some(v) = &o.value {
        return apply_edits(
            s,
            vec![Edit {
                start: v.start,
                end: v.end,
                repl: quote_arg(val),
            }],
        );
    }
    format!(
        "{} {}{}",
        &s[..o.flag.end],
        quote_arg(val),
        &s[o.flag.end..]
    )
}

/// `tuneExtraDrop` : retire toutes les occurrences (valeur comprise).
pub fn extra_drop(s: &str, arity: usize, names: &[&str]) -> String {
    let edits = extra_flags(&arg_spans(s), arity, names)
        .iter()
        .flat_map(|o| o.tokens.clone())
        .map(|t| Edit {
            start: t.start,
            end: t.end,
            repl: String::new(),
        })
        .collect();
    apply_edits(s, edits)
}

/// `tuneExtraAddSwitch`.
pub fn extra_add_switch(s: &str, name: &str) -> String {
    if s.trim().is_empty() {
        return name.to_string();
    }
    format!("{} {name}", s.trim_end_matches([' ', '\t']))
}

/// `tuneExtraDropExperts` : retire le placement MANUEL des experts ; rend
/// aussi ce qui a été retiré.
pub fn extra_drop_experts(s: &str) -> (String, Vec<String>) {
    let spans = arg_spans(s);
    let mut edits = Vec::new();
    let mut removed = Vec::new();
    for o in extra_flags(&spans, 1, &["-ot", "--override-tensor"]) {
        let v = if o.has_eq {
            o.eq_val.clone()
        } else {
            match &o.value {
                Some(v) => v.val.clone(),
                None => continue,
            }
        };
        let mut rest = Vec::new();
        let mut hit = false;
        for el in v.split(',') {
            let el = el.trim();
            let pat = el.split_once('=').map_or(el, |(p, _)| p);
            if el.is_empty() {
            } else if crate::serve::moe::ot_targets_experts(pat) {
                removed.push(format!("{} {el}", o.name));
                hit = true;
            } else {
                rest.push(el.to_string());
            }
        }
        if !hit {
            continue;
        }
        if rest.is_empty() {
            for t in &o.tokens {
                edits.push(Edit {
                    start: t.start,
                    end: t.end,
                    repl: String::new(),
                });
            }
        } else if o.has_eq {
            edits.push(Edit {
                start: o.flag.start,
                end: o.flag.end,
                repl: format!("{}={}", o.name, quote_arg(&rest.join(","))),
            });
        } else if let Some(v) = &o.value {
            edits.push(Edit {
                start: v.start,
                end: v.end,
                repl: quote_arg(&rest.join(",")),
            });
        }
    }
    for o in extra_flags(&spans, 1, &["-ncmoe", "--n-cpu-moe"]) {
        let v = match &o.value {
            Some(v) => v.val.clone(),
            None => o.eq_val.clone(),
        };
        removed.push(format!("{} {v}", o.name));
        for t in &o.tokens {
            edits.push(Edit {
                start: t.start,
                end: t.end,
                repl: String::new(),
            });
        }
    }
    for o in extra_flags(&spans, 0, &["-cmoe", "--cpu-moe"]) {
        removed.push(o.name.clone());
        edits.push(Edit {
            start: o.flag.start,
            end: o.flag.end,
            repl: String::new(),
        });
    }
    (apply_edits(s, edits), removed)
}

// ---------------------------------------------------------------------------
// Réglages : par la clé, ou par le drapeau qui l'écrase
// ---------------------------------------------------------------------------

/// `tuneKnob`.
#[derive(Clone, Copy, Debug)]
pub struct Knob {
    pub key: &'static str,
    pub flags: &'static [&'static str],
}

pub const KNOB_BATCH: Knob = Knob {
    key: "BATCH",
    flags: &["-b", "--batch-size"],
};
pub const KNOB_UBATCH: Knob = Knob {
    key: "UBATCH",
    flags: &["-ub", "--ubatch-size"],
};
pub const KNOB_THREADS: Knob = Knob {
    key: "THREADS",
    flags: &["-t", "--threads"],
};
pub const KNOB_FIT_TARGET: Knob = Knob {
    key: "FIT_TARGET",
    flags: &["-fitt", "--fit-target"],
};
pub const KNOB_SPEC_N: Knob = Knob {
    key: "SPEC_N_MAX",
    flags: &["--spec-draft-n-max"],
};

fn get<'a>(cfg: &'a Cfg, k: &str) -> &'a str {
    cfg.get(k).map(String::as_str).unwrap_or("")
}

impl Knob {
    /// La valeur effective (drapeau d'EXTRA_ARGS, sinon la clé).
    pub fn get(&self, cfg: &Cfg) -> String {
        let v = flag_value(&split_args(get(cfg, "EXTRA_ARGS")), self.flags);
        if !v.is_empty() {
            return v;
        }
        get(cfg, self.key).trim().to_string()
    }

    /// Une COPIE de cfg où la valeur vaut `val` ("" = retirée).
    pub fn set(&self, cfg: &Cfg, val: &str) -> Cfg {
        let mut out = cfg.clone();
        if has_any_flag(&split_args(get(cfg, "EXTRA_ARGS")), self.flags) {
            let e = extra_set(get(cfg, "EXTRA_ARGS"), self.flags, val);
            if e.is_empty() {
                out.remove("EXTRA_ARGS");
            } else {
                out.insert("EXTRA_ARGS".into(), e);
            }
            return out;
        }
        if val.is_empty() {
            out.remove(self.key);
        } else {
            out.insert(self.key.into(), val.into());
        }
        out
    }
}

/// `setKey` : copie de cfg avec key = val ("" = retirée).
pub fn set_key(cfg: &Cfg, key: &str, val: &str) -> Cfg {
    let mut out = cfg.clone();
    if val.is_empty() {
        out.remove(key);
    } else {
        out.insert(key.into(), val.into());
    }
    out
}

fn first_non_empty(a: &str, b: &str) -> String {
    if a.trim().is_empty() {
        b.to_string()
    } else {
        a.to_string()
    }
}

// ---------------------------------------------------------------------------
// Les essais
// ---------------------------------------------------------------------------

/// `tuneEnv` : ce que l'optimiseur sait de la machine et de ce que
/// l'utilisateur a coché.
#[derive(Clone, Debug, Default)]
pub struct Env {
    pub help: String,
    pub gpus: i64,
    pub vram_mib: i64,
    pub model_bytes: i64,
    pub moe: bool,
    pub phys_cores: i64,
    pub smi: bool,
    pub arg_env: HashMap<String, String>,
    pub user_env: HashMap<String, String>,
    pub queues_env: bool,
    pub placement: bool,
    pub opt_in: bool,
}

/// `tuneCand` : un essai — la configuration COMPLÈTE à lancer.
#[derive(Clone, Debug, PartialEq)]
pub struct Cand {
    pub stage: String,
    pub label: String,
    pub cfg: Cfg,
}

fn cand(stage: &str, label: impl Into<String>, cfg: Cfg) -> Cand {
    Cand {
        stage: stage.into(),
        label: label.into(),
        cfg,
    }
}

type Gen = fn(&Cfg, &Env) -> (Vec<Cand>, String);

/// `tuneStages` : l'ordre de la recherche.
pub const STAGES: [(&str, Gen); 7] = [
    ("placement", placement_cands),
    ("marges", fit_target_cands),
    ("lots", batch_cands),
    ("délestage", offload_cands),
    ("threads", thread_cands),
    ("files CUDA", queue_cands),
    ("opt-in", opt_in_cands),
];

/// `tuneStageWanted` : noms comparés sans accent ni casse, par leur premier
/// mot.
pub fn stage_wanted(name: &str, only: &[String]) -> bool {
    let norm = |s: &str| {
        let s = s.trim().to_lowercase().replace(['é', 'è', 'ê'], "e");
        match s.find([' ', '-']) {
            Some(i) if i > 0 && s != "opt-in" => s[..i].to_string(),
            _ => s,
        }
    };
    let n = norm(name);
    if only.is_empty() || n == "placement" || n == "opt-in" {
        return true;
    }
    only.iter().any(|o| norm(o) == n)
}

/// `tuneSI`.
pub fn sys_info(env: &Env) -> SysInfo {
    SysInfo {
        help: env.help.clone(),
        arg_env: env.arg_env.clone(),
        user_env: env.user_env.clone(),
        ..Default::default()
    }
}

/// `tuneWeightsOnCPU`.
pub fn weights_on_cpu(cfg: &Cfg, env: &Env) -> bool {
    let extra = split_args(get(cfg, "EXTRA_ARGS"));
    if !crate::serve::cache::cpu_weights(cfg, &extra, &env.arg_env, 0).is_empty() {
        return true;
    }
    env.vram_mib > 0 && env.model_bytes > env.vram_mib << 20
}

/// `tunePlacementCands` : le placement manuel remplacé par --fit.
pub fn placement_cands(cfg: &Cfg, env: &Env) -> (Vec<Cand>, String) {
    if !env.placement {
        return (vec![], "non demandé (« inclure le placement »)".into());
    }
    let extra = split_args(get(cfg, "EXTRA_ARGS"));
    if !crate::serve::moe::help_supports_fit(&env.help) {
        return (vec![], "ce moteur ne connaît pas --fit".into());
    }
    let (c, ok) = crate::serve::spec::ctx_fixed(cfg, &extra);
    if !ok {
        return (
            vec![],
            format!("contexte « {c} » non chiffré : --fit pourrait le réduire"),
        );
    }
    if crate::serve::split::split_mode_key(cfg) == "tensor" {
        return (vec![], "SPLIT_MODE=tensor".into());
    }
    let moe = !crate::serve::moe::cpu_experts(&extra, &env.arg_env).is_empty();
    let ts = has_any_flag(&extra, &["-ts", "--tensor-split"]) && env.gpus >= 2;
    if !moe && !ts {
        return (vec![], "rien n'est placé à la main".into());
    }
    let mut e = get(cfg, "EXTRA_ARGS").to_string();
    let mut label = String::from("placement auto (--fit)");
    if moe {
        e = extra_drop_experts(&e).0;
        label.push_str(" au lieu des experts placés à la main");
    }
    if ts {
        e = extra_drop(&e, 1, &["-ts", "--tensor-split"]);
        label.push_str(" au lieu de --tensor-split");
    }
    let nc = set_key(cfg, "EXTRA_ARGS", &e);
    let why = crate::serve::expert::fit_blocker(&nc, &split_args(&e), &sys_info(env));
    if !why.is_empty() {
        return (vec![], format!("--fit resterait inactif ({why})"));
    }
    (vec![cand("placement", label, nc)], String::new())
}

/// `tuneFitTargetCands` : la marge que --fit laisse libre par carte.
pub fn fit_target_cands(cfg: &Cfg, env: &Env) -> (Vec<Cand>, String) {
    let extra = split_args(get(cfg, "EXTRA_ARGS"));
    if env.gpus < 2 {
        return (vec![], "une seule carte".into());
    }
    if !env.smi {
        return (vec![], "VRAM libre non mesurable (nvidia-smi)".into());
    }
    if !env.help.contains("--fit-target") {
        return (vec![], "ce moteur ne connaît pas --fit-target".into());
    }
    if !crate::serve::spec::ctx_fixed(cfg, &extra).1 {
        return (vec![], "contexte non chiffré".into());
    }
    let why = crate::serve::expert::fit_blocker(cfg, &extra, &sys_info(env));
    if !why.is_empty() {
        return (vec![], format!("--fit inactif ({why})"));
    }
    let vals: &[&str] = if env.gpus == 2 {
        &["1024,2048", "2048,1024", "1536"]
    } else {
        &["1536", "2048"]
    };
    let cur = KNOB_FIT_TARGET.get(cfg);
    let out = vals
        .iter()
        .filter(|v| **v != cur)
        .map(|v| {
            cand(
                "marges",
                format!("FIT_TARGET={v}"),
                KNOB_FIT_TARGET.set(cfg, v),
            )
        })
        .collect();
    (out, String::new())
}

/// `strconv.Atoi` (0 sur erreur, comme `curB, _ := strconv.Atoi(...)`).
fn atoi0(s: &str) -> i64 {
    let digits = s.strip_prefix(['+', '-']).unwrap_or(s);
    if digits.is_empty() || !digits.bytes().all(|c| c.is_ascii_digit()) {
        return 0;
    }
    s.parse().unwrap_or(0)
}

/// `tuneBatchCands` : micro-lot et lot.
pub fn batch_cands(cfg: &Cfg, env: &Env) -> (Vec<Cand>, String) {
    let extra = split_args(get(cfg, "EXTRA_ARGS"));
    let cur_ub = crate::serve::moe::serve_ubatch(cfg, &extra);
    let cur_b = atoi0(&first_non_empty(&KNOB_BATCH.get(cfg), "2048"));
    let mut ubs = vec![512i64, 1024, 2048];
    if env.moe {
        ubs.push(4096);
    }
    let multi = env.gpus >= 2 && !weights_on_cpu(cfg, env);
    let mut out = Vec::new();
    for ub in ubs {
        if !env.smi && ub > cur_ub {
            continue;
        }
        let mut seen = BTreeSet::new();
        for b in [2048.max(ub), (2 * ub).min(8192)] {
            if seen.contains(&b) || b < ub || (multi && b <= ub) || (ub == cur_ub && b == cur_b) {
                continue;
            }
            seen.insert(b);
            let c = KNOB_UBATCH.set(cfg, &ub.to_string());
            let c = KNOB_BATCH.set(&c, &b.to_string());
            out.push(cand("lots", format!("UBATCH={ub} BATCH={b}"), c));
        }
    }
    if out.is_empty() {
        return (vec![], "rien à essayer".into());
    }
    (out, String::new())
}

/// `tuneOffloadCands` : seuil d'envoi au GPU des poids restés en RAM.
pub fn offload_cands(cfg: &Cfg, env: &Env) -> (Vec<Cand>, String) {
    if !weights_on_cpu(cfg, env) {
        return (vec![], "tous les poids sur GPU".into());
    }
    if env
        .user_env
        .get("GGML_OP_OFFLOAD_MIN_BATCH")
        .is_some_and(|v| !v.is_empty())
    {
        return (
            vec![],
            "GGML_OP_OFFLOAD_MIN_BATCH déjà posé dans l'environnement".into(),
        );
    }
    let cur = first_non_empty(get(cfg, "OP_OFFLOAD_MIN_BATCH").trim(), "32");
    let out = ["32", "128", "512"]
        .iter()
        .filter(|v| **v != cur)
        .map(|v| {
            cand(
                "délestage",
                format!("OP_OFFLOAD_MIN_BATCH={v}"),
                set_key(cfg, "OP_OFFLOAD_MIN_BATCH", v),
            )
        })
        .collect();
    (out, String::new())
}

/// `tuneThreadCands` : threads CPU, quand des poids tournent sur CPU.
pub fn thread_cands(cfg: &Cfg, env: &Env) -> (Vec<Cand>, String) {
    if !weights_on_cpu(cfg, env) {
        return (vec![], "tous les poids sur GPU".into());
    }
    if env.phys_cores <= 1 {
        return (vec![], "nombre de cœurs physiques inconnu".into());
    }
    let mut cur = KNOB_THREADS.get(cfg);
    if cur == "0" {
        cur.clear();
    }
    let mut seen = BTreeSet::from([cur]);
    let mut out = Vec::new();
    for n in [0, env.phys_cores - 1, env.phys_cores / 2] {
        let v = if n > 0 { n.to_string() } else { String::new() };
        if seen.contains(&v) || n < 0 {
            continue;
        }
        seen.insert(v.clone());
        let label = if v.is_empty() {
            "threads omis (cœurs physiques)".to_string()
        } else {
            format!("THREADS={v}")
        };
        out.push(cand("threads", label, KNOB_THREADS.set(cfg, &v)));
    }
    (out, String::new())
}

/// `tuneQueueCands` : files de lancement CUDA, sur deux cartes ou plus.
pub fn queue_cands(cfg: &Cfg, env: &Env) -> (Vec<Cand>, String) {
    if env.gpus < 2 {
        return (vec![], "une seule carte".into());
    }
    if env.queues_env {
        return (
            vec![],
            "CUDA_SCALE_LAUNCH_QUEUES déjà posé dans l'environnement".into(),
        );
    }
    let cur = get(cfg, "CUDA_LAUNCH_QUEUES").trim().to_lowercase();
    let out = ["off", "4x"]
        .iter()
        .filter(|v| **v != cur)
        .map(|v| {
            cand(
                "files CUDA",
                format!("CUDA_LAUNCH_QUEUES={v}"),
                set_key(cfg, "CUDA_LAUNCH_QUEUES", v),
            )
        })
        .collect();
    (out, String::new())
}

/// `tuneOptInCands` : spéculation, graphes CUDA, échantillonnage sur GPU —
/// seulement si l'utilisateur l'a coché.
pub fn opt_in_cands(cfg: &Cfg, env: &Env) -> (Vec<Cand>, String) {
    if !env.opt_in {
        return (
            vec![],
            "non demandé (« inclure les options opt-in »)".into(),
        );
    }
    let extra = split_args(get(cfg, "EXTRA_ARGS"));
    let mut out = Vec::new();
    let mode = crate::serve::spec::spec_mode(cfg);
    if mode == "off"
        && crate::serve::spec::help_supports_mtp(&env.help)
        && !crate::serve::spec::spec_user_set(&extra, &env.arg_env)
    {
        out.push(cand("opt-in", "SPEC=auto", set_key(cfg, "SPEC", "auto")));
    }
    if mode != "off"
        && !mode.is_empty()
        && mode != "ngram"
        && env.help.contains("--spec-draft-n-max")
    {
        let cur = first_non_empty(&KNOB_SPEC_N.get(cfg), "3");
        for v in ["2", "3", "4"] {
            if v != cur {
                out.push(cand(
                    "opt-in",
                    format!("SPEC_N_MAX={v}"),
                    KNOB_SPEC_N.set(cfg, v),
                ));
            }
        }
    }
    if !get(cfg, "CUDA_GRAPH_OPT").trim().eq_ignore_ascii_case("on")
        && env
            .user_env
            .get("GGML_CUDA_GRAPH_OPT")
            .is_none_or(|v| v.is_empty())
    {
        out.push(cand(
            "opt-in",
            "CUDA_GRAPH_OPT=on",
            set_key(cfg, "CUDA_GRAPH_OPT", "on"),
        ));
    }
    if env.help.contains("--backend-sampling")
        && !has_any_flag(&extra, &["-bs", "--backend-sampling"])
        && !crate::serve::env_truthy(
            env.arg_env
                .get("LLAMA_ARG_BACKEND_SAMPLING")
                .map(String::as_str)
                .unwrap_or(""),
        )
        && crate::serve::split::split_mode_key(cfg) != "tensor"
    {
        out.push(cand(
            "opt-in",
            "--backend-sampling",
            set_key(
                cfg,
                "EXTRA_ARGS",
                &extra_add_switch(get(cfg, "EXTRA_ARGS"), "--backend-sampling"),
            ),
        ));
    }
    if out.is_empty() {
        return (vec![], "rien à essayer".into());
    }
    (out, String::new())
}

// ---------------------------------------------------------------------------
// Garde-fous
// ---------------------------------------------------------------------------

/// `tuneCoreArgs` : la ligne de commande sans ce que l'optimiseur a le droit
/// de régler.
pub fn core_args(args: &[String], opt_in: bool) -> Vec<String> {
    let mut out = Vec::with_capacity(args.len());
    let mut i = 0;
    while i < args.len() {
        let a = &args[i];
        let (name, val, has_eq) = match a.split_once('=') {
            Some((n, v)) => (n.to_string(), v.to_string(), true),
            None => (a.clone(), String::new(), false),
        };
        let next = |i: &mut usize| -> String {
            if has_eq {
                return val.clone();
            }
            if *i + 1 < args.len() {
                *i += 1;
                return args[*i].clone();
            }
            String::new()
        };
        match name.as_str() {
            "--host" | "--port" | "--api-key" | "--slot-save-path" | "-cram" | "--cache-ram" => {
                next(&mut i);
                out.push(name.clone());
                out.push("*".into());
                i += 1;
                continue;
            }
            _ => {}
        }
        if opt_in && (name.starts_with("--spec-") || name == "-md" || name == "--model-draft") {
            next(&mut i);
            i += 1;
            continue;
        }
        let Some(f) = flag_for(&name, opt_in) else {
            out.push(a.clone());
            i += 1;
            continue;
        };
        if f.arity == 0 {
            i += 1;
            continue;
        }
        let v = next(&mut i);
        if name == "-ot" || name == "--override-tensor" {
            let rest: Vec<String> = v
                .split(',')
                .filter_map(|el| {
                    let el = el.trim();
                    let pat = el.split_once('=').map_or(el, |(p, _)| p).trim();
                    (!el.is_empty() && !crate::serve::moe::ot_targets_experts(pat))
                        .then(|| el.to_string())
                })
                .collect();
            if !rest.is_empty() {
                out.push(name.clone());
                out.push(rest.join(","));
            }
        }
        i += 1;
    }
    out
}

/// `tuneEnvTuned` : variables du moteur que l'optimiseur règle.
pub fn env_tuned(k: &str, opt_in: bool) -> bool {
    match k {
        "CUDA_SCALE_LAUNCH_QUEUES" | "GGML_OP_OFFLOAD_MIN_BATCH" => true,
        "GGML_CUDA_GRAPH_OPT" => opt_in,
        _ => false,
    }
}

/// `tuneLaunch` : la ligne de commande composée par `loki serve` pour un
/// essai.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct Launch {
    #[serde(default)]
    pub bin: String,
    #[serde(default)]
    pub args: Vec<String>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub env: BTreeMap<String, String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub notes: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub probe: Option<Probe>,
}

/// `tuneProbe` : ce que `loki serve` a sondé pour composer la ligne.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct Probe {
    #[serde(default)]
    pub help: String,
    #[serde(default)]
    pub gpus: i64,
    #[serde(default)]
    pub vram_mib: i64,
    #[serde(default)]
    pub model_bytes: i64,
    #[serde(default)]
    pub moe: bool,
    #[serde(default)]
    pub cpu_threads: i64,
    #[serde(default, skip_serializing_if = "HashMap::is_empty")]
    pub arg_env: HashMap<String, String>,
    #[serde(default, skip_serializing_if = "HashMap::is_empty")]
    pub user_env: HashMap<String, String>,
    #[serde(default)]
    pub queues_env: bool,
}

/// `tuneDenature` : "" = même calcul ; sinon la première différence.
pub fn denature(base: &Launch, cand: &Launch, opt_in: bool) -> String {
    let (ba, ca) = (core_args(&base.args, opt_in), core_args(&cand.args, opt_in));
    for i in 0..ba.len().max(ca.len()) {
        let x = ba.get(i).map(String::as_str).unwrap_or("");
        let y = ca.get(i).map(String::as_str).unwrap_or("");
        if x != y {
            return format!(
                "ligne de commande différente hors réglages permis (« {y} » au lieu de « {x} »)"
            );
        }
    }
    // Le Go parcourt une table : l'ordre des clés y est aléatoire. Ici, trié.
    let keys: BTreeSet<&String> = base.env.keys().chain(cand.env.keys()).collect();
    for k in keys {
        let (b, c) = (
            base.env.get(k).map(String::as_str).unwrap_or(""),
            cand.env.get(k).map(String::as_str).unwrap_or(""),
        );
        if !env_tuned(k, opt_in) && b != c {
            return format!("variable {k} différente (« {c} » au lieu de « {b} »)");
        }
    }
    String::new()
}

/// `tuneLaunchKey` : identité d'une ligne de lancement, adresse et port mis
/// à part.
pub fn launch_key(l: &Launch) -> String {
    let mut full = Vec::with_capacity(l.args.len());
    let mut i = 0;
    while i < l.args.len() {
        match l.args[i].as_str() {
            "--host" | "--port" | "--api-key" | "--slot-save-path" => {
                i += 2;
                continue;
            }
            _ => full.push(l.args[i].clone()),
        }
        i += 1;
    }
    let keys: Vec<String> = l.env.iter().map(|(k, v)| format!("{k}={v}")).collect();
    format!("{}\x01{}", full.join("\x00"), keys.join("\x00"))
}

fn all_keys<'a>(a: &'a Cfg, b: &'a Cfg) -> BTreeSet<&'a String> {
    a.keys().chain(b.keys()).collect()
}

/// `tuneCfgCheck` : la configuration d'un essai ne diffère de la référence
/// que par des clés permises, et son EXTRA_ARGS que par la liste blanche.
pub fn cfg_check(base: &Cfg, cand: &Cfg, opt_in: bool) -> Result<(), String> {
    for k in all_keys(base, cand) {
        if get(base, k) == get(cand, k) {
            continue;
        }
        if k == "EXTRA_ARGS" {
            let a = core_args(&split_args(get(base, k)), opt_in);
            let b = core_args(&split_args(get(cand, k)), opt_in);
            if a != b {
                return Err("EXTRA_ARGS modifié hors liste blanche".into());
            }
            continue;
        }
        if !is_key(k) && !(opt_in && is_opt_in_key(k)) {
            return Err(format!("clé {k} intouchable"));
        }
    }
    Ok(())
}

/// `tuneDiff` : ce qui change d'une configuration à l'autre, clé par clé.
pub fn diff(base: &Cfg, cand: &Cfg) -> Vec<String> {
    let show = |v: &str| {
        if v.is_empty() {
            "(absent)".to_string()
        } else {
            v.to_string()
        }
    };
    all_keys(base, cand)
        .into_iter()
        .filter(|k| get(base, k) != get(cand, k))
        .map(|k| format!("{k} : {} → {}", show(get(base, k)), show(get(cand, k))))
        .collect()
}

/// `tunePatchPreset` : le preset réécrit clé par clé, vérifié.
pub fn patch_preset(content: &str, base: &Cfg, win: &Cfg) -> Result<String, String> {
    let changed: Vec<&String> = all_keys(base, win)
        .into_iter()
        .filter(|k| get(base, k) != get(win, k))
        .collect();
    let mut want = crate::presets::parse_env(content);
    let mut out = content.to_string();
    for k in changed {
        if !is_key(k) && !is_opt_in_key(k) {
            return Err(format!("clé {k} intouchable"));
        }
        out = crate::presets_api::preset_set_key(&out, k, get(win, k));
        if get(win, k).is_empty() {
            want.remove(k);
        } else {
            want.insert(k.clone(), get(win, k).to_string());
        }
    }
    let got = crate::presets::parse_env(&out);
    if got.len() != want.len() {
        return Err("réécriture du preset incohérente".into());
    }
    let mut keys: Vec<&String> = want.keys().collect();
    keys.sort();
    for k in keys {
        if got.get(k) != want.get(k) {
            return Err(format!("réécriture du preset incohérente sur {k}"));
        }
    }
    Ok(out)
}

// ---------------------------------------------------------------------------
// Journal de l'essai
// ---------------------------------------------------------------------------

/// `tuneLogFacts` : couches déportées (dernière mention ; -1 = absente) et
/// types du cache KV vus dans le journal.
pub fn log_facts(log: &str) -> (i64, i64, String) {
    let off_re = regex::Regex::new(r"offloaded (\d+)/(\d+) layers to GPU").expect("regex");
    let kv_re =
        regex::Regex::new(r"K \(([A-Za-z0-9_]+)\):[^\n]*V \(([A-Za-z0-9_]+)\):").expect("regex");
    let (mut off, mut total) = (-1, -1);
    for m in off_re.captures_iter(log) {
        off = m[1].parse().unwrap_or(0);
        total = m[2].parse().unwrap_or(0);
    }
    let kvs: BTreeSet<String> = kv_re
        .captures_iter(log)
        .map(|m| format!("{}/{}", m[1].to_lowercase(), m[2].to_lowercase()))
        .collect();
    (off, total, kvs.into_iter().collect::<Vec<_>>().join(","))
}

/// `tuneLoadFailure` : pourquoi un essai n'a pas chargé.
pub fn load_failure(log: &str) -> String {
    let l = log.to_lowercase();
    if l.contains("out of memory")
        || l.contains("cudamalloc failed")
        || l.contains("failed to allocate")
    {
        return "mémoire insuffisante (OOM)".into();
    }
    if l.contains("failed to fit") {
        return "--fit n'a pas su placer le modèle".into();
    }
    if l.contains("failed to load model") || l.contains("error loading model") {
        return "échec du chargement du modèle".into();
    }
    let trimmed = log.trim();
    if let Some(last) = trimmed.split('\n').next_back()
        && !last.is_empty()
    {
        let last = last.trim();
        // last[:160] du Go : en octets — un caractère coupé devient U+FFFD
        // par octet à l'encodage JSON.
        let last = if last.len() > 160 {
            crate::system::go_lossy(&last.as_bytes()[..160])
        } else {
            last.to_string()
        };
        return format!("moteur arrêté : {last}");
    }
    "moteur arrêté au chargement".into()
}

// ---------------------------------------------------------------------------
// Mesure et score
// ---------------------------------------------------------------------------

/// `tuneWeights` : la forme d'un tour typique.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct Weights {
    #[serde(default, serialize_with = "ser_go_f64")]
    pub k: f64,
    #[serde(default, serialize_with = "ser_go_f64")]
    pub g: f64,
    #[serde(default, rename = "f_cold", serialize_with = "ser_go_f64")]
    pub f_cold: f64,
    #[serde(default)]
    pub from: String,
}

impl Default for Weights {
    fn default() -> Self {
        Weights {
            k: 2000.0,
            g: 600.0,
            f_cold: 0.05,
            from: "défaut".into(),
        }
    }
}

/// Un enregistrement de la télémétrie, vu de l'optimiseur.
#[derive(Clone, Debug, Default, PartialEq, Deserialize)]
pub struct PerfRec {
    #[serde(default)]
    pub kind: String,
    #[serde(default)]
    pub complete: bool,
    #[serde(default)]
    pub total: i64,
    #[serde(default)]
    pub cached: Option<i64>,
    #[serde(default)]
    pub new: i64,
    #[serde(default, rename = "gen")]
    pub generated: i64,
}

/// `perfMedian` : la médiane (moyenne des deux du milieu).
fn median(v: &mut [f64]) -> f64 {
    v.sort_by(|a, b| a.partial_cmp(b).unwrap_or(std::cmp::Ordering::Equal));
    let n = v.len();
    if n % 2 == 1 {
        v[n / 2]
    } else {
        (v[n / 2 - 1] + v[n / 2]) / 2.0
    }
}

/// `tuneWeightsFrom` : K, G et fCold des médianes de la télémétrie quand
/// elle en a assez vu ; sinon les valeurs par défaut.
pub fn weights_from(recs: &[PerfRec]) -> Weights {
    let (mut news, mut gens) = (Vec::new(), Vec::new());
    let (mut cold, mut known) = (0, 0);
    for r in recs {
        if !r.complete || matches!(r.kind.as_str(), "bench" | "prewarm" | "foreign") {
            continue;
        }
        if r.new > 0 {
            news.push(r.new as f64);
        }
        if r.generated > 0 {
            gens.push(r.generated as f64);
        }
        if let Some(c) = r.cached
            && r.total >= 2048
        {
            known += 1;
            if c * 10 < r.total {
                cold += 1;
            }
        }
    }
    if news.len() < 20 || gens.len() < 20 {
        return Weights::default();
    }
    let mut w = Weights {
        k: median(&mut news).clamp(128.0, 32768.0),
        g: median(&mut gens).clamp(16.0, 8192.0),
        from: "télémétrie".into(),
        ..Default::default()
    };
    if known >= 20 {
        w.f_cold = cold as f64 / known as f64;
    }
    w
}

/// `tuneRun` : une mesure d'un essai.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct Run {
    #[serde(default, serialize_with = "ser_go_f64")]
    pub turn_sec: f64,
    #[serde(default, skip_serializing_if = "zero", serialize_with = "ser_go_f64")]
    pub cold_pp: f64,
    #[serde(default, skip_serializing_if = "zero", serialize_with = "ser_go_f64")]
    pub cached_pp: f64,
    #[serde(default, skip_serializing_if = "zero", serialize_with = "ser_go_f64")]
    pub tg: f64,
    #[serde(default, serialize_with = "ser_go_f64")]
    pub prose_pp: f64,
    #[serde(default, serialize_with = "ser_go_f64")]
    pub prose_tg: f64,
    #[serde(default, skip_serializing_if = "zero_i")]
    pub depth: i64,
}

fn zero(v: &f64) -> bool {
    *v == 0.0
}

fn zero_i(v: &i64) -> bool {
    *v == 0
}

/// `tuneRunFrom` : un bench complet réduit à son score.
pub fn run_from(res: Option<&BenchResult>, w: &Weights) -> Result<Run, String> {
    let Some(res) = res.filter(|r| r.predicted_per_second > 0.0 && r.prompt_per_second > 0.0)
    else {
        return Err("mesure vide".into());
    };
    let mut r = Run {
        prose_pp: res.prompt_per_second,
        prose_tg: res.predicted_per_second,
        ..Default::default()
    };
    let d = match &res.depth {
        Some(d) if d.skipped.is_empty() => d,
        _ => {
            r.turn_sec = w.k / r.prose_pp + w.g / r.prose_tg;
            return Ok(r);
        }
    };
    if !d.partial.is_empty() {
        return Err(format!("mesure partielle : {}", d.partial));
    }
    let Some(cold) = d.cold.as_ref().filter(|c| c.prompt_per_second > 0.0) else {
        return Err("mesure en profondeur incomplète".into());
    };
    if d.turns.is_empty() || d.decode_per_second <= 0.0 {
        return Err("mesure en profondeur incomplète".into());
    }
    let (mut fresh, mut ms) = (0i64, 0f64);
    for t in &d.turns {
        if t.cached < 0 || t.expected <= 0 {
            fresh = 0;
            ms = 0.0;
            break;
        }
        fresh += t.new + t.cached - t.expected;
        ms += t.prompt_ms;
    }
    r.cached_pp = d.cached_per_second;
    if fresh > 0 && ms > 0.0 {
        r.cached_pp = fresh as f64 / (ms / 1000.0);
    }
    if r.cached_pp <= 0.0 {
        return Err("prefill des tours illisible".into());
    }
    r.cold_pp = cold.prompt_per_second;
    r.tg = d.decode_per_second;
    r.depth = d.target;
    r.turn_sec = w.k / r.cached_pp + w.g / r.tg + w.f_cold * d.target as f64 / r.cold_pp;
    Ok(r)
}

/// `tuneMean`.
pub fn mean(v: &[f64]) -> f64 {
    if v.is_empty() {
        return 0.0;
    }
    v.iter().sum::<f64>() / v.len() as f64
}

/// `tuneSpread` : l'écart relatif entre passages ((max-min)/moyenne).
pub fn spread(v: &[f64]) -> f64 {
    if v.len() < 2 {
        return 0.0;
    }
    let lo = v.iter().copied().fold(f64::INFINITY, f64::min);
    let hi = v.iter().copied().fold(f64::NEG_INFINITY, f64::max);
    let m = mean(v);
    if m <= 0.0 {
        return 0.0;
    }
    (hi - lo) / m
}

/// `tuneMinGain`.
pub const MIN_GAIN: f64 = 0.03;

/// `tuneWins` : le candidat bat-il la référence, d'au moins max(3 %, l'écart
/// entre passages), et sur deux passages ?
pub fn wins(base: &[f64], cand: &[f64]) -> (f64, f64, bool) {
    let mb = mean(base);
    if mb <= 0.0 || cand.is_empty() {
        return (0.0, MIN_GAIN, false);
    }
    let gain = (mb - mean(cand)) / mb;
    let need = MIN_GAIN.max(spread(base)).max(spread(cand));
    (gain, need, cand.len() >= 2 && gain > need)
}

/// `tuneWorthRepeat`.
pub fn worth_repeat(base: &[f64], first: f64) -> bool {
    let mb = mean(base);
    mb > 0.0 && (mb - first) / mb > MIN_GAIN
}

/// `tuneTightMiB`.
pub const TIGHT_MIB: i64 = 768;
/// `tuneVisionReserveMiB`.
pub const VISION_RESERVE_MIB: i64 = 512;

/// `tuneHeadroom` : VRAM libre minimale sur les cartes visibles (Mio) ;
/// `(total, used)` par carte ; `visible` vide = toutes.
pub fn headroom(gpus: &[(i64, i64)], visible: &[i64]) -> Option<i64> {
    let mut min_free: Option<i64> = None;
    for (i, (total, used)) in gpus.iter().enumerate() {
        if !visible.is_empty() && !visible.contains(&(i as i64)) {
            continue;
        }
        if *total <= 0 {
            continue;
        }
        let free = total - used;
        min_free = Some(min_free.map_or(free, |m| m.min(free)));
    }
    min_free
}

/// `tuneVisibleGPUs` : index des cartes de CUDA_VISIBLE_DEVICES ; vide =
/// toutes, ou des UUID qu'on ne sait pas relier.
pub fn visible_gpus(cfg: &Cfg) -> Vec<i64> {
    let mut out = Vec::new();
    for p in get(cfg, "CUDA_VISIBLE_DEVICES").split(',') {
        let p = p.trim();
        let digits = p.strip_prefix(['+', '-']).unwrap_or(p);
        if digits.is_empty() || !digits.bytes().all(|c| c.is_ascii_digit()) {
            return Vec::new();
        }
        match p.parse() {
            Ok(n) => out.push(n),
            Err(_) => return Vec::new(),
        }
    }
    out
}

/// `tuneForceAddr` : toutes les occurrences de --host / --port pointent sur
/// l'adresse de l'essai.
pub fn force_addr(args: &[String], host: &str, port: &str) -> Vec<String> {
    let mut out = args.to_vec();
    let mut i = 0;
    while i < out.len() {
        let (name, has_eq) = match out[i].split_once('=') {
            Some((n, _)) => (n.to_string(), true),
            None => (out[i].clone(), false),
        };
        let val = match name.as_str() {
            "--host" => host,
            "--port" => port,
            _ => {
                i += 1;
                continue;
            }
        };
        if has_eq {
            out[i] = format!("{name}={val}");
        } else if i + 1 < out.len() {
            out[i + 1] = val.to_string();
            i += 1;
        }
        i += 1;
    }
    out
}

// ---------------------------------------------------------------------------
// Fichiers de l'essai
// ---------------------------------------------------------------------------

/// `writeTuneTrialConfig` (le texte) : relue, la configuration doit revenir
/// à l'identique — une valeur que le format ne sait pas représenter serait
/// un autre essai que celui voulu.
pub fn trial_config_text(cfg: &Cfg) -> Result<String, String> {
    let sorted: BTreeMap<String, String> =
        cfg.iter().map(|(k, v)| (k.clone(), v.clone())).collect();
    let text = crate::presets_api::format_env(&sorted);
    let back = crate::presets::parse_env(&text);
    if back.len() != cfg.len() {
        return Err("configuration d'essai non représentable".into());
    }
    for (k, v) in &sorted {
        if back.get(k) != Some(v) {
            return Err(format!(
                "valeur de {k} non représentable dans un fichier de configuration"
            ));
        }
    }
    Ok(text)
}
