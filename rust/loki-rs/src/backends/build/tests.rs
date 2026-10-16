use super::*;

fn feed_all(sp: &mut Splitter, writes: &[&[u8]]) -> Vec<String> {
    let mut got = Vec::new();
    for w in writes {
        sp.feed(w, &mut |l| got.push(l));
    }
    sp.finish(&mut |l| got.push(l));
    got
}

/// `TestSinkWriterProgression` : une barre (\r sans \n) apparaît sans noyer
/// le journal ; \r\n reste une fin de ligne, même coupé entre deux écritures.
#[test]
fn barre_de_progression() {
    let mut sp = Splitter::new(true);
    let mut got = Vec::new();
    sp.feed(b"Step 5\r", &mut |l| got.push(l));
    sp.feed(
        "\n  modèle : 1 / 76 GB\r  modèle : 2 / 76 GB\r".as_bytes(),
        &mut |l| got.push(l),
    );
    sp.feed("  modèle : 3 / 76 GB\r".as_bytes(), &mut |l| got.push(l));
    sp.last_prog = Some(Instant::now() - Duration::from_secs(60));
    sp.feed("  modèle : 40 / 76 GB\r[ok] fini\n".as_bytes(), &mut |l| {
        got.push(l)
    });
    // Un reste sans fin de ligne est perdu, comme au Go.
    sp.feed(b"reste", &mut |l| got.push(l));
    sp.finish(&mut |l| got.push(l));
    assert_eq!(
        got,
        [
            "Step 5",
            "modèle : 1 / 76 GB",
            "modèle : 3 / 76 GB",
            "[ok] fini"
        ]
    );
}

/// Le scanner : \n seul coupe, \r final retiré, dernier morceau rendu,
/// octets invalides en U+FFFD un à un.
#[test]
fn scanner_de_lignes() {
    let mut sp = Splitter::new(false);
    let got = feed_all(&mut sp, &[b"a\r\nb\rc\n", b"d", b"\xff\xfee\n", b"fin\r"]);
    assert_eq!(got, ["a", "b\rc", "d\u{fffd}\u{fffd}e", "fin"]);
    let mut sp = Splitter::new(false);
    let long = vec![b'x'; MAX_LINE + 1];
    let got = feed_all(&mut sp, &[b"ok\n", &long, b"\napres\n"]);
    assert_eq!(got, ["ok"]);
}

/// `TestCudaToolkitRoot`.
#[test]
fn racine_du_toolkit() {
    for (nvcc, want) in [
        ("/usr/local/cuda/bin/nvcc", "/usr/local/cuda"),
        ("/usr/local/cuda-12.8/bin/nvcc", "/usr/local/cuda-12.8"),
        ("/usr/bin/nvcc", ""),
        ("/opt/nvcc", ""),
        ("/bin/nvcc", ""),
        ("bin/nvcc", ""),
        ("", ""),
    ] {
        assert_eq!(cuda_toolkit_root(Path::new(nvcc)), want, "{nvcc}");
    }
}

#[test]
fn cache_deplace() {
    let d = tempfile::tempdir().unwrap();
    let repo = d.path().join("llama.cpp");
    let build = repo.join("build");
    std::fs::create_dir_all(&build).unwrap();
    assert!(!cache_stale(&build, &repo), "sans cache");
    let cache = |home: &str| {
        std::fs::write(
            build.join("CMakeCache.txt"),
            format!("# x\nCMAKE_HOME_DIRECTORY:INTERNAL={home}  \nAUTRE=1\n"),
        )
        .unwrap()
    };
    cache(&repo.to_string_lossy());
    assert!(!cache_stale(&build, &repo), "même dossier");
    cache("/ailleurs/llama.cpp");
    assert!(cache_stale(&build, &repo), "dépôt déplacé");
    cache("");
    assert!(!cache_stale(&build, &repo), "chemin vide");
    std::fs::write(
        build.join("CMakeCache.txt"),
        "CMAKE_HOME_DIRECTORY:INTERNAL\n",
    )
    .unwrap();
    assert!(!cache_stale(&build, &repo), "sans =");
}

#[test]
fn noms_et_urls() {
    for (url, want) in [
        ("https://github.com/prism/llama.cpp.git", "llama.cpp-prism"),
        ("git@github.com:prism/llama.cpp.git", "llama.cpp-prism"),
        ("https://github.com/ikawrakow/ik_llama.cpp/", "ik_llama.cpp"),
        ("  https://github.com/Owner/LLAMA.CPP  ", "llama.cpp-owner"),
        ("llama.cpp", "llama.cpp"),
        ("https://", "https"),
        ("", "backend"),
        ("/", "backend"),
    ] {
        assert_eq!(derive_backend_name(url), want, "{url}");
    }
    for (url, ok) in [
        ("https://x", true),
        ("http://x", true),
        ("git@x:y", true),
        ("ssh://x", true),
        ("git://x", true),
        ("ftp://x", false),
        ("/chemin/local", false),
        (" https://x", false),
    ] {
        assert_eq!(looks_like_git_url(url), ok, "{url}");
    }
}

