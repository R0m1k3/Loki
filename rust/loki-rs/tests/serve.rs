//! Lot 14 : `loki serve` composé en Rust (`LOKI_SERVE=rust`).
//!
//! Le journal tiré du Go (`internal/loki/parity_serve_test.go` →
//! `tests/parity_serve/serve.json`) est rejoué : ligne de commande,
//! environnement et notes de ~1 900 lancements (machines × presets × aides de
//! moteur × variables), puis les sondes pures.

use std::collections::{BTreeMap, HashMap};

use loki_rs::gguf::GgufMeta;
use loki_rs::serve::{self, CpuBudget, SplitDev, SysInfo};
use serde_json::Value;

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_serve/serve.json")).expect("journal")
}

fn i(v: &Value, k: &str) -> i64 {
    v.get(k).and_then(Value::as_i64).unwrap_or(0)
}
fn st(v: &Value, k: &str) -> String {
    v.get(k).and_then(Value::as_str).unwrap_or("").to_string()
}
fn b(v: &Value, k: &str) -> bool {
    v.get(k).and_then(Value::as_bool).unwrap_or(false)
}

fn gguf_from(v: &Value) -> Option<GgufMeta> {
    if v.is_null() {
        return None;
    }
    Some(GgufMeta {
        version: i(v, "Version"),
        arch: st(v, "Arch"),
        block_count: i(v, "BlockCount"),
        next_n: i(v, "NextN"),
        context_length: i(v, "ContextLength"),
        expert_count: i(v, "ExpertCount"),
        key_len: i(v, "KeyLen"),
        val_len: i(v, "ValLen"),
        head_count_kv: i(v, "HeadCountKV"),
        head_count_kv_layers: v
            .get("HeadCountKVLayers")
            .and_then(Value::as_array)
            .map(|a| a.iter().map(|x| x.as_i64().unwrap()).collect()),
        full_attn_interval: i(v, "FullAttnInterval"),
        hybrid: b(v, "Hybrid"),
        embedding_length: i(v, "EmbeddingLength"),
        head_count: i(v, "HeadCount"),
        ssm_conv: i(v, "SSMConv"),
        ssm_inner: i(v, "SSMInner"),
        ssm_state: i(v, "SSMState"),
        ssm_groups: i(v, "SSMGroups"),
        has_next_n_tensor: b(v, "HasNextNTensor"),
        tensor_count: i(v, "TensorCount"),
    })
}

fn map_from(v: &Value) -> HashMap<String, String> {
    v.as_object()
        .map(|o| {
            o.iter()
                .map(|(k, x)| (k.clone(), x.as_str().unwrap().to_string()))
                .collect()
        })
        .unwrap_or_default()
}

fn si_from(m: &Value) -> SysInfo {
    let cpu = &m["CPU"];
    SysInfo {
        help: st(m, "Help"),
        model: st(m, "Model"),
        mmproj: st(m, "MMProj"),
        api_key: st(m, "APIKey"),
        cpu: CpuBudget {
            n: i(cpu, "N"),
            engine: i(cpu, "Engine"),
            why: st(cpu, "Why"),
        },
        launch_queues: st(m, "LaunchQueues"),
        gpus: i(m, "GPUs"),
        arg_env: map_from(&m["ArgEnv"]),
        user_env: map_from(&m["UserEnv"]),
        gguf: gguf_from(&m["GGUF"]),
        model_bytes: i(m, "ModelBytes"),
        ram_mib: i(m, "RAMMiB"),
        ram_avail_mib: i(m, "RAMAvailMiB"),
        vram_mib: i(m, "VRAMMiB"),
        unified_mem: b(m, "UnifiedMem"),
        engine_vram_mib: i(m, "EngineVRAMMiB"),
        slot_dir: st(m, "SlotDir"),
        engine_build: i(m, "EngineBuild"),
        draft: st(m, "Draft"),
        draft_err: st(m, "DraftErr"),
        draft_gguf: gguf_from(&m["DraftGGUF"]),
        draft_is_model: b(m, "DraftIsModel"),
        spec_auto_blocked: st(m, "SpecAutoBlocked"),
        split_devs: m["SplitDevs"]
            .as_array()
            .map(|a| {
                a.iter()
                    .map(|d| SplitDev {
                        id: st(d, "ID"),
                        total_mib: i(d, "TotalMiB"),
                    })
                    .collect()
            })
            .unwrap_or_default(),
    }
}

