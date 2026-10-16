//! Les tests du Go (`backend_strata_test.go`), portés.

use super::*;

fn cfg(kv: &[(&str, &str)]) -> Cfg {
    kv.iter()
        .map(|(k, v)| (k.to_string(), v.to_string()))
        .collect()
}

fn args_of(v: &Value) -> Vec<String> {
    arg_list(v)
}

fn arg_after(args: &[String], flag: &str) -> String {
    args.windows(2)
        .find(|w| w[0] == flag)
        .map(|w| w[1].clone())
        .unwrap_or_default()
}

#[test]
fn reglages_valides_appliques() {
    let base = json!({
        "exe": "/x/engine-cuda12/strata",
        "args": ["--pack", "/p", "--prefill", "8192", "--max-context", "131072"],
        "port": 8080.0,
    });
    let out = build_config(
        &base,
        &cfg(&[("PORT", "8081"), ("CTX", "65536")]),
        "k",
        "ram",
    )
    .unwrap();
    let a = args_of(&out);
    for (flag, want) in [
        ("--prefill", "16384"),
        ("--short-read", "150"),
        ("--prompt-cache-root", "256"),
        ("--max-context", "65536"),
        ("--pack", "/p"),
    ] {
        assert_eq!(arg_after(&a, flag), want, "{flag} : {a:?}");
    }
    assert_eq!(a.join(" ").matches("--prefill").count(), 1);
    assert!(!has_arg(&a, "--remote-expert-opt") && !has_arg(&a, "--expert-cache-device1"));
    assert_eq!(out["port"], 8081);
    assert_eq!(out["api_key"], "k");
    assert_eq!(out["host"], "127.0.0.1");
    assert_eq!(base["args"][3], "8192", "la config de base est modifiée");
}

#[test]
fn carte_d_aide() {
    let base = json!({
        "args": ["--pack", "/p", "--mmap-experts", "--vram-reserve-mib", "700"],
        "gpu": 1.0,
        "vision": { "exe": "v", "gpu": true },
    });
    let out = build_config(&base, &cfg(&[("STRATA_HELPER_GPU", "0")]), "", "ram").unwrap();
    let a = args_of(&out);
    assert_eq!(arg_after(&a, "--expert-cache-device1"), "auto");
    assert!(has_arg(&a, "--remote-expert-opt"));
    assert!(!has_arg(&a, "--vram-reserve-mib") && !has_arg(&a, "700"));
    assert!(out.get("api_key").is_none());
    assert!(out.get("gpu").is_none());
    assert_eq!(out["vision"]["cuda_device"], 0);
    assert_eq!(out["env"]["STRATA_PLE_BATCH"], "0");
    assert!(build_config(&json!({}), &Cfg::new(), "", "ram").is_err());
}

#[test]
fn cartes_cuda() {
    for (main, helper, want) in [
        ("1", "0", "1,0"),
        ("0", "-1", "0"),
        ("0", "", "0"),
        ("", "", ""),
        ("-1", "0", ""),
    ] {
        assert_eq!(
            cuda_devices(&cfg(&[
                ("STRATA_MAIN_GPU", main),
                ("STRATA_HELPER_GPU", helper)
            ])),
            want
        );
    }
}

fn machine() -> Env {
    Env {
        supported: true,
        main: 1,
        helper: 0,
        ram_gb: 47.0,
        avail_gb: 47.0,
        disk_free_gb: 400.0,
        gpus: vec![
            Gpu {
                index: 0,
                name: String::new(),
                vram_gb: 8.0,
                arch: 86,
            },
            Gpu {
                index: 1,
                name: String::new(),
                vram_gb: 16.0,
                arch: 120,
            },
        ],
        ..Env::default()
    }
}

#[test]
fn recommandation() {
    let d = tempfile::tempdir().unwrap();
    let p = Paths {
        home: d.path().to_path_buf(),
    };
    let mut env = machine();
    assert_eq!(recommend(&p, "swift", &env), "IQ3_XXS");
    assert_eq!(recommend(&p, "qwen", &env), "IQ3_XXS");
    assert_eq!(recommend(&p, "autre", &env), "");
    let f = fit_for(quant("IQ3_XXS").unwrap(), &env, 0.0);
    assert!(f.ok && f.mmap && !f.drop);
    env.ram_gb = 64.0;
    env.avail_gb = 64.0;
    assert_eq!(recommend(&p, "qwen", &env), "IQ3_S");
    let small = Env {
        supported: true,
        main: 0,
        helper: -1,
        ram_gb: 16.0,
        disk_free_gb: 400.0,
        gpus: vec![Gpu {
            index: 0,
            name: String::new(),
            vram_gb: 8.0,
            arch: 86,
        }],
        ..Env::default()
    };
    assert_eq!(recommend(&p, "swift", &small), "");
    env.disk_free_gb = 50.0;
    assert!(!fit_for(quant("Q2_0").unwrap(), &env, 0.0).ok);
}

