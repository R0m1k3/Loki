//! État d'installation des binaires mis à jour, sous `<LOKI_HOME>/bin` :
//!
//! ```text
//! bin/release.json        manifeste du build actif (écrit en DERNIER)
//! bin/<build>/loki        binaire Go
//! bin/<build>/loki-rs     binaire Rust
//! ```
//!
//! La bascule d'une version à l'autre se résume à réécrire `release.json` :
//! un arrêt brutal pendant une mise à jour laisse l'ancien build intact.

use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};

pub const RELEASE_FILE: &str = "release.json";
pub const GO_BIN: &str = "loki";
pub const RS_BIN: &str = "loki-rs";

/// Contenu de `manifest.json` (release) et de `release.json` (installé).
///
/// `build` est le numéro de run de la CI : il ne fait que croître, c'est lui
/// qui ordonne les versions (un commit ne s'ordonne pas). `abi` est l'ABI
/// d'image exigée par ces binaires.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Manifest {
    pub build: u64,
    pub abi: u32,
    #[serde(default)]
    pub commit: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Decision {
    Install,
    UpToDate,
    AbiMismatch,
}

pub fn read_manifest(path: &Path) -> Option<Manifest> {
    let text = std::fs::read_to_string(path).ok()?;
    serde_json::from_str(&text).ok()
}

/// Build installé dans `data_bin` et utilisable avec cette image : même ABI,
/// plus récent que les binaires de l'image, et les deux binaires présents.
pub fn current(data_bin: &Path, image_build: u64, image_abi: u32) -> Option<(Manifest, PathBuf)> {
    let m = read_manifest(&data_bin.join(RELEASE_FILE))?;
    if m.abi != image_abi || m.build <= image_build {
        return None;
    }
    let dir = data_bin.join(m.build.to_string());
    if !dir.join(GO_BIN).is_file() || !dir.join(RS_BIN).is_file() {
        return None;
    }
    Some((m, dir))
}

/// Dossier dont l'entrypoint doit prendre `loki` et `loki-rs` : toujours les
/// deux du même endroit, jamais un Go d'une version avec un Rust d'une autre.
pub fn select_dir(data_bin: &Path, image_dir: &Path, image_build: u64, image_abi: u32) -> PathBuf {
    match current(data_bin, image_build, image_abi) {
        Some((_, dir)) => dir,
        None => image_dir.to_path_buf(),
    }
}

/// Faut-il installer `remote`, sachant que le build en service est `current_build` ?
pub fn decide(remote: &Manifest, current_build: u64, image_abi: u32) -> Decision {
    if remote.abi != image_abi {
        Decision::AbiMismatch
    } else if remote.build <= current_build {
        Decision::UpToDate
    } else {
        Decision::Install
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn m(build: u64, abi: u32) -> Manifest {
        Manifest {
            build,
            abi,
            commit: "abc".into(),
        }
    }

    fn install(data_bin: &Path, man: &Manifest, bins: &[&str]) {
        let dir = data_bin.join(man.build.to_string());
        std::fs::create_dir_all(&dir).unwrap();
        for b in bins {
            std::fs::write(dir.join(b), b"x").unwrap();
        }
        std::fs::write(
            data_bin.join(RELEASE_FILE),
            serde_json::to_string(man).unwrap(),
        )
        .unwrap();
    }

    #[test]
    fn decision() {
        assert_eq!(decide(&m(10, 2), 5, 1), Decision::AbiMismatch);
        assert_eq!(decide(&m(5, 1), 5, 1), Decision::UpToDate);
        assert_eq!(decide(&m(4, 1), 5, 1), Decision::UpToDate);
        assert_eq!(decide(&m(6, 1), 5, 1), Decision::Install);
    }

    #[test]
    fn selection_sans_installation() {
        let t = tempfile::tempdir().unwrap();
        let img = Path::new("/usr/local/bin");
        assert_eq!(select_dir(t.path(), img, 3, 1), img);
    }

    #[test]
    fn selection_installation_valide() {
        let t = tempfile::tempdir().unwrap();
        install(t.path(), &m(7, 1), &[GO_BIN, RS_BIN]);
        let img = Path::new("/usr/local/bin");
        assert_eq!(select_dir(t.path(), img, 3, 1), t.path().join("7"));
    }

    #[test]
    fn selection_refuse_abi_differente() {
        let t = tempfile::tempdir().unwrap();
        install(t.path(), &m(7, 2), &[GO_BIN, RS_BIN]);
        let img = Path::new("/img");
        assert_eq!(select_dir(t.path(), img, 3, 1), img);
    }

    #[test]
    fn selection_refuse_plus_ancien_que_l_image() {
        // Image tirée à la main plus récente que /data/bin : l'image gagne.
        let t = tempfile::tempdir().unwrap();
        install(t.path(), &m(7, 1), &[GO_BIN, RS_BIN]);
        let img = Path::new("/img");
        assert_eq!(select_dir(t.path(), img, 7, 1), img);
        assert_eq!(select_dir(t.path(), img, 9, 1), img);
    }

    #[test]
    fn selection_refuse_binaire_manquant() {
        let t = tempfile::tempdir().unwrap();
        install(t.path(), &m(7, 1), &[GO_BIN]);
        let img = Path::new("/img");
        assert_eq!(select_dir(t.path(), img, 3, 1), img);
    }
}
