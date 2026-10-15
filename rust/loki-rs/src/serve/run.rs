//! `loki-rs engine serve` — port de `cmdServe` : lit le monde (configuration
//! par l'API interne du Go, aide du moteur, GGUF, RAM, VRAM, cgroup,
//! variables), compose la ligne par [`super::build_serve_args`], puis cède la
//! place à llama-server (`exec`).
//!
//! Ce qui reste au Go : un preset Strata et le moteur d'essai de
//! l'optimiseur (`LOKI_TUNE_TRIAL`) — `loki serve` est alors lancé à notre
//! place, à l'identique. Sans accès à l'API interne (lancement à la main hors
//! de la façade), même repli.

use std::collections::{BTreeMap, HashMap};
use std::io::Write as _;
use std::net::{SocketAddr, TcpStream, ToSocketAddrs};
use std::path::{Path, PathBuf};
use std::process::{Command, ExitCode, Stdio};
use std::time::{Duration, Instant};

use super::*;
use crate::gguf;
use crate::store::{BK_CONFIG, BK_STATE, Store};

/// Variables d'environnement du lancement : celles du process, plus ce que
/// `loki serve` pose (sélection GPU, bibliothèques, variables du moteur).
#[derive(Default)]
pub struct Env {
    overlay: BTreeMap<String, String>,
}

impl Env {
    pub fn get(&self, k: &str) -> Option<String> {
        self.overlay
            .get(k)
            .cloned()
            .or_else(|| std::env::var(k).ok())
    }
    /// `os.Getenv` : vide si absente.
    pub fn getenv(&self, k: &str) -> String {
        self.get(k).unwrap_or_default()
    }
    pub fn set(&mut self, k: &str, v: &str) {
        self.overlay.insert(k.into(), v.into());
    }
    /// L'environnement complet, trié.
    pub fn all(&self) -> Vec<(String, String)> {
        let mut m: BTreeMap<String, String> = std::env::vars().collect();
        for (k, v) in &self.overlay {
            m.insert(k.clone(), v.clone());
        }
        m.into_iter().collect()
    }
    fn command(&self, bin: &str) -> Command {
        let mut c = Command::new(bin);
        c.envs(&self.overlay);
        c
    }
}

/// Écrit un fichier privé (0600).
pub(crate) fn write_private(p: &Path, data: &[u8]) -> std::io::Result<()> {
    let mut o = std::fs::OpenOptions::new();
    o.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        o.mode(0o600);
    }
    o.open(p)?.write_all(data)
}

/// Sortie combinée d'une commande bornée dans le temps ; `None` si elle n'a
/// pas pu démarrer. (succès, sortie)
fn output_with_timeout(mut cmd: Command, timeout: Duration) -> Option<(bool, String)> {
    let path = std::env::temp_dir().join(format!(
        "loki-serve-{}-{}.out",
        std::process::id(),
        Instant::now().elapsed().as_nanos() ^ rand_u64()
    ));
    let f = std::fs::File::create(&path).ok()?;
    let f2 = f.try_clone().ok()?;
    let mut child = cmd.stdin(Stdio::null()).stdout(f).stderr(f2).spawn().ok()?;
    let deadline = Instant::now() + timeout;
    let ok = loop {
        match child.try_wait() {
            Ok(Some(st)) => break st.success(),
            Ok(None) if Instant::now() < deadline => std::thread::sleep(Duration::from_millis(20)),
            _ => {
                let _ = child.kill();
                let _ = child.wait();
                break false;
            }
        }
    };
    let out = std::fs::read(&path).unwrap_or_default();
    let _ = std::fs::remove_file(&path);
    Some((ok, String::from_utf8_lossy(&out).into_owned()))
}

fn rand_u64() -> u128 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_nanos())
}

/// `binHelp` : sortie de `<bin> --help` (vide si illisible).
pub fn bin_help(env: &Env, bin: &str) -> String {
    let mut c = env.command(bin);
    c.arg("--help");
    match output_with_timeout(c, Duration::from_secs(5)) {
        Some((ok, out)) if ok || !out.is_empty() => out,
        _ => String::new(),
    }
}

