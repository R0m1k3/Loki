//! Tests de l'optimiseur : ceux du verrou (`backend_tune_test.go`), portés,
//! et les aides du job.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, SystemTime};

use super::lock::*;

fn me() -> Proc {
    Proc {
        pid: i64::from(std::process::id()),
        start: "moi".into(),
        exes: vec![],
    }
}

/// `TestTuneLock` : le verrou se prend, bloque, note l'essai, se rend ; un
/// propriétaire mort le rend périmé et le ménage le retire.
#[tokio::test]
async fn verrou() {
    let tmp = tempfile::tempdir().unwrap();
    let alive = Arc::new(AtomicBool::new(true));
    let a = alive.clone();
    let pid = me().pid;
    let lf = LockFile::with(
        tmp.path(),
        Box::new(move |p: &Proc| a.load(Ordering::SeqCst) && p.pid == pid),
        me(),
    );
    let no_reap = || async { false };
    assert!(lf.active().is_none(), "garde levée sans verrou");
    let l = lf.acquire("cli", true, no_reap).await.unwrap();
    assert!(lf.active().is_some(), "garde muette pendant l'optimisation");
    let e = lf.acquire("web", false, no_reap).await.err().unwrap();
    assert!(
        e.starts_with(&format!(
            "une optimisation tourne déjà (cli, PID {pid}, depuis "
        )),
        "{e}"
    );
    l.set_trial(Some(Proc {
        pid: 999999,
        start: "x".into(),
        exes: vec![],
    }));
    let o = read_owner(&lf.path).unwrap();
    assert_eq!(o.trial.as_ref().map(|t| t.pid), Some(999999));
    assert!(o.main_was_active && o.phase == "essais" && o.via == "cli");
    l.set_phase("relance");
    l.set_backup("/b", "Dense", "Dense");
    let o = read_owner(&lf.path).unwrap();
    assert_eq!(
        (o.phase.as_str(), o.backup.as_str(), o.preset_id.as_str()),
        ("relance", "/b", "Dense")
    );
    l.release();
    assert!(!lf.path.exists(), "verrou non rendu");

    // Propriétaire mort : périmé, il ne bloque rien et cède la place.
    let l = lf.acquire("cli", true, no_reap).await.unwrap();
    l.set_trial(Some(Proc {
        pid: 999999,
        start: "x".into(),
        exes: vec![],
    }));
    alive.store(false, Ordering::SeqCst);
    assert!(lf.active().is_none(), "verrou périmé encore bloquant");
    let claimed = lf.reap_claim().expect("verrou périmé non écarté");
    assert!(claimed.main_was_active, "MainWasActive perdu au nettoyage");
    assert!(!lf.path.exists(), "verrou périmé non retiré");
    drop(l);
    alive.store(true, Ordering::SeqCst);
    let l2 = lf.acquire("web", false, no_reap).await.unwrap();
    // Un verrou d'un autre processus (même PID et même prise, autre
    // démarrage) n'est jamais retiré par release.
    let mut other = l2.snapshot();
    other.owner.start = "autre".into();
    std::fs::write(&lf.path, serde_json::to_vec(&other).unwrap()).unwrap();
    l2.release();
    assert_eq!(read_owner(&lf.path), Some(other), "verrou d'autrui retiré");
}

/// Un verrou périmé dans la place, et `acquire` qui le fait écarter.
#[tokio::test]
async fn verrou_perime_ecarte_a_la_prise() {
    let tmp = tempfile::tempdir().unwrap();
    let lf = LockFile::with(tmp.path(), Box::new(|_: &Proc| false), me());
    let dead = Owner {
        owner: Proc {
            pid: 999999,
            start: "x".into(),
            exes: vec![],
        },
        via: "cli".into(),
        phase: "essais".into(),
        main_was_active: true,
        ..Default::default()
    };
    std::fs::write(&lf.path, serde_json::to_vec(&dead).unwrap()).unwrap();
    let reaped = std::sync::Mutex::new(0);
    let l = lf
        .acquire("web", false, || async {
            *reaped.lock().unwrap() += 1;
            lf.reap_claim().is_some()
        })
        .await
        .unwrap();
    assert_eq!(*reaped.lock().unwrap(), 1);
    assert_eq!(read_owner(&lf.path).unwrap().via, "web");
    drop(l);
    assert!(!lf.path.exists(), "verrou lâché sans release : rendu");
}

