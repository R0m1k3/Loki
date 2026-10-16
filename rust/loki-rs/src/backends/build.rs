//! Compilation de llama.cpp depuis les sources (lot 41) — port de
//! `backend_build.go` (plan, CMake, étapes suivies), de `lcRunInstall`,
//! `lcRunUpdate`, `lcBuildAndSwitch` (`web_llamacpp.go`) et de
//! `installCustomBackend` (`backend_llamacpp.go`), pour Linux et macOS.
//!
//! Chaque étape passe par le job des moteurs : phases, sortie des commandes
//! ligne à ligne (fichiers compilés et phases tirés des lignes), journaux
//! `configure.log` et `build.log` dans le dépôt, fin des journaux remontée
//! en cas d'échec. Les outils manquants sont installés au mieux par le
//! gestionnaire de paquets de la machine, comme le Go.

use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::time::{Duration, Instant};

use super::probe::{Probe, is_dir};
use super::{Backends, Log, llama_server_bin, sanitize_backend_name, service, service_stop_if_up};
use crate::api::Ctx;
use crate::store::BK_CONFIG;
use crate::system::go_lossy;

/// `llamacppRepoURL`.
pub const REPO_URL: &str = "https://github.com/ggml-org/llama.cpp.git";

/// `sinkProgressEvery` : une barre qui se redessine (\r), montrée toutes les
/// 10 s.
const PROGRESS_EVERY: Duration = Duration::from_secs(10);

/// Une ligne de sortie lue au plus (`bufio.Scanner`, 1 Mio).
const MAX_LINE: usize = 1 << 20;

/// `buildPlan` complet : backend, drapeaux CMake, nvcc.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Plan {
    pub backend: &'static str,
    pub cuda_arch: String,
    pub jobs: usize,
    pub flags: Vec<String>,
    pub cuda_cxx: String,
}

/// `cudaToolkitRoot` : `<racine>/bin/nvcc` → `<racine>`, sauf `/usr` (la
/// racine n'apprend alors rien à CMake).
pub fn cuda_toolkit_root(nvcc: &Path) -> String {
    let Some(bin) = nvcc.parent() else {
        return String::new();
    };
    if bin.file_name().and_then(|n| n.to_str()) != Some("bin") {
        return String::new();
    }
    let root = bin
        .parent()
        .map(|r| r.to_string_lossy().into_owned())
        .unwrap_or_default();
    match root.as_str() {
        "" | "." | "/" | "/usr" => String::new(),
        _ => root,
    }
}

impl Probe {
    /// `detectBuildPlan`.
    pub async fn build_plan(&self) -> Plan {
        let base = self.detect_build_plan().await;
        let mut p = Plan {
            backend: base.backend,
            cuda_arch: base.cuda_arch,
            jobs: base.jobs,
            flags: [
                "-DCMAKE_BUILD_TYPE=Release",
                "-DGGML_NATIVE=ON",
                // L'UI de llama-server demande npm : Loki a la sienne.
                "-DLLAMA_BUILD_UI=OFF",
                "-DLLAMA_USE_PREBUILT_UI=OFF",
            ]
            .map(String::from)
            .to_vec(),
            cuda_cxx: String::new(),
        };
        match p.backend {
            "metal" => p.flags.push("-DGGML_METAL=ON".into()),
            "cuda" => {
                let nvcc = self.find_nvcc().unwrap_or_default();
                p.cuda_cxx = nvcc.to_string_lossy().into_owned();
                p.flags.push("-DGGML_CUDA=ON".into());
                p.flags.push("-DGGML_CUDA_F16=ON".into());
                let root = cuda_toolkit_root(&nvcc);
                if !root.is_empty() {
                    p.flags.push(format!("-DCUDAToolkit_ROOT={root}"));
                    p.flags
                        .push(format!("-DCMAKE_CUDA_COMPILER={}", p.cuda_cxx));
                }
                if !p.cuda_arch.is_empty() {
                    p.flags
                        .push(format!("-DCMAKE_CUDA_ARCHITECTURES={}", p.cuda_arch));
                }
            }
            "hip" => p.flags.push("-DGGML_HIP=ON".into()),
            "vulkan" => p.flags.push("-DGGML_VULKAN=ON".into()),
            _ => {}
        }
        p
    }
}