fn strs(v: &Value) -> Vec<String> {
    v.as_array()
        .map(|a| a.iter().map(|x| x.as_str().unwrap().to_string()).collect())
        .unwrap_or_default()
}

#[test]
fn parite_des_lignes_de_commande() {
    let fx = fixture();
    let machines = fx["machines"].as_array().unwrap();
    let presets = fx["presets"].as_array().unwrap();
    let helps = fx["helps"].as_array().unwrap();
    let arg_envs = fx["arg_envs"].as_array().unwrap();
    let user_envs = fx["user_envs"].as_array().unwrap();
    let mut n = 0;
    for c in fx["cases"].as_array().unwrap() {
        let (m, p, h, e) = (
            i(c, "m") as usize,
            i(c, "c") as usize,
            i(c, "h") as usize,
            i(c, "e") as usize,
        );
        let mut si = si_from(&machines[m]);
        si.help = helps[h].as_str().unwrap().to_string();
        si.arg_env = map_from(&arg_envs[e]);
        si.user_env = if (m + p) % 3 == 1 {
            map_from(&user_envs[1])
        } else {
            HashMap::new()
        };
        let cfg = map_from(&presets[p]);
        let extra = loki_rs::models::split_args(cfg.get("EXTRA_ARGS").map_or("", String::as_str));
        let got = serve::build_serve_args(&cfg, &extra, "/app/llama-server", &si);
        let ctx = format!(
            "machine {m}, preset {p} {:?}, aide {h}, variables {e}",
            presets[p]
        );
        assert_eq!(got.args, strs(&c["args"]), "{ctx} : arguments");
        let want_env: BTreeMap<String, String> = map_from(&c["env"]).into_iter().collect();
        assert_eq!(got.env, want_env, "{ctx} : environnement");
        assert_eq!(got.notes, strs(&c["notes"]), "{ctx} : notes");
        n += 1;
    }
    assert!(n > 1500, "{n} lancements seulement");
}

