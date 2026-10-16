//! Lot 28 : la partie pure de l'optimiseur, rejouée sur le journal tiré du Go
//! (`internal/loki/parity_tune_test.go` → `tests/parity_tune/core.json`).

use std::collections::HashMap;

use loki_rs::bench::BenchResult;
use loki_rs::tune::core::{self as tc, Cfg, Env, Knob, Launch, PerfRec};
use serde_json::{Value, json};

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_tune/core.json")).expect("journal")
}

fn s(v: &Value) -> &str {
    v.as_str().unwrap_or("")
}

fn cfg(v: &Value) -> Cfg {
    serde_json::from_value(if v.is_null() { json!({}) } else { v.clone() }).unwrap()
}

fn strs(v: &Value) -> Vec<String> {
    serde_json::from_value(if v.is_null() { json!([]) } else { v.clone() }).unwrap()
}

/// Une liste Go : `null` et `[]` se valent ici (tranche nil ou vide).
fn list(v: Vec<String>) -> Value {
    json!(v)
}

fn same_list(got: Vec<String>, want: &Value, what: &str) {
    assert_eq!(list(got), list(strs(want)), "{what}");
}

const KNOBS: [(&str, Knob); 5] = [
    ("BATCH", tc::KNOB_BATCH),
    ("UBATCH", tc::KNOB_UBATCH),
    ("THREADS", tc::KNOB_THREADS),
    ("FIT_TARGET", tc::KNOB_FIT_TARGET),
    ("SPEC_N_MAX", tc::KNOB_SPEC_N),
];

#[test]
fn extra_args_jeton_par_jeton() {
    let fx = fixture();
    for c in fx["extra"].as_array().unwrap() {
        let input = s(&c["in"]);
        let spans: Vec<Value> = tc::arg_spans(input)
            .into_iter()
            .map(|sp| json!([sp.start, sp.end, sp.val]))
            .collect();
        let want = if c["spans"].is_null() {
            json!([])
        } else {
            c["spans"].clone()
        };
        assert_eq!(Value::Array(spans), want, "spans {input:?}");
        same_list(
            loki_rs::models::split_args(input),
            &c["split"],
            &format!("split {input:?}"),
        );
        let ub = tc::KNOB_UBATCH.flags;
        let t = tc::KNOB_THREADS.flags;
        for (key, got) in [
            ("set_ub", tc::extra_set(input, ub, "2048")),
            ("drop_ub", tc::extra_set(input, ub, "")),
            ("set_t", tc::extra_set(input, t, "7")),
            ("set_t_space", tc::extra_set(input, t, "a b")),
            ("set_b", tc::extra_set(input, tc::KNOB_BATCH.flags, "4096")),
            (
                "drop_ts",
                tc::extra_drop(input, 1, &["-ts", "--tensor-split"]),
            ),
            (
                "drop_cmoe",
                tc::extra_drop(input, 0, &["-cmoe", "--cpu-moe"]),
            ),
            ("add_bs", tc::extra_add_switch(input, "--backend-sampling")),
        ] {
            assert_eq!(got, s(&c[key]), "{key} {input:?}");
        }
        let (dropped, removed) = tc::extra_drop_experts(input);
        assert_eq!(dropped, s(&c["drop_experts"]), "drop_experts {input:?}");
        same_list(removed, &c["removed"], &format!("removed {input:?}"));
        let split = loki_rs::models::split_args(input);
        same_list(
            tc::core_args(&split, false),
            &c["core"],
            &format!("core {input:?}"),
        );
        same_list(
            tc::core_args(&split, true),
            &c["core_optin"],
            &format!("core opt-in {input:?}"),
        );
    }
}

#[test]
fn reglages_par_cle_ou_par_drapeau() {
    let fx = fixture();
    for c in fx["knobs"].as_array().unwrap() {
        let base = cfg(&c["cfg"]);
        for (name, k) in KNOBS {
            assert_eq!(k.get(&base), s(&c["get"][name]), "get {name} {base:?}");
            assert_eq!(
                k.set(&base, "99"),
                cfg(&c["set"][name]),
                "set {name} {base:?}"
            );
            assert_eq!(
                k.set(&base, ""),
                cfg(&c["unset"][name]),
                "unset {name} {base:?}"
            );
        }
    }
}

fn env(v: &Value) -> Env {
    let map = |x: &Value| -> HashMap<String, String> {
        serde_json::from_value(if x.is_null() { json!({}) } else { x.clone() }).unwrap()
    };
    Env {
        help: s(&v["help"]).to_string(),
        gpus: v["gpus"].as_i64().unwrap(),
        vram_mib: v["vram_mib"].as_i64().unwrap(),
        model_bytes: v["model_bytes"].as_i64().unwrap(),
        moe: v["moe"].as_bool().unwrap(),
        phys_cores: v["phys_cores"].as_i64().unwrap(),
        smi: v["smi"].as_bool().unwrap(),
        arg_env: map(&v["arg_env"]),
        user_env: map(&v["user_env"]),
        queues_env: v["queues_env"].as_bool().unwrap(),
        placement: v["placement"].as_bool().unwrap(),
        opt_in: v["opt_in"].as_bool().unwrap(),
    }
}

