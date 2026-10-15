//! Routes de Strata tenues par la façade (lot 25) : la machine et ce qui y
//! tient (`/api/strata`), les réglages d'un modèle installé
//! (`/api/strata/settings`) et l'installation (`/api/strata/install`, un job
//! des moteurs : paquet figé vérifié, installeur de Strata, preset, bascule).

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use http_body_util::{BodyExt, Limited};
use hyper::body::Incoming;
use hyper::{Method, Request, Response, StatusCode};
use serde_json::{Value, json};
use sha2::Digest;

use super::*;
use crate::api::Ctx;
use crate::auth::json as send_json;
use crate::backends::probe::Probe;
use crate::backends::{Backends, Log};
use crate::gojson::{self, Kind};
use crate::proxy::Body;
use crate::store::{BK_STATE, Store};

const SETTINGS_REQ: &str = "strataSettingsReq";
const INSTALL_REQ: &str = "strataInstallReq";

/// Strata, tenu par la façade.
pub struct Strata {
    pub paths: Paths,
    /// PATH des outils (nvidia-smi, python3, bash) et racine de /proc.
    pub probe: Probe,
    /// `strataReleaseBase`.
    pub release_base: String,
    /// `strataAssetSHA`.
    pub asset_sha: Mutex<HashMap<String, String>>,
    /// Linux x86_64 (le seul paquet publié).
    pub platform_ok: bool,
    /// Place libre imposée (tests) ; `None` : celle du disque.
    pub disk_free: Mutex<Option<i64>>,
    client: reqwest::Client,
}

impl Strata {
    pub fn new(home: PathBuf, probe: Probe) -> Self {
        crate::chat::engine::install_crypto();
        Strata {
            paths: Paths { home },
            probe,
            release_base: RELEASE_BASE.into(),
            asset_sha: Mutex::new(default_asset_sha()),
            platform_ok: cfg!(all(target_os = "linux", target_arch = "x86_64")),
            disk_free: Mutex::new(None),
            client: reqwest::Client::builder().build().unwrap_or_default(),
        }
    }

    fn meminfo(&self) -> Option<String> {
        std::fs::read_to_string(self.probe.sys("/proc/meminfo")).ok()
    }

    /// `totalRAMGB` (Linux) : MemTotal, en Gio.
    pub fn total_ram_gb(&self) -> f64 {
        let Some(b) = self.meminfo() else {
            return 0.0;
        };
        for line in b.split('\n') {
            if line.starts_with("MemTotal:") {
                let f: Vec<&str> = line.split_whitespace().collect();
                if f.len() >= 2
                    && let Ok(kb) = f[1].parse::<f64>()
                {
                    return kb / (1024.0 * 1024.0);
                }
            }
        }
        0.0
    }

    /// `memAvailableGB` : MemAvailable, en Go ; la RAM totale à défaut.
    pub fn mem_available_gb(&self) -> f64 {
        let Some(b) = self.meminfo() else {
            return self.total_ram_gb();
        };
        for line in b.split('\n') {
            if let Some(v) = line.strip_prefix("MemAvailable:")
                && let Some(f) = v.split_whitespace().next()
                && let Ok(kb) = f.parse::<f64>()
            {
                return kb * 1024.0 / 1e9;
            }
        }
        self.total_ram_gb()
    }