/// `cacheStale` : un `build/` configuré pour un autre dossier source (dépôt
/// déplacé) ne se reconfigure pas en place.
pub fn cache_stale(build: &Path, repo: &Path) -> bool {
    let Ok(b) = std::fs::read_to_string(build.join("CMakeCache.txt")) else {
        return false;
    };
    let abs = std::path::absolute(repo).unwrap_or_else(|_| repo.to_path_buf());
    let abs = abs.to_string_lossy();
    for line in b.split('\n') {
        if line.starts_with("CMAKE_HOME_DIRECTORY:")
            && let Some(i) = line.find('=')
        {
            let home = line[i + 1..].trim();
            return !home.is_empty() && home != abs;
        }
    }
    false
}

/// L'erreur d'une commande, au format d'`exec` du Go.
fn exit_err(status: std::process::ExitStatus) -> Result<(), String> {
    if status.success() {
        return Ok(());
    }
    match status.code() {
        Some(c) => Err(format!("exit status {c}")),
        None => {
            #[cfg(unix)]
            {
                use std::os::unix::process::ExitStatusExt as _;
                if let Some(s) = status.signal() {
                    return Err(format!("signal: {}", signal_name(s)));
                }
            }
            Err(status.to_string())
        }
    }
}

#[cfg(unix)]
fn signal_name(s: i32) -> &'static str {
    match s {
        libc::SIGKILL => "killed",
        libc::SIGTERM => "terminated",
        libc::SIGINT => "interrupt",
        libc::SIGSEGV => "segmentation fault",
        libc::SIGABRT => "aborted",
        _ => "signal",
    }
}

/// Une commande lancée par une étape : sorties standard et d'erreur dans le
/// même tube (leur ordre gardé), lues ligne à ligne (voir [`Splitter`]) et
/// copiées dans `log_file`, créé avant le lancement comme au Go.
struct Running {
    rx: tokio::sync::mpsc::UnboundedReceiver<String>,
    done: tokio::task::JoinHandle<Result<(), String>>,
}

fn spawn_lines(
    probe: &Probe,
    dir: Option<&Path>,
    env: &[(String, String)],
    name: &str,
    args: &[String],
    progress: bool,
    log_file: Option<&Path>,
) -> Result<Running, String> {
    let mut file = log_file.and_then(|p| std::fs::File::create(p).ok());
    let Some(bin) = probe.look_path(name) else {
        return Err(format!(
            "exec: \"{name}\": executable file not found in $PATH"
        ));
    };
    let (mut reader, writer) = std::io::pipe().map_err(|e| e.to_string())?;
    let mut cmd = std::process::Command::new(&bin);
    cmd.args(args)
        .stdin(Stdio::null())
        .stdout(writer.try_clone().map_err(|e| e.to_string())?)
        .stderr(writer);
    if let Some(d) = dir {
        cmd.current_dir(d);
    }
    for (k, v) in env {
        cmd.env(k, v);
    }
    let spawned = cmd.spawn();
    // Les extrémités d'écriture n'appartiennent plus qu'à l'enfant : le tube
    // se ferme à sa fin.
    drop(cmd);
    let mut child =
        spawned.map_err(|e| crate::paths::os_err("fork/exec", &bin.to_string_lossy(), &e))?;
    let (tx, rx) = tokio::sync::mpsc::unbounded_channel::<String>();
    let done = tokio::task::spawn_blocking(move || {
        split_lines(&mut reader, progress, |l| {
            if let Some(f) = file.as_mut() {
                use std::io::Write as _;
                let _ = writeln!(f, "{l}");
            }
            let _ = tx.send(l);
        });
        drop(reader);
        exit_err(child.wait().map_err(|e| e.to_string())?)
    });
    Ok(Running { rx, done })
}