/// `TestTuneLockStaleSelfAndFresh` : un verrou à notre PID qui n'est pas
/// l'un des nôtres est périmé ; illisible et neuf il reste, vieux il part.
#[tokio::test]
async fn verrou_perime_de_soi_et_frais() {
    let tmp = tempfile::tempdir().unwrap();
    let lf = LockFile::with(tmp.path(), Box::new(|_: &Proc| true), me());
    let stale = Owner {
        owner: me(),
        via: "web".into(),
        phase: "essais".into(),
        ..Default::default()
    };
    std::fs::write(&lf.path, serde_json::to_vec(&stale).unwrap()).unwrap();
    assert!(
        lf.active().is_none(),
        "verrou d'un ancien processus au même PID encore bloquant"
    );
    let l = lf
        .acquire("cli", false, || async { lf.reap_claim().is_some() })
        .await
        .unwrap();
    assert!(lf.active().is_some(), "notre propre verrou ne bloque plus");
    let mine = l.snapshot();
    l.release();
    // Rendu : plus inscrit comme tenu, le même contenu est périmé.
    std::fs::write(&lf.path, serde_json::to_vec(&mine).unwrap()).unwrap();
    assert!(
        lf.active().is_none(),
        "verrou rendu encore inscrit comme tenu"
    );
    std::fs::remove_file(&lf.path).unwrap();

    // Illisible et neuf : laissé en place.
    std::fs::write(&lf.path, b"").unwrap();
    assert!(lf.reap_claim().is_none());
    assert!(lf.path.exists(), "verrou en cours d'écriture retiré");
    // Illisible et vieux : écarté.
    let f = std::fs::File::options().write(true).open(&lf.path).unwrap();
    f.set_modified(SystemTime::now() - 2 * FRESHNESS).unwrap();
    drop(f);
    assert!(lf.reap_claim().is_none());
    assert!(!lf.path.exists(), "verrou illisible abandonné non retiré");
}

/// Le ménage relit ce qu'il a déplacé : un verrou vivant pris entre la
/// lecture et le déplacement est remis en place.
#[tokio::test]
async fn menage_jamais_sur_un_verrou_vivant() {
    let tmp = tempfile::tempdir().unwrap();
    let alive = Arc::new(AtomicBool::new(false));
    let a = alive.clone();
    let lf = LockFile::with(
        tmp.path(),
        Box::new(move |_: &Proc| a.load(Ordering::SeqCst)),
        me(),
    );
    let o = Owner {
        owner: Proc {
            pid: 999999,
            start: "x".into(),
            exes: vec![],
        },
        via: "cli".into(),
        ..Default::default()
    };
    std::fs::write(&lf.path, serde_json::to_vec(&o).unwrap()).unwrap();
    alive.store(true, Ordering::SeqCst);
    assert!(lf.reap_claim().is_none(), "verrou vivant écarté");
    assert_eq!(read_owner(&lf.path).unwrap(), o);
    // Prises et rendus répétés pendant que le ménage tourne : jamais écarté.
    let stop = Arc::new(AtomicBool::new(false));
    let lf = Arc::new(LockFile::with(
        tmp.path(),
        Box::new(|p: &Proc| p.pid == i64::from(std::process::id())),
        me(),
    ));
    std::fs::remove_file(&lf.path).unwrap();
    let (s2, lf2) = (stop.clone(), lf.clone());
    let reaper = std::thread::spawn(move || {
        let mut n = 0;
        while !s2.load(Ordering::SeqCst) {
            if lf2.reap_claim().is_some() {
                n += 1;
            }
        }
        n
    });
    for i in 0..60 {
        let l = lf
            .acquire("apply", true, || async { false })
            .await
            .unwrap_or_else(|e| panic!("tour {i} : {e}"));
        l.set_phase("application");
        l.release();
    }
    stop.store(true, Ordering::SeqCst);
    assert_eq!(
        reaper.join().unwrap(),
        0,
        "verrou vivant de ce processus écarté"
    );
}

