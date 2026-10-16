//! Lot 43 : jobs d'arrière-plan du mode Code (`bash_bg`, `bash_tail`) tenus
//! par la façade, rejoués sur le journal du Go
//! (`internal/loki/parity_jobs_test.go` → `tests/parity_jobs/jobs.json`).
#![cfg(target_os = "linux")]

use std::time::Duration;

use loki_rs::tools::jobs::BgJobs;
use regex::Regex;
use serde_json::{Map, Value};

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn jobs_comme_le_go() {
    let fx: Value = serde_json::from_str(include_str!("parity_jobs/jobs.json")).expect("journal");
    let tmp = tempfile::tempdir().unwrap();
    let ws = tmp.path().join("ws");
    std::fs::create_dir_all(&ws).unwrap();
    let jobs = BgJobs::default();
    let pid = Regex::new(r"\(pid [0-9]+\)").unwrap();
    let since = Regex::new(r"depuis [0-9hms.]+").unwrap();
    let state = Regex::new(r"— (en cours|terminé \(exit -1\)) —").unwrap();
    for st in fx["steps"].as_array().unwrap() {
        let name = st["name"].as_str().unwrap();
        if let Some(id) = st["wait"].as_str() {
            // Le job attendu est le seul qui tourne encore.
            for _ in 0..1000 {
                if jobs.running() == 0 {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
            assert_eq!(jobs.running(), 0, "{name} : {id} pas fini");
        }
        let args: Map<String, Value> = serde_json::from_value(st["args"].clone()).unwrap();
        let got = match st["tool"].as_str().unwrap() {
            "bash_bg" => jobs.bash_bg(&ws, "c1", &args, None).await,
            _ => jobs.bash_tail(&args).await,
        };
        let got = pid.replace_all(&got, "(pid {{PID}})");
        let mut got = since.replace_all(&got, "depuis {{D}}").into_owned();
        if st["after"] == "kill" {
            got = state.replace_all(&got, "— {{ETAT}} —").into_owned();
        }
        assert_eq!(got, st["result"].as_str().unwrap(), "{name}");
        let mut logs: Vec<String> = std::fs::read_dir(ws.join(".loki/jobs"))
            .into_iter()
            .flatten()
            .flatten()
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .collect();
        logs.sort();
        assert_eq!(serde_json::json!(logs), st["logs"], "{name} : journaux");
    }
}