    /// `strataEngineRSSGB` : la RAM tenue par le moteur Strata en marche
    /// (arbre du service), en Go.
    pub fn engine_rss_gb(&self, cfg: &Cfg) -> f64 {
        if !is_strata(cfg) {
            return 0.0;
        }
        let svc = std::env::var("LOKI_SERVICE")
            .ok()
            .filter(|s| !s.is_empty())
            .unwrap_or_else(|| "loki-engine".into());
        let root: i64 = std::fs::read_to_string(self.paths.home.join(format!("{svc}.pid")))
            .ok()
            .and_then(|s| s.trim().parse().ok())
            .unwrap_or(0);
        if root <= 0 {
            return 0.0;
        }
        let proc_dir = self.probe.sys("/proc");
        let Ok(rd) = std::fs::read_dir(&proc_dir) else {
            return 0.0;
        };
        let mut parent: HashMap<i64, i64> = HashMap::new();
        let mut rss: HashMap<i64, f64> = HashMap::new();
        for e in rd.flatten() {
            let Ok(pid) = e.file_name().to_string_lossy().parse::<i64>() else {
                continue;
            };
            let Ok(b) = std::fs::read_to_string(e.path().join("status")) else {
                continue;
            };
            for line in b.split('\n') {
                if let Some(v) = line.strip_prefix("PPid:") {
                    parent.insert(pid, v.trim().parse().unwrap_or(0));
                } else if let Some(v) = line.strip_prefix("VmRSS:")
                    && let Some(f) = v.split_whitespace().next()
                {
                    rss.insert(pid, f.parse::<f64>().unwrap_or(0.0) * 1024.0 / 1e9);
                }
            }
        }
        let mut total = 0.0;
        for (pid, r) in &rss {
            let (mut p, mut n) = (*pid, 0);
            while p > 0 && n < 64 {
                if p == root {
                    total += r;
                    break;
                }
                p = parent.get(&p).copied().unwrap_or(0);
                n += 1;
            }
        }
        total
    }

    /// `strataCheckPython` : Python 3.10+ et son module venv.
    pub async fn check_python(&self) -> Result<String, (String, String)> {
        let Some(py) = self.probe.look_path("python3") else {
            return Err((
                String::new(),
                "Python 3 est introuvable : installe-le (apt install python3 python3-venv)".into(),
            ));
        };
        let py_s = py.to_string_lossy().into_owned();
        let mut c = tokio::process::Command::new(&py);
        c.arg("--version").stdin(std::process::Stdio::null());
        let (out, err) = crate::backends::devices::combined(c, &py_s, None).await;
        if err.is_some() {
            return Err((
                String::new(),
                "Python 3 est introuvable : installe-le (apt install python3 python3-venv)".into(),
            ));
        }
        let re = regex::Regex::new(r"Python ([0-9]+)\.([0-9]+)").expect("motif");
        let Some(m) = re.captures(&out) else {
            return Err((
                String::new(),
                format!("version de Python illisible : {}", out.trim()),
            ));
        };
        let maj: i64 = m[1].parse().unwrap_or(0);
        let mnr: i64 = m[2].parse().unwrap_or(0);
        let ver = format!("{}.{}", &m[1], &m[2]);
        if maj < 3 || (maj == 3 && mnr < 10) {
            return Err((
                ver.clone(),
                format!("Python {ver} est trop ancien : il faut Python 3.10 ou plus récent"),
            ));
        }
        let mut c = tokio::process::Command::new(&py);
        c.args(["-c", "import venv, ensurepip"])
            .stdin(std::process::Stdio::null());
        let (_, err) = crate::backends::devices::combined(c, &py_s, None).await;
        if err.is_some() {
            return Err((
                ver.clone(),
                format!("le module venv de Python manque : apt install python{ver}-venv"),
            ));
        }
        Ok(ver)
    }