/// Un verrou sans PID n'en est pas un.
#[test]
fn verrou_sans_pid() {
    let tmp = tempfile::tempdir().unwrap();
    let p = tmp.path().join(LOCK_NAME);
    std::fs::write(&p, r#"{"owner":{"pid":0},"via":"cli"}"#).unwrap();
    assert_eq!(read_owner(&p), None);
    std::fs::write(&p, r#"{"owner":{"pid":-3},"via":"cli"}"#).unwrap();
    assert_eq!(read_owner(&p), None);
    std::fs::write(&p, r#"{"owner":{"pid":7},"via":"cli"}"#).unwrap();
    assert_eq!(read_owner(&p).map(|o| o.owner.pid), Some(7));
    std::fs::write(&p, r#"{"owner":{"pid":"7"}}"#).unwrap();
    assert_eq!(read_owner(&p), None, "illisible");
}

/// `tuneProcAlive` : l'instant de démarrage doit concorder s'il est noté ;
/// le binaire, s'il est lisible et noté, doit être l'un des admis.
#[test]
fn meme_processus() {
    let id = |s: &str, e: &str| Some((s.to_string(), e.to_string()));
    let p = Proc {
        pid: 9,
        start: "b/1".into(),
        exes: vec!["/usr/bin/loki".into(), "/opt/llama-server".into()],
    };
    assert!(same_proc(&p, id("b/1", "/opt/llama-server")));
    assert!(same_proc(&p, id("b/1", "/usr/bin/loki")));
    assert!(!same_proc(&p, id("b/2", "/usr/bin/loki")), "PID recyclé");
    assert!(!same_proc(&p, id("b/1", "/usr/bin/autre")));
    // Binaire illisible (droits) : l'instant suffit.
    assert!(same_proc(&p, id("b/1", "")));
    assert!(!same_proc(&p, None));
    let bare = Proc {
        pid: 9,
        ..Default::default()
    };
    assert!(same_proc(&bare, id("x", "/quoi/que/ce/soit")));
}

/// `TestTuneSameExeDeleted`.
#[test]
fn binaire_remplace() {
    assert!(same_exe(
        "/usr/local/bin/loki",
        "/usr/local/bin/loki (deleted)"
    ));
    assert!(!same_exe("/usr/local/bin/loki", "/usr/bin/llama-server"));
    assert!(!same_exe("", ""));
    // Un nom vide n'est jamais le même binaire, même face à « . ».
    assert!(!same_exe(".", ""));
    assert!(!same_exe("", "."));
}

/// L'identité d'un processus comme le Go l'écrit : instant de démarrage
/// préfixé de l'identifiant du démarrage, binaire sans « (deleted) ».
#[cfg(target_os = "linux")]
#[test]
fn identite_d_un_processus() {
    let tmp = tempfile::tempdir().unwrap();
    let root = tmp.path();
    let p = root.join("proc/77");
    std::fs::create_dir_all(&p).unwrap();
    std::fs::create_dir_all(root.join("proc/sys/kernel/random")).unwrap();
    let stat = |state: &str| {
        format!("77 (llama (x) server) {state} 1 77 77 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 4242 0 0")
    };
    std::fs::write(p.join("stat"), stat("S")).unwrap();
    std::os::unix::fs::symlink("/opt/llama-server (deleted)", p.join("exe")).unwrap();
    assert_eq!(
        proc_identity_in(root, 77),
        Some(("4242".into(), "/opt/llama-server".into()))
    );
    std::fs::write(root.join("proc/sys/kernel/random/boot_id"), "b0c1\n").unwrap();
    assert_eq!(proc_identity_in(root, 77).unwrap().0, "b0c1/4242");
    std::fs::write(p.join("stat"), stat("Z")).unwrap();
    assert_eq!(proc_identity_in(root, 77), None, "zombie");
    assert_eq!(proc_identity_in(root, 0), None);
    assert_eq!(proc_identity_in(root, 78), None);
    // Ce processus : vivant, reconnu, et pas un autre au même PID.
    let me = self_proc();
    assert!(proc_alive(&me));
    assert!(!proc_alive(&Proc {
        start: format!("{}x", me.start),
        ..me.clone()
    }));
    assert!(!proc_alive(&Proc {
        exes: vec!["/usr/bin/autre".into()],
        ..me.clone()
    }));
    assert!(proc_alive(&Proc {
        exes: vec![],
        start: String::new(),
        ..me
    }));
}

/// `TestTuneReapOrphan` (la partie processus) : le groupe de l'essai est
/// arrêté, SIGTERM d'abord.
#[cfg(unix)]
#[test]
fn arret_du_groupe() {
    use std::os::unix::process::CommandExt;
    let mut child = std::process::Command::new("sleep")
        .arg("60")
        .process_group(0)
        .spawn()
        .unwrap();
    let pid = i64::from(child.id());
    assert!(pid_alive(pid));
    let t0 = std::time::Instant::now();
    let reaped = Arc::new(AtomicBool::new(false));
    let r = reaped.clone();
    let waiter = std::thread::spawn(move || {
        let _ = child.wait();
        r.store(true, Ordering::SeqCst);
    });
    kill_tree(pid, &|| reaped.load(Ordering::SeqCst));
    waiter.join().unwrap();
    assert!(t0.elapsed() < Duration::from_secs(5), "SIGTERM ignoré ?");
    assert!(!pid_alive(pid));

    // Un essai qui ignore SIGTERM : SIGKILL au groupe après le délai.
    let mut child = std::process::Command::new("sh")
        .args(["-c", "trap '' TERM; while :; do sleep 1; done"])
        .process_group(0)
        .spawn()
        .unwrap();
    let pid = i64::from(child.id());
    std::thread::sleep(Duration::from_millis(200));
    let reaped = Arc::new(AtomicBool::new(false));
    let r = reaped.clone();
    let waiter = std::thread::spawn(move || {
        let _ = child.wait();
        r.store(true, Ordering::SeqCst);
    });
    kill_tree_within(
        pid,
        &|| reaped.load(Ordering::SeqCst),
        Duration::from_millis(300),
    );
    waiter.join().unwrap();
    assert!(reaped.load(Ordering::SeqCst), "essai toujours là");
}

/// `plan_apply` : un résultat qui touche une clé sur option sans l'option
/// (enregistré par une autre version, ou altéré) est refusé avant toute
/// écriture ; avec l'option, le preset est réécrit ; une valeur vide retire
/// la clé.
#[test]
fn application_planifiee() {
    let tmp = tempfile::tempdir().unwrap();
    let dir = tmp.path().join("presets");
    std::fs::create_dir_all(&dir).unwrap();
    let content = "# NAME=Dense\nCTX=32768\nUBATCH=2048\nBATCH=1024\n";
    std::fs::write(dir.join("dense.env"), content).unwrap();
    let tune = super::job::Tune::new(tmp.path().to_path_buf(), "loki".into());
    let mut res = super::job::TuneResult {
        preset_id: "dense".into(),
        preset_name: "Dense".into(),
        preset_fp: crate::presets::preset_fingerprint(content),
        ..Default::default()
    };
    res.set.insert("SPEC".into(), "ngram".into());
    assert_eq!(tune.plan_apply(&res), Err("clé SPEC intouchable".into()));
    res.opt_in = true;
    let (before, patched) = tune.plan_apply(&res).unwrap();
    assert_eq!(before, content);
    assert!(patched.contains("SPEC=ngram"), "{patched}");
    res.set.insert("BATCH".into(), String::new());
    let (_, patched) = tune.plan_apply(&res).unwrap();
    assert!(
        !crate::presets::parse_env(&patched).contains_key("BATCH"),
        "{patched}"
    );
}