/// [`spawn_lines`], chaque ligne versée au job.
#[allow(clippy::too_many_arguments)]
async fn run_lines(
    probe: &Probe,
    log: &Log<'_>,
    dir: Option<&Path>,
    env: &[(String, String)],
    name: &str,
    args: &[String],
    progress: bool,
    log_file: Option<&Path>,
) -> Result<(), String> {
    let mut r = spawn_lines(probe, dir, env, name, args, progress, log_file)?;
    while let Some(line) = r.rx.recv().await {
        log.line(&line).await;
    }
    r.done.await.map_err(|e| e.to_string())?
}

/// [`spawn_lines`], chaque ligne passée à `f` (tests).
#[cfg(test)]
#[allow(clippy::too_many_arguments)]
async fn run_lines_with(
    probe: &Probe,
    dir: Option<&Path>,
    env: &[(String, String)],
    name: &str,
    args: &[String],
    progress: bool,
    log_file: Option<&Path>,
    mut f: impl FnMut(String),
) -> Result<(), String> {
    let mut r = spawn_lines(probe, dir, env, name, args, progress, log_file)?;
    while let Some(line) = r.rx.recv().await {
        f(line);
    }
    r.done.await.map_err(|e| e.to_string())?
}

/// Le découpage en lignes de `run_lines` : `sinkWriter` (`progress`) ou
/// `bufio.Scanner`.
pub struct Splitter {
    progress: bool,
    buf: Vec<u8>,
    /// Dernière barre de progression montrée.
    pub last_prog: Option<Instant>,
    /// Ligne démesurée : le scanner s'est arrêté.
    pub stopped: bool,
}

impl Splitter {
    pub fn new(progress: bool) -> Self {
        Splitter {
            progress,
            buf: Vec::new(),
            last_prog: None,
            stopped: false,
        }
    }

    /// Une écriture de la commande.
    pub fn feed(&mut self, p: &[u8], emit: &mut impl FnMut(String)) {
        if self.stopped {
            return;
        }
        self.buf.extend_from_slice(p);
        let progress = self.progress;
        while let Some(i) = self
            .buf
            .iter()
            .position(|&c| c == b'\n' || (progress && c == b'\r'))
        {
            if self.buf[i] == b'\n' {
                let mut line = &self.buf[..i];
                if line.last() == Some(&b'\r') {
                    line = &line[..line.len() - 1];
                }
                if !progress && line.len() > MAX_LINE {
                    self.stopped = true;
                    return;
                }
                emit(go_lossy(line));
                self.buf.drain(..=i);
                continue;
            }
            // \r : fin de ligne Windows ou barre qui se redessine ; le \n
            // peut arriver dans l'écriture suivante.
            if i + 1 >= self.buf.len() {
                break;
            }
            if self.buf[i + 1] == b'\n' {
                emit(go_lossy(&self.buf[..i]));
                self.buf.drain(..i + 2);
                continue;
            }
            let prog = go_lossy(&self.buf[..i]).trim().to_string();
            if !prog.is_empty() && self.last_prog.is_none_or(|t| t.elapsed() >= PROGRESS_EVERY) {
                self.last_prog = Some(Instant::now());
                emit(prog);
            }
            self.buf.drain(..=i);
        }
        if !progress && self.buf.len() > MAX_LINE {
            self.stopped = true;
        }
    }

    /// Fin du flux : le scanner rend le dernier morceau, le `sinkWriter` le
    /// perd.
    pub fn finish(&mut self, emit: &mut impl FnMut(String)) {
        if self.progress || self.stopped || self.buf.is_empty() {
            return;
        }
        let mut line = &self.buf[..];
        if line.last() == Some(&b'\r') {
            line = &line[..line.len() - 1];
        }
        emit(go_lossy(line));
        self.buf.clear();
    }
}

fn split_lines(r: &mut impl std::io::Read, progress: bool, mut emit: impl FnMut(String)) {
    let mut sp = Splitter::new(progress);
    let mut chunk = [0u8; 8192];
    loop {
        match r.read(&mut chunk) {
            Ok(0) | Err(_) => break,
            Ok(n) => sp.feed(&chunk[..n], &mut emit),
        }
        if sp.stopped {
            return;
        }
    }
    sp.finish(&mut emit);
}