    /// `strataDetect` : la machine, pour l'écran d'installation.
    pub async fn detect(&self, cfg: &Cfg) -> Env {
        let mut env = Env {
            ram_gb: self.total_ram_gb(),
            ..Env::default()
        };
        env.avail_gb = (self.mem_available_gb() + self.engine_rss_gb(cfg)).min(env.ram_gb);
        let forced = *self.disk_free.lock().unwrap_or_else(|e| e.into_inner());
        let free = forced.unwrap_or_else(|| {
            crate::downloads::disk_free(&self.paths.strata_home().to_string_lossy())
        });
        if free > 0 {
            env.disk_free_gb = free as f64 / 1e9;
        }
        if !self.platform_ok {
            env.reason = "Strata n'est proposé que sous Linux (x86_64)".into();
            return env;
        }
        let gpus = match self.probe.detect_gpus().await {
            Ok(g) if !g.is_empty() => g,
            Ok(_) => {
                env.reason = "Strata demande une carte NVIDIA (aucune carte détectée)".into();
                return env;
            }
            Err(e) => {
                env.reason = format!("Strata demande une carte NVIDIA ({e})");
                return env;
            }
        };
        for g in gpus {
            let mib: f64 = g.mem_total.trim().parse().unwrap_or(0.0);
            let cc: f64 = g.cap.trim().parse().unwrap_or(0.0);
            env.gpus.push(Gpu {
                index: g.index,
                name: g.name,
                vram_gb: mib / 1024.0,
                arch: (cc * 10.0 + 0.5) as i64,
            });
        }
        let mut cards = env.gpus.clone();
        cards.sort_by(|a, b| {
            if a.vram_gb != b.vram_gb {
                b.vram_gb
                    .partial_cmp(&a.vram_gb)
                    .unwrap_or(std::cmp::Ordering::Equal)
            } else {
                b.arch.cmp(&a.arch)
            }
        });
        if cards[0].arch < MIN_ARCH {
            env.reason = format!(
                "la carte {} est trop ancienne pour Strata (RTX 20 ou plus récente)",
                cards[0].name
            );
            return env;
        }
        if cards[0].vram_gb < 7.5 {
            env.reason = format!(
                "la carte {} n'a que {} Go de VRAM (8 Go minimum)",
                cards[0].name,
                f0(cards[0].vram_gb)
            );
            return env;
        }
        env.main = cards[0].index;
        if cards.len() > 1 && cards[1].arch >= MIN_ARCH && cards[1].vram_gb >= 5.5 {
            env.helper = cards[1].index;
        }
        match self.check_python().await {
            Ok(v) => {
                env.python = v;
                env.supported = true;
            }
            Err((v, e)) => {
                env.python = v;
                env.reason = e;
            }
        }
        env
    }

    /// `strataDetailsFor` : la config de lancement d'un preset installé, en
    /// clair (celle que le lancement calculerait).
    pub async fn details_for(
        &self,
        store: &Store,
        cfg: &Cfg,
        order: &[String],
        id: &str,
        env: &Env,
    ) -> Option<Value> {
        let content = crate::presets_api::read_preset(&self.paths.home, id).ok()?;
        let pc = crate::presets::parse_env(&content);
        let raw = std::fs::read(cfg_get(&pc, "STRATA_CONFIG")).ok()?;
        let base: Value = serde_json::from_slice(&raw).ok()?;
        if !base.is_object() {
            return None;
        }
        let mut mode = resolve_experts(
            &self.paths,
            &pc,
            &arg_list(&base),
            env.avail_gb,
            helper_vram(&pc, &env.gpus),
        )
        .to_string();
        let active = crate::presets::list_presets(&self.paths.home, cfg, order)
            .ok()
            .and_then(|l| l.into_iter().find(|p| p.id == id))
            .is_some_and(|p| p.active);
        if active {
            let m = store.get_str(BK_STATE, LAUNCH_MODE_KEY).await;
            if !m.is_empty() {
                mode = m;
            }
        }
        let out = build_config(&base, &pc, "", &mode).ok()?;
        let args = arg_list(&out);
        let val = |flag: &str| -> String {
            args.windows(2)
                .find(|w| w[0] == flag)
                .map(|w| w[1].clone())
                .unwrap_or_default()
        };
        let gpu_name = |idx: &str| -> String {
            env.gpus
                .iter()
                .find(|g| g.index.to_string() == idx.trim())
                .map(|g| {
                    format!(
                        "{} ({} Go)",
                        g.name.strip_prefix("NVIDIA GeForce ").unwrap_or(&g.name),
                        f0(g.vram_gb)
                    )
                })
                .unwrap_or_default()
        };
        let main_gpu = gpu_name(cfg_get(&pc, "STRATA_MAIN_GPU"));
        let mut experts = cfg_get(&pc, "STRATA_EXPERTS").trim().to_lowercase();
        if experts != EXPERTS_RAM && experts != EXPERTS_DISK {
            experts = EXPERTS_AUTO.into();
        }
        let mut d = json!({
            "version": VERSION, "ctx": val("--max-context"), "kv": val("--kv"),
            "spec": val("--spec"), "prefill": val("--prefill"), "short_read": val("--short-read"),
            "cache_root": val("--prompt-cache-root"),
            "mmap": mode == "disk" || mode == "arena", "drop": mode == "drop",
            "experts": experts, "mode_label": experts_label(&mode), "mode": mode,
            "main_gpu": main_gpu,
            "has_helper": false, "helper_on": false,
        });
        let kvr = val("--kv-resident");
        if !kvr.is_empty() {
            d["kv_resident"] = json!(kvr);
        }
        let h = cfg_get(&pc, "STRATA_HELPER_GPU").trim();
        if !h.is_empty() && h != "-1" {
            d["has_helper"] = json!(true);
            if helper_on(&pc) {
                d["helper_on"] = json!(true);
                let hg = gpu_name(h);
                if !hg.is_empty() {
                    d["helper_gpu"] = json!(hg);
                }
            }
        }
        let vision = cfg_get(&pc, "STRATA_VISION") != "0";
        d["vision"] = json!(vision);
        if vision && let Some(Value::Object(v)) = out.get("vision") {
            let vg = match v.get("cuda_device") {
                Some(dev) => gpu_name(&go_sprint(dev)),
                None => main_gpu.clone(),
            };
            if !vg.is_empty() {
                d["vision_gpu"] = json!(vg);
            }
        }
        Some(d)
    }