#[test]
fn parite_des_sondes() {
    let fx = fixture();
    for (input, want) in fx["cpu_list"].as_object().unwrap() {
        let got = serve::parse_cpu_list(input);
        match want {
            Value::String(_) => assert!(got.is_none(), "{input:?}"),
            _ => assert_eq!(
                got.unwrap(),
                want.as_array()
                    .unwrap()
                    .iter()
                    .map(|x| x.as_i64().unwrap())
                    .collect::<Vec<_>>(),
                "{input:?}"
            ),
        }
    }
    for c in fx["cpuset"].as_array().unwrap() {
        let tmp = tempfile::tempdir().unwrap();
        for (p, d) in c["files"].as_object().unwrap() {
            let path = tmp.path().join(p);
            std::fs::create_dir_all(path.parent().unwrap()).unwrap();
            std::fs::write(path, d.as_str().unwrap()).unwrap();
        }
        let got = serve::cpuset_threads(tmp.path());
        assert_eq!(
            (got.n, got.engine, got.why),
            (i(c, "n"), i(c, "engine"), st(c, "why")),
            "{}",
            c["name"]
        );
    }
    for c in fx["cgroup"].as_array().unwrap() {
        let tmp = tempfile::tempdir().unwrap();
        for (p, d) in c["files"].as_object().unwrap() {
            let path = tmp.path().join(p);
            std::fs::create_dir_all(path.parent().unwrap()).unwrap();
            std::fs::write(path, d.as_str().unwrap()).unwrap();
        }
        assert_eq!(
            serve::cgroup_mem_mib(tmp.path()),
            (i(c, "limit"), i(c, "current")),
            "{}",
            c["files"]
        );
    }
    for c in fx["smi"].as_array().unwrap() {
        assert_eq!(
            serve::vram_from_smi(&st(c, "out"), &st(c, "cvd"), b(c, "pci")),
            i(c, "mib"),
            "{c}"
        );
    }
    let devs = serve::parse_list_devices(&st(&fx["devices"], "out"));
    let want: Vec<(String, i64)> = fx["devices"]["devs"]
        .as_array()
        .unwrap()
        .iter()
        .map(|d| (st(d, "ID"), i(d, "TotalMiB")))
        .collect();
    assert_eq!(
        devs.iter()
            .map(|d| (d.id.clone(), d.total_mib))
            .collect::<Vec<_>>(),
        want
    );
    for (out, want) in fx["builds"].as_object().unwrap() {
        assert_eq!(
            serve::run::parse_engine_build(out),
            want.as_i64().unwrap(),
            "{out:?}"
        );
    }
    for c in fx["cmdlines"].as_array().unwrap() {
        let env: BTreeMap<String, String> = map_from(&c["env"]).into_iter().collect();
        assert_eq!(serve::engine_cmdline(&env, &strs(&c["args"])), st(c, "out"));
    }
    let cur = serve::SpecAutoMark {
        fp: "abc".into(),
        bin: "/app/llama-server".into(),
        build: 11500,
    };
    for c in fx["verdicts"].as_array().unwrap() {
        let attempt = c["attempt"].as_object().map(|a| serve::SpecAutoMark {
            fp: a["fp"].as_str().unwrap().into(),
            bin: a["bin"].as_str().unwrap().into(),
            build: a["build"].as_i64().unwrap(),
        });
        let failed = map_from(&c["failed"]);
        assert_eq!(
            serve::spec_auto_verdict(&cur, attempt.as_ref(), &failed),
            (st(c, "why"), b(c, "record")),
            "{c}"
        );
    }
    for (h, want) in fx["help_fit"].as_object().unwrap() {
        assert_eq!(
            serve::help_supports_fit(h),
            want.as_bool().unwrap(),
            "{h:?}"
        );
    }
    for (m, want) in fx["gib"].as_object().unwrap() {
        assert_eq!(
            serve::gib_text(m.parse().unwrap()),
            want.as_str().unwrap(),
            "{m}"
        );
    }
    for c in fx["load_flags"].as_array().unwrap() {
        let (out, note) = serve::normalize_load_flags(&strs(&c["args"]), b(c, "modern"));
        assert_eq!((out, note), (strs(&c["out"]), st(c, "note")), "{c}");
    }
    for c in fx["state_bytes"].as_array().unwrap() {
        let g = gguf_from(&c["gguf"]).unwrap();
        let (pt, rec) = serve::gguf_state_bytes(&g, &st(c, "k"), &st(c, "v"));
        assert_eq!(pt, c["per_token"].as_f64().unwrap(), "{c}");
        assert_eq!(rec, i(c, "recurrent"), "{c}");
    }
}

#[test]
fn port_libre() {
    let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let port = l.local_addr().unwrap().port().to_string();
    let err = serve::run::wait_port_free("0.0.0.0", &port, std::time::Duration::from_millis(600))
        .unwrap_err();
    assert!(err.contains("déjà utilisé"), "{err}");
    drop(l);
    serve::run::wait_port_free("[::]", &port, std::time::Duration::from_millis(600)).unwrap();
    serve::run::wait_port_free("127.0.0.1", "", std::time::Duration::ZERO).unwrap();
}

#[test]
fn cle_de_persistance() {
    let tmp = tempfile::tempdir().unwrap();
    let model = tmp.path().join("m.gguf");
    std::fs::write(&model, b"x").unwrap();
    let si = SysInfo {
        model: model.to_string_lossy().into_owned(),
        ..SysInfo::default()
    };
    let args: Vec<String> = ["/b", "-m", "x", "--api-key", "secret"]
        .iter()
        .map(|s| s.to_string())
        .collect();
    let env = BTreeMap::new();
    let k1 = serve::slot_persist_key(&args, &env, &[], &si, "/b");
    assert_eq!(k1.len(), 16);
    // La clé d'API n'entre pas dans l'empreinte.
    let mut a2 = args.clone();
    a2[4] = "autre".into();
    assert_eq!(serve::slot_persist_key(&a2, &env, &[], &si, "/b"), k1);
    // Une variable du moteur, si.
    let pe = vec![("GGML_X".to_string(), "1".to_string())];
    assert_ne!(serve::slot_persist_key(&args, &env, &pe, &si, "/b"), k1);
    assert!(serve::slot_persist_file(
        "persist-0123456789abcdef-0123456789ab.bin"
    ));
    assert!(!serve::slot_persist_file(
        "persist-0123456789abcdef-0123456789ab.bak"
    ));
}

