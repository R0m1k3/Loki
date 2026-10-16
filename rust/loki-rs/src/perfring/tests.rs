//! L'anneau rejoué contre le journal du Go (`parity_perf_test.go` →
//! `tests/parity_perf/perf.json`), et les cas de `perf_log_test.go`.

use std::collections::HashMap;

use serde_json::{Value, json};

use super::*;

fn journal() -> Value {
    serde_json::from_str(include_str!("../../tests/parity_perf/perf.json")).expect("journal")
}

fn ring() -> PerfRing {
    PerfRing::new(std::path::PathBuf::from("/nulle-part"))
}

#[test]
fn parite_avec_le_go() {
    let j = journal();
    let r = ring();
    for (i, st) in j["steps"].as_array().unwrap().iter().enumerate() {
        let mut rec: PerfRec = serde_json::from_value(st["rec"].clone()).unwrap();
        // L'heure posée par le journal (le Go la fixe avant de ranger).
        rec.at = st["out"]["at"].as_str().unwrap().to_string();
        let prefix: Vec<u64> = st["prefix"]
            .as_array()
            .map(|a| {
                a.iter()
                    .map(|p| p.as_str().unwrap().parse().unwrap())
                    .collect()
            })
            .unwrap_or_default();
        let got = r.record(rec, &prefix);
        assert_eq!(serde_json::to_value(&got).unwrap(), st["out"], "étape {i}");
        assert_eq!(perf_line(&got), st["line"].as_str().unwrap(), "étape {i}");
        assert_eq!(
            perf_alert(&got, 1000),
            st["alert_1000"] == json!(1),
            "étape {i}"
        );
    }
    let snap = r.snapshot();
    assert_eq!(
        serde_json::to_value(summarize(&snap)).unwrap(),
        j["summary"]
    );
    assert_eq!(serde_json::to_value(summarize(&[])).unwrap(), j["empty"]);
}

#[test]
fn seuils_d_alerte_du_go() {
    let j = journal();
    for c in j["alerts"].as_array().unwrap() {
        let cfg: HashMap<String, String> = serde_json::from_value(c["cfg"].clone()).unwrap();
        // L'environnement est lu par lost_alert_at : un test à la fois ici.
        let env = c["env"].as_str().unwrap_or("");
        // SAFETY : les tests de ce module ne lisent cette variable qu'ici.
        unsafe { std::env::set_var("LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT", env) };
        let got = lost_alert_at(&cfg, &|_| None);
        assert_eq!(got, c["want"].as_i64().unwrap(), "{c}");
    }
    unsafe { std::env::remove_var("LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT") };
    // Un modèle hybride sans espacement posé : 2048 + micro-batch.
    let cfg: HashMap<String, String> = [("MODEL".to_string(), "h.gguf".to_string())].into();
    assert_eq!(lost_alert_at(&cfg, &|m| Some(m == "h.gguf")), 2560);
    assert_eq!(lost_alert_at(&cfg, &|_| Some(false)), 1000);
    // Le seuil : une perte au-delà, ou un gros prompt repris de zéro.
    let rec = |lost: Option<i64>, cached: Option<i64>, total: i64| PerfRec {
        lost,
        cached,
        total,
        ..Default::default()
    };
    assert!(!perf_alert(&rec(Some(900), None, 0), 1000));
    assert!(perf_alert(&rec(Some(1500), None, 0), 1000));
    assert!(!perf_alert(&rec(Some(2000), None, 0), 2560));
    assert!(perf_alert(&rec(None, Some(0), 9000), 1000));
    assert!(!perf_alert(&rec(None, Some(0), 500), 1000));
    assert!(!perf_alert(&rec(None, None, 9000), 1000));
}

/// `TestPerfRingBounded` : l'anneau et l'état par conversation sont bornés,
/// l'ordre gardé.
#[test]
fn anneau_borne() {
    let r = ring();
    for i in 0..RING_MAX + 10 {
        r.record(
            PerfRec {
                kind: "main".into(),
                conv: char::from(b'a' + (i % 26) as u8).to_string() + &(i % 200).to_string(),
                complete: true,
                total: 10,
                ..Default::default()
            },
            &[i as u64],
        );
    }
    let recs = r.snapshot();
    assert_eq!(recs.len(), RING_MAX);
    for w in recs.windows(2) {
        assert_eq!(w[1].seq, w[0].seq + 1);
    }
    assert_eq!(recs.last().unwrap().seq, (RING_MAX + 10) as u64);
    assert!(r.convs_len() <= CONV_MAX, "{}", r.convs_len());
}

/// L'heure posée à l'enregistrement quand elle manque (ou nulle, venue du
/// Go).
#[test]
fn heure_posee() {
    let mut r = ring();
    r.clock = || "2026-10-16T09:00:00Z".to_string();
    let got = r.record(PerfRec::default(), &[]);
    assert_eq!(got.at, "2026-10-16T09:00:00Z");
    let got = r.record(
        PerfRec {
            at: "0001-01-01T00:00:00Z".into(),
            ..Default::default()
        },
        &[],
    );
    assert_eq!((got.at.as_str(), got.seq), ("2026-10-16T09:00:00Z", 2));
}