    /// `handleStrata`.
    pub async fn overview(&self, store: &Store) -> Value {
        let cfg = store.config().await;
        let order = crate::presets::parse_preset_order(
            store
                .get(BK_STATE, crate::presets::PRESET_ORDER_KEY)
                .await
                .ok()
                .flatten()
                .as_deref(),
        );
        let env = self.detect(&cfg).await;
        let mut installed: Vec<String> = Vec::new();
        let mut active: HashMap<String, bool> = HashMap::new();
        if let Ok(list) = crate::presets::list_presets(&self.paths.home, &cfg, &order) {
            for p in list {
                if let Ok(c) = crate::presets_api::read_preset(&self.paths.home, &p.id)
                    && is_strata(&crate::presets::parse_env(&c))
                {
                    installed.push(p.id.clone());
                    active.insert(p.id, p.active);
                }
            }
        }
        let mut fams = Vec::new();
        for f in FAMILIES {
            let mut fits = Vec::new();
            for id in f.quants {
                let q = quant(id).expect("quant");
                let mut fit = fit_for(q, &env, have_gb(&self.paths, f.id, id));
                let pid = preset_name(f.id, id);
                if let Some(a) = active.get(&pid) {
                    fit.preset = pid.clone();
                    fit.active = *a;
                    fit.details = self.details_for(store, &cfg, &order, &pid, &env).await;
                }
                fits.push(fit.to_json());
            }
            fams.push(json!({
                "id": f.id, "label": f.label, "about": f.about, "quants": f.quants,
                "fits": fits, "recommend": recommend(&self.paths, f.id, &env),
            }));
        }
        json!({
            "version": VERSION, "env": env.to_json(), "families": fams,
            "installed": installed, "active": is_strata(&cfg),
        })
    }

    // --- installation ----------------------------------------------------

    fn sha256_file(p: &Path) -> Result<String, String> {
        let mut f = std::fs::File::open(p)
            .map_err(|e| crate::paths::os_err("open", &p.to_string_lossy(), &e))?;
        let mut h = sha2::Sha256::new();
        let mut buf = vec![0u8; 1 << 16];
        loop {
            let n = std::io::Read::read(&mut f, &mut buf).map_err(|e| e.to_string())?;
            if n == 0 {
                break;
            }
            h.update(&buf[..n]);
        }
        Ok(hex::encode(h.finalize()))
    }