#[test]
fn noms() {
    assert_eq!(
        preset_name("swift", "IQ3_XXS"),
        "STRATA FLASH NEXT SWIFT 1.5 IQ3_XXS"
    );
    assert_eq!(
        preset_name("qwen", "Q2_0"),
        "STRATA FLASH NEXT CLASSIQUE Q2_0"
    );
    assert_eq!(family_label("qwen"), "Qwen3.8 Flash Next");
    assert_eq!(family_label("swift"), "Qwen3.8 Flash Next Swift 1.5");
    assert_eq!(family_label("x"), "x");
    assert_eq!(config_tag("/a/strata-swift-iq3_xxs.json"), "swift-iq3_xxs");
    assert_eq!(config_tag("autre.json"), "autre");
}

#[test]
fn donnees_deja_la() {
    let mut env = machine();
    env.avail_gb = 0.0;
    env.disk_free_gb = 32.0;
    assert!(fit_for(quant("IQ3_XXS").unwrap(), &env, 120.0).ok);
    assert!(!fit_for(quant("IQ3_XXS").unwrap(), &env, 0.0).ok);
}

#[test]
fn reglages_du_preset() {
    let base = json!({ "args": ["--kv", "int8", "--spec", "4"] });
    let out = build_config(
        &base,
        &cfg(&[("STRATA_KV", "fp16"), ("STRATA_SPEC", "3")]),
        "",
        "ram",
    )
    .unwrap();
    let a = args_of(&out);
    assert_eq!(arg_after(&a, "--kv"), "fp16");
    assert_eq!(arg_after(&a, "--spec"), "3");
}

#[test]
fn deja_sur_le_disque() {
    let d = tempfile::tempdir().unwrap();
    let p = Paths {
        home: d.path().to_path_buf(),
    };
    let write = |rel: &str, n: usize| {
        let f = p.data_dir().join(rel);
        std::fs::create_dir_all(f.parent().unwrap()).unwrap();
        std::fs::write(f, vec![0u8; n]).unwrap();
    };
    write("models/swift-IQ3_XXS/a.gguf", 3000);
    write("packs/swift-iq3_xxs/experts.bin", 2000);
    write("mtp/rt/x", 500);
    write("models/IQ2_XS/b.gguf", 7000);
    assert_eq!((have_gb(&p, "swift", "IQ3_XXS") * 1e9).round(), 5500.0);
    assert_eq!((have_gb(&p, "swift", "IQ2_XS") * 1e9).round(), 500.0);
}