/// `libraryPathValue` (Linux) : dossier du binaire, CUDA, puis l'existant.
pub fn library_path_value(env: &Env, dir: &str) -> (String, String) {
    let mut parts = vec![dir.to_string()];
    parts.extend(cuda_lib_dirs());
    let var = if cfg!(target_os = "macos") {
        "DYLD_LIBRARY_PATH"
    } else {
        "LD_LIBRARY_PATH"
    };
    let existing = env.getenv(var);
    if !existing.is_empty() {
        parts.push(existing);
    }
    (var.into(), parts.join(":"))
}

fn is_dir(p: &str) -> bool {
    std::fs::metadata(p).is_ok_and(|m| m.is_dir())
}

fn is_file(p: &str) -> bool {
    std::fs::metadata(p).is_ok_and(|m| !m.is_dir())
}

/// `cudaLibDirs`.
fn cuda_lib_dirs() -> Vec<String> {
    let mut dirs: Vec<String> = Vec::new();
    let mut add = |d: String| {
        if !d.is_empty() && !dirs.contains(&d) && is_dir(&d) {
            dirs.push(d);
        }
    };
    add("/usr/local/cuda/lib64".into());
    add("/usr/local/cuda/targets/x86_64-linux/lib".into());
    let mut versioned: Vec<String> = std::fs::read_dir("/usr/local")
        .map(|rd| {
            rd.flatten()
                .map(|e| e.file_name().to_string_lossy().into_owned())
                .filter(|n| n.starts_with("cuda-"))
                .map(|n| format!("/usr/local/{n}/lib64"))
                .filter(|p| Path::new(p).exists())
                .collect()
        })
        .unwrap_or_default();
    versioned.sort();
    for v in versioned.into_iter().rev() {
        add(v);
    }
    dirs
}

/// `engineSource` / `prebuiltOwns` / `engineOwns` / `samePath`.
pub struct EnginePaths {
    pub home: PathBuf,
}

fn norm_path(s: &str) -> String {
    crate::paths::go_clean(s).replace('\\', "/").to_lowercase()
}

impl EnginePaths {
    fn prebuilt_dir(&self) -> String {
        self.home
            .join("backends")
            .join("llama.cpp-prebuilt")
            .to_string_lossy()
            .into_owned()
    }
    fn engine_dir(&self) -> String {
        self.home.join("engine").to_string_lossy().into_owned()
    }
    pub fn prebuilt_owns(&self, p: &str) -> bool {
        !p.is_empty() && norm_path(p).starts_with(&(norm_path(&self.prebuilt_dir()) + "/"))
    }
    pub fn engine_owns(&self, p: &str) -> bool {
        !p.is_empty() && norm_path(p).starts_with(&(norm_path(&self.engine_dir()) + "/"))
    }
    /// `prebuiltServerBin` : le binaire du VERSION, sinon le dernier.
    fn prebuilt_server_bin(&self) -> String {
        let mut found = Vec::new();
        fn walk(d: &Path, out: &mut Vec<String>) {
            let Ok(rd) = std::fs::read_dir(d) else { return };
            for e in rd.flatten() {
                let p = e.path();
                if e.file_type().is_ok_and(|t| t.is_dir()) {
                    walk(&p, out);
                } else if e.file_name() == "llama-server" {
                    out.push(p.to_string_lossy().into_owned());
                }
            }
        }
        walk(Path::new(&self.prebuilt_dir()), &mut found);
        found.sort();
        if found.is_empty() {
            return String::new();
        }
        let tag = std::fs::read_to_string(Path::new(&self.prebuilt_dir()).join("VERSION"))
            .ok()
            .and_then(|s| s.split_whitespace().next().map(str::to_string))
            .unwrap_or_default();
        if !tag.is_empty()
            && let Some(p) = found
                .iter()
                .find(|p| p.replace('\\', "/").contains(&format!("/llama-{tag}/")))
        {
            return p.clone();
        }
        found.last().cloned().unwrap_or_default()
    }
    /// `prebuiltResolveBin`.
    pub fn prebuilt_resolve_bin(&self, bin: &str) -> String {
        if !self.prebuilt_owns(bin) || std::fs::metadata(bin).is_ok() {
            return bin.into();
        }
        let cur = self.prebuilt_server_bin();
        if cur.is_empty() { bin.into() } else { cur }
    }
    /// `providedEngineBin`.
    fn provided_engine_bin(&self) -> String {
        let p = std::env::var("LOKI_ENGINE_BIN").unwrap_or_default();
        let p = trim(&p);
        if p.is_empty() {
            return String::new();
        }
        if is_file(&self.prebuilt_resolve_bin(p)) {
            p.into()
        } else {
            String::new()
        }
    }
    /// `engineSource`.
    pub fn engine_source(&self, bin: &str) -> &'static str {
        let img = self.provided_engine_bin();
        if !img.is_empty() && same_path(bin, &img) {
            "image"
        } else if self.engine_owns(bin) {
            "downloaded"
        } else if self.prebuilt_owns(bin) {
            "prebuilt"
        } else {
            "custom"
        }
    }
}