    /// `strataFetch` : un fichier de la release figée, vérifié par SHA-256.
    async fn fetch(&self, log: &Log<'_>, asset: &str) -> Result<PathBuf, String> {
        let want = self
            .asset_sha
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .get(asset)
            .cloned()
            .unwrap_or_default();
        let dl = self.paths.dl_dir();
        let dst = dl.join(asset);
        if Self::sha256_file(&dst).is_ok_and(|g| g == want) {
            log.line(&format!("{asset} déjà téléchargé")).await;
            return Ok(dst);
        }
        std::fs::create_dir_all(&dl)
            .map_err(|e| crate::paths::os_err("mkdir", &dl.to_string_lossy(), &e))?;
        log.phase(&format!("téléchargement de {asset}…")).await;
        let tmp = PathBuf::from(format!("{}.part", dst.to_string_lossy()));
        let rq = self.client.get(format!("{}{asset}", self.release_base));
        if let Err(e) = crate::backends::log_download(log, rq, &tmp, 0).await {
            let _ = std::fs::remove_file(&tmp);
            return Err(format!("téléchargement de {asset} : {e}"));
        }
        let got = Self::sha256_file(&tmp)?;
        if got != want {
            let _ = std::fs::remove_file(&tmp);
            return Err(format!(
                "{asset} : empreinte SHA-256 inattendue (fichier altéré ou incomplet)"
            ));
        }
        std::fs::rename(&tmp, &dst).map_err(|e| {
            // *os.LinkError : « rename ancien nouveau: erreur ».
            crate::paths::os_err(
                "rename",
                &format!("{} {}", tmp.to_string_lossy(), dst.to_string_lossy()),
                &e,
            )
        })?;
        Ok(dst)
    }

