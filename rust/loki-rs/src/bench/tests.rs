//! Tests du bench : ceux du Go (`llm_bench_test.go`), portés, et les bornes.

use super::*;

fn cfg(pairs: &[(&str, &str)]) -> Cfg {
    pairs
        .iter()
        .map(|(k, v)| (k.to_string(), v.to_string()))
        .collect()
}

#[test]
fn profondeur_choisie() {
    let warm = 2 * 512 + WARM_GEN;
    let turns = TURNS * (APPEND_TOKENS + GEN_TOKENS);
    for (ctx, cpu, want, skipped, what) in [
        (131072, false, DEPTH_MAX, false, "grand contexte : plafond"),
        (
            131072,
            true,
            DEPTH_CPU,
            false,
            "poids sur CPU : plafond réduit",
        ),
        (65536, false, 32768, false, "moitié du contexte"),
        (
            16384,
            false,
            16384 - turns - GEN_TOKENS - warm - MARGIN,
            false,
            "les tours doivent tenir",
        ),
        (8192, false, 0, true, "trop petit"),
        (0, false, 0, true, "inconnu"),
    ] {
        let (got, why) = depth_for(ctx, cpu, warm);
        assert_eq!((got, !why.is_empty()), (want, skipped), "{what}");
        if got > 0 {
            assert!(got + turns + GEN_TOKENS <= ctx, "{what} : déborde");
        }
    }
    assert_eq!(depth_for(0, false, 0).1, "taille de contexte inconnue");
    assert_eq!(
        depth_for(8192, false, warm).1,
        "contexte de 8192 jetons trop petit pour la mesure en profondeur"
    );
    // Juste au seuil : 4096 tient, 4095 non.
    let at = |d: i64| d + turns + GEN_TOKENS + warm + MARGIN;
    assert_eq!(depth_for(at(4096), false, warm).0, 4096);
    assert_eq!(depth_for(at(4095), false, warm).0, 0);
}

#[test]
fn types_de_cache() {
    for (k, v, label, quant) in [
        ("", "", "f16", false),
        ("f16", "", "f16", false),
        ("bf16", "bf16", "bf16", false),
        ("q8_0", "q8_0", "q8_0", true),
        ("q8_0", "", "q8_0/f16", true),
        ("", "q4_0", "f16/q4_0", true),
        (" Q8_0 ", "q8_0", "q8_0", true),
    ] {
        assert_eq!(kv_label(k, v), (label.to_string(), quant), "{k:?} {v:?}");
    }
    // Le -ctk d'EXTRA_ARGS l'emporte, comme au lancement.
    let s = setup_from(
        &cfg(&[
            ("KV_TYPE", "f16"),
            ("EXTRA_ARGS", "-ctk q8_0 -ctv q8_0 --n-cpu-moe 20"),
        ]),
        &HashMap::new(),
        &EffortFallbacks::new(),
    );
    assert_eq!(
        (s.kv.as_str(), s.kv_quant, s.cpu_placed),
        ("q8_0", true, true)
    );
    let s = setup_from(&cfg(&[]), &HashMap::new(), &EffortFallbacks::new());
    assert!(!s.cpu_placed);
    // Placement par l'environnement de llama-server.
    let env = HashMap::from([("LLAMA_ARG_CPU_MOE".to_string(), "1".to_string())]);
    let s = setup_from(&cfg(&[]), &env, &EffortFallbacks::new());
    assert!(s.cpu_placed);
}

