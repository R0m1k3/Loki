//! Claude Code au niveau du processus (`harness::claude::run`), avec un faux
//! `claude` en shell : flux rejoué, environnement et arguments reçus, arrêt
//! qui tue aussi un enfant sourd à SIGTERM, reprise introuvable, sortie sans
//! résultat.
#![cfg(unix)]

use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

use loki_rs::chat::turn::Cancel;
use loki_rs::chat::types::StreamEvent;
use loki_rs::harness::claude::{ClaudeTurn, run};

fn fake(dir: &Path, body: &str) -> PathBuf {
    let p = dir.join("claude");
    std::fs::write(&p, format!("#!/bin/bash\n{body}\n")).unwrap();
    std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o755)).unwrap();
    p
}

fn turn(dir: &Path, bin: PathBuf) -> ClaudeTurn {
    let cwd = dir.join("w");
    std::fs::create_dir_all(&cwd).unwrap();
    ClaudeTurn {
        bin,
        home: dir.join("home"),
        cwd,
        memory_dir: dir.join("mem"),
        prompt: "fais-le".into(),
        session: None,
        base_url: "http://127.0.0.1:1".into(),
        token: "cle".into(),
        model: "qwen".into(),
        ctx_window: 65536,
        auto_update: false,
    }
}

async fn go(
    t: &ClaudeTurn,
    cancel: &Cancel,
) -> (loki_rs::harness::claude::Outcome, Vec<StreamEvent>) {
    let mut evs = Vec::new();
    let mut cb = |e: StreamEvent| {
        evs.push(e);
        true
    };
    let out = run(t, cancel, &mut cb).await;
    (out, evs)
}

#[tokio::test]
async fn flux_rejoue_et_environnement() {
    let d = tempfile::tempdir().unwrap();
    let fx = Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/harness/tour_outils.jsonl");
    let dump = d.path().join("dump");
    let bin = fake(
        d.path(),
        &format!(
            "{{ pwd; printf '%s\\n' \"$@\"; env | sort; }} > '{}'\necho '[claude-code:unrecognized_model] {{}}' >&2\ncat '{}'",
            dump.display(),
            fx.display()
        ),
    );
    let mut t = turn(d.path(), bin);
    t.session = Some("s0".into());
    let (out, evs) = go(&t, &Cancel::never()).await;
    assert_eq!(out.session_id, "f95fb9ca-1fba-495a-acc8-87400e60d4df");
    assert_eq!(out.final_text, "fini");
    assert_eq!(out.err, None);
    assert!(!evs.iter().any(|e| matches!(e, StreamEvent::Err(_))));
    assert_eq!(
        evs.iter()
            .filter(|e| matches!(e, StreamEvent::ToolUsed(t) if t.done))
            .count(),
        4
    );
    let dump = std::fs::read_to_string(dump).unwrap();
    assert!(
        dump.starts_with(&format!("{}\n-p\nfais-le\n", t.cwd.display())),
        "{dump}"
    );
    assert!(dump.contains("\n--resume\ns0\n"), "{dump}");
    for want in [
        "IS_SANDBOX=1",
        "ANTHROPIC_AUTH_TOKEN=cle",
        "ANTHROPIC_MODEL=qwen",
        "DISABLE_AUTOUPDATER=1",
        "DISABLE_TELEMETRY=1",
        &format!("HOME={}", t.home.display()),
    ] {
        assert!(dump.lines().any(|l| l == want), "{want} absent : {dump}");
    }
    // Environnement vidé : rien du processus parent hors PATH/LANG.
    assert!(!dump.contains("CARGO_MANIFEST_DIR"), "{dump}");
}

#[tokio::test]
async fn arret_tue_aussi_les_enfants() {
    let d = tempfile::tempdir().unwrap();
    let pidf = d.path().join("enfant");
    let bin = fake(
        d.path(),
        &format!(
            "bash -c 'trap \"\" TERM; echo $$ > \"{}\"; while :; do sleep 0.1; done' &\ntrap '' TERM\necho '{{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"s1\"}}'\nwhile :; do sleep 0.1; done",
            pidf.display()
        ),
    );
    let t = turn(d.path(), bin);
    let (tx, cancel) = Cancel::new();
    let h = tokio::spawn(async move { go(&t, &cancel).await });
    // L'enfant est lancé.
    let t0 = Instant::now();
    while !pidf.exists() || std::fs::read_to_string(&pidf).unwrap().trim().is_empty() {
        assert!(
            t0.elapsed() < Duration::from_secs(10),
            "enfant jamais lancé"
        );
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    let child: i32 = std::fs::read_to_string(&pidf)
        .unwrap()
        .trim()
        .parse()
        .unwrap();
    tx.send(true).unwrap();
    let (out, _) = tokio::time::timeout(Duration::from_secs(15), h)
        .await
        .unwrap()
        .unwrap();
    assert_eq!(out.session_id, "s1");
    assert_eq!(out.err, None, "un arrêt n'est pas une erreur");
    // SAFETY: kill(pid, 0) ne fait que tester l'existence.
    let t0 = Instant::now();
    loop {
        let alive = unsafe { libc::kill(child, 0) } == 0
            && std::fs::read_to_string(format!("/proc/{child}/stat"))
                .map(|s| !s.contains(") Z"))
                .unwrap_or(false);
        if !alive {
            break;
        }
        assert!(
            t0.elapsed() < Duration::from_secs(5),
            "enfant {child} toujours vivant"
        );
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
}

#[tokio::test]
async fn reprise_introuvable() {
    let d = tempfile::tempdir().unwrap();
    let fx = Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/harness/reprise_absente.jsonl");
    let bin = fake(
        d.path(),
        &format!(
            "cat '{}'\necho 'No conversation found with session ID: x' >&2\nexit 1",
            fx.display()
        ),
    );
    let mut t = turn(d.path(), bin);
    t.session = Some("x".into());
    let (out, evs) = go(&t, &Cancel::never()).await;
    assert!(out.resume_missing);
    assert_eq!(out.session_id, "", "la session ratée n'est jamais gardée");
    assert!(
        !evs.iter().any(|e| matches!(e, StreamEvent::Err(_))),
        "{evs:?}"
    );
}

#[tokio::test]
async fn sortie_sans_resultat() {
    let d = tempfile::tempdir().unwrap();
    let bin = fake(d.path(), "echo 'boum' >&2\nexit 2");
    let (out, evs) = go(&turn(d.path(), bin), &Cancel::never()).await;
    let e = out.err.unwrap();
    assert!(e.contains("boum"), "{e}");
    assert!(matches!(&evs[..], [StreamEvent::Err(m)] if m.contains("boum")));
    // Binaire absent.
    let (out, _) = go(&turn(d.path(), d.path().join("rien")), &Cancel::never()).await;
    assert!(out.err.unwrap().contains("ne démarre pas"));
}