mod common;

/// Script exécutable, écrit par un `sh` à part (jamais tenu ouvert en
/// écriture par ce process : pas d'ETXTBSY au lancement).
#[cfg(unix)]
fn script(dir: &std::path::Path, name: &str, body: &str) -> String {
    let p = dir.join(name);
    let ok = std::process::Command::new("sh")
        .args(["-c", "printf %s \"$1\" > \"$2\" && chmod 755 \"$2\""])
        .arg("sh")
        .arg(body)
        .arg(&p)
        .status()
        .unwrap()
        .success();
    assert!(ok);
    p.to_string_lossy().into_owned()
}

/// `loki-rs engine serve` de bout en bout : configuration lue par l'API
/// interne d'un faux Go, aide d'un faux llama-server, exec de la ligne
/// composée (le faux moteur écrit ses arguments), commande gardée en base.
#[cfg(unix)]
#[tokio::test]
async fn lancement_de_bout_en_bout() {
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    std::fs::create_dir_all(home.join("models")).unwrap();
    std::fs::write(home.join("models/m.gguf"), b"pas un vrai gguf").unwrap();
    let out = home.join("argv.txt");
    let bin = script(
        &home,
        "llama-server",
        "#!/bin/sh\nif [ \"$1\" = \"--help\" ]; then echo '-ngl N  (default: auto) --load-mode --fit [on|off]'; exit 0; fi\nprintf '%s\\n' \"$@\" > \"$OUT\"\nprintf '%s' \"$LD_LIBRARY_PATH\" > \"$OUT.ld\"\n",
    );
    let port = std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port();
    let (_facade, go, _stop) = common::fakego::start(home.clone()).await;
    go.set("config", "BIN", &bin);
    go.set("config", "MODEL", "m.gguf");
    go.set("config", "PORT", &port.to_string());
    go.set("config", "CTX", "8192");
    go.set("config", "EXTRA_ARGS", "--mlock");
    let backend = go.backend.lock().unwrap().unwrap();
    let exe = env!("CARGO_BIN_EXE_loki-rs");
    let st = tokio::process::Command::new(exe)
        .args(["engine", "serve"])
        .env("LOKI_HOME", &home)
        .env("LOKI_GO_ADDR", backend.to_string())
        .env("LOKI_INTERNAL_TOKEN", common::fakego::JETON)
        .env("OUT", &out)
        .env_remove("LLAMA_ARG_THREADS")
        .output()
        .await
        .unwrap();
    assert!(
        st.status.success(),
        "{}",
        String::from_utf8_lossy(&st.stderr)
    );
    let argv = std::fs::read_to_string(&out).unwrap();
    let model = home.join("models/m.gguf").to_string_lossy().into_owned();
    let want = [
        "-m",
        &model,
        "-c",
        "8192",
        "-b",
        "2048",
        "-ub",
        "512",
        "--host",
        "0.0.0.0",
        "--port",
        &port.to_string(),
        "--parallel",
        "1",
        "-ngl",
        "auto",
        "--load-mode",
        "mmap+mlock",
    ];
    let got: Vec<&str> = argv.lines().collect();
    // -t éventuel (conteneur à l'étroit) : la sonde dépend de la machine.
    let got: Vec<&str> = if got.get(4) == Some(&"-t") {
        [&got[..4], &got[6..]].concat()
    } else {
        got
    };
    assert_eq!(got, want, "{argv}");
    let ld = std::fs::read_to_string(home.join("argv.txt.ld")).unwrap();
    assert!(ld.starts_with(home.to_str().unwrap()), "{ld}");
    let cmdline = go.get("state", "engine_cmdline").unwrap();
    assert!(cmdline.starts_with(&bin), "{cmdline}");
    assert!(cmdline.contains("--load-mode mmap+mlock"), "{cmdline}");

    // Port pris : refus en clair, code 1.
    let l = std::net::TcpListener::bind(("127.0.0.1", port)).unwrap();
    let st = tokio::process::Command::new(exe)
        .args(["engine", "serve"])
        .env("LOKI_HOME", &home)
        .env("LOKI_GO_ADDR", backend.to_string())
        .env("LOKI_INTERNAL_TOKEN", common::fakego::JETON)
        .output()
        .await
        .unwrap();
    drop(l);
    assert_eq!(st.status.code(), Some(1));
    assert!(String::from_utf8_lossy(&st.stderr).contains("déjà utilisé"));

    // Modèle absent.
    go.set("config", "MODEL", "absent.gguf");
    let st = tokio::process::Command::new(exe)
        .args(["engine", "serve"])
        .env("LOKI_HOME", &home)
        .env("LOKI_GO_ADDR", backend.to_string())
        .env("LOKI_INTERNAL_TOKEN", common::fakego::JETON)
        .output()
        .await
        .unwrap();
    assert_eq!(st.status.code(), Some(1));
    assert!(
        String::from_utf8_lossy(&st.stderr).contains("modèle introuvable"),
        "{}",
        String::from_utf8_lossy(&st.stderr)
    );

    // Preset externe : rien à lancer, sortie propre.
    go.set("config", "EXTERNAL", "1");
    let st = tokio::process::Command::new(exe)
        .args(["engine", "serve"])
        .env("LOKI_HOME", &home)
        .env("LOKI_GO_ADDR", backend.to_string())
        .env("LOKI_INTERNAL_TOKEN", common::fakego::JETON)
        .output()
        .await
        .unwrap();
    assert!(st.status.success());
    assert!(String::from_utf8_lossy(&st.stdout).contains("preset externe"));
}