#[test]
fn reglages() {
    let input =
        "# NAME=x\nENGINE=strata\nCTX=131072\nSTRATA_KV=fp16\nSTRATA_SPEC=3\nSTRATA_HELPER_GPU=0\n";
    let r = SettingsReq {
        ctx: 65536,
        kv: "int8".into(),
        spec: 4,
        ..SettingsReq::default()
    };
    let out = apply_settings(input, &r).unwrap();
    let c = crate::presets::parse_env(&out);
    for (k, v) in [
        ("CTX", "65536"),
        ("STRATA_KV", "int8"),
        ("STRATA_SPEC", "4"),
        ("STRATA_VISION", "0"),
        ("STRATA_HELPER", "0"),
        ("STRATA_HELPER_GPU", "0"),
        ("ENGINE", "strata"),
    ] {
        assert_eq!(c.get(k).map(String::as_str), Some(v), "{k} : {out:?}");
    }
    assert_eq!(out.matches("CTX=").count(), 1);
    // Les bornes acceptées.
    for (ctx, spec) in [(32768, 2), (MAX_CTX, 4)] {
        let ok = SettingsReq {
            ctx,
            kv: "fp16".into(),
            spec,
            ..SettingsReq::default()
        };
        assert!(apply_settings(input, &ok).is_ok(), "{ctx} {spec}");
    }
    assert!(!helper_on(&c));
    assert_eq!(
        cuda_devices(&cfg(&[
            ("STRATA_MAIN_GPU", "1"),
            ("STRATA_HELPER_GPU", "0"),
            ("STRATA_HELPER", "0")
        ])),
        "1"
    );
    let base = json!({ "args": ["--vision", "--pack", "/p"], "vision": { "exe": "v" } });
    let b = build_config(&base, &c, "", "ram").unwrap();
    let a = args_of(&b);
    assert!(!has_arg(&a, "--vision") && b.get("vision").is_none());
    assert!(!has_arg(&a, "--expert-cache-device1"));
    for (ctx, kv, spec, why) in [
        (1000, "fp16", 3, "contexte hors limites : 1000"),
        (32768, "q2", 3, "cache KV inconnu : q2"),
        (32768, "fp16", 9, "MTP hors limites : 9"),
        (36864 + 1, "fp16", 3, "contexte hors limites : 36865"),
        (MAX_CTX + 4096, "fp16", 3, "contexte hors limites : 266240"),
        (32768, "fp16", 1, "MTP hors limites : 1"),
        (32768, "fp16", 5, "MTP hors limites : 5"),
        (28672, "fp16", 3, "contexte hors limites : 28672"),
    ] {
        let bad = SettingsReq {
            ctx,
            kv: kv.into(),
            spec,
            ..SettingsReq::default()
        };
        assert_eq!(apply_settings(input, &bad).unwrap_err(), why);
    }
}

#[test]
fn moteur_du_preset() {
    for (v, want) in [
        ("", ENGINE_LLAMACPP),
        ("llamacpp", ENGINE_LLAMACPP),
        ("strata", ENGINE_STRATA),
        (" Strata ", ENGINE_STRATA),
        ("moe", ENGINE_STRATA),
        ("autre", ENGINE_LLAMACPP),
    ] {
        assert_eq!(engine_of(&cfg(&[("ENGINE", v)])), want, "{v:?}");
    }
}

#[test]
fn placement_des_experts() {
    let d = tempfile::tempdir().unwrap();
    let p = Paths {
        home: d.path().to_path_buf(),
    };
    let pack = d.path().join("pack");
    std::fs::create_dir_all(&pack).unwrap();
    let args = vec!["--pack".to_string(), pack.to_string_lossy().into_owned()];
    let mut c = cfg(&[
        ("STRATA_CONFIG", "/x/strata-swift-iq3_xxs.json"),
        ("STRATA_HELPER_GPU", "1"),
        ("STRATA_DROP", "1"),
    ]);
    for (experts, avail, want) in [
        ("", 44.0, "disk"),
        ("", 60.0, "ram"),
        ("auto", 52.8, "disk"),
        ("disk", 60.0, "disk"),
        ("ram", 20.0, "ram"),
    ] {
        c.insert("STRATA_EXPERTS".into(), experts.into());
        assert_eq!(
            resolve_experts(&p, &c, &args, avail, 12.0),
            want,
            "{experts} {avail}"
        );
    }
    std::fs::write(pack.join("experts.bin"), "x").unwrap();
    std::fs::create_dir_all(p.src_dir().join("engine-cuda12")).unwrap();
    std::fs::write(p.engine_marker(), format!("{ENGINE_ASSET}\n")).unwrap();
    c.insert("STRATA_EXPERTS".into(), String::new());
    assert_eq!(resolve_experts(&p, &c, &args, 44.0, 12.0), "drop");
    assert_eq!(resolve_experts(&p, &c, &args, 30.0, 12.0), "arena");
    c.insert("STRATA_HELPER".into(), "0".into());
    assert_eq!(resolve_experts(&p, &c, &args, 44.0, 12.0), "arena");
    c.insert("STRATA_EXPERTS".into(), "disk".into());
    assert_eq!(resolve_experts(&p, &c, &args, 60.0, 12.0), "arena");
    assert_eq!(
        resolve_experts(
            &p,
            &cfg(&[("STRATA_CONFIG", "/x/autre.json")]),
            &[],
            500.0,
            0.0
        ),
        "disk"
    );
}

