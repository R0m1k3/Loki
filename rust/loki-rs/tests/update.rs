mod common;

use loki_rs::update::{self, Outcome, UpdateConfig, sha256_file, state};
use std::collections::HashMap;
use std::path::Path;
use std::time::Duration;

const GO: &[u8] = b"binaire go";
const RS: &[u8] = b"binaire rust";

fn sha(b: &[u8]) -> String {
    let t = tempfile::NamedTempFile::new().unwrap();
    std::fs::write(t.path(), b).unwrap();
    sha256_file(t.path()).unwrap()
}

fn release(
    build: u64,
    abi: u32,
    go: &[u8],
    rs: &[u8],
    sums_go: Option<&str>,
) -> HashMap<String, Vec<u8>> {
    let sums = format!(
        "{}  loki-linux\n{}  loki-rs-linux\n",
        sums_go.map(str::to_string).unwrap_or_else(|| sha(go)),
        sha(rs)
    );
    HashMap::from([
        (
            "manifest.json".into(),
            format!(r#"{{"build":{build},"abi":{abi},"commit":"c{build}"}}"#).into_bytes(),
        ),
        ("SHA256SUMS".into(), sums.into_bytes()),
        ("loki-linux".into(), go.to_vec()),
        ("loki-rs-linux".into(), rs.to_vec()),
    ])
}

fn cfg(url: String, data_bin: &Path) -> UpdateConfig {
    UpdateConfig {
        enabled: true,
        base_url: url,
        data_bin: data_bin.to_path_buf(),
        image_build: 3,
        image_abi: 1,
        small_timeout: Duration::from_secs(10),
        big_timeout: Duration::from_secs(10),
    }
}

#[test]
fn installe_puis_reconnait_a_jour() {
    let srv = common::serve_files(release(7, 1, GO, RS, None));
    let t = tempfile::tempdir().unwrap();
    let c = cfg(srv.url(), t.path());

    assert_eq!(update::run(&c), Outcome::Installed(7));
    let dir = state::select_dir(t.path(), Path::new("/img"), 3, 1);
    assert_eq!(dir, t.path().join("7"));
    assert_eq!(std::fs::read(dir.join("loki")).unwrap(), GO);
    assert_eq!(std::fs::read(dir.join("loki-rs")).unwrap(), RS);
    assert!(!t.path().join(".tmp-download").exists());

    assert_eq!(update::run(&c), Outcome::UpToDate(7));
}

#[test]
fn nouveau_build_remplace_et_nettoie_l_ancien() {
    let t = tempfile::tempdir().unwrap();
    let srv = common::serve_files(release(7, 1, GO, RS, None));
    assert_eq!(
        update::run(&cfg(srv.url(), t.path())),
        Outcome::Installed(7)
    );
    let srv2 = common::serve_files(release(9, 1, b"go 9", b"rs 9", None));
    assert_eq!(
        update::run(&cfg(srv2.url(), t.path())),
        Outcome::Installed(9)
    );
    assert!(!t.path().join("7").exists());
    assert_eq!(
        state::select_dir(t.path(), Path::new("/img"), 3, 1),
        t.path().join("9")
    );
}

#[test]
fn empreinte_fausse_ne_change_rien() {
    let t = tempfile::tempdir().unwrap();
    let srv = common::serve_files(release(7, 1, GO, RS, None));
    assert_eq!(
        update::run(&cfg(srv.url(), t.path())),
        Outcome::Installed(7)
    );

    let bad = "0".repeat(64);
    let srv2 = common::serve_files(release(9, 1, b"go 9", b"rs 9", Some(&bad)));
    assert!(matches!(
        update::run(&cfg(srv2.url(), t.path())),
        Outcome::Failed(_)
    ));
    assert_eq!(
        state::select_dir(t.path(), Path::new("/img"), 3, 1),
        t.path().join("7")
    );
    assert!(!t.path().join("9").exists());
    assert!(!t.path().join(".tmp-download").exists());
}

#[test]
fn abi_differente_ne_telecharge_que_le_manifeste() {
    let t = tempfile::tempdir().unwrap();
    let srv = common::serve_files(release(7, 2, GO, RS, None));
    assert_eq!(
        update::run(&cfg(srv.url(), t.path())),
        Outcome::AbiMismatch {
            remote_abi: 2,
            image_abi: 1
        }
    );
    assert_eq!(srv.hits(), vec!["manifest.json".to_string()]);
}

#[test]
fn image_plus_recente_que_la_release() {
    let t = tempfile::tempdir().unwrap();
    let srv = common::serve_files(release(3, 1, GO, RS, None));
    assert_eq!(update::run(&cfg(srv.url(), t.path())), Outcome::UpToDate(3));
}

#[test]
fn serveur_absent() {
    let t = tempfile::tempdir().unwrap();
    let mut c = cfg("http://127.0.0.1:9/rolling".into(), t.path());
    c.small_timeout = Duration::from_secs(3);
    assert!(matches!(update::run(&c), Outcome::Failed(_)));
}

#[test]
fn desactive_sans_requete() {
    let t = tempfile::tempdir().unwrap();
    let srv = common::serve_files(release(7, 1, GO, RS, None));
    let mut c = cfg(srv.url(), t.path());
    c.enabled = false;
    assert_eq!(update::run(&c), Outcome::Disabled);
    assert!(srv.hits().is_empty());
}