    /// `runStepEnv` avec le job pour journal : la sortie mêlée, découpée en
    /// lignes comme le `sinkWriter` du Go.
    async fn run_logged(
        &self,
        log: &Log<'_>,
        dir: &Path,
        name: &str,
        args: &[String],
        env: &[(&str, String)],
    ) -> Result<(), String> {
        let Some(bin) = self.probe.look_path(name) else {
            return Err(format!(
                "exec: \"{name}\": executable file not found in $PATH"
            ));
        };
        let (reader, writer) = std::io::pipe().map_err(|e| e.to_string())?;
        let writer2 = writer.try_clone().map_err(|e| e.to_string())?;
        let mut c = tokio::process::Command::new(&bin);
        c.args(args)
            .current_dir(dir)
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::from(writer))
            .stderr(std::process::Stdio::from(writer2));
        for (k, v) in env {
            c.env(k, v);
        }
        let mut child = c
            .spawn()
            .map_err(|e| crate::paths::os_err("fork/exec", &bin.to_string_lossy(), &e))?;
        drop(c);
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel::<Vec<u8>>();
        let read = tokio::task::spawn_blocking(move || {
            let mut r = reader;
            let mut buf = [0u8; 8192];
            loop {
                match std::io::Read::read(&mut r, &mut buf) {
                    Ok(0) | Err(_) => break,
                    Ok(n) => {
                        if tx.send(buf[..n].to_vec()).is_err() {
                            break;
                        }
                    }
                }
            }
        });
        let mut sink = SinkWriter::default();
        while let Some(chunk) = rx.recv().await {
            for l in sink.write(&chunk) {
                log.line(&l).await;
            }
        }
        let _ = read.await;
        let status = child.wait().await.map_err(|e| e.to_string())?;
        match status.code() {
            Some(0) => Ok(()),
            Some(code) => Err(format!("exit status {code}")),
            None => Err(status.to_string()),
        }
    }

    /// `strataRunInstall` : le corps du job d'installation.
    async fn run_install(&self, ctx: &Ctx, b: &Backends, family: &str, quant_id: &str) {
        let store = &ctx.store;
        let log = Log {
            jobs: &b.jobs,
            store,
        };
        let cfg = store.config().await;
        let env = self.detect(&cfg).await;
        if !env.supported {
            return b.jobs.fail(store, &env.reason).await;
        }
        let Some(q) = quant(quant_id) else {
            return b
                .jobs
                .fail(store, &format!("quant inconnu : {quant_id}"))
                .await;
        };
        let fit = fit_for(q, &env, have_gb(&self.paths, family, q.id));
        if !fit.ok {
            return b
                .jobs
                .fail(
                    store,
                    &format!("{} ne tient pas sur cette machine : {}", q.id, fit.why),
                )
                .await;
        }
        let src = match self.fetch(&log, SRC_ASSET).await {
            Ok(p) => p,
            Err(e) => return b.jobs.fail(store, &e).await,
        };
        let engine = match self.fetch(&log, ENGINE_ASSET).await {
            Ok(p) => p,
            Err(e) => return b.jobs.fail(store, &e).await,
        };
        let src_dir = self.paths.src_dir();
        let home = self.paths.strata_home();
        if std::fs::metadata(src_dir.join("setup.py")).is_err() {
            log.phase("décompression de Strata…").await;
            let (s2, h2) = (src.clone(), home.clone());
            let r =
                tokio::task::spawn_blocking(move || crate::backends::prebuilt::extract(&s2, &h2))
                    .await
                    .unwrap_or_else(|e| Err(e.to_string()));
            if let Err(e) = r {
                return b.jobs.fail(store, &format!("décompression : {e}")).await;
            }
        }
        let pre = home.join("prebuilt");
        let _ = std::fs::create_dir_all(&pre);
        let dst = pre.join(ENGINE_LOCAL);
        if let Err(e) = std::fs::copy(&engine, &dst) {
            return b
                .jobs
                .fail(
                    store,
                    &crate::paths::os_err("open", &dst.to_string_lossy(), &e),
                )
                .await;
        }
        log.phase(&format!(
            "installation de {} {} (téléchargement de ~{} Go)…",
            family_label(family),
            q.id,
            f0(q.download_gb)
        ))
        .await;
        let mut pre_s = pre.to_string_lossy().into_owned();
        pre_s.push(std::path::MAIN_SEPARATOR);
        let args: Vec<String> = [
            "setup.sh",
            "--setup",
            "--yes",
            "--no-start",
            "--no-browser",
            "--family",
            family,
            "--model",
            q.id,
            "--context",
            "131072",
            "--vision",
            "gpu",
            "--cuda",
            "12",
            "--prebuilt",
            &pre_s,
            "--data-dir",
            &self.paths.data_dir().to_string_lossy(),
            "--gpu",
            &env.main.to_string(),
            "--low-ram",
            "off",
        ]
        .iter()
        .map(|s| s.to_string())
        .collect();
        let extra = [
            ("CUDA_DEVICE_ORDER", "PCI_BUS_ID".to_string()),
            ("PYTHONUNBUFFERED", "1".to_string()),
            (
                "XDG_CACHE_HOME",
                home.join("cache").to_string_lossy().into_owned(),
            ),
        ];
        if let Err(e) = self.run_logged(&log, &src_dir, "bash", &args, &extra).await {
            return b
                .jobs
                .fail(
                    store,
                    &format!("l'installation a échoué : {e} (voir le journal)"),
                )
                .await;
        }
        let _ = std::fs::write(self.paths.engine_marker(), format!("{ENGINE_ASSET}\n"));
        let cfg_path = match find_config(&src_dir, q.id) {
            Ok(p) => p,
            Err(e) => return b.jobs.fail(store, &e).await,
        };
        let name = preset_name(family, q.id);
        let presets = self.paths.presets_dir();
        let preset = presets.join(format!("{name}.env"));
        let body = [
            format!("# NAME={} {} (Strata)", family_label(family), q.id),
            format!("ENGINE={ENGINE_STRATA}"),
            format!("STRATA_CONFIG={cfg_path}"),
            format!("STRATA_MAIN_GPU={}", env.main),
            format!("STRATA_HELPER_GPU={}", env.helper),
            "STRATA_VISION=1".into(),
            format!("STRATA_EXPERTS={EXPERTS_AUTO}"),
            "STRATA_DROP=1".into(),
            "STRATA_KV=fp16".into(),
            "STRATA_KV_RESIDENT=32768".into(),
            "STRATA_SPEC=3".into(),
            "CTX=131072".into(),
            "REASONING=off".into(),
            String::new(),
        ]
        .join("\n");
        if let Err(e) = std::fs::create_dir_all(&presets) {
            return b
                .jobs
                .fail(
                    store,
                    &crate::paths::os_err("mkdir", &presets.to_string_lossy(), &e),
                )
                .await;
        }
        if let Err(e) = std::fs::write(&preset, body) {
            return b
                .jobs
                .fail(
                    store,
                    &crate::paths::os_err("open", &preset.to_string_lossy(), &e),
                )
                .await;
        }
        log.phase("activation du modèle (premier chargement : quelques minutes)…")
            .await;
        if let Err(e) = switch(store, &preset).await {
            return b
                .jobs
                .fail(
                    store,
                    &format!("modèle installé mais bascule impossible : {e}"),
                )
                .await;
        }
        b.jobs
            .done(
                store,
                &format!("{} {} installé (Strata)", family_label(family), q.id),
            )
            .await;
    }
}