/// `runStep` : une commande dont chaque ligne va au job.
pub async fn run_step(
    probe: &Probe,
    log: &Log<'_>,
    dir: Option<&Path>,
    name: &str,
    args: &[&str],
) -> Result<(), String> {
    let args: Vec<String> = args.iter().map(|s| s.to_string()).collect();
    run_lines(probe, log, dir, &[], name, &args, true, None).await
}

/// `buildLlamacpp` : configure puis compile la cible llama-server. Un
/// `build/` configuré ailleurs (ou `clean`) repart de zéro (`build.old`).
pub async fn build_llamacpp(
    probe: &Probe,
    log: &Log<'_>,
    repo: &Path,
    p: &Plan,
    clean: bool,
) -> Result<(), String> {
    let build = repo.join("build");
    if (clean || cache_stale(&build, repo)) && is_dir(&build) {
        let old = repo.join("build.old");
        let _ = std::fs::remove_dir_all(&old);
        if std::fs::rename(&build, &old).is_err() {
            let _ = std::fs::remove_dir_all(&build);
        }
    }
    // nvcc dans le PATH et exposé par CUDACXX pour la configuration CMake.
    let mut env: Vec<(String, String)> = Vec::new();
    if p.backend == "cuda" && !p.cuda_cxx.is_empty() {
        let cuda_bin = Path::new(&p.cuda_cxx)
            .parent()
            .map(|d| d.to_string_lossy().into_owned())
            .unwrap_or_default();
        env.push(("CUDACXX".into(), p.cuda_cxx.clone()));
        let path = std::env::var("PATH").unwrap_or_default();
        env.push(("PATH".into(), format!("{cuda_bin}:{path}")));
    }
    let mut cfg_args: Vec<String> = ["-B", "build", "-S", "."].map(String::from).to_vec();
    cfg_args.extend(p.flags.iter().cloned());
    let cfg_log = repo.join("configure.log");
    if let Err(e) = run_lines(
        probe,
        log,
        Some(repo),
        &env,
        "cmake",
        &cfg_args,
        false,
        Some(&cfg_log),
    )
    .await
    {
        hint_missing_dep(probe, p, &cfg_log);
        return Err(format!("configuration CMake échouée: {e}"));
    }
    let build_args: Vec<String> = [
        "--build",
        "build",
        "--config",
        "Release",
        "-j",
        &p.jobs.to_string(),
        "--target",
        "llama-server",
    ]
    .map(String::from)
    .to_vec();
    run_lines(
        probe,
        log,
        Some(repo),
        &env,
        "cmake",
        &build_args,
        false,
        Some(&repo.join("build.log")),
    )
    .await
    .map_err(|e| format!("compilation échouée: {e}"))
}

/// `hintMissingBuildDep` : une dépendance connue manquante, dite sur la
/// sortie d'erreur de la façade (le Go l'écrit sur son terminal, pas dans
/// le job).
fn hint_missing_dep(probe: &Probe, p: &Plan, cfg_log: &Path) {
    let Ok(log) = std::fs::read_to_string(cfg_log) else {
        return;
    };
    if p.backend == "cuda" && log.contains("CUDA Toolkit not found") {
        eprintln!(
            "[dépendance] nvcc est présent mais le CUDA Toolkit complet (en-têtes + cudart) est introuvable."
        );
    }
    if p.backend == "vulkan" && log.contains("SPIRV-Headers") {
        let hint = pkg_install_hint(probe, "spirv-headers");
        eprintln!(
            "[dépendance] en-têtes SPIR-V (paquet « SPIRV-Headers ») introuvables pour le backend Vulkan{}",
            if hint.is_empty() {
                String::new()
            } else {
                format!(" : {hint}")
            }
        );
    }
}

/// `pkgInstallHint`.
fn pkg_install_hint(probe: &Probe, pkg: &str) -> String {
    for (bin, cmd) in [
        ("pacman", "sudo pacman -S "),
        ("apt-get", "sudo apt-get install -y "),
        ("dnf", "sudo dnf install -y "),
        ("zypper", "sudo zypper install -y "),
        ("brew", "brew install "),
    ] {
        if probe.has_tool(bin) {
            return format!("{cmd}{pkg}");
        }
    }
    String::new()
}