/// Le plan complet par machine (faux outils, fausse racine).
#[cfg(target_os = "linux")]
#[tokio::test]
async fn plan_par_machine() {
    let tools = tempfile::tempdir().unwrap();
    let root = tempfile::tempdir().unwrap();
    let probe = Probe {
        path: Some(vec![tools.path().to_path_buf()]),
        root: root.path().to_path_buf(),
    };
    let script = |name: &str, body: &str| {
        let p = tools.path().join(name);
        std::fs::write(&p, format!("#!/bin/sh\n{body}\n")).unwrap();
        use std::os::unix::fs::PermissionsExt as _;
        std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o755)).unwrap();
    };
    let base = [
        "-DCMAKE_BUILD_TYPE=Release",
        "-DGGML_NATIVE=ON",
        "-DLLAMA_BUILD_UI=OFF",
        "-DLLAMA_USE_PREBUILT_UI=OFF",
    ];
    let p = probe.build_plan().await;
    assert_eq!(
        (p.backend, p.flags.clone(), p.cuda_cxx.as_str()),
        ("cpu", base.map(String::from).to_vec(), "")
    );
    assert!(p.jobs >= 1);

    // nvcc dans le PATH seulement (racine /usr : rien à apprendre à CMake).
    script("nvcc", "");
    script(
        "nvidia-smi",
        r#"case "$1" in -L) echo "GPU 0: RTX";; --query-gpu=compute_cap) printf '8.6\n8.9\n8.6\n';; esac"#,
    );
    let p = probe.build_plan().await;
    let nvcc = tools.path().join("nvcc").to_string_lossy().into_owned();
    let mut want: Vec<String> = base.map(String::from).to_vec();
    want.extend(["-DGGML_CUDA=ON".into(), "-DGGML_CUDA_F16=ON".into()]);
    want.push("-DCMAKE_CUDA_ARCHITECTURES=86;89".into());
    assert_eq!((p.backend, p.cuda_arch.as_str()), ("cuda", "86;89"));
    // Le dossier des outils ne s'appelle pas bin : pas de racine.
    assert_eq!(p.flags, want);
    assert_eq!(p.cuda_cxx, nvcc);

    // Un vrai toolkit l'emporte, racine explicite.
    let tk = root.path().join("usr/local/cuda-12.8/bin");
    std::fs::create_dir_all(&tk).unwrap();
    std::fs::write(tk.join("nvcc"), "").unwrap();
    let p = probe.build_plan().await;
    let tk_nvcc = tk.join("nvcc").to_string_lossy().into_owned();
    let tk_root = root
        .path()
        .join("usr/local/cuda-12.8")
        .to_string_lossy()
        .into_owned();
    let mut want: Vec<String> = base.map(String::from).to_vec();
    want.extend([
        "-DGGML_CUDA=ON".into(),
        "-DGGML_CUDA_F16=ON".into(),
        format!("-DCUDAToolkit_ROOT={tk_root}"),
        format!("-DCMAKE_CUDA_COMPILER={tk_nvcc}"),
        "-DCMAKE_CUDA_ARCHITECTURES=86;89".into(),
    ]);
    assert_eq!(p.flags, want);
    assert_eq!(p.cuda_cxx, tk_nvcc);

    // Sans compute_cap : pas d'architecture imposée.
    script("nvidia-smi", r#"[ "$1" = -L ] && echo "GPU 0: RTX""#);
    let p = probe.build_plan().await;
    assert!(
        !p.flags
            .iter()
            .any(|f| f.starts_with("-DCMAKE_CUDA_ARCHITECTURES"))
    );
    assert_eq!(p.cuda_arch, "");

    // HIP, puis Vulkan.
    std::fs::remove_file(tools.path().join("nvidia-smi")).unwrap();
    script("hipcc", "");
    let p = probe.build_plan().await;
    assert_eq!(
        (p.backend, p.flags.last().unwrap().as_str()),
        ("hip", "-DGGML_HIP=ON")
    );
    std::fs::remove_file(tools.path().join("hipcc")).unwrap();
    script("glslc", "");
    script("vulkaninfo", "");
    let p = probe.build_plan().await;
    assert_eq!(
        (p.backend, p.flags.last().unwrap().as_str()),
        ("vulkan", "-DGGML_VULKAN=ON")
    );
}

/// Les étapes suivies : sortie des deux flux dans l'ordre, journal écrit,
/// erreurs au format d'`exec`.
#[cfg(unix)]
#[tokio::test]
async fn erreurs_des_commandes() {
    let tools = tempfile::tempdir().unwrap();
    let probe = Probe {
        path: Some(vec![tools.path().to_path_buf()]),
        root: tools.path().to_path_buf(),
    };
    let p = tools.path().join("outil");
    std::fs::write(&p, "#!/bin/sh\necho un\necho deux >&2\nprintf trois\n[ -n \"$X\" ] && echo \"X=$X\"\nexit $1\n").unwrap();
    use std::os::unix::fs::PermissionsExt as _;
    std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o755)).unwrap();
    let args = |a: &[&str]| a.iter().map(|s| s.to_string()).collect::<Vec<_>>();
    let mut got = Vec::new();
    let log = tools.path().join("x.log");
    let r = run_lines_with(
        &probe,
        Some(tools.path()),
        &[("X".into(), "1".into())],
        "outil",
        &args(&["3"]),
        false,
        Some(&log),
        |l| got.push(l),
    )
    .await;
    assert_eq!(r, Err("exit status 3".into()));
    assert_eq!(got, ["un", "deux", "troisX=1"]);
    assert_eq!(
        std::fs::read_to_string(&log).unwrap(),
        "un\ndeux\ntroisX=1\n"
    );
    let mut got = Vec::new();
    let r = run_lines_with(&probe, None, &[], "outil", &args(&["0"]), true, None, |l| {
        got.push(l)
    })
    .await;
    assert_eq!(r, Ok(()));
    assert_eq!(got, ["un", "deux"]);
    let r = run_lines_with(&probe, None, &[], "absent", &[], true, None, |_| {}).await;
    assert_eq!(
        r,
        Err("exec: \"absent\": executable file not found in $PATH".into())
    );
    std::fs::write(&p, "#!/bin/sh\nkill -9 $$\n").unwrap();
    let r = run_lines_with(&probe, None, &[], "outil", &[], true, None, |_| {}).await;
    assert_eq!(r, Err("signal: killed".into()));
}