/// `SwitchToPreset` : le garde de l'optimiseur, puis l'application et le
/// redémarrage par le Go.
async fn switch(store: &Store, path: &Path) -> Result<(), String> {
    if let Some(e) = crate::backends::tune_busy(store).await {
        return Err(e);
    }
    crate::presets_api::preset_apply(store, path, "switch").await
}

/// Le `sinkWriter` du Go : un flux découpé en lignes ; une barre qui se
/// redessine (`\r`) n'est montrée que toutes les 10 s.
#[derive(Default)]
pub struct SinkWriter {
    buf: Vec<u8>,
    last_prog: Option<Instant>,
}

/// `sinkProgressEvery`.
const PROGRESS_EVERY: Duration = Duration::from_secs(10);

impl SinkWriter {
    pub fn write(&mut self, p: &[u8]) -> Vec<String> {
        self.buf.extend_from_slice(p);
        let mut out = Vec::new();
        while let Some(i) = self.buf.iter().position(|c| *c == b'\r' || *c == b'\n') {
            if self.buf[i] == b'\n' {
                let line = String::from_utf8_lossy(&self.buf[..i]).into_owned();
                out.push(line.trim_end_matches('\r').to_string());
                self.buf.drain(..=i);
                continue;
            }
            if i + 1 >= self.buf.len() {
                break;
            }
            if self.buf[i + 1] == b'\n' {
                out.push(String::from_utf8_lossy(&self.buf[..i]).into_owned());
                self.buf.drain(..i + 2);
                continue;
            }
            let prog = String::from_utf8_lossy(&self.buf[..i]).trim().to_string();
            if !prog.is_empty() && self.last_prog.is_none_or(|t| t.elapsed() >= PROGRESS_EVERY) {
                self.last_prog = Some(Instant::now());
                out.push(prog);
            }
            self.buf.drain(..=i);
        }
        out
    }
}

/// Les routes servies par ce module.
pub fn handles(path: &str) -> bool {
    matches!(
        path,
        "/api/strata" | "/api/strata/install" | "/api/strata/settings"
    )
}

fn fail(code: StatusCode, e: impl Into<String>) -> Response<Body> {
    send_json(code, &json!({ "ok": false, "error": e.into() }))
}