/// `samePath` : symlinks résolus, casse ignorée.
pub fn same_path(a: &str, b: &str) -> bool {
    if a.is_empty() || b.is_empty() {
        return false;
    }
    let n = |p: &str| -> String {
        let r = std::fs::canonicalize(p)
            .map(|x| x.to_string_lossy().into_owned())
            .unwrap_or_else(|_| p.to_string());
        norm_path(&r)
    };
    n(a) == n(b)
}

/// `parseEngineBuild`.
pub fn parse_engine_build(out: &str) -> i64 {
    let long = regex::Regex::new(r"build\s+(\d+),\s*commit\s+([0-9a-fA-F]+)").expect("motif");
    let short = regex::Regex::new(r"(?m)^\s*(?:version|build)\s*:\s*(\d+)\s*\(([0-9a-fA-F]+)\)")
        .expect("motif");
    for re in [long, short] {
        if let Some(m) = re.captures(out) {
            return m[1].parse().unwrap_or(0);
        }
    }
    0
}

/// `engineBuildTrusted`.
fn engine_build_trusted(env: &Env, ep: &EnginePaths, bin: &str) -> i64 {
    let src = ep.engine_source(bin);
    if src == "custom" || !is_file(bin) {
        return 0;
    }
    let mut c = env.command(bin);
    c.arg("--version");
    let dir = Path::new(bin)
        .parent()
        .map(|d| d.to_string_lossy().into_owned())
        .unwrap_or_default();
    let (k, v) = library_path_value(env, &dir);
    c.env(k, v);
    let out = output_with_timeout(c, Duration::from_secs(15))
        .map(|(_, o)| o)
        .unwrap_or_default();
    engine_build_trust(src, parse_engine_build(&out))
}

fn has_tool(name: &str) -> bool {
    crate::paths::look_path(name).is_some()
}

/// `nvidiaGPUCount`.
fn nvidia_gpu_count() -> i64 {
    if !has_tool("nvidia-smi") {
        return 0;
    }
    let mut c = Command::new("nvidia-smi");
    c.args(["--query-gpu=index", "--format=csv,noheader"]);
    match output_with_timeout(c, Duration::from_secs(3)) {
        Some((true, out)) => out.split('\n').filter(|l| !trim(l).is_empty()).count() as i64,
        _ => 0,
    }
}

/// `nvidiaVRAMMiB`.
fn nvidia_vram_mib(cvd: &str, pci: bool) -> i64 {
    if !has_tool("nvidia-smi") {
        return 0;
    }
    let mut c = Command::new("nvidia-smi");
    c.args([
        "--query-gpu=index,memory.total",
        "--format=csv,noheader,nounits",
    ]);
    match output_with_timeout(c, Duration::from_secs(3)) {
        Some((true, out)) => vram_from_smi(&out, cvd, pci),
        _ => 0,
    }
}

