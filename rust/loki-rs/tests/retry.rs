//! Lot 30 : les motifs `retry_patterns` d'une surcharge, rejoués sur le
//! journal tiré du Go (`internal/loki/parity_retry_test.go` →
//! `tests/parity_retry/patterns.json`) : même compilation, même première
//! correspondance.

mod common;

use common::fakego;
use loki_rs::chat::stream::compile_go_regex;
use serde_json::Value;

#[test]
fn motifs_comme_le_go() {
    let fx: Value =
        serde_json::from_str(include_str!("parity_retry/patterns.json")).expect("journal");
    let inputs: Vec<&str> = fx["inputs"]
        .as_array()
        .unwrap()
        .iter()
        .map(|v| v.as_str().unwrap())
        .collect();
    for e in fx["patterns"].as_array().unwrap() {
        let pat = e["pattern"].as_str().unwrap();
        let re = compile_go_regex(&format!("(?mi){pat}"));
        assert_eq!(re.is_some(), e["compiles"].as_bool().unwrap(), "{pat}");
        let Some(re) = re else { continue };
        for m in e["matches"].as_array().unwrap() {
            let input = inputs[m["in"].as_u64().unwrap() as usize];
            let got = re
                .find(input)
                .map(|x| (x.as_str().to_string(), x.start() as i64));
            let want = m["match"]
                .as_str()
                .map(|s| (s.to_string(), m["at"].as_i64().unwrap()));
            assert_eq!(got, want, "{pat} sur {input:?}");
        }
    }
}

/// La surcharge est lue au démarrage de la conversation de la façade ;
/// absente ou illisible, les motifs embarqués s'appliquent.
#[tokio::test]
async fn surcharge_lue_au_demarrage() {
    for (seed, custom, n) in [
        (Some(r#"["^oups", "[", "\\bdo_\\w+\\("]"#), true, 2),
        (Some("pas du json"), false, 0),
        (None, false, 0),
    ] {
        let tmp = tempfile::tempdir().unwrap();
        let (_f, _go, _stop, chat) = fakego::start_with(
            tmp.path().to_path_buf(),
            fakego::Opts {
                chat: true,
                seed: seed
                    .map(|s| vec![("state".into(), "retry_patterns".into(), s.into())])
                    .unwrap_or_default(),
                ..Default::default()
            },
        )
        .await;
        let p = &chat.unwrap().shared.patterns;
        assert_eq!(p.is_custom(), custom, "{seed:?}");
        if custom {
            assert_eq!(p.len(), n);
            assert!(p.textual_tool_call("OUPS, raté"));
        }
    }
}