/// `from_go` : remise par le Go (tunnel), déjà authentifiée par lui.
pub async fn serve(
    ctx: &Ctx,
    s: &Arc<Strata>,
    req: Request<Incoming>,
    from_go: bool,
) -> Response<Body> {
    let Some(b) = ctx.backends.clone() else {
        return fail(
            StatusCode::SERVICE_UNAVAILABLE,
            "backends non tenus par la façade",
        );
    };
    let (parts, body) = req.into_parts();
    if !from_go && let Err(refus) = ctx.guard(&parts.headers).await {
        return *refus;
    }
    let raw = Limited::new(body, 1 << 20)
        .collect()
        .await
        .map(|b| b.to_bytes().to_vec())
        .unwrap_or_default();
    let store = &ctx.store;
    let post = parts.method == Method::POST;
    match parts.uri.path() {
        "/api/strata" => send_json(StatusCode::OK, &s.overview(store).await),
        "/api/strata/settings" => {
            if !post {
                return fail(StatusCode::METHOD_NOT_ALLOWED, "POST attendu");
            }
            let d = gojson::decode_struct(
                &raw,
                SETTINGS_REQ,
                &[
                    ("preset", Kind::Str),
                    ("ctx", Kind::Int),
                    ("kv", Kind::Str),
                    ("spec", Kind::Int),
                    ("vision", Kind::Bool),
                    ("helper", Kind::Bool),
                    ("experts", Kind::Str),
                ],
            );
            if let Some(e) = d.err.clone() {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let r = SettingsReq {
                preset: d.str("preset"),
                ctx: d.int("ctx"),
                kv: d.str("kv"),
                spec: d.int("spec"),
                vision: d.bool("vision"),
                helper: d.bool("helper"),
                experts: d.str("experts"),
            };
            if r.preset.contains(['/', '\\']) || r.preset.is_empty() {
                return fail(StatusCode::BAD_REQUEST, "preset invalide");
            }
            let home = &s.paths.home;
            let content = match crate::presets_api::read_preset(home, &r.preset) {
                Ok(c) if is_strata(&crate::presets::parse_env(&c)) => c,
                _ => return fail(StatusCode::NOT_FOUND, "modèle introuvable"),
            };
            let next = match apply_settings(&content, &r) {
                Ok(n) => n,
                Err(e) => return fail(StatusCode::BAD_REQUEST, e),
            };
            // « En service » se décide AVANT l'écriture (comme save_applying) :
            // c'est l'ancien contenu qui porte l'empreinte de la configuration.
            let cfg = store.config().await;
            let order = crate::presets::parse_preset_order(
                store
                    .get(BK_STATE, crate::presets::PRESET_ORDER_KEY)
                    .await
                    .ok()
                    .flatten()
                    .as_deref(),
            );
            let active = crate::presets::list_presets(home, &cfg, &order)
                .is_ok_and(|l| l.iter().any(|p| p.id == r.preset && p.active));
            let path = s.paths.presets_dir().join(format!("{}.env", r.preset));
            if let Err(e) = std::fs::write(&path, next) {
                return fail(
                    StatusCode::INTERNAL_SERVER_ERROR,
                    crate::paths::os_err("open", &path.to_string_lossy(), &e),
                );
            }
            let mut restarted = false;
            if active {
                if let Some(e) = crate::backends::tune_busy(store).await {
                    return send_json(
                        StatusCode::CONFLICT,
                        &json!({ "ok": false, "tuning": true, "error": e }),
                    );
                }
                if let Err(e) = crate::presets_api::preset_apply(store, &path, "switch").await {
                    return fail(StatusCode::INTERNAL_SERVER_ERROR, e);
                }
                restarted = true;
            }
            send_json(
                StatusCode::OK,
                &json!({ "ok": true, "restarted": restarted }),
            )
        }
        "/api/strata/install" => {
            if !post {
                return fail(StatusCode::METHOD_NOT_ALLOWED, "POST attendu");
            }
            let d = gojson::decode_struct(
                &raw,
                INSTALL_REQ,
                &[("family", Kind::Str), ("quant", Kind::Str)],
            );
            if let Some(e) = d.err.clone() {
                return fail(StatusCode::BAD_REQUEST, e);
            }
            let (family, q) = (d.str("family"), d.str("quant"));
            if !FAMILIES
                .iter()
                .any(|f| f.id == family && f.quants.contains(&q.as_str()))
            {
                return fail(StatusCode::BAD_REQUEST, "version ou quant inconnu");
            }
            if let Some(err) = crate::backends::tune_busy(store).await {
                return fail(StatusCode::CONFLICT, err);
            }
            if let Err(err) = b.jobs.start(store, "strata").await {
                return fail(StatusCode::CONFLICT, err);
            }
            let (ctx2, s2, b2) = (ctx.clone(), s.clone(), b.clone());
            tokio::spawn(async move {
                s2.run_install(&ctx2, &b2, &family, &q).await;
                b2.jobs.end(&ctx2.store).await;
            });
            send_json(StatusCode::OK, &json!({ "ok": true }))
        }
        _ => fail(StatusCode::NOT_FOUND, "route inconnue"),
    }
}