/// `autoInstallTool` : le paquet par le gestionnaire de la machine (sudo
/// hors root), sorties sur celles de la façade.
async fn auto_install_tool(probe: &Probe, name: &str) -> Result<(), String> {
    for (m, install) in [
        ("apt-get", &["install", "-y"][..]),
        ("dnf", &["install", "-y"][..]),
        ("pacman", &["-S", "--noconfirm"][..]),
        ("brew", &["install"][..]),
    ] {
        let Some(bin) = probe.look_path(m) else {
            continue;
        };
        let mut argv: Vec<String> = vec![bin.to_string_lossy().into_owned()];
        argv.extend(install.iter().map(|s| s.to_string()));
        argv.push(name.to_string());
        #[cfg(unix)]
        let root = unsafe { libc::geteuid() } == 0;
        #[cfg(not(unix))]
        let root = true;
        if m != "brew" && !root {
            let Some(sudo) = probe.look_path("sudo") else {
                return Err(format!("{m} requiert root (ni root ni sudo disponibles)"));
            };
            argv.insert(0, sudo.to_string_lossy().into_owned());
        }
        let status = tokio::process::Command::new(&argv[0])
            .args(&argv[1..])
            .stdin(Stdio::null())
            .status()
            .await
            .map_err(|e| e.to_string())?;
        return exit_err(status);
    }
    Err(format!(
        "aucun gestionnaire de paquets connu — installe {name} manuellement"
    ))
}

/// `requireTools` : les outils manquants installés au mieux, puis revus.
pub async fn require_tools(probe: &Probe, tools: &[&str]) -> Result<(), String> {
    let missing: Vec<&str> = tools
        .iter()
        .copied()
        .filter(|t| !probe.has_tool(t))
        .collect();
    if missing.is_empty() {
        return Ok(());
    }
    eprintln!(
        "[info] outils manquants: {} — installation automatique…",
        missing.join(", ")
    );
    for t in &missing {
        if let Err(e) = auto_install_tool(probe, t).await {
            eprintln!("  • {t}: {e}");
        }
    }
    let still: Vec<&str> = tools
        .iter()
        .copied()
        .filter(|t| !probe.has_tool(t))
        .collect();
    if !still.is_empty() {
        return Err(format!(
            "outils toujours manquants après tentative d'install: {} — installe-les à la main puis réessaie",
            still.join(", ")
        ));
    }
    Ok(())
}

/// `ensureCompiler` (Unix) : cc, c++ et make, installés au mieux.
pub async fn ensure_compiler(probe: &Probe) -> Result<(), String> {
    let any = |names: &[&str]| names.iter().any(|n| probe.has_tool(n));
    if any(&["cc", "gcc", "clang"]) && any(&["c++", "g++", "clang++"]) && probe.has_tool("make") {
        return Ok(());
    }
    let candidates: &[&str] = if probe.has_tool("apt-get") {
        &["build-essential", "gcc", "g++", "make"]
    } else {
        &["gcc", "gcc-c++", "make"]
    };
    eprintln!("[info] compilateur C/C++ absent — installation via le gestionnaire de paquets…");
    for pkg in candidates {
        let _ = auto_install_tool(probe, pkg).await;
    }
    if any(&["cc", "gcc"]) && any(&["c++", "g++"]) && probe.has_tool("make") {
        return Ok(());
    }
    Err(
        "compilateur C/C++ introuvable — installe gcc/g++/make (ou build-essential) manuellement"
            .into(),
    )
}

/// `lcAppendLogTail` : la fin des journaux de build, dans le job.
async fn append_log_tail(log: &Log<'_>, paths: &[PathBuf]) {
    for p in paths {
        let Ok(b) = std::fs::read(p) else { continue };
        let text = String::from_utf8_lossy(&b);
        let lines: Vec<&str> = text.trim_end_matches('\n').split('\n').collect();
        let tail = &lines[lines.len().saturating_sub(30)..];
        let base = p
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_default();
        log.line(&format!("--- fin de {base} ---")).await;
        for l in tail {
            log.line(l).await;
        }
    }
}

