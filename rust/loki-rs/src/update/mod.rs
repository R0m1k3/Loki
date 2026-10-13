//! Mise à jour des binaires Loki au démarrage du conteneur.
//!
//! La CI publie à chaque push sur `main` une release GitHub `rolling` :
//! `loki-linux`, `loki-rs-linux`, `SHA256SUMS` puis `manifest.json` (en
//! dernier). Au boot, [`run`] compare le manifeste au build en service et, si
//! besoin, installe les deux binaires dans `<LOKI_HOME>/bin/<build>/` (voir
//! [`state`]). Rien ici ne doit bloquer le démarrage : toute erreur se solde
//! par [`Outcome::Failed`] et l'ancien état reste en place.
//!
//! Le transfert HTTPS passe par `curl` (présent dans l'image) : le binaire
//! n'embarque ainsi aucune pile TLS.

pub mod state;
pub mod sums;

use sha2::{Digest, Sha256};
use std::fs;
use std::io::Read;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::Duration;

use state::{Decision, Manifest};

pub const DEFAULT_URL: &str = "https://github.com/R0m1k3/Loki/releases/download/rolling";
pub const IMAGE_ABI_FILE: &str = "/etc/loki/IMAGE_ABI";

/// Nom de l'asset publié → nom du binaire installé.
pub const ASSETS: [(&str, &str); 2] = [
    ("loki-linux", state::GO_BIN),
    ("loki-rs-linux", state::RS_BIN),
];

const TMP_DIR: &str = ".tmp-download";

#[derive(Debug, Clone)]
pub struct UpdateConfig {
    pub enabled: bool,
    pub base_url: String,
    pub data_bin: PathBuf,
    pub image_build: u64,
    pub image_abi: u32,
    /// Durée maximale du téléchargement du manifeste et des sommes.
    pub small_timeout: Duration,
    /// Durée maximale de chaque binaire (un transfert figé est coupé bien
    /// avant, cf. `--speed-limit`).
    pub big_timeout: Duration,
}