#[test]
fn essais_de_chaque_etape() {
    let fx = fixture();
    let mut n = 0;
    for c in fx["cands"].as_array().unwrap() {
        let (base, e) = (cfg(&c["cfg"]), env(&c["env"]));
        let what = format!("{} / {}", s(&c["cfg_name"]), s(&c["env_name"]));
        assert_eq!(
            json!(tc::weights_on_cpu(&base, &e)),
            c["weights_on_cpu"],
            "{what} : poids sur CPU"
        );
        for (name, generate) in tc::STAGES {
            let (cands, why) = generate(&base, &e);
            let want = &c["stages"][name];
            assert_eq!(why, s(&want["why"]), "{what} / {name} : pourquoi");
            let got: Vec<Value> = cands
                .iter()
                .map(|x| json!({ "stage": x.stage, "label": x.label, "cfg": x.cfg }))
                .collect();
            assert_eq!(Value::Array(got), want["cands"], "{what} / {name} : essais");
            n += cands.len();
        }
    }
    assert!(n > 100, "{n} essais seulement");
    for c in fx["stage_wanted"].as_array().unwrap() {
        let only = strs(&c["only"]);
        assert_eq!(
            json!(tc::stage_wanted(s(&c["name"]), &only)),
            c["wanted"],
            "{} {only:?}",
            c["name"]
        );
    }
}

#[test]
fn garde_fous() {
    let fx = fixture();
    for c in fx["denature"].as_array().unwrap() {
        let name = s(&c["name"]);
        let b: Launch = serde_json::from_value(c["base"].clone()).unwrap();
        let k: Launch = serde_json::from_value(c["cand"].clone()).unwrap();
        assert_eq!(tc::denature(&b, &k, false), s(&c["denature"]), "{name}");
        assert_eq!(
            tc::denature(&b, &k, true),
            s(&c["denature_optin"]),
            "{name} opt-in"
        );
        assert_eq!(
            json!(tc::launch_key(&b) == tc::launch_key(&k)),
            c["same_key"],
            "{name} : même lancement"
        );
    }
    for c in fx["cfg_check"].as_array().unwrap() {
        let name = s(&c["name"]);
        let (b, k) = (cfg(&c["base"]), cfg(&c["cand"]));
        let err = |r: Result<(), String>| r.err().unwrap_or_default();
        assert_eq!(err(tc::cfg_check(&b, &k, false)), s(&c["check"]), "{name}");
        assert_eq!(
            err(tc::cfg_check(&b, &k, true)),
            s(&c["check_optin"]),
            "{name} opt-in"
        );
        same_list(tc::diff(&b, &k), &c["diff"], &format!("{name} : diff"));
    }
    let content = s(&fx["patch"]["content"]);
    let base = loki_rs::presets::parse_env(content);
    for c in fx["patch"]["cases"].as_array().unwrap() {
        let name = s(&c["name"]);
        let (out, err) = match tc::patch_preset(content, &base, &cfg(&c["win"])) {
            Ok(o) => (o, String::new()),
            Err(e) => (String::new(), e),
        };
        assert_eq!(
            (out.as_str(), err.as_str()),
            (s(&c["out"]), s(&c["error"])),
            "{name}"
        );
    }
}

#[test]
fn journal_d_un_essai() {
    let fx = fixture();
    for c in fx["logs"].as_array().unwrap() {
        let log = s(&c["log"]);
        let (off, total, kv) = tc::log_facts(log);
        assert_eq!(
            json!([off, total, kv]),
            json!([c["offloaded"], c["total"], c["kv"]]),
            "{log:?}"
        );
        assert_eq!(tc::load_failure(log), s(&c["failure"]), "{log:?}");
    }
}

