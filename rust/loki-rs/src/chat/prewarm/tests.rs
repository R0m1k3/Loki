//! Tests du préchauffage (cas de `chat_prewarm_test.go`).

use super::*;
use serde_json::json;

fn cfg(kv: &[(&str, &str)]) -> HashMap<String, String> {
    kv.iter()
        .map(|(k, v)| (k.to_string(), v.to_string()))
        .collect()
}

fn um(s: &str) -> Message {
    Message {
        role: "user".into(),
        content: Some(json!(s)),
        ..Default::default()
    }
}

#[test]
fn mode() {
    for (v, want) in [
        ("on", "on"),
        (" OUI ", "on"),
        ("1", "on"),
        ("full", "full"),
        ("FULL", "full"),
        ("off", ""),
        ("", ""),
    ] {
        assert_eq!(prewarm_mode(&cfg(&[("PREWARM", v)])), want, "{v:?}");
    }
    assert_eq!(prewarm_mode(&cfg(&[])), "");
    // Jamais pour un preset externe.
    assert_eq!(
        prewarm_mode(&cfg(&[
            ("PREWARM", "full"),
            (crate::engstatus::EXT_KEY_FLAG, "1")
        ])),
        ""
    );
}

/// Seuls les champs qui changent le prompt rendu comptent ; l'échantillonnage
/// et les champs de réponse, non.
#[test]
fn empreinte_des_options() {
    let base = json!({
        "model": "m", "messages": [], "stream": true, "temperature": 0.7,
        "tools": [{"type": "function", "function": {"name": "bash"}}],
        "tool_choice": "auto", "chat_template_kwargs": {"enable_thinking": true}
    });
    let h = opts_hash(&base);
    let mut same = base.clone();
    same["temperature"] = json!(0.2);
    same["stream"] = json!(false);
    same["max_tokens"] = json!(1);
    same["id_slot"] = json!(0);
    same["messages"] = json!([{"role": "user", "content": "x"}]);
    assert_eq!(opts_hash(&same), h);
    for (k, v) in [
        ("tools", json!([])),
        ("tool_choice", json!("none")),
        ("chat_template_kwargs", json!({"enable_thinking": false})),
        ("reasoning_effort", json!("low")),
        ("parallel_tool_calls", json!(true)),
    ] {
        let mut other = base.clone();
        other[k] = v;
        assert_ne!(opts_hash(&other), h, "{k}");
    }
    // Un champ absent n'est pas un champ vide.
    let mut absent = base.clone();
    absent.as_object_mut().unwrap().remove("tool_choice");
    let mut empty = base.clone();
    empty["tool_choice"] = json!("");
    assert_ne!(opts_hash(&absent), opts_hash(&empty));
}

#[test]
fn role_de_la_sentinelle() {
    assert_eq!(
        sentinel_caps(&json!({"Code": true, "Role": ""}))["Role"],
        "builder"
    );
    assert_eq!(sentinel_caps(&json!({"Code": true}))["Role"], "builder");
    assert_eq!(
        sentinel_caps(&json!({"Code": true, "Role": "verifier"}))["Role"],
        "verifier"
    );
    assert!(sentinel_caps(&json!({"Agent": true})).get("Role").is_none());
}

/// Une requête prolonge le préchauffage quand ses n premiers messages ont la
/// même empreinte, qu'elle en a au moins un de plus, et mêmes options.
#[test]
fn prolongement() {
    let wire = vec![um("a"), um("b"), um(SENTINEL)];
    let pre = perf_prefix(&wire);
    let run = Run {
        id: 1,
        n: 2,
        prefix: pre[1],
        opts: 7,
    };
    let real = perf_prefix(&[um("a"), um("b"), um("vraie question")]);
    assert!(run.matches(&real, 7));
    assert!(!run.matches(&real, 8), "autres options");
    assert!(!run.matches(&real[..2], 7), "rien de plus que le préfixe");
    let other = perf_prefix(&[um("a"), um("c"), um("vraie question")]);
    assert!(!run.matches(&other, 7), "préfixe différent");
    let longer = perf_prefix(&[um("a"), um("b"), um("x"), um("y")]);
    assert!(run.matches(&longer, 7));
    assert!(!Run { n: 0, ..run }.matches(&real, 7));
}

/// Un seul en vol ; gardé par la requête qui le prolonge ; annulé (par le
/// Go ou par une autre requête) ; la place rendue à sa fin seulement.
#[test]
fn etat_partage() {
    let p = Prewarm::default();
    assert!(!p.live());
    let (cancelled, done) = p.register().expect("libre");
    assert!(p.live());
    assert!(p.register().is_none(), "deux en vol");
    // Pas encore inscrit au Go : rien à garder, mais annulable.
    assert!(!p.keeps(&[1, 2, 3], 7));
    p.set_run(Run {
        id: 4,
        n: 2,
        prefix: 2,
        opts: 7,
    });
    assert!(p.keeps(&[1, 2, 3], 7));
    assert!(!p.keeps(&[1, 2, 3], 8));
    assert!(!p.cancel_id(9), "annulation d'un autre préchauffage");
    assert!(!*cancelled.borrow());
    assert!(p.cancel_id(4));
    assert!(*cancelled.borrow());
    assert!(p.live(), "la place reste prise jusqu'à la fin");
    p.release(Some(9));
    assert!(p.live(), "fin d'un autre préchauffage");
    p.release(Some(4));
    assert!(!p.live());
    drop(done);
    // stop : annule et rend de quoi attendre la fin.
    let (cancelled, done) = p.register().unwrap();
    let mut wait = p.stop().expect("en vol");
    assert!(*cancelled.borrow() && !p.live());
    let _ = done.send(true);
    assert!(*wait.borrow_and_update());
    assert!(p.stop().is_none());
}

#[test]
fn capacites_par_discussion() {
    let p = Prewarm::default();
    p.note_caps("", json!({"Agent": true}));
    assert!(p.caps_for("").is_none());
    p.note_caps("c1", json!({"Agent": true}));
    p.note_caps("c1", json!({"Agent": false}));
    assert_eq!(p.caps_for("c1"), Some(json!({"Agent": false})));
    for i in 0..CAPS_MAX + 10 {
        p.note_caps(&format!("x{i}"), json!({}));
    }
    assert_eq!(p.caps.lock().unwrap().len(), CAPS_MAX);
}
