//! Lot 21 : mise à jour de Loki tenue par la façade (`LOKI_UPDATE=rust`, en
//! conteneur). Le journal tiré du Go (`internal/loki/parity_update_test.go` →
//! `tests/parity_update/update.json`) est rejoué par la façade.

mod common;

use common::fakego;
use serde_json::Value;

#[tokio::test]
async fn parite_en_conteneur() {
    let fx: Value = serde_json::from_str(include_str!("parity_update/update.json")).unwrap();
    let tmp = tempfile::tempdir().unwrap();
    let (facade, _go, _stop, _) = fakego::start_with(
        tmp.path().to_path_buf(),
        fakego::Opts {
            update: true,
            ..Default::default()
        },
    )
    .await;
    for st in fx["steps"].as_array().unwrap() {
        let (code, b) = fakego::send(
            facade,
            st["method"].as_str().unwrap(),
            st["path"].as_str().unwrap(),
            &[],
            "",
        )
        .await;
        assert_eq!(code.as_u16() as i64, st["status"].as_i64().unwrap(), "{st}");
        // La version est celle du Go (0.15.0 pour le faux Go).
        let out: Value =
            serde_json::from_str(&String::from_utf8_lossy(&b).replace("0.15.0", "{{VERSION}}"))
                .unwrap();
        assert_eq!(out, st["out"], "{st}");
    }
}

#[test]
fn seulement_en_conteneur() {
    assert!(loki_rs::sysupdate::usable(Some("1")));
    assert!(!loki_rs::sysupdate::usable(Some("0")));
    assert!(!loki_rs::sysupdate::usable(None));
}

#[tokio::test]
async fn sans_la_variable_la_mise_a_jour_reste_au_go() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, _go, _stop) = fakego::start(tmp.path().to_path_buf()).await;
    for p in ["/api/update", "/api/update/apply"] {
        let (_, _, b) = fakego::get(facade, p, &[]).await;
        assert_eq!(&b[..], format!("go:{p}").as_bytes());
    }
}