/// Strata, essai de l'optimiseur, pas d'API interne : `loki serve` (Go) est
/// lancé à notre place.
#[cfg(unix)]
#[tokio::test]
async fn delegation_au_go() {
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    let mark = home.join("go.txt");
    let go_bin = script(&home, "loki", "#!/bin/sh\necho \"go $*\" >> \"$MARK\"\n");
    let exe = env!("CARGO_BIN_EXE_loki-rs");
    // Sans API interne.
    let st = tokio::process::Command::new(exe)
        .args(["engine", "serve"])
        .env("LOKI_HOME", &home)
        .env("LOKI_GO_BIN", &go_bin)
        .env("MARK", &mark)
        .env_remove("LOKI_GO_ADDR")
        .output()
        .await
        .unwrap();
    assert!(
        st.status.success(),
        "{}",
        String::from_utf8_lossy(&st.stderr)
    );
    // Essai de l'optimiseur.
    let st = tokio::process::Command::new(exe)
        .args(["engine", "serve"])
        .env("LOKI_HOME", &home)
        .env("LOKI_GO_BIN", &go_bin)
        .env("MARK", &mark)
        .env("LOKI_TUNE_TRIAL", "/tmp/essai")
        .output()
        .await
        .unwrap();
    assert!(st.status.success());
    // Preset Strata.
    let (_facade, go, _stop) = common::fakego::start(home.clone()).await;
    go.set("config", "ENGINE", "strata");
    let backend = go.backend.lock().unwrap().unwrap();
    let st = tokio::process::Command::new(exe)
        .args(["engine", "serve"])
        .env("LOKI_HOME", &home)
        .env("LOKI_GO_BIN", &go_bin)
        .env("MARK", &mark)
        .env("LOKI_GO_ADDR", backend.to_string())
        .env("LOKI_INTERNAL_TOKEN", common::fakego::JETON)
        .output()
        .await
        .unwrap();
    assert!(st.status.success());
    assert_eq!(
        std::fs::read_to_string(&mark).unwrap(),
        "go serve\ngo serve\ngo serve\n"
    );
}