/// Lectures des variables du moteur (`probe*Env`).
fn probe_env(env: &Env, si: &mut SysInfo, keys: &[&str], trim_check: bool) {
    for k in keys {
        if let Some(v) = env.get(k) {
            let keep = if trim_check {
                !trim(&v).is_empty()
            } else {
                !v.is_empty()
            };
            if keep {
                si.arg_env.insert((*k).into(), v);
            }
        }
    }
}

/// `probeServeGPUs`.
fn probe_serve_gpus(env: &Env, cfg: &Cfg, extra: &[String], si: &mut SysInfo) {
    si.launch_queues = env.getenv("CUDA_SCALE_LAUNCH_QUEUES");
    si.arg_env = HashMap::new();
    for k in SERVE_ARG_ENV {
        match env.get(k) {
            Some(v) if !v.is_empty() => {
                si.arg_env.insert((*k).into(), v);
            }
            Some(_) if *k == "LLAMA_ARG_NO_KV_OFFLOAD" => {
                si.arg_env.insert((*k).into(), "1".into());
            }
            _ => {}
        }
    }
    if !si.launch_queues.is_empty()
        || trim(get(cfg, "CUDA_LAUNCH_QUEUES")).eq_ignore_ascii_case("off")
    {
        return;
    }
    if has_any_flag(extra, &["-dev", "--device"]) || !si.env("LLAMA_ARG_DEVICE").is_empty() {
        return;
    }
    let cvd = trim(&env.getenv("CUDA_VISIBLE_DEVICES")).to_string();
    if !cvd.is_empty() {
        si.gpus = cvd.split(',').filter(|d| !trim(d).is_empty()).count() as i64;
        return;
    }
    si.gpus = nvidia_gpu_count();
}

/// `probeCacheRAM`.
pub(crate) fn probe_cache_ram(env: &Env, cfg: &Cfg, extra: &[String], si: &mut SysInfo) {
    probe_env(env, si, CACHE_ARG_ENV, true);
    if !si.model.is_empty() {
        let mp = Path::new(&si.model);
        let dir = mp.parent().unwrap_or(Path::new("."));
        let base = mp
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_default();
        si.model_bytes = crate::engstatus::shard_family_size(dir, &base);
        if let Ok(g) = gguf::gguf_meta(mp) {
            si.gguf = Some(g);
        }
    }
    let (used, total) = crate::sysinfo::ram_usage_mb();
    if total > 0 {
        si.ram_mib = total;
        si.ram_avail_mib = total - used;
    }
    if cfg!(target_os = "linux") {
        let (limit, cur) = cgroup_mem_mib(Path::new("/"));
        if limit > 0 {
            if si.ram_mib == 0 || limit < si.ram_mib {
                si.ram_mib = limit;
            }
            let free = limit - cur;
            if free >= 0 && (si.ram_avail_mib == 0 || free < si.ram_avail_mib) {
                si.ram_avail_mib = free;
            }
        }
    }
    si.unified_mem = cfg!(all(target_os = "macos", target_arch = "aarch64"));
    if cfg!(target_os = "macos")
        || has_any_flag(extra, &["-dev", "--device"])
        || !si.env("LLAMA_ARG_DEVICE").is_empty()
    {
        return;
    }
    let (cvd, pci) = match get(cfg, "CUDA_VISIBLE_DEVICES") {
        "" => (
            env.getenv("CUDA_VISIBLE_DEVICES"),
            env.getenv("CUDA_DEVICE_ORDER") == "PCI_BUS_ID",
        ),
        v => (v.to_string(), true),
    };
    si.vram_mib = nvidia_vram_mib(&cvd, pci);
}

/// `listEngineDevices`.
fn list_engine_devices(env: &Env, bin: &str) -> Vec<SplitDev> {
    let mut c = env.command(bin);
    c.arg("--list-devices");
    match output_with_timeout(c, Duration::from_secs(20)) {
        Some((true, out)) => parse_list_devices(&out),
        _ => Vec::new(),
    }
}

