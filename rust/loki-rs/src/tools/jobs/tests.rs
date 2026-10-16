use super::*;

fn args(kv: &[(&str, Value)]) -> Map<String, Value> {
    kv.iter().map(|(k, v)| (k.to_string(), v.clone())).collect()
}

/// Optimisation en cours : refus du Go recopié ; discussion supprimée : ses
/// jobs arrêtés et oubliés, ceux des autres gardés.
#[cfg(unix)]
#[tokio::test]
async fn refus_et_discussion_supprimee() {
    let tmp = tempfile::tempdir().unwrap();
    let jobs = BgJobs::default();
    let a = args(&[("command", Value::from("sleep 30"))]);
    assert_eq!(
        jobs.bash_bg(tmp.path(), "c", &a, Some("optimisation en cours".into()))
            .await,
        "[erreur] optimisation en cours"
    );
    assert!(!tmp.path().join(".loki").exists());
    assert!(
        jobs.bash_bg(tmp.path(), "c1", &a, None)
            .await
            .starts_with("[ok] job job1 lancé")
    );
    assert!(
        jobs.bash_bg(tmp.path(), "c2", &a, None)
            .await
            .starts_with("[ok] job job2 lancé")
    );
    assert_eq!(jobs.running(), 2);
    jobs.stop_conv("c1");
    for _ in 0..500 {
        if jobs.running() == 1 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    assert_eq!(jobs.running(), 1);
    assert_eq!(
        jobs.bash_tail(&args(&[("id", Value::from("job1"))])).await,
        "[erreur] job inconnu : job1"
    );
    jobs.stop_conv("c2");
    for _ in 0..500 {
        if jobs.running() == 0 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    assert_eq!(jobs.running(), 0);
}

/// Dossier des journaux impossible à créer : l'erreur du Go.
#[cfg(unix)]
#[tokio::test]
async fn dossier_impossible() {
    let tmp = tempfile::tempdir().unwrap();
    std::fs::write(tmp.path().join(".loki"), "un fichier").unwrap();
    let jobs = BgJobs::default();
    let got = jobs
        .bash_bg(
            tmp.path(),
            "c",
            &args(&[("command", Value::from("true"))]),
            None,
        )
        .await;
    assert_eq!(
        got,
        format!(
            "[erreur] mkdir {}: not a directory",
            tmp.path().join(".loki").display()
        )
    );
}