#[test]
fn micro_lot_du_preset() {
    let ub = |pairs: &[(&str, &str)]| {
        setup_from(&cfg(pairs), &HashMap::new(), &EffortFallbacks::new()).ub
    };
    assert_eq!(ub(&[]), 512);
    assert_eq!(ub(&[("UBATCH", "1024")]), 1024);
    assert_eq!(ub(&[("UBATCH", " 2048 ")]), 2048);
    assert_eq!(ub(&[("UBATCH", "0")]), 512);
    assert_eq!(ub(&[("UBATCH", "x")]), 512);
    assert_eq!(ub(&[("EXTRA_ARGS", "-ub 256"), ("UBATCH", "1024")]), 256);
    assert_eq!(ub(&[("EXTRA_ARGS", "--ubatch-size 128")]), 128);
    assert_eq!(ub(&[("EXTRA_ARGS", "-ub -4"), ("UBATCH", "64")]), 64);
    let gpus = setup_from(
        &cfg(&[("CUDA_VISIBLE_DEVICES", " 0,1 ")]),
        &HashMap::new(),
        &EffortFallbacks::new(),
    )
    .gpus;
    assert_eq!(gpus, "0,1");
}

#[test]
fn requete_du_bench() {
    let msgs = vec![("user".to_string(), "x".to_string())];
    let fb = EffortFallbacks::new();
    let p = setup_from(
        &cfg(&[("TEMP", "0.6"), ("TOP_P", "0.95"), ("TOP_K", "20")]),
        &HashMap::new(),
        &fb,
    )
    .payload(&msgs, 10, true);
    assert_eq!(p["temperature"], json!(0.6));
    assert_eq!(p["top_p"], json!(0.95));
    assert_eq!(p["top_k"], json!(20));
    assert_eq!(p["seed"], json!(SEED));
    assert_eq!(p["max_tokens"], json!(10));
    assert_eq!(p["model"], json!("loki"));
    assert_eq!(p["messages"], json!([{ "role": "user", "content": "x" }]));
    assert_eq!(
        (p["cache_prompt"].clone(), p["stream"].clone()),
        (json!(true), json!(false))
    );
    for k in [
        "chat_template_kwargs",
        "reasoning_effort",
        "ignore_eos",
        "id_slot",
    ] {
        assert!(p.get(k).is_none(), "{k}");
    }
    let s = setup_from(&cfg(&[]), &HashMap::new(), &fb);
    let p = s.payload(&msgs, 10, false);
    assert_eq!(p["temperature"], json!(0.7));
    assert_eq!(p["cache_prompt"], json!(false));
    assert!(p.get("top_k").is_none());
    let mut s = setup_from(&cfg(&[("REASONING_EFFORT", "high")]), &HashMap::new(), &fb);
    assert_eq!(s.payload(&msgs, 1, true)["reasoning_effort"], json!("high"));
    s.side_slot = true;
    assert_eq!(s.payload(&msgs, 1, true)["id_slot"], json!(1));
    // Raisonnement coupé : le gabarit le dit.
    let s = setup_from(&cfg(&[("REASONING", "off")]), &HashMap::new(), &fb);
    assert_eq!(
        s.payload(&msgs, 1, true)["chat_template_kwargs"],
        json!({ "enable_thinking": false })
    );
    let s = setup_from(&cfg(&[("REASONING_EFFORT", "none")]), &HashMap::new(), &fb);
    assert!(s.off);
    // Une traduction apprise est envoyée d'emblée.
    let c = cfg(&[("REASONING_EFFORT", "high"), ("MODEL", "/m/Q.gguf")]);
    fb.remember(&c, "high", "xhigh");
    let s = setup_from(&c, &HashMap::new(), &fb);
    assert_eq!((s.want.as_str(), s.effort.as_str()), ("high", "xhigh"));
}