/// `lcBuildAndSwitch` : compile llama-server et y pointe BIN. Faux : le job
/// a échoué.
async fn build_and_switch(
    ctx: &Ctx,
    b: &Backends,
    log: &Log<'_>,
    repo: &Path,
    clean: bool,
) -> bool {
    let plan = b.probe.build_plan().await;
    log.line(&format!(
        "plan de build : backend={} arch={} jobs={}",
        plan.backend, plan.cuda_arch, plan.jobs
    ))
    .await;
    log.phase("configuration CMake…").await;
    if let Err(e) = build_llamacpp(&b.probe, log, repo, &plan, clean).await {
        append_log_tail(log, &[repo.join("configure.log"), repo.join("build.log")]).await;
        b.jobs.fail(&ctx.store, &e).await;
        return false;
    }
    let bin = llama_server_bin(repo);
    if bin.is_empty() {
        b.jobs
            .fail(
                &ctx.store,
                &format!(
                    "build terminé mais binaire introuvable sous {}",
                    repo.join("build").display()
                ),
            )
            .await;
        return false;
    }
    log.line(&format!("binaire compilé : {bin}")).await;
    if let Err(e) = ctx.store.put(BK_CONFIG, "BIN", bin.into_bytes()).await {
        b.jobs
            .fail(
                &ctx.store,
                &format!("build ok mais échec d'écriture de BIN : {e}"),
            )
            .await;
        return false;
    }
    log.line("BIN mis à jour").await;
    true
}

/// La vérification commune des outils ; l'erreur est rangée dans le job.
async fn tools_ready(ctx: &Ctx, b: &Backends) -> bool {
    let res = match require_tools(&b.probe, &["git", "cmake"]).await {
        Ok(()) => ensure_compiler(&b.probe).await,
        Err(e) => Err(e),
    };
    match res {
        Ok(()) => true,
        Err(e) => {
            b.jobs.fail(&ctx.store, &e).await;
            false
        }
    }
}

/// `lcRunInstall` : clone, compilation, BIN. Dépôt déjà là : une mise à
/// jour (`force` : il est supprimé et recloné).
pub async fn run_install(ctx: &Ctx, b: &Backends, force: bool) {
    let log = Log {
        jobs: &b.jobs,
        store: &ctx.store,
    };
    let repo = b.default_repo_dir();
    log.phase("vérification des outils (git, cmake, compilateur)…")
        .await;
    if !tools_ready(ctx, b).await {
        return;
    }
    if is_dir(&repo.join(".git")) {
        if !force {
            log.phase("dépôt déjà présent — bascule en mise à jour")
                .await;
            Box::pin(run_update(ctx, b, false)).await;
            return;
        }
        log.phase("suppression du dépôt existant (--force)…").await;
        if let Err(e) = std::fs::remove_dir_all(&repo) {
            b.jobs
                .fail(
                    &ctx.store,
                    &crate::paths::os_err("unlinkat", &repo.to_string_lossy(), &e),
                )
                .await;
            return;
        }
    }
    if let Some(parent) = repo.parent()
        && let Err(e) = std::fs::create_dir_all(parent)
    {
        b.jobs
            .fail(
                &ctx.store,
                &crate::paths::os_err("mkdir", &parent.to_string_lossy(), &e),
            )
            .await;
        return;
    }
    log.phase("clone de llama.cpp…").await;
    let repo_s = repo.to_string_lossy().into_owned();
    if let Err(e) = run_step(
        &b.probe,
        &log,
        None,
        "git",
        &["clone", "--depth=1", REPO_URL, &repo_s],
    )
    .await
    {
        b.jobs
            .fail(&ctx.store, &format!("git clone a échoué : {e}"))
            .await;
        return;
    }
    if !build_and_switch(ctx, b, &log, &repo, true).await {
        return;
    }
    b.jobs.done(&ctx.store, "installation terminée").await;
}