#[test]
fn modes_des_experts() {
    let base = json!({
        "args": ["--pack", "/p", "--resident-experts", "--pcie-frac", "0.3"],
        "env": { "X": "1" },
    });
    let c = cfg(&[("STRATA_HELPER_GPU", "1")]);
    let out = build_config(&base, &c, "", "disk").unwrap();
    let a = args_of(&out);
    assert!(has_arg(&a, "--mmap-experts") && !has_arg(&a, "--resident-experts"));
    assert_eq!(arg_after(&a, "--pcie-frac"), "0");
    assert!(out["env"].get("STRATA_ARENA_MMAP").is_none());
    assert_eq!(out["env"]["X"], "1");
    let out = build_config(&base, &c, "", "ram").unwrap();
    let a = args_of(&out);
    assert!(!has_arg(&a, "--mmap-experts") && !has_arg(&a, "--resident-experts"));
    assert!(out["env"].get("STRATA_REMOTE_DROP").is_none());
    let out = build_config(&base, &c, "", "arena").unwrap();
    assert_eq!(arg_after(&args_of(&out), "--pcie-frac"), "0");
    assert_eq!(out["env"]["STRATA_ARENA_MMAP"], "1");
    let out = build_config(&base, &c, "", "drop").unwrap();
    assert!(!has_arg(&args_of(&out), "--pcie-frac"));
    assert_eq!(out["env"]["STRATA_REMOTE_DROP"], "1");
    assert_eq!(out["env"]["STRATA_STAGER_THREADS"], "8");
    let out = build_config(&base, &cfg(&[("STRATA_HELPER_GPU", "-1")]), "", "drop").unwrap();
    assert!(out["env"].get("STRATA_REMOTE_DROP").is_none());
}

// Moteur 1.1 (`TestStrataBuildConfigHelperPrompt`).
#[test]
fn lecture_des_prompts_par_la_carte_d_aide() {
    let base = json!({ "args": ["--pack", "/p"] });
    let out = build_config(&base, &cfg(&[("STRATA_HELPER_GPU", "0")]), "", "ram").unwrap();
    assert_eq!(arg_after(&args_of(&out), "--short-read"), "48");
    assert_eq!(out["env"]["STRATA_PREFILL_HELPER_SOURCE"], "1");
    for c in [
        Cfg::new(),
        cfg(&[("STRATA_HELPER_GPU", "0"), ("STRATA_HELPER", "0")]),
    ] {
        let out = build_config(&base, &c, "", "ram").unwrap();
        assert_eq!(arg_after(&args_of(&out), "--short-read"), "150", "{c:?}");
        assert!(out["env"].get("STRATA_PREFILL_HELPER_SOURCE").is_none());
    }
}

// `TestStrataBuildConfigDropReserveAndCtx`.
#[test]
fn hors_ram_reserve_et_contexte() {
    let base = json!({ "args": ["--pack", "/p"], "env": {} });
    let c = cfg(&[("STRATA_HELPER_GPU", "0"), ("CTX", "167936")]);
    let out = build_config(&base, &c, "", "drop").unwrap();
    assert_eq!(out["env"]["STRATA_REMOTE_DROP_RESERVE_MIB"], "640");
    assert_eq!(arg_after(&args_of(&out), "--max-context"), "131072");
    for mode in ["ram", "arena", "disk"] {
        let out = build_config(&base, &c, "", mode).unwrap();
        assert_eq!(
            arg_after(&args_of(&out), "--max-context"),
            "167936",
            "{mode}"
        );
        assert!(out["env"].get("STRATA_REMOTE_DROP_RESERVE_MIB").is_none());
    }
}

#[test]
fn reglages_des_experts() {
    let input = "ENGINE=strata\nSTRATA_EXPERTS=auto\n";
    let mut r = SettingsReq {
        ctx: 131072,
        kv: "fp16".into(),
        spec: 3,
        experts: "disk".into(),
        ..SettingsReq::default()
    };
    let out = apply_settings(input, &r).unwrap();
    assert_eq!(crate::presets::parse_env(&out)["STRATA_EXPERTS"], "disk");
    r.experts = "nvme".into();
    assert_eq!(
        apply_settings(input, &r).unwrap_err(),
        "placement des experts inconnu : nvme"
    );
    r.experts = String::new();
    let out = apply_settings(input, &r).unwrap();
    assert_eq!(crate::presets::parse_env(&out)["STRATA_EXPERTS"], "auto");
}