#[test]
fn morceaux_du_corpus() {
    let text = "ééé ligne\n".repeat(100);
    let mut off = 0;
    while off < text.len() {
        let (chunk, next) = slice(&text, off, 37);
        assert!(!chunk.is_empty(), "à {off}");
        if next < text.len() {
            assert!(chunk.ends_with('\n'), "coupé hors fin de ligne : {chunk:?}");
        }
        off = next;
    }
    assert_eq!(slice(&text, text.len(), 10).0, "");
    assert_eq!(slice(&text, 0, 0).0, "");
    assert_eq!(slice("abc", 0, 99), ("abc", 3));
    // Sans fin de ligne assez loin : coupé à la taille, jamais dans un
    // caractère.
    assert_eq!(slice("éééééé", 0, 3), ("é", 2));
    assert_eq!(slice("ab\ncdefgh", 0, 8), ("ab\ncdefg", 8));
    assert_eq!(slice("abcde\nfgh", 0, 8), ("abcde\n", 6));
    assert_eq!(slice("abcd\nfgh", 0, 8), ("abcd\nfgh", 8));
    assert_eq!(line_start("ab\ncd", 1), 3);
    assert_eq!(line_start("ab\ncd", 3), 3);
    assert_eq!(line_start("ab\ncd", 0), 0);
    assert_eq!(line_start("ab\ncd", -5), 0);
    assert_eq!(line_start("ab\ncd", 4), 5);
    assert_eq!(line_start("ab\ncd", 9), 5);
}

#[test]
fn reprise_du_cache() {
    let t = |cached, expected| Turn {
        cached,
        expected,
        ..Default::default()
    };
    let (r, note) = reuse(&[t(1000, 1004), t(3000, 3000)], false);
    assert!(r > 0.99 && note.is_empty(), "{r} {note}");
    assert_eq!(reuse(&[t(1000, 1016)], false).1, "");
    assert!(!reuse(&[t(1000, 1017)], false).1.is_empty());
    let part = [t(512, 3000)];
    assert!(reuse(&part, true).1.contains("hybride"));
    assert!(reuse(&part, false).1.contains("gabarit"));
    assert_eq!(reuse(&part, false).0, 512.0 / 3000.0);
    let (r, note) = reuse(&[t(-1, 3000)], false);
    assert_eq!(r, -1.0);
    assert!(!note.is_empty());
    assert_eq!(reuse(&[t(10, 0)], false).0, -1.0);
    // Un cache plus grand que l'attendu compte pour l'attendu.
    assert_eq!(reuse(&[t(5000, 3000)], false).0, 1.0);
}

#[test]
fn relectures_du_disque() {
    assert_eq!(thrash_hint(100 << 20, 256, 0), "");
    assert_eq!(thrash_hint(0, 256, 0), "");
    assert_eq!(thrash_hint(1 << 40, 0, 0), "");
    assert_eq!(thrash_hint(512 << 20, 256, 0), "");
    let h = thrash_hint(3 << 30, 768, 60 << 30);
    assert_eq!(
        h,
        "possible thrash : 1.0 Go relus du disque par 256 jetons générés (RSS 60.0 Go) — le modèle ne tient sans doute pas en RAM"
    );
    assert!(!thrash_hint((512 << 20) + 1, 256, 0).is_empty());
}

#[test]
fn brouillon() {
    assert_eq!(draft_text(None), "");
    assert_eq!(draft_text(Some(&Draft { n: 0, accepted: 0 })), "");
    assert_eq!(
        draft_text(Some(&Draft {
            n: 30,
            accepted: 21
        })),
        "   brouillon accepté 70 % (21/30)"
    );
    let d = draft_add(None, Some(Draft { n: 10, accepted: 7 }));
    let d = draft_add(d, None);
    let d = draft_add(d, Some(Draft { n: 5, accepted: 1 }));
    assert_eq!(d, Some(Draft { n: 15, accepted: 8 }));
}

#[test]
fn durees_du_go() {
    let ms = Duration::from_millis;
    assert_eq!(go_dur(Duration::ZERO), "0s");
    assert_eq!(go_dur(Duration::from_nanos(7)), "7ns");
    assert_eq!(go_dur(Duration::from_nanos(1500)), "1.5µs");
    assert_eq!(go_dur(ms(150)), "150ms");
    assert_eq!(go_dur(Duration::from_micros(1500)), "1.5ms");
    assert_eq!(go_dur(ms(1000)), "1s");
    assert_eq!(go_dur(ms(1500)), "1.5s");
    assert_eq!(go_dur(Duration::from_secs(6 * 60)), "6m0s");
    assert_eq!(go_dur(Duration::from_secs(3600)), "1h0m0s");
    assert_eq!(go_dur(Duration::from_secs(3661)), "1h1m1s");
    assert_eq!(
        go_dur(Duration::from_nanos(61_000_000_001)),
        "1m1.000000001s"
    );
}