/// `lcRunUpdate` : fetch, pull au besoin, recompilation (service arrêté le
/// temps du build), BIN.
pub async fn run_update(ctx: &Ctx, b: &Backends, clean: bool) {
    let log = Log {
        jobs: &b.jobs,
        store: &ctx.store,
    };
    log.phase("vérification des outils (git, cmake, compilateur)…")
        .await;
    if !tools_ready(ctx, b).await {
        return;
    }
    let cfg_bin = ctx
        .store
        .config()
        .await
        .get("BIN")
        .cloned()
        .unwrap_or_default();
    let repo = b.repo_dir(&cfg_bin);
    if !is_dir(&repo.join(".git")) {
        b.jobs
            .fail(
                &ctx.store,
                &format!(
                    "aucun dépôt llama.cpp ({}) — lance d'abord l'installation",
                    repo.display()
                ),
            )
            .await;
        return;
    }
    let old = b.git(&repo, &["rev-parse", "--short", "HEAD"]).await;
    b.jobs.set_commits(Some(&old), None).await;
    let mut branch = b.git(&repo, &["rev-parse", "--abbrev-ref", "HEAD"]).await;
    if branch.is_empty() || branch == "HEAD" {
        branch = "master".into();
    }
    log.phase("git fetch origin…").await;
    if let Err(e) = run_step(
        &b.probe,
        &log,
        Some(&repo),
        "git",
        &["fetch", "origin", "--quiet"],
    )
    .await
    {
        b.jobs
            .fail(&ctx.store, &format!("git fetch a échoué : {e}"))
            .await;
        return;
    }
    let local = b.git(&repo, &["rev-parse", "HEAD"]).await;
    let remote = b
        .git(&repo, &["rev-parse", &format!("origin/{branch}")])
        .await;
    if !local.is_empty() && local == remote && !clean && !llama_server_bin(&repo).is_empty() {
        b.jobs
            .done(&ctx.store, &format!("déjà à jour ({old}) — rien à faire"))
            .await;
        return;
    }
    if local != remote {
        log.phase(&format!("git pull origin/{branch}…")).await;
        if let Err(e) = run_step(
            &b.probe,
            &log,
            Some(&repo),
            "git",
            &["pull", "--ff-only", "origin", &branch],
        )
        .await
        {
            b.jobs
                .fail(
                    &ctx.store,
                    &format!("git pull a échoué (modifs locales ?) : {e}"),
                )
                .await;
            return;
        }
    }
    let new = b.git(&repo, &["rev-parse", "--short", "HEAD"]).await;
    b.jobs.set_commits(None, Some(&new)).await;
    // Le binaire en service ne se réécrit pas : le moteur s'arrête le temps
    // du build, et repart après, même en échec.
    let (svc_was_up, err) = service_stop_if_up(ctx).await;
    if svc_was_up {
        log.phase("arrêt du service le temps du build…").await;
        if !err.is_empty() {
            log.line(&format!("[warn] impossible d'arrêter le service : {err}"))
                .await;
        }
    }
    let ok = build_and_switch(ctx, b, &log, &repo, clean).await;
    if svc_was_up {
        log.phase("redémarrage du service…").await;
        let (_, e) = service(ctx, "start", false).await;
        if !e.is_empty() {
            log.line(&format!("[warn] redémarrage du service échoué : {e}"))
                .await;
        }
    }
    if !ok {
        return;
    }
    let msg = if old == new {
        format!("recompilé ({new})")
    } else {
        format!("mis à jour : {old} → {new}")
    };
    b.jobs.done(&ctx.store, &msg).await;
}

/// `looksLikeGitURL`.
pub fn looks_like_git_url(u: &str) -> bool {
    ["https://", "http://", "git@", "ssh://", "git://"]
        .iter()
        .any(|p| u.starts_with(p))
}

/// `deriveBackendName` : le nom du dépôt, suffixé du propriétaire pour un
/// fork nommé « llama.cpp ».
pub fn derive_backend_name(url: &str) -> String {
    let s = url.trim();
    let s = s.strip_suffix(".git").unwrap_or(s);
    let s = s.trim_end_matches('/');
    let parts: Vec<&str> = s.split(['/', ':']).filter(|p| !p.is_empty()).collect();
    let (repo, owner) = match parts.len() {
        0 => ("", ""),
        1 => (parts[0], ""),
        n => (parts[n - 1], parts[n - 2]),
    };
    let mut name = if repo.is_empty() {
        "backend".to_string()
    } else {
        repo.to_string()
    };
    if name.eq_ignore_ascii_case("llama.cpp") && !owner.is_empty() {
        name = format!("{name}-{owner}");
    }
    sanitize_backend_name(&name)
}