/// `waitPortFree`.
pub fn wait_port_free(host: &str, port: &str, wait: Duration) -> Result<(), String> {
    if port.is_empty() {
        return Ok(());
    }
    let mut h = host.trim_matches(['[', ']']).to_string();
    if h.is_empty() || h == "0.0.0.0" || h == "::" {
        h = "127.0.0.1".into();
    }
    let addr = if h.contains(':') {
        format!("[{h}]:{port}")
    } else {
        format!("{h}:{port}")
    };
    let deadline = Instant::now() + wait;
    loop {
        let reachable = addr
            .to_socket_addrs()
            .ok()
            .and_then(|mut a| a.next())
            .is_some_and(|sa: SocketAddr| {
                TcpStream::connect_timeout(&sa, Duration::from_millis(500)).is_ok()
            });
        if !reachable {
            return Ok(());
        }
        if Instant::now() > deadline {
            return Err(format!(
                "le port {port} est déjà utilisé (sans doute un ancien llama-server encore actif) : « loki stop », docker restart loki, ou change PORT"
            ));
        }
        std::thread::sleep(Duration::from_millis(500));
    }
}

/// Erreur de `loki serve` et son code de sortie.
pub struct ServeError {
    pub msg: String,
    pub code: u8,
}

impl From<String> for ServeError {
    fn from(msg: String) -> Self {
        ServeError { msg, code: 1 }
    }
}

/// Accès à la base du Go pour ce lancement.
pub struct GoStore {
    rt: tokio::runtime::Runtime,
    store: Store,
}

impl GoStore {
    /// `LOKI_GO_ADDR` + `LOKI_INTERNAL_TOKEN`, posées par la façade pour le Go
    /// et héritées par ses enfants.
    pub fn from_env() -> Option<Self> {
        let addr: SocketAddr = std::env::var("LOKI_GO_ADDR").ok()?.parse().ok()?;
        let token = std::env::var(crate::auth::INTERNAL_ENV).ok()?;
        if token.is_empty() {
            return None;
        }
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .ok()?;
        Some(GoStore {
            rt,
            store: Store::new(addr, token),
        })
    }
    pub fn config(&self) -> Result<Cfg, String> {
        self.rt
            .block_on(self.store.all(BK_CONFIG))
            .map(|m| m.into_iter().collect())
    }
    pub fn get(&self, bucket: &str, key: &str) -> Option<Vec<u8>> {
        self.rt.block_on(self.store.get(bucket, key)).ok().flatten()
    }
    pub fn put(&self, bucket: &str, key: &str, v: Vec<u8>) {
        let _ = self.rt.block_on(self.store.put(bucket, key, v));
    }
    pub fn del(&self, bucket: &str, key: &str) {
        let _ = self.rt.block_on(self.store.del(bucket, key));
    }
}

/// Lit le jeton de tentative et les échecs inscrits.
fn read_spec_auto(st: &GoStore) -> (Option<SpecAutoMark>, HashMap<String, String>) {
    let attempt = st
        .get(BK_STATE, SPEC_ATTEMPT_KEY)
        .and_then(|b| serde_json::from_slice::<SpecAutoMark>(&b).ok());
    let failed = st
        .get(BK_STATE, SPEC_FAILED_KEY)
        .and_then(|b| serde_json::from_slice::<HashMap<String, String>>(&b).ok())
        .unwrap_or_default();
    (attempt, failed)
}

/// `specAutoSettle`.
fn spec_auto_settle(st: &GoStore, cur: &SpecAutoMark, why: &str, record: bool, attempt: bool) {
    let (_, mut failed) = read_spec_auto(st);
    st.del(BK_STATE, SPEC_ATTEMPT_KEY);
    if record {
        if failed.len() >= SPEC_FAILED_MAX {
            failed.clear();
        }
        failed.insert(cur.key(), why.into());
        let sorted: BTreeMap<_, _> = failed.into_iter().collect();
        st.put(
            BK_STATE,
            SPEC_FAILED_KEY,
            crate::gojson::marshal(&sorted).into_bytes(),
        );
    }
    if attempt {
        st.put(
            BK_STATE,
            SPEC_ATTEMPT_KEY,
            crate::gojson::marshal(cur).into_bytes(),
        );
    }
}