#[test]
fn reponse_du_moteur() {
    let full = br#"{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":120,"prompt_tokens_details":{"cached_tokens":30}},"timings":{"prompt_n":100,"prompt_ms":50,"predicted_n":10,"predicted_ms":200,"draft_n":4,"draft_n_accepted":3}}"#;
    let r = parse_reply(full).unwrap();
    assert_eq!(r.turn.new, 100);
    assert_eq!(r.turn.prompt_per_second, 2000.0);
    assert_eq!(r.turn.predicted_per_second, 50.0);
    assert_eq!(r.turn.cached, 30);
    assert_eq!(r.turn.draft, Some(Draft { n: 4, accepted: 3 }));
    assert_eq!((r.total, r.content.as_str()), (120, "ok"));
    assert_eq!(r.prompt_total(), 120);
    // cache_n l'emporte ; débits donnés par le moteur gardés.
    let r = parse_reply(br#"{"timings":{"prompt_n":"8","prompt_ms":4,"predicted_n":2,"predicted_ms":0,"cache_n":5,"prompt_per_second":7,"draft_n":0,"draft_n_accepted":0}}"#).unwrap();
    assert_eq!((r.turn.cached, r.turn.prompt_per_second), (5, 7.0));
    assert_eq!(r.turn.predicted_per_second, 0.0);
    assert_eq!(r.turn.draft, None);
    assert_eq!(r.prompt_total(), 13);
    // Cache inconnu : -1, et le total sans lui.
    let r = parse_reply(
        br#"{"timings":{"prompt_n":8,"prompt_ms":4,"predicted_n":2,"predicted_ms":1}} trailing"#,
    )
    .unwrap();
    assert_eq!(
        (r.turn.cached, r.prompt_total(), r.content.as_str()),
        (-1, 8, "")
    );
    for bad in [
        &br#"{"timings":{"prompt_n":8,"prompt_ms":4,"predicted_n":2}}"#[..],
        br#"{"timings":{"prompt_n":-1,"prompt_ms":4,"predicted_n":2,"predicted_ms":1}}"#,
        br#"{"timings":"x"}"#,
        b"pas du json",
        br#"{"timings":{"prompt_n":1e13,"prompt_ms":4,"predicted_n":2,"predicted_ms":1}}"#,
    ] {
        assert_eq!(
            parse_reply(bad),
            Err(ReqError::Other(ERR_NO_TIMINGS.into())),
            "{}",
            String::from_utf8_lossy(bad)
        );
    }
}

#[test]
fn erreur_http() {
    let e = ReqError::Http {
        method: "POST".into(),
        path: "/v1/chat/completions".into(),
        status: "500 Internal Server Error".into(),
        body: format!("  {}é{}  ", "x".repeat(299), "y".repeat(10)),
    };
    // 300 octets au plus, jamais au milieu d'un caractère.
    assert_eq!(
        e.to_string(),
        format!(
            "POST /v1/chat/completions : 500 Internal Server Error {}",
            "x".repeat(299)
        )
    );
    assert_eq!(go_op("POST"), "Post");
    assert_eq!(go_op("get"), "Get");
}

#[test]
fn flottants_du_go() {
    let t = Turn {
        prompt_ms: 3836.0,
        prompt_per_second: 1037.5,
        ..Default::default()
    };
    let v = serde_json::to_string(&t).unwrap();
    assert!(v.contains(r#""prompt_ms":3836,"#), "{v}");
    assert!(v.contains(r#""prompt_per_second":1037.5"#), "{v}");
    let d = Depth {
        reuse: -1.0,
        ..Default::default()
    };
    assert_eq!(
        serde_json::to_value(&d).unwrap(),
        json!({ "ctx": 0, "target": 0, "reuse": -1 })
    );
}