/// `installCustomBackend` : clone (ou met à jour) un fork dans
/// `backends/<nom>` et le compile, sans toucher à BIN ; rend le binaire.
pub async fn install_custom(
    b: &Backends,
    log: &Log<'_>,
    url: &str,
    name: &str,
    git_ref: &str,
) -> Result<String, String> {
    let url = url.trim();
    if url.is_empty() {
        return Err("URL du dépôt vide".into());
    }
    if !looks_like_git_url(url) {
        return Err(format!(
            "URL de dépôt invalide (attendu https://…, git@… ou ssh://…) : {url}"
        ));
    }
    let name = if name.trim().is_empty() {
        derive_backend_name(url)
    } else {
        sanitize_backend_name(name)
    };
    // `backendDir` : le nom assaini ne sort jamais de backends/.
    let dir = std::path::absolute(b.backends_dir())
        .map_err(|e| e.to_string())?
        .join(&name);
    log.phase("vérification des outils (git, cmake, compilateur)…")
        .await;
    require_tools(&b.probe, &["git", "cmake"]).await?;
    ensure_compiler(&b.probe).await?;
    let probe = &b.probe;
    if is_dir(&dir.join(".git")) {
        log.phase("dépôt déjà présent — mise à jour…").await;
        let _ = run_step(
            probe,
            log,
            Some(&dir),
            "git",
            &["fetch", "origin", "--quiet"],
        )
        .await;
        if !git_ref.is_empty() {
            run_step(probe, log, Some(&dir), "git", &["checkout", git_ref]).await?;
        } else {
            let branch = b.git(&dir, &["rev-parse", "--abbrev-ref", "HEAD"]).await;
            if !branch.is_empty() && branch != "HEAD" {
                let _ = run_step(
                    probe,
                    log,
                    Some(&dir),
                    "git",
                    &["pull", "--ff-only", "origin", &branch],
                )
                .await;
            }
        }
    } else {
        if let Some(parent) = dir.parent() {
            std::fs::create_dir_all(parent)
                .map_err(|e| crate::paths::os_err("mkdir", &parent.to_string_lossy(), &e))?;
        }
        log.phase(&format!("clone de {url}…")).await;
        let dir_s = dir.to_string_lossy().into_owned();
        run_step(
            probe,
            log,
            None,
            "git",
            &["clone", "--depth=1", url, &dir_s],
        )
        .await
        .map_err(|e| format!("git clone a échoué : {e}"))?;
        if !git_ref.is_empty() {
            let _ = run_step(
                probe,
                log,
                Some(&dir),
                "git",
                &["fetch", "--unshallow", "origin"],
            )
            .await;
            run_step(probe, log, Some(&dir), "git", &["checkout", git_ref]).await?;
        }
    }
    let plan = probe.build_plan().await;
    log.phase(&format!("compilation (backend={})…", plan.backend))
        .await;
    build_llamacpp(probe, log, &dir, &plan, false).await?;
    let bin = llama_server_bin(&dir);
    if bin.is_empty() {
        return Err(format!(
            "build terminé mais binaire introuvable sous {}",
            dir.join("build").display()
        ));
    }
    Ok(bin)
}

/// `lcRunCustomInstall` : un backend custom, sans bascule de BIN (il se
/// choisit par preset).
pub async fn run_custom(ctx: &Ctx, b: &Backends, url: &str, name: &str, git_ref: &str) {
    let log = Log {
        jobs: &b.jobs,
        store: &ctx.store,
    };
    match install_custom(b, &log, url, name, git_ref).await {
        Err(e) => b.jobs.fail(&ctx.store, &e).await,
        Ok(bin) => {
            log.line(&format!("binaire compilé : {bin}")).await;
            log.line("→ pour l'utiliser : édite un modèle → section Moteur → « backend détecté » et choisis-le.")
                .await;
            b.jobs.done(&ctx.store, "backend custom installé").await;
        }
    }
}

#[cfg(test)]
mod tests;