/// Ce que [`prepare`] rend : de quoi lancer le moteur.
pub struct Ready {
    pub bin: String,
    pub launch: Launch,
    pub env: Env,
    pub home: PathBuf,
}

/// `cmdServe` jusqu'à la ligne de commande comprise (sans exec). `Ok(None)` :
/// rien à lancer (preset externe).
pub fn prepare(st: &GoStore, home: &Path) -> Result<Option<Ready>, ServeError> {
    let cfg = st.config()?;
    if is_external(&cfg) {
        println!("[info] preset externe actif : pas de moteur local à lancer");
        return Ok(None);
    }
    let mut env = Env::default();
    let mut bin = get(&cfg, "BIN").to_string();
    if bin.is_empty() {
        return Err("BIN non défini — lance « loki edit »".to_string().into());
    }
    let model = get(&cfg, "MODEL").to_string();
    if model.is_empty() {
        return Err("MODEL non défini — lance « loki edit »".to_string().into());
    }
    let stored =
        gguf::parse_stored_model_dirs(st.get(BK_STATE, gguf::STATE_MODEL_DIRS_KEY).as_deref());
    let env_dirs = std::env::var(gguf::MODEL_DIRS_ENV).ok();
    let resolve = |m: &str| gguf::resolve_serve_model_path(m, home, &stored, env_dirs.as_deref());
    let model = resolve(&model)?.to_string_lossy().into_owned();
    if std::fs::metadata(&model).is_err() {
        return Err(format!("modèle introuvable : {model}").into());
    }
    let mp = Path::new(&model);
    let mdir = mp.parent().unwrap_or(Path::new("."));
    let mbase = mp
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    let fam = gguf::shard_family(&mbase);
    let missing: Vec<String> = fam
        .iter()
        .filter(|n| !std::fs::metadata(mdir.join(n)).is_ok_and(|m| !m.is_dir()))
        .cloned()
        .collect();
    if !missing.is_empty() {
        return Err(format!(
            "modèle incomplet : il manque {} dans {} — ce modèle tient en {} fichiers, télécharge-les tous",
            missing.join(", "),
            mdir.display(),
            fam.len()
        )
        .into());
    }
    if !crate::paths::go_is_abs(&bin) {
        bin = home.join(&bin).to_string_lossy().into_owned();
    }
    let ep = EnginePaths {
        home: home.to_path_buf(),
    };
    bin = ep.prebuilt_resolve_bin(&bin);
    let bdir = Path::new(&bin)
        .parent()
        .map(|d| d.to_string_lossy().into_owned())
        .unwrap_or_default();
    let (lk, lv) = library_path_value(&env, &bdir);
    env.set(&lk, &lv);
    for (k, v) in cuda_device_env(&cfg) {
        env.set(&k, &v);
    }

    let mut si = SysInfo {
        help: bin_help(&env, &bin),
        model: model.clone(),
        ..SysInfo::default()
    };
    let mm = trim(get(&cfg, "MMPROJ")).to_string();
    if !mm.is_empty() {
        let p = resolve(&mm)
            .map_err(|e| format!("projecteur vision introuvable : {mm} ({e})"))?
            .to_string_lossy()
            .into_owned();
        if std::fs::metadata(&p).is_err() {
            return Err(format!("projecteur vision introuvable : {p}").into());
        }
        si.mmproj = p;
    }
    let key = st
        .get(BK_STATE, "api_key")
        .map(|b| String::from_utf8_lossy(&b).into_owned())
        .unwrap_or_default();
    si.api_key = if key.is_empty() {
        trim(get(&cfg, "API_KEY")).to_string()
    } else {
        key
    };
    if cfg!(target_os = "linux") && env.getenv("LLAMA_ARG_THREADS").is_empty() {
        si.cpu = cpuset_threads(Path::new("/"));
    }

    let extra = crate::models::split_args(get(&cfg, "EXTRA_ARGS"));
    probe_serve_gpus(&env, &cfg, &extra, &mut si);
    // probeExpertEnv
    for k in EXPERT_ENV_KEYS {
        let v = env.getenv(k);
        if !v.is_empty() {
            si.user_env.insert((*k).into(), v);
        }
    }
    probe_env(&env, &mut si, FIT_ARG_ENV, false);
    // probeLoadEnv
    for k in LOAD_ARG_ENV {
        let v = trim(&env.getenv(k)).to_string();
        if !v.is_empty() {
            si.arg_env.insert((*k).into(), v);
        }
    }
    probe_env(&env, &mut si, FIDELITY_ARG_ENV, false);
    probe_cache_ram(&env, &cfg, &extra, &mut si);
    probe_env(&env, &mut si, CKPT_ARG_ENV, true);
    if side_slot_on(&cfg) {
        probe_env(&env, &mut si, SIDE_SLOT_ARG_ENV, true);
    }
    if split_mode_key(&cfg) == "tensor" && help_supports_split_tensor(&si.help) {
        probe_env(&env, &mut si, SPLIT_ARG_ENV, true);
        let ar = env.getenv("GGML_CUDA_ALLREDUCE");
        if !ar.is_empty() {
            si.user_env.insert("GGML_CUDA_ALLREDUCE".into(), ar);
        }
        si.split_devs = list_engine_devices(&env, &bin);
    }
    let (norm_extra, _) = normalize_load_flags(&extra, help_supports_load_mode(&si.help));
    if load_guard_needs_devices(&cfg, &norm_extra, &si) {
        let devs = if si.split_devs.is_empty() {
            list_engine_devices(&env, &bin)
        } else {
            si.split_devs.clone()
        };
        si.engine_vram_mib = engine_devs_vram(&devs);
    }
    let spec = spec_mode(&cfg);
    if spec != "off" && !spec.is_empty() {
        probe_env(&env, &mut si, SPEC_ARG_ENV, true);
        let d = trim(get(&cfg, "MODEL_DRAFT")).to_string();
        if !d.is_empty() {
            match resolve(&d)
                .map_err(|e| e.to_string())
                .and_then(|p| match std::fs::metadata(&p) {
                    Ok(_) => Ok(p),
                    Err(e) => Err(format!("stat {}: {e}", p.display())),
                }) {
                Ok(p) => {
                    si.draft = p.to_string_lossy().into_owned();
                    si.draft_is_model = same_file(&si.draft, &si.model);
                    if let Ok(g) = gguf::gguf_meta(&p) {
                        si.draft_gguf = Some(g);
                    }
                }
                Err(e) => si.draft_err = e,
            }
        }
    }
    if gguf_hybrid(si.gguf.as_ref()) || spec == "auto" {
        si.engine_build = engine_build_trusted(&env, &ep, &bin);
    }
    let mark = SpecAutoMark {
        fp: crate::presets::config_fingerprint(&cfg),
        bin: bin.clone(),
        build: si.engine_build,
    };
    let mut spec_record = false;
    if spec == "auto" {
        let (attempt, failed) = read_spec_auto(st);
        let (why, rec) = spec_auto_verdict(&mark, attempt.as_ref(), &failed);
        si.spec_auto_blocked = why;
        spec_record = rec;
    }
    if slot_persist_on(&cfg) {
        probe_env(&env, &mut si, SLOT_PERSIST_ARG_ENV, true);
    }
    if si.help.contains("--slot-save-path") && !has_any_flag(&extra, &["--slot-save-path"]) {
        si.slot_dir = prepare_slot_dir(home, slot_persist_on(&cfg));
    }

    let launch = build_serve_args(&cfg, &extra, &bin, &si);
    for (k, v) in &launch.env {
        env.set(k, v);
    }
    let mut persist_key = String::new();
    let (persist_ok, _) = slot_persist_plan(&cfg, &extra, &si);
    if persist_ok {
        persist_key = slot_persist_key(&launch.args, &launch.env, &env.all(), &si, &bin);
        slot_persist_prune(Path::new(&si.slot_dir));
    } else if !slot_persist_on(&cfg) {
        slot_persist_purge(&home.join("slots"));
    }
    for n in &launch.notes {
        eprintln!("[loki serve] {n}");
    }
    let (_, refuse) = load_mode_risk(&cfg, &launch.args, &si);
    if !refuse.is_empty() {
        return Err(ServeError {
            msg: refuse,
            code: LOAD_GUARD_EXIT_STATUS as u8,
        });
    }
    let host = arg_value(&launch.args, "--host");
    let port = arg_value(&launch.args, "--port");
    wait_port_free(&host, &port, Duration::from_secs(5))?;
    let (kt, vt, _) = effective_kv_types(&cfg, &extra, &si.arg_env);
    if slow_kv(&kt, &vt) {
        eprintln!(
            "[loki serve] avertissement : cache KV {kt}/{vt} non accéléré par llama.cpp CUDA (converti en f16 à chaque pas, lent). Préfère q8_0/q8_0, q4_0/q4_0 ou f16."
        );
    }
    if spec == "auto" {
        let auto = spec_args(&cfg, &extra, &si).auto;
        spec_auto_settle(st, &mark, &si.spec_auto_blocked, spec_record, auto);
    }
    write_slot_persist_marker(home, &persist_key);
    eprintln!(
        "[loki serve] {bin}  model={}  port={port}",
        crate::paths::base_name(&model)
    );
    st.put(
        BK_STATE,
        "engine_cmdline",
        engine_cmdline(&launch.env, &launch.args).into_bytes(),
    );
    Ok(Some(Ready {
        bin,
        launch,
        env,
        home: home.to_path_buf(),
    }))
}

