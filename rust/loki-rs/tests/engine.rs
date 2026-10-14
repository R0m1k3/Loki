//! Supervision du moteur (lot 3) — Linux seulement : /proc, setsid, signaux.
#![cfg(target_os = "linux")]

use loki_rs::engine::proctree::linux::pid_alive;
use loki_rs::engine::{self, Opts, Paths};
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::time::Duration;

/// Faux binaire Go : `serve` lance un « llama-server » dans sa session et un
/// « moteur Strata » qui prend SA PROPRE session (comme le vrai), puis attend.
fn faux_go(dir: &Path, corps: &str) -> PathBuf {
    let p = dir.join("faux-loki");
    std::fs::write(&p, format!("#!/bin/sh\n{corps}\n")).unwrap();
    std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o755)).unwrap();
    p
}

fn opts(go: PathBuf) -> Opts {
    Opts {
        go_bin: go,
        check_delay: Duration::from_millis(500),
        grace: Duration::from_secs(2),
        settle: Duration::from_secs(5),
    }
}

fn paths(home: &Path) -> Paths {
    Paths {
        home: home.to_path_buf(),
        service: "loki-engine".into(),
    }
}

fn lire_pid(p: &Path) -> i32 {
    std::fs::read_to_string(p).unwrap().trim().parse().unwrap()
}

#[test]
fn demarre_puis_arrete_tout_l_arbre() {
    let t = tempfile::tempdir().unwrap();
    let home = t.path();
    let go = faux_go(
        home,
        r#"[ "$1" = serve ] || exit 9
echo "serve lancé dans $(pwd)"
sleep 300 &
echo $! > llama.pid
setsid sleep 300 &
echo $! > strata.pid
wait"#,
    );
    let (p, o) = (paths(home), opts(go));

    engine::run("start", &p, &o).unwrap();
    let chef = lire_pid(&p.pid());
    assert!(pid_alive(chef));
    std::thread::sleep(Duration::from_millis(300));
    let llama = lire_pid(&home.join("llama.pid"));
    let strata = lire_pid(&home.join("strata.pid"));
    assert!(pid_alive(llama) && pid_alive(strata));
    let log = std::fs::read_to_string(p.log()).unwrap();
    assert!(log.contains("serve lancé"), "journal : {log}");

    // Déjà démarré : rien de relancé.
    engine::run("start", &p, &o).unwrap();
    assert_eq!(lire_pid(&p.pid()), chef);

    engine::run("stop", &p, &o).unwrap();
    assert!(!p.pid().exists());
    for (nom, pid) in [("chef", chef), ("llama", llama), ("strata", strata)] {
        assert!(!pid_alive(pid), "{nom} (PID {pid}) survit à l'arrêt");
    }
    // Rien à arrêter : pas d'erreur.
    engine::run("stop", &p, &o).unwrap();
}

#[test]
fn redemarre() {
    let t = tempfile::tempdir().unwrap();
    let go = faux_go(t.path(), "exec sleep 300");
    let (p, o) = (paths(t.path()), opts(go));
    engine::run("start", &p, &o).unwrap();
    let avant = lire_pid(&p.pid());
    engine::run("restart", &p, &o).unwrap();
    let apres = lire_pid(&p.pid());
    assert_ne!(avant, apres);
    assert!(!pid_alive(avant) && pid_alive(apres));
    engine::run("stop", &p, &o).unwrap();
}

#[test]
fn echec_au_demarrage() {
    let t = tempfile::tempdir().unwrap();
    let go = faux_go(t.path(), "echo 'modèle introuvable'; exit 1");
    let (p, o) = (paths(t.path()), opts(go));
    let err = engine::run("start", &p, &o).unwrap_err();
    assert_eq!(err, "service loki-engine non démarré");
    assert!(!p.pid().exists(), "PID laissé après un échec");
    assert!(
        std::fs::read_to_string(p.log())
            .unwrap()
            .contains("modèle introuvable")
    );
}

#[test]
fn action_inconnue() {
    let t = tempfile::tempdir().unwrap();
    let (p, o) = (paths(t.path()), opts(PathBuf::from("/bin/true")));
    assert!(engine::run("enable", &p, &o).is_err());
}