#[test]
fn nombres_a_la_go() {
    assert_eq!(go_g(8192.0), "8192");
    assert_eq!(go_g(123456.0), "123456");
    assert_eq!(go_g(1e6), "1e+06");
    assert_eq!(go_g(1234567.0), "1.234567e+06");
    assert_eq!(go_g(0.3), "0.3");
    assert_eq!(go_g(0.0001), "0.0001");
    assert_eq!(go_g(0.00001), "1e-05");
    assert_eq!(go_g(-2.5), "-2.5");
    assert_eq!(go_sprint(&json!("x")), "x");
    assert_eq!(go_sprint(&json!(true)), "true");
    assert_eq!(go_num(34.0), json!(34));
    assert_eq!(go_num(35.5), json!(35.5));
}

#[test]
fn ligne_par_ligne() {
    let mut s = api::SinkWriter::default();
    assert_eq!(s.write(b"a\nb"), vec!["a"]);
    assert_eq!(s.write(b"c\r"), Vec::<String>::new());
    assert_eq!(s.write(b"\n10%\r20%\r"), vec!["bc", "10%"]);
    assert_eq!(s.write(b"fin\r\n"), vec!["fin"]);
}

/// La RAM du moteur Strata en marche : l'arbre du service, lu dans /proc.
#[test]
fn ram_du_moteur_en_marche() {
    let home = tempfile::tempdir().unwrap();
    let root = tempfile::tempdir().unwrap();
    let probe = crate::backends::probe::Probe {
        path: Some(vec![]),
        root: root.path().to_path_buf(),
    };
    let st = api::Strata::new(home.path().to_path_buf(), probe);
    let status = |pid: &str, ppid: i64, kb: i64| {
        let d = root.path().join("proc").join(pid);
        std::fs::create_dir_all(&d).unwrap();
        std::fs::write(
            d.join("status"),
            format!("Name:\tx\nPPid:\t{ppid}\nVmRSS:\t   {kb} kB\n"),
        )
        .unwrap();
    };
    status("100", 1, 1_048_576);
    status("101", 100, 2048);
    status("102", 101, 1024);
    status("200", 1, 999_999);
    std::fs::create_dir_all(root.path().join("proc/self")).unwrap();
    let strata = cfg(&[("ENGINE", "strata")]);
    assert_eq!(st.engine_rss_gb(&strata), 0.0, "sans fichier de PID");
    std::fs::write(home.path().join("loki-engine.pid"), "100\n").unwrap();
    let want = (1_048_576.0 + 2048.0 + 1024.0) * 1024.0 / 1e9;
    assert!((st.engine_rss_gb(&strata) - want).abs() < 1e-9);
    assert_eq!(st.engine_rss_gb(&Cfg::new()), 0.0, "llama.cpp actif");
    std::fs::write(
        root.path().join("proc/meminfo"),
        "MemTotal:       8388608 kB\nMemAvailable:   4194304 kB\n",
    )
    .unwrap();
    assert_eq!(st.total_ram_gb(), 8.0);
    assert!((st.mem_available_gb() - 4194304.0 * 1024.0 / 1e9).abs() < 1e-9);
}

/// Les drapeaux posés une seule fois, où qu'ils soient dans la liste.
#[test]
fn drapeaux_uniques() {
    let v = |a: &[&str]| a.iter().map(|s| s.to_string()).collect::<Vec<_>>();
    assert_eq!(
        set_arg(v(&["--a", "1", "--kv", "int8"]), "--kv", "fp16"),
        v(&["--a", "1", "--kv", "fp16"])
    );
    assert_eq!(
        set_arg(v(&["--kv"]), "--kv", "x"),
        v(&["--kv", "--kv", "x"])
    );
    assert_eq!(
        drop_flag_value(v(&["--a", "1", "--b", "2"]), "--b"),
        v(&["--a", "1"])
    );
    assert_eq!(drop_flag(v(&["--a", "--b", "--a"]), "--a"), v(&["--b"]));
    let base = json!({ "args": ["--remote-expert-opt", "--pack", "/p"] });
    let out = build_config(&base, &cfg(&[("STRATA_HELPER_GPU", "1")]), "", "drop").unwrap();
    let a = args_of(&out);
    assert_eq!(
        a.iter().filter(|x| *x == "--remote-expert-opt").count(),
        1,
        "{a:?}"
    );
    assert_eq!(out["env"]["STRATA_STAGER_RING"], "32");
    assert_eq!(out["env"]["STRATA_NO_LARGEPAGES"], "1");
}