#[test]
fn score_et_departage() {
    let fx = fixture();
    for c in fx["weights"].as_array().unwrap() {
        let recs: Vec<PerfRec> = serde_json::from_value(if c["recs"].is_null() {
            json!([])
        } else {
            c["recs"].clone()
        })
        .unwrap();
        assert_eq!(
            serde_json::to_value(tc::weights_from(&recs)).unwrap(),
            c["weights"],
            "{}",
            c["name"]
        );
    }
    let w = tc::Weights {
        k: 2000.0,
        g: 600.0,
        f_cold: 0.05,
        from: String::new(),
    };
    for c in fx["runs"].as_array().unwrap() {
        let bench: Option<BenchResult> = serde_json::from_value(c["bench"].clone()).unwrap();
        let (run, err) = match tc::run_from(bench.as_ref(), &w) {
            Ok(r) => (r, String::new()),
            Err(e) => (tc::Run::default(), e),
        };
        assert_eq!(
            serde_json::to_value(&run).unwrap(),
            c["run"],
            "{}",
            c["name"]
        );
        assert_eq!(err, s(&c["error"]), "{}", c["name"]);
    }
    let f = |v: &Value| -> Vec<f64> {
        serde_json::from_value(if v.is_null() { json!([]) } else { v.clone() }).unwrap()
    };
    for c in fx["wins"].as_array().unwrap() {
        let (b, k) = (f(&c["base"]), f(&c["cand"]));
        let (gain, need, ok) = tc::wins(&b, &k);
        let n = |k: &str| c[k].as_f64().unwrap();
        assert_eq!(
            (gain, need, ok, tc::spread(&b), tc::mean(&k)),
            (
                n("gain"),
                n("need"),
                c["wins"].as_bool().unwrap(),
                n("spread_base"),
                n("mean_cand")
            ),
            "{b:?} {k:?}"
        );
    }
    for c in fx["worth_repeat"].as_array().unwrap() {
        assert_eq!(
            json!(tc::worth_repeat(
                &f(&c["base"]),
                c["first"].as_f64().unwrap()
            )),
            c["worth"],
            "{c}"
        );
    }
    for c in fx["headroom"].as_array().unwrap() {
        let gpus: Vec<(i64, i64)> = serde_json::from_value(if c["gpus"].is_null() {
            json!([])
        } else {
            c["gpus"].clone()
        })
        .unwrap();
        let visible: Vec<i64> = serde_json::from_value(if c["visible"].is_null() {
            json!([])
        } else {
            c["visible"].clone()
        })
        .unwrap();
        let got = tc::headroom(&gpus, &visible);
        assert_eq!(
            json!([got.unwrap_or(0), got.is_some()]),
            json!([c["free"], c["ok"]]),
            "{c}"
        );
    }
    for c in fx["visible"].as_array().unwrap() {
        let got = tc::visible_gpus(&HashMap::from([(
            "CUDA_VISIBLE_DEVICES".to_string(),
            s(&c["in"]).to_string(),
        )]));
        let want: Vec<i64> = serde_json::from_value(if c["out"].is_null() {
            json!([])
        } else {
            c["out"].clone()
        })
        .unwrap();
        assert_eq!(got, want, "{c}");
    }
    for c in fx["force_addr"].as_array().unwrap() {
        assert_eq!(
            list(tc::force_addr(&strs(&c["in"]), "127.0.0.1", "41234")),
            c["out"],
            "{c}"
        );
    }
    for c in fx["trial_config"].as_array().unwrap() {
        let (text, err) = match tc::trial_config_text(&cfg(&c["cfg"])) {
            Ok(t) => (t, String::new()),
            Err(e) => (String::new(), e),
        };
        assert_eq!(
            (text.as_str(), err.as_str()),
            (s(&c["text"]), s(&c["error"])),
            "{c}"
        );
    }
}

/// Le verrou : le fichier écrit octet pour octet comme le Go, la clé de
/// prise, les binaires comparés par leur nom.
#[test]
fn verrou_au_format_du_go() {
    use loki_rs::tune::lock::{Owner, held_key, same_exe};
    let fx = fixture();
    for c in fx["lock_owners"].as_array().unwrap() {
        let o: Owner = serde_json::from_str(s(&c["json"])).unwrap();
        assert_eq!(serde_json::to_string(&o).unwrap(), s(&c["json"]));
        assert_eq!(held_key(&o), s(&c["key"]));
    }
    for c in fx["same_exe"].as_array().unwrap() {
        assert_eq!(
            same_exe(s(&c["a"]), s(&c["b"])),
            c["same"].as_bool().unwrap(),
            "{c}"
        );
    }
}

/// Le résultat : enregistré et montré comme le Go l'écrit.
#[test]
fn resultat_au_format_du_go() {
    use loki_rs::tune::job::TuneResult;
    let fx = fixture();
    for r in fx["results"].as_array().unwrap() {
        let res: TuneResult = serde_json::from_str(s(r)).unwrap();
        assert_eq!(serde_json::to_string(&res).unwrap(), s(r));
    }
}

/// Après application : la configuration est-elle l'essai retenu ?
#[test]
fn application_conforme() {
    use loki_rs::tune::job::{TuneResult, applied_mismatch};
    let fx = fixture();
    let a = &fx["applied"];
    let res = TuneResult {
        base_fp: s(&a["base_fp"]).to_string(),
        set: serde_json::from_value(a["set"].clone()).unwrap(),
        old: serde_json::from_value(a["old"].clone()).unwrap(),
        ..Default::default()
    };
    for c in a["cases"].as_array().unwrap() {
        assert_eq!(applied_mismatch(&cfg(&c["now"]), &res), s(&c["why"]), "{c}");
    }
}
