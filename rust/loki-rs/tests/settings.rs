//! Lot 10 : réglages partagés tenus par la façade (`LOKI_SETTINGS=rust`).
//!
//! Le journal tiré du Go (`internal/loki/parity_settings_test.go` →
//! `tests/parity_settings/settings.json`) est rejoué : normalisation de
//! l'adresse publique, intensité du raisonnement, puis 29 appels de route
//! par la façade, avec le contenu de la base après chacun.

mod common;

use std::collections::BTreeMap;

use common::fakego;
use loki_rs::settings;
use serde_json::Value;

fn fixture() -> Value {
    serde_json::from_str(include_str!("parity_settings/settings.json")).expect("journal")
}

#[test]
fn parite_des_fonctions_pures() {
    let fx = fixture();
    for c in fx["public_url"].as_array().unwrap() {
        let input = c["in"].as_str().unwrap();
        match settings::normalize_public_url(input) {
            Ok(o) => {
                assert!(
                    c.get("err").is_none(),
                    "{input:?} : erreur attendue {}",
                    c["err"]
                );
                assert_eq!(o, c["out"].as_str().unwrap_or(""), "{input:?}");
            }
            Err(e) => assert_eq!(e, c["err"].as_str().unwrap_or("<ok attendu>"), "{input:?}"),
        }
    }
    for (v, want) in fx["effort_value"].as_object().unwrap() {
        assert_eq!(
            loki_rs::chat::wire::reasoning_effort_value(v),
            want.as_str().unwrap(),
            "{v:?}"
        );
    }
}

#[tokio::test]
async fn parite_des_routes() {
    let fx = fixture();
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().to_path_buf();
    let opts = fakego::Opts {
        settings: true,
        ..Default::default()
    };
    let (facade, go, _stop, _) = fakego::start_with(home.clone(), opts).await;
    let keys = [
        "sysprompt",
        "sysprompt:alpha",
        "sysprompt:beta",
        "public_url",
    ];
    for (i, st) in fx["steps"].as_array().unwrap().iter().enumerate() {
        if let Some(cfg) = st.get("config").and_then(Value::as_object) {
            let mut s = go.store.lock().unwrap();
            let b = s.entry("config".into()).or_default();
            b.clear();
            for (k, v) in cfg {
                b.insert(k.clone(), v.as_str().unwrap().as_bytes().to_vec());
            }
        }
        if let Some(p) = st.get("preset").and_then(Value::as_object) {
            std::fs::create_dir_all(home.join("presets")).unwrap();
            for (id, content) in p {
                std::fs::write(
                    home.join("presets").join(format!("{id}.env")),
                    content.as_str().unwrap(),
                )
                .unwrap();
            }
        }
        let (m, p) = (st["method"].as_str().unwrap(), st["path"].as_str().unwrap());
        let body = st.get("body").and_then(Value::as_str).unwrap_or("");
        let (code, b) = fakego::send(facade, m, p, &[], body).await;
        let ctx = format!("étape {i} : {m} {p} {body}");
        assert_eq!(
            code.as_u16() as u64,
            st["status"].as_u64().unwrap(),
            "{ctx}"
        );
        let got: Value = serde_json::from_slice(&b).unwrap();
        assert_eq!(got, st["out"], "{ctx}");

        let prefs: BTreeMap<String, String> = go
            .store
            .lock()
            .unwrap()
            .get("prefs")
            .map(|b| {
                b.iter()
                    .map(|(k, v)| (k.clone(), String::from_utf8_lossy(v).into_owned()))
                    .collect()
            })
            .unwrap_or_default();
        let want_prefs: BTreeMap<String, String> = serde_json::from_value(
            st.get("prefs")
                .cloned()
                .unwrap_or(Value::Object(Default::default())),
        )
        .unwrap();
        assert_eq!(prefs, want_prefs, "{ctx} (préférences en base)");
        let mut state = BTreeMap::new();
        for k in keys {
            if let Some(v) = go.get("state", k).filter(|v| !v.is_empty()) {
                state.insert(k.to_string(), v);
            }
        }
        if let Some(v) = go
            .get("config", "REASONING_EFFORT")
            .filter(|v| !v.is_empty())
        {
            state.insert("config:REASONING_EFFORT".into(), v);
        }
        let want_state: BTreeMap<String, String> = serde_json::from_value(
            st.get("state")
                .cloned()
                .unwrap_or(Value::Object(Default::default())),
        )
        .unwrap();
        assert_eq!(state, want_state, "{ctx} (base)");
    }
}

#[tokio::test]
async fn sans_la_variable_les_reglages_restent_au_go() {
    let tmp = tempfile::tempdir().unwrap();
    let (facade, _go, _stop) = fakego::start(tmp.path().to_path_buf()).await;
    for p in [
        "/api/prefs",
        "/api/sysprompt",
        "/api/reasoning-effort",
        "/api/oai/public",
    ] {
        let (_, _, b) = fakego::get(facade, p, &[]).await;
        assert_eq!(&b[..], format!("go:{p}").as_bytes());
    }
}