fn same_file(a: &str, b: &str) -> bool {
    #[cfg(unix)]
    {
        use std::os::unix::fs::MetadataExt;
        match (std::fs::metadata(a), std::fs::metadata(b)) {
            (Ok(x), Ok(y)) => x.dev() == y.dev() && x.ino() == y.ino(),
            _ => false,
        }
    }
    #[cfg(not(unix))]
    {
        same_path(a, b)
    }
}

/// Lance `loki serve` (Go) à notre place : Strata, essai de l'optimiseur,
/// pas d'API interne.
fn delegate_to_go(go_bin: &Path) -> ExitCode {
    #[cfg(unix)]
    {
        use std::os::unix::process::CommandExt;
        let err = Command::new(go_bin).arg("serve").exec();
        eprintln!("[err] exec {} serve : {err}", go_bin.display());
        ExitCode::FAILURE
    }
    #[cfg(not(unix))]
    {
        match Command::new(go_bin).arg("serve").status() {
            Ok(s) if s.success() => ExitCode::SUCCESS,
            _ => ExitCode::FAILURE,
        }
    }
}

/// `loki-rs engine serve`.
pub fn run(go_bin: &Path, home: &Path) -> ExitCode {
    if std::env::var("LOKI_TUNE_TRIAL").is_ok_and(|v| !v.is_empty()) {
        return delegate_to_go(go_bin);
    }
    let Some(st) = GoStore::from_env() else {
        return delegate_to_go(go_bin);
    };
    match st.config() {
        Ok(cfg) if is_strata(&cfg) => return delegate_to_go(go_bin),
        Ok(_) => {}
        Err(_) => return delegate_to_go(go_bin),
    }
    let ready = match prepare(&st, home) {
        Ok(Some(r)) => r,
        Ok(None) => return ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("[err] {}", e.msg);
            return ExitCode::from(e.code);
        }
    };
    let _ = std::env::set_current_dir(&ready.home);
    exec_engine(&ready)
}

fn exec_engine(r: &Ready) -> ExitCode {
    let args = &r.launch.args;
    let mut c = Command::new(&r.bin);
    c.args(&args[1..]).env_clear().envs(r.env.all());
    #[cfg(unix)]
    {
        use std::os::unix::process::CommandExt;
        c.arg0(&args[0]);
        let err = c.exec();
        eprintln!("[err] exec {} : {err}", r.bin);
        ExitCode::FAILURE
    }
    #[cfg(not(unix))]
    {
        match c.status() {
            Ok(s) if s.success() => ExitCode::SUCCESS,
            _ => ExitCode::FAILURE,
        }
    }
}