impl UpdateConfig {
    pub fn from_env() -> Self {
        let env = |k: &str| std::env::var(k).ok().filter(|v| !v.trim().is_empty());
        let home = env("LOKI_HOME").unwrap_or_else(|| "/data".into());
        let image_abi = fs::read_to_string(IMAGE_ABI_FILE)
            .ok()
            .or_else(|| env("LOKI_IMAGE_ABI"))
            .and_then(|s| s.trim().parse().ok())
            .unwrap_or(0);
        UpdateConfig {
            enabled: !matches!(
                env("LOKI_AUTO_UPDATE").as_deref().map(str::trim),
                Some("0" | "false" | "off" | "no")
            ),
            base_url: env("LOKI_UPDATE_URL").unwrap_or_else(|| DEFAULT_URL.into()),
            data_bin: Path::new(&home).join("bin"),
            image_build: env("LOKI_BUILD")
                .and_then(|s| s.trim().parse().ok())
                .unwrap_or(0),
            image_abi,
            small_timeout: Duration::from_secs(20),
            big_timeout: Duration::from_secs(600),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Outcome {
    Disabled,
    UpToDate(u64),
    /// Version publiée incompatible avec l'image : il faut tirer l'image.
    AbiMismatch {
        remote_abi: u32,
        image_abi: u32,
    },
    Installed(u64),
    Failed(String),
}

impl Outcome {
    /// Ligne de journal pour l'entrypoint.
    pub fn message(&self) -> String {
        match self {
            Outcome::Disabled => "mise à jour au démarrage désactivée (LOKI_AUTO_UPDATE=0)".into(),
            Outcome::UpToDate(b) => format!("binaires à jour (build {b})"),
            Outcome::AbiMismatch {
                remote_abi,
                image_abi,
            } if remote_abi < image_abi => format!(
                "image plus récente que la release publiée (ABI {remote_abi}, image {image_abi}) \
                 — binaires de l'image conservés"
            ),
            Outcome::AbiMismatch {
                remote_abi,
                image_abi,
            } => format!(
                "nouvelle version disponible mais elle exige une nouvelle image \
                 (ABI {remote_abi}, image {image_abi}) : docker compose pull"
            ),
            Outcome::Installed(b) => format!("build {b} installé — actif dès ce démarrage"),
            Outcome::Failed(e) => {
                format!("mise à jour impossible ({e}) — binaires actuels conservés")
            }
        }
    }
}

pub fn run(cfg: &UpdateConfig) -> Outcome {
    if !cfg.enabled {
        return Outcome::Disabled;
    }
    if let Err(e) = fs::create_dir_all(&cfg.data_bin) {
        return Outcome::Failed(format!("{}: {e}", cfg.data_bin.display()));
    }
    let tmp = cfg.data_bin.join(TMP_DIR);
    let _ = fs::remove_dir_all(&tmp);
    let res = fs::create_dir_all(&tmp)
        .map_err(|e| format!("{}: {e}", tmp.display()))
        .and_then(|_| run_in(cfg, &tmp));
    let _ = fs::remove_dir_all(&tmp);
    res.unwrap_or_else(Outcome::Failed)
}

fn run_in(cfg: &UpdateConfig, tmp: &Path) -> Result<Outcome, String> {
    let current = state::current(&cfg.data_bin, cfg.image_build, cfg.image_abi)
        .map(|(m, _)| m.build)
        .unwrap_or(cfg.image_build);

    let man_path = tmp.join("manifest.json");
    fetch(&cfg.base_url, "manifest.json", &man_path, cfg.small_timeout)?;
    let remote = state::read_manifest(&man_path).ok_or("manifest.json illisible")?;

    match state::decide(&remote, current, cfg.image_abi) {
        Decision::AbiMismatch => {
            return Ok(Outcome::AbiMismatch {
                remote_abi: remote.abi,
                image_abi: cfg.image_abi,
            });
        }
        Decision::UpToDate => {
            prune(&cfg.data_bin, current);
            return Ok(Outcome::UpToDate(current));
        }
        Decision::Install => {}
    }

    let sums_path = tmp.join("SHA256SUMS");
    fetch(&cfg.base_url, "SHA256SUMS", &sums_path, cfg.small_timeout)?;
    let sums_text = fs::read_to_string(&sums_path).map_err(|e| format!("SHA256SUMS: {e}"))?;
    let sums = sums::parse(&sums_text);

    let stage = tmp.join("stage");
    fs::create_dir_all(&stage).map_err(|e| e.to_string())?;
    for (asset, local) in ASSETS {
        let want = sums
            .get(asset)
            .ok_or(format!("SHA256SUMS sans entrée pour {asset}"))?;
        let dst = stage.join(local);
        fetch(&cfg.base_url, asset, &dst, cfg.big_timeout)?;
        let got = sha256_file(&dst).map_err(|e| format!("{asset}: {e}"))?;
        if &got != want {
            return Err(format!("empreinte de {asset} invalide"));
        }
        make_executable(&dst).map_err(|e| format!("{asset}: {e}"))?;
    }
    write_manifest(&stage.join(state::RELEASE_FILE), &remote)?;

    let dest = cfg.data_bin.join(remote.build.to_string());
    let _ = fs::remove_dir_all(&dest);
    fs::rename(&stage, &dest).map_err(|e| format!("{}: {e}", dest.display()))?;

    // Bascule : release.json réécrit en dernier, par renommage atomique.
    let pointer = cfg.data_bin.join(state::RELEASE_FILE);
    let pointer_tmp = cfg.data_bin.join("release.json.tmp");
    write_manifest(&pointer_tmp, &remote)?;
    fs::rename(&pointer_tmp, &pointer).map_err(|e| format!("{}: {e}", pointer.display()))?;

    prune(&cfg.data_bin, remote.build);
    Ok(Outcome::Installed(remote.build))
}

/// Télécharge `<base>/<name>` dans `dst` via curl. `--speed-limit` coupe un
/// transfert figé (moins de 10 Ko/s pendant 20 s) bien avant `timeout`.
fn fetch(base: &str, name: &str, dst: &Path, timeout: Duration) -> Result<(), String> {
    let url = format!("{}/{}", base.trim_end_matches('/'), name);
    let out = Command::new("curl")
        .args([
            "-fsSL",
            "--connect-timeout",
            "10",
            "--speed-limit",
            "10240",
            "--speed-time",
            "20",
        ])
        .arg("--max-time")
        .arg(timeout.as_secs().max(1).to_string())
        .arg("-o")
        .arg(dst)
        .arg(&url)
        .output()
        .map_err(|e| format!("curl introuvable : {e}"))?;
    if !out.status.success() {
        let err = String::from_utf8_lossy(&out.stderr);
        return Err(format!("{name} : {}", err.trim()));
    }
    Ok(())
}

pub fn sha256_file(path: &Path) -> std::io::Result<String> {
    let mut f = fs::File::open(path)?;
    let mut h = Sha256::new();
    let mut buf = [0u8; 64 * 1024];
    loop {
        let n = f.read(&mut buf)?;
        if n == 0 {
            break;
        }
        h.update(&buf[..n]);
    }
    Ok(hex::encode(h.finalize()))
}

fn write_manifest(path: &Path, m: &Manifest) -> Result<(), String> {
    let text = serde_json::to_string(m).map_err(|e| e.to_string())?;
    fs::write(path, text).map_err(|e| format!("{}: {e}", path.display()))
}

#[cfg(unix)]
fn make_executable(path: &Path) -> std::io::Result<()> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(0o755))
}

#[cfg(not(unix))]
fn make_executable(_: &Path) -> std::io::Result<()> {
    Ok(())
}

/// Supprime les builds autres que `keep` (un seul build est gardé : celui en
/// service). Ne touche qu'aux dossiers au nom purement numérique.
fn prune(data_bin: &Path, keep: u64) {
    let Ok(rd) = fs::read_dir(data_bin) else {
        return;
    };
    for e in rd.flatten() {
        let name = e.file_name();
        let Some(name) = name.to_str() else { continue };
        if let Ok(n) = name.parse::<u64>()
            && n != keep
            && e.path().is_dir()
        {
            let _ = fs::remove_dir_all(e.path());
        }
    }
}
